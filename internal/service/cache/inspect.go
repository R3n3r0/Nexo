package cache

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// Inspect implements Operations: the key the cache computes for the URL,
// the effective policy with the level of each setting, and the state of
// the key on every node (docs/11 §11.7, D-CCH-05).
func (s *service) Inspect(ctx context.Context, in InspectInput) (InspectResult, error) {
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return InspectResult{}, fmt.Errorf("%w: %q is not an http(s) URL", ErrInvalid, in.URL)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	host := normalizeHost(u.Host)
	res := InspectResult{URL: in.URL, Host: host, Path: u.Path, Mode: s.mode()}
	route := s.deps.Distributions.Resolve(ctx, u.Host)
	if route.Kind != distribution.RouteDistribution || route.Distribution == nil {
		res.Bypass, res.Reason = true, "no distribution serves this host"
		return res, nil
	}
	d := route.Distribution
	res.Distribution, res.Bucket = d.ID, d.Origin.Bucket
	test, err := s.deps.Distributions.Test(ctx, distribution.TestInput{URL: in.URL, Method: http.MethodGet})
	if err != nil {
		return InspectResult{}, err
	}
	switch {
	case test.RedirectStatus != 0:
		res.Bypass, res.Reason = true, fmt.Sprintf("redirected (%d) to %s", test.RedirectStatus, test.RedirectLocation)
	case test.RefusalStatus != 0:
		res.Bypass, res.Reason = true, fmt.Sprintf("refused (%d): %s", test.RefusalStatus, test.Refusal)
	}
	key := test.Key
	prefix := keyPrefixOf(*d)
	if (key == prefix || strings.HasSuffix(key, "/")) && test.Behavior.IndexDocument != "" {
		key += test.Behavior.IndexDocument
	}
	if d.Mode == distribution.ModeS3 {
		key = strings.TrimPrefix(u.Path, "/")
	}
	res.Key = key
	path := "/" + strings.TrimPrefix(key, prefix)
	header := in.Header
	if header == nil {
		header = http.Header{}
	}
	req := Request{Distribution: d, Alias: route.Alias, Host: route.Host, Method: http.MethodGet, Path: u.Path, RawQuery: u.RawQuery,
		Header: header, Secure: u.Scheme == "https", Credentials: header.Get("Authorization") != ""}
	r := s.snap.Load().resolveFor(ctx, d.ID, route.Host, path)
	res.Policy = r.eff
	if reason := bypassReason(&r.p, &req); reason != "" && !res.Bypass {
		res.Bypass, res.Reason = true, reason
	}
	if key == "" {
		return res, nil
	}
	credential := ""
	if req.Credentials && !r.p.BypassAuth {
		credential = credentialOf(&req)
	}
	res.CacheKey = keyOf(&r.p, scopeOf(&r.p, d.ID, route.Host), d.Origin.Bucket, key, &req, credential)
	res.Nodes = s.presenceEverywhere(ctx, res.CacheKey)
	return res, nil
}

// mode is the effective cluster mode.
func (s *service) mode() Mode {
	if len(s.deps.Peers) == 0 {
		return ModeLocal
	}
	return s.opts.mode
}

// presenceEverywhere reports a key on every node.
func (s *service) presenceEverywhere(ctx context.Context, key string) []NodePresence {
	var owner Peer
	ownerKnown := s.shared()
	if ownerKnown {
		owner = s.ownerOf(key)
	}
	out := make([]NodePresence, 1+len(s.deps.Peers))
	out[0] = NodePresence{Node: s.deps.NodeID, Online: true, Owner: ownerKnown && owner == nil, Entry: s.store.presence(key, s.now())}
	var wg sync.WaitGroup
	for i, p := range s.deps.Peers {
		out[i+1] = NodePresence{Node: p.NodeID(), Online: p.IsOnline(), Owner: ownerKnown && owner == p}
		if !p.IsOnline() {
			out[i+1].Error = "offline"
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, s.opts.ackTimeout)
			defer cancel()
			rep, err := p.Presence(cctx, PresenceRequest{Keys: []string{key}})
			if err != nil || len(rep.Entries) == 0 {
				if err != nil {
					out[i+1].Error = err.Error()
				}
				return
			}
			out[i+1].Entry = rep.Entries[0]
		}()
	}
	wg.Wait()
	return out
}
