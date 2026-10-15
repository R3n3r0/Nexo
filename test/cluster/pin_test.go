//go:build cluster

package cluster_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPinnedReadSurvivesNodeLoss: a long GET released its lock once the
// shards were open; an overwrite through another node then removes the
// data it reads, and two nodes of four die in the middle of the transfer.
// The read still has the redundancy of EC 4+4: the shards of the
// surviving nodes, pinned by handle when the read started, deliver the
// whole version it resolved (regression: the spare shards were reopened by
// path and the body was cut short with unexpected EOF).
//
// covers: S-OBJ-02, S-STO-06
func TestPinnedReadSurvivesNodeLoss(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 4)
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "pin")
	body := make([]byte, 64<<20)
	for i := range body {
		body[i] = byte(i*7 + i>>13)
	}
	require.NoError(t, put(ctx, c.node(1).rootClient(), "pin", "big", body))

	reader := client("http://"+c.node(1).S3, rootUser, rootPassword, 1, 2*time.Minute)
	out, err := reader.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("pin"), Key: aws.String("big")})
	require.NoError(t, err)
	defer func() { _ = out.Body.Close() }()
	head := make([]byte, 1<<20)
	_, err = io.ReadFull(out.Body, head)
	require.NoError(t, err)

	timed(t, "overwrite during the read", func() {
		require.NoError(t, put(ctx, c.node(2).rootClient(), "pin", "big", []byte("new")))
	})
	c.node(2).kill()
	c.node(3).kill()

	rest, err := io.ReadAll(out.Body)
	require.NoError(t, err, "the read keeps its quorum with the shards it pinned")
	got := append(head, rest...)
	require.Len(t, got, len(body))
	assert.Equal(t, sha256.Sum256(body), sha256.Sum256(got), "the whole version the read resolved")
	assert.True(t, bytes.Equal(body[:1<<20], head))
}
