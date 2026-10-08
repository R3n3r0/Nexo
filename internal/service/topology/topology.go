package topology

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// service is the default implementation of Service.
type service struct {
	deps Deps
	opts options
}

// compile-time check: the implementation satisfies the interface.
var _ Service = (*service)(nil)

// New returns the topology service. Nil dependencies are replaced by defaults.
func New(deps Deps, opts ...Option) Service {
	if deps.Hosts == nil {
		deps.Hosts = NewOSHostResolver()
	}
	if deps.IDs == nil {
		deps.IDs = NewUUIDGenerator()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &service{deps: deps, opts: o}
}

// Parse implements Service.
func (s *service) Parse(ctx context.Context, args []string) (*Layout, error) {
	if len(args) == 0 {
		return nil, ErrNoEndpoints
	}

	groups, err := groupPools(args)
	if err != nil {
		return nil, err
	}

	// Expand and parse every pool, checking duplicates and kind consistency globally.
	type rawPool struct {
		args []string
		eps  []domain.Endpoint
	}
	pools := make([]rawPool, 0, len(groups))
	seen := make(map[string]struct{})
	schemes := make(map[string]struct{})
	urlCount, pathCount := 0, 0
	for _, group := range groups {
		rp := rawPool{args: group}
		for _, arg := range group {
			expanded, err := expandArg(arg)
			if err != nil {
				return nil, err
			}
			for _, raw := range expanded {
				ep, err := s.parseEndpoint(raw)
				if err != nil {
					return nil, err
				}
				key := ep.String()
				if _, dup := seen[key]; dup {
					return nil, fmt.Errorf("%w: %s", ErrDuplicateEndpoint, key)
				}
				seen[key] = struct{}{}
				if ep.IsURL() {
					urlCount++
					schemes[ep.Scheme] = struct{}{}
				} else {
					pathCount++
				}
				rp.eps = append(rp.eps, ep)
			}
		}
		pools = append(pools, rp)
	}
	if urlCount > 0 && pathCount > 0 {
		return nil, ErrMixedEndpoints
	}
	if len(schemes) > 1 {
		return nil, fmt.Errorf("%w: %v", ErrMixedSchemes, sortedKeys(schemes))
	}

	layout := &Layout{Mode: detectMode(urlCount, pathCount)}
	for i, rp := range pools {
		pool, err := s.buildPool(i, rp.args, rp.eps, layout.Mode)
		if err != nil {
			return nil, err
		}
		layout.Pools = append(layout.Pools, pool)
	}

	if err := s.markLocal(ctx, layout); err != nil {
		return nil, err
	}
	layout.Nodes = buildNodes(layout)

	if err := layout.Validate(); err != nil {
		return nil, err
	}
	s.deps.Logger.LogAttrs(ctx, slog.LevelInfo, "topology parsed",
		slog.String("mode", layout.Mode.String()),
		slog.Int("pools", len(layout.Pools)),
		slog.Int("nodes", len(layout.Nodes)),
		slog.Int("drives", layout.DriveCount()),
		slog.Int("local_drives", len(layout.LocalEndpoints())),
		slog.String("hash", layout.Hash()))
	return layout, nil
}

// groupPools splits the arguments into pools: one pool per argument when every
// argument has an ellipsis pattern, a single pool when none has.
func groupPools(args []string) ([][]string, error) {
	withEllipsis := 0
	for _, arg := range args {
		if hasEllipsis(arg) {
			withEllipsis++
		}
	}
	switch withEllipsis {
	case 0:
		return [][]string{args}, nil
	case len(args):
		groups := make([][]string, 0, len(args))
		for _, arg := range args {
			groups = append(groups, []string{arg})
		}
		return groups, nil
	default:
		return nil, ErrMixedEllipsis
	}
}

// detectMode applies the rules of docs/08 §8.1.
func detectMode(urlCount, pathCount int) domain.Mode {
	switch {
	case urlCount > 0:
		return domain.ModeDistributed
	case pathCount == 1:
		return domain.ModeSNSD
	default:
		return domain.ModeSNMD
	}
}

// parseEndpoint parses one expanded argument into a canonical endpoint. It is
// stricter than domain.ParseEndpoint: local paths must be absolute and URL
// endpoints get a lower-cased host and an explicit port.
func (s *service) parseEndpoint(raw string) (domain.Endpoint, error) {
	if strings.Contains(raw, "://") {
		return s.parseURLEndpoint(raw)
	}
	if !filepath.IsAbs(raw) {
		return domain.Endpoint{}, fmt.Errorf("%w: %q (use an absolute path or a http(s):// URL)", ErrRelativePath, raw)
	}
	clean := filepath.Clean(raw)
	if clean == string(filepath.Separator) {
		return domain.Endpoint{}, fmt.Errorf("%w: %q", ErrRootPath, raw)
	}
	return domain.Endpoint{Path: clean, IsLocal: true}, nil
}

func (s *service) parseURLEndpoint(raw string) (domain.Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("%w: %q: %v", ErrInvalidEndpoint, raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return domain.Endpoint{}, fmt.Errorf("%w: %q", ErrInvalidScheme, raw)
	}
	if u.User != nil {
		return domain.Endpoint{}, fmt.Errorf("%w: %q: user info is not allowed", ErrInvalidEndpoint, raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return domain.Endpoint{}, fmt.Errorf("%w: %q: query and fragment are not allowed", ErrInvalidEndpoint, raw)
	}
	host := u.Hostname()
	if host == "" {
		return domain.Endpoint{}, fmt.Errorf("%w: %q: missing host", ErrInvalidEndpoint, raw)
	}
	port := s.opts.defaultPort
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return domain.Endpoint{}, fmt.Errorf("%w: %q: invalid port %q", ErrInvalidEndpoint, raw, p)
		}
	}
	if port < 1 || port > 65535 {
		return domain.Endpoint{}, fmt.Errorf("%w: %q: port %d out of range", ErrInvalidEndpoint, raw, port)
	}
	if u.Path == "" || u.Path == "/" {
		return domain.Endpoint{}, fmt.Errorf("%w: %q: a drive path is required after the host", ErrRootPath, raw)
	}
	clean := path.Clean(u.Path)
	if !strings.HasPrefix(clean, "/") {
		return domain.Endpoint{}, fmt.Errorf("%w: %q", ErrRelativePath, raw)
	}
	if clean == "/" {
		return domain.Endpoint{}, fmt.Errorf("%w: %q", ErrRootPath, raw)
	}
	return newURLEndpoint(scheme, host, port, clean), nil
}

