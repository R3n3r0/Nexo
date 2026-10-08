# Reverse proxies in front of Nexo

Examples exposing the S3 API (`s3.example.com`, optionally `*.s3.example.com` for virtual-host style
buckets) and the console (`console.example.com`) with TLS termination at the edge.

| File | Proxy |
|------|-------|
| `nginx.conf` | nginx site file (`/etc/nginx/conf.d/nexo.conf`) |
| `Caddyfile` | Caddy with automatic HTTPS |
| `traefik.yml` + `traefik-dynamic.yml` | Traefik v3 (static + file provider) |

Every example follows the rules from [docs/08 §8.5](../../docs/08-modalita-deploy.md):

1. **`Host` is forwarded unchanged** — SigV4 signatures include it; a rewritten `Host` breaks every
   signed request and every presigned URL (`SignatureDoesNotMatch`).
2. **No body size limit, no request buffering** — uploads stream to Nexo as they arrive.
3. **`X-Forwarded-Proto` / `X-Forwarded-Host`** are set so Nexo can build public URLs; Nexo trusts them
   only from `NEXO_TRUSTED_PROXIES`.
4. **Long timeouts** on the console for SSE/WebSocket streams (live logs, traces) and on S3 for large
   transfers.

Matching Nexo settings (`nexo.env`):

```bash
NEXO_SERVER_URL=https://s3.example.com
NEXO_BROWSER_REDIRECT_URL=https://console.example.com
NEXO_TRUSTED_PROXIES=127.0.0.1          # the proxy's address(es)
NEXO_DOMAIN=s3.example.com              # only for virtual-host style buckets
```

Health checks for load balancers: `/nexo/health/live` (process up) and `/nexo/health/ready` (able to
serve traffic), both on the S3 and the console port.
