# Access rules

AWS-format policies know only the `*` and `?` wildcards. **Access rules** are an additional, per-bucket
layer, simpler to write and with **prefix**, **glob** and **regex (RE2)** patterns. They are evaluated by
the same engine as the IAM policies and the bucket policy:

1. an applicable `deny` in any source (IAM policies, bucket policy, rules) → **denied**;
2. otherwise an `allow` from at least one source → allowed;
3. otherwise denied.

Rules never override Object Lock or the restrictions of the inline policies of access keys. They are
managed from the **Access rules** tab of the bucket in the console, with `nexoctl access` or from the Admin
API (`/api/v1/buckets/{bucket}/access-rules`), and take effect immediately on every node.

## Anatomy of a rule

| Field | Values |
|-------|--------|
| `id` | unique name within the bucket |
| `priority` | evaluation order, lowest first (default 100) |
| `effect` | `allow` or `deny` |
| `principals` | `*` (everyone, anonymous users included), `user:NAME`, `group:NAME`, `accesskey:KEY`, `distribution:ID` |
| `actions` | `read` (GET/HEAD, video streaming included), `stream` (the videos play as HLS, the originals cannot be downloaded: `nexo:GetMediaStream`), `list`, `write` (PUT/POST/multipart), `delete`, `tag`, `all`, or actions (`s3:GetObject`, `nexo:GetMediaStream`) |
| `match` | `prefix`, `glob` or `regex` on the key (optional: without it, the whole bucket) |
| conditions | client IP or CIDR (`--source-ip`, `--not-source-ip`), `--referer`, `--host`, `--secure-transport`, time window (`--from`, `--to`), `--method`, `--header`, named groups of the regex (`--capture`) |

### Patterns

| Type | Syntax | Example |
|------|--------|---------|
| `prefix` | literal prefix | `public/` |
| `glob` | `*` (one segment), `**` (several segments), `?`, `[abc]`, `{a,b}` | `docs/**/*.pdf`, `{img,video}/*` |
| `regex` | RE2 (no backreferences: linear time, no ReDoS) | `^users/[a-z0-9_-]+/avatar\.png$` |

- Keys are matched **decoded** and normalized (Unicode NFC, `--key-normalization`): `%2e%2e/`,
  double encodings or Unicode variants do not bypass the rules.
- A regex is validated when saved (error with position), 1024 characters at most; at most 200
  rules per bucket (`--access-rules-max`).
- `--ignore-case` makes the match case-insensitive.

## Examples

### Public images, private folders always denied

```console
$ nexoctl access rule add local/media --id img-pub --allow --principal '*' --action read \
    --regex '^img/[0-9]{4}/.*\.(jpe?g|png)$' --ignore-case
$ nexoctl access rule add local/media --id no-private --deny --principal '*' --action all \
    --glob '**/private/**' --priority 5
$ nexoctl access rule list local/media
ID                   PRIORITY  EFFECT PRINCIPALS           ACTIONS          MATCH
no-private                  5  deny   *                    all              glob **/private/**
img-pub                   100  allow  *                    read             regex(i) ^img/[0-9]{4}/.*\.(jpe?g|png)$
```

| Anonymous request | Outcome |
|-------------------|---------|
| `GET /media/img/2026/a.jpg` | 200 |
| `GET /media/img/2026/b.PNG` | 200 (`--ignore-case`) |
| `GET /media/img/old/c.jpg` | 403 (the year does not have 4 digits) |
| `GET /media/docs/private/x.pdf` | 403 (deny) |

### Home directories with a regex

Every member of the `employees` group accesses only `home/<their own name>/`: the named group `user` of the
regex must equal `${aws:username}`.

```bash
nexoctl access rule add local/home --id home --allow --principal 'group:employees' --action all \
  --regex '^(?P<user>[^/]+)/' --capture 'user=${aws:username}'
```

### Access from a corporate network, HTTPS only

```bash
nexoctl access rule add local/report --id lan --allow --principal 'group:analysts' --action read \
  --source-ip 10.0.0.0/8 --source-ip 2001:db8::/32 --secure-transport true
```

The client IP is the real one derived from the trusted proxies (`--trusted-proxies`); an `X-Forwarded-For`
header from an untrusted proxy is ignored.

### Traffic from the proxy only (origin secret)

```bash
# on the server: NEXO_SECRET_PROXY_TOKEN=<random value>
nexoctl access rule add local/cdn --id via-proxy --allow --principal '*' --action read \
  --header 'X-Origin-Secret: ${secret:proxy-token}'
```

The `${secret:proxy-token}` reference is resolved on every node from `NEXO_SECRET_PROXY_TOKEN` (or `_FILE`):
the secret never ends up in the configuration. A reference that cannot be resolved grants nothing.

### Time window

```bash
nexoctl access rule add local/press --id embargo --allow --principal '*' --action read \
  --prefix releases/2026-11/ --from 2026-11-01T09:00:00Z
```

## Checking before applying

`nexoctl access test` evaluates requests without executing them, with every source (IAM, bucket policy,
rules):

```console
$ nexoctl access test local/media/img/2026/a.jpg local/media/img/old/c.jpg --principal anonymous --ip 1.2.3.4
img/2026/a.jpg: allowed by access rule `img-pub` (anonymous, s3:GetObject)
  rules: allow (matched: img-pub)
img/old/c.jpg: denied (no statement allows it) (anonymous, s3:GetObject)
  rules: none
```

`-v` shows every rule with the reason why it does not apply; `--host`, `--header`, `--referer`, `--secure`,
`--distribution` simulate the rest of the request. The console has the same tester, with the sample keys of
the bucket highlighted.

## Order, edit, export

```bash
nexoctl access rule move local/media --id img-pub --priority 10
nexoctl access rule info local/media --id img-pub
nexoctl access rule add local/media --id img-pub --replace …        # replaces the rule
nexoctl access rule remove local/media --id img-pub
nexoctl access export local/media rules.yaml                        # YAML for GitOps
nexoctl access import local/media-copy rules.yaml
```

Every change replaces the rules of the bucket atomically and fails if they changed in the meantime.

YAML format (the same as the export):

```yaml
bucket: media
rules:
  - id: img-pub
    priority: 100
    effect: allow
    principals:
      - '*'
    actions:
      - read
    match:
      type: regex
      pattern: ^img/[0-9]{4}/.*\.(jpe?g|png)$
      caseInsensitive: true
  - id: no-private
    priority: 5
    effect: deny
    principals:
      - '*'
    actions:
      - all
    match:
      type: glob
      pattern: '**/private/**'
```

## Public presets and block public access

| Preset (`nexoctl access public set …`) | Effect |
|----------------------------------------|--------|
| `private` | no anonymous rule |
| `public-read` | anonymous read of the whole bucket |
| `public-read-list` | read and list |
| `public-prefix` | read of a prefix, glob or regex (`--prefix`, `--glob`, `--regex`) |
| `public-write` | anonymous write (requires `--confirm`) |

Block public access (per bucket or cluster) rejects or ignores the rules with principal `*`: see
[Sharing and anonymous access](sharing.md#public-access-rules-and-block-public-access).
