package admin_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// TestKMSContract exercises the KMS endpoints and the KMS of the server
// information against a server with the builtin KMS, every exchange
// validated against api/openapi.yaml: the creation of a key is reserved
// to the administrators.
func TestKMSContract(t *testing.T) {
	srv := testutil.StartServer(t, testutil.WithKMS())
	root := newConsole(t, srv, srv.RootUser, srv.RootPassword)

	st := decodeAs[admin.KMSStatusResponse](t, root.call(http.MethodGet, "/kms/status", nil, http.StatusOK))
	assert.True(t, st.Configured)
	assert.True(t, st.Online)
	assert.Equal(t, "builtin", st.Backend)
	assert.Equal(t, testutil.KMSKeyName, st.DefaultKeyID)

	keys := decodeAs[admin.KMSKeyListResponse](t, root.call(http.MethodGet, "/kms/keys", nil, http.StatusOK))
	require.Len(t, keys.Items, 1)
	assert.Equal(t, admin.KMSKeyResponse{ID: testutil.KMSKeyName, Default: true}, keys.Items[0])

	created := decodeAs[admin.KMSKeyResponse](t, root.call(http.MethodPost, "/kms/keys", map[string]string{"id": "app-key"}, http.StatusCreated))
	assert.Equal(t, "app-key", created.ID)
	assert.NotNil(t, created.CreatedAt)
	root.call(http.MethodPost, "/kms/keys", map[string]string{"id": "app-key"}, http.StatusConflict)
	root.call(http.MethodPost, "/kms/keys", map[string]string{"id": "bad/key"}, http.StatusBadRequest)

	keys = decodeAs[admin.KMSKeyListResponse](t, root.call(http.MethodGet, "/kms/keys?prefix=app", nil, http.StatusOK))
	require.Len(t, keys.Items, 1)
	assert.Equal(t, "app-key", keys.Items[0].ID)

	info := decodeAs[admin.ServerInfoResponse](t, root.call(http.MethodGet, "/info", nil, http.StatusOK))
	require.NotNil(t, info.KMS)
	assert.True(t, info.KMS.Configured)
	assert.ElementsMatch(t, []string{testutil.KMSKeyName, "app-key"}, info.KMS.Keys)

	// A user without the KMS actions reads nor creates keys.
	root.call(http.MethodPost, "/users", map[string]any{"accessKey": "kms-reader", "secretKey": "kms-reader-secret", "policyNames": []string{"readonly"}}, http.StatusCreated)
	reader := newConsole(t, srv, "kms-reader", "kms-reader-secret")
	reader.call(http.MethodPost, "/kms/keys", map[string]string{"id": "other"}, http.StatusForbidden)
	reader.call(http.MethodGet, "/kms/keys", nil, http.StatusForbidden)
}
