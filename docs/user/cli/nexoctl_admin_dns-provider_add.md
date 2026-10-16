# nexoctl admin dns-provider add

Create a DNS provider

```
nexoctl admin dns-provider add ALIAS NAME --type TYPE [flags]
```

## Examples

```
  nexoctl admin dns-provider add local cf --type cloudflare --api-token-file cf-token.txt
  nexoctl admin dns-provider add local aws --type route53 --hosted-zone-id Z123EXAMPLE
  nexoctl admin dns-provider add local bind --type rfc2136 --nameserver ns1.example.com \
    --tsig-key nexo-acme --tsig-secret-file tsig.key
```

## Options

```
      --access-key-id string            route53: access key id (none: the ambient AWS credentials of the nodes)
      --api-token string                cloudflare: API token (Zone:DNS:Edit)
      --api-token-file string           cloudflare: file holding the API token
      --comment string                  free comment
  -h, --help                            help for add
      --hosted-zone-id string           route53: id of the hosted zone (default: the public zone found by name)
      --nameserver string               rfc2136: primary nameserver accepting the updates (host[:port])
      --polling-interval duration       delay between two checks of the propagation (default 4s)
      --propagation-timeout duration    wait of the records on the nameservers (default 2m)
      --region string                   route53: region of the API (default us-east-1)
      --secret-access-key string        route53: secret access key
      --secret-access-key-file string   route53: file holding the secret access key
      --session-token string            route53: session token of temporary credentials
      --session-token-file string       route53: file holding the session token
      --tsig-algorithm string           rfc2136: TSIG algorithm (default hmac-sha256)
      --tsig-key string                 rfc2136: name of the TSIG key (none: unsigned updates)
      --tsig-secret string              rfc2136: TSIG secret, base64
      --tsig-secret-file string         rfc2136: file holding the TSIG secret
      --ttl int                         TTL of the records in seconds (default 60)
      --type string                     provider type: cloudflare, route53 or rfc2136
      --zone string                     rfc2136: zone of the records (default: from the SOA)
      --zone-id string                  cloudflare: id of the zone (default: found by name)
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

* [nexoctl admin dns-provider](nexoctl_admin_dns-provider.md)	 - Manage the DNS providers of the ACME DNS-01 challenge

