## nexoctl admin heal

Heal the drives of the server (erasure-coded deployments)

### Synopsis

Start a healing job on the whole server (the format of the drives and every
bucket), a bucket or an object, or with --recursive on every object below the
bucket or prefix, follow its progress and print its outcome: the items examined,
repaired and failed, the drives that missed or held corrupt copies.

With --dry-run the objects are checked and nothing is repaired; --deep verifies
the bitrot checksum of every block (it reads all the data); --remove deletes the
dangling objects found (present on too few drives to ever be read again).
With --background the job is started and its id printed: follow it with
"nexoctl admin heal status ALIAS ID". The exit code is 1 when the job failed or
some item could not be healed.

```
nexoctl admin heal [flags] ALIAS[/BUCKET[/PREFIX]]
```

### Examples

```
  nexoctl admin heal --recursive --dry-run local/photos
  nexoctl admin heal -r --deep local
  nexoctl admin heal --background -r local/photos/2026/
  nexoctl admin heal status local
```

### Options

```
      --background          start the job and print its id without waiting
      --deep                verify the bitrot checksum of every block (reads all the data)
      --dry-run             only report what would be healed
  -h, --help                help for heal
      --interval duration   pause between two queries of the job progress (default 1s)
  -r, --recursive           heal every object under the bucket or prefix
      --remove              delete the dangling objects found
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
* [nexoctl admin heal status](nexoctl_admin_heal_status.md)	 - Show a healing job, or list the running and recent ones

