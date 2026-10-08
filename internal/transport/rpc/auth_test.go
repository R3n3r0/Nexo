package rpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

func TestAuthenticator(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	auth := func(node string, opts ...rpc.AuthOption) rpc.Authenticator {
		base := []rpc.AuthOption{rpc.WithCredentials(rootUser, rootPassword), rpc.WithNodeID(node), rpc.WithBuildVersion("v1.4.0")}
		a, err := rpc.NewAuthenticator(rpc.AuthDeps{Clock: clk, Logger: discard()}, append(base, opts...)...)
		require.NoError(t, err)
		return a
	}
	alice, bob := auth("alice:9000"), auth("bob:9000")
	assert.Equal(t, "alice:9000", alice.NodeID())

	token, err := alice.Token(ctx)
	require.NoError(t, err)
	claims, err := bob.Verify(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, rpc.Claims{NodeID: "alice:9000", Protocol: rpc.ProtocolVersion, Version: "v1.4.0", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}, claims)
	again, err := alice.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, token, again, "tokens are reused while fresh")
	clk.Advance(31 * time.Second)
	renewed, err := alice.Token(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, token, renewed, "renewed after half of their life")

	// Expired tokens, beyond the tolerated clock skew.
	clk.Advance(2*time.Minute + time.Second)
	_, err = bob.Verify(ctx, token)
	assert.ErrorIs(t, err, rpc.ErrInvalidToken)
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)

	// Other credentials, garbage, no token.
	mallory, err := rpc.NewAuthenticator(rpc.AuthDeps{Clock: clk}, rpc.WithCredentials(rootUser, "guessed"), rpc.WithNodeID("mallory:9000"))
	require.NoError(t, err)
	forged, err := mallory.Token(ctx)
	require.NoError(t, err)
	for name, tok := range map[string]string{"forged": forged, "garbage": "not-a-jwt", "empty": ""} {
		_, err = bob.Verify(ctx, tok)
		assert.ErrorIs(t, err, rpc.ErrInvalidToken, name)
	}

	// Deployment binding: refused once both know their (different) ids,
	// accepted from a node that does not know it yet (bootstrap).
	alice.SetDeploymentID(ctx, "dep-1")
	bob.SetDeploymentID(ctx, "dep-2")
	token, err = alice.Token(ctx)
	require.NoError(t, err)
	_, err = bob.Verify(ctx, token)
	assert.ErrorIs(t, err, rpc.ErrDeploymentMismatch)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	bob.SetDeploymentID(ctx, "dep-1")
	claims, err = bob.Verify(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, "dep-1", claims.DeploymentID)
	carol := auth("carol:9000")
	token, err = carol.Token(ctx)
	require.NoError(t, err)
	_, err = bob.Verify(ctx, token)
	require.NoError(t, err, "a node without deployment id proves the root credentials")

	// Major versions must match.
	dave := auth("dave:9000", rpc.WithBuildVersion("v2.0.0"))
	token, err = dave.Token(ctx)
	require.NoError(t, err)
	_, err = carol.Verify(ctx, token)
	assert.ErrorIs(t, err, rpc.ErrIncompatibleVersion)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = alice.Token(canceled)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestAuthenticatorValidation(t *testing.T) {
	_, err := rpc.NewAuthenticator(rpc.AuthDeps{}, rpc.WithNodeID("n:1"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "credentials required")
	_, err = rpc.NewAuthenticator(rpc.AuthDeps{}, rpc.WithCredentials("a", "b"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "node id required")
}
