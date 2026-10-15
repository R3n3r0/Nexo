package admin_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/certs"
	certsmock "github.com/r3n3r0/nexo/internal/service/certs/mock"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
	"github.com/r3n3r0/nexo/internal/service/heal"
	healmock "github.com/r3n3r0/nexo/internal/service/heal/mock"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/scanner"
	scannermock "github.com/r3n3r0/nexo/internal/service/scanner/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
)

// covers: S-OPS-08
func TestServerInfo(t *testing.T) {
	start := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	ep, _ := domain.ParseEndpoint("/data1")
	ep.SetIdx, ep.DiskIdx = 0, 1

	online := storagemock.NewStorageAPI(t)
	online.EXPECT().Endpoint().Return(ep)
	online.EXPECT().DiskInfo(mock.Anything, storage.DiskInfoOptions{}).
		Return(domain.DiskInfo{Endpoint: "/data1", Path: "/data1", ID: "disk-1", State: domain.StateOnline, Total: 100, Free: 60, Used: 40}, nil)
	online.EXPECT().Healing().Return(nil)

	broken := storagemock.NewStorageAPI(t)
	brokenEP, _ := domain.ParseEndpoint("/data2")
	broken.EXPECT().Endpoint().Return(brokenEP)
	broken.EXPECT().DiskInfo(mock.Anything, storage.DiskInfoOptions{}).Return(domain.DiskInfo{}, errors.New("io error"))
	broken.EXPECT().Healing().Return(nil)

	objects := objectmock.NewService(t)
	objects.EXPECT().ListBuckets(mock.Anything).Return([]domain.BucketInfo{
		{Bucket: domain.Bucket{Name: "a"}, Objects: 2, Size: 100},
		{Bucket: domain.Bucket{Name: "b"}, Objects: 3, Size: 50},
	}, nil)

	svc := admin.New(admin.Deps{
		Clock:        clk,
		Build:        admin.BuildInfo{Version: "1.2.3", Commit: "abc", Date: "today"},
		Mode:         domain.ModeSNSD,
		Region:       "us-east-1",
		DeploymentID: "dep-1",
		Drives:       []storage.StorageAPI{online, broken},
		Pools:        []domain.PoolInfo{{Index: 0, Sets: 1, DrivesPerSet: 2}},
		Objects:      objects,
	})
	clk.Advance(90 * time.Second)
	info, err := svc.ServerInfo(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", info.Version)
	assert.Equal(t, domain.ModeSNSD, info.Mode)
	assert.Equal(t, "dep-1", info.DeploymentID)
	assert.Equal(t, start, info.StartedAt)
	assert.Equal(t, 90*time.Second, info.Uptime)
	require.Len(t, info.Pools, 1)
	// 2 drives without parity: both needed; the broken one makes the set
	// unavailable.
	assert.Equal(t, 2, info.Pools[0].ReadQuorum)
	assert.Equal(t, 2, info.Pools[0].WriteQuorum)
	assert.Equal(t, []domain.ErasureSetInfo{{Index: 0, Drives: 2, Online: 1, Offline: 1, State: domain.SetStateUnavailable}}, info.Pools[0].ErasureSets)
	require.Len(t, info.Nodes, 1)
	assert.True(t, info.Nodes[0].Local)
	require.Len(t, info.Nodes[0].Disks, 2)
	d1, d2 := info.Nodes[0].Disks[0], info.Nodes[0].Disks[1]
	assert.Equal(t, "/data1", d1.Path)
	assert.Equal(t, "disk-1", d1.ID)
	assert.Equal(t, domain.StateOnline, d1.State)
	assert.Equal(t, uint64(40), d1.Used)
	assert.Equal(t, 1, d1.DiskIdx)
	assert.Equal(t, "/data2", d2.Path, "endpoint fills the missing fields")
	assert.Equal(t, domain.StateOffline, d2.State)
	assert.Equal(t, "io error", d2.Error)
	assert.Equal(t, uint64(2), info.Buckets)
	assert.Equal(t, uint64(5), info.Objects)
	assert.Equal(t, uint64(150), info.Usage)
}

func TestDataUsageAndNotImplemented(t *testing.T) {
	ctx := context.Background()
	svc := admin.New(admin.Deps{})
	_, err := svc.DataUsage(ctx)
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = svc.StartHeal(ctx, admin.HealInput{})
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = svc.HealStatus(ctx, "h1")
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = svc.ListHeals(ctx)
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	assert.ErrorIs(t, svc.ServiceAction(ctx, admin.ActionRestart), domain.ErrNotImplemented)
	assert.ErrorIs(t, svc.ServiceAction(ctx, "dance"), domain.ErrInvalidArgument)

	objects := objectmock.NewService(t)
	objects.EXPECT().DataUsage(mock.Anything, "").
		Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "a"}, Objects: 1, Size: 10, Versions: 2}}, nil).Once()
	withObjects := admin.New(admin.Deps{Objects: objects})
	du, err := withObjects.DataUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), du.BucketsCount)
	assert.Equal(t, uint64(10), du.TotalSize)
	assert.Equal(t, uint64(2), du.Buckets["a"].VersionsCount)

	objects.EXPECT().DataUsage(mock.Anything, "").Return(nil, errors.New("down")).Once()
	_, err = withObjects.DataUsage(ctx)
	assert.Error(t, err)

	var got admin.Action
	withActions := admin.New(admin.Deps{Actions: admin.ActionHandlerFunc(func(_ context.Context, a admin.Action) error { got = a; return nil })})
	require.NoError(t, withActions.ServiceAction(ctx, admin.ActionStop))
	assert.Equal(t, admin.ActionStop, got)
}

