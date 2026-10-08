# 9. URL presigned, POST policy, policy di accesso e condivisione

Tutte le funzionalità di questo documento sono **P0/P1 obbligatorie per la v1.0** e sono disponibili da
**API S3, CLI (`nexoctl`) e dashboard**, in modalità singolo nodo e cluster.

## 9.1 URL presigned (SigV4 query string)

### Operazioni supportate
| Operazione | Metodo | Uso |
|------------|--------|-----|
| GetObject | GET | Download / link di condivisione |
| HeadObject | HEAD | Verifica esistenza/metadati |
| PutObject | PUT | Upload diretto da client senza credenziali |
| DeleteObject | DELETE | Cancellazione delegata |
| CreateMultipartUpload / UploadPart / CompleteMultipartUpload / AbortMultipartUpload | POST/PUT | Upload di file grandi direttamente dal browser o da client terzi |
| GetObject con `versionId` | GET | Link a una versione specifica |
| ListObjectsV2 (bucket) | GET | Listing delegato (se la policy del firmatario lo consente) |

### Regole
| Regola | Valore |
|--------|--------|
| Parametri | `X-Amz-Algorithm`, `X-Amz-Credential`, `X-Amz-Date`, `X-Amz-Expires`, `X-Amz-SignedHeaders`, `X-Amz-Signature`, `X-Amz-Security-Token` (per credenziali temporanee) |
| Scadenza | 1 s – **7 giorni** (604800 s); oltre → `AuthorizationQueryParametersError` |
| Credenziali temporanee | Il link scade al minimo tra `X-Amz-Expires` e la scadenza della sessione STS / access key |
| Permessi | Valutati **al momento dell'uso** con l'identità del firmatario: se l'utente viene disabilitato, la policy rimossa o la chiave revocata, il link smette di funzionare subito |
| Header firmati | Se firmati (`Content-Type`, `x-amz-meta-*`, `x-amz-server-side-encryption`, `Content-MD5`, checksum…) devono coincidere |
| Override risposta | `response-content-type`, `response-content-disposition`, `response-content-language`, `response-expires`, `response-cache-control`, `response-content-encoding` (solo con richiesta firmata) |
| SSE-C | Supportato: le chiavi viaggiano come header firmati |
| Virtual-host e path-style | Entrambi; host pubblico configurabile (`NEXO_SERVER_URL`) |
| Clock skew | `X-Amz-Date` nel futuro di oltre 15 min → rifiuto |
| Policy di bucket | Condizioni `s3:signatureAge`, `s3:authType` (`REST-QUERY-STRING`), `aws:SourceIp`, `aws:Referer`, `aws:SecureTransport` per limitare l'uso dei presigned |

### Revoca dei link condivisi
Un URL presigned SigV4 non è revocabile singolarmente per natura. Nexo fornisce quindi:
1. **Share link gestiti** (console e CLI): ogni condivisione è firmata con un **service account dedicato**
   (`share-<id>`, figlio dell'utente, policy inline limitata a quell'oggetto/prefix e alle sole azioni richieste,
   scadenza uguale al link). Revocare il link = eliminare quel service account → effetto immediato su tutto il cluster.
2. Elenco delle condivisioni attive per utente/bucket (`nexoctl share list`, pagina "Condivisioni" in dashboard), con
   creatore, oggetto, permessi, scadenza, revoca.
3. Revoca di massa: disabilitare l'utente o ruotare le sue chiavi invalida tutti i suoi link. La rotazione del
   segreto dalla console o dalla CLI (`PUT /users/{u}` con `secretKey`) revoca gli share gestiti dell'utente
   (i presigned raw firmati col vecchio segreto smettono di verificare da soli) e chiude le sue sessioni console:
   ogni sessione è legata al segreto con cui è stata aperta. Le revoche delle sessioni (logout) sono persistite
   nello store IAM, quindi sopravvivono al riavvio e valgono su ogni nodo. L'eliminazione dell'utente
   (`DELETE /users/{u}`, `nexoctl admin user remove`) rimuove anche i record dei suoi share gestiti: i loro
   service account spariscono con l'utente, quindi non restano elencati come attivi né passano a un utente
   ricreato con lo stesso nome.

## 9.2 POST policy (upload da form HTML)

- Endpoint `POST /{bucket}` con `multipart/form-data`, campi `key` (anche `${filename}`), `policy` (base64 JSON),
  `x-amz-algorithm`, `x-amz-credential`, `x-amz-date`, `x-amz-signature`, `success_action_status`,
  `success_action_redirect`, `Content-Type`, `x-amz-meta-*`, tagging, SSE.
- Condizioni supportate: `eq`, `starts-with`, `content-length-range`, match esatto su bucket/key/acl/
  Content-Type/metadati; `expiration` obbligatoria.
