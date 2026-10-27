package ilm_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// memTiers is a tier store in memory, with its tier names.
type memTiers struct {
	mu      sync.Mutex
	names   []string
	objects map[string][]byte
	next    int
}

func (m *memTiers) TierNames(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.names...), nil
}

func (m *memTiers) PutTiered(_ context.Context, tier string, r io.Reader, _ int64, _ object.TierRef) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	name := fmt.Sprintf("%s/%d", tier, m.next)
	m.objects[name] = data
	return name, nil
}

func (m *memTiers) OpenTiered(_ context.Context, _, name string, offset, length int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return io.NopCloser(bytes.NewReader(m.objects[name][offset : offset+length])), nil
}

func (m *memTiers) DeleteTiered(_ context.Context, _, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, name)
	return nil
}

func (w *world) storageClasses(bucket string) map[string]string {
	w.t.Helper()
	res, err := w.objects.ListObjectVersions(context.Background(), domain.ListObjectsInput{Bucket: bucket, IncludeVersions: true})
	require.NoError(w.t, err)
	out := map[string]string{}
	for _, v := range res.Objects {
		out[v.Key+"@"+v.VersionID] = v.StorageClass
	}
	return out
}

// TestTransitions covers S-BKT-07 (transitions) and S-STO-12: the current
// versions move to their tier after their days, the noncurrent ones after
// theirs, an expiration due wins over a transition, the inline versions
// stay, and the transitioned data reads the same.
//
// covers: S-BKT-07, S-STO-12
func TestTransitions(t *testing.T) {
	ctx := context.Background()
	tiers := &memTiers{names: []string{"COLD"}, objects: map[string][]byte{}}
	w := newWorldWith(t, tiers, tiers)
	w.bucket("ver", domain.VersioningEnabled, doc(
		`<Rule><ID>cold</ID><Status>Enabled</Status><Filter><Prefix>data/</Prefix></Filter><Transition><Days>30</Days><StorageClass>COLD</StorageClass></Transition><NoncurrentVersionTransition><NoncurrentDays>1</NoncurrentDays><StorageClass>COLD</StorageClass></NoncurrentVersionTransition></Rule>`,
		`<Rule><ID>tmp</ID><Status>Enabled</Status><Filter><Prefix>data/tmp/</Prefix></Filter><Expiration><Days>5</Days></Expiration><Transition><Days>2</Days><StorageClass>COLD</StorageClass></Transition></Rule>`,
	))
	v1 := w.put("ver", "data/report.csv")
	w.clk.Advance(24 * time.Hour)
	v2 := w.put("ver", "data/report.csv")
	tiny := w.put("ver", "data/x")
	tmp := w.put("ver", "data/tmp/scratch.bin")

	w.clk.Advance(3 * 24 * time.Hour)
	res, err := w.svc.ApplyNow(ctx, "ver")
	require.NoError(t, err)
	assert.Equal(t, 2, res.Transitioned, "v1 noncurrent for 3 days, the scratch file 3 days old")
	classes := w.storageClasses("ver")
	assert.Equal(t, "COLD", classes["data/report.csv@"+v1.VersionID])
	assert.Equal(t, domain.StorageClassStandard, classes["data/report.csv@"+v2.VersionID])
	assert.Equal(t, "COLD", classes["data/tmp/scratch.bin@"+tmp.VersionID])
	assert.Equal(t, domain.StorageClassStandard, classes["data/x@"+tiny.VersionID], "a version stored inline stays")

	r, err := w.objects.GetObject(ctx, "ver", "data/report.csv", object.GetObjectOptions{VersionID: v1.VersionID})
	require.NoError(t, err)
	body, err := io.ReadAll(r)
	require.NoError(t, r.Close())
	require.NoError(t, err)
	assert.Equal(t, "data/report.csv", string(body))

	// Again: nothing more is due.
	res, err = w.svc.ApplyNow(ctx, "ver")
	require.NoError(t, err)
	assert.Zero(t, res.Transitioned)

	// The scratch file expires (a delete marker): the expiration wins.
	w.clk.Advance(3 * 24 * time.Hour)
	res, err = w.svc.ApplyNow(ctx, "ver")
	require.NoError(t, err)
	assert.Equal(t, 1, res.DeleteMarkers)

	w.clk.Advance(30 * 24 * time.Hour)
	res, err = w.svc.ApplyNow(ctx, "ver")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Transitioned, "v2 is 34 days old")
	assert.Equal(t, "COLD", w.storageClasses("ver")["data/report.csv@"+v2.VersionID])
}

// TestTransitionToUnknownTier: a configuration naming no tier is refused,
// and accepted once the tier exists.
//
// covers: S-STO-12
func TestTransitionToUnknownTier(t *testing.T) {
	ctx := context.Background()
	tiers := &memTiers{objects: map[string][]byte{}}
	w := newWorldWith(t, tiers, tiers)
	cfg := []byte(doc(`<Rule><ID>cold</ID><Status>Enabled</Status><Filter></Filter><Transition><Days>1</Days><StorageClass>COLD</StorageClass></Transition></Rule>`))
	_, err := w.svc.Validate(ctx, cfg)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	var s3err *domain.S3Error
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "InvalidStorageClass", s3err.Code)
	tiers.mu.Lock()
	tiers.names = []string{"COLD"}
	tiers.mu.Unlock()
	_, err = w.svc.Validate(ctx, cfg)
	require.NoError(t, err)
	_, err = ilm.New(ilm.Deps{Objects: w.objects, Buckets: nil})
	require.Error(t, err)
}
