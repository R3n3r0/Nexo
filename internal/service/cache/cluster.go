package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// --- sending -------------------------------------------------------------------------

// link sends the invalidations of this node to a peer, in order of
// sequence: one batch in flight, the messages not acknowledged kept for
// the retries.
type link struct {
	peer   Peer
	sendMu sync.Mutex

	mu    sync.Mutex
	queue []InvalidationMessage
	acked uint64
	// collapsed are the ids of the manual invalidations folded into a
	// FlushAll when the queue overflowed.
	collapsed []string
}

// enqueue queues a message; past max messages the queue collapses into a
// single flush of the peer's cache.
func (l *link) enqueue(m InvalidationMessage, maxPending int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queue = append(l.queue, m)
	if len(l.queue) <= maxPending {
		return
	}
	for _, q := range l.queue {
		if q.ID != "" {
			l.collapsed = append(l.collapsed, q.ID)
		}
	}
	l.queue = []InvalidationMessage{{Seq: m.Seq, FlushAll: true}}
}

// pending reports queued messages.
func (l *link) pending() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue) > 0
}

// flush sends the queued messages up to seq upTo (all with 0) and
// reports whether upTo is acknowledged.
func (s *service) flush(ctx context.Context, l *link, upTo uint64) error {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	l.mu.Lock()
	if upTo > 0 && l.acked >= upTo {
		l.mu.Unlock()
		return nil
	}
	batch := append([]InvalidationMessage(nil), l.queue...)
	collapsed := l.collapsed
	l.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	ack, err := l.peer.ApplyInvalidations(ctx, InvalidationBatch{Origin: s.deps.NodeID, Epoch: s.epoch, Messages: batch})
	if err != nil {
		return err
	}
	last := batch[len(batch)-1].Seq
	l.mu.Lock()
	n := 0
	for n < len(l.queue) && l.queue[n].Seq <= last {
		n++
	}
	l.queue = l.queue[n:]
	l.acked = max(l.acked, last)
	l.collapsed = l.collapsed[len(collapsed):]
	l.mu.Unlock()
	ids := map[string]int{}
	for _, m := range batch {
		if m.ID != "" {
			ids[m.ID] = ack.Removed[m.ID]
		}
	}
	for _, id := range collapsed {
		ids[id] = 0
	}
	if len(ids) > 0 {
		s.invalidationsApplied(ctx, l.peer.NodeID(), ids)
	}
	return nil
}

