package admin

import "github.com/r3n3r0/nexo/internal/service/policy"

// actionScope tells how an action of the catalog is evaluated by
// Permissions.
type actionScope int

const (
	// scopeGlobal actions are never bound to a bucket (admin:*,
	// s3:ListAllMyBuckets, s3:CreateBucket).
	scopeGlobal actionScope = iota
	// scopeBucket actions are evaluated on the bucket.
	scopeBucket
	// scopeObject actions are evaluated on "<bucket>/<prefix>*".
	scopeObject
)

// catalogEntry is an action of the permission catalog.
type catalogEntry struct {
	action policy.Action
	scope  actionScope
}

// catalog lists the actions the console and the Admin API care about: the
// admin:* action of every Admin API endpoint and the s3:* actions of the
// bucket and object operations. Permissions evaluates exactly these.
func catalog() []catalogEntry {
	scoped := func(scope actionScope, actions ...policy.Action) []catalogEntry {
		out := make([]catalogEntry, 0, len(actions))
		for _, a := range actions {
			out = append(out, catalogEntry{action: a, scope: scope})
		}
		return out
	}
	var out []catalogEntry
	out = append(out, scoped(scopeGlobal,
		policy.ActionAdminServerInfo, policy.ActionAdminDataUsage, policy.ActionAdminHeal,
		policy.ActionAdminServiceRestart, policy.ActionAdminServiceStop, policy.ActionAdminServerTrace,
		policy.ActionAdminConsoleLog, policy.ActionAdminConfigWrite,
		policy.ActionAdminCreateUser, policy.ActionAdminDeleteUser, policy.ActionAdminListUsers, policy.ActionAdminGetUser,
		policy.ActionAdminEnableUser, policy.ActionAdminDisableUser,
		policy.ActionAdminCreatePolicy, policy.ActionAdminDeletePolicy, policy.ActionAdminGetPolicy,
		policy.ActionAdminListUserPolicies, policy.ActionAdminAttachUserOrGroupPolicy, policy.ActionAdminSimulatePolicy,
		policy.ActionAdminAddUserToGroup, policy.ActionAdminRemoveUserFromGroup, policy.ActionAdminGetGroup,
		policy.ActionAdminListGroups, policy.ActionAdminEnableGroup, policy.ActionAdminDisableGroup,
		policy.ActionAdminCreateAccessKey, policy.ActionAdminUpdateAccessKey, policy.ActionAdminRemoveServiceAccount,
		policy.ActionAdminListAccessKeys, policy.ActionAdminManageShares,
		policy.ActionAdminExportCluster, policy.ActionAdminImportCluster,
		policy.ActionAdminListLocks, policy.ActionAdminForceUnlock,
		policy.ActionAdminDecommission, policy.ActionAdminRebalance,
		policy.ActionAdminSiteReplicationAdd, policy.ActionAdminSiteReplicationInfo,
		policy.ActionAdminSiteReplicationRemove, policy.ActionAdminSiteReplicationOperation,
		policy.ActionAdminListTier, policy.ActionAdminSetTier,
		policy.ActionAdminKMSKeyStatus, policy.ActionAdminKMSCreateKey,
		policy.ActionAdminManageDistributions, policy.ActionAdminManageCertificates, policy.ActionAdminSignDistributionURL,
		policy.ActionAdminManageCache, policy.ActionAdminInvalidateCache,
		// The metrics of the console (GET /metrics/query) need one of
		// admin:Prometheus or admin:ServerInfo.
		policy.ActionAdminPrometheus,
		policy.ActionAdminProfiling, policy.ActionAdminSpeedtest,
		policy.ActionListAllMyBuckets, policy.ActionCreateBucket,
	)...)
	out = append(out, scoped(scopeBucket,
		policy.ActionAdminManageAccessRules,
		policy.ActionAdminGetBucketQuota, policy.ActionAdminSetBucketQuota,
		policy.ActionAdminGetBucketMedia, policy.ActionAdminSetBucketMedia,
		policy.ActionDeleteBucket, policy.ActionForceDeleteBucket, policy.ActionListBucket,
		policy.ActionListBucketVersions, policy.ActionListMultipart,
		policy.ActionGetBucketPolicy, policy.ActionPutBucketPolicy, policy.ActionDeleteBucketPolicy,
		policy.ActionGetBucketVersioning, policy.ActionPutBucketVersioning,
		policy.ActionGetBucketTagging, policy.ActionPutBucketTagging,
		policy.ActionGetEncryption, policy.ActionPutEncryption,
		policy.ActionGetBucketObjectLock, policy.ActionPutBucketObjectLock,
		policy.ActionGetLifecycle, policy.ActionPutLifecycle, policy.ActionAdminApplyLifecycle,
		policy.ActionGetBucketNotification, policy.ActionPutBucketNotification,
		policy.ActionGetBucketCORS, policy.ActionPutBucketCORS,
		policy.ActionGetReplication, policy.ActionPutReplication, policy.ActionResetReplication,
		policy.ActionAdminSetBucketTarget, policy.ActionAdminGetBucketTarget,
	)...)
	out = append(out, scoped(scopeObject,
		policy.ActionGetObject, policy.ActionGetObjectVersion, policy.ActionPutObject,
		policy.ActionDeleteObject, policy.ActionDeleteObjectVersion,
		policy.ActionGetObjectTagging, policy.ActionPutObjectTagging,
		policy.ActionGetObjectVersionTagging, policy.ActionPutObjectVersionTagging,
		policy.ActionAbortMultipart, policy.ActionListMultipartParts,
		policy.ActionGetObjectRetention, policy.ActionPutObjectRetention,
		policy.ActionGetObjectLegalHold, policy.ActionPutObjectLegalHold,
		policy.ActionBypassGovernanceRetention,
		policy.ActionGetObjectAttributes, policy.ActionGetObjectVersionAttributes,
		policy.ActionGetMediaStream,
	)...)
	return out
}

// CatalogActions returns the actions evaluated by
// Authorizer.Permissions, in catalog order. The transport checks that
// every action an Admin API route declares belongs to it, so that the
// permission-aware console knows about every action it may be denied.
func CatalogActions() []policy.Action {
	entries := catalog()
	out := make([]policy.Action, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.action)
	}
	return out
}
