//go:build upgrade

package upgrade_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMixedClusterPinnedRead: during a rolling upgrade the nodes of the
// release before answer ErrNotImplemented to storage/open-file, so an
// upgraded node cannot pin the shards of their drives by handle. A GET on
// the upgraded node then keeps the lock of the object until it ends, as the
// release before did: an overwrite through another node waits for it, and
// the loss of two nodes in the middle of the read (within the parity of EC
// 4+4) leaves the read whole (regression: the read released its lock with
// the D shards of its first streams only; the overwrite deleted the other
// shards of the version and the loss of the nodes truncated the body).
//
// covers: T-9.8
func TestMixedClusterPinnedRead(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, 4, 2)
	d.start(oldBin)
	// Node 1 runs the release under test, nodes 2-4 the release before.
	n1 := d.node(1)
	n1.stop()
	n1.start(newBin)
	n1.waitReady()
	waitFor(t, 2*time.Minute, "write quorum of the mixed cluster", func() error {
		if n1.clusterHealthy() {
			return nil
		}
		return io.ErrUnexpectedEOF
	})

	const bucket, key = "mixed", "big"
	c1 := n1.rootClient()
	_, err := c1.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	body := payload("mixed-pinned-read", 64<<20)
	_, err = c1.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body)})
	require.NoError(t, err)

	out, err := c1.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err)
	defer func() { _ = out.Body.Close() }()
	head := make([]byte, 1<<20)
	_, err = io.ReadFull(out.Body, head)
	require.NoError(t, err)

	// The overwrite through node 4, the node that survives: it waits for
	// the lock of the read.
	overwritten := make(chan error, 1)
	go func() {
		_, err := d.node(4).rootClient().PutObject(context.WithoutCancel(ctx), &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key),
			Body: bytes.NewReader([]byte("new"))})
		overwritten <- err
	}()
	select {
	case err := <-overwritten:
		t.Fatalf("the overwrite did not wait for the read: %v", err)
	case <-time.After(2 * time.Second):
	}
	d.node(2).kill()
	d.node(3).kill()

	rest, err := io.ReadAll(out.Body)
	require.NoError(t, err, "the read survives the loss of two nodes")
	got := append(head, rest...)
	assert.Equal(t, len(body), len(got))
	assert.Equal(t, digest(body), digest(got), "the whole version the read started with")
	_ = out.Body.Close()
	// The overwrite waited for the read; with two nodes of four gone it
	// may then fail for lack of write quorum (after the retries of the
	// SDK): its outcome is not the point.
	select {
	case err := <-overwritten:
		t.Logf("overwrite after the read: %v", err)
	default:
	}
}
