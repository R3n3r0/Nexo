package iam

import (
	"context"
	"fmt"
	"slices"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

func errGroupNotFound(name string) error {
	return fmt.Errorf("%w: %q", domain.ErrGroupNotFound, name)
}

func toGroup(r *groupRecord) *Group {
	return &Group{
		Name:        r.Name,
		Status:      r.Status,
		Members:     slices.Clone(r.Members),
		PolicyNames: slices.Clone(r.PolicyNames),
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// checkMembers verifies that every member is an existing user and returns
// them sorted and deduplicated. Callers hold mu.
func (st *state) checkMembers(members []string) ([]string, error) {
	for _, m := range members {
		if st.users[m] == nil {
			return nil, fmt.Errorf("%w: group member %q", domain.ErrUserNotFound, m)
		}
	}
	return sortedUnique(members), nil
}

// CreateGroup implements Service.
func (s *service) CreateGroup(ctx context.Context, in CreateGroupInput) (*Group, error) {
	if err := validateName("group", in.Name, maxGroupNameLen); err != nil {
		return nil, err
	}
	policies, err := normalizePolicyNames(in.PolicyNames)
	if err != nil {
		return nil, err
	}
	var out *Group
	err = s.withWrite(ctx, func(st *state) error {
		if st.groups[in.Name] != nil {
			return fmt.Errorf("%w: %q", domain.ErrGroupAlreadyExists, in.Name)
		}
		members, err := st.checkMembers(in.Members)
		if err != nil {
			return err
		}
		now := s.now()
		rec := &groupRecord{Name: in.Name, Status: domain.StatusEnabled, Members: members, PolicyNames: policies, CreatedAt: now, UpdatedAt: now}
		if err := s.put(ctx, NamespaceGroups, rec.Name, rec); err != nil {
			return err
		}
		st.groups[rec.Name] = rec
		out = toGroup(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.deps.Logger.InfoContext(ctx, "iam: group created", "group", in.Name)
	return out, nil
}

// GetGroup implements Service.
func (s *service) GetGroup(ctx context.Context, name string) (*Group, error) {
	var out *Group
	err := s.withRead(ctx, func(st *state) error {
		rec := st.groups[name]
		if rec == nil {
			return errGroupNotFound(name)
		}
		out = toGroup(rec)
		return nil
	})
	return out, err
}

// ListGroups implements Service.
func (s *service) ListGroups(ctx context.Context) ([]Group, error) {
	var out []Group
	err := s.withRead(ctx, func(st *state) error {
		out = make([]Group, 0, len(st.groups))
		for _, k := range sortedKeys(st.groups) {
			out = append(out, *toGroup(st.groups[k]))
		}
		return nil
	})
	return out, err
}

// DeleteGroup implements Service.
func (s *service) DeleteGroup(ctx context.Context, name string) error {
	return s.withWrite(ctx, func(st *state) error {
		if st.groups[name] == nil {
			return errGroupNotFound(name)
		}
		if err := s.del(ctx, NamespaceGroups, name); err != nil {
			return err
		}
		delete(st.groups, name)
		return nil
	})
}

// updateGroup applies change to a copy of the group record and persists it.
func (s *service) updateGroup(ctx context.Context, name string, change func(st *state, g *groupRecord) error) error {
	return s.withWrite(ctx, func(st *state) error {
		cur := st.groups[name]
		if cur == nil {
			return errGroupNotFound(name)
		}
		g := *cur
		if err := change(st, &g); err != nil {
			return err
		}
		g.UpdatedAt = s.now()
		if err := s.put(ctx, NamespaceGroups, name, &g); err != nil {
			return err
		}
		st.groups[name] = &g
		return nil
	})
}

// AddGroupMembers implements Service.
func (s *service) AddGroupMembers(ctx context.Context, name string, members []string) error {
	return s.updateGroup(ctx, name, func(st *state, g *groupRecord) error {
		add, err := st.checkMembers(members)
		if err != nil {
			return err
		}
		g.Members = union(g.Members, add)
		return nil
	})
}

// RemoveGroupMembers implements Service.
func (s *service) RemoveGroupMembers(ctx context.Context, name string, members []string) error {
	return s.updateGroup(ctx, name, func(_ *state, g *groupRecord) error {
		g.Members = difference(g.Members, members)
		return nil
	})
}

// SetGroupStatus implements Service.
func (s *service) SetGroupStatus(ctx context.Context, name string, status domain.CredentialStatus) error {
	st, err := parseStatus(status, "")
	if err != nil {
		return err
	}
	return s.updateGroup(ctx, name, func(_ *state, g *groupRecord) error {
		g.Status = st
		return nil
	})
}

// SetGroupPolicy implements Service.
func (s *service) SetGroupPolicy(ctx context.Context, name string, policyNames []string) error {
	names, err := normalizePolicyNames(policyNames)
	if err != nil {
		return err
	}
	return s.updateGroup(ctx, name, func(_ *state, g *groupRecord) error {
		g.PolicyNames = names
		return nil
	})
}

// --- policy attachments -------------------------------------------------------

// Attached implements policy.Attachments.
func (s *service) Attached(ctx context.Context, e policy.Entity) ([]string, error) {
	if err := validateEntityKind(e.Kind); err != nil {
		return nil, err
	}
	var out []string
	err := s.withRead(ctx, func(st *state) error {
		switch e.Kind {
		case policy.EntityUser:
			if u := st.users[e.Name]; u != nil {
				out = slices.Clone(u.PolicyNames)
			}
		case policy.EntityGroup:
			if g := st.groups[e.Name]; g != nil {
				out = slices.Clone(g.PolicyNames)
			}
		}
		return nil
	})
	return out, err
}

// ListAttached implements policy.Attachments.
func (s *service) ListAttached(ctx context.Context, kind policy.EntityKind) (map[string][]string, error) {
	if err := validateEntityKind(kind); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	err := s.withRead(ctx, func(st *state) error {
		if kind == policy.EntityUser {
			for k, u := range st.users {
				if len(u.PolicyNames) > 0 {
					out[k] = slices.Clone(u.PolicyNames)
				}
			}
			return nil
		}
		for k, g := range st.groups {
			if len(g.PolicyNames) > 0 {
				out[k] = slices.Clone(g.PolicyNames)
			}
		}
		return nil
	})
	return out, err
}

// Attach implements policy.Attachments.
func (s *service) Attach(ctx context.Context, in policy.AttachInput) error {
	return s.changeAttachments(ctx, in, union)
}

// Detach implements policy.Attachments.
func (s *service) Detach(ctx context.Context, in policy.AttachInput) error {
	return s.changeAttachments(ctx, in, difference)
}

// changeAttachments combines the attached policies of the entity of in
// with in.PolicyNames.
func (s *service) changeAttachments(ctx context.Context, in policy.AttachInput, combine func(cur, names []string) []string) error {
	if (in.User == "") == (in.Group == "") {
		return invalidArg("exactly one of user and group must be set")
	}
	names, err := normalizePolicyNames(in.PolicyNames)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return invalidArg("no policy names")
	}
	if in.User != "" {
		return s.updateUser(ctx, in.User, func(u *userRecord) { u.PolicyNames = combine(u.PolicyNames, names) })
	}
	return s.updateGroup(ctx, in.Group, func(_ *state, g *groupRecord) error {
		g.PolicyNames = combine(g.PolicyNames, names)
		return nil
	})
}

func validateEntityKind(k policy.EntityKind) error {
	if k != policy.EntityUser && k != policy.EntityGroup {
		return invalidArg("unknown entity kind %q", k)
	}
	return nil
}
