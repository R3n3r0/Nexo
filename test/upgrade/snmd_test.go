//go:build upgrade

package upgrade_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpgradeSNMD: a single node with 4 drives written by the release
// before is taken over by the release under test, which reads back every
// version, encryption, lock, configuration, permission and queued event,
// then changes the data; the release before is started again on the
// drives (downgrade: it serves them when the formats did not change, else
// it refuses them) and the release under test once more, nothing lost.
//
// covers: T-9.8
func TestUpgradeSNMD(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, 1, 4)
	n := d.node(1)
	hook := newWebhook(t)
	drive := d.driveDirs(1)[0]

	// 1. The release before writes the data set.
	n.start(oldBin)
	n.waitReady()
	before := serverVersion(t, n)
	ds := writeDataset(t, n.rootClient(), n.admin(), hook)
	ds.verifyObjects(t, n.rootClient(), "old release")
	old := driveFormats(t, drive, bucketData, "keep")
	n.stop()

	// 2. The release under test takes the drives over.
	n.start(newBin)
	n.waitReady()
	after := serverVersion(t, n)
	t.Logf("upgrade %s -> %s", before, after)
	if builtVersions {
		assert.Equal(t, oldVersion, before)
		assert.Equal(t, newVersion, after)
	}
	c := n.rootClient()
	ds.verifyObjects(t, c, "upgraded")
	ds.verifyState(t, n, "upgraded")

	// The event queued while the webhook was down is delivered by the new
	// release, and the configuration keeps working.
	hook.offline.Store(false)
	hook.waitEvent(t, "queued-before-upgrade")
	cfg, err := c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(bucketEvents)})
	require.NoError(t, err)
	require.Len(t, cfg.QueueConfigurations, 1)
	assert.Equal(t, webhookARN(), aws.ToString(cfg.QueueConfigurations[0].QueueArn))
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucketEvents), Key: aws.String("after-upgrade"), Body: strings.NewReader("x")})
	require.NoError(t, err)
	hook.waitEvent(t, "after-upgrade")

	// The new release changes the data set.
	ds.change(t, c, "new")
	ds.verifyObjects(t, c, "changed by the new release")
	ds.verifyState(t, n, "changed by the new release")
	written := driveFormats(t, drive, bucketData, "sse-kms-new")
	n.stop()

	// The formats the release under test writes are those the release
	// before reads (format.json untouched, same xl.meta major version):
	// the downgrade is then supported, and must work.
	require.Equal(t, old.deployment, written.deployment, "same deployment")
	compatible := old.formatVersion == written.formatVersion && old.xlMetaMajor == written.xlMetaMajor
	t.Logf("formats: format.json v%d -> v%d, xl.meta %d.x -> %d.x", old.formatVersion, written.formatVersion, old.xlMetaMajor, written.xlMetaMajor)

	// 3. Downgrade: the release before on the drives the new one changed.
	n.start(oldBin)
	err = n.awaitReady(readyTimeout)
	switch {
	case compatible:
		require.NoError(t, err, "same formats: the release before serves the drives")
		t.Log("downgrade: the release before serves the drives the release under test changed")
		c = n.rootClient()
		ds.verifyObjects(t, c, "downgraded")
		ds.verifyState(t, n, "downgraded")
		ds.change(t, c, "old-again")
		n.stop()
	default:
		require.Error(t, err, "newer formats: the release before refuses the drives")
		require.True(t, n.exited(), "a release refusing the drives exits rather than serving them")
		t.Logf("downgrade refused, as expected with newer formats: %v", err)
		require.Equal(t, written, driveFormats(t, drive, bucketData, "sse-kms-new"), "the refused release left the drives as they were")
	}

	// 4. The release under test once more: nothing was lost either way.
	n.start(newBin)
	n.waitReady()
	c = n.rootClient()
	ds.verifyObjects(t, c, "upgraded again")
	ds.verifyState(t, n, "upgraded again")
	n.stop()
}
