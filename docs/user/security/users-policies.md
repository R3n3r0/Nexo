# Users, policies and credentials

Nexo has a single account with a **root** user (credentials from the environment) and IAM users managed from
the console, `nexoctl admin` or the Admin API. Every request, S3 or console, is evaluated by the same
AWS-format policy engine: **an explicit deny wins**, then an **allow** is needed (from the policies of the
user, of their groups, from the bucket policy or from the [access rules](access-rules.md)), otherwise the
request is denied.

## Root

- `NEXO_ROOT_USER` / `NEXO_ROOT_PASSWORD` (or `_FILE`), the same on every node of a cluster.
- Use it only for administration: for applications create users or access keys with the minimum permissions.
- `nexoctl admin config set local api root_access=off` prevents the use of the root credentials on the S3
  API (the console and the Admin API stay reachable, so it can always be turned back on).
- Changing the root password requires restarting every node with the new value; the root credentials also
  sign the tokens between nodes and encrypt the IAM export.

## Users and groups

```bash
nexoctl admin user add local alice 'a-long-secret-key' --policy readwrite
nexoctl admin user add local bob 'another-long-secret-key' --group employees --disabled
nexoctl admin user list local
nexoctl admin user info local alice
nexoctl admin user disable local alice          # immediate block on every node; revokes sessions and links
nexoctl admin user enable local alice
nexoctl admin user remove local alice           # also removes their access keys and share links

nexoctl admin group add local employees alice bob --policy home-dir    # creates the group if it does not exist
nexoctl admin group members local employees
nexoctl admin group remove local employees bob                        # removes a member
nexoctl admin group disable local employees
```

A user without policies cannot do anything. The policies of a user add up to those of their groups. The
secret key is changed from the console or with the Admin API (`PUT /api/v1/users/{u}`): the change revokes
their console sessions and their managed share links. Disabling a user revokes them too, together with the console
sessions of their access keys, and for good: enabling the user again restores the credentials, not the sessions
nor the links (log in and share again).

## Policies

### Built-in policies

| Policy | Grants |
|--------|--------|
| `readonly` | `s3:Get*` and `s3:List*` on every bucket |
| `readwrite` | every `s3:*` action on every bucket |
| `writeonly` | writing (`s3:PutObject`, multipart) on every bucket, without reading |
| `diagnostics` | server information, trace, logs, Prometheus metrics, profiling, speedtest (no data) |
| `consoleAdmin` | `admin:*` and `s3:*`: full administration |

### Management

```bash
nexoctl admin policy create local photos-ro photos-ro.json        # --update to replace it
nexoctl admin policy validate local photos-ro.json                # errors with line and column
nexoctl admin policy attach local photos-ro --user alice
nexoctl admin policy attach local home-dir --group employees
nexoctl admin policy detach local photos-ro --user alice
nexoctl admin policy entities local photos-ro                     # who it is attached to
nexoctl admin policy info local photos-ro
```

The language is that of AWS: `Action`/`NotAction`, `Resource`/`NotResource`, `*` and `?` wildcards,
conditions (`String*`, `Numeric*`, `Date*`, `Bool`, `IpAddress`/`NotIpAddress`, `Null`, `ArnLike`,
`IfExists` suffix, `ForAnyValue:`/`ForAllValues:`) with keys such as `aws:SourceIp`,
`aws:SecureTransport`, `aws:CurrentTime`, `aws:username`, `aws:Referer`, `s3:prefix`, `s3:delimiter`,
`s3:x-amz-server-side-encryption`, `s3:ExistingObjectTag/<k>`, `s3:RequestObjectTag/<k>`,
`s3:signatureAge`, `s3:authType`, and the `${aws:username}` variable. The administration actions are
`admin:*` (for example `admin:CreateUser`, `admin:ServerInfo`, `admin:Heal`, `admin:ConfigUpdate`).

### Example: read-only access to a bucket

