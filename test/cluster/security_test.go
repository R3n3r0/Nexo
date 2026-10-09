//go:build cluster

package cluster_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestRevocationReachesFrozenNode: a user deleted on node 1 while node 3
// is frozen (SIGSTOP), node 1 dying before it could deliver the
// invalidation to node 3, must not stay usable on node 3: the node
// reloads the state it caches when it notices it was frozen, when its
// peers see it back, and periodically (docs/03 §3.9).
func TestRevocationReachesFrozenNode(t *testing.T) {
	c := newCluster(t, 4)
	c.start()
	ctx := context.Background()
	mustCreateBucket(t, c.node(1).rootClient(), "revoke")
	require.NoError(t, put(ctx, c.node(1).rootClient(), "revoke", "doc", []byte("data")))
	_, err := c.node(1).admin().CreateUser(ctx, nexoadmin.CreateUserRequest{AccessKey: "victim", SecretKey: "victim-secret", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	victimOn3 := client("http://"+c.node(3).S3, "victim", "victim-secret", 1, 10*time.Second)
	waitFor(t, 30*time.Second, "node 3 knows the user", func() error {
		_, _, err := get(ctx, victimOn3, "revoke", "doc")
		return err
	})

	c.node(3).pause()
	require.NoError(t, c.node(1).admin().DeleteUser(ctx, "victim"))
	c.node(1).kill() // the invalidation queued for node 3 dies with it
	c.node(3).resume()

	for _, i := range []int{2, 4} {
		_, _, err := get(ctx, client("http://"+c.node(i).S3, "victim", "victim-secret", 1, 10*time.Second), "revoke", "doc")
		require.Error(t, err, "node %d refuses the deleted user", i)
	}
	eventually(t, 30*time.Second, "node 3 refuses the deleted user", func() bool {
		_, _, err := get(ctx, victimOn3, "revoke", "doc")
		return err != nil
	})
}

// TestRestartedNodeRefusesForeignDeployment: a node restarted alone (its
// peers are down, its bootstrap waits for them) knows its deployment from
// the format of its drives before it accepts any call: a caller of
// another deployment sharing the root credentials, or a caller without
// deployment, cannot write on its drives (docs/03 §3.4).
func TestRestartedNodeRefusesForeignDeployment(t *testing.T) {
	c := newCluster(t, 4)
	c.start()
	for _, nd := range c.nodes {
		nd.stop()
	}
	n1 := c.node(1)
	n1.start()
	waitFor(t, 30*time.Second, "node 1 waits for its peers", func() error {
		if len(n1.logLines("waiting for the peers")) == 0 {
			return errors.New("not waiting yet")
		}
		return nil
	})
	ctx := context.Background()
	ep, err := domain.ParseEndpoint("http://" + n1.S3 + "/proc/self/cwd/p0/d1")
	require.NoError(t, err)
	for name, dep := range map[string]string{"without deployment": "", "of another deployment": "11111111-2222-3333-4444-555555555555"} {
		t.Run(name, func(t *testing.T) {
			auth, err := rpc.NewAuthenticator(rpc.AuthDeps{Logger: slog.New(slog.DiscardHandler)},
				rpc.WithCredentials(rootUser, rootPassword), rpc.WithNodeID("127.0.0.1:1"), rpc.WithDeploymentID(dep))
			require.NoError(t, err)
			conn, err := rpc.NewConn(rpc.ConnDeps{Auth: auth, Logger: slog.New(slog.DiscardHandler)}, n1.S3)
			require.NoError(t, err)
			defer func() { _ = conn.Close(ctx) }()
			drive, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: conn, Logger: slog.New(slog.DiscardHandler)}, ep)
			require.NoError(t, err)
			defer func() { _ = drive.Close(ctx) }()
			err = drive.WriteAll(ctx, domain.SystemBucket, "tmp/"+strings.ReplaceAll(name, " ", "-"), []byte("x"))
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrAccessDenied)
			_, err = drive.ReadAll(ctx, domain.SystemBucket, storage.ConfigDir+"/config.json")
			assert.ErrorIs(t, err, domain.ErrAccessDenied)
		})
	}
}
