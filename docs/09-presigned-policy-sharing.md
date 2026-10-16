# 9. Presigned URLs, POST policy, access policies and sharing

All the features in this document are **P0/P1, mandatory for v1.0** and are available from the
**S3 API, the CLI (`nexoctl`) and the dashboard**, in single-node and cluster mode.

## 9.1 Presigned URLs (SigV4 query string)

### Supported operations
| Operation | Method | Use |
|------------|--------|-----|
| GetObject | GET | Download / share link |
| HeadObject | HEAD | Check existence/metadata |
| PutObject | PUT | Direct upload from a client without credentials |
| DeleteObject | DELETE | Delegated deletion |
| CreateMultipartUpload / UploadPart / CompleteMultipartUpload / AbortMultipartUpload | POST/PUT | Upload of large files directly from the browser or from third-party clients |
| GetObject with `versionId` | GET | Link to a specific version |
| ListObjectsV2 (bucket) | GET | Delegated listing (if the signer's policy allows it) |

### Rules
| Rule | Value |
|--------|--------|
| Parameters | `X-Amz-Algorithm`, `X-Amz-Credential`, `X-Amz-Date`, `X-Amz-Expires`, `X-Amz-SignedHeaders`, `X-Amz-Signature`, `X-Amz-Security-Token` (for temporary credentials) |
| Expiry | 1 s – **7 days** (604800 s); beyond that → `AuthorizationQueryParametersError` |
| Temporary credentials | The link expires at the earlier of `X-Amz-Expires` and the expiry of the STS session / access key |
| Permissions | Evaluated **at the time of use** with the signer's identity: if the user is disabled, the policy removed or the key revoked, the link stops working immediately |
| Signed headers | If signed (`Content-Type`, `x-amz-meta-*`, `x-amz-server-side-encryption`, `Content-MD5`, checksums…) they must match |
| Response overrides | `response-content-type`, `response-content-disposition`, `response-content-language`, `response-expires`, `response-cache-control`, `response-content-encoding` (signed requests only) |
| SSE-C | Supported: the keys travel as signed headers |
| Virtual-host and path-style | Both; configurable public host (`NEXO_SERVER_URL`) |
| Clock skew | `X-Amz-Date` more than 15 min in the future → rejected |
| Bucket policy | Conditions `s3:signatureAge`, `s3:authType` (`REST-QUERY-STRING`), `aws:SourceIp`, `aws:Referer`, `aws:SecureTransport` to restrict the use of presigned URLs |

### Revoking shared links
A SigV4 presigned URL cannot, by its nature, be revoked individually. Nexo therefore provides:
1. **Managed share links** (console and CLI): every share is signed with a **dedicated service account**
   (`share-<id>`, child of the user, inline policy limited to that object/prefix and to the requested actions only,
   expiry equal to the link's). Revoking the link = deleting that service account → immediate effect on the whole
   cluster.
2. A list of the active shares per user/bucket (`nexoctl share list`, "Shares" page in the dashboard), with creator,
   object, permissions, expiry, revocation.
3. Bulk revocation: disabling the user or rotating their keys invalidates all their links. Rotating the secret from
   the console or the CLI (`PUT /users/{u}` with `secretKey`) revokes the user's managed shares (raw presigned URLs
   signed with the old secret stop verifying on their own) and closes their console sessions: every session is bound
   to the secret it was opened with. Session revocations (logout) are persisted in the IAM store, so they survive a
   restart and apply on every node. Deleting the user (`DELETE /users/{u}`, `nexoctl admin user remove`) also removes
   the records of their managed shares: their service accounts disappear with the user, so they are neither listed as
   active nor passed on to a user recreated with the same name.

## 9.2 POST policy (upload from an HTML form)

- Endpoint `POST /{bucket}` with `multipart/form-data`, fields `key` (also `${filename}`), `policy` (base64 JSON),
  `x-amz-algorithm`, `x-amz-credential`, `x-amz-date`, `x-amz-signature`, `success_action_status`,
  `success_action_redirect`, `Content-Type`, `x-amz-meta-*`, tagging, SSE.
- Supported conditions: `eq`, `starts-with`, `content-length-range`, exact match on bucket/key/acl/
  Content-Type/metadata; `expiration` is required.
- The `file` field must be the last one; the size is checked while streaming (no full buffering).
- Generation from the CLI (`nexoctl share upload --post`) and from the dashboard (ready-to-use HTML/curl snippet).

## 9.3 Access policies

| Type | Scope | Management |
|------|--------|----------|
| **IAM policy** (identity-based) | Users, groups, service accounts, STS roles | Admin API, CLI `admin policy`, dashboard "Policies" |
| **Inline policy of a service account / STS** | Restricts the parent (intersection) | CLI `admin accesskey`, dashboard "Access keys" |
| **Bucket policy** (resource-based) | Buckets and prefixes, also for anonymous users (`"Principal": "*"`) | S3 `PutBucketPolicy`, CLI `anonymous`/`policy`, the bucket's "Access" tab in the dashboard |
| **Simplified anonymous access** | `private`, `download`, `upload`, `public` per bucket or **prefix** | CLI `nexoctl anonymous set download local/bkt/public/`, dashboard with per-prefix rules |
| **Object lock / legal hold** | WORM protection independent of permissions | See S-BKT-08 |

Evaluation (as in AWS): an explicit deny in any policy → deny; otherwise allow if granted by an IAM policy **or** by
the bucket policy; for service accounts/STS the result is intersected with the inline policy.

Predefined policies: `readonly`, `readwrite`, `writeonly`, `diagnostics`, `consoleAdmin`.

Policy support:
- `Action`/`NotAction`, `Resource`/`NotResource`, `Principal`/`NotPrincipal` (bucket policy), wildcards `*` and `?`;
- operators `String*`, `Numeric*`, `Date*`, `Bool`, `IpAddress`/`NotIpAddress`, `Null`, `ArnLike`, the `IfExists`
  suffix, `ForAnyValue:`/`ForAllValues:`;
- keys `aws:SourceIp`, `aws:SecureTransport`, `aws:CurrentTime`, `aws:username`, `aws:userid`, `aws:Referer`,
  `aws:UserAgent`, `s3:prefix`, `s3:delimiter`, `s3:max-keys`, `s3:x-amz-server-side-encryption`,
  `s3:ExistingObjectTag/<k>`, `s3:RequestObjectTag/<k>`, `s3:object-lock-*`, `s3:signatureAge`, `s3:authType`;
- variables `${aws:username}` for per-user home directories (e.g. `arn:aws:s3:::home/${aws:username}/*`);
- **policy simulator**: Admin API `POST /api/v1/policies/simulate` (user/action/resource/context → allow/deny and
  which statement decided), exposed by the CLI `admin policy simulate` and by the dashboard.

## 9.4 CLI features

| Command | Description |
|---------|-------------|
| `nexoctl share download [--expire 2h] [--version-id] [--content-disposition attachment] ALIAS/B/OBJ` | Presigned GET URL (also recursive on a prefix → list of URLs) |
| `nexoctl share upload [--expire] [--content-type] ALIAS/B/OBJ` | Presigned PUT URL + example `curl` command |
| `nexoctl share upload --post [--max-size 10MiB] [--prefix uploads/] ALIAS/B` | POST policy fields + example HTML form |
| `nexoctl share list [--user] [ALIAS/B]` / `share revoke ID` / `share revoke --all --user U` | Managed share links |
| `nexoctl presign --method GET|PUT|HEAD|DELETE [--header k:v] ALIAS/B/OBJ` | "Raw" presigned URL (unmanaged, not revocable) |
| `nexoctl anonymous set private|download|upload|public ALIAS/B[/PREFIX]`, `anonymous list`, `anonymous get-json`, `anonymous set-json` | Anonymous access and bucket policy |
| `nexoctl admin policy create|info|list|remove|attach|detach|entities|simulate` | IAM policies |
| `nexoctl admin accesskey create --policy file.json --expiry 30d` | Service account with an inline policy |

## 9.5 Dashboard features

| Code | Feature |
|--------|--------------|
| D-SHR-01 | "Share" dialog on an object/version/prefix: permissions (download, upload), expiry (preset + date), content-disposition, QR code, copy link. A prefix shared for download produces a single managed share and one link per object (`GET /shares/{id}/links`, paginated, copy one or all links), all signed by the share and revoked with it, like `nexoctl share download --recursive` |
| D-SHR-02 | Upload generator: PUT URL, POST policy with conditions (max size, prefix, content-type) and HTML/curl/JS snippets |
| D-SHR-03 | "Shares" page: list of active links (mine / everyone's for admins), filter, single or multiple revocation |
| D-POL-01 | The bucket's "Access" tab: private/public/custom, anonymous rules per prefix, JSON bucket policy editor with validation |
| D-POL-02 | IAM policy editor: Monaco with JSON schema and autocompletion of actions/condition keys, visual builder |
| D-POL-03 | Policy simulator: user + action + resource + context (IP, time, TLS) → outcome and deciding statement |
| D-POL-04 | "Effective permissions" view of a user (direct policies + groups + bucket policies) |

## 9.6 Tests

### Server
| ID | Case | Expected |
|----|------|--------|
| TPS-01 | Presigned GET/PUT/HEAD/DELETE with aws-sdk-go-v2, boto3, aws-cli, executed with curl | 200/204 |
| TPS-02 | Presigned with `X-Amz-Expires` = 0, 604801 | Error |
| TPS-03 | Expired link (fake clock) | 403 `AccessDenied` "Request has expired" |
| TPS-04 | Altered signature, parameter added/removed, different host | 403 `SignatureDoesNotMatch` |
| TPS-05 | Different signed header (Content-Type) in a PUT | 403 |
| TPS-06 | Presigned with STS credentials: the session expires before the link | 403 at STS expiry |
| TPS-07 | User disabled / policy removed / access key deleted after generation | Immediate 403 on all cluster nodes |
| TPS-08 | `response-content-disposition` and other overrides | Correct response headers |
| TPS-09 | Presigned multipart: create, 3 presigned part PUTs, complete | Correct object |
| TPS-10 | Presigned on a `versionId` | Correct version |
| TPS-11 | Bucket policy denying `s3:authType = REST-QUERY-STRING` or `s3:signatureAge > 600000` | 403 for presigned URLs |
| TPS-12 | POST policy: conditions met / violated (size, prefix, content-type, expiry) | 204/201 or 403/400 |
| TPS-13 | POST policy with `success_action_redirect` | 303 with bucket/key/etag in the query |
| TPS-14 | Managed share link → revoke → reuse | 403 within 1 s on all nodes |
| TPS-15 | Policy matrix: every condition operator, NotAction/NotResource, `${aws:username}` variables, explicit deny | Expected outcome (> 150 cases) |
| TPS-16 | Anonymous access per prefix: `download` on `pub/`, private on the rest | Only `pub/` accessible |
| TPS-17 | Service account with an inline policy broader than the parent | The parent prevails (intersection) |
| TPS-18 | Policy simulator agrees with real enforcement (property-based on randomly generated policies) | 100% agreement |
| TPS-19 | Presigned behind a reverse proxy with a public host (see §8.5) | 200 |
| TPS-20 | Fuzzing of the presigned query parameters and of the POST policy document | No panic, no bypass |
| TPS-21 | Unsigned `x-amz-*` headers added to a presigned URL (`x-amz-copy-source`, `x-amz-tagging`, `x-amz-meta-*`, `x-amz-storage-class`) or to a header-signed request | 403 `AccessDenied` "There were headers present in the request which were not signed", no effect |

### CLI
| ID | Case |
|----|------|
| TCL-P-01 | `share download` → `curl` downloads identical content; `--expire 1s` → 403 after 2 s |
| TCL-P-02 | `share upload` → `curl -T` uploads; `--post` → upload via form with curl (fields with `--form-string`, never read as local files even if they start with `@` or `<`; the file with `-F file=@...`) |
| TCL-P-03 | `share list` / `share revoke` / `share revoke --all --user` |
| TCL-P-04 | `anonymous set download` on a prefix → anonymous `curl` OK only on that prefix |
| TCL-P-05 | `admin policy simulate` consistent with a real operation |
| TCL-P-06 | `presign --method DELETE` works |

### Dashboard
| ID | Case |
|----|------|
| TD-P-01 | Share an object with a 1 h expiry → opening it in an anonymous browser context downloads the file |
| TD-P-02 | Revoke from the Shares page → the link returns 403 |
| TD-P-03 | POST policy generator → HTML snippet executed in a test page uploads the file to the correct prefix |
| TD-P-04 | Access tab: set a public prefix → object accessible anonymously; back to private → 403 |
| TD-P-05 | Policy editor: invalid JSON blocked, valid policy saved and applied |
| TD-P-06 | Simulator: allow/deny and deciding statement displayed |
| TD-P-07 | A user without `admin:*` sees only their own shares |
