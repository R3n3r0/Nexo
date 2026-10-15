package distribution

import (
	"context"
	"slices"
	"strings"
)

// routeEntry is a distribution reached through one of its aliases.
type routeEntry struct {
	alias string
	d     *Distribution
}

// trieNode is a node of the wildcard trie: one label of the reversed
// domain ("com" → "example" → "preview" for "*.preview.example.com").
type trieNode struct {
	children map[string]*trieNode
	// wildcard is the distribution owning "*.<path to this node>".
	wildcard *routeEntry
}

// table is an immutable routing table: built from the distributions and
// swapped atomically.
type table struct {
	exact map[string]routeEntry
	wild  *trieNode
	// byID are the distributions (shared, read-only snapshots).
	byID map[string]*Distribution
	// aliases maps every alias (exact or wildcard) to its distribution id.
	aliases map[string]string
}

// newTable builds the routing table of the distributions. Aliases are
// normalized and unique (Validate refuses the conflicts): a duplicate
// left by a concurrent write is resolved in favor of the smallest id, the
// same on every node.
func newTable(ds []*Distribution, reserved ...func(alias, bucket string) string) *table {
	sorted := slices.Clone(ds)
	slices.SortFunc(sorted, func(a, b *Distribution) int { return strings.Compare(a.ID, b.ID) })
	t := &table{
		exact:   make(map[string]routeEntry),
		wild:    &trieNode{},
		byID:    make(map[string]*Distribution, len(sorted)),
		aliases: make(map[string]string),
	}
	for _, d := range sorted {
		t.byID[d.ID] = d
		for _, a := range d.Aliases {
			if _, taken := t.aliases[a]; taken {
				continue
			}
			if len(reserved) > 0 && reserved[0] != nil && reserved[0](a, d.Origin.Bucket) != "" {
				// Saved before the domains of the deployment reserved it:
				// never routed (Reload logs it).
				continue
			}
			t.aliases[a] = d.ID
			if rest, ok := strings.CutPrefix(a, "*."); ok {
				t.insertWildcard(rest, routeEntry{alias: a, d: d})
				continue
			}
			t.exact[a] = routeEntry{alias: a, d: d}
		}
	}
	return t
}

// insertWildcard registers "*.<suffix>".
func (t *table) insertWildcard(suffix string, e routeEntry) {
	labels := strings.Split(suffix, ".")
	n := t.wild
	for i := len(labels) - 1; i >= 0; i-- {
		if n.children == nil {
			n.children = make(map[string]*trieNode)
		}
		child, ok := n.children[labels[i]]
		if !ok {
			child = &trieNode{}
			n.children[labels[i]] = child
		}
		n = child
	}
	entry := e
	n.wildcard = &entry
}

// lookupWildcard returns the most specific wildcard alias matching host:
// the longest suffix with at least one label before it.
func (t *table) lookupWildcard(host string) (routeEntry, bool) {
	var best *routeEntry
	n := t.wild
	end := len(host)
	for end > 0 {
		start := strings.LastIndexByte(host[:end], '.') + 1
		label := host[start:end]
		child, ok := n.children[label]
		if !ok {
			break
		}
		n = child
		// The wildcard needs a label of its own: "*.example.com" does not
		// match "example.com".
		if n.wildcard != nil && start > 0 {
			best = n.wildcard
		}
		if start == 0 {
			break
		}
		end = start - 1
	}
	if best == nil {
		return routeEntry{}, false
	}
	return *best, true
}

// Resolve implements Resolver.
func (s *service) Resolve(_ context.Context, host string) Route {
	t := s.table.Load()
	h := normalizeHost(host)
	r := Route{Host: h}
	if h == "" {
		r.Kind = RouteMain
		return r
	}
	if e, ok := t.exact[h]; ok {
		return distributionRoute(r, e)
	}
	if e, ok := t.lookupWildcard(h); ok {
		return distributionRoute(r, e)
	}
	for _, d := range s.opts.domains {
		if h == d {
			r.Kind = RouteMain
			return r
		}
		if bucket, ok := strings.CutSuffix(h, "."+d); ok && bucket != "" {
			r.Kind, r.Bucket = RouteVirtualHost, bucket
			return r
		}
	}
	if isIPLiteral(h) || slices.Contains(s.opts.mainHosts, h) {
		r.Kind = RouteMain
		return r
	}
	if s.opts.misdirected {
		r.Kind = RouteMisdirected
		return r
	}
	r.Kind = RoutePathStyle
	return r
}

func distributionRoute(r Route, e routeEntry) Route {
	r.Kind = RouteDistribution
	r.Alias = e.alias
	r.Distribution = e.d
	r.Bucket = e.d.Origin.Bucket
	return r
}