// broadcast sends a message to every peer and waits (bounded) for their
// acknowledgements; it returns the error of every peer missed (retried in
// the background).
func (s *service) broadcast(ctx context.Context, build func(seq uint64) InvalidationMessage) map[string]error {
	if len(s.links) == 0 {
		return nil
	}
	s.orderMu.Lock()
	seq := s.seq.Add(1)
	msg := build(seq)
	msg.Seq = seq
	for _, l := range s.links {
		l.enqueue(msg, s.opts.maxPending)
	}
	// When each peer last compared its sequence before this one existed:
	// the trust a peer missing this message may still have.
	polled := s.polledTimes()
	s.orderMu.Unlock()
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ackTimeout)
	defer cancel()
	var (
		mu   sync.Mutex
		errs = map[string]error{}
		wg   sync.WaitGroup
	)
	for _, l := range s.links {
		if !l.peer.IsOnline() {
			errs[l.peer.NodeID()] = fmt.Errorf("peer %s: %w (retried)", l.peer.NodeID(), domain.ErrNodeOffline)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.flush(cctx, l, seq); err != nil {
				mu.Lock()
				errs[l.peer.NodeID()] = err
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	s.fence(ctx, errs, polled)
	return errs
}

// fence waits, before a write missed by some peers is acknowledged, for
// the trust those peers had in their entries to lapse: a peer trusts its
// entries for the trust lease after the start of a comparison of the
// sequences with every peer, and it compared this node's sequence before
// the message existed. A paused or partitioned node that comes back
// therefore revalidates its entries instead of serving the old content
// after the 200 of the write (TCC-05, TCC-C-04). The wait is bounded by
// the lease.
func (s *service) fence(ctx context.Context, missed map[string]error, polled map[string]time.Time) {
	lease := s.opts.trustLease
	var wait time.Duration
	now := s.now()
	for node := range missed {
		at, ok := polled[node]
		if !ok {
			continue
		}
		wait = max(wait, min(at.Add(lease).Sub(now), lease))
	}
	if wait <= 0 {
		return
	}
	s.deps.Logger.DebugContext(ctx, "cache: waiting for the trust of the peers missed to lapse", "wait", wait)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// polledTimes returns when each peer last compared this node's sequence.
func (s *service) polledTimes() map[string]time.Time {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	out := make(map[string]time.Time, len(s.polled))
	for k, v := range s.polled {
		out[k] = v
	}
	return out
}

// retryLinks sends the messages the peers missed.
func (s *service) retryLinks(ctx context.Context) {
	for _, l := range s.links {
		if !l.pending() || !l.peer.IsOnline() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, s.opts.ackTimeout)
		if err := s.flush(cctx, l, 0); err != nil {
			s.deps.Logger.DebugContext(ctx, "cache: invalidations still pending", "peer", l.peer.NodeID(), "err", err)
		}
		cancel()
	}
}

// --- receiving -----------------------------------------------------------------------

// originState is what this node received from a peer: the run (epoch)
// and the last sequence of its invalidations.
type originState struct {
	epoch string
	last  uint64
	known bool
	// lag is the sequence the last poll reported beyond last: until the
	// messages up to it are applied the node does not trust its entries;
	// still behind at the next poll, the node missed invalidations and
	// drops its cache.
	lag uint64
	// lagAt is when the lag was seen (a retry of the peer may still
	// deliver the messages within lagGrace).
	lagAt time.Time
	// reachable reports that the last poll answered.
	reachable bool
}

// lagGrace is how long the peer has to deliver the messages a comparison
// found missing (a retry, bounded by the ack timeout) before they are
// considered lost.
func (s *service) lagGrace() time.Duration { return s.opts.retryInterval + s.opts.ackTimeout }

// behind reports a peer whose invalidations this node has not all
// applied.
func (o *originState) behind() bool { return o.lag > o.last }

// countLaggingLocked updates the number of peers this node is behind.
func (s *service) countLaggingLocked() {
	n := int32(0)
	for _, o := range s.origins {
		if o.behind() {
			n++
		}
	}
	s.lagging.Store(n)
}

// ApplyInvalidations implements Node: the messages are applied in order
// (every message, a duplicate too: they are idempotent); a sequence that
// skips one drops the whole cache.
func (s *service) ApplyInvalidations(ctx context.Context, batch InvalidationBatch) (InvalidationAck, error) {
	ack := InvalidationAck{Node: s.deps.NodeID, Removed: map[string]int{}}
	if batch.Origin == "" || batch.Origin == s.deps.NodeID {
		return ack, fmt.Errorf("%w: invalidations without origin", ErrInvalid)
	}
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	st := s.origins[batch.Origin]
	if st == nil {
		st = &originState{}
		s.origins[batch.Origin] = st
	}
	for _, m := range batch.Messages {
		if st.epoch != batch.Epoch {
			// A new run of the peer, or the first message heard from it.
			first := !st.known
			restarted := st.known && m.Seq != 1
			st.epoch, st.last, st.known = batch.Epoch, m.Seq-1, true
			if restarted {
				s.flushAll(ctx, "the peer "+batch.Origin+" restarted and this node missed its first invalidations")
				ack.Flushed = true
			} else if first && m.Seq > 1 {
				// The earlier messages of the peer may predate the entries
				// of this node, or not: they are revalidated.
				s.store.unverifyBefore(s.now())
			}
		}
		if m.Seq > st.last+1 && !m.FlushAll {
			// A gap (a flush message covers the messages it replaced).
			s.flushAll(ctx, fmt.Sprintf("invalidations %d to %d of %s missed", st.last+1, m.Seq-1, batch.Origin))
			ack.Flushed = true
		}
		removed := s.applyMessage(ctx, m)
		if m.ID != "" {
			ack.Removed[m.ID] = removed
		}
		st.last = max(st.last, m.Seq)
	}
	if st.lag <= st.last {
		st.lag = 0
	}
	s.countLaggingLocked()
	ack.LastSeq = st.last
	return ack, nil
}

// SyncStatus implements Node. It remembers when the peer asked (before
// the sequence is read, see fence).
func (s *service) SyncStatus(_ context.Context, req SyncRequest) (SyncStatus, error) {
	if req.Node != "" && req.Node != s.deps.NodeID {
		s.pollMu.Lock()
		if _, known := s.polled[req.Node]; known || len(s.polled) < maxPolled {
			s.polled[req.Node] = s.now()
		}
		s.pollMu.Unlock()
	}
	return SyncStatus{Node: s.deps.NodeID, Epoch: s.epoch, Seq: s.seq.Load()}, nil
}

// maxPolled bounds the peers whose comparisons are remembered.
const maxPolled = 1024

// flushAll drops the whole cache of this node.
func (s *service) flushAll(ctx context.Context, why string) {
	n := s.store.clear()
	s.flushes.Add(1)
	s.deps.Logger.WarnContext(ctx, "cache: dropping the cache of this node", "why", why, "entries", n)
}

// syncPeers compares the invalidation sequences of the peers with what
// this node received. A complete comparison (every peer answered) renews
// the trust lease of the entries from its start; a peer ahead of what
// this node applied suspends the trust until the missing messages are
// applied, and still ahead at the next comparison means they were lost
// (the cache is dropped). An unreachable peer revokes the trust.
func (s *service) syncPeers(ctx context.Context) {
	if len(s.deps.Peers) == 0 {
		return
	}
	type answer struct {
		node string
		st   SyncStatus
		err  error
	}
	start := s.now()
	answers := make([]answer, len(s.deps.Peers))
	var wg sync.WaitGroup
	for i, p := range s.deps.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, s.opts.ackTimeout)
			defer cancel()
			st, err := p.SyncStatus(cctx, SyncRequest{Node: s.deps.NodeID})
			answers[i] = answer{node: p.NodeID(), st: st, err: err}
		}()
	}
	wg.Wait()
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	complete := true
	for _, a := range answers {
		o := s.origins[a.node]
		if o == nil {
			o = &originState{}
			s.origins[a.node] = o
		}
		if a.err != nil {
			o.reachable = false
			complete = false
			continue
		}
		o.reachable = true
		switch {
		case !o.known:
			// The baseline: the messages of the peer before it are not
			// known to this node, so the entries cached before it are
			// revalidated.
			o.epoch, o.last, o.known, o.lag = a.st.Epoch, a.st.Seq, true, 0
			if a.st.Seq > 0 {
				s.store.unverifyBefore(start)
			}
		case o.epoch != a.st.Epoch:
			// The peer restarted: its new run starts at 1.
			o.epoch, o.last, o.lag, o.lagAt = a.st.Epoch, 0, a.st.Seq, start
		case a.st.Seq > o.last:
			switch {
			case !o.behind():
				o.lag, o.lagAt = a.st.Seq, start
			case start.Sub(o.lagAt) >= s.lagGrace():
				s.flushAll(ctx, fmt.Sprintf("invalidations %d to %d of %s missed", o.last+1, o.lag, a.node))
				o.last, o.lag = a.st.Seq, 0
			}
		default:
			o.lag = 0
		}
	}
	s.countLaggingLocked()
	if complete {
		until := start.Add(s.opts.trustLease)
		s.trustUntil.Store(&until)
	} else {
		s.trustUntil.Store(nil)
	}
}

