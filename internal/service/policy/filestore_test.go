package policy_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

func TestFileStore(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "policies-root")
	st, err := policy.NewFileStore(dir)
	require.NoError(t, err)
	d := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`))

	names := []string{"readers", "Mixed.Case", ".hidden", "a+b=c,d@e_f-g"}
	for _, n := range names {
		require.NoError(t, st.Set(ctx, policy.Named{Name: n, Policy: d, UpdatedAt: testNow}))
	}
	assert.ErrorIs(t, st.Set(ctx, policy.Named{Name: "../escape", Policy: d}), domain.ErrInvalidArgument)
	assert.FileExists(t, filepath.Join(dir, "policies", "readers.json"))
	assert.FileExists(t, filepath.Join(dir, "policies", "a+b=c,d@e_f-g.json"))
	entries, err := os.ReadDir(filepath.Join(dir, "policies"))
	require.NoError(t, err)
	assert.Len(t, entries, len(names), "one file per policy, no temporary leftovers")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "policies", "readers.json"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	alice := policy.Entity{Kind: policy.EntityUser, Name: "alice"}
	team := policy.Entity{Kind: policy.EntityGroup, Name: "team"}
	require.NoError(t, st.SetAttached(ctx, alice, []string{"readers", "Mixed.Case"}))
	require.NoError(t, st.SetAttached(ctx, team, []string{"readers"}))
	require.NoError(t, st.SetAttached(ctx, policy.Entity{Kind: policy.EntityGroup, Name: "empty"}, []string{"x"}))
	require.NoError(t, st.SetAttached(ctx, policy.Entity{Kind: policy.EntityGroup, Name: "empty"}, nil))
	assert.ErrorIs(t, st.SetAttached(ctx, policy.Entity{Kind: "role", Name: "x"}, nil), domain.ErrInvalidArgument)
	require.NoError(t, st.Delete(ctx, ".hidden"))
	assert.ErrorIs(t, st.Delete(ctx, ".hidden"), domain.ErrPolicyNotFound)

	// A new store over the same directory reloads everything.
	reopened, err := policy.NewFileStore(dir)
	require.NoError(t, err)
	list, err := reopened.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"Mixed.Case", "a+b=c,d@e_f-g", "readers"}, []string{list[0].Name, list[1].Name, list[2].Name})
	got, err := reopened.Get(ctx, "Mixed.Case")
	require.NoError(t, err)
	assert.Equal(t, d, got.Policy)
	assert.True(t, testNow.Equal(got.UpdatedAt))
	_, err = reopened.Get(ctx, ".hidden")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	attached, err := reopened.Attached(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, []string{"Mixed.Case", "readers"}, attached)
	groups, err := reopened.ListAttached(ctx, policy.EntityGroup)
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"team": {"readers"}}, groups)

	// The engine works on top of the file store.
	svc := policy.New(policy.Deps{Store: reopened, Clock: clock.NewFake(testNow)})
	dec, err := svc.Evaluate(ctx, policy.Args{Identity: user("alice"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	require.NoError(t, svc.DeletePolicy(ctx, "readers"))
	final, err := policy.NewFileStore(dir)
	require.NoError(t, err)
	attached, _ = final.Attached(ctx, alice)
	assert.Equal(t, []string{"Mixed.Case"}, attached)
	groups, _ = final.ListAttached(ctx, policy.EntityGroup)
	assert.Empty(t, groups)

	// Cancelled contexts do not write.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	assert.ErrorIs(t, final.Set(cancelled, policy.Named{Name: "x", Policy: d}), context.Canceled)
	assert.ErrorIs(t, final.Delete(cancelled, "Mixed.Case"), context.Canceled)
	assert.ErrorIs(t, final.SetAttached(cancelled, alice, nil), context.Canceled)
}

func TestFileStoreCorruption(t *testing.T) {
	_, err := policy.NewFileStore("")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	write := func(t *testing.T, dir, rel, content string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o600))
	}
	tests := []struct {
		name  string
		files map[string]string
	}{
		{"policy not json", map[string]string{"policies/p.json": "{"}},
		{"name mismatch", map[string]string{"policies/p.json": `{"name":"q","policy":{"Version":"2012-10-17","Statement":[]}}`}},
		{"invalid name", map[string]string{"policies/p.json": `{"name":"bad name","policy":{"Version":"2012-10-17","Statement":[]}}`}},
		{"missing document", map[string]string{"policies/p.json": `{"name":"p"}`}},
		{"users not json", map[string]string{"users.json": "["}},
		{"groups not a map", map[string]string{"groups.json": `["a"]`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, content := range tc.files {
				write(t, dir, rel, content)
			}
			_, err := policy.NewFileStore(dir)
			assert.ErrorIs(t, err, domain.ErrInvalidFormat)
		})
	}

	// Temporary files and unrelated entries are ignored.
	dir := t.TempDir()
	write(t, dir, "policies/.tmp-123", "garbage")
	write(t, dir, "policies/README.txt", "notes")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "policies", "sub.json"), 0o700))
	_, err = policy.NewFileStore(dir)
	require.NoError(t, err)

	// The directory must be creatable.
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, err = policy.NewFileStore(filepath.Join(file, "sub"))
	assert.Error(t, err)
}

func TestFileStoreWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permissions are not enforced")
	}
	ctx := context.Background()
	dir := t.TempDir()
	st, err := policy.NewFileStore(dir)
	require.NoError(t, err)
	d := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`))
	require.NoError(t, st.Set(ctx, policy.Named{Name: "p", Policy: d}))
	require.NoError(t, os.Chmod(filepath.Join(dir, "policies"), 0o500))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o700)
		_ = os.Chmod(filepath.Join(dir, "policies"), 0o700)
	})
	assert.Error(t, st.Set(ctx, policy.Named{Name: "q", Policy: d}))
	assert.Error(t, st.Delete(ctx, "p"))
	assert.Error(t, st.SetAttached(ctx, policy.Entity{Kind: policy.EntityUser, Name: "u"}, []string{"p"}))
	_, err = st.Get(ctx, "q")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound, "a failed write is not visible")
	_, err = st.Get(ctx, "p")
	require.NoError(t, err, "a failed delete keeps the policy")
}
