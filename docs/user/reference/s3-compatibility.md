# S3 compatibility

Nexo implements the Amazon S3 API used by the AWS SDKs, the AWS CLI and the most common tools
(rclone, restic, s3cmd, Terraform, Velero, …). This page lists the supported APIs, the known differences and
the results of the compatibility suite.

## Endpoints and authentication

| Aspect | Support |
|--------|---------|
| Addressing | path-style (`https://s3.example.com/bucket/key`) always; virtual-host (`https://bucket.s3.example.com/key`) with `NEXO_DOMAIN=s3.example.com` |
| Region | a single one, `us-east-1` by default (`--region`, `NEXO_REGION`) |
| Signatures | AWS Signature V4: header, query (presigned URLs, up to 7 days), chunked payload (`aws-chunked`, with trailers too), POST policies of HTML forms |
| Unsupported signatures | Signature V2 (deprecated by AWS) |
| Temporary credentials | STS `AssumeRole` (15 minutes – 12 hours) |
| Errors | standard S3 XML, with `x-amz-request-id` and `x-amz-id-2` in every response; unsupported APIs answer `501 NotImplemented` |

## Supported APIs

**Buckets**: `ListBuckets`, `CreateBucket`, `HeadBucket`, `DeleteBucket`, `GetBucketLocation`,
`Get/Put/DeleteBucketPolicy`, `GetBucketPolicyStatus`, `Get/PutBucketVersioning`, `Get/Put/DeleteBucketTagging`,
`Get/Put/DeleteBucketCors`, `Get/Put/DeleteBucketLifecycleConfiguration`, `Get/PutObjectLockConfiguration`,
`Get/Put/DeleteBucketEncryption`, `Get/PutBucketNotificationConfiguration`, `ListObjects`, `ListObjectsV2`,
`ListObjectVersions`, `ListMultipartUploads`, `Get/PutBucketAcl` (`private` only).

