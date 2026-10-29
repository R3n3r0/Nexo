/**
 * Vocabulary of the AWS policy language as accepted by the Nexo policy
 * engine (internal/service/policy): the S3 and admin actions, the condition
 * operators and the condition keys. It feeds the editor autocompletion, the
 * visual builder, the simulator and the client-side lint.
 */

/** S3 actions known to the engine (policy.s3Actions). */
export const s3ActionNames = [
  'AbortMultipartUpload',
  'BypassGovernanceRetention',
  'CreateBucket',
  'DeleteBucket',
  'DeleteBucketCORS',
  'DeleteBucketEncryption',
  'DeleteBucketLifecycle',
  'DeleteBucketOwnershipControls',
  'DeleteBucketPolicy',
  'DeleteBucketReplication',
  'DeleteBucketTagging',
  'DeleteBucketWebsite',
  'DeleteObject',
  'DeleteObjectTagging',
  'DeleteObjectVersion',
  'DeleteObjectVersionTagging',
  'ForceDeleteBucket',
  'GetAccelerateConfiguration',
  'GetAnalyticsConfiguration',
  'GetBucketAcl',
  'GetBucketCORS',
  'GetBucketLocation',
  'GetBucketLogging',
  'GetBucketNotification',
  'GetBucketObjectLockConfiguration',
  'GetBucketOwnershipControls',
  'GetBucketPolicy',
  'GetBucketPolicyStatus',
  'GetBucketPublicAccessBlock',
  'GetBucketRequestPayment',
  'GetBucketTagging',
  'GetBucketVersioning',
  'GetBucketWebsite',
  'GetEncryptionConfiguration',
  'GetIntelligentTieringConfiguration',
  'GetInventoryConfiguration',
  'GetLifecycleConfiguration',
  'GetMetricsConfiguration',
  'GetObject',
  'GetObjectAcl',
  'GetObjectAttributes',
  'GetObjectLegalHold',
  'GetObjectRetention',
  'GetObjectTagging',
  'GetObjectTorrent',
  'GetObjectVersion',
  'GetObjectVersionAcl',
  'GetObjectVersionAttributes',
  'GetObjectVersionForReplication',
  'GetObjectVersionTagging',
  'GetObjectVersionTorrent',
  'GetReplicationConfiguration',
  'ListAllMyBuckets',
  'ListBucket',
  'ListBucketMultipartUploads',
  'ListBucketVersions',
  'ListMultipartUploadParts',
  'ListenBucketNotification',
  'ListenNotification',
  'ObjectOwnerOverrideToBucketOwner',
  'PutAccelerateConfiguration',
  'PutAnalyticsConfiguration',
  'PutBucketAcl',
  'PutBucketCORS',
  'PutBucketLogging',
  'PutBucketNotification',
  'PutBucketObjectLockConfiguration',
  'PutBucketOwnershipControls',
  'PutBucketPolicy',
  'PutBucketPublicAccessBlock',
  'PutBucketRequestPayment',
  'PutBucketTagging',
  'PutBucketVersioning',
  'PutBucketWebsite',
  'PutEncryptionConfiguration',
  'PutIntelligentTieringConfiguration',
  'PutInventoryConfiguration',
  'PutLifecycleConfiguration',
  'PutMetricsConfiguration',
  'PutObject',
  'PutObjectAcl',
  'PutObjectLegalHold',
  'PutObjectRetention',
  'PutObjectTagging',
  'PutObjectVersionAcl',
  'PutObjectVersionTagging',
  'PutReplicationConfiguration',
  'ReplicateDelete',
  'ReplicateObject',
  'ReplicateTags',
  'ResetBucketReplicationState',
  'RestoreObject',
] as const;

export const s3Actions: readonly string[] = s3ActionNames.map((name) => `s3:${name}`);

/** Object actions of Nexo (policy.nexoActions): nexo:GetMediaStream streams
 * a video (HLS) without allowing the download of the original;
 * nexo:IssuePlaybackToken issues the playback tokens of a video;
 * nexo:DownloadProtectedMedia downloads the original of a protected video. */
export const nexoActionNames = [
  'GetMediaStream',
  'IssuePlaybackToken',
  'DownloadProtectedMedia',
] as const;

export const nexoActions: readonly string[] = nexoActionNames.map((name) => `nexo:${name}`);

