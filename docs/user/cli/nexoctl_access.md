## nexoctl access

Manage the access rules of the buckets (prefix, glob, regex)

### Synopsis

Manage the access rules of the buckets of a Nexo server (Admin API,
admin:ManageAccessRules): rules with a priority, an effect (allow or deny),
principals, actions and a pattern of the keys (a prefix, a glob with \*, \*\*, ?,
[abc] and {a,b}, or an RE2 regular expression), evaluated by the policy engine
next to the IAM policies and the bucket policy: a deny in any source denies.

Principals: '\*' (everyone, anonymous included), user:NAME, group:NAME,
accesskey:KEY, distribution:ID. Actions: read, list, write, delete, tag, all,
or S3 actions (s3:GetObject). Every change replaces the rules of the bucket
atomically and fails if they changed meanwhile.

```
nexoctl access [flags]
```

### Options

```
  -h, --help   help for access
```

### Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

### SEE ALSO

* [nexoctl](nexoctl.md)	 - Nexo command-line client
* [nexoctl access block-public](nexoctl_access_block-public.md)	 - Block the public access of the cluster or of a bucket
* [nexoctl access export](nexoctl_access_export.md)	 - Export the access rules of a bucket as YAML
* [nexoctl access import](nexoctl_access_import.md)	 - Replace the access rules of a bucket with those of a YAML file
* [nexoctl access public](nexoctl_access_public.md)	 - Apply and show the public access presets of a bucket
* [nexoctl access rule](nexoctl_access_rule.md)	 - Add, list, show, remove and reorder the access rules of a bucket
* [nexoctl access test](nexoctl_access_test.md)	 - Test the access rules on keys (who may do what)

