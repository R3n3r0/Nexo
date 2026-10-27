package update_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	sysmock "github.com/r3n3r0/nexo/internal/service/sysstore/mock"
	"github.com/r3n3r0/nexo/internal/service/update"
	updatemock "github.com/r3n3r0/nexo/internal/service/update/mock"
	"github.com/r3n3r0/nexo/internal/version"
)

// memDocs backs the generated sysstore mock with a map.
func memDocs(t *testing.T) sysstore.Store {
	var mu sync.Mutex
	docs := map[string][]byte{}
	m := sysmock.NewStore(t)
	m.EXPECT().Get(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		d, ok := docs[p]
		if !ok {
			return nil, sysstore.ErrNotFound
		}
		return d, nil
	}).Maybe()
	m.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p string, d []byte) error {
		mu.Lock()
		defer mu.Unlock()
		docs[p] = bytes.Clone(d)
		return nil
	}).Maybe()
	return m
}

// cluster records what happens to the nodes, in order.
type cluster struct {
	mu     sync.Mutex
	events []string
	// down is the node restarting, if any: the storage cannot lose
	// another one meanwhile.
	down string
}

func (c *cluster) record(e string) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *cluster) log() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

// Health implements update.Health: one node restarting makes the
// deployment unable to lose another one.
func (c *cluster) Health(_ context.Context, opts object.HealthOptions) object.HealthResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if opts.Maintenance && c.down != "" {
		return object.HealthResult{Message: c.down + " is restarting"}
	}
	return object.HealthResult{Healthy: true}
}

// fakeNode is the agent of another node: a restart takes restartDelay,
// during which it does not answer.
type fakeNode struct {
	c            *cluster
	id           string
	mu           sync.Mutex
	version      string
	boot         int
	staged       string
	stageVersion string
	stageErr     error
	// stuck nodes never come back after a restart.
	stuck        bool
	restartDelay time.Duration
	restarting   bool
}

func newFakeNode(c *cluster, id, ver string) *fakeNode {
	return &fakeNode{c: c, id: id, version: ver, stageVersion: version.Get().Version, restartDelay: 30 * time.Millisecond}
}

func (n *fakeNode) NodeID() string { return n.id }

func (n *fakeNode) State(context.Context) (update.NodeState, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.restarting {
		return update.NodeState{}, errors.New("connection refused")
	}
	st := update.NodeState{Node: n.id, Version: n.version, Boot: fmt.Sprintf("%s-%d", n.id, n.boot)}
	if n.staged != "" {
		st.Staged = &update.Staged{SHA256: n.staged, Version: n.stageVersion}
	}
	return st, nil
}

func (n *fakeNode) Stage(ctx context.Context, in update.StageInput) (update.NodeState, error) {
	n.mu.Lock()
	if n.stageErr != nil {
		n.mu.Unlock()
		return update.NodeState{}, n.stageErr
	}
	n.staged = in.SHA256
	n.mu.Unlock()
	n.c.record("stage " + n.id)
	return n.State(ctx)
}

func (n *fakeNode) restart(newVersion string) {
	n.mu.Lock()
	n.restarting = true
	n.mu.Unlock()
	n.c.mu.Lock()
	n.c.down = n.id
	n.c.mu.Unlock()
	if n.stuck {
		return
	}
	go func() {
		time.Sleep(n.restartDelay)
		n.mu.Lock()
		n.restarting = false
		n.boot++
		if newVersion != "" {
			n.version, n.staged = newVersion, ""
		}
		n.mu.Unlock()
		n.c.mu.Lock()
		n.c.down = ""
		n.c.mu.Unlock()
	}()
}

func (n *fakeNode) Activate(_ context.Context, sum string) error {
	n.mu.Lock()
	staged, ver := n.staged, n.stageVersion
	n.mu.Unlock()
	if staged != sum {
		return fmt.Errorf("%w: not staged", domain.ErrPreconditionFailed)
	}
	n.c.record("activate " + n.id)
	n.restart(ver)
	return nil
}

func (n *fakeNode) RestartNode(context.Context) error {
	n.c.record("restart " + n.id)
	n.restart("")
	return nil
}

func (n *fakeNode) StopNode(context.Context) error {
	n.c.record("stop " + n.id)
	return nil
}

// world is a deployment of fake peers around the service of this node,
// whose binary is a file of a temporary directory.
type world struct {
	t       *testing.T
	c       *cluster
	peers   []*fakeNode
	docs    sysstore.Store
	proc    *updatemock.Process
	exe     string
	srv     *httptest.Server
	binary  []byte
	sum     string
	execed  chan struct{}
	svc     update.Service
	options []update.Option
}

