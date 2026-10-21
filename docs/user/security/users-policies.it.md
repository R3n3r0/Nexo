# Utenti, policy e credenziali

Nexo ha un solo account con un utente **root** (credenziali da ambiente) e utenti IAM gestiti da console,
`nexoctl admin` o Admin API. Ogni richiesta, S3 o della console, è valutata dallo stesso motore di policy
in formato AWS: **deny esplicito vince**, poi serve un **allow** (da policy dell'utente, dei suoi gruppi,
della bucket policy o delle [regole di accesso](access-rules.md)), altrimenti la richiesta è negata.

## Root

- `NEXO_ROOT_USER` / `NEXO_ROOT_PASSWORD` (o `_FILE`), uguali su tutti i nodi di un cluster.
- Usalo solo per amministrare: per le applicazioni crea utenti o access key con i permessi minimi.
- `nexoctl admin config set local api root_access=off` impedisce l'uso delle credenziali root sull'API
  S3 (la console e l'Admin API restano accessibili, così si può sempre riattivare).
- Cambiare la password root richiede di riavviare tutti i nodi con il nuovo valore; le credenziali root
  firmano anche i token tra i nodi e cifrano l'export IAM.

## Utenti e gruppi

```bash
nexoctl admin user add local alice 'una-secret-key-lunga' --policy readwrite
nexoctl admin user add local bob 'altra-secret-key-lunga' --group dipendenti --disabled
nexoctl admin user list local
nexoctl admin user info local alice
nexoctl admin user disable local alice          # blocco immediato su tutti i nodi; revoca sessioni e link
nexoctl admin user enable local alice
nexoctl admin user remove local alice           # rimuove anche le sue access key e i suoi share link

nexoctl admin group add local dipendenti alice bob --policy home-dir   # crea il gruppo se non esiste
nexoctl admin group members local dipendenti
nexoctl admin group remove local dipendenti bob                       # toglie un membro
nexoctl admin group disable local dipendenti
```

Un utente senza policy non può fare nulla. Le policy di un utente si sommano a quelle dei suoi gruppi. La
secret key si cambia dalla console o con l'Admin API (`PUT /api/v1/users/{u}`): il cambio revoca le sue
sessioni console e i suoi share link gestiti. Anche disabilitare un utente li revoca, insieme alle sessioni console
delle sue access key, e in modo definitivo: riabilitare l'utente ripristina le credenziali, non le sessioni né i
link (occorre un nuovo login e una nuova condivisione).

## Policy

### Policy predefinite

| Policy | Concede |
|--------|---------|
| `readonly` | `s3:Get*` e `s3:List*` su tutti i bucket |
| `readwrite` | tutte le azioni `s3:*` su tutti i bucket |
| `writeonly` | scrittura (`s3:PutObject`, multipart) su tutti i bucket, senza lettura |
| `diagnostics` | informazioni sul server, trace, log, metriche Prometheus, profiling, speedtest (nessun dato) |
| `consoleAdmin` | `admin:*` e `s3:*`: amministrazione completa |

### Gestione

```bash
nexoctl admin policy create local foto-ro foto-ro.json            # --update per sostituirla
nexoctl admin policy validate local foto-ro.json                  # errori con riga e colonna
nexoctl admin policy attach local foto-ro --user alice
nexoctl admin policy attach local home-dir --group dipendenti
nexoctl admin policy detach local foto-ro --user alice
nexoctl admin policy entities local foto-ro                       # a chi è assegnata
nexoctl admin policy info local foto-ro
```

Il linguaggio è quello di AWS: `Action`/`NotAction`, `Resource`/`NotResource`, wildcard `*` e `?`,
condizioni (`String*`, `Numeric*`, `Date*`, `Bool`, `IpAddress`/`NotIpAddress`, `Null`, `ArnLike`,
suffisso `IfExists`, `ForAnyValue:`/`ForAllValues:`) con chiavi come `aws:SourceIp`,
`aws:SecureTransport`, `aws:CurrentTime`, `aws:username`, `aws:Referer`, `s3:prefix`, `s3:delimiter`,
`s3:x-amz-server-side-encryption`, `s3:ExistingObjectTag/<k>`, `s3:RequestObjectTag/<k>`,
`s3:signatureAge`, `s3:authType`, e la variabile `${aws:username}`. Le azioni di amministrazione sono
`admin:*` (per esempio `admin:CreateUser`, `admin:ServerInfo`, `admin:Heal`, `admin:ConfigUpdate`).

### Esempio: sola lettura di un bucket

`foto-ro.json`: l'utente elenca e scarica gli oggetti del bucket `foto` e nient'altro.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": ["arn:aws:s3:::foto"]
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": ["arn:aws:s3:::foto/*"]
    }
  ]
}
```

```console
$ nexoctl admin policy create local foto-ro foto-ro.json
$ nexoctl admin user add local alice 'alice-secret-123' --policy foto-ro
$ nexoctl alias set alice http://localhost:9000 alice 'alice-secret-123'
Added alias "alice" for http://localhost:9000 (credentials accepted; listing the buckets is denied).
$ nexoctl cat alice/foto/hello.txt
ciao Nexo
$ nexoctl cp x.txt alice/foto/x.txt
nexoctl: error: alice/foto/x.txt: access denied: Access Denied. (AccessDenied)
```

Per far vedere il bucket anche nell'elenco dei bucket aggiungi uno statement con `s3:ListAllMyBuckets` su
`arn:aws:s3:::*` (mostra i nomi di tutti i bucket, non il loro contenuto).

### Esempio: home directory per utente

Un solo bucket `home`, ogni utente legge e scrive solo sotto `home/<proprio nome>/`. Una sola policy per
tutti, assegnata a un gruppo, grazie a `${aws:username}`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": ["arn:aws:s3:::home"],
      "Condition": {"StringLike": {"s3:prefix": ["${aws:username}/*", "${aws:username}"]}}
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
      "Resource": ["arn:aws:s3:::home/${aws:username}/*"]
    }
  ]
}
```

```bash
nexoctl mb local/home
nexoctl admin policy create local home-dir home.json
nexoctl admin group add local dipendenti alice bob --policy home-dir
nexoctl cp report.pdf bob/home/bob/            # ok
nexoctl cp report.pdf bob/home/alice/          # AccessDenied
nexoctl ls bob/home/alice/                     # AccessDenied
```

La console mostra a questi utenti il bucket `home` con il solo contenuto accessibile. Con le
[regole di accesso](access-rules.md#home-directory-con-una-regex) lo stesso schema si esprime anche con
una regex e un gruppo nominato.

### Esempio: sola scrittura (cartella di deposito)

Uno scanner o un sistema esterno deposita file in `inbox` senza poterli rileggere né elencare:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:AbortMultipartUpload"],
      "Resource": ["arn:aws:s3:::inbox/*"]
    }
  ]
}
```

`s3:PutObject` copre anche gli upload multipart (file grandi). Per limitare a un prefisso usa
`arn:aws:s3:::inbox/scanner1/*`; per imporre la cifratura aggiungi una condizione su
`s3:x-amz-server-side-encryption`.

### Esempio: streaming dei video senza download

`nexo:GetMediaStream` permette la presentazione HLS di un video (playlist, segmenti, qualità, poster, miniature: il
player della console, i [link di condivisione](sharing.it.md) e i [behavior video](../distributions/README.it.md#streaming-video-sul-tuo-dominio)
delle distribuzioni) senza `s3:GetObject`, che scarica l'originale. Una bucket policy può concederla a tutti:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": "*",
      "Action": ["nexo:GetMediaStream"],
      "Resource": ["arn:aws:s3:::courses/lessons/*"]
    }
  ]
}
```

Chi può leggere un oggetto (`s3:GetObject`) può anche riprodurlo in streaming.

### Simulatore

Prima di applicare una policy, o per capire un `AccessDenied`:

```console
$ nexoctl admin policy simulate --user bob --action s3:GetObject local/home/alice/x.txt
Decision: deny
User: bob
Action: s3:GetObject
Resource: home/alice/x.txt
Reason: no policy or access rule allows s3:GetObject

$ nexoctl admin policy simulate --user bob --action s3:ListBucket --condition s3:prefix=bob/ local/home
Decision: allow
…
Reason: allowed by group policy "home-dir" (group dipendenti), statement 1
```

Opzioni: `--source-ip`, `--secure`/`--insecure`, `--time`, `--condition chiave=valore`, `-v` per vedere
ogni statement valutato. La console ha lo stesso strumento (**Simulatore policy**) e mostra i permessi
effettivi di ogni utente.

## Access key (service account)

Un utente può avere più access key, ognuna con scadenza e una **policy inline** che restringe (mai
allarga) i permessi dell'utente: ideali per applicazioni e pipeline CI.

```console
$ nexoctl admin accesskey create local --user alice --name ci --description "CI build" \
    --expiry 30d --policy only-2026.json
Access Key: FZR6F4G0E0NF208I5QQ5
Secret Key: AWGjCT6eugjPgtYyAMig2FE01tJC6nYAK6HcLjMk
Parent User: alice
Expiration: 2026-11-14 11:57:31 UTC
Policy: inline (restricts the parent user)
The secret key is shown only once: store it now.
```

```bash
nexoctl admin accesskey list local --user alice
nexoctl admin accesskey edit local FZR6F4G0E0NF208I5QQ5 --disable
nexoctl admin accesskey edit local FZR6F4G0E0NF208I5QQ5 --expiry 2027-01-31 --clear-policy
nexoctl admin accesskey remove local FZR6F4G0E0NF208I5QQ5
```

- La secret key è mostrata **una sola volta**.
- Disabilitare una chiave (`--disable`) revoca le sue sessioni console e gli share link gestiti creati con essa;
  `--enable` non li ripristina.
- Senza `--user` la chiave appartiene all'utente dell'alias: ogni utente gestisce le proprie chiavi, root
  quelle di tutti.
- I permessi effettivi sono l'intersezione tra la policy inline e quelle dell'utente: se all'utente viene
  tolta una policy, le sue chiavi la perdono subito.

## Credenziali temporanee (STS AssumeRole)

Nexo implementa `AssumeRole` dell'API STS sulla porta S3: un utente (o root) ottiene credenziali
temporanee (access key, secret key e session token) valide da 15 minuti a 12 ore (default 1 ora),
eventualmente ristrette da una **session policy**. Il "ruolo" è l'identità che firma la richiesta:
`RoleArn` è richiesto dagli SDK ma non viene usato.

Con la AWS CLI:

```bash
aws sts assume-role --endpoint-url http://localhost:9000 --region us-east-1 \
  --role-arn arn:aws:iam::000000000000:role/qualsiasi --role-session-name ci \
  --duration-seconds 3600 \
  --policy '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::foto/*"}]}'
```

Oppure con `curl` (firma SigV4 per il servizio `sts`):

```bash
curl -s --aws-sigv4 "aws:amz:us-east-1:sts" --user 'alice:alice-secret-123' \
  -X POST -H 'Content-Type: application/x-www-form-urlencoded' \
  --data 'Action=AssumeRole&Version=2011-06-15&DurationSeconds=900&RoleSessionName=ci' \
  http://localhost:9000/
```

La risposta XML contiene `AccessKeyId`, `SecretAccessKey`, `SessionToken` ed `Expiration`. Le credenziali
temporanee non possono a loro volta chiamare `AssumeRole`. Un URL presigned firmato con credenziali STS
scade al più tardi con la sessione.

## Federazione delle identità (OpenID Connect, LDAP)

Gli utenti di un provider di identità esterno accedono alla console e ottengono credenziali temporanee senza un
utente Nexo proprio. I loro permessi vengono da Nexo:

- i **gruppi Nexo** con lo stesso nome dei loro gruppi nel provider (il claim `groups` di OpenID, i gruppi della
  directory per LDAP) danno le loro policy; un gruppo disattivato non ne dà nessuna, e le modifiche valgono per
  le sessioni aperte dalla richiesta successiva;
- con OpenID si aggiungono le policy nominate dal claim `policy` (`claim_name`).

Un utente che non corrisponde ad alcuna policy viene rifiutato. Le credenziali appartengono all'utente padre
`openid:<username>` o `ldap:<username>` (visibile nell'audit log e utilizzabile nelle policy come
`${aws:username}`); i claim del token sono disponibili nelle condizioni delle policy come `jwt:<claim>`.

### OpenID Connect

```bash
nexoctl admin config set local identity_openid enable=on \
  config_url=https://idp.example.com/.well-known/openid-configuration \
  client_id=nexo-console client_secret=... display_name="SSO aziendale"
```

| Chiave | Default | Significato |
|--------|---------|-------------|
| `config_url` | | documento di discovery del provider |
| `client_id`, `client_secret` | | il client registrato nel provider (client confidenziale) |
| `scopes` | `openid,profile,email` | scope del login della console |
| `claim_name` | `policy` | claim con le policy di Nexo (stringa separata da virgole o array) |
| `groups_claim` | `groups` | claim con i gruppi |
| `username_claim` | `preferred_username` | nome dell'utente (poi `email`, poi `sub`) |
| `display_name` | `SSO` | etichetta del pulsante nella pagina di login |
| `redirect_uri` | URL della console + `/api/v1/auth/openid/callback` | il redirect URI registrato nel provider |

La pagina di login mostra **Accedi con <display_name>**: la console esegue l'authorization code flow con PKCE
(S256), uno state legato al browser e un nonce. Registra il redirect URI nel provider: con
`--browser-redirect-url` impostato è `<quell'URL>/api/v1/auth/openid/callback`, altrimenti l'URL della console
come lo raggiunge il browser. La sessione della console dura quanto la durata di sessione configurata (12 ore),
non quanto l'ID token.

Le applicazioni scambiano un ID token con credenziali S3 tramite `AssumeRoleWithWebIdentity` (senza firma;
l'audience del token deve essere `client_id`):

```bash
aws sts assume-role-with-web-identity --endpoint-url http://localhost:9000 \
  --role-arn arn:aws:iam::000000000000:role/any --role-session-name ci \
  --web-identity-token "$ID_TOKEN" --duration-seconds 3600
```

Le credenziali non sopravvivono mai al token. Sono accettate chiavi RSA (2048 bit e oltre) ed ECDSA (P-256,
P-384, P-521); le chiavi del provider vengono riscaricate quando un token ne nomina una sconosciuta.

### LDAP e Active Directory

```bash
nexoctl admin config set local identity_ldap enable=on server_addr=ldap.example.com:636 \
  lookup_bind_dn="cn=nexo,ou=services,dc=example,dc=com" lookup_bind_password=... \
  user_dn_search_base_dn="ou=people,dc=example,dc=com" user_dn_search_filter="(uid=%s)" \
  group_search_base_dn="ou=groups,dc=example,dc=com" group_search_filter="(&(objectClass=groupOfNames)(member=%d))"
```

La connessione usa LDAPS (`tls=on`, il default) o StartTLS (`tls=off starttls=on`); `tls=off` da solo è LDAP in
chiaro, solo per i test. Nexo si autentica con l'account di lookup, cerca l'utente con `user_dn_search_filter`
(`%s` è lo username con escape; deve corrispondere esattamente una voce), si autentica come l'utente per
verificare la password, poi elenca i suoi gruppi con `group_search_filter` (`%d` è il DN dell'utente, `%s` lo
username), nominati da `group_name_attribute` (default `cn`). Per Active Directory usa `(sAMAccountName=%s)` e
`(&(objectClass=group)(member=%d))`. Cinque tentativi falliti in un minuto bloccano uno username per un minuto.

La pagina di login della console offre allora **Directory (LDAP)**. Le applicazioni usano
`AssumeRoleWithLDAPIdentity` (campi `LDAPUsername` e `LDAPPassword` nel body, mai nell'URL):

```bash
curl -s -X POST http://localhost:9000/ -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode Action=AssumeRoleWithLDAPIdentity --data-urlencode Version=2011-06-15 \
  --data-urlencode LDAPUsername=carol --data-urlencode LDAPPassword="$PASSWORD"
```

## Buone pratiche

- Un utente o un'access key per applicazione, con i permessi minimi e una scadenza.
- Gruppi per i ruoli (lettori, editor, amministratori di un progetto) invece di policy per singolo utente.
- `diagnostics` per il monitoraggio (Prometheus, trace), mai root.
- Disabilitare (non solo eliminare) un utente sospetto: l'effetto è immediato su tutto il cluster, e le sue
  sessioni console e i suoi link gestiti sono revocati in modo definitivo. Cambiarne anche la secret prima di
  riabilitarlo: gli URL presigned raw firmati con la vecchia secret tornerebbero validi.
- Rivedere periodicamente `nexoctl admin accesskey list` e gli share attivi (`nexoctl share list --user …`).
