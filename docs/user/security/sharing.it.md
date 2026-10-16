# Condivisione e accesso anonimo

| Bisogno | Strumento | Revocabile |
|---------|-----------|------------|
| Dare un file (o una cartella) a qualcuno senza credenziali | share link gestito (`nexoctl share download`, console **Condividi**) | ✅ singolarmente |
| Far caricare un file a qualcuno | share link di upload (`nexoctl share upload`), POST policy per form HTML | ✅ (PUT gestito) |
| URL presigned S3 per qualsiasi metodo | `nexoctl presign`, `share … --raw`, SDK S3 | ❌ (scade e basta) |
| Rendere pubblico un bucket o un prefisso | accesso anonimo (`nexoctl anonymous`) o [regole di accesso](access-rules.md) | ✅ |
| Servire un sito o dei download su un dominio | [distribuzioni](../distributions/README.md) | ✅ |

## Share link gestiti

Ogni link gestito è firmato da un **service account dedicato**, figlio dell'utente che lo crea, con una
policy limitata all'oggetto (o al prefisso) e all'operazione, e con la stessa scadenza del link. Revocare
il link elimina quel service account: l'URL smette di funzionare subito, su tutti i nodi.

```console
$ nexoctl share download --expire 2h local/foto/hello.txt
URL: local/foto/hello.txt
Expire: 2h0m0s
Share: http://localhost:9000/foto/hello.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=share-2h5zvraevd7j%2F…
ID: 2h5zvraevd7j (revoke with: nexoctl share revoke local 2h5zvraevd7j)
```

| Opzione | Effetto |
|---------|---------|
| `--expire 10m` … `7d` | validità (da 1 secondo a 7 giorni, default 7 giorni) |
| `--recursive` | un link per ogni oggetto sotto un prefisso (un solo share, revocato in blocco) |
| `--version-id ID` | una versione specifica |
| `--content-disposition attachment` | forza il download con il nome del file |
| `--content-type` | Content-Type della risposta |

```bash
nexoctl share list local                   # i miei link attivi
nexoctl share list --user alice local/foto  # di un altro utente (admin:ManageShares)
nexoctl share revoke local 2h5zvraevd7j
nexoctl share revoke --all --user alice local
```

Anche senza revoca esplicita, un link smette di funzionare se l'utente che l'ha creato viene disabilitato,
eliminato, perde il permesso o cambia la secret key: i permessi sono valutati **al momento dell'uso**.
Disabilitare l'utente (o l'access key con cui il link è stato creato), eliminarlo o cambiarne la secret key
revoca i suoi link gestiti in modo definitivo: riabilitarlo non li ripristina. La
console offre lo stesso flusso (**Condividi** su un oggetto o una cartella, con QR code) e la pagina
**Condivisioni** per vedere e revocare i link.

## Link di upload

```console
$ nexoctl share upload --expire 1h local/inbox/report.pdf
URL: local/inbox/report.pdf
Expire: 1h0m0s
Share: http://localhost:9000/inbox/report.pdf?X-Amz-Algorithm=…
Command: curl -X PUT --upload-file <FILE> 'http://localhost:9000/inbox/report.pdf?X-Amz-Algorithm=…'
ID: ij3hcpbsedr5 (revoke with: nexoctl share revoke local ij3hcpbsedr5)
```

Per una pagina web che carica file direttamente nel bucket usa una **POST policy**, firmata localmente con
le credenziali dell'alias, con condizioni su chiave, dimensione e tipo:

```console
$ nexoctl share upload --post --max-size 10MiB --prefix uploads/ --expire 1h local/inbox
URL: local/inbox
Expire: 1h0m0s
POST: http://localhost:9000/inbox
Fields:
  key: uploads/${filename}
  policy: eyJjb25kaXRpb25zIjpb…
  x-amz-algorithm: AWS4-HMAC-SHA256
  x-amz-credential: admin/20261015/us-east-1/s3/aws4_request
  x-amz-date: 20261015T115822Z
  x-amz-signature: 646cf714…
Command: curl --form-string 'key=uploads/${filename}' … -F file=@<FILE> 'http://localhost:9000/inbox'
HTML:
<form action="http://localhost:9000/inbox" method="post" enctype="multipart/form-data">
  …
  <input type="file" name="file">
  <input type="submit" value="Upload">
</form>
```

Il server verifica la firma, la scadenza e le condizioni (`content-length-range`, `starts-with` sulla
chiave, `Content-Type`) in streaming. Una POST policy non è revocabile: firmala con un utente o un'access
key dedicati e con una scadenza breve. La console ha un generatore con snippet HTML, curl e JavaScript.

## URL presigned "grezzi"

```bash
nexoctl presign --expire 10m local/foto/hello.txt                               # GET
nexoctl presign --method PUT --header "Content-Type: image/png" local/img/logo.png
nexoctl presign --method DELETE --expire 10m local/tmp/old.bin
nexoctl share download --raw --expire 10m local/foto/hello.txt
```