// script is a binary that answers "version --json" like a Nexo server.
func script(ver string) []byte {
	return []byte("#!/bin/sh\necho '{\"version\":\"" + ver + "\",\"platform\":\"" + runtime.GOOS + "/" + runtime.GOARCH + "\"}'\n")
}

func newWorld(t *testing.T, peers int, newVersion string) *world {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the staged binary is a shell script")
	}
	w := &world{t: t, c: &cluster{}, docs: memDocs(t), execed: make(chan struct{}, 1),
		options: []update.Option{update.WithPollInterval(5 * time.Millisecond), update.WithNodeTimeout(3 * time.Second)}}
	for i := range peers {
		w.peers = append(w.peers, newFakeNode(w.c, fmt.Sprintf("node%d:9000", i+2), version.Get().Version))
	}
	dir := t.TempDir()
	w.exe = filepath.Join(dir, "nexo")
	require.NoError(t, os.WriteFile(w.exe, []byte("old binary"), 0o755)) //nolint:gosec // an executable
	w.binary = script(newVersion)
	h := sha256.Sum256(w.binary)
	w.sum = hex.EncodeToString(h[:])
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nexo" {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write(w.binary)
	}))
	t.Cleanup(w.srv.Close)
	w.proc = updatemock.NewProcess(t)
	w.proc.EXPECT().Executable().Return(w.exe, nil).Maybe()
	w.proc.EXPECT().Exec().RunAndReturn(func() error {
		w.c.record("exec node1:9000")
		w.execed <- struct{}{}
		return nil
	}).Maybe()
	w.proc.EXPECT().Restart().RunAndReturn(func() error {
		w.c.record("restart node1:9000")
		w.execed <- struct{}{}
		return nil
	}).Maybe()
	w.proc.EXPECT().Stop().RunAndReturn(func() error {
		w.c.record("stop node1:9000")
		return nil
	}).Maybe()
	w.svc = w.service()
	return w
}

// service returns a run of the service of this node: a new one is the
// next run of the node, after its restart.
func (w *world) service() update.Service {
	agents := make([]update.Agent, 0, len(w.peers))
	for _, p := range w.peers {
		agents = append(agents, p)
	}
	svc, err := update.New(update.Deps{NodeID: "node1:9000", Peers: agents, Process: w.proc, Health: w.c, Docs: w.docs, HTTP: w.srv.Client()}, w.options...)
	require.NoError(w.t, err)
	return svc
}

// waitThisNode waits for this node to replace or restart its process.
func (w *world) waitThisNode() {
	w.t.Helper()
	select {
	case <-w.execed:
	case <-time.After(10 * time.Second):
		w.t.Fatalf("this node was not restarted: %v", w.c.log())
	}
}

// waitEnded waits for the job of svc to end.
func waitEnded(t *testing.T, svc update.Service) update.Job {
	t.Helper()
	var j update.Job
	require.Eventually(t, func() bool {
		var err error
		j, err = svc.Job(context.Background())
		return err == nil && j.State != update.JobRunning
	}, 10*time.Second, 5*time.Millisecond)
	return j
}

func phases(j update.Job) map[string]update.Phase {
	out := map[string]update.Phase{}
	for _, n := range j.Nodes {
		out[n.Node] = n.Phase
	}
	return out
}

// TestRollingUpdate (S-OPS-09): the binary is staged on every node, then
// activated on the other nodes one at a time, each once the previous one
// is back and the deployment can lose a node; this node is last, its
// binary swapped (the previous one kept as .old) and its process
// replaced. The next run of this node completes the job.
//
// covers: S-OPS-09
func TestRollingUpdate(t *testing.T) {
	w := newWorld(t, 3, version.Get().Version)
	ctx := context.Background()
	_, err := w.svc.Job(ctx)
	require.ErrorIs(t, err, domain.ErrNotFound)

	j, err := w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: w.sum})
	require.NoError(t, err)
	assert.Equal(t, update.JobRunning, j.State)
	assert.Equal(t, update.ModeRolling, j.Mode)
	assert.Len(t, j.Nodes, 4)
	w.waitThisNode()

	log := w.c.log()
	// The stages run in parallel, before any activation.
	stages := 0
	for _, e := range log[:3] {
		if len(e) > 5 && e[:5] == "stage" {
			stages++
		}
	}
	assert.Equal(t, 3, stages, "%v", log)
	assert.Equal(t, []string{"activate node2:9000", "activate node3:9000", "activate node4:9000", "exec node1:9000"}, log[3:])
	got, err := os.ReadFile(w.exe)
	require.NoError(t, err)
	assert.Equal(t, w.binary, got, "the new binary is in place")
	old, err := os.ReadFile(w.exe + ".old")
	require.NoError(t, err)
	assert.Equal(t, "old binary", string(old))

	// The job stays running until this node runs again; a second job is
	// refused meanwhile.
	j, err = w.svc.Job(ctx)
	require.NoError(t, err)
	assert.Equal(t, update.JobRunning, j.State)
	assert.Equal(t, update.PhaseRestarting, phases(j)["node1:9000"])
	assert.Equal(t, update.PhaseDone, phases(j)["node4:9000"])
	_, err = w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: w.sum})
	require.ErrorIs(t, err, domain.ErrConflict)

	next := w.service()
	j = waitEnded(t, next)
	assert.Equal(t, update.JobCompleted, j.State, j.Error)
	for node, p := range phases(j) {
		assert.Equal(t, update.PhaseDone, p, node)
	}
	assert.Equal(t, version.Get().Version, j.Version)
}