func TestHealJobs(t *testing.T) {
	ctx := context.Background()
	healer := healmock.NewService(t)
	svc := admin.New(admin.Deps{Heal: healer})
	job := heal.Job{ID: "h1", State: heal.JobRunning}
	healer.EXPECT().StartJob(mock.Anything, heal.JobRequest{Bucket: "b", Prefix: "p/", Recursive: true,
		Options: heal.Options{DryRun: true, Deep: true, Remove: true}}).Return(job, nil).Once()
	got, err := svc.StartHeal(ctx, admin.HealInput{Bucket: "b", Prefix: "p/", Recursive: true, DryRun: true, Deep: true, Remove: true})
	require.NoError(t, err)
	assert.Equal(t, job, got)

	healer.EXPECT().StartJob(mock.Anything, heal.JobRequest{}).Return(heal.Job{}, domain.ErrTooManyRequests).Once()
	_, err = svc.StartHeal(ctx, admin.HealInput{})
	assert.ErrorIs(t, err, domain.ErrTooManyRequests)

	healer.EXPECT().Job(mock.Anything, "h1").Return(job, nil).Once()
	got, err = svc.HealStatus(ctx, "h1")
	require.NoError(t, err)
	assert.Equal(t, "h1", got.ID)
	healer.EXPECT().Job(mock.Anything, "zz").Return(heal.Job{}, domain.ErrNotFound).Once()
	_, err = svc.HealStatus(ctx, "zz")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	healer.EXPECT().Jobs(mock.Anything).Return([]heal.Job{job}, nil).Once()
	jobs, err := svc.ListHeals(ctx)
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
	healer.EXPECT().Jobs(mock.Anything).Return(nil, errors.New("down")).Once()
	_, err = svc.ListHeals(ctx)
	assert.Error(t, err)
}

