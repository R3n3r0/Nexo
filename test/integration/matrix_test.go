package integration_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
)

// Deployment modes of the matrix (docs/08 §8.6 TM-01).
const (
	modeSNSD    = "snsd"
	modeSNMD    = "snmd"
	modeCluster = "cluster"
)

// matrixTest is the name of the test running the suite in the
// erasure-coded modes.
const matrixTest = "TestDeploymentModes"

// modeOf returns the deployment mode a test runs in: TestDeploymentModes
// names it in the path of its subtests ("TestDeploymentModes/<mode>/..."),
// every other test runs on a single drive.
func modeOf(t testing.TB) string {
	parts := strings.Split(t.Name(), "/")
	if len(parts) >= 2 && parts[0] == matrixTest {
		return parts[1]
	}
	return modeSNSD
}

// startDeployment starts a deployment of the given mode: one drive, one
// node with 4 drives (EC 2+2), or 4 nodes x 2 drives (EC 4+4) behind the
// load balancer of testutil.StartCluster.
func startDeployment(t *testing.T, mode string, opts ...testutil.Option) *testutil.Server {
	t.Helper()
	switch mode {
	case modeSNMD:
		return testutil.StartServer(t, append([]testutil.Option{testutil.Drives(4)}, opts...)...)
	case modeCluster:
		return testutil.StartCluster(t, 4, 2, opts...)
	default:
		return testutil.StartServer(t, opts...)
	}
}

// suiteTest is a test of the suite run in every mode.
type suiteTest struct {
	name string
	fn   func(t *testing.T)
	// slow tests (thousands of objects, 100 MiB transfers, hundreds of
	// concurrent requests) are skipped in the erasure-coded modes with
	// -short and under the race detector, which multiplies their cost
	// (the CI runs them without it: make test-integration).
	slow bool
}