// TestUpdateAllAtOnce: every node is activated without waiting, this one
// last.
//
// covers: S-OPS-09
func TestUpdateAllAtOnce(t *testing.T) {
	w := newWorld(t, 2, version.Get().Version)
	ctx := context.Background()
	_, err := w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: w.sum, Mode: update.ModeAll})
	require.NoError(t, err)
	w.waitThisNode()
	log := w.c.log()
	assert.ElementsMatch(t, []string{"activate node2:9000", "activate node3:9000"}, log[2:4])
	assert.Equal(t, "exec node1:9000", log[4])
	j := waitEnded(t, w.service())
	assert.Equal(t, update.JobCompleted, j.State, j.Error)
}

// TestUpdateRefusals: nothing is activated when a node fails its staging,
// reports another version or cannot be reached, when the deployment
// cannot lose a node, for a change of major version in a rolling update,
// or a server not run as a process; the input is checked first.
//
// covers: S-OPS-09
func TestUpdateRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("invalid input", func(t *testing.T) {
		w := newWorld(t, 1, version.Get().Version)
		for _, in := range []update.UpdateInput{
			{URL: "ftp://x/nexo", SHA256: w.sum},
			{URL: w.srv.URL + "/nexo", SHA256: "abc"},
			{URL: w.srv.URL + "/nexo", SHA256: w.sum, Mode: "sometimes"},
		} {
			_, err := w.svc.Update(ctx, in)
			require.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", in)
		}
	})
	t.Run("a node fails its staging", func(t *testing.T) {
		w := newWorld(t, 2, version.Get().Version)
		w.peers[1].stageErr = errors.New("disk full")
		_, err := w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: w.sum})
		require.NoError(t, err)
		j := waitEnded(t, w.svc)
		assert.Equal(t, update.JobFailed, j.State)
		assert.Contains(t, j.Error, "disk full")
		assert.Equal(t, update.PhaseFailed, phases(j)["node3:9000"])
		for _, e := range w.c.log() {
			assert.NotContains(t, e, "activate")
		}
		old, err := os.ReadFile(w.exe)
		require.NoError(t, err)
		assert.Equal(t, "old binary", string(old), "the binary of this node is untouched")
	})
	t.Run("a wrong digest", func(t *testing.T) {
		w := newWorld(t, 0, version.Get().Version)
		sum := sha256.Sum256([]byte("another binary"))
		_, err := w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: hex.EncodeToString(sum[:])})
		require.NoError(t, err)
		j := waitEnded(t, w.svc)
		assert.Equal(t, update.JobFailed, j.State)
		assert.Contains(t, j.Error, "SHA-256")
		entries, err := os.ReadDir(filepath.Dir(w.exe))
		require.NoError(t, err)
		assert.Len(t, entries, 1, "the download is removed")
	})
	t.Run("not a Nexo binary", func(t *testing.T) {
		w := newWorld(t, 0, version.Get().Version)
		w.binary = []byte("#!/bin/sh\necho hello\n")
		h := sha256.Sum256(w.binary)
		_, err := w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: hex.EncodeToString(h[:])})
		require.NoError(t, err)
		j := waitEnded(t, w.svc)
		assert.Contains(t, j.Error, "not a Nexo server")
	})
	t.Run("versions differ", func(t *testing.T) {
		w := newWorld(t, 2, version.Get().Version)
		w.peers[0].stageVersion = "v9.9.9"
		_, err := w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: w.sum})
		require.NoError(t, err)
		j := waitEnded(t, w.svc)
		assert.Contains(t, j.Error, "v9.9.9")
	})
	t.Run("a major version in a rolling update", func(t *testing.T) {
		w := newWorld(t, 2, "v2.0.0")
		for _, p := range w.peers {
			p.version, p.stageVersion = "v1.4.0", "v2.0.0"
		}
		_, err := w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: w.sum})
		require.NoError(t, err)
		j := waitEnded(t, w.svc)
		assert.Contains(t, j.Error, "major version")
		assert.Empty(t, slices.DeleteFunc(w.c.log(), func(e string) bool { return e[:5] == "stage" }))
	})
	t.Run("a node unreachable", func(t *testing.T) {
		w := newWorld(t, 2, version.Get().Version)
		w.peers[0].restarting = true
		_, err := w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: w.sum})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "node2:9000")
	})
	t.Run("the deployment cannot lose a node", func(t *testing.T) {
		w := newWorld(t, 2, version.Get().Version)
		w.c.down = "node5:9000"
		_, err := w.svc.Update(ctx, update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: w.sum})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		_, err = w.svc.Restart(ctx, update.RestartInput{Mode: update.ModeAll})
		require.NoError(t, err, "every node at once does not need it")
		w.waitThisNode()
	})
	t.Run("not run as a process", func(t *testing.T) {
		svc, err := update.New(update.Deps{NodeID: "local", Docs: memDocs(t)})
		require.NoError(t, err)
		_, err = svc.Update(ctx, update.UpdateInput{URL: "https://a.test/nexo", SHA256: hex.EncodeToString(make([]byte, 32))})
		require.ErrorIs(t, err, domain.ErrNotImplemented)
		require.ErrorIs(t, svc.StopAll(ctx), domain.ErrNotImplemented)
		_, err = svc.Stage(ctx, update.StageInput{URL: "https://a.test/nexo"})
		require.ErrorIs(t, err, domain.ErrNotImplemented)
	})
}

