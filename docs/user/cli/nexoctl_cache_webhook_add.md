# nexoctl cache webhook add

Add a purge webhook to a distribution

## Synopsis

Add a webhook purging a CDN or a proxy in front of the cluster on the
invalidations of the distribution, manual and automatic (docs/11 §11.5), with
retries and a persistent queue: cloudflare (--zone, --token), fastly (--zone is
the service id, --token), akamai (--url, --token, --client-token,
--client-secret), nginx (proxy\_cache\_purge, --url), varnish (BAN, --url) or
generic (--url and --body templates). Without --domain, the webhook purges the
domains not bound to another webhook. Secrets may be ${secret:webhook-NAME}
(NEXO\_SECRET\_WEBHOOK\_NAME on the server); the endpoint must be a public
address or a network of --cache-webhook-allow.

```
nexoctl cache webhook add ALIAS DIST --type TYPE [--id ID] [--domain HOST]... [flags]
```

## Examples

```
  nexoctl cache webhook add local sito-web --domain static.example.it --type cloudflare --zone 0123abcd --token-file cf.token
  nexoctl cache webhook add local sito-web --type varnish --url http://varnish:6081 --id varnish
```

## Options

```
      --body-file string            body template of a generic webhook (Go text/template)
      --client-secret string        Akamai EdgeGrid client secret (or ${secret:webhook-NAME})
      --client-secret-file string   file holding the Akamai client secret
      --client-token string         Akamai EdgeGrid client token
      --cp-code string              Akamai CP code (purge of a whole domain)
      --domain stringArray          domain whose invalidations call the webhook (repeatable; default every domain)
      --header stringArray          header 'Name: value' added to every call (repeatable)
  -h, --help                        help for add
      --id string                   id of the webhook (default its type)
      --method string               HTTP method (nginx: PURGE, varnish: BAN, generic: POST)
      --network string              Akamai network: production (default) or staging
      --scheme string               scheme of the purged URLs (default https)
      --token string                API token (or ${secret:webhook-NAME})
      --token-file string           file holding the API token
      --type string                 cloudflare, fastly, akamai, nginx, varnish or generic
      --url string                  API base, address of the proxy, or URL template (generic)
      --zone string                 Cloudflare zone id, Fastly service id
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

* [nexoctl cache webhook](nexoctl_cache_webhook.md)	 - Manage the purge webhooks of a distribution

