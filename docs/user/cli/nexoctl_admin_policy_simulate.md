# nexoctl admin policy simulate

Simulate a request: allowed or denied, and by which statement

## Synopsis

Evaluate a request with the policy engine of the server, as the S3 API would
(identity policies, group policies, inline policies of access keys and the
bucket policy), and print the decision with the statement that decided.
The request is made by --user (default: the user of the alias).

```
nexoctl admin policy simulate [flags] ALIAS[/BUCKET[/OBJECT]] --action ACTION
```

## Examples

```
  nexoctl admin policy simulate --user alice --action s3:GetObject local/photos/2026/a.jpg
  nexoctl admin policy simulate --action s3:ListBucket --condition s3:prefix=2026/ local/photos
```

## Options

```
      --action string           the action (s3:GetObject, s3:PutObject, admin:CreateUser, ...)
      --condition stringArray   condition key of the request, key=value (repeatable)
  -h, --help                    help for simulate
      --insecure                the request does not use TLS (aws:SecureTransport=false)
      --secure                  the request uses TLS (aws:SecureTransport=true)
      --source-ip string        aws:SourceIp of the request
      --time string             aws:CurrentTime of the request (RFC 3339)
      --user string             the user making the request (default: the user of the alias)
  -v, --verbose                 list every evaluated statement
```

## Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

## SEE ALSO

* [nexoctl admin policy](nexoctl_admin_policy.md)	 - Manage the IAM policies (AWS policy language)

