# Regole di accesso

Le policy in formato AWS conoscono solo i wildcard `*` e `?`. Le **regole di accesso** sono un livello in
più, per bucket, più semplice da scrivere e con pattern **prefix**, **glob** e **regex (RE2)**. Sono
valutate dallo stesso motore delle policy IAM e della bucket policy:

1. un `deny` applicabile in qualsiasi sorgente (policy IAM, bucket policy, regole) → **negato**;
2. altrimenti un `allow` da almeno una sorgente → consentito;
3. altrimenti negato.

Le regole non scavalcano mai Object Lock né le restrizioni delle policy inline delle access key. Si
gestiscono dalla scheda **Regole di accesso** del bucket in console, con `nexoctl access` o dall'Admin API
(`/api/v1/buckets/{bucket}/access-rules`), e valgono subito su tutti i nodi.

## Anatomia di una regola

| Campo | Valori |
|-------|--------|
| `id` | nome univoco nel bucket |
| `priority` | ordine di valutazione, il più basso prima (default 100) |
| `effect` | `allow` o `deny` |
| `principals` | `*` (tutti, anonimi compresi), `user:NOME`, `group:NOME`, `accesskey:CHIAVE`, `distribution:ID` |
| `actions` | `read` (GET/HEAD, streaming dei video incluso), `stream` (i video si riproducono in HLS, gli originali non si scaricano: `nexo:GetMediaStream`), `list`, `write` (PUT/POST/multipart), `delete`, `tag`, `all`, oppure azioni (`s3:GetObject`, `nexo:GetMediaStream`) |
| `match` | `prefix`, `glob` o `regex` sulla chiave (opzionale: senza, tutto il bucket) |
| condizioni | IP o CIDR del client (`--source-ip`, `--not-source-ip`), `--referer`, `--host`, `--secure-transport`, finestra temporale (`--from`, `--to`), `--method`, `--header`, gruppi nominati della regex (`--capture`) |

### Pattern

| Tipo | Sintassi | Esempio |
|------|----------|---------|
| `prefix` | prefisso letterale | `public/` |
| `glob` | `*` (un segmento), `**` (più segmenti), `?`, `[abc]`, `{a,b}` | `docs/**/*.pdf`, `{img,video}/*` |
| `regex` | RE2 (nessun backreference: tempo lineare, nessun ReDoS) | `^users/[a-z0-9_-]+/avatar\.png$` |

- Le chiavi sono confrontate **decodificate** e normalizzate (Unicode NFC, `--key-normalization`): `%2e%2e/`,
  doppie codifiche o varianti Unicode non aggirano le regole.
- Una regex è validata al salvataggio (errore con posizione), al massimo 1024 caratteri; al massimo 200
  regole per bucket (`--access-rules-max`).
- `--ignore-case` rende il confronto insensibile alle maiuscole.

## Esempi

### Immagini pubbliche, cartelle private sempre negate

```console
$ nexoctl access rule add local/media --id img-pub --allow --principal '*' --action read \
    --regex '^img/[0-9]{4}/.*\.(jpe?g|png)$' --ignore-case
$ nexoctl access rule add local/media --id no-private --deny --principal '*' --action all \
    --glob '**/private/**' --priority 5
$ nexoctl access rule list local/media
ID                   PRIORITY  EFFECT PRINCIPALS           ACTIONS          MATCH
no-private                  5  deny   *                    all              glob **/private/**
img-pub                   100  allow  *                    read             regex(i) ^img/[0-9]{4}/.*\.(jpe?g|png)$
```