// buildPool splits the drives of a pool into erasure sets spread across nodes.
func (s *service) buildPool(idx int, args []string, eps []domain.Endpoint, mode domain.Mode) (Pool, error) {
	ordered, nodeKeys, perNode := distributeRoundRobin(eps)
	total, nodeCount := len(ordered), len(nodeKeys)

	setSize, parity := 1, 0
	if mode != domain.ModeSNSD {
		var err error
		if s.opts.setDriveCount > 0 {
			setSize = s.opts.setDriveCount
			err = validateSetDriveCount(setSize, total, nodeCount)
		} else {
			setSize, err = chooseSetDriveCount(total, nodeCount)
		}
		if err != nil {
			return Pool{}, fmt.Errorf("pool %d (%s): %w", idx+1, strings.Join(args, " "), err)
		}
		// Every node must expose the same number of drives for the round-robin
		// distribution to be uniform.
		for _, key := range nodeKeys {
			if len(perNode[key]) != len(perNode[nodeKeys[0]]) {
				return Pool{}, fmt.Errorf("pool %d: %w", idx+1, ErrAsymmetricPool)
			}
		}
		parity = DefaultParity(setSize)
		if s.opts.parity >= 0 {
			parity = s.opts.parity
		}
		if err := validateParity(parity, setSize); err != nil {
			return Pool{}, fmt.Errorf("pool %d: %w", idx+1, err)
		}
	}

	for i := range ordered {
		ordered[i].PoolIdx = idx
		ordered[i].SetIdx = i / setSize
		ordered[i].DiskIdx = i % setSize
	}
	return Pool{
		Index:         idx,
		Args:          append([]string(nil), args...),
		Endpoints:     ordered,
		NodeCount:     nodeCount,
		SetCount:      total / setSize,
		SetDriveCount: setSize,
		Parity:        parity,
	}, nil
}

