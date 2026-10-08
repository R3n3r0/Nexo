package iam

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

// callers returns the identities the ownership tests use.
func callers() (root, user, derived, anonymous domain.Identity) {
	return domain.Identity{AccessKey: "admin", IsRoot: true},
		domain.Identity{AccessKey: "bob"},
		domain.Identity{AccessKey: "AKIABOB", Parent: "bob"},
		domain.Identity{IsAnonymous: true}
}

func TestKeyParent(t *testing.T) {
	testRoot, testBob, testDerived, testAnon := callers()
	tests := []struct {
		name      string
		caller    domain.Identity
		requested string
		listing   bool
		want      string
		denied    bool
	}{
		{name: "root creates for itself", caller: testRoot, want: "admin"},
		{name: "root creates for another user", caller: testRoot, requested: "alice", want: "alice"},
		{name: "root lists every key", caller: testRoot, listing: true, want: ""},
		{name: "root lists a user", caller: testRoot, requested: "alice", listing: true, want: "alice"},
		{name: "user defaults to itself", caller: testBob, want: "bob"},
		{name: "user lists its own keys", caller: testBob, listing: true, want: "bob"},
		{name: "user names itself", caller: testBob, requested: "bob", want: "bob"},
		{name: "user names another user", caller: testBob, requested: "alice", denied: true},
		{name: "user lists another user", caller: testBob, requested: "alice", listing: true, denied: true},
		{name: "derived credentials", caller: testDerived, denied: true},
		{name: "anonymous", caller: testAnon, denied: true},
		{name: "empty identity", caller: domain.Identity{}, denied: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := keyParent(tt.caller, tt.requested, tt.listing)
			if tt.denied {
				assert.ErrorIs(t, err, domain.ErrAccessDenied)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckKeyOwner(t *testing.T) {
	testRoot, testBob, testDerived, testAnon := callers()
	tests := []struct {
		name   string
		caller domain.Identity
		parent string
		denied bool
	}{
		{name: "root owns everything", caller: testRoot, parent: "alice"},
		{name: "user owns its key", caller: testBob, parent: "bob"},
		{name: "user and another user's key", caller: testBob, parent: "alice", denied: true},
		{name: "derived credentials", caller: testDerived, parent: "bob", denied: true},
		{name: "anonymous", caller: testAnon, parent: "", denied: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkKeyOwner(tt.caller, tt.parent)
			if tt.denied {
				assert.ErrorIs(t, err, domain.ErrAccessDenied)
				return
			}
			assert.NoError(t, err)
		})
	}
}
