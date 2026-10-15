// Package upgrade holds the upgrade tests of Nexo (docs/05 T-9.8, docs/06
// §6.1): the release under test takes over the drives written by the
// release before it, and the release before it may take them back.
//
// TestMain builds both server binaries: the release before from a git
// commit (NEXO_UPGRADE_FROM, the M7 commit 224bb31 by default; its tree
// is extracted with git archive into a temporary directory, so the
// repository needs that commit) and the release under test from the
// working tree. NEXO_UPGRADE_OLD_BIN and NEXO_UPGRADE_NEW_BIN use prebuilt
// binaries instead.
//
//   - TestUpgradeSNMD: a single node with 4 drives. The old release writes
//     versioned objects (several versions and a delete marker), multipart
//     uploads (one completed with CRC32C checksums, one left open with two
//     parts), SSE-S3, SSE-KMS (builtin KMS) and SSE-C objects, a bucket
//     with default encryption, Object Lock retention and legal hold, tags
//     and metadata, IAM users, policies and access keys, a bucket policy,
//     lifecycle, CORS, bucket notifications to a webhook and an event
//     queued while the webhook was down. The new release reads all of it
//     back (sha256 of every version, ETags, checksums, decryption, locks,
//     configurations, permissions), delivers the queued event, completes
//     the open upload and changes the data. The old release is then
//     started again on the drives (downgrade, see below) and the new one
//     once more: nothing is lost either way.
//   - TestRollingUpgradeCluster: 4 nodes of 2 drives behind a load
//     balancer, upgraded one node at a time while a workload writes,
//     reads and deletes (every read checked against the sha256 of the
//     last acknowledged write): no client error beyond the retries of the
//     SDK, then the data set of the old release is verified, a node of
//     the old release rejoins the upgraded cluster (rollback of one node)
//     and leaves it again without corrupting anything.
//
// Downgrade policy (docs/08): a release reads the drives of the releases
// before it of the same major version. Going back is supported only
// between releases with the same on-disk and internode formats
// (format.json version, xl.meta major version, internode protocol): the
// tests check that the release under test still writes the formats the
// release before reads, and run it; a release that changes a format bumps
// it, and the older release then refuses the drives (it does not start)
// rather than misreading them.
//
// The tests are behind the "upgrade" build tag and need Linux (signals,
// per-process /proc/self/cwd endpoints of the cluster). Run them with
//
//	make test-upgrade
//	go test -tags upgrade -count=1 -v ./test/upgrade/...
package upgrade
