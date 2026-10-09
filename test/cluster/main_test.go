//go:build cluster

package cluster_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
	"github.com/r3n3r0/nexo/internal/version"
)

// nexoBin is the server binary the tests run: built once by TestMain, or
// NEXO_CLUSTER_BIN.
var nexoBin string

// Environment of the lock holder helper process (TC-09): the test binary
// re-executed holds a distributed write lock as a node would.
const (
	envLockHolder     = "NEXO_CLUSTER_LOCK_HOLDER"
	envLockNodes      = "NEXO_CLUSTER_LOCK_NODES"
	envLockDeployment = "NEXO_CLUSTER_LOCK_DEPLOYMENT"
	envLockResource   = "NEXO_CLUSTER_LOCK_RESOURCE"
	lockHolderReady   = "LOCKED"
)

func TestMain(m *testing.M) {
	if os.Getenv(envLockHolder) != "" {
		os.Exit(holdLock())
	}
	if runtime.GOOS == "windows" {
		fmt.Println("cluster tests need a Unix system (signals): skipped")
		os.Exit(0)
	}
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cluster tests:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// run builds the server binary (unless NEXO_CLUSTER_BIN names one) and
// runs the tests.
func run(m *testing.M) (int, error) {
	if bin := os.Getenv("NEXO_CLUSTER_BIN"); bin != "" {
		abs, err := filepath.Abs(bin)
		if err != nil {
			return 0, err
		}
		nexoBin = abs
		return m.Run(), nil
	}
	dir, err := os.MkdirTemp("", "nexo-cluster-bin-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	nexoBin = filepath.Join(dir, "nexo")
	if err := buildNexo(nexoBin, ""); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

// buildNexo builds the server binary into out with the test hooks
// (build tag nexotest: clock offset and internode dial map, see
// cmd/nexo/testhooks_nexotest.go), announcing version when not empty.
func buildNexo(out, version string) error {
	args := []string{"build", "-tags", "nexotest", "-o", out}
	if version != "" {
		args = append(args, "-ldflags", "-X github.com/r3n3r0/nexo/internal/version.Version="+version)
	}
	// The test runs in test/cluster: the module root is two levels up.
	cmd := exec.Command("go", append(args, "./cmd/nexo")...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build bin/nexo: %w\n%s", err, output)
	}
	return nil
}

// holdLock is the lock holder helper process: it joins the lock servers
// of the nodes (NEXO_CLUSTER_LOCK_NODES, comma separated) with the
// internode credentials of the deployment, acquires the distributed write
// lock of NEXO_CLUSTER_LOCK_RESOURCE ("bucket/key") as the object layer
// of a node does before a commit, prints LOCKED and keeps the lock
// (refreshing it) until it is killed.
func holdLock() int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "lock holder:", err)
		return 1
	}
	user, password := os.Getenv("NEXO_ROOT_USER"), os.Getenv("NEXO_ROOT_PASSWORD")
	auth, err := rpc.NewAuthenticator(rpc.AuthDeps{Logger: logger},
		rpc.WithCredentials(user, password), rpc.WithNodeID("lock-holder"),
		rpc.WithDeploymentID(os.Getenv(envLockDeployment)), rpc.WithBuildVersion(version.Get().Version))
	if err != nil {
		return fail(err)
	}
	var servers []lock.Server
	for _, node := range strings.Split(os.Getenv(envLockNodes), ",") {
		conn, err := rpc.NewConn(rpc.ConnDeps{Auth: auth, Logger: logger}, node, rpc.WithCallTimeout(5*time.Second))
		if err != nil {
			return fail(err)
		}
		ls, err := rpc.NewLockClient(rpc.LockClientDeps{Conn: conn})
		if err != nil {
			return fail(err)
		}
		servers = append(servers, ls)
	}
	locker, err := lock.NewDistributed(lock.DistributedDeps{Servers: servers, Owner: "lock-holder", Logger: logger},
		lock.WithRefreshInterval(time.Second))
	if err != nil {
		return fail(err)
	}
	bucket, key, _ := strings.Cut(os.Getenv(envLockResource), "/")
	ctx := context.Background()
	if _, err := locker.NewNSLock(ctx, bucket, key).GetLock(ctx, 30*time.Second); err != nil {
		return fail(err)
	}
	fmt.Println(lockHolderReady)
	time.Sleep(time.Hour) // killed by the test
	return 0
}
