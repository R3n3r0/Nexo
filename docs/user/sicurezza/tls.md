# TLS e certificati

Senza configurazione Nexo serve HTTP in chiaro. Con una directory di certificati (`--certs-dir`,
`NEXO_CERTS_DIR`) **tutti** i listener (S3 sulla 9000, console e Admin API sulla 9001, listener web delle
distribuzioni) passano a TLS, e i nodi di un cluster usano TLS anche tra loro (endpoint `https://`).

## La directory dei certificati

```text
/etc/nexo/certs/
├── public.crt            # certificato di default: foglia + intermedi (PEM)
├── private.key           # chiave privata (PKCS #8, PKCS #1 o SEC 1)
├── s3.example.com/       # certificati aggiuntivi, scelti per SNI
│   ├── public.crt
│   └── private.key
└── CAs/                  # CA fidate: peer del cluster, client mTLS
    └── ca.crt
```

- `public.crt` + `private.key` sono obbligatori; file di al massimo 1 MiB; una chiave leggibile da altri
  utenti produce un avviso nei log (usa `chmod 600`).
- Le **sottocartelle** contengono certificati scelti per **SNI** in base ai nomi DNS della foglia (prima il
  nome esatto, poi la wildcard di un livello; a parità vince quello valido che scade per ultimo). Le cartelle
  nascoste (per esempio `..data` dei Secret Kubernetes) e quelle senza coppia sono ignorate.
- `CAs/` contiene le CA fidate per verificare i peer del cluster: come server (in aggiunta alle radici di
  sistema e alla catena di `public.crt`) e come client mTLS (solo `CAs/` e il certificato di default stesso).

Il certificato deve coprire i nomi con cui i client raggiungono Nexo (nome pubblico, load balancer) e, in
cluster, i nomi degli endpoint dei nodi (`nexo1.lan` … `nexo4.lan`).

## Abilitare TLS

```bash
sudo install -d -o root -g nexo -m 0750 /etc/nexo/certs
sudo install -o root -g nexo -m 0640 fullchain.pem /etc/nexo/certs/public.crt
sudo install -o root -g nexo -m 0640 privkey.pem   /etc/nexo/certs/private.key
# /etc/default/nexo
NEXO_CERTS_DIR=/etc/nexo/certs
```

Poi i client usano `https://`:

```bash
curl --cacert /etc/nexo/certs/CAs/ca.crt https://nexo.lan:9000/nexo/health/live
nexoctl alias set local https://nexo.lan:9000 admin supersegreta
```

`nexoctl` (come ogni programma Go) si fida delle CA di sistema: per una CA privata installala nel trust
store del sistema oppure indica il file con `SSL_CERT_FILE` su Linux:

```bash
SSL_CERT_FILE=/etc/nexo/certs/CAs/ca.crt nexoctl alias set local https://nexo.lan:9000 admin supersegreta
```

Per un ambiente di sviluppo `deploy/docker/gen-dev-certs.sh` crea una CA e un certificato per i nodi del
cluster Compose (`compose.cluster-tls.yml`).

## Politica TLS

- TLS 1.2 minimo (`--tls-min-version 1.3` / `NEXO_TLS_MIN_VERSION=1.3` per richiedere TLS 1.3); TLS 1.3 è
  negoziato con ogni client che lo supporta.
- In TLS 1.2 solo suite ECDHE con AEAD (AES-GCM, ChaCha20-Poly1305); ALPN `h2` e `http/1.1`.
- Le chiavi SSE-C sono accettate solo su HTTPS (vedi [KMS e SSE](kms-sse.md)).

## Rinnovo senza riavvio

La directory è osservata: quando i file cambiano (anche attraverso link simbolici, come `live/` e
`archive/` di certbot) i certificati vengono ricaricati in pochi secondi, senza interrompere le connessioni
aperte, che continuano con il certificato con cui sono nate. Nei log compare `tls: certificates reloaded`.