**Objects**: `PutObject`, `GetObject` (ranges and conditional requests too), `HeadObject`, `DeleteObject`,
`DeleteObjects`, `CopyObject`, `GetObjectAttributes`, `Get/Put/DeleteObjectTagging`, `Get/PutObjectRetention`,
`Get/PutObjectLegalHold`, `PostObject`, `Get/PutObjectAcl` (`private` only), `SelectObjectContent` (see
[S3 Select](#s3-select)).

**Multipart**: `CreateMultipartUpload`, `UploadPart`, `UploadPartCopy`, `CompleteMultipartUpload`,
`AbortMultipartUpload`, `ListParts`.

**STS**: `AssumeRole`.

**Features**: versioning with delete markers, Object Lock (governance and compliance, default retention,
legal hold), SSE-S3, SSE-KMS and SSE-C encryption, additional checksums (`CRC32`, `CRC32C`, `CRC64NVME`, `SHA1`,
`SHA256`, composite ones for multipart too), tags, lifecycle (expirations, noncurrent versions, delete markers,
incomplete uploads), event notifications, CORS, quotas (a Nexo extension), replication (`?replication`, with the
ARN of a target registered through the Admin API as destination: see [bucket replication](../replication.md)).

### Not supported

They answer `501 NotImplemented`: bucket website (`?website`; the
[distributions](../distributions/README.md) take its place), `PublicAccessBlock` (the Nexo
[block public access](../security/sharing.md#public-access-rules-and-block-public-access) takes its
place), server access logging, accelerate, analytics, inventory, metrics,
intelligent tiering, ownership controls, request payment, `RestoreObject`,
`GetObjectTorrent`. Lifecycle transitions name a remote tier ([Remote tiers](../tiering.md)); the other storage
classes are rejected (`InvalidStorageClass`). Planned after v1.0: full ACLs, `AssumeRoleWithWebIdentity`.

### S3 Select

`SelectObjectContent` runs SQL over CSV and JSON objects (plain, GZIP or BZIP2) and answers with the S3 event
stream (`Records`, `Progress`, `Stats`, `End`), as the SDKs expect. It needs `s3:GetObject`.

- **SQL**: `SELECT <columns or aggregates> FROM S3Object[*][.path] [alias] [WHERE …] [LIMIT n]`; paths
  (`s.name`, `s._1`, `s.a.b[0]`, `"quoted"`), arithmetic, `||`, comparisons, `AND`/`OR`/`NOT`, `LIKE … ESCAPE`,
  `BETWEEN`, `IN`, `IS [NOT] NULL|MISSING`, `CAST(… AS INT|FLOAT|DECIMAL|STRING|BOOL)`, `LOWER`, `UPPER`,
  `CHAR_LENGTH`, `TRIM`, `SUBSTRING`, `COALESCE`, `NULLIF`; aggregates `COUNT`, `SUM`, `AVG`, `MIN`, `MAX`.
- **CSV**: `FileHeaderInfo` `USE`/`IGNORE`/`NONE`, field delimiter and comment character of one character,
  record delimiter of one character or `\r\n`, `"` quotes. A comparison or an arithmetic operation with a
  number reads a CSV field as a number when it holds one; an empty field has no value for `CAST` and the
  aggregates.
- **JSON**: `LINES` and `DOCUMENT` (a top-level array is a list of records).
- **Not supported**: Parquet, `ScanRange`, the date and time functions (`501 NotImplemented` or
  `UnsupportedSyntax`).

## Known differences

| Area | Nexo behavior |
|------|---------------|
| Keys and prefixes | A key cannot also be the prefix of other keys (as in MinIO): with `a/b` present, writing `a/b/c` is rejected with `XNexoParentIsObject` (400); with `a/b/c` present, writing `a/b` is rejected with `XNexoObjectExistsAsDirectory` (400) |
| ACLs | Every bucket behaves like an S3 bucket with Object Ownership `BucketOwnerEnforced` (the AWS default since 2023): ACLs other than `private` and explicit grants are rejected; access is granted with policies, anonymous access and access rules |
| Accounts | A single account: users have the rights of their policies, not those of "owner" of the buckets they create; an existing bucket always answers `BucketAlreadyOwnedByYou` |
| Authorization | A request on an object is authorized before the key is looked up: with only `s3:ListBucket` a missing key gives 403; with `s3:GetObject` it gives 404 even without `s3:ListBucket` |
| Non-ASCII metadata | `x-amz-meta-*` values are returned with the bytes received (UTF-8), without RFC 2047 encoding |
| CORS | A bucket **without** a CORS configuration follows the global policy `api.cors_allow_origin` (default `*`): the preflight answers 200 instead of S3's 403. A bucket with a CORS configuration behaves as in S3 |
| Notifications | Records in the S3 format with `eventSource` `nexo:s3`; the payload adds `EventName` and `Key` at the top level (like MinIO); retention and legal hold changes generate no events, as in S3 |
| Slow downloads | A client that stops reading the body of a GET for more than 20 s (`NEXO_SEND_TIMEOUT`) sees the response aborted (on HTTP/2 the connection that no longer accepts bytes is closed); a client that reads steadily is never aborted, even with many HTTP/2 streams on the same connection, as long as the connection receives at least about 40 KB/s |
| Specific errors | `XNexoQuotaExceeded` and `XNexoStorageFull` (507), `XNexoParentIsObject` and `XNexoObjectExistsAsDirectory` (400); a set without quorum answers `503 SlowDownWrite`/`SlowDownRead` (`XNexoLockTimeout` if it cannot get the lock, `ServiceUnavailable` for an offline drive or node); the SDKs retry `503` responses, `SlowDown` remains for throttled requests |
| Block public access | Acts on the access rules and on the distributions, not on anonymous bucket policies (see [Sharing](../security/sharing.md)) |

Otherwise the responses follow S3 even in the details the SDKs check: for example no
`x-amz-version-id` for the `null` version, `If-Match` on a missing key → 404 `NoSuchKey`, `partNumber` beyond
the parts → 416, different SSE-C key → 403, repeating a successful `CompleteMultipartUpload` → same
outcome, parts in strictly increasing order (`InvalidPartOrder`).

## Ceph s3-tests results

The [`ceph/s3-tests`](https://github.com/ceph/s3-tests) suite runs every night in all three modes
(`make test-s3compat`, [`test/compat`](https://github.com/R3n3r0/Nexo/tree/main/test/compat)). Latest
published measurement (suite commit `5522d1c`, `test_s3.py`, 838 tests, 2026-10-09):

| Mode | Applicable tests passed |
|------|-------------------------|
| Single node, multi drive | 422/422 |
| Cluster | 422/422 |
| Single node, single drive | 421/422 |

- In SNSD `test_atomic_read_4mb` failed in 2 runs out of 4: the suite's client writes the key
  while it is not yet reading the GET of the same key, and the read lock held during the transfer
  blocks the write until `NEXO_SEND_TIMEOUT` expires.
- Main groups: versioning 28/28, Object Lock 39/39, multipart 31/31, tagging 17/17, SSE-C 18/18,
  checksums 11/11.
- 415 tests are excluded, each with its reason in
  [`test/compat/excluded.txt`](https://github.com/R3n3r0/Nexo/blob/main/test/compat/excluded.txt):
  Ceph RGW extensions that AWS itself does not pass (marked `fails_on_aws` by the suite), full ACLs,
  Signature V2, `PublicAccessBlock`, server access logging and the known differences above.

The current status, with any new measurements, is in
[docs/05](https://github.com/R3n3r0/Nexo/blob/main/docs/05-roadmap.md) (S3 compatibility status).

## Trying a client

```bash
aws configure set aws_access_key_id admin
aws configure set aws_secret_access_key supersecret
aws --endpoint-url http://localhost:9000 s3 mb s3://test
aws --endpoint-url http://localhost:9000 s3 cp file.txt s3://test/
aws --endpoint-url http://localhost:9000 s3api list-object-versions --bucket test
```

With rclone:

```ini
[nexo]
type = s3
provider = Minio
endpoint = http://localhost:9000
access_key_id = admin
secret_access_key = supersecret
```

The rclone `Minio` provider uses path-style addressing, which works without configuring `NEXO_DOMAIN`.