/** Admin API actions (policy.ActionAdmin*). */
export const adminActions: readonly string[] = [
  'admin:ServerInfo',
  'admin:DataUsageInfo',
  'admin:Heal',
  'admin:ServiceRestart',
  'admin:ServiceStop',
  'admin:ServerUpdate',
  'admin:ServerTrace',
  'admin:ConsoleLog',
  'admin:ConfigUpdate',
  'admin:CreateUser',
  'admin:DeleteUser',
  'admin:ListUsers',
  'admin:GetUser',
  'admin:EnableUser',
  'admin:DisableUser',
  'admin:CreatePolicy',
  'admin:DeletePolicy',
  'admin:GetPolicy',
  'admin:ListUserPolicies',
  'admin:AttachUserOrGroupPolicy',
  'admin:SimulatePolicy',
  'admin:AddUserToGroup',
  'admin:RemoveUserFromGroup',
  'admin:GetGroup',
  'admin:ListGroups',
  'admin:EnableGroup',
  'admin:DisableGroup',
  'admin:CreateServiceAccount',
  'admin:UpdateServiceAccount',
  'admin:RemoveServiceAccount',
  'admin:ListServiceAccounts',
  'admin:ManageShares',
  'admin:Decommission',
  'admin:Rebalance',
  'admin:SiteReplicationAdd',
  'admin:SiteReplicationInfo',
  'admin:SiteReplicationRemove',
  'admin:SiteReplicationOperation',
  'admin:ListTier',
  'admin:SetTier',
  'admin:GetBucketQuota',
  'admin:SetBucketQuota',
  'admin:GetBucketMedia',
  'admin:SetBucketMedia',
  'admin:RevokePlaybackTokens',
  'admin:GetBucketTarget',
  'admin:SetBucketTarget',
  'admin:ApplyLifecycle',
  'admin:KMSKeyStatus',
  'admin:KMSCreateKey',
  'admin:Prometheus',
  'admin:ManageAccessRules',
  'admin:ManageDistributions',
  'admin:ManageCertificates',
  'admin:SignDistributionURL',
  'admin:ManageCache',
  'admin:InvalidateCache',
];

/** Every action the editor proposes, wildcards first. */
export const policyActions: readonly string[] = [
  '*',
  's3:*',
  'admin:*',
  ...s3Actions,
  ...nexoActions,
  ...adminActions,
];

/** Services an Action may name (policy.validAction). */
export const actionServices = ['s3', 'nexo', 'admin', 'sts', 'kms'] as const;

/** Base condition operators (policy.operatorType). */
export const conditionOperatorNames = [
  'StringEquals',
  'StringNotEquals',
  'StringEqualsIgnoreCase',
  'StringNotEqualsIgnoreCase',
  'StringLike',
  'StringNotLike',
  'NumericEquals',
  'NumericNotEquals',
  'NumericLessThan',
  'NumericLessThanEquals',
  'NumericGreaterThan',
  'NumericGreaterThanEquals',
  'DateEquals',
  'DateNotEquals',
  'DateLessThan',
  'DateLessThanEquals',
  'DateGreaterThan',
  'DateGreaterThanEquals',
  'Bool',
  'IpAddress',
  'NotIpAddress',
  'ArnEquals',
  'ArnLike',
  'ArnNotEquals',
  'ArnNotLike',
  'Null',
] as const;

/**
 * True for a valid operator: a base operator with an optional IfExists
 * suffix and ForAllValues:/ForAnyValue: qualifier (never on Null).
 */
export function isConditionOperator(op: string): boolean {
  let rest = op;
  let qualified = false;
  for (const qualifier of ['ForAllValues:', 'ForAnyValue:']) {
    if (rest.startsWith(qualifier)) {
      rest = rest.slice(qualifier.length);
      qualified = true;
    }
  }
  let ifExists = false;
  if (rest.endsWith('IfExists')) {
    rest = rest.slice(0, -'IfExists'.length);
    ifExists = true;
  }
  if (!(conditionOperatorNames as readonly string[]).includes(rest)) {
    return false;
  }
  return !(rest === 'Null' && (ifExists || qualified));
}

/** Condition keys supported by the engine (docs/09 §9.3). */
export const conditionKeys: readonly string[] = [
  'aws:CurrentTime',
  'aws:EpochTime',
  'aws:SecureTransport',
  'aws:SourceIp',
  'aws:Referer',
  'aws:UserAgent',
  'aws:username',
  'aws:userid',
  'aws:PrincipalType',
  's3:authType',
  's3:signatureAge',
  's3:prefix',
  's3:delimiter',
  's3:max-keys',
  's3:versionid',
  's3:x-amz-server-side-encryption',
  's3:x-amz-content-sha256',
  's3:ExistingObjectTag/<key>',
  's3:RequestObjectTag/<key>',
  's3:object-lock-mode',
  's3:object-lock-retain-until-date',
  's3:object-lock-legal-hold',
];

