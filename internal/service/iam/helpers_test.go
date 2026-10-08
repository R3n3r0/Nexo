package iam_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

const (
	rootAK = "rootadmin"
	rootSK = "rootsecret123"
)

// testKDF keeps Argon2id cheap in tests.
func testKDF() iam.KDFParams { return iam.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1} }

func t0() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func rootID() domain.Identity { return domain.Identity{AccessKey: rootAK, IsRoot: true} }

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newIAM returns a bootstrapped service on store (memory when nil) and its
// fake clock.
func newIAM(t *testing.T, store iam.Store) (iam.Service, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(t0())
	svc := iam.New(iam.Deps{Store: store, Clock: clk, Logger: discard()}, iam.WithExportKDFParams(testKDF()))
	require.NoError(t, svc.Bootstrap(context.Background(), domain.Credentials{AccessKey: rootAK, SecretKey: rootSK}))
	return svc, clk
}

// mustUser creates an enabled user.
func mustUser(t *testing.T, svc iam.Service, ak string, policies ...string) {
	t.Helper()
	_, err := svc.CreateUser(context.Background(), iam.CreateUserInput{AccessKey: ak, SecretKey: ak + "-secret", PolicyNames: policies})
	require.NoError(t, err)
}

func readOnlyPolicy(resource string) *domain.Policy {
	return &domain.Policy{Version: domain.PolicyVersion, Statement: domain.Statements{{
		Effect: domain.Allow, Action: domain.StringSet{"s3:GetObject"}, Resource: domain.StringSet{resource},
	}}}
}
