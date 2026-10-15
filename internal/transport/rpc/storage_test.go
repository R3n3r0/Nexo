package rpc_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/storage/storagetest"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

// openLocal opens the local drive at dir.
func openLocal(t testing.TB, dir string) storage.StorageAPI {
	t.Helper()
	ep, err := domain.ParseEndpoint(dir)
	require.NoError(t, err)
	d, err := storage.NewLocal(storage.Deps{Logger: discard()}, ep, storage.WithoutFsync())
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

// remoteDrive serves the local drive at dir from an RPC server and returns
// the client of another node reaching it: client → HTTP/2 → xl storage.
func remoteDrive(t testing.TB, dir string, opts ...rpc.StorageClientOption) (storage.StorageAPI, rpc.Conn) {
	t.Helper()
	local := openLocal(t, dir)
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local}})
	require.NoError(t, err)
	addr, _ := startRPC(t, routes)
	conn := dial(t, newAuth(t, "client:9000"), addr)
	ep := domain.Endpoint{Scheme: "http", Host: addr, Path: dir}
	client, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: conn, Logger: discard()}, ep,
		append([]rpc.StorageClientOption{rpc.WithDiskCheckInterval(50 * time.Millisecond)}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client, conn
}

// TestRemoteHandlesLifecycle: the file handles a node opens on a peer are
// closed when its drive client closes (a node that stops leaves none
// behind), and those of an earlier run of a node are closed as soon as its
// new run opens a file (regression: a stopped, crashed or restarted reader
// left its handles, and the replaced data they hold, open on its peers).
func TestRemoteHandlesLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/d"
	local := openLocal(t, dir)
	require.NoError(t, local.MakeVol(ctx, "bkt"))
	require.NoError(t, local.WriteAll(ctx, "bkt", "obj/part.1", []byte("shard")))
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local}})
	require.NoError(t, err)
	addr, _ := startRPC(t, routes)
	ep := domain.Endpoint{Scheme: "http", Host: addr, Path: dir}
	client := func(boot string) storage.StorageAPI {
		c, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: dial(t, newAuth(t, "client:9000"), addr), Boot: boot, Logger: discard()}, ep)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close(ctx) })
		return c
	}

	first := client("boot-1")
	_, err = first.OpenFile(ctx, "bkt", "obj/part.1")
	require.NoError(t, err)
	_, err = first.OpenFile(ctx, "bkt", "obj/part.1")
	require.NoError(t, err)
	assert.Equal(t, 2, rpc.OpenHandles(routes))
	require.NoError(t, first.Close(ctx))
	assert.Zero(t, rpc.OpenHandles(routes), "the client closes its handles before closing")

	// A run that crashed: its handles stay until the next run opens one.
	crashed := client("boot-2")
	_, err = crashed.OpenFile(ctx, "bkt", "obj/part.1")
	require.NoError(t, err)
	restarted := client("boot-3")
	h, err := restarted.OpenFile(ctx, "bkt", "obj/part.1")
	require.NoError(t, err)
	assert.Equal(t, 1, rpc.OpenHandles(routes), "the handle of the earlier run is closed")
	rc, err := h.ReadStream(ctx, 0, -1)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "shard", string(data))
	require.NoError(t, h.Close(ctx))
	assert.Zero(t, rpc.OpenHandles(routes))
}

// TestRemoteHandlesRenewed: a handle its reader holds without streaming
// it (a spare shard of an erasure read, streamed only when another shard
// fails) is renewed while open, so it outlives the idle timeout of its
// node however long the read lasts; the handle of a reader that can no
// longer renew it (crashed, unreachable) still expires (regression: a read
// lasting more than the idle timeout lost the spare shards it held on its
// peers, and with them the redundancy of the parity).
func TestRemoteHandlesRenewed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/d"
	local := openLocal(t, dir)
	require.NoError(t, local.MakeVol(ctx, "bkt"))
	require.NoError(t, local.WriteAll(ctx, "bkt", "obj/part.1", []byte("shard")))
	// Swept every second, idle after 400ms: renewed every 100ms.
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local}}, rpc.WithFileHandles(10, 400*time.Millisecond))
	require.NoError(t, err)
	require.NoError(t, routes.Start(ctx))
	t.Cleanup(func() { _ = routes.Stop(ctx) })
	addr, _ := startRPC(t, routes)
	ep := domain.Endpoint{Scheme: "http", Host: addr, Path: dir}
	open := func(boot string) (storage.FileHandle, rpc.Conn) {
		conn := dial(t, newAuth(t, "client:9000"), addr)
		c, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: conn, Boot: boot, Logger: discard()}, ep)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close(ctx) })
		h, err := c.OpenFile(ctx, "bkt", "obj/part.1")
		require.NoError(t, err)
		return h, conn
	}
	reading, _ := open("boot")
	_, lost := open("boot")
	require.Equal(t, 2, rpc.OpenHandles(routes))
	require.NoError(t, lost.Close(ctx), "the second reader can no longer reach the node")

	time.Sleep(3 * time.Second) // three sweeps, seven idle timeouts
	assert.Equal(t, 1, rpc.OpenHandles(routes), "the renewed handle stays open, the other expires")
	rc, err := reading.ReadStream(ctx, 0, -1)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "shard", string(data))
	require.NoError(t, reading.Close(ctx))
	assert.Zero(t, rpc.OpenHandles(routes))
}