- `systemctl reload nexo` (o `kill -HUP <pid>`) forza la ricarica immediata, anche dei certificati delle
  distribuzioni.
- Una ricarica che fallisce (chiave che non corrisponde, file scritto a metà) mantiene i certificati
  precedenti e viene registrata nei log e in `nexo_tls_server_certificate_reloads_total{result}`.
- La scadenza è esposta in `nexo_tls_server_certificate_expiry_seconds{certificate}`; i log avvisano a 21
  giorni dalla scadenza.
- Scrivi i file nuovi accanto e rinominali (`mv`) al posto dei vecchi, per evitare letture a metà.

Esempio con certbot (hook di rinnovo):

```bash
#!/bin/sh
# /etc/letsencrypt/renewal-hooks/deploy/nexo.sh
install -o root -g nexo -m 0640 "$RENEWED_LINEAGE/fullchain.pem" /etc/nexo/certs/public.crt.new
install -o root -g nexo -m 0640 "$RENEWED_LINEAGE/privkey.pem"   /etc/nexo/certs/private.key.new
mv /etc/nexo/certs/private.key.new /etc/nexo/certs/private.key
mv /etc/nexo/certs/public.crt.new  /etc/nexo/certs/public.crt
```

## TLS tra i nodi e mTLS

In un cluster con `NEXO_CERTS_DIR` gli endpoint diventano `https://…`; ogni nodo verifica il certificato
dei peer con le CA di `CAs/`, le radici di sistema e `public.crt`.

Con `--tls-internode-verify` (`NEXO_TLS_INTERNODE_VERIFY=true`) il TLS tra i nodi è **mutuo**: ogni nodo
presenta il proprio certificato come client (che deve ammettere l'uso `clientAuth`) e le chiamate tra nodi
senza un certificato client verificato vengono rifiutate. Il certificato client è verificato solo con le CA di
`CAs/` (o è il certificato condiviso da tutti i nodi), mai con gli intermedi della catena di `public.crt`, e deve
nominare (SAN DNS o IP) uno degli host degli endpoint: se i certificati dei nodi sono emessi da una CA privata,
mettila in `CAs/`. Le richieste S3 sulla stessa porta non devono presentare certificati.

```bash
NEXO_CERTS_DIR=/etc/nexo/certs NEXO_TLS_INTERNODE_VERIFY=true \
  nexo server "https://nexo{1...4}.lan:9000/mnt/disk{1...4}"
```

Il rinnovo di un certificato firmato dalla stessa CA avviene a caldo. Sostituire la **CA** (o un certificato
autofirmato usato come radice) richiede invece un riavvio a rotazione dei nodi, perché la fiducia verso i
peer viene fissata all'avvio.

## Dietro un proxy che termina TLS

Se TLS termina su nginx, HAProxy, Traefik o un load balancer:

- inoltra l'header `Host` invariato (la firma SigV4 lo include) e lo schema con `X-Forwarded-Proto`;
- elenca il proxy in `--trusted-proxies` (`NEXO_TRUSTED_PROXIES`), così `aws:SecureTransport` e l'IP del
  client sono corretti;
- imposta `NEXO_SERVER_URL=https://s3.example.com` e `NEXO_BROWSER_REDIRECT_URL` per i link generati;
- non pubblicare mai `/nexo/rpc/` sul dominio pubblico.

Configurazioni pronte per nginx, Caddy, Traefik, HAProxy e Cloudflare sono in
[`deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy).

## Certificati dei domini delle distribuzioni

I domini personalizzati delle [distribuzioni](../distribuzioni/README.md) hanno certificati propri,
caricati (`nexoctl dist cert set … --cert … --key …`) o ottenuti automaticamente via ACME
(`nexoctl dist cert set … --auto`), salvati cifrati e distribuiti a tutti i nodi; in cluster il rinnovo è
eseguito da un solo nodo. Sono presentati per SNI solo per gli alias delle distribuzioni abilitate che li
usano. Vedi il [tutorial](../distribuzioni/tutorial-sito-statico.md).