// TestUpdateNodeNotBack: a node that does not come back with the new
// version fails the job; the next nodes, and this one, keep their binary.
//
// covers: S-OPS-09
func TestUpdateNodeNotBack(t *testing.T) {
	w := newWorld(t, 3, version.Get().Version)
	w.options = append(w.options, update.WithNodeTimeout(200*time.Millisecond))
	w.svc = w.service()
	w.peers[0].stuck = true
	_, err := w.svc.Update(context.Background(), update.UpdateInput{URL: w.srv.URL + "/nexo", SHA256: w.sum})
	require.NoError(t, err)
	j := waitEnded(t, w.svc)
	assert.Equal(t, update.JobFailed, j.State)
	assert.Contains(t, j.Error, "node2:9000")
	assert.Equal(t, update.PhaseFailed, phases(j)["node2:9000"])
	assert.Equal(t, update.PhaseStaged, phases(j)["node3:9000"])
	assert.NotContains(t, w.c.log(), "activate node3:9000")
	assert.NotContains(t, w.c.log(), "exec node1:9000")
	old, err := os.ReadFile(w.exe)
	require.NoError(t, err)
	assert.Equal(t, "old binary", string(old))
}

// TestCoordinatedRestart (C-ADM-09): a rolling restart restarts the other
// nodes one at a time, then this one; StopAll stops the other nodes, then
// this one.
//
// covers: C-ADM-09, S-OPS-09
func TestCoordinatedRestart(t *testing.T) {
	w := newWorld(t, 2, version.Get().Version)
	ctx := context.Background()
	j, err := w.svc.Restart(ctx, update.RestartInput{})
	require.NoError(t, err)
	assert.Equal(t, update.KindRestart, j.Kind)
	w.waitThisNode()
	assert.Equal(t, []string{"restart node2:9000", "restart node3:9000", "restart node1:9000"}, w.c.log())
	j = waitEnded(t, w.service())
	assert.Equal(t, update.JobCompleted, j.State, j.Error)

	require.NoError(t, w.svc.StopAll(ctx))
	log := w.c.log()
	assert.ElementsMatch(t, []string{"stop node2:9000", "stop node3:9000"}, log[3:5])
	assert.Equal(t, "stop node1:9000", log[5])
}

// TestInterruptedJob: a job whose orchestrator restarted before
// activating every node fails; the nodes done stay done.
//
// covers: S-OPS-09
func TestInterruptedJob(t *testing.T) {
	w := newWorld(t, 2, version.Get().Version)
	w.peers[1].restartDelay = time.Hour
	_, err := w.svc.Restart(context.Background(), update.RestartInput{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return slices.Contains(w.c.log(), "restart node3:9000") }, 5*time.Second, time.Millisecond)
	// This node stops while waiting for node3: the job is interrupted.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.svc.Stop(stopCtx))
	j, err := w.svc.Job(context.Background())
	require.NoError(t, err)
	assert.Equal(t, update.JobFailed, j.State)
	assert.Contains(t, j.Error, "interrupted")
	assert.Equal(t, update.PhaseDone, phases(j)["node2:9000"])
}
