# Sharing and anonymous access

| Need | Tool | Revocable |
|------|------|-----------|
| Give a file (or a folder) to someone without credentials | managed share link (`nexoctl share download`, console **Share**) | ✅ individually |
| Let someone upload a file | upload share link (`nexoctl share upload`), POST policy for HTML forms | ✅ (managed PUT) |
| S3 presigned URL for any method | `nexoctl presign`, `share … --raw`, S3 SDKs | ❌ (it just expires) |
| Make a bucket or a prefix public | anonymous access (`nexoctl anonymous`) or [access rules](access-rules.md) | ✅ |
| Serve a site or downloads on a domain | [distributions](../distributions/README.md) | ✅ |

## Managed share links

Every managed link is signed by a **dedicated service account**, a child of the user who creates it, with a
policy limited to the object (or the prefix) and to the operation, and with the same expiry as the link.
Revoking the link deletes that service account: the URL stops working immediately, on every node.

```console
$ nexoctl share download --expire 2h local/photos/hello.txt
URL: local/photos/hello.txt
Expire: 2h0m0s
Share: http://localhost:9000/photos/hello.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=share-2h5zvraevd7j%2F…
ID: 2h5zvraevd7j (revoke with: nexoctl share revoke local 2h5zvraevd7j)
```

| Option | Effect |
|--------|--------|
| `--expire 10m` … `7d` | validity (from 1 second to 7 days, default 7 days) |
| `--recursive` | one link for every object under a prefix (a single share, revoked as a whole) |
| `--version-id ID` | a specific version |
| `--content-disposition attachment` | forces the download with the file name |
| `--content-type` | Content-Type of the response |

```bash
nexoctl share list local                     # my active links
nexoctl share list --user alice local/photos  # of another user (admin:ManageShares)
nexoctl share revoke local 2h5zvraevd7j
nexoctl share revoke --all --user alice local
```

Even without an explicit revocation, a link stops working if the user who created it is disabled,
deleted, loses the permission or changes their secret key: permissions are evaluated **at the time of use**.
The console offers the same flow (**Share** on an object or a folder, with a QR code) and the **Shares**
page to see and revoke the links.

## Upload links

```console
$ nexoctl share upload --expire 1h local/inbox/report.pdf
URL: local/inbox/report.pdf
Expire: 1h0m0s
Share: http://localhost:9000/inbox/report.pdf?X-Amz-Algorithm=…
Command: curl -X PUT --upload-file <FILE> 'http://localhost:9000/inbox/report.pdf?X-Amz-Algorithm=…'
ID: ij3hcpbsedr5 (revoke with: nexoctl share revoke local ij3hcpbsedr5)
```

For a web page that uploads files directly to the bucket use a **POST policy**, signed locally with the
alias's credentials, with conditions on key, size and type:

```console
$ nexoctl share upload --post --max-size 10MiB --prefix uploads/ --expire 1h local/inbox
URL: local/inbox
Expire: 1h0m0s
POST: http://localhost:9000/inbox
Fields:
  key: uploads/${filename}
  policy: eyJjb25kaXRpb25zIjpb…
  x-amz-algorithm: AWS4-HMAC-SHA256
  x-amz-credential: admin/20261015/us-east-1/s3/aws4_request
  x-amz-date: 20261015T115822Z
  x-amz-signature: 646cf714…
Command: curl --form-string 'key=uploads/${filename}' … -F file=@<FILE> 'http://localhost:9000/inbox'
HTML:
<form action="http://localhost:9000/inbox" method="post" enctype="multipart/form-data">
  …
  <input type="file" name="file">
  <input type="submit" value="Upload">
</form>
```

The server verifies the signature, the expiry and the conditions (`content-length-range`, `starts-with` on
the key, `Content-Type`) while streaming. A POST policy cannot be revoked: sign it with a dedicated user or
access key and with a short expiry. The console has a generator with HTML, curl and JavaScript snippets.

## "Raw" presigned URLs

```bash
nexoctl presign --expire 10m local/photos/hello.txt                             # GET
nexoctl presign --method PUT --header "Content-Type: image/png" local/img/logo.png
nexoctl presign --method DELETE --expire 10m local/tmp/old.bin
nexoctl share download --raw --expire 10m local/photos/hello.txt
```

