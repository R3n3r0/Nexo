package clitest

import (
	"context"
	"fmt"
	"slices"

	"github.com/stretchr/testify/mock"

	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The site replication of the store (nexoctl admin replicate): the group
// is saved as given, the deployment IDs derived from the site names; the
// status of the other sites is set by the tests (SetSiteReplicationPeers).

// memSiteState is the site replication of MemAdmin.
type memSiteState struct {
	info  *nexoadmin.SiteReplicationInfo
	syncs int
}

// SetSiteReplicationPeers sets the status of the other sites of the group.
func (m *MemAdmin) SetSiteReplicationPeers(peers []nexoadmin.SiteReplicationPeerStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.site.info != nil {
		m.site.info.Peers = slices.Clone(peers)
	}
}

// SiteReplicationSyncs counts the rounds asked with SyncSiteReplication.
func (m *MemAdmin) SiteReplicationSyncs() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.site.syncs
}

// expectSiteReplication registers the site replication methods on c.
func (s *memSession) expectSiteReplication(c *climock.AdminClient) {
	x := mock.Anything
	c.EXPECT().GetSiteReplication(x).RunAndReturn(s.getSiteReplication).Maybe()
	c.EXPECT().AddSiteReplication(x, x).RunAndReturn(s.addSiteReplication).Maybe()
	c.EXPECT().RemoveSiteReplication(x).RunAndReturn(s.removeSiteReplication).Maybe()
	c.EXPECT().SyncSiteReplication(x).RunAndReturn(s.syncSiteReplication).Maybe()
}

func memDeploymentID(name string) string { return fmt.Sprintf("dep-%s", name) }

func (s *memSession) getSiteReplication(context.Context) (*nexoadmin.SiteReplicationInfo, error) {
	if err := s.replBegin("GetSiteReplication", "admin:SiteReplicationInfo"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if s.m.site.info == nil {
		return &nexoadmin.SiteReplicationInfo{DeploymentID: memDeploymentID("mem"), Sites: []nexoadmin.SiteReplicationSite{},
			Peers: []nexoadmin.SiteReplicationPeerStatus{}}, nil
	}
	out := *s.m.site.info
	out.Sites, out.Peers = slices.Clone(out.Sites), slices.Clone(out.Peers)
	return &out, nil
}

func (s *memSession) addSiteReplication(_ context.Context, sites []nexoadmin.SiteReplicationSite) (*nexoadmin.SiteReplicationInfo, error) {
	if err := s.replBegin("AddSiteReplication", "admin:SiteReplicationAdd"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if s.m.site.info != nil {
		return nil, conflict("this site is already in a site replication group: remove it first")
	}
	if len(sites) < 2 {
		return nil, invalid("site replication needs at least two sites")
	}
	info := &nexoadmin.SiteReplicationInfo{Enabled: true, Name: sites[0].Name, DeploymentID: memDeploymentID(sites[0].Name),
		Peers: []nexoadmin.SiteReplicationPeerStatus{}}
	for _, site := range sites {
		site.DeploymentID = memDeploymentID(site.Name)
		info.Sites = append(info.Sites, site)
		if site.Name != info.Name {
			info.Peers = append(info.Peers, nexoadmin.SiteReplicationPeerStatus{Name: site.Name, DeploymentID: site.DeploymentID})
		}
	}
	slices.SortFunc(info.Sites, func(a, b nexoadmin.SiteReplicationSite) int {
		if a.Name < b.Name {
			return -1
		}
		return 1
	})
	s.m.site.info = info
	out := *info
	return &out, nil
}

func (s *memSession) removeSiteReplication(context.Context) error {
	if err := s.replBegin("RemoveSiteReplication", "admin:SiteReplicationRemove"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	if s.m.site.info == nil {
		return notFound("not_found", "this site is in no site replication group")
	}
	s.m.site.info = nil
	return nil
}

func (s *memSession) syncSiteReplication(context.Context) error {
	if err := s.replBegin("SyncSiteReplication", "admin:SiteReplicationAdd"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	if s.m.site.info == nil {
		return notFound("not_found", "this site is in no site replication group")
	}
	s.m.site.syncs++
	return nil
}
