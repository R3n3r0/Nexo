package iam

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

func TestRootOnly(t *testing.T) {
	ctx := context.Background()
	svc := NewRootOnly(Deps{})

	_, _, err := svc.LookupCredentials(ctx, "admin")
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey)
	assert.ErrorIs(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: "admin"}), domain.ErrInvalidArgument)

	require.NoError(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: "admin", SecretKey: "password123"}))
	creds, ident, err := svc.LookupCredentials(ctx, "admin")
	require.NoError(t, err)
	assert.Equal(t, "password123", creds.SecretKey)
	assert.True(t, creds.IsEnabled())
	assert.True(t, ident.IsRoot)

	_, _, err = svc.LookupCredentials(ctx, "other")
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey)

	u, err := svc.GetUser(ctx, "admin")
	require.NoError(t, err)
	assert.Equal(t, "admin", u.AccessKey)
	_, err = svc.GetUser(ctx, "bob")
	assert.ErrorIs(t, err, domain.ErrUserNotFound)

	_, err = svc.CreateUser(ctx, CreateUserInput{AccessKey: "bob"})
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = svc.AssumeRole(ctx, AssumeRoleInput{})
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
}