// TestDataUsageFromScanner: the usage of the scanner is served for the
// buckets it met, the newer buckets are computed on demand, the removed
// ones dropped; without a usage yet everything is computed.
func TestDataUsageFromScanner(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC)
	scan := scannermock.NewService(t)
	objects := objectmock.NewService(t)
	svc := admin.New(admin.Deps{Objects: objects, Scanner: scan})

	scan.EXPECT().Usage(mock.Anything).Return(scanner.Usage{LastUpdate: at, Cycle: 3, Buckets: map[string]scanner.BucketUsage{
		"old":  {Counts: scanner.Counts{Objects: 4, Versions: 1, DeleteMarkers: 2, Size: 400}, Prefixes: map[string]scanner.Counts{"a/": {Objects: 4, Size: 400}}},
		"gone": {Counts: scanner.Counts{Objects: 9, Size: 900}},
	}}, nil).Once()
	objects.EXPECT().ListBuckets(mock.Anything).Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "new"}}, {Bucket: domain.Bucket{Name: "old"}}}, nil).Once()
	objects.EXPECT().DataUsage(mock.Anything, "new").Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "new"}, Objects: 1, Size: 10}}, nil).Once()
	du, err := svc.DataUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, at, du.LastUpdate)
	assert.Equal(t, uint64(2), du.BucketsCount)
	assert.Equal(t, uint64(5), du.ObjectsCount)
	assert.Equal(t, uint64(1), du.VersionsCount)
	assert.Equal(t, uint64(410), du.TotalSize)
	assert.Equal(t, admin.BucketUsage{Size: 400, ObjectsCount: 4, VersionsCount: 1, DeleteMarkers: 2,
		Prefixes: map[string]admin.PrefixUsage{"a/": {Size: 400, ObjectsCount: 4}}}, du.Buckets["old"])
	assert.NotContains(t, du.Buckets, "gone")

	// No cycle yet: the walk of every bucket.
	scan.EXPECT().Usage(mock.Anything).Return(scanner.Usage{}, domain.ErrNotFound).Once()
	objects.EXPECT().DataUsage(mock.Anything, "").Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "x"}, Objects: 2}}, nil).Once()
	du, err = svc.DataUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), du.ObjectsCount)

	// Errors.
	scan.EXPECT().Usage(mock.Anything).Return(scanner.Usage{}, nil).Once()
	objects.EXPECT().ListBuckets(mock.Anything).Return(nil, errors.New("down")).Once()
	_, err = svc.DataUsage(ctx)
	assert.Error(t, err)
	scan.EXPECT().Usage(mock.Anything).Return(scanner.Usage{}, nil).Once()
	objects.EXPECT().ListBuckets(mock.Anything).Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "racy"}}, {Bucket: domain.Bucket{Name: "bad"}}}, nil).Once()
	objects.EXPECT().DataUsage(mock.Anything, "racy").Return(nil, domain.ErrBucketNotFound).Once()
	objects.EXPECT().DataUsage(mock.Anything, "bad").Return(nil, errors.New("io")).Once()
	_, err = svc.DataUsage(ctx)
	assert.Error(t, err)
}

func TestServerInfoHealingProgress(t *testing.T) {
	ep, _ := domain.ParseEndpoint("/data3")
	started := time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)
	d := storagemock.NewStorageAPI(t)
	d.EXPECT().Endpoint().Return(ep)
	d.EXPECT().DiskInfo(mock.Anything, storage.DiskInfoOptions{}).Return(domain.DiskInfo{Endpoint: "/data3", State: domain.StateOnline}, nil)
	d.EXPECT().IsOnline().Return(true).Maybe()
	d.EXPECT().Healing().Return(&storage.HealingTracker{Started: started, Bucket: "b", Object: "k", ObjectsHealed: 7, HealedBuckets: []string{"a"}})
	info, err := admin.New(admin.Deps{Drives: []storage.StorageAPI{d}}).ServerInfo(context.Background())
	require.NoError(t, err)
	disk := info.Nodes[0].Disks[0]
	assert.True(t, disk.Healing)
	assert.Equal(t, domain.StateHealing, disk.State)
	require.NotNil(t, disk.HealingProgress)
	assert.Equal(t, domain.HealingProgress{Started: started, Bucket: "b", Object: "k", ObjectsHealed: 7, HealedBuckets: 1}, *disk.HealingProgress)
}