// markLocal sets Endpoint.IsLocal on every endpoint and, for distributed
// deployments, checks that exactly one node is local.
func (s *service) markLocal(ctx context.Context, layout *Layout) error {
	if layout.Mode != domain.ModeDistributed {
		for i := range layout.Pools {
			for j := range layout.Pools[i].Endpoints {
				layout.Pools[i].Endpoints[j].IsLocal = true
			}
		}
		return nil
	}

	checker := s.newLocalityChecker(ctx)
	localNodes := make(map[string]struct{})
	for i := range layout.Pools {
		for j := range layout.Pools[i].Endpoints {
			ep := &layout.Pools[i].Endpoints[j]
			local, err := checker.isLocal(ctx, *ep, s.opts.localPort)
			if err != nil {
				return err
			}
			ep.IsLocal = local
			if local {
				localNodes[nodeKey(*ep)] = struct{}{}
			}
		}
	}
	switch len(localNodes) {
	case 0:
		return fmt.Errorf("%w: hostname %q, local port %d, local addresses %v",
			ErrNoLocalEndpoint, checker.hostname, s.opts.localPort, checker.localIPList())
	case 1:
		return nil
	default:
		return fmt.Errorf("%w: %v", ErrAmbiguousLocalNode, sortedKeys(localNodes))
	}
}

// IsLocal implements Service.
func (s *service) IsLocal(ctx context.Context, ep domain.Endpoint) (bool, error) {
	if !ep.IsURL() {
		return true, nil
	}
	return s.newLocalityChecker(ctx).isLocal(ctx, ep, s.opts.localPort)
}

// localityChecker caches the local host identity for the duration of a Parse
// or IsLocal call.
type localityChecker struct {
	svc      *service
	hostname string
	localIPs map[string]struct{}
	lookups  map[string]bool
}

func (s *service) newLocalityChecker(ctx context.Context) *localityChecker {
	c := &localityChecker{svc: s, localIPs: make(map[string]struct{}), lookups: make(map[string]bool)}

	hostname, err := s.deps.Hosts.Hostname()
	if err != nil {
		s.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "cannot determine local hostname", slog.Any("error", err))
	}
	c.hostname = strings.ToLower(hostname)

	addrs, err := s.deps.Hosts.InterfaceAddrs()
	if err != nil {
		s.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "cannot list local interface addresses", slog.Any("error", err))
	}
	for _, addr := range addrs {
		var ip net.IP
		switch a := addr.(type) {
		case *net.IPNet:
			ip = a.IP
		case *net.IPAddr:
			ip = a.IP
		default:
			if parsed := net.ParseIP(strings.Split(addr.String(), "/")[0]); parsed != nil {
				ip = parsed
			}
		}
		if ip != nil {
			c.localIPs[ip.String()] = struct{}{}
		}
	}
	if c.hostname != "" {
		if ips, err := s.deps.Hosts.LookupHost(ctx, c.hostname); err == nil {
			for _, ip := range ips {
				if parsed := net.ParseIP(ip); parsed != nil {
					c.localIPs[parsed.String()] = struct{}{}
				}
			}
		} else {
			s.deps.Logger.LogAttrs(ctx, slog.LevelDebug, "cannot resolve local hostname",
				slog.String("hostname", c.hostname), slog.Any("error", err))
		}
	}
	return c
}

