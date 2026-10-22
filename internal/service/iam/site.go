package iam

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Prefixes of the keys of SiteRecords.
const (
	SiteUserPrefix           = "user/"
	SiteGroupPrefix          = "group/"
	SiteServiceAccountPrefix = "accesskey/"
)

// SiteRecord is one replicated IAM entry (S-STO-11): the record as stored
// and the time of its last change.
type SiteRecord struct {
	Value   []byte
	ModTime time.Time
}

// SiteRecords implements Service.
func (s *service) SiteRecords(ctx context.Context) (map[string]SiteRecord, error) {
	out := map[string]SiteRecord{}
	add := func(key string, rec any, mod time.Time) error {
		raw, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("iam: encode %s: %w", key, err)
		}
		out[key] = SiteRecord{Value: raw, ModTime: mod}
		return nil
	}
	err := s.withRead(ctx, func(st *state) error {
		for k, u := range st.users {
			if err := add(SiteUserPrefix+k, u, u.UpdatedAt); err != nil {
				return err
			}
		}
		for k, g := range st.groups {
			if err := add(SiteGroupPrefix+k, g, g.UpdatedAt); err != nil {
				return err
			}
		}
		for k, sa := range st.sas {
			if err := add(SiteServiceAccountPrefix+k, sa, sa.UpdatedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ApplySiteRecords implements Service. The records are validated first;
// the users are written before the groups and the access keys, the
// deletions of access keys before those of groups and users.
func (s *service) ApplySiteRecords(ctx context.Context, puts map[string][]byte, deletes []string) error {
	var users []*userRecord
	var groups []*groupRecord
	var sas []*serviceAccountRecord
	for key, raw := range puts {
		var err error
		switch {
		case strings.HasPrefix(key, SiteUserPrefix):
			var u userRecord
			if err = json.Unmarshal(raw, &u); err == nil {
				err = siteKeyMatches(key, SiteUserPrefix, u.AccessKey)
			}
			users = append(users, &u)
		case strings.HasPrefix(key, SiteGroupPrefix):
			var g groupRecord
			if err = json.Unmarshal(raw, &g); err == nil {
				err = siteKeyMatches(key, SiteGroupPrefix, g.Name)
			}
			groups = append(groups, &g)
		case strings.HasPrefix(key, SiteServiceAccountPrefix):
			var sa serviceAccountRecord
			if err = json.Unmarshal(raw, &sa); err == nil {
				err = siteKeyMatches(key, SiteServiceAccountPrefix, sa.AccessKey)
			}
			sas = append(sas, &sa)
		default:
			err = invalidArg("unknown IAM record %q", key)
		}
		if err != nil {
			return corrupted("site record "+key, err)
		}
	}
	payload := exportPayload{Users: users, Groups: groups, ServiceAccounts: sas}
	if err := payload.validate(); err != nil {
		return err
	}
	for _, key := range deletes {
		if !strings.HasPrefix(key, SiteUserPrefix) && !strings.HasPrefix(key, SiteGroupPrefix) && !strings.HasPrefix(key, SiteServiceAccountPrefix) {
			return invalidArg("unknown IAM record %q", key)
		}
	}
	return s.withWrite(ctx, func(st *state) error {
		for _, u := range users {
			if s.isRootKey(u.AccessKey) || st.sas[u.AccessKey] != nil || st.sts[u.AccessKey] != nil {
				return invalidArg("access key %q is in use", u.AccessKey)
			}
			if err := s.put(ctx, NamespaceUsers, u.AccessKey, u); err != nil {
				return err
			}
			st.users[u.AccessKey] = u
		}
		for _, g := range groups {
			if err := s.put(ctx, NamespaceGroups, g.Name, g); err != nil {
				return err
			}
			st.groups[g.Name] = g
		}
		for _, sa := range sas {
			if s.isRootKey(sa.AccessKey) || st.users[sa.AccessKey] != nil || st.sts[sa.AccessKey] != nil {
				return invalidArg("access key %q is in use", sa.AccessKey)
			}
			if err := s.put(ctx, NamespaceServiceAccounts, sa.AccessKey, sa); err != nil {
				return err
			}
			st.sas[sa.AccessKey] = sa
		}
		for _, prefix := range []string{SiteServiceAccountPrefix, SiteGroupPrefix, SiteUserPrefix} {
			for _, key := range deletes {
				name, ok := strings.CutPrefix(key, prefix)
				if !ok {
					continue
				}
				switch prefix {
				case SiteServiceAccountPrefix:
					if st.sas[name] == nil {
						continue
					}
					if err := s.del(ctx, NamespaceServiceAccounts, name); err != nil {
						return err
					}
					delete(st.sas, name)
				case SiteGroupPrefix:
					if st.groups[name] == nil {
						continue
					}
					if err := s.del(ctx, NamespaceGroups, name); err != nil {
						return err
					}
					delete(st.groups, name)
				default:
					if st.users[name] == nil {
						continue
					}
					if err := s.del(ctx, NamespaceUsers, name); err != nil {
						return err
					}
					delete(st.users, name)
				}
			}
		}
		return nil
	})
}

func siteKeyMatches(key, prefix, name string) error {
	if key != prefix+name {
		return fmt.Errorf("the record of %q names %q", key, name)
	}
	return nil
}