// TestServerInfoCluster: a distributed deployment reports every node,
// this one first with its own drives, the unreachable ones offline, the
// clock offset of the peers that answer and the state of the erasure
// sets; a failure to reach the peers falls back to this node.
//
// covers: S-OPS-08
func TestServerInfoCluster(t *testing.T) {
	ep, _ := domain.ParseEndpoint("http://n1:9000/data1")
	d := storagemock.NewStorageAPI(t)
	d.EXPECT().Endpoint().Return(ep)
	d.EXPECT().DiskInfo(mock.Anything, storage.DiskInfoOptions{}).Return(domain.DiskInfo{Endpoint: ep.String(), State: domain.StateOnline, Total: 10}, nil)
	d.EXPECT().Healing().Return(nil)
	peers := clustermock.NewInfo(t)
	disk := func(node string, set int, state string) domain.DiskInfo {
		return domain.DiskInfo{Endpoint: "http://" + node + "/data1", SetIdx: set, State: state, Healing: state == domain.StateHealing}
	}
	queue := &domain.HealQueueStats{Pending: 3, Healed: 4}
	nodes := []domain.NodeInfo{
		{Endpoint: "n1:9000", State: domain.StateOnline, Local: true, Disks: []domain.DiskInfo{{Endpoint: ep.String()}}, HealQueue: queue},
		{Endpoint: "n2:9000", State: domain.StateOffline, Disks: []domain.DiskInfo{disk("n2:9000", 0, domain.StateOffline), disk("n2:9000", 1, domain.StateOffline)}},
		{Endpoint: "n3:9000", State: domain.StateOnline, Disks: []domain.DiskInfo{disk("n3:9000", 0, domain.StateHealing), disk("n3:9000", 1, domain.StateOnline)}},
		{Endpoint: "n4:9000", State: domain.StateOnline, Disks: []domain.DiskInfo{disk("n4:9000", 0, domain.StateOnline), disk("n4:9000", 1, domain.StateOffline)}},
	}
	peers.EXPECT().Nodes(mock.Anything).Return(nodes, nil).Once()
	peers.EXPECT().ClockSkew(mock.Anything).Return([]cluster.Skew{
		{NodeID: "n2:9000", Err: "offline"},
		{NodeID: "n3:9000", Offset: 3 * time.Second, RTT: time.Millisecond, Exceeded: true},
		{NodeID: "n4:9000", Offset: -20 * time.Millisecond, RTT: 2 * time.Millisecond},
	}, nil).Once()
	svc := admin.New(admin.Deps{Mode: domain.ModeDistributed, Drives: []storage.StorageAPI{d}, Cluster: peers,
		Pools: []domain.PoolInfo{{Index: 0, Sets: 2, DrivesPerSet: 4, Parity: 2}}})

	info, err := svc.ServerInfo(context.Background())
	require.NoError(t, err)
	require.Len(t, info.Nodes, 4)
	assert.Equal(t, "n1:9000", info.Nodes[0].Endpoint)
	assert.True(t, info.Nodes[0].Local)
	assert.Equal(t, uint64(10), info.Nodes[0].Disks[0].Total, "this node reports its own drives")
	assert.Equal(t, queue, info.Nodes[0].HealQueue)
	assert.Equal(t, domain.StateOffline, info.Nodes[1].State)
	assert.Nil(t, info.Nodes[0].ClockSkew, "the reference clock")
	assert.Nil(t, info.Nodes[1].ClockSkew, "an unreachable node has no measure")
	assert.Equal(t, &domain.ClockSkew{Offset: 3 * time.Second, RTT: time.Millisecond, Exceeded: true}, info.Nodes[2].ClockSkew)
	assert.Equal(t, &domain.ClockSkew{Offset: -20 * time.Millisecond, RTT: 2 * time.Millisecond}, info.Nodes[3].ClockSkew)

	// 2 sets of 4 drives with parity 2: D == P, so 2 to read and 3 to
	// write. Set 0: n1 online, n2 offline, n3 healing, n4 online;
	// set 1: n2 and n4 offline, n3 online.
	require.Len(t, info.Pools, 1)
	pool := info.Pools[0]
	assert.Equal(t, 2, pool.ReadQuorum)
	assert.Equal(t, 3, pool.WriteQuorum)
	assert.Equal(t, []domain.ErasureSetInfo{
		{Index: 0, Drives: 4, Online: 3, Offline: 1, Healing: 1, State: domain.SetStateDegraded},
		{Index: 1, Drives: 4, Online: 1, Offline: 3, State: domain.SetStateUnavailable},
	}, pool.ErasureSets)

	peers.EXPECT().Nodes(mock.Anything).Return(nil, errors.New("unreachable")).Once()
	peers.EXPECT().ClockSkew(mock.Anything).Return(nil, errors.New("unreachable")).Once()
	info, err = svc.ServerInfo(context.Background())
	require.NoError(t, err)
	require.Len(t, info.Nodes, 1)
	assert.Equal(t, "local", info.Nodes[0].Endpoint)
}

