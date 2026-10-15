## nexoctl admin policy

Manage the IAM policies (AWS policy language)

### Synopsis

Manage the named IAM policies, attach them to users and groups and simulate their
effect. The built-in policies are readonly, readwrite, writeonly, diagnostics and
consoleAdmin.

```
nexoctl admin policy [flags]
```

### Options

```
  -h, --help   help for policy
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

* [nexoctl admin](nexoctl_admin.md)	 - Administer a Nexo server (Admin API)
* [nexoctl admin policy attach](nexoctl_admin_policy_attach.md)	 - Attach policies to a user or a group
* [nexoctl admin policy create](nexoctl_admin_policy_create.md)	 - Create a policy from a JSON document (FILE, or - for stdin)
* [nexoctl admin policy detach](nexoctl_admin_policy_detach.md)	 - Detach policies from a user or a group
* [nexoctl admin policy entities](nexoctl_admin_policy_entities.md)	 - List the users and groups a policy is attached to
* [nexoctl admin policy info](nexoctl_admin_policy_info.md)	 - Print the document of a policy
* [nexoctl admin policy list](nexoctl_admin_policy_list.md)	 - List the policies
* [nexoctl admin policy remove](nexoctl_admin_policy_remove.md)	 - Remove policies (built-in policies cannot be removed)
* [nexoctl admin policy simulate](nexoctl_admin_policy_simulate.md)	 - Simulate a request: allowed or denied, and by which statement
* [nexoctl admin policy validate](nexoctl_admin_policy_validate.md)	 - Validate a policy document without saving it