They are standard SigV4 URLs, signed with the alias's credentials: they work with any HTTP client,
expire after 7 days at most and **cannot be revoked** except by disabling or changing the credentials that
signed them. Signed headers (`--header`) must be sent identical. Behind a reverse proxy set
`NEXO_SERVER_URL` to the public URL, so that the console and the CLI generate links with the right host, and
have the proxy forward the `Host` header unchanged (the signature includes it).

A bucket policy can limit the use of presigned URLs with the conditions `s3:authType` (`REST-QUERY-STRING`),
`s3:signatureAge`, `aws:SourceIp`, `aws:Referer`.

## Anonymous access per bucket and prefix

`nexoctl anonymous` manages the **bucket policy** with four levels, for the whole bucket or for a prefix:

| Level | Anonymous users can |
|-------|---------------------|
| `private` | nothing (removes the rule) |
| `download` | read the objects and list the prefix |
| `upload` | write objects |
| `public` | read, list and write |

```console
$ nexoctl anonymous set download local/site/public/
Access permission for `local/site/public/` is set to `download`.
$ curl -s -o /dev/null -w '%{http_code}\n' http://localhost:9000/site/public/p.txt
200
$ curl -s -o /dev/null -w '%{http_code}\n' http://localhost:9000/site/private/p.txt
403
$ nexoctl anonymous list local/site
download local/site/public/
```

For a custom policy: `nexoctl anonymous get-json local/site > policy.json`, edit the file and run
`nexoctl anonymous set-json policy.json local/site` (a document without statements removes the policy). The
same settings are in the **Access** tab of the bucket in the console.

`upload` and `public` let anyone write to the bucket: use them only with a quota
(`nexoctl quota set local/bucket --size 10GiB`) and for non-sensitive data.

## Public access rules and block public access

The [access rules](access-rules.md) offer more expressive public presets (glob and regex):

```bash
nexoctl access public set public-read local/media
nexoctl access public set public-prefix local/media --glob 'img/**'
nexoctl access public set private local/media
```

**Block public access** prevents access rules with principal `*` (everyone, anonymous users included) on a
bucket or on the whole cluster:

```console
$ nexoctl access block-public on local/site            # one bucket
$ nexoctl access block-public on local --mode ignore   # the whole cluster
$ nexoctl access block-public status local/site
Public access of `local/site`: blocked (cluster: off, mode reject, bucket: on).
$ nexoctl access public set public-read local/site
nexoctl: error: local/site: rule 1 ("preset-public-read"), principals: public access is blocked (bucket): a rule cannot allow everyone ("*") (public_access_blocked)
```

- `reject` (default): public rules are rejected when saved;
- `ignore`: they are saved but ignored during evaluation;
- it also applies to public `website` distributions, which answer `403` while the block is on;
- it can also be set at startup for the whole cluster with `--block-public-access` (`NEXO_BLOCK_PUBLIC_ACCESS`)
  or at runtime with the `access` subsystem (`nexoctl admin config set local access block_public=on`).

> **Note**: block public access acts on the access rules and on the distributions; an anonymous bucket
> policy set with `nexoctl anonymous` (or with `PutBucketPolicy`) stays in force. To close anonymous
> access always check `nexoctl anonymous list local/bucket` too.

## CORS

Web applications served from another domain that call the S3 API from the browser need CORS:

```bash
cat > cors.xml <<'EOF'
<CORSConfiguration>
  <CORSRule>
    <AllowedOrigin>https://app.example.com</AllowedOrigin>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedMethod>PUT</AllowedMethod>
    <AllowedHeader>*</AllowedHeader>
    <ExposeHeader>ETag</ExposeHeader>
    <MaxAgeSeconds>3600</MaxAgeSeconds>
  </CORSRule>
</CORSConfiguration>
EOF
nexoctl cors set local/photos cors.xml
nexoctl cors get local/photos
```

A bucket **without** a CORS configuration follows the global policy `api.cors_allow_origin` (default `*`):
`nexoctl admin config set local api cors_allow_origin=https://app.example.com` restricts it for the whole
deployment.