// TestPoolStates covers the quorum rule and the set states.
func TestPoolStates(t *testing.T) {
	tests := []struct {
		name                string
		drives, parity      int
		online, healing     int
		wantRead, wantWrite int
		want                string
	}{
		{"single drive", 1, 0, 1, 0, 1, 1, domain.SetStateOK},
		{"single drive gone", 1, 0, 0, 0, 1, 1, domain.SetStateUnavailable},
		{"16 drives EC:4 all online", 16, 4, 16, 0, 12, 12, domain.SetStateOK},
		{"16 drives EC:4 one healing", 16, 4, 16, 1, 12, 12, domain.SetStateDegraded},
		{"16 drives EC:4 four offline", 16, 4, 12, 0, 12, 12, domain.SetStateDegraded},
		{"16 drives EC:4 five offline", 16, 4, 11, 0, 12, 12, domain.SetStateUnavailable},
		{"8 drives EC:4 three offline", 8, 4, 5, 0, 4, 5, domain.SetStateDegraded},
		{"8 drives EC:4 four offline", 8, 4, 4, 0, 4, 5, domain.SetStateReadOnly},
		{"8 drives EC:4 five offline", 8, 4, 3, 0, 4, 5, domain.SetStateUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var disks []domain.DiskInfo
			for i := range tt.drives {
				d := domain.DiskInfo{DiskIdx: i, State: domain.StateOffline}
				if i < tt.online {
					d.State = domain.StateOnline
				}
				if i < tt.healing {
					d.State, d.Healing = domain.StateHealing, true
				}
				disks = append(disks, d)
			}
			pools := admin.PoolStates([]domain.PoolInfo{{Sets: 1, DrivesPerSet: tt.drives, Parity: tt.parity}}, []domain.NodeInfo{{Disks: disks}})
			require.Len(t, pools, 1)
			assert.Equal(t, tt.wantRead, pools[0].ReadQuorum)
			assert.Equal(t, tt.wantWrite, pools[0].WriteQuorum)
			require.Len(t, pools[0].ErasureSets, 1)
			set := pools[0].ErasureSets[0]
			assert.Equal(t, tt.want, set.State)
			assert.Equal(t, tt.online, set.Online)
			assert.Equal(t, tt.drives-tt.online, set.Offline)
			assert.Equal(t, tt.healing, set.Healing)
		})
	}
}

func TestServerInfoTLSAndTelemetry(t *testing.T) {
	notAfter := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	pairs := []certs.KeyPair{
		{Name: "", Domains: []string{"nexo.example.com"}, Subject: "CN=nexo", Issuer: "CN=ca", NotAfter: notAfter, Fingerprint: "ab"},
		{Name: "other", Domains: []string{"*.example.org"}, IPs: []string{"10.0.0.1"}, Subject: "CN=other", Issuer: "CN=ca", NotAfter: notAfter, Fingerprint: "cd"},
	}
	tests := []struct {
		name      string
		tls       *domain.TLSInfo
		keystore  bool
		telemetry *domain.TelemetryInfo
		wantTLS   *domain.TLSInfo
	}{
		{name: "not reported"},
		{
			name:    "plain HTTP",
			tls:     &domain.TLSInfo{MinVersion: "1.2"},
			wantTLS: &domain.TLSInfo{MinVersion: "1.2"},
		},
		{
			name:      "certificates of the keystore",
			tls:       &domain.TLSInfo{Enabled: true, MinVersion: "1.3", InternodeVerify: true, Certificates: []domain.TLSCertificate{{Name: "stale"}}},
			keystore:  true,
			telemetry: &domain.TelemetryInfo{Enabled: true, Endpoint: "http://otel:4318", Protocol: "http/protobuf", SampleRatio: 0.5},
			wantTLS: &domain.TLSInfo{Enabled: true, MinVersion: "1.3", InternodeVerify: true, Certificates: []domain.TLSCertificate{
				{Domains: []string{"nexo.example.com"}, Subject: "CN=nexo", Issuer: "CN=ca", NotAfter: notAfter, Fingerprint: "ab"},
				{Name: "other", Domains: []string{"*.example.org"}, IPs: []string{"10.0.0.1"}, Subject: "CN=other", Issuer: "CN=ca", NotAfter: notAfter, Fingerprint: "cd"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := admin.Deps{Clock: clock.NewFake(time.Unix(0, 0)), TLS: tt.tls, Telemetry: tt.telemetry}
			if tt.keystore {
				ks := certsmock.NewKeystore(t)
				ks.EXPECT().KeyPairs(mock.Anything).Return(pairs)
				deps.Keystore = ks
			}
			info, err := admin.New(deps).ServerInfo(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.wantTLS, info.TLS)
			assert.Equal(t, tt.telemetry, info.Telemetry)
			if tt.telemetry != nil {
				assert.NotSame(t, tt.telemetry, info.Telemetry, "a copy")
			}
		})
	}
}