// suite lists the tests of the package run in every deployment mode.
// TestSuiteCoversEveryTest keeps it complete.
func suite() []suiteTest {
	return []suiteTest{
		{name: "TestPolicyMatrix", fn: TestPolicyMatrix},
		{name: "TestPolicyConditionMatrix", fn: TestPolicyConditionMatrix},
		{name: "TestPublicReadBucket", fn: TestPublicReadBucket},
		{name: "TestAnonymousPrefixAccess", fn: TestAnonymousPrefixAccess},
		{name: "TestServiceAccountNarrowPolicy", fn: TestServiceAccountNarrowPolicy},
		{name: "TestServiceAccountBroadPolicy", fn: TestServiceAccountBroadPolicy},
		{name: "TestShareLinkRevocation", fn: TestShareLinkRevocation},
		{name: "TestBucketPolicyRoundTrip", fn: TestBucketPolicyRoundTrip},
		{name: "TestConsoleBulkPrefixNeedsListBucket", fn: TestConsoleBulkPrefixNeedsListBucket},
		{name: "TestConsoleBehindProxy", fn: TestConsoleBehindProxy},
		{name: "TestS3BehindProxyUsesTheForwardedClient", fn: TestS3BehindProxyUsesTheForwardedClient},
		{name: "TestConsoleMatchesS3PolicyConditions", fn: TestConsoleMatchesS3PolicyConditions},
		{name: "TestConsoleHomeDirectory", fn: TestConsoleHomeDirectory},
		{name: "TestConsolePrefixShareLinks", fn: TestConsolePrefixShareLinks},
		{name: "TestConsoleRootAccessSetting", fn: TestConsoleRootAccessSetting},
		{name: "TestConsoleRevocations", fn: TestConsoleRevocations},
		{name: "TestConsoleDisableRevokes", fn: TestConsoleDisableRevokes},
		{name: "TestConsoleRestoreVersion", fn: TestConsoleRestoreVersion},
		{name: "TestConsoleNullVersionDeletes", fn: TestConsoleNullVersionDeletes},
		{name: "TestSpecialCharacterKeys", fn: TestSpecialCharacterKeys},
		{name: "TestPathTraversalKeys", fn: TestPathTraversalKeys},
		{name: "TestMultipart", fn: TestMultipart},
		{name: "TestBucketLifecycle", fn: TestBucketLifecycle},
		{name: "TestReservedBucketName", fn: TestReservedBucketName},
		{name: "TestPutGetSizes", fn: TestPutGetSizes, slow: true},
		{name: "TestRangeGet", fn: TestRangeGet},
		{name: "TestConditionalRequests", fn: TestConditionalRequests},
		{name: "TestListing", fn: TestListing, slow: true},
		{name: "TestListingEdgeCases", fn: TestListingEdgeCases},
		{name: "TestVersioning", fn: TestVersioning},
		{name: "TestConcurrentPutSameKey", fn: TestConcurrentPutSameKey, slow: true},
		{name: "TestReadAfterWrite", fn: TestReadAfterWrite},
		{name: "TestPostPolicy", fn: TestPostPolicy},
		{name: "TestPostPolicyRedirect", fn: TestPostPolicyRedirect},
		{name: "TestPostPolicyTruncatedForm", fn: TestPostPolicyTruncatedForm},
		{name: "TestPresignedOperations", fn: TestPresignedOperations},
		{name: "TestPresignedExpiresBounds", fn: TestPresignedExpiresBounds},
		{name: "TestPresignedExpiry", fn: TestPresignedExpiry},
		{name: "TestPresignedTampering", fn: TestPresignedTampering},
		{name: "TestPresignedSignedHeaders", fn: TestPresignedSignedHeaders},
		{name: "TestPresignedRevokedSigner", fn: TestPresignedRevokedSigner},
		{name: "TestPresignedResponseOverrides", fn: TestPresignedResponseOverrides},
		{name: "TestPresignedUnsignedHeaders", fn: TestPresignedUnsignedHeaders},
		{name: "TestPresignedBehindProxy", fn: TestPresignedBehindProxy},
		{name: "TestS3ErrorDocuments", fn: TestS3ErrorDocuments},
		{name: "TestRequestIDHeaders", fn: TestRequestIDHeaders},
		{name: "TestVirtualHostStyle", fn: TestVirtualHostStyle},
		{name: "TestVirtualHostSystemPathsAreKeys", fn: TestVirtualHostSystemPathsAreKeys},
		{name: "TestCredentialScopeService", fn: TestCredentialScopeService},
		{name: "TestPayloadIntegrity", fn: TestPayloadIntegrity},
		{name: "TestRestartPreservesState", fn: TestRestartPreservesState},
		{name: "TestInterruptedUpload", fn: TestInterruptedUpload, slow: true},
		{name: "TestLifecycleExpiration", fn: TestLifecycleExpiration},
		{name: "TestBucketNotifications", fn: TestBucketNotifications},
		{name: "TestBucketNotificationsOfflineTarget", fn: TestBucketNotificationsOfflineTarget},
		{name: "TestVersioningDeleteMarkers", fn: TestVersioningDeleteMarkers},
		{name: "TestVersioningListAndBatchDelete", fn: TestVersioningListAndBatchDelete},
		{name: "TestVersioningSuspendedAndMultipart", fn: TestVersioningSuspendedAndMultipart},
		{name: "TestObjectLockBucket", fn: TestObjectLockBucket},
		{name: "TestObjectLockRetentionAndLegalHold", fn: TestObjectLockRetentionAndLegalHold},
		{name: "TestObjectLockForceDeleteRace", fn: TestObjectLockForceDeleteRace},
		{name: "TestChecksums", fn: TestChecksums},
		{name: "TestMultipartChecksums", fn: TestMultipartChecksums},
		{name: "TestBucketCORS", fn: TestBucketCORS},
		{name: "TestBucketTagging", fn: TestBucketTagging},
		{name: "TestObjectTagsAndPolicies", fn: TestObjectTagsAndPolicies},
		{name: "TestServerSideEncryptionKMS", fn: TestServerSideEncryptionKMS},
		{name: "TestServerSideEncryptionKES", fn: TestServerSideEncryptionKES},
		{name: "TestServerSideEncryptionCustomerKeys", fn: TestServerSideEncryptionCustomerKeys},
		{name: "TestSSESealedChecksumsAndPartKeys", fn: TestSSESealedChecksumsAndPartKeys},
		{name: "TestBucketDefaultEncryption", fn: TestBucketDefaultEncryption},
		{name: "TestEncryptionWithoutKMS", fn: TestEncryptionWithoutKMS},
		{name: "TestKMSAdminAPI", fn: TestKMSAdminAPI},
		{name: "TestBucketQuota", fn: TestBucketQuota},
		{name: "TestConsoleBucketConfigurations", fn: TestConsoleBucketConfigurations},
		{name: "TestConsoleObjectLock", fn: TestConsoleObjectLock},
		{name: "TestConsoleUploadEncryption", fn: TestConsoleUploadEncryption},
		{name: "TestConsoleKeepsEncryption", fn: TestConsoleKeepsEncryption},
		{name: "TestStalledDownloadReleasesLock", fn: TestStalledDownloadReleasesLock},
		{name: "TestStalledDownloadIsASnapshot", fn: TestStalledDownloadIsASnapshot},
		{name: "TestStalledWebsiteDownloadReleasesLock", fn: TestStalledWebsiteDownloadReleasesLock},
		{name: "TestMultipartCompletionRetry", fn: TestMultipartCompletionRetry},
		{name: "TestMultipartCompletionValidation", fn: TestMultipartCompletionValidation},
		{name: "TestRequestHeaderValidation", fn: TestRequestHeaderValidation},
		{name: "TestS3ResponseDetails", fn: TestS3ResponseDetails},
		{name: "TestObjectLockValidation", fn: TestObjectLockValidation},
		{name: "TestSSECustomerKeyRequirements", fn: TestSSECustomerKeyRequirements},
		{name: "TestSSECustomerKeyOverPlainHTTP", fn: TestSSECustomerKeyOverPlainHTTP},
		{name: "TestRateLimitS3", fn: TestRateLimitS3},
		{name: "TestRateLimitConcurrency", fn: TestRateLimitConcurrency},
		{name: "TestRateLimitAdminAPI", fn: TestRateLimitAdminAPI},
		{name: "TestRateLimitDistributions", fn: TestRateLimitDistributions},
		{name: "TestDistributionWebsite", fn: TestDistributionWebsite},
		{name: "TestDistributionS3Mode", fn: TestDistributionS3Mode},
		{name: "TestDistributionSignedAndRestricted", fn: TestDistributionSignedAndRestricted},
		{name: "TestDistributionWebListeners", fn: TestDistributionWebListeners},
		{name: "TestDistributionACME", fn: TestDistributionACME},
		{name: "TestDistributionACMEDNS01", fn: TestDistributionACMEDNS01},
		{name: "TestDistributionAliasTakeover", fn: TestDistributionAliasTakeover},
		{name: "TestDistributionListenerModes", fn: TestDistributionListenerModes},
		{name: "TestInternodeMutualTLS", fn: TestInternodeMutualTLS},
		{name: "TestAccessRulesS3", fn: TestAccessRulesS3},
		{name: "TestAccessRulesBehindProxies", fn: TestAccessRulesBehindProxies},
		{name: "TestAccessRulesConsole", fn: TestAccessRulesConsole},
		{name: "TestAccessRulesPublicAndBlocked", fn: TestAccessRulesPublicAndBlocked},
		{name: "TestAccessRulesPropagation", fn: TestAccessRulesPropagation},
		{name: "TestCacheServesAndInvalidates", fn: TestCacheServesAndInvalidates},
		{name: "TestCacheManualInvalidation", fn: TestCacheManualInvalidation},
		{name: "TestCacheSecurity", fn: TestCacheSecurity},
		{name: "TestCacheDomainsAndConfig", fn: TestCacheDomainsAndConfig},
		{name: "TestCacheSharedMode", fn: TestCacheSharedMode},
		{name: "TestCacheKeyIsUnambiguous", fn: TestCacheKeyIsUnambiguous},
		{name: "TestCacheStaleIfOriginDown", fn: TestCacheStaleIfOriginDown},
		{name: "TestDiagnostics", fn: TestDiagnostics},
		{name: "TestLiveTrace", fn: TestLiveTrace},
		{name: "TestAuditLog", fn: TestAuditLog},
		{name: "TestClusterMetrics", fn: TestClusterMetrics},
		{name: "TestConsoleVideoStreaming", fn: TestConsoleVideoStreaming},
		{name: "TestVideoStreamShare", fn: TestVideoStreamShare},
		{name: "TestMediaWorker", fn: TestMediaWorker},
	}
}