| Richiesta anonima | Esito |
|-------------------|-------|
| `GET /media/img/2026/a.jpg` | 200 |
| `GET /media/img/2026/b.PNG` | 200 (`--ignore-case`) |
| `GET /media/img/old/c.jpg` | 403 (l'anno non è di 4 cifre) |
| `GET /media/docs/private/x.pdf` | 403 (deny) |

### Home directory con una regex

Ogni membro del gruppo `dipendenti` accede solo a `home/<proprio nome>/`: il gruppo nominato `user` della
regex deve coincidere con `${aws:username}`.

```bash
nexoctl access rule add local/home --id home --allow --principal 'group:dipendenti' --action all \
  --regex '^(?P<user>[^/]+)/' --capture 'user=${aws:username}'
```

### Accesso da una rete aziendale, solo in HTTPS

```bash
nexoctl access rule add local/report --id lan --allow --principal 'group:analisti' --action read \
  --source-ip 10.0.0.0/8 --source-ip 2001:db8::/32 --secure-transport true
```

L'IP del client è quello reale ricavato dai proxy fidati (`--trusted-proxies`); un header `X-Forwarded-For`
da un proxy non fidato è ignorato.

### Solo traffico dal proxy (origin secret)

```bash
# sul server: NEXO_SECRET_PROXY_TOKEN=<valore casuale>
nexoctl access rule add local/cdn --id via-proxy --allow --principal '*' --action read \
  --header 'X-Origin-Secret: ${secret:proxy-token}'
```

Il riferimento `${secret:proxy-token}` è risolto su ogni nodo da `NEXO_SECRET_PROXY_TOKEN` (o `_FILE`): il
segreto non finisce mai nella configurazione. Un riferimento non risolvibile non concede nulla.

### Finestra temporale

```bash
nexoctl access rule add local/press --id embargo --allow --principal '*' --action read \
  --prefix comunicati/2026-11/ --from 2026-11-01T09:00:00Z
```

## Verificare prima di applicare

`nexoctl access test` valuta richieste senza eseguirle, con tutte le sorgenti (IAM, bucket policy, regole):

```console
$ nexoctl access test local/media/img/2026/a.jpg local/media/img/old/c.jpg --principal anonymous --ip 1.2.3.4
img/2026/a.jpg: allowed by access rule `img-pub` (anonymous, s3:GetObject)
  rules: allow (matched: img-pub)
img/old/c.jpg: denied (no statement allows it) (anonymous, s3:GetObject)
  rules: none
```

`-v` mostra ogni regola con il motivo per cui non si applica; `--host`, `--header`, `--referer`, `--secure`,
`--distribution` simulano il resto della richiesta. La console ha lo stesso tester, con le chiavi
d'esempio del bucket evidenziate.

## Ordinare, modificare, esportare

```bash
nexoctl access rule move local/media --id img-pub --priority 10
nexoctl access rule info local/media --id img-pub
nexoctl access rule add local/media --id img-pub --replace …        # sostituisce la regola
nexoctl access rule remove local/media --id img-pub
nexoctl access export local/media rules.yaml                        # YAML per GitOps
nexoctl access import local/media-copia rules.yaml
```

Ogni modifica sostituisce le regole del bucket in modo atomico e fallisce se nel frattempo sono cambiate.

Formato YAML (lo stesso dell'export):

```yaml
bucket: media
rules:
  - id: img-pub
    priority: 100
    effect: allow
    principals:
      - '*'
    actions:
      - read
    match:
      type: regex
      pattern: ^img/[0-9]{4}/.*\.(jpe?g|png)$
      caseInsensitive: true
  - id: no-private
    priority: 5
    effect: deny
    principals:
      - '*'
    actions:
      - all
    match:
      type: glob
      pattern: '**/private/**'
```

## Preset pubblici e block public access

| Preset (`nexoctl access public set …`) | Effetto |
|----------------------------------------|---------|
| `private` | nessuna regola anonima |
| `public-read` | lettura anonima di tutto il bucket |
| `public-read-list` | lettura ed elenco |
| `public-prefix` | lettura di un prefisso, glob o regex (`--prefix`, `--glob`, `--regex`) |
| `public-write` | scrittura anonima (richiede `--confirm`) |

Il block public access (per bucket o cluster) rifiuta o ignora le regole con principale `*`: vedi
[Condivisione e accesso anonimo](sharing.md#regole-di-accesso-pubbliche-e-block-public-access).