/** How a console action is evaluated by GET /me/permissions (admin.catalog). */
export type ActionScope = 'global' | 'bucket' | 'object';

/** The console action catalog, in the order of the Go catalog. */
export const consoleActionScopes: ReadonlyArray<readonly [string, ActionScope]> = [
  ...[
    'admin:ServerInfo',
    'admin:DataUsageInfo',
    'admin:Heal',
    'admin:ServiceRestart',
    'admin:ServiceStop',
    'admin:ServerUpdate',
    'admin:ServerTrace',
    'admin:ConsoleLog',
    'admin:ConfigUpdate',
    'admin:CreateUser',
    'admin:DeleteUser',
    'admin:ListUsers',
    'admin:GetUser',
    'admin:EnableUser',
    'admin:DisableUser',
    'admin:CreatePolicy',
    'admin:DeletePolicy',
    'admin:GetPolicy',
    'admin:ListUserPolicies',
    'admin:AttachUserOrGroupPolicy',
    'admin:SimulatePolicy',
    'admin:AddUserToGroup',
    'admin:RemoveUserFromGroup',
    'admin:GetGroup',
    'admin:ListGroups',
    'admin:EnableGroup',
    'admin:DisableGroup',
    'admin:CreateServiceAccount',
    'admin:UpdateServiceAccount',
    'admin:RemoveServiceAccount',
    'admin:ListServiceAccounts',
    'admin:ManageShares',
    'admin:Decommission',
    'admin:Rebalance',
    'admin:SiteReplicationAdd',
    'admin:SiteReplicationInfo',
    'admin:SiteReplicationRemove',
    'admin:SiteReplicationOperation',
    'admin:ListTier',
    'admin:SetTier',
    'admin:ManageDistributions',
    'admin:ManageCertificates',
    'admin:SignDistributionURL',
    'admin:ManageCache',
    'admin:InvalidateCache',
    's3:ListAllMyBuckets',
    's3:CreateBucket',
  ].map((a) => [a, 'global'] as const),
  ...[
    'admin:ManageAccessRules',
    'admin:GetBucketQuota',
    'admin:SetBucketQuota',
    'admin:GetBucketMedia',
    'admin:SetBucketMedia',
    'admin:RevokePlaybackTokens',
    'admin:GetBucketTarget',
    'admin:SetBucketTarget',
    's3:DeleteBucket',
    's3:ForceDeleteBucket',
    's3:ListBucket',
    's3:ListBucketVersions',
    's3:ListBucketMultipartUploads',
    's3:GetBucketPolicy',
    's3:PutBucketPolicy',
    's3:DeleteBucketPolicy',
    's3:GetBucketVersioning',
    's3:PutBucketVersioning',
    's3:GetBucketTagging',
    's3:PutBucketTagging',
    's3:GetEncryptionConfiguration',
    's3:PutEncryptionConfiguration',
    's3:GetBucketObjectLockConfiguration',
    's3:PutBucketObjectLockConfiguration',
    's3:GetLifecycleConfiguration',
    's3:PutLifecycleConfiguration',
    's3:GetBucketNotification',
    's3:PutBucketNotification',
    's3:GetReplicationConfiguration',
    's3:PutReplicationConfiguration',
    's3:ResetBucketReplicationState',
  ].map((a) => [a, 'bucket'] as const),
  ...[
    's3:GetObject',
    's3:GetObjectVersion',
    's3:PutObject',
    's3:DeleteObject',
    's3:DeleteObjectVersion',
    's3:GetObjectTagging',
    's3:PutObjectTagging',
    's3:GetObjectVersionTagging',
    's3:PutObjectVersionTagging',
    's3:AbortMultipartUpload',
    's3:ListMultipartUploadParts',
    's3:ReplicateObject',
    's3:ReplicateDelete',
    's3:ReplicateTags',
  ].map((a) => [a, 'object'] as const),
];

/** ARN prefix of S3 resources. */
export const s3ArnPrefix = 'arn:aws:s3:::';

/** ARN of a bucket, or of the objects under a prefix of it ("bucket/prefix*"). */
export function s3Arn(bucket: string, prefix?: string): string {
  if (prefix === undefined) {
    return `${s3ArnPrefix}${bucket}`;
  }
  return `${s3ArnPrefix}${bucket}/${prefix}*`;
}
