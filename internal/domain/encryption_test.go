package domain

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSEKeysRedacted checks that an SSE-C key printed, logged or encoded
// as JSON by mistake never discloses the key, only its MD5.
func TestSSEKeysRedacted(t *testing.T) {
	raw := bytes.Repeat([]byte{0xA5}, SSECustomerKeySize)
	key, err := NewSSECustomerKey(raw, "")
	require.NoError(t, err)
	req := SSERequest{Type: SSEC, CustomerKey: key}
	type input struct {
		Bucket string
		SSE    SSERequest
	}
	secrets := []string{
		base64.StdEncoding.EncodeToString(raw), hex.EncodeToString(raw), fmt.Sprintf("%v", raw), string(raw),
		"165 165 165", "a5a5a5", "paWl", // decimal, hex and base64 fragments of the key bytes
	}

	var logs bytes.Buffer
	text := slog.New(slog.NewTextHandler(&logs, nil))
	jsonLog := slog.New(slog.NewJSONHandler(&logs, nil))
	text.Info("put", "key", key, "sse", req, "in", input{Bucket: "b", SSE: req})
	jsonLog.Info("put", "key", key, "sse", req, "in", input{Bucket: "b", SSE: req})
	encoded, err := json.Marshal(input{Bucket: "b", SSE: req})
	require.NoError(t, err)

	outputs := map[string]string{
		"%v": fmt.Sprintf("%v", key), "%+v": fmt.Sprintf("%+v", key), "%#v": fmt.Sprintf("%#v", key),
		"%x": fmt.Sprintf("%x", key), "%d": fmt.Sprintf("%d", key), "%s": fmt.Sprintf("%s", key),
		"request %+v": fmt.Sprintf("%+v", req), "request %#v": fmt.Sprintf("%#v", req),
		"nested %+v": fmt.Sprintf("%+v", input{Bucket: "b", SSE: req}), "nested %#v": fmt.Sprintf("%#v", input{Bucket: "b", SSE: req}),
		"logs": logs.String(), "json": string(encoded),
	}
	for name, out := range outputs {
		for _, secret := range secrets {
			assert.NotContains(t, out, secret, name)
		}
	}
	assert.Contains(t, outputs["%v"], key.KeyMD5)
	assert.Contains(t, outputs["logs"], key.KeyMD5)
	assert.Contains(t, outputs["json"], key.KeyMD5)
	assert.Equal(t, "SSECustomerKey{}", SSECustomerKey{}.String())
}
