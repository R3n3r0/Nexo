package siterepl

import (
	"context"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// adminDialer opens the Admin API of the other sites with the root
// credentials.
type adminDialer struct {
	user, password string
}

// Dial implements Dialer.
func (d adminDialer) Dial(_ context.Context, site Site) (Peer, error) {
	c, err := nexoadmin.New(nexoadmin.Config{Endpoint: site.AdminEndpoint, AccessKey: d.user, SecretKey: d.password,
		HTTPClient: &http.Client{Timeout: 2 * time.Minute}, UserAgent: "nexo-site-replication"})
	if err != nil {
		return nil, err
	}
	return adminPeer{c: c}, nil
}

type adminPeer struct {
	c nexoadmin.AdminClient
}

func toWire(sites []Site) []nexoadmin.SiteReplicationSite {
	out := make([]nexoadmin.SiteReplicationSite, 0, len(sites))
	for _, s := range sites {
		out = append(out, nexoadmin.SiteReplicationSite(s))
	}
	return out
}

func fromWire(sites []nexoadmin.SiteReplicationSite) []Site {
	out := make([]Site, 0, len(sites))
	for _, s := range sites {
		out = append(out, Site(s))
	}
	return out
}

func (p adminPeer) Info(ctx context.Context) (PeerInfo, error) {
	in, err := p.c.SiteReplicationPeerInfo(ctx)
	if err != nil {
		return PeerInfo{}, err
	}
	return PeerInfo{DeploymentID: in.DeploymentID, Enabled: in.Enabled, Sites: fromWire(in.Sites)}, nil
}

func (p adminPeer) Join(ctx context.Context, sites []Site) error {
	return p.c.JoinSiteReplication(ctx, toWire(sites))
}

func (p adminPeer) Leave(ctx context.Context) error { return p.c.LeaveSiteReplication(ctx) }

func (p adminPeer) State(ctx context.Context) (map[string]Entry, error) {
	st, err := p.c.SiteReplicationState(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Entry, len(st.Entries))
	for k, e := range st.Entries {
		out[k] = Entry(e)
	}
	return out, nil
}

func (p adminPeer) Items(ctx context.Context, keys []string) ([]byte, error) {
	return p.c.SiteReplicationItems(ctx, keys)
}

func (p adminPeer) Apply(ctx context.Context, sealed []byte) ([]Result, error) {
	rs, err := p.c.SiteReplicationApply(ctx, sealed)
	out := make([]Result, 0, len(rs))
	for _, r := range rs {
		out = append(out, Result(r))
	}
	return out, err
}
