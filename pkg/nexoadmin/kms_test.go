package nexoadmin_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestKMSAgainstServer drives the KMS methods against a server with the
// builtin KMS, every exchange validated against api/openapi.yaml.
func TestKMSAgainstServer(t *testing.T) {
	h := newHarness(t)
	ctx, c := h.ctx, h.root

	st, err := c.KMSStatus(ctx)
	require.NoError(t, err)
	assert.True(t, st.Configured)
	assert.True(t, st.Online)
	assert.Equal(t, nexoadmin.KMSBackendBuiltin, st.Backend)
	assert.Equal(t, testutil.KMSKeyName, st.DefaultKeyID)

	keys, err := c.ListKMSKeys(ctx, "")
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	ids := map[string]bool{}
	for _, k := range keys {
		ids[k.ID] = k.Default
	}
	assert.True(t, ids[testutil.KMSKeyName], "the default key is listed as default: %v", keys)

	created, err := c.CreateKMSKey(ctx, "app-key")
	require.NoError(t, err)
	assert.Equal(t, "app-key", created.ID)
	assert.False(t, created.Default)
	_, err = c.CreateKMSKey(ctx, "app-key")
	assert.Equal(t, http.StatusConflict, apiError(t, err).Status, "an existing key is a conflict")

	keys, err = c.ListKMSKeys(ctx, "app")
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, "app-key", keys[0].ID)

	// Reserved to the administrators.
	_, err = c.CreateUser(ctx, nexoadmin.CreateUserRequest{AccessKey: "kmsuser", SecretKey: "kmsuser-secret", PolicyNames: []string{"readwrite"}})
	require.NoError(t, err)
	_, err = h.client("kmsuser", "kmsuser-secret").KMSStatus(ctx)
	assert.Equal(t, http.StatusForbidden, apiError(t, err).Status)
	t.Logf("operations exercised: %v", h.contract.operations())
}
