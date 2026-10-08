package policy_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// testNow is the fixed evaluation time of the tests.
var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func mustPolicy(t testing.TB, doc string) *domain.Policy {
	t.Helper()
	var p domain.Policy
	require.NoError(t, json.Unmarshal([]byte(doc), &p), doc)
	return &p
}

// doc wraps statements into a policy document.
func doc(statements ...string) string {
	out := `{"Version":"2012-10-17","Statement":[`
	for i, s := range statements {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out + `]}`
}

func newEngine(t testing.TB) (policy.Service, policy.PolicyStore) {
	t.Helper()
	store := policy.NewMemoryStore()
	return policy.New(policy.Deps{Store: store, Clock: clock.NewFake(testNow), Logger: slog.New(slog.DiscardHandler)}), store
}

// engineWith returns an engine where each named document is stored.
func engineWith(t testing.TB, policies map[string]string) policy.Service {
	t.Helper()
	svc, _ := newEngine(t)
	for name, d := range policies {
		require.NoError(t, svc.SetPolicy(context.Background(), name, mustPolicy(t, d)), name)
	}
	return svc
}

func user(name string, policies ...string) *domain.Identity {
	return &domain.Identity{AccessKey: name, PolicyNames: policies}
}
