package domain

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// Endpoint is a storage location: either a local filesystem path or an
// http(s) URL pointing at a drive served by another node. PoolIdx, SetIdx and
// DiskIdx are the position in the topology, assigned when the layout is
// computed.
type Endpoint struct {
	// Scheme is "" for local paths, "http" or "https" for remote drives.
	Scheme string
	// Host is host:port for URL endpoints, empty for local paths.
	Host string
	// Path is the drive path (cleaned, absolute for URL endpoints).
	Path string
	// IsLocal reports whether the drive is attached to this node. Local
	// paths are always local; for URLs the topology decides.
	IsLocal bool
	PoolIdx int
	SetIdx  int
	DiskIdx int
}

// ParseEndpoint parses a raw endpoint. Values containing "://" are parsed as
// URLs and must use http or https, have a host and a non-root path; anything
// else is a local path.
func ParseEndpoint(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("%w: empty endpoint", ErrInvalidArgument)
	}
	if !strings.Contains(raw, "://") {
		clean := filepath.Clean(raw)
		if clean == string(filepath.Separator) || clean == "." {
			return Endpoint{}, fmt.Errorf("%w: endpoint path %q is not usable as a drive", ErrInvalidArgument, raw)
		}
		return Endpoint{Path: clean, IsLocal: true}, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%w: endpoint %q: %v", ErrInvalidArgument, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Endpoint{}, fmt.Errorf("%w: endpoint %q must use http or https", ErrInvalidArgument, raw)
	}
	if u.Host == "" || u.Hostname() == "" {
		return Endpoint{}, fmt.Errorf("%w: endpoint %q has no host", ErrInvalidArgument, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Endpoint{}, fmt.Errorf("%w: endpoint %q must not carry user info, query or fragment", ErrInvalidArgument, raw)
	}
	p := path.Clean(u.Path)
	if p == "/" || p == "." || p == "" {
		return Endpoint{}, fmt.Errorf("%w: endpoint %q has no drive path", ErrInvalidArgument, raw)
	}
	return Endpoint{Scheme: u.Scheme, Host: u.Host, Path: p}, nil
}

// IsURL reports whether the endpoint is a remote URL.
func (e Endpoint) IsURL() bool { return e.Scheme != "" }

// String renders the endpoint as it was given: a URL or a plain path.
func (e Endpoint) String() string {
	if !e.IsURL() {
		return e.Path
	}
	return (&url.URL{Scheme: e.Scheme, Host: e.Host, Path: e.Path}).String()
}

// Hostname returns the host without port, empty for local paths.
func (e Endpoint) Hostname() string {
	if e.Host == "" {
		return ""
	}
	return (&url.URL{Host: e.Host}).Hostname()
}

// Port returns the port of a URL endpoint, empty when absent.
func (e Endpoint) Port() string {
	if e.Host == "" {
		return ""
	}
	return (&url.URL{Host: e.Host}).Port()
}
