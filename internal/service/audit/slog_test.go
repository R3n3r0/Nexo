package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
)

func TestSlogLogsEntry(t *testing.T) {
	var buf bytes.Buffer
	logger := slogJSON(&buf)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc := NewSlog(Deps{Logger: logger, Clock: clock.NewFake(now)})

	err := svc.Log(context.Background(), Entry{
		RequestID: "req-1",
		API:       API{Name: "PutObject", Bucket: "b", Object: "o", StatusCode: 200, InputBytes: 42},
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
	assert.Contains(t, rec["time"], "2026-10-07T12:00:00")
	assert.NotNil(t, rec["tags"])
}
