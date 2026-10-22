package object_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

const replTarget = "arn:nexo:replication::t1:dst"

// replicationConfig replicates the keys under docs/ to replTarget, and the
// keys tagged team=red to a second target.
func replicationConfig(t *testing.T) *domain.ReplicationConfig {
	t.Helper()
	doc := `<ReplicationConfiguration>
  <Rule><ID>docs</ID><Priority>1</Priority><Status>Enabled</Status><Filter><Prefix>docs/</Prefix></Filter>
    <DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication>
    <Destination><Bucket>` + replTarget + `</Bucket></Destination></Rule>
  <Rule><ID>red</ID><Priority>2</Priority><Status>Enabled</Status><Filter><Tag><Key>team</Key><Value>red</Value></Tag></Filter>
    <Destination><Bucket>arn:nexo:replication::t2:other</Bucket></Destination></Rule>
</ReplicationConfiguration>`
	cfg, err := domain.ParseReplicationConfig([]byte(doc), nil)
	require.NoError(t, err)
	return cfg
}

// TestReplicationStatus (S-BKT-12): a version a rule selects is written
// PENDING for each of its destinations; the states recorded per target
// give the overall state; the others have none.
//
// covers: S-BKT-12
func TestReplicationStatus(t *testing.T) {
	for _, kind := range implKinds() {
		t.Run(kind, func(t *testing.T) {
			fx := newFixture(t, kind)
			ctx := context.Background()
			require.NoError(t, fx.svc.MakeBucket(ctx, "src", object.MakeBucketOptions{}))
			fx.setBucketConfig("src", object.BucketConfig{Versioning: domain.VersioningEnabled, Replication: replicationConfig(t)})

			put := func(key string, tags map[string]string) domain.ObjectInfo {
				info, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "src", Key: key, Reader: bytes.NewReader([]byte("data")), Size: 4, Tags: tags})
				require.NoError(t, err)
				return info
			}
			skipped := put("other/a", nil)
			assert.Empty(t, skipped.ReplicationStatus)
			both := put("docs/a", map[string]string{"team": "red"})
			assert.Equal(t, domain.ReplicationPending, both.ReplicationStatus)
			assert.Equal(t, map[string]domain.ReplicationStatus{replTarget: domain.ReplicationPending, "arn:nexo:replication::t2:other": domain.ReplicationPending}, both.ReplicationTargets)

			info, err := fx.svc.SetReplicationStatus(ctx, "src", "docs/a", both.VersionID, replTarget, domain.ReplicationCompleted)
			require.NoError(t, err)
			assert.Equal(t, domain.ReplicationPending, info.ReplicationStatus, "one destination still pending")
			info, err = fx.svc.SetReplicationStatus(ctx, "src", "docs/a", both.VersionID, "arn:nexo:replication::t2:other", domain.ReplicationFailed)
			require.NoError(t, err)
			assert.Equal(t, domain.ReplicationFailed, info.ReplicationStatus)
			info, err = fx.svc.SetReplicationStatus(ctx, "src", "docs/a", both.VersionID, "arn:nexo:replication::t2:other", domain.ReplicationCompleted)
			require.NoError(t, err)
			assert.Equal(t, domain.ReplicationCompleted, info.ReplicationStatus)
			got, err := fx.svc.GetObjectInfo(ctx, "src", "docs/a", object.GetObjectOptions{VersionID: both.VersionID})
			require.NoError(t, err)
			assert.Equal(t, domain.ReplicationCompleted, got.ReplicationStatus)
			assert.Equal(t, both.ModTime, got.ModTime, "the version keeps its time")
			assert.Empty(t, got.UserMetadata)

			// A copy is a new write: selected again (its tags copied too).
			cp, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "src", SrcKey: "docs/a", DstBucket: "src", DstKey: "docs/b", ReplaceTags: true})
			require.NoError(t, err)
			assert.Equal(t, map[string]domain.ReplicationStatus{replTarget: domain.ReplicationPending}, cp.ReplicationTargets)

			_, err = fx.svc.SetReplicationStatus(ctx, "src", "docs/a", both.VersionID, replTarget, "DONE")
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
}

// TestReplicaWrites (S-BKT-12): the writes of the replication of another
// site keep the version id, the time and the ETag of the source, are
// REPLICA (never replicated again) whatever the rules, and a replicated
// delete marker keeps its id.
//
// covers: S-BKT-12
func TestReplicaWrites(t *testing.T) {
	for _, kind := range implKinds() {
		t.Run(kind, func(t *testing.T) {
			fx := newFixture(t, kind)
			ctx := context.Background()
			require.NoError(t, fx.svc.MakeBucket(ctx, "dst", object.MakeBucketOptions{}))
			fx.setBucketConfig("dst", object.BucketConfig{Versioning: domain.VersioningEnabled, Replication: replicationConfig(t)})

			at := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
			const vid = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
			info, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "dst", Key: "docs/r", Reader: bytes.NewReader([]byte("replica")), Size: 7,
				VersionID: vid, ModTime: at, Replica: true, ReplicaETag: "0123456789abcdef0123456789abcdef-3"})
			require.NoError(t, err)
			assert.Equal(t, vid, info.VersionID)
			assert.True(t, info.ModTime.Equal(at))
			assert.Equal(t, domain.ReplicationReplica, info.ReplicationStatus)
			assert.Empty(t, info.ReplicationTargets)
			assert.Equal(t, "0123456789abcdef0123456789abcdef-3", info.ETag)

			// An older replica sorts below the newer local version: the
			// last writer wins.
			local, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "dst", Key: "docs/lww", Reader: bytes.NewReader([]byte("new")), Size: 3})
			require.NoError(t, err)
			_, err = fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "dst", Key: "docs/lww", Reader: bytes.NewReader([]byte("old")), Size: 3,
				VersionID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5c", ModTime: local.ModTime.Add(-time.Hour), Replica: true})
			require.NoError(t, err)
			r, err := fx.svc.GetObject(ctx, "dst", "docs/lww", object.GetObjectOptions{})
			require.NoError(t, err)
			body, _ := io.ReadAll(r)
			_ = r.Close()
			assert.Equal(t, "new", string(body))

			// A multipart replica.
			up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "dst", Key: "docs/mp", Replica: true,
				VersionID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5d", ModTime: at})
			require.NoError(t, err)
			p1 := uploadPart(t, fx.svc, "dst", "docs/mp", up.UploadID, 1, bytes.Repeat([]byte("a"), 32))
			done, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "dst", Key: "docs/mp", UploadID: up.UploadID,
				Parts: []domain.CompletePart{{Number: 1, ETag: p1.ETag}}})
			require.NoError(t, err)
			assert.Equal(t, "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5d", done.VersionID)
			assert.True(t, done.ModTime.Equal(at))
			assert.Equal(t, domain.ReplicationReplica, done.ReplicationStatus)

			// A replicated delete marker.
			del, err := fx.svc.DeleteObject(ctx, "dst", "docs/r", object.DeleteObjectOptions{MarkerVersionID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e", ModTime: at.Add(time.Second)})
			require.NoError(t, err)
			assert.Equal(t, "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5e", del.DeleteMarkerVersionID)
		})
	}
}