// isLocal reports whether a URL endpoint belongs to this host: its port must
// equal localPort and its host must be the local hostname, a loopback or
// unspecified address, or an address of a local interface (directly or after
// DNS resolution). Resolution failures make the endpoint non-local.
func (c *localityChecker) isLocal(ctx context.Context, ep domain.Endpoint, localPort int) (bool, error) {
	if !ep.IsURL() {
		return true, nil
	}
	if endpointPort(ep) != localPort {
		return false, nil
	}
	host := endpointHostname(ep)
	if host == "localhost" || (c.hostname != "" && host == c.hostname) {
		return true, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return c.isLocalIP(ip), nil
	}
	if local, done := c.lookups[host]; done {
		return local, nil
	}
	ips, err := c.svc.deps.Hosts.LookupHost(ctx, host)
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("resolve %q: %w", host, ctx.Err())
		}
		c.svc.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "cannot resolve endpoint host, assuming remote",
			slog.String("host", host), slog.Any("error", err))
		c.lookups[host] = false
		return false, nil
	}
	local := false
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil && c.isLocalIP(ip) {
			local = true
			break
		}
	}
	c.lookups[host] = local
	return local, nil
}

func (c *localityChecker) isLocalIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	_, ok := c.localIPs[ip.String()]
	return ok
}

func (c *localityChecker) localIPList() []string {
	return sortedKeys(c.localIPs)
}

// buildNodes groups the endpoints of every pool by node, in order of first appearance.
func buildNodes(layout *Layout) []Node {
	index := make(map[string]int)
	var nodes []Node
	for _, p := range layout.Pools {
		for _, ep := range p.Endpoints {
			key := nodeKey(ep)
			i, ok := index[key]
			if !ok {
				i = len(nodes)
				index[key] = i
				nodes = append(nodes, Node{Host: endpointHostname(ep), Port: endpointPort(ep), Local: ep.IsLocal})
			}
			nodes[i].Endpoints = append(nodes[i].Endpoints, ep)
		}
	}
	return nodes
}

// NewFormats implements Service.
func (s *service) NewFormats(_ context.Context, layout *Layout, poolIdx int, deploymentID string) ([]*FormatV1, error) {
	pool, err := poolAt(layout, poolIdx)
	if err != nil {
		return nil, err
	}
	return NewFormats(layout.Mode, pool, deploymentID, s.deps.IDs)
}

// CheckFormats implements Service.
func (s *service) CheckFormats(ctx context.Context, layout *Layout, poolIdx int, formats []*FormatV1) (*FormatV1, error) {
	pool, err := poolAt(layout, poolIdx)
	if err != nil {
		return nil, err
	}
	if len(formats) != len(pool.Endpoints) {
		return nil, fmt.Errorf("%w: %d format(s) for %d drive(s) in pool %d", ErrFormatLayoutMismatch, len(formats), len(pool.Endpoints), poolIdx+1)
	}
	ref, err := CheckFormats(formats)
	if err != nil {
		return nil, fmt.Errorf("pool %d: %w", poolIdx+1, err)
	}
	if err := ref.MatchesPool(layout.Mode, pool); err != nil {
		return nil, fmt.Errorf("pool %d: %w", poolIdx+1, err)
	}
	s.deps.Logger.LogAttrs(ctx, slog.LevelDebug, "format.json consistent",
		slog.Int("pool", poolIdx+1), slog.String("deployment_id", ref.ID),
		slog.Int("sets", ref.SetCount()), slog.Int("set_drives", ref.SetDriveCount()))
	return ref, nil
}

func poolAt(layout *Layout, poolIdx int) (Pool, error) {
	if layout == nil {
		return Pool{}, fmt.Errorf("%w: nil layout", ErrInvalidLayout)
	}
	if poolIdx < 0 || poolIdx >= len(layout.Pools) {
		return Pool{}, fmt.Errorf("%w: pool index %d out of range [0..%d)", ErrInvalidLayout, poolIdx, len(layout.Pools))
	}
	return layout.Pools[poolIdx], nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