// trusted reports whether this node may serve its entries without asking
// the origin: every peer reachable, a complete comparison of the
// sequences started less than the trust lease ago, and every invalidation
// it reported applied. A node that was paused or cut off finds its lease
// expired when it comes back: its entries are revalidated until a fresh
// comparison (TCC-C-04).
func (s *service) trusted() bool {
	if len(s.deps.Peers) == 0 {
		return true
	}
	for _, p := range s.deps.Peers {
		if !p.IsOnline() {
			return false
		}
	}
	if until := s.trustUntil.Load(); until != nil && s.lagging.Load() == 0 && s.now().Before(*until) {
		return true
	}
	// Compare the sequences now rather than at the next period.
	if last := s.syncKicked.Load(); time.Since(time.Unix(0, last)) >= syncKickMin && s.syncKicked.CompareAndSwap(last, time.Now().UnixNano()) {
		select {
		case s.syncNow <- struct{}{}:
		default:
		}
	}
	return false
}

// syncKickMin bounds the comparisons of the sequences asked by the
// requests of a node that does not trust its entries.
const syncKickMin = 100 * time.Millisecond

// --- shared mode ------------------------------------------------------------------------

// shared reports the shared mode of a cluster.
func (s *service) shared() bool {
	return len(s.deps.Peers) > 0 && (s.opts.mode == ModeShared || s.opts.mode == ModeSharedLocal)
}

