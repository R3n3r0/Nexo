# Reverse proxies in front of Nexo

Complete examples exposing, with TLS terminated at the edge:

- the **S3 API** (`s3.example.com`, optionally `*.s3.example.com` for virtual-host style buckets);
- the **console** and the Admin API (`console.example.com`);
- the **distributions** (docs/10 §10.2): the custom domains of the website and s3 distributions
  (`www.example.com`, `example.com`, `*.preview.example.com`), routed by Nexo on the `Host` header,
  with the **origin secret** of the proxy.

| File | Proxy | Client address |
|------|-------|----------------|
| `nginx.conf` | nginx ≥ 1.25.1 site file (`/etc/nginx/conf.d/nexo.conf`) | `X-Forwarded-For` |
| `Caddyfile` | Caddy ≥ 2.7 with automatic HTTPS | `X-Forwarded-For` (or PROXY v2, commented) |
| `traefik.yml` + `traefik-dynamic.yml` | Traefik v3 (static + file provider) | `X-Forwarded-For` |
| `haproxy.cfg` | HAProxy ≥ 2.4 | PROXY protocol v2 with the SSL information (`send-proxy-v2-ssl`) |
| `cloudflare.md` | Cloudflare (Transform Rule, SSL Full strict, cache rules, upload limits) | `X-Forwarded-For` |

[`deploy/docker/compose.multi-proxy.yml`](../docker/compose.multi-proxy.yml) runs two of them (nginx
and HAProxy, different domains and secrets) in front of a 4-node cluster; `make test-proxy` checks
those two configurations with the real binaries (`test/proxy`, `TestRealProxies`) and the same
scenarios with in-process proxies in every deployment mode (`TestMultiProxy`).

## Rules every proxy follows

1. **`Host` is forwarded unchanged, port included.** SigV4 signs it: a rewritten `Host` breaks every
   signed request and presigned URL (`SignatureDoesNotMatch`), and Nexo picks the distribution
   from it.
2. **No body size limit, no buffering, long timeouts.** Uploads and downloads stream through
   (`client_max_body_size 0` + `proxy_request_buffering off` + `proxy_buffering off` in nginx,
   `flush_interval -1` in Caddy, `readTimeout: 0` in Traefik, `timeout client/server 1h` in HAProxy).
3. **The client is described once, by a trusted proxy.** Either `X-Forwarded-For/Proto/Host`
   (default, `NEXO_FORWARDED_HEADERS=x-forwarded`), or RFC 7239 `Forwarded`
   (`NEXO_FORWARDED_HEADERS=forwarded`), or the **PROXY protocol** v1/v2 on the listeners named in
   `NEXO_PROXY_PROTOCOL` (`s3`, `console`, `web`). Nexo reads them only from the addresses in
   `NEXO_TRUSTED_PROXIES`, `X-Forwarded-For` from the right (the entries a client forges on the
   left are ignored), and one header family only: the proxies drop the other one (`Forwarded` in
   these examples). The real client address feeds the `sourceIp` conditions of the access rules,
   the address restrictions of the distributions and the `IpAddress` of signed URLs; the scheme
   feeds the HTTPS-only redirects and `aws:SecureTransport`.
4. **Each proxy has its own origin secret** on the distributions (`X-Origin-Secret`, set by the
   proxy, replacing any value a client sends). A request that reaches a node without one of the
   secrets of the distribution is refused with `403`, so the nodes can stay reachable on the
   internal network without exposing the distributions.
5. **The internode RPC (`/nexo/rpc/`) is never exposed**, on any public domain: the S3 domain and the
   domains of the distributions (it shares the S3 port, and Nexo serves it before routing the host).

## Matching Nexo settings

```bash
NEXO_SERVER_URL=https://s3.example.com            # links of the console and the CLI
NEXO_BROWSER_REDIRECT_URL=https://console.example.com
NEXO_TRUSTED_PROXIES=10.0.0.10,10.0.0.11          # the addresses the proxies connect FROM
NEXO_DOMAIN=s3.example.com                        # only for virtual-host style buckets
NEXO_PROXY_PROTOCOL=s3                            # only behind HAProxy (send-proxy-v2*)
NEXO_SECRET_PROXY_A=...                           # ${secret:proxy-a} in the distributions
NEXO_SECRET_PROXY_B=...                           # ${secret:proxy-b}
```

A listener with `NEXO_PROXY_PROTOCOL` still accepts the connections without a PROXY header
(another proxy using `X-Forwarded-*`, the internode RPC, the health checks); a PROXY header sent
by an address outside `NEXO_TRUSTED_PROXIES` is read and ignored.

## Origin secrets with several proxies

Two ways to give each proxy its secret:

- **By reference** (recommended with several proxies, GitOps friendly): the distribution lists
  `values: ["${secret:proxy-a}", "${secret:proxy-b}"]`, and every node has `NEXO_SECRET_PROXY_A`
  and `NEXO_SECRET_PROXY_B` (or their `_FILE` variants) in its environment. The YAML of the
  distribution holds no secret. Rotating one proxy: add a third reference (`${secret:proxy-a-next}`),
  restart the nodes with its value, switch the proxy, then remove the old reference.
- **By value**, managed at runtime: `nexoctl dist origin-secret rotate local site --keep 1`
  generates a new secret (printed once, stored hashed) and keeps the previous one active, so the
  proxies are switched one after the other without downtime; `--drop-previous` on the next
  rotation removes the old one. The values are shared by every proxy of the distribution.

Either way the change applies on every node at once, without restart (the distributions are
propagated by the cluster), and a secret that cannot be resolved matches no request (the
distribution fails closed).

## Health checks

`/nexo/health/live` (process up) and `/nexo/health/ready` (able to serve: `503` while a cluster
node is bootstrapping or stopping) on the S3 and console ports. A stopping node answers `503` to
the requests it receives until the checks take it out of the rotation: the proxies retry the reads
on another node (nginx `proxy_next_upstream ... http_503`, HAProxy `retry-on ... 503` with L7
retries limited to `GET`/`HEAD`/`OPTIONS`), and the S3 SDKs retry the writes.
