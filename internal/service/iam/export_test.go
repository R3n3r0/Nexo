package iam_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

func TestExportImport(t *testing.T) {
	ctx := context.Background()
	src, clk := newIAM(t, nil)
	mustUser(t, src, "alice", "readwrite")
	mustUser(t, src, "bob")
	_, err := src.CreateGroup(ctx, iam.CreateGroupInput{Name: "dev", Members: []string{"alice", "bob"}, PolicyNames: []string{"diagnostics"}})
	require.NoError(t, err)
	key, err := src.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID(), ParentUser: "alice", Policy: readOnlyPolicy("arn:aws:s3:::b/*")})
	require.NoError(t, err)
	tmp, err := src.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "bob"}, Duration: time.Hour})
	require.NoError(t, err)
	short, err := src.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "bob"}, Duration: 15 * time.Minute})
	require.NoError(t, err)

	doc, err := src.Export(ctx)
	require.NoError(t, err)
	assert.False(t, bytes.Contains(doc, []byte(key.SecretKey)), "the export is encrypted")
	assert.False(t, bytes.Contains(doc, []byte("alice")))
	var env map[string]any
	require.NoError(t, json.Unmarshal(doc, &env))
	assert.Equal(t, "nexo-iam-export", env["format"])

	// Another deployment with the same root credentials, later in time.
	dst, dstClk := newIAM(t, nil)
	dstClk.Set(clk.Now().Add(20 * time.Minute))
	mustUser(t, dst, "alice") // replaced by the import
	require.NoError(t, dst.Import(ctx, doc))

	users, err := dst.ListUsers(ctx)
	require.NoError(t, err)
	require.Len(t, users, 2)
	assert.Equal(t, []string{"readwrite"}, users[0].PolicyNames)
	_, id, err := dst.LookupCredentials(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"dev"}, id.Groups)
	creds, id, err := dst.LookupCredentials(ctx, key.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, key.SecretKey, creds.SecretKey)
	assert.Equal(t, "alice", id.Parent)
	assert.NotNil(t, id.SessionPolicy)
	creds, _, err = dst.LookupCredentials(ctx, tmp.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, tmp.SessionToken, creds.SessionToken, "same root credentials: tokens stay valid")
	_, _, err = dst.LookupCredentials(ctx, short.AccessKey)
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey, "expired sessions are not imported")
}

func TestImportErrors(t *testing.T) {
	ctx := context.Background()
	src, _ := newIAM(t, nil)
	mustUser(t, src, "alice")
	doc, err := src.Export(ctx)
	require.NoError(t, err)

	other := iam.New(iam.Deps{Logger: discard()}, iam.WithExportKDFParams(testKDF()))
	require.NoError(t, other.Bootstrap(ctx, domain.Credentials{AccessKey: rootAK, SecretKey: "different-secret"}))
	err = other.Import(ctx, doc)
	assert.ErrorIs(t, err, iam.ErrWrongKey)

	rewrite := func(change func(m map[string]any)) []byte {
		var m map[string]any
		require.NoError(t, json.Unmarshal(doc, &m))
		change(m)
		out, err := json.Marshal(m)
		require.NoError(t, err)
		return out
	}
	dst, _ := newIAM(t, nil)
	tests := []struct {
		name string
		data []byte
	}{
		{"not JSON", []byte("{")},
		{"unknown format", rewrite(func(m map[string]any) { m["format"] = "zip" })},
		{"future version", rewrite(func(m map[string]any) { m["version"] = 7 })},
		{"short salt", rewrite(func(m map[string]any) { m["kdf"].(map[string]any)["salt"] = "AAAA" })},
		{"absurd kdf", rewrite(func(m map[string]any) { m["kdf"].(map[string]any)["time"] = 1000 })},
		{"bad nonce", rewrite(func(m map[string]any) { m["nonce"] = "AAAA" })},
		{"oversized", bytes.Repeat([]byte(" "), 65<<20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, dst.Import(ctx, tt.data), domain.ErrInvalidFormat)
		})
	}

	unbootstrapped := iam.New(iam.Deps{Logger: discard()})
	_, err = unbootstrapped.Export(ctx)
	assert.ErrorIs(t, err, iam.ErrNotBootstrapped)
	assert.ErrorIs(t, unbootstrapped.Import(ctx, doc), iam.ErrNotBootstrapped)
}

func TestExportDefaultKDF(t *testing.T) {
	if testing.Short() {
		t.Skip("default Argon2id parameters use 64 MiB")
	}
	ctx := context.Background()
	svc := iam.New(iam.Deps{Logger: discard()})
	require.NoError(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: rootAK, SecretKey: rootSK}))
	doc, err := svc.Export(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(doc), `"memoryKiB":65536`)
	require.NoError(t, svc.Import(ctx, doc))
}

// TestImportCollisions checks that an import never shadows root or an
// access key of another kind.
func TestImportCollisions(t *testing.T) {
	ctx := context.Background()
	src, _ := newIAM(t, nil)
	mustUser(t, src, "taken-key")
	mustUser(t, src, "alice")
	_, err := src.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID(), ParentUser: "alice", AccessKey: "bob"})
	require.NoError(t, err)
	doc, err := src.Export(ctx)
	require.NoError(t, err)

	dst, _ := newIAM(t, nil)
	mustUser(t, dst, "bob")
	mustUser(t, dst, "carol")
	_, err = dst.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID(), ParentUser: "carol", AccessKey: "taken-key"})
	require.NoError(t, err)
	require.NoError(t, dst.Import(ctx, doc))

	_, err = dst.GetAccessKey(ctx, rootID(), "taken-key")
	require.NoError(t, err, "the existing key is kept, the user skipped")
	_, err = dst.GetUser(ctx, "bob")
	require.NoError(t, err, "the existing user is kept, the key skipped")
	_, err = dst.GetAccessKey(ctx, rootID(), "bob")
	assert.ErrorIs(t, err, domain.ErrAccessKeyNotFound)
	_, err = dst.GetUser(ctx, "alice")
	assert.NoError(t, err)
}
