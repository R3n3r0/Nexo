# nexoctl access rule add

Add an access rule to a bucket

## Synopsis

Add an access rule to a bucket. Without --prefix, --glob or --regex the rule covers
the whole bucket. The conditions all must hold: --source-ip/--not-source-ip
(CIDRs, the client behind the trusted proxies), --referer (patterns with \* and
?), --host (the host of the request, \*.example.com for the subdomains),
--secure-transport, --from/--to (RFC 3339), --method, --header 'Name: value'
(the value may be ${secret:NAME}) and --capture NAME=VALUE (a named group of
the regex, e.g. user='${aws:username}').

```
nexoctl access rule add ALIAS/BUCKET --id ID (--allow|--deny) --principal P --action A [--prefix|--glob|--regex PATTERN] [flags]
```

## Examples

```
  nexoctl access rule add local/media --id img-pub --allow --principal '*' --action read \
      --regex '^img/[0-9]{4}/.*\.(jpe?g|png)$' --ignore-case
  nexoctl access rule add local/media --id no-private --deny --principal '*' --action all --glob '**/private/**' --priority 5
  nexoctl access rule add local/home --id home --allow --principal 'group:staff' --action all \
      --regex '^home/(?P<user>[^/]+)/' --capture 'user=${aws:username}'
```

## Options

```
      --action stringArray          action: read, stream, list, write, delete, tag, all or an action (repeatable)
      --allow                       allow the actions
      --capture stringArray         condition: the named group NAME of the regex equals VALUE (repeatable)
      --deny                        deny the actions (a deny in any source denies)
      --description string          free text
      --from string                 condition: not before this time (RFC 3339)
      --glob string                 keys of the rule: a glob (*: one segment, **: any number, ?, [abc], {a,b})
      --header stringArray          condition: the request header 'Name: value' (repeatable)
  -h, --help                        help for add
      --host stringArray            condition: the request is addressed to this host (repeatable)
      --id string                   id of the rule (letters, digits, '.', '_', '-')
      --ignore-case                 match the pattern case-insensitively
      --method stringArray          condition: the HTTP method (repeatable)
      --not-source-ip stringArray   condition: the client is not in this CIDR (repeatable)
      --prefix string               keys of the rule: a literal prefix
      --principal stringArray       principal: '*', user:NAME, group:NAME, accesskey:KEY, distribution:ID (repeatable)
      --priority int                order of evaluation, lower first (default 100)
      --referer stringArray         condition: the Referer matches this pattern (repeatable)
      --regex string                keys of the rule: an RE2 regular expression
      --replace                     replace the rule with the same id
      --secure-transport string     condition: the request uses HTTPS (true) or not (false)
      --source-ip stringArray       condition: the client is in this CIDR (repeatable)
      --to string                   condition: not after this time (RFC 3339)
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

* [nexoctl access rule](nexoctl_access_rule.md)	 - Add, list, show, remove and reorder the access rules of a bucket