- Il campo `file` deve essere l'ultimo; la dimensione è verificata in streaming (nessun buffering completo).
- Generazione da CLI (`nexoctl share upload --post`) e dalla dashboard (snippet HTML/curl pronto all'uso).

## 9.3 Policy di accesso

| Tipo | Ambito | Gestione |
|------|--------|----------|
| **Policy IAM** (identity-based) | Utenti, gruppi, service account, ruoli STS | Admin API, CLI `admin policy`, dashboard "Policy" |
| **Policy inline di service account / STS** | Restringe il parent (intersezione) | CLI `admin accesskey`, dashboard "Access key" |
| **Bucket policy** (resource-based) | Bucket e prefix, anche per anonimi (`"Principal": "*"`) | S3 `PutBucketPolicy`, CLI `anonymous`/`policy`, dashboard "Accesso" del bucket |
| **Accesso anonimo semplificato** | `private`, `download`, `upload`, `public` per bucket o **prefix** | CLI `nexoctl anonymous set download local/bkt/pubblico/`, dashboard con regole per prefix |
| **Object lock / legal hold** | Protezione WORM indipendente dai permessi | Vedi S-BKT-08 |

Valutazione (come AWS): deny esplicito in qualsiasi policy → nega; altrimenti allow se concesso da policy IAM
**o** dalla bucket policy; per service account/STS il risultato è intersecato con la policy inline.

Policy predefinite: `readonly`, `readwrite`, `writeonly`, `diagnostics`, `consoleAdmin`.

Supporto policy:
- `Action`/`NotAction`, `Resource`/`NotResource`, `Principal`/`NotPrincipal` (bucket policy), wildcard `*` e `?`;
- operatori `String*`, `Numeric*`, `Date*`, `Bool`, `IpAddress`/`NotIpAddress`, `Null`, `ArnLike`, suffisso `IfExists`,
  `ForAnyValue:`/`ForAllValues:`;
- chiavi `aws:SourceIp`, `aws:SecureTransport`, `aws:CurrentTime`, `aws:username`, `aws:userid`, `aws:Referer`,
  `aws:UserAgent`, `s3:prefix`, `s3:delimiter`, `s3:max-keys`, `s3:x-amz-server-side-encryption`,
  `s3:ExistingObjectTag/<k>`, `s3:RequestObjectTag/<k>`, `s3:object-lock-*`, `s3:signatureAge`, `s3:authType`;
- variabili `${aws:username}` per home directory per utente (es. `arn:aws:s3:::home/${aws:username}/*`);
- **simulatore di policy**: Admin API `POST /api/v1/policies/simulate` (utente/azione/risorsa/contesto → allow/deny
  e quale statement ha deciso), esposto da CLI `admin policy simulate` e dalla dashboard.

## 9.4 Funzionalità CLI

| Comando | Descrizione |
|---------|-------------|
| `nexoctl share download [--expire 2h] [--version-id] [--content-disposition attachment] ALIAS/B/OBJ` | URL presigned GET (anche ricorsivo su prefix → elenco di URL) |
| `nexoctl share upload [--expire] [--content-type] ALIAS/B/OBJ` | URL presigned PUT + comando `curl` di esempio |
| `nexoctl share upload --post [--max-size 10MiB] [--prefix uploads/] ALIAS/B` | Campi POST policy + form HTML d'esempio |
| `nexoctl share list [--user] [ALIAS/B]` / `share revoke ID` / `share revoke --all --user U` | Share link gestiti |
| `nexoctl presign --method GET|PUT|HEAD|DELETE [--header k:v] ALIAS/B/OBJ` | Presigned "grezzo" (non gestito, non revocabile) |
| `nexoctl anonymous set private|download|upload|public ALIAS/B[/PREFIX]`, `anonymous list`, `anonymous get-json`, `anonymous set-json` | Accesso anonimo e bucket policy |
| `nexoctl admin policy create|info|list|remove|attach|detach|entities|simulate` | Policy IAM |
| `nexoctl admin accesskey create --policy file.json --expiry 30d` | Service account con policy inline |

## 9.5 Funzionalità dashboard

| Codice | Funzionalità |
|--------|--------------|
| D-SHR-01 | Dialog "Condividi" su oggetto/versione/prefix: permessi (download, upload), scadenza (preset + data), content-disposition, QR code, copia link. Un prefix in download produce un solo share gestito e un link per oggetto (`GET /shares/{id}/links`, paginato, copia singola o di tutti i link), tutti firmati dallo share e revocati con esso, come `nexoctl share download --recursive` |
| D-SHR-02 | Generatore upload: URL PUT, POST policy con condizioni (dimensione max, prefix, content-type) e snippet HTML/curl/JS |
| D-SHR-03 | Pagina "Condivisioni": elenco link attivi (miei / di tutti per admin), filtro, revoca singola o multipla |
| D-POL-01 | Tab "Accesso" del bucket: private/public/custom, regole anonime per prefix, editor bucket policy JSON con validazione |
| D-POL-02 | Editor policy IAM: Monaco con schema JSON e autocompletamento di azioni/chiavi di condizione, builder visuale |
| D-POL-03 | Simulatore policy: utente + azione + risorsa + contesto (IP, ora, TLS) → esito e statement determinante |
| D-POL-04 | Vista "Permessi effettivi" di un utente (policy dirette + gruppi + bucket policy) |

## 9.6 Test

### Server
| ID | Caso | Atteso |
|----|------|--------|
| TPS-01 | Presigned GET/PUT/HEAD/DELETE con aws-sdk-go-v2, boto3, aws-cli, eseguito con curl | 200/204 |
| TPS-02 | Presigned con `X-Amz-Expires` = 0, 604801 | Errore |
| TPS-03 | Link scaduto (clock finto) | 403 `AccessDenied` "Request has expired" |
| TPS-04 | Firma alterata, parametro aggiunto/rimosso, host diverso | 403 `SignatureDoesNotMatch` |
| TPS-05 | Header firmato (Content-Type) diverso in PUT | 403 |
| TPS-06 | Presigned con credenziali STS: scadenza della sessione prima del link | 403 alla scadenza STS |
| TPS-07 | Utente disabilitato / policy rimossa / access key eliminata dopo la generazione | 403 immediato su tutti i nodi del cluster |
| TPS-08 | `response-content-disposition` e altri override | Header di risposta corretti |
| TPS-09 | Presigned multipart: create, 3 part PUT presigned, complete | Oggetto corretto |
| TPS-10 | Presigned su `versionId` | Versione corretta |
| TPS-11 | Bucket policy che nega `s3:authType = REST-QUERY-STRING` o `s3:signatureAge > 600000` | 403 per i presigned |
| TPS-12 | POST policy: condizioni rispettate / violate (dimensione, prefix, content-type, scadenza) | 204/201 o 403/400 |
| TPS-13 | POST policy con `success_action_redirect` | 303 con bucket/key/etag in query |
| TPS-14 | Share link gestito → revoca → riuso | 403 entro 1 s su tutti i nodi |
| TPS-15 | Matrice policy: ogni operatore di condizione, NotAction/NotResource, variabili `${aws:username}`, deny esplicito | Esito atteso (> 150 casi) |
| TPS-16 | Accesso anonimo per prefix: `download` su `pub/`, private sul resto | Solo `pub/` accessibile |
| TPS-17 | Service account con policy inline più ampia del parent | Prevale il parent (intersezione) |
| TPS-18 | Simulatore di policy concorde con l'enforcement reale (property-based su policy generate casualmente) | 100% coincidenza |
| TPS-19 | Presigned dietro reverse proxy con host pubblico (vedi §8.5) | 200 |
| TPS-20 | Fuzzing dei parametri di query presigned e del documento POST policy | Nessun panic, nessun bypass |
| TPS-21 | Header `x-amz-*` non firmati aggiunti a un presigned (`x-amz-copy-source`, `x-amz-tagging`, `x-amz-meta-*`, `x-amz-storage-class`) o a una richiesta firmata via header | 403 `AccessDenied` "There were headers present in the request which were not signed", nessun effetto |

### CLI
| ID | Caso |
|----|------|
| TCL-P-01 | `share download` → `curl` scarica contenuto identico; `--expire 1s` → dopo 2 s 403 |
| TCL-P-02 | `share upload` → `curl -T` carica; `--post` → upload via form con curl (campi con `--form-string`, mai letti come file locali anche se iniziano con `@` o `<`; il file con `-F file=@...`) |
| TCL-P-03 | `share list` / `share revoke` / `share revoke --all --user` |
| TCL-P-04 | `anonymous set download` su prefix → `curl` anonimo OK solo su quel prefix |
| TCL-P-05 | `admin policy simulate` coerente con un'operazione reale |
| TCL-P-06 | `presign --method DELETE` funzionante |

### Dashboard
| ID | Caso |
|----|------|
| TD-P-01 | Condividi oggetto con scadenza 1 h → apertura in contesto browser anonimo scarica il file |
| TD-P-02 | Revoca dalla pagina Condivisioni → link restituisce 403 |
| TD-P-03 | Generatore POST policy → snippet HTML eseguito in pagina di test carica il file nel prefix corretto |
| TD-P-04 | Tab Accesso: imposta prefix pubblico → oggetto accessibile anonimo; torna private → 403 |
| TD-P-05 | Editor policy: JSON invalido bloccato, policy valida salvata e applicata |
| TD-P-06 | Simulatore: allow/deny e statement determinante visualizzati |
| TD-P-07 | Utente senza `admin:*` vede solo le proprie condivisioni |