`photos-ro.json`: the user lists and downloads the objects of the `photos` bucket and nothing else.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": ["arn:aws:s3:::photos"]
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": ["arn:aws:s3:::photos/*"]
    }
  ]
}
```

```console
$ nexoctl admin policy create local photos-ro photos-ro.json
$ nexoctl admin user add local alice 'alice-secret-123' --policy photos-ro
$ nexoctl alias set alice http://localhost:9000 alice 'alice-secret-123'
Added alias "alice" for http://localhost:9000 (credentials accepted; listing the buckets is denied).
$ nexoctl cat alice/photos/hello.txt
hi Nexo!
$ nexoctl cp x.txt alice/photos/x.txt
nexoctl: error: alice/photos/x.txt: access denied: Access Denied. (AccessDenied)
```

To make the bucket show up in the list of buckets too, add a statement with `s3:ListAllMyBuckets` on
`arn:aws:s3:::*` (it shows the names of every bucket, not their contents).

### Example: per-user home directory

A single `home` bucket, every user reads and writes only under `home/<their own name>/`. A single policy for
everyone, attached to a group, thanks to `${aws:username}`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": ["arn:aws:s3:::home"],
      "Condition": {"StringLike": {"s3:prefix": ["${aws:username}/*", "${aws:username}"]}}
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
      "Resource": ["arn:aws:s3:::home/${aws:username}/*"]
    }
  ]
}
```

```bash
nexoctl mb local/home
nexoctl admin policy create local home-dir home.json
nexoctl admin group add local employees alice bob --policy home-dir
nexoctl cp report.pdf bob/home/bob/            # ok
nexoctl cp report.pdf bob/home/alice/          # AccessDenied
nexoctl ls bob/home/alice/                     # AccessDenied
```

The console shows these users the `home` bucket with only the accessible content. With the
[access rules](access-rules.md#home-directories-with-a-regex) the same scheme can also be expressed with
a regex and a named group.

### Example: write-only (drop folder)

A scanner or an external system drops files into `inbox` without being able to read them back or list them:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:AbortMultipartUpload"],
      "Resource": ["arn:aws:s3:::inbox/*"]
    }
  ]
}
```

`s3:PutObject` also covers multipart uploads (large files). To restrict it to a prefix use
`arn:aws:s3:::inbox/scanner1/*`; to enforce encryption add a condition on
`s3:x-amz-server-side-encryption`.

### Simulator

Before applying a policy, or to understand an `AccessDenied`:

```console
$ nexoctl admin policy simulate --user bob --action s3:GetObject local/home/alice/x.txt
Decision: deny
User: bob
Action: s3:GetObject
Resource: home/alice/x.txt
Reason: no policy or access rule allows s3:GetObject

$ nexoctl admin policy simulate --user bob --action s3:ListBucket --condition s3:prefix=bob/ local/home
Decision: allow
…
Reason: allowed by group policy "home-dir" (group employees), statement 1
```

Options: `--source-ip`, `--secure`/`--insecure`, `--time`, `--condition key=value`, `-v` to see
every evaluated statement. The console has the same tool (**Policy simulator**) and shows the effective
permissions of every user.

## Access keys (service accounts)

A user can have several access keys, each with an expiry and an **inline policy** that restricts (never
widens) the user's permissions: ideal for applications and CI pipelines.

```console
$ nexoctl admin accesskey create local --user alice --name ci --description "CI build" \
    --expiry 30d --policy only-2026.json
Access Key: FZR6F4G0E0NF208I5QQ5
Secret Key: AWGjCT6eugjPgtYyAMig2FE01tJC6nYAK6HcLjMk
Parent User: alice
Expiration: 2026-11-14 11:57:31 UTC
Policy: inline (restricts the parent user)
The secret key is shown only once: store it now.
```

```bash
nexoctl admin accesskey list local --user alice
nexoctl admin accesskey edit local FZR6F4G0E0NF208I5QQ5 --disable
nexoctl admin accesskey edit local FZR6F4G0E0NF208I5QQ5 --expiry 2027-01-31 --clear-policy
nexoctl admin accesskey remove local FZR6F4G0E0NF208I5QQ5
```

- The secret key is shown **only once**.
- Disabling a key (`--disable`) revokes its console sessions and the managed share links created with it;
  `--enable` does not bring them back.
- Without `--user` the key belongs to the alias's user: every user manages their own keys, root
  those of everyone.
- The effective permissions are the intersection of the inline policy and those of the user: if a policy is
  removed from the user, their keys lose it immediately.

## Temporary credentials (STS AssumeRole)

Nexo implements `AssumeRole` of the STS API on the S3 port: a user (or root) obtains temporary credentials
(access key, secret key and session token) valid from 15 minutes to 12 hours (default 1 hour),
optionally restricted by a **session policy**. The "role" is the identity that signs the request:
`RoleArn` is required by the SDKs but is not used.

With the AWS CLI:

```bash
aws sts assume-role --endpoint-url http://localhost:9000 --region us-east-1 \
  --role-arn arn:aws:iam::000000000000:role/any --role-session-name ci \
  --duration-seconds 3600 \
  --policy '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}]}'
```

Or with `curl` (SigV4 signature for the `sts` service):

```bash
curl -s --aws-sigv4 "aws:amz:us-east-1:sts" --user 'alice:alice-secret-123' \
  -X POST -H 'Content-Type: application/x-www-form-urlencoded' \
  --data 'Action=AssumeRole&Version=2011-06-15&DurationSeconds=900&RoleSessionName=ci' \
  http://localhost:9000/
```

The XML response contains `AccessKeyId`, `SecretAccessKey`, `SessionToken` and `Expiration`. Temporary
credentials cannot call `AssumeRole` themselves. A presigned URL signed with STS credentials expires at the
latest with the session. Identity federation (OIDC, LDAP) is not available yet.

## Best practices

- One user or access key per application, with the minimum permissions and an expiry.
- Groups for roles (readers, editors, administrators of a project) instead of per-user policies.
- `diagnostics` for monitoring (Prometheus, trace), never root.
- Disable (not just delete) a suspicious user: the effect is immediate on the whole cluster, and its
  console sessions and managed links are revoked for good. Change its secret too before enabling it again:
  raw presigned URLs signed with the old secret would work again.
- Periodically review `nexoctl admin accesskey list` and the active shares (`nexoctl share list --user …`).