// TestDeploymentModes runs the integration suite on the erasure-coded
// modes (TM-01): SNMD with 4 drives and a cluster of 4 nodes x 2 drives in
// the test process. The modes run in parallel, and so do the tests that
// call t.Parallel.
//
// covers: S-STO-00, S-STO-02, S-STO-06
func TestDeploymentModes(t *testing.T) {
	for _, mode := range []string{modeSNMD, modeCluster} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			for _, tc := range suite() {
				t.Run(tc.name, func(t *testing.T) {
					if tc.slow && (testing.Short() || raceDetector) {
						t.Skipf("slow test of the %s mode skipped with -short and -race", mode)
					}
					tc.fn(t)
				})
			}
		})
	}
}

// TestSuiteCoversEveryTest keeps the suite of TestDeploymentModes in sync
// with the tests of the package: a new test runs in every mode.
func TestSuiteCoversEveryTest(t *testing.T) {
	listed := map[string]bool{}
	for _, tc := range suite() {
		assert.False(t, listed[tc.name], "%s listed twice", tc.name)
		listed[tc.name] = true
	}
	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	found := 0
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Params.NumFields() != 1 {
				continue
			}
			switch fn.Name.Name {
			case matrixTest, "TestSuiteCoversEveryTest", migrationTest:
				continue
			}
			found++
			assert.True(t, listed[fn.Name.Name], "%s (%s) is not in the suite of %s", fn.Name.Name, file, matrixTest)
		}
	}
	assert.Equal(t, len(listed), found, "every listed test exists")
}