Sono URL SigV4 standard, firmati con le credenziali dell'alias: funzionano con qualsiasi client HTTP,
scadono al più dopo 7 giorni e **non si possono revocare** se non cambiando o eliminando le credenziali
che li hanno firmati (disabilitarle sospende gli URL finché non vengono riabilitate). Gli header firmati (`--header`) vanno inviati identici. Dietro un reverse proxy
imposta `NEXO_SERVER_URL` con l'URL pubblico, così console e CLI generano link con l'host giusto, e fai
inoltrare l'header `Host` invariato (la firma lo include).

Una bucket policy può limitare l'uso dei presigned con le condizioni `s3:authType` (`REST-QUERY-STRING`),
`s3:signatureAge`, `aws:SourceIp`, `aws:Referer`.

## Accesso anonimo per bucket e prefisso

`nexoctl anonymous` gestisce la **bucket policy** con quattro livelli, per l'intero bucket o per un prefisso:

| Livello | Anonimo può |
|---------|-------------|
| `private` | nulla (rimuove la regola) |
| `download` | leggere gli oggetti ed elencare il prefisso |
| `upload` | scrivere oggetti |
| `public` | leggere, elencare e scrivere |

```console
$ nexoctl anonymous set download local/sito/pubblico/
Access permission for `local/sito/pubblico/` is set to `download`.
$ curl -s -o /dev/null -w '%{http_code}\n' http://localhost:9000/sito/pubblico/p.txt
200
$ curl -s -o /dev/null -w '%{http_code}\n' http://localhost:9000/sito/privato/p.txt
403
$ nexoctl anonymous list local/sito
download local/sito/pubblico/
```

Per una policy su misura: `nexoctl anonymous get-json local/sito > policy.json`, modifica il file e
`nexoctl anonymous set-json policy.json local/sito` (un documento senza statement rimuove la policy). Le
stesse impostazioni sono nella scheda **Accesso** del bucket in console.

`upload` e `public` permettono a chiunque di scrivere nel bucket: usali solo con una quota
(`nexoctl quota set local/bucket --size 10GiB`) e per dati non sensibili.

## Regole di accesso pubbliche e block public access

Le [regole di accesso](access-rules.md) offrono preset pubblici più espressivi (glob e regex):

```bash
nexoctl access public set public-read local/media
nexoctl access public set public-prefix local/media --glob 'img/**'
nexoctl access public set private local/media
```

Il **block public access** impedisce le regole di accesso con principale `*` (tutti, anonimi compresi) su un
bucket o su tutto il cluster:

```console
$ nexoctl access block-public on local/sito            # un bucket
$ nexoctl access block-public on local --mode ignore   # tutto il cluster
$ nexoctl access block-public status local/sito
Public access of `local/sito`: blocked (cluster: off, mode reject, bucket: on).
$ nexoctl access public set public-read local/sito
nexoctl: error: local/sito: rule 1 ("preset-public-read"), principals: public access is blocked (bucket): a rule cannot allow everyone ("*") (public_access_blocked)
```

- `reject` (default): le regole pubbliche sono rifiutate al salvataggio;
- `ignore`: vengono salvate ma ignorate durante la valutazione;
- vale anche per le distribuzioni `website` pubbliche, che rispondono `403` finché il blocco è attivo;
- si imposta anche all'avvio per tutto il cluster con `--block-public-access` (`NEXO_BLOCK_PUBLIC_ACCESS`)
  o a caldo con il sottosistema `access` (`nexoctl admin config set local access block_public=on`).

> **Nota**: il block public access agisce sulle regole di accesso e sulle distribuzioni; una bucket policy
> anonima impostata con `nexoctl anonymous` (o con `PutBucketPolicy`) resta in vigore. Per chiudere un
> accesso anonimo verifica sempre anche `nexoctl anonymous list local/bucket`.

## CORS

Le applicazioni web servite da un altro dominio che chiamano l'API S3 dal browser hanno bisogno del CORS:

```bash
cat > cors.xml <<'EOF'
<CORSConfiguration>
  <CORSRule>
    <AllowedOrigin>https://app.example.com</AllowedOrigin>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedMethod>PUT</AllowedMethod>
    <AllowedHeader>*</AllowedHeader>
    <ExposeHeader>ETag</ExposeHeader>
    <MaxAgeSeconds>3600</MaxAgeSeconds>
  </CORSRule>
</CORSConfiguration>
EOF
nexoctl cors set local/foto cors.xml
nexoctl cors get local/foto
```

Un bucket **senza** configurazione CORS segue la politica globale `api.cors_allow_origin` (default `*`):
`nexoctl admin config set local api cors_allow_origin=https://app.example.com` la restringe per tutto il
deployment.
