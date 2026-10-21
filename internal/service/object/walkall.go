package object

import (
	"container/heap"
	"context"
	"slices"
	"time"

	"github.com/r3n3r0/nexo/internal/service/storage"
)

// setStream is the walk of one store, consumed entry by entry.
type setStream struct {
	ch   chan walkEntry
	errc chan error
	head *walkEntry
	done bool
	err  error
}

func (s *setStream) advance() {
	e, ok := <-s.ch
	if !ok {
		s.head, s.done = nil, true
		s.err = <-s.errc
		return
	}
	s.head = &e
}

// streamHeap orders the live streams by their head entry.
type streamHeap []*setStream

func (h streamHeap) Len() int { return len(h) }
func (h streamHeap) Less(i, j int) bool {
	return entryBefore(h[i].head.Name, h[i].head.IsDir, h[j].head.Name, h[j].head.IsDir)
}
func (h streamHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *streamHeap) Push(x any)   { *h = append(*h, x.(*setStream)) }
func (h *streamHeap) Pop() any {
	old := *h
	s := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return s
}

// walkAll walks every store (all the sets of all the pools) and merges
// their entries in S3 key order with a heap: a prefix reported by several
// sets is reported once, the versions of a key held by several pools are
// merged (newest first). fn stops the walk by returning an error, which
// walkAll returns; a store whose walk fails makes the whole walk fail.
func (l *layer) walkAll(ctx context.Context, opts storage.WalkDirOptions, fn func(walkEntry) error) error {
	return walkStores(ctx, l.sets, opts, fn)
}

// walkStores is walkAll over the given stores.
func walkStores(ctx context.Context, stores []store, opts storage.WalkDirOptions, fn func(walkEntry) error) error {
	if len(stores) == 1 {
		return stores[0].WalkVersions(ctx, opts, fn)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	streams := make([]*setStream, len(stores))
	for i, st := range stores {
		s := &setStream{ch: make(chan walkEntry, walkBuffer), errc: make(chan error, 1)}
		streams[i] = s
		go func() {
			err := st.WalkVersions(ctx, opts, func(e walkEntry) error {
				select {
				case s.ch <- e:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			s.errc <- err
			close(s.ch)
		}()
	}
	defer func() {
		cancel()
		for _, s := range streams {
			for !s.done {
				s.advance()
			}
		}
	}()
	h := make(streamHeap, 0, len(streams))
	for _, s := range streams {
		s.advance()
		if s.err != nil {
			return s.err
		}
		if s.head != nil {
			h = append(h, s)
		}
	}
	heap.Init(&h)
	for h.Len() > 0 {
		top := h[0]
		entry := *top.head
		merged := false
		// Pop every stream positioned on the same entry.
		for h.Len() > 0 && h[0].head.Name == entry.Name && h[0].head.IsDir == entry.IsDir {
			s := h[0]
			if s != top && !entry.IsDir {
				entry.Versions = append(slices.Clone(entry.Versions), s.head.Versions...)
				merged = true
			}
			s.advance()
			if s.err != nil {
				return s.err
			}
			if s.head == nil {
				heap.Pop(&h)
			} else {
				heap.Fix(&h, 0)
			}
		}
		if merged {
			entry.Versions = sortVersions(entry.Versions)
		}
		if err := fn(entry); err != nil {
			return err
		}
	}
	return nil
}

// sortVersions orders the versions of a key gathered from several pools,
// newest first, keeps the newest of a version id found twice (the null
// version of a key written in two pools) and recomputes the positional
// fields.
func sortVersions(versions []storage.FileInfo) []storage.FileInfo {
	slices.SortStableFunc(versions, func(a, b storage.FileInfo) int { return b.ModTime.Compare(a.ModTime) })
	seen := make(map[string]bool, len(versions))
	versions = slices.DeleteFunc(versions, func(fi storage.FileInfo) bool {
		dup := seen[fi.VersionID]
		seen[fi.VersionID] = true
		return dup
	})
	for i := range versions {
		versions[i].IsLatest = i == 0
		versions[i].Idx = i
		versions[i].NumVersions = len(versions)
		versions[i].SuccessorModTime = time.Time{}
		if i > 0 {
			versions[i].SuccessorModTime = versions[i-1].ModTime
		}
	}
	return versions
}
