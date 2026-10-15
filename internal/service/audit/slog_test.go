package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/audit"
)

func TestSlogLogsEntry(t *testing.T) {
	var buf bytes.Buffer
	logger := slogJSON(&buf)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc := audit.NewSlog(audit.Deps{Logger: logger, Clock: clock.NewFake(now)})

	err := svc.Log(context.Background(), audit.Entry{
		RequestID: "req-1",
		API:       audit.API{Name: "PutObject", Bucket: "b", Object: "o", StatusCode: 200, InputBytes: 42},
		AccessKey: "admin",
		Tags:      map[string]any{"size": 42},
	})
	require.NoError(t, err)

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "audit", rec["msg"])
	assert.Equal(t, "PutObject", rec["api"])
	assert.Equal(t, "req-1", rec["request_id"])
	assert.Equal(t, "1", rec["version"])
	assert.Equal(t, float64(200), rec["status_code"])
	assert.Equal(t, "admin", rec["access_key"])
	assert.Contains(t, rec["event_time"], "2026-10-07T12:00:00")
	assert.NotNil(t, rec["tags"])
	// Every key once (regression: "time" twice, the time of the record and
	// the one of the request).
	assert.Empty(t, duplicateKeys(t, buf.Bytes()))
}

// duplicateKeys returns the keys a JSON object (and its nested objects)
// holds more than once.
func duplicateKeys(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	var dups []string
	var walk func() // reads one value
	walk = func() {
		tok, err := dec.Token()
		require.NoError(t, err)
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				require.NoError(t, err)
				k, _ := key.(string)
				if seen[k] {
					dups = append(dups, k)
				}
				seen[k] = true
				walk()
			}
			_, err = dec.Token()
			require.NoError(t, err)
		case json.Delim('['):
			for dec.More() {
				walk()
			}
			_, err = dec.Token()
			require.NoError(t, err)
		}
	}
	walk()
	return dups
}
