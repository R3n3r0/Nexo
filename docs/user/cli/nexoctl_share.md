# nexoctl share

Share objects with links that need no credentials

## Synopsis

Share objects with download and upload links that need no credentials.

By default the links are managed: the Nexo server signs each link with a
dedicated service account, lists it ("share list") and revokes it on demand
("share revoke"), with immediate effect. --raw signs the URL locally with
the credentials of the alias instead (any S3 endpoint): a raw URL cannot be
revoked one by one, only by removing the access key or the permissions of its
user. POST policies for HTML forms (share upload --post) are always signed
locally. Every link expires after --expire (at most 7 days).

```
nexoctl share [flags]
```

## Options

```
  -h, --help   help for share
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client
* [nexoctl share download](nexoctl_share_download.md)	 - Generate download links (managed by default, --raw for presigned GET)
* [nexoctl share list](nexoctl_share_list.md)	 - List the active managed share links
* [nexoctl share revoke](nexoctl_share_revoke.md)	 - Revoke managed share links (immediately)
* [nexoctl share upload](nexoctl_share_upload.md)	 - Generate upload links (managed PUT by default, --raw, or a --post policy)