// score is the rendezvous hash of a key on a node.
func score(node, key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(node))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	x := h.Sum64()
	// A final mix (splitmix64) spreads the close inputs.
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// ownerOf returns the peer owning a key among the online nodes (nil: this
// node).
func (s *service) ownerOf(key string) Peer {
	best, bestScore := Peer(nil), score(s.deps.NodeID, key)
	for _, p := range s.deps.Peers {
		if !p.IsOnline() {
			continue
		}
		if sc := score(p.NodeID(), key); sc > bestScore || (sc == bestScore && p.NodeID() < s.deps.NodeID) {
			best, bestScore = p, sc
		}
	}
	return best
}

// getPiece serves a piece: from this node, or from the owner of its key in
// the shared mode (from the origin, without caching, when the owner cannot
// be reached).
func (s *service) getPiece(ctx context.Context, ps *pieceSpec) (piece, error) {
	if !s.shared() {
		return s.localPiece(ctx, ps)
	}
	owner := s.ownerOf(ps.Key)
	if owner == nil {
		return s.localPiece(ctx, ps)
	}
	hot := s.opts.mode == ModeSharedLocal
	if hot {
		if p, ok := s.hotPiece(ctx, ps); ok {
			return p, nil
		}
	}
	since := s.store.begin()
	defer s.store.end(since)
	// The owner is bounded by its own deadline: a node that stalls (a
	// paused process, a partition) is skipped like an offline one.
	cctx, cancel := context.WithTimeout(ctx, s.opts.fetchTimeout)
	p, err := s.remotePiece(cctx, owner, ps)
	if err != nil && ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w: cache: the owner %s did not answer in %s: %w", domain.ErrNodeOffline, owner.NodeID(), s.opts.fetchTimeout, err)
	}
	cancel()
	if err == nil {
		if hot && p.meta.Kind != kindPass {
			m := *p.meta
			m.MemoryOnly = true
			s.store.put(&m, p.body, since, nil)
		}
		return p, nil
	}
	if errors.Is(err, domain.ErrNodeOffline) {
		s.deps.Logger.DebugContext(ctx, "cache: owner unreachable, reading the origin", "owner", owner.NodeID(), "key", ps.Key, "err", err)
		cp := *ps
		cp.NoStore = true
		return s.localPiece(ctx, &cp)
	}
	return piece{}, err
}

