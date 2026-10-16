# TLS and certificates

Without configuration Nexo serves plain HTTP. With a certificates directory (`--certs-dir`,
`NEXO_CERTS_DIR`) **every** listener (S3 on 9000, console and Admin API on 9001, web listeners of the
distributions) switches to TLS, and the nodes of a cluster use TLS between themselves too (`https://`
endpoints).

## The certificates directory

```text
/etc/nexo/certs/
├── public.crt            # default certificate: leaf + intermediates (PEM)
├── private.key           # private key (PKCS #8, PKCS #1 or SEC 1)
├── s3.example.com/       # additional certificates, chosen by SNI
│   ├── public.crt
│   └── private.key
└── CAs/                  # trusted CAs: cluster peers, mTLS clients
    └── ca.crt
```

- `public.crt` + `private.key` are mandatory; files of at most 1 MiB; a key readable by other
  users produces a warning in the logs (use `chmod 600`).
- The **subdirectories** contain certificates chosen by **SNI** according to the DNS names of the leaf
  (first the exact name, then the one-level wildcard; on a tie the valid one that expires last wins).
  Hidden directories (for example the `..data` of Kubernetes Secrets) and those without a pair are ignored.
- `CAs/` contains the CAs trusted to verify the cluster peers: as a server (in addition to the system roots
  and the chain of `public.crt`) and as an mTLS client (only `CAs/` and the default certificate itself).

The certificate must cover the names with which clients reach Nexo (public name, load balancer) and, in a
cluster, the names of the node endpoints (`nexo1.lan` … `nexo4.lan`).

## Enabling TLS

```bash
sudo install -d -o root -g nexo -m 0750 /etc/nexo/certs
sudo install -o root -g nexo -m 0640 fullchain.pem /etc/nexo/certs/public.crt
sudo install -o root -g nexo -m 0640 privkey.pem   /etc/nexo/certs/private.key
# /etc/default/nexo
NEXO_CERTS_DIR=/etc/nexo/certs
```

Then the clients use `https://`:

```bash
curl --cacert /etc/nexo/certs/CAs/ca.crt https://nexo.lan:9000/nexo/health/live
nexoctl alias set local https://nexo.lan:9000 admin supersecret
```

`nexoctl` (like every Go program) trusts the system CAs: for a private CA install it in the system trust
store or point to the file with `SSL_CERT_FILE` on Linux:

```bash
SSL_CERT_FILE=/etc/nexo/certs/CAs/ca.crt nexoctl alias set local https://nexo.lan:9000 admin supersecret
```

For a development environment `deploy/docker/gen-dev-certs.sh` creates a CA and a certificate for the nodes
of the Compose cluster (`compose.cluster-tls.yml`).

## TLS policy

- TLS 1.2 minimum (`--tls-min-version 1.3` / `NEXO_TLS_MIN_VERSION=1.3` to require TLS 1.3); TLS 1.3 is
  negotiated with every client that supports it.
- In TLS 1.2 only ECDHE suites with AEAD (AES-GCM, ChaCha20-Poly1305); ALPN `h2` and `http/1.1`.
- SSE-C keys are accepted only over HTTPS (see [KMS and SSE](kms-sse.md)).

## Renewal without restart

The directory is watched: when the files change (even through symbolic links, like certbot's `live/` and
`archive/`) the certificates are reloaded within a few seconds, without interrupting the open connections,
which keep the certificate they started with. The logs show `tls: certificates reloaded`.

- `systemctl reload nexo` (or `kill -HUP <pid>`) forces an immediate reload, including the certificates of
  the distributions.
- A reload that fails (key that does not match, half-written file) keeps the previous certificates
  and is recorded in the logs and in `nexo_tls_server_certificate_reloads_total{result}`.
- The expiry is exposed in `nexo_tls_server_certificate_expiry_seconds{certificate}`; the logs warn 21
  days before expiry.
- Write the new files next to the old ones and rename them (`mv`) over the old ones, to avoid half reads.

Example with certbot (renewal hook):

```bash
#!/bin/sh
# /etc/letsencrypt/renewal-hooks/deploy/nexo.sh
install -o root -g nexo -m 0640 "$RENEWED_LINEAGE/fullchain.pem" /etc/nexo/certs/public.crt.new
install -o root -g nexo -m 0640 "$RENEWED_LINEAGE/privkey.pem"   /etc/nexo/certs/private.key.new
mv /etc/nexo/certs/private.key.new /etc/nexo/certs/private.key
mv /etc/nexo/certs/public.crt.new  /etc/nexo/certs/public.crt
```

## TLS between nodes and mTLS

In a cluster with `NEXO_CERTS_DIR` the endpoints become `https://…`; every node verifies the certificate of
its peers with the CAs in `CAs/`, the system roots and `public.crt`.

With `--tls-internode-verify` (`NEXO_TLS_INTERNODE_VERIFY=true`) TLS between nodes is **mutual**: every node
presents its own certificate as a client (it must allow the `clientAuth` usage) and calls between nodes
without a verified client certificate are rejected. The client certificate is verified only against the CAs
in `CAs/` (or it is the certificate shared by every node), never against the intermediates of the chain of
`public.crt`, and it must name (DNS or IP SAN) one of the endpoint hosts: if the node certificates are issued
by a private CA, put it in `CAs/`. S3 requests on the same port do not need to present certificates.

```bash
NEXO_CERTS_DIR=/etc/nexo/certs NEXO_TLS_INTERNODE_VERIFY=true \
  nexo server "https://nexo{1...4}.lan:9000/mnt/disk{1...4}"
```

Renewing a certificate signed by the same CA happens at runtime. Replacing the **CA** (or a self-signed
certificate used as root) instead requires a rolling restart of the nodes, because the trust in the
peers is fixed at startup.

## Behind a proxy that terminates TLS

If TLS terminates on nginx, HAProxy, Traefik or a load balancer:

- forward the `Host` header unchanged (the SigV4 signature includes it) and the scheme with `X-Forwarded-Proto`;
- list the proxy in `--trusted-proxies` (`NEXO_TRUSTED_PROXIES`), so that `aws:SecureTransport` and the
  client IP are correct;
- set `NEXO_SERVER_URL=https://s3.example.com` and `NEXO_BROWSER_REDIRECT_URL` for the generated links;
- never publish `/nexo/rpc/` on the public domain.

Ready-made configurations for nginx, Caddy, Traefik, HAProxy and Cloudflare are in
[`deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy).

## Certificates of the distribution domains

The custom domains of the [distributions](../distributions/README.md) have their own certificates,
uploaded (`nexoctl dist cert set … --cert … --key …`) or obtained automatically through ACME
(`nexoctl dist cert set … --auto`), stored encrypted and distributed to every node; in a cluster the renewal
is performed by a single node. They are presented by SNI only for the aliases of the enabled distributions
that use them. See the [tutorial](../distributions/tutorial-static-site.md). The wildcard domains get their
ACME certificate through the DNS-01 challenge and a DNS provider (`--auto --dns-provider NAME`): see
[wildcard domains](../distributions/README.md#wildcard-domains-the-dns-01-challenge); the credentials of the
providers are stored encrypted and never shown again.