// TestRemoteBehavior runs the StorageAPI behavior suite of the local drive,
// unchanged, against the RPC client of a drive served by another node.
func TestRemoteBehavior(t *testing.T) {
	storagetest.Run(t, func(t *testing.T, dir string) storage.StorageAPI {
		d, _ := remoteDrive(t, dir)
		return d
	})
}

func TestStorageRoutesValidation(t *testing.T) {
	dir := t.TempDir() + "/d"
	local := openLocal(t, dir)
	_, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local, local}})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "a drive exposed twice")
	_, err = rpc.NewStorageClient(rpc.StorageClientDeps{}, domain.Endpoint{Path: "/x"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestRemoteDriveUnknown: a node answers for its own drives only.
func TestRemoteDriveUnknown(t *testing.T) {
	ctx := context.Background()
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{openLocal(t, t.TempDir()+"/d")}})
	require.NoError(t, err)
	addr, _ := startRPC(t, routes)
	conn := dial(t, newAuth(t, "client:9000"), addr)
	client, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: conn, Logger: discard()},
		domain.Endpoint{Scheme: "http", Host: addr, Path: "/not/served"})
	require.NoError(t, err)
	_, err = client.ListVols(ctx)
	assert.ErrorIs(t, err, domain.ErrDiskNotFound)
	assert.False(t, client.IsOnline(), "a drive its node does not serve is offline")
	assert.True(t, conn.IsOnline(), "the node itself is online")
}

// TestRemoteNodeOffline: when the node stops, the drive is offline
// (ErrDiskNotFound wrapping ErrNodeOffline) and calls fail fast; when the
// node is back, the background probe closes the circuit again.
func TestRemoteNodeOffline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/d"
	local := openLocal(t, dir)
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local}})
	require.NoError(t, err)
	ln := listen(t)
	addr := ln.Addr().String()
	serverAuth := newAuth(t, addr)
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: serverAuth, Routes: []rpc.Routes{routes}, Logger: discard()})
	require.NoError(t, err)
	srv := serve(t, ln, s)
	conn := dial(t, newAuth(t, "client:9000"), addr, rpc.WithDialTimeout(time.Second))
	client, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: conn, Logger: discard()}, domain.Endpoint{Scheme: "http", Host: addr, Path: dir})
	require.NoError(t, err)
	require.NoError(t, client.MakeVol(ctx, "b"))
	require.True(t, client.IsOnline())

	srv.CloseClientConnections()
	srv.Close()
	_, err = client.ListVols(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDiskNotFound)
	assert.ErrorIs(t, err, domain.ErrNodeOffline)
	// A broken exchange is verified by a ping before the circuit opens.
	require.Eventually(t, func() bool { return !conn.IsOnline() }, 5*time.Second, 5*time.Millisecond)
	assert.False(t, client.IsOnline())
	start := time.Now()
	_, err = client.StatVol(ctx, "b")
	assert.ErrorIs(t, err, domain.ErrNodeOffline)
	assert.Less(t, time.Since(start), 100*time.Millisecond, "an open circuit fails fast")

	// The node restarts on the same address.
	ln2, err := listenOn(addr)
	require.NoError(t, err)
	serve(t, ln2, s)
	require.Eventually(t, conn.IsOnline, 5*time.Second, 10*time.Millisecond, "the probe notices the node")
	vols, err := client.ListVols(ctx)
	require.NoError(t, err)
	require.Len(t, vols, 1)
	assert.True(t, client.IsOnline())
}

// TestRemoteDriveComesBack: a drive its node reported missing is checked
// in the background and comes back online.
func TestRemoteDriveComesBack(t *testing.T) {
	ctx := context.Background()
	client2, _ := remoteDrive(t, t.TempDir()+"/e")
	require.NoError(t, client2.MakeVol(ctx, "b"))
	// The node answers that it does not have the drive asked for (another
	// drive id): the drive is offline until a check finds it.
	_, err := client2.DiskInfo(ctx, storage.DiskInfoOptions{DiskID: "not-this-one"})
	assert.ErrorIs(t, err, domain.ErrDiskNotFound)
	assert.False(t, client2.IsOnline(), "the node reported the drive missing")
	require.Eventually(t, client2.IsOnline, 5*time.Second, 10*time.Millisecond, "the background check finds it again")
}

// failingReader fails after some bytes.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestRemoteCreateFileReaderError: a failing source aborts the remote
// write with the error of the source, and nothing is left behind.
func TestRemoteCreateFileReaderError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/d"
	client, _ := remoteDrive(t, dir)
	boom := errors.New("source failed")
	err := client.CreateFile(ctx, domain.SystemBucket, "tmp/u/part.1", -1, &failingReader{data: bytes.Repeat([]byte("x"), 300<<10), err: boom})
	assert.ErrorIs(t, err, boom)
	infos, err := client.StatInfoFile(ctx, domain.SystemBucket, "tmp/u/*", true)
	require.NoError(t, err)
	assert.Empty(t, infos)

	// A read error on a stream of the node is reported to the reader.
	require.NoError(t, client.CreateFile(ctx, domain.SystemBucket, "tmp/v/part.1", 5, strings.NewReader("hello")))
	rc, err := client.ReadFileStream(ctx, domain.SystemBucket, "tmp/v/part.1", 0, -1)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
	require.NoError(t, rc.Close())
}
