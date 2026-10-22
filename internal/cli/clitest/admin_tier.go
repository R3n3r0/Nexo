package clitest

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/stretchr/testify/mock"

	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The remote tiers of the store (nexoctl admin tier): saved as given, the
// secret dropped; a tier the tests mark as holding data
// (SetTierHoldsData) cannot be removed.

// memTierState is the tiers of MemAdmin.
type memTierState struct {
	tiers []nexoadmin.Tier
	data  map[string]bool
}

// SetTierHoldsData marks a tier as holding the data of transitioned
// versions.
func (m *MemAdmin) SetTierHoldsData(name string, holds bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tier.data == nil {
		m.tier.data = map[string]bool{}
	}
	m.tier.data[name] = holds
}

// expectTiers registers the tier methods on c.
func (s *memSession) expectTiers(c *climock.AdminClient) {
	x := mock.Anything
	c.EXPECT().ListTiers(x).RunAndReturn(s.listTiers).Maybe()
	c.EXPECT().AddTier(x, x).RunAndReturn(s.addTier).Maybe()
	c.EXPECT().EditTier(x, x, x).RunAndReturn(s.editTier).Maybe()
	c.EXPECT().RemoveTier(x, x).RunAndReturn(s.removeTier).Maybe()
}

func (s *memSession) listTiers(context.Context) ([]nexoadmin.Tier, error) {
	if err := s.replBegin("ListTiers", "admin:ListTier"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	return slices.Clone(s.m.tier.tiers), nil
}

func (s *memSession) findTier(name string) int {
	return slices.IndexFunc(s.m.tier.tiers, func(t nexoadmin.Tier) bool { return t.Name == name })
}

func (s *memSession) addTier(_ context.Context, in nexoadmin.TierInput) (*nexoadmin.Tier, error) {
	if err := s.replBegin("AddTier", "admin:SetTier"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if in.Name == "" || strings.ToUpper(in.Name) != in.Name {
		return nil, invalid("invalid tier name %q: upper case letters, digits, '-' and '_' only", in.Name)
	}
	if in.AccessKey == "" || in.SecretKey == "" {
		return nil, invalid("the credentials of the tier are required")
	}
	if s.findTier(in.Name) >= 0 {
		return nil, conflict("tier %s exists", in.Name)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t := nexoadmin.Tier{Name: in.Name, Endpoint: in.Endpoint, Bucket: in.Bucket, Prefix: in.Prefix, Region: in.Region,
		StorageClass: in.StorageClass, AccessKey: in.AccessKey, Created: now, Updated: now}
	s.m.tier.tiers = append(s.m.tier.tiers, t)
	slices.SortFunc(s.m.tier.tiers, func(a, b nexoadmin.Tier) int { return strings.Compare(a.Name, b.Name) })
	return &t, nil
}

func (s *memSession) editTier(_ context.Context, name string, in nexoadmin.TierInput) (*nexoadmin.Tier, error) {
	if err := s.replBegin("EditTier", "admin:SetTier"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	i := s.findTier(name)
	if i < 0 {
		return nil, notFound("not_found", "tier %s", name)
	}
	t := &s.m.tier.tiers[i]
	t.AccessKey = in.AccessKey
	if in.Region != "" {
		t.Region = in.Region
	}
	if in.StorageClass != "" {
		t.StorageClass = in.StorageClass
	}
	out := *t
	return &out, nil
}

func (s *memSession) removeTier(_ context.Context, name string) error {
	if err := s.replBegin("RemoveTier", "admin:SetTier"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	i := s.findTier(name)
	if i < 0 {
		return notFound("not_found", "tier %s", name)
	}
	if s.m.tier.data[name] {
		return conflict("tier %s holds the data of transitioned versions: bring them back or delete them first", name)
	}
	s.m.tier.tiers = slices.Delete(s.m.tier.tiers, i, i+1)
	return nil
}