// hotPiece serves a piece from the local memory tier of the shared+local
// mode.
func (s *service) hotPiece(ctx context.Context, ps *pieceSpec) (piece, bool) {
	meta, body, _, ok := s.store.get(ctx, ps.Key)
	if !ok || !ps.owns(meta) {
		return piece{}, false
	}
	now := s.now()
	switch meta.Kind {
	case kindBlock, kindVariant:
		if meta.ETag == ps.ETag {
			return piece{meta: meta, body: body, result: ResultHit, node: s.deps.NodeID}, true
		}
	case kindObject, kindHead, kindNegative:
		if now.Before(meta.Expires) && s.trusted() && (ps.WantBody || meta.Kind != kindObject || body != nil) {
			return piece{meta: meta, body: body, result: ResultHit, node: s.deps.NodeID}, true
		}
	case kindInfo:
		if !ps.WantBody && now.Before(meta.Expires) && s.trusted() {
			return piece{meta: meta, result: ResultHit, node: s.deps.NodeID}, true
		}
	}
	return piece{}, false
}

// remotePiece asks the owner of a key for a piece.
func (s *service) remotePiece(ctx context.Context, owner Peer, ps *pieceSpec) (piece, error) {
	spec, err := encodeSpec(ps)
	if err != nil {
		return piece{}, err
	}
	reply, err := owner.FetchPiece(ctx, PieceRequest{Spec: spec})
	if err != nil {
		return piece{}, err
	}
	var meta entryMeta
	if err := json.Unmarshal(reply.Meta, &meta); err != nil {
		return piece{}, fmt.Errorf("cache: piece of %s from %s: %w", ps.Key, owner.NodeID(), err)
	}
	if !ps.owns(&meta) {
		return piece{}, fmt.Errorf("%w: cache: the owner %s answered %s/%s for %s/%s", domain.ErrNodeOffline, owner.NodeID(),
			meta.Bucket, meta.Object, ps.Bucket, ps.Object)
	}
	return piece{meta: &meta, body: reply.Body, result: reply.Result, node: reply.Node}, nil
}

// FetchPiece implements Node.
func (s *service) FetchPiece(ctx context.Context, req PieceRequest) (PieceReply, error) {
	ps, err := decodeSpec(req.Spec)
	if err != nil {
		return PieceReply{}, err
	}
	p, err := s.localPiece(ctx, ps)
	if errors.Is(err, errObjectChanged) {
		return PieceReply{}, fmt.Errorf("cache: %s changed: %w", ps.Key, domain.ErrPreconditionFailed)
	}
	if err != nil {
		return PieceReply{}, err
	}
	meta, err := json.Marshal(p.meta)
	if err != nil {
		return PieceReply{}, err
	}
	return PieceReply{Node: s.deps.NodeID, Result: p.result, Meta: meta, Body: p.body}, nil
}

// Presence implements Node.
func (s *service) Presence(_ context.Context, req PresenceRequest) (PresenceReply, error) {
	out := PresenceReply{Node: s.deps.NodeID}
	now := s.now()
	for _, k := range req.Keys {
		out.Entries = append(out.Entries, s.store.presence(k, now))
	}
	return out, nil
}

// backgroundLoop runs the periodic work: the retries of the
// invalidations, the comparison of the sequences, the gauges.
func (s *service) backgroundLoop(ctx context.Context) {
	defer s.bg.Done()
	retry := time.NewTicker(s.opts.retryInterval)
	defer retry.Stop()
	sync := time.NewTicker(s.opts.syncInterval)
	defer sync.Stop()
	gauges := time.NewTicker(10 * time.Second)
	defer gauges.Stop()
	index := time.NewTicker(distRefreshEvery)
	defer index.Stop()
	// The first comparison of the sequences does not delay the start of
	// the node (the peers may not be up yet): until it succeeds the
	// entries are revalidated.
	s.syncPeers(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
			s.retryLinks(ctx)
		case <-sync.C:
			s.syncPeers(ctx)
		case <-s.syncNow:
			s.syncPeers(ctx)
		case <-gauges.C:
			s.updateGauges(ctx)
		case <-index.C:
			s.refreshDistributions(ctx)
		}
	}
}
