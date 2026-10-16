# Riferimento CLI (nexoctl)

`nexoctl` è il client a riga di comando di Nexo: gestisce gli alias dei server, bucket e oggetti (con
qualsiasi endpoint compatibile S3) e l'amministrazione di Nexo tramite l'Admin API.

Il riferimento completo di ogni comando, con sintassi, esempi e opzioni, è generato dal codice di `nexoctl`
e parte da [nexoctl](../cli/nexoctl.md). Questa pagina riassume le convenzioni comuni a tutti i comandi.

## Gruppi di comandi

| Gruppo | Comandi |
|--------|---------|
| Bucket e oggetti | [`ls`](../cli/nexoctl_ls.md), [`mb`](../cli/nexoctl_mb.md), [`rb`](../cli/nexoctl_rb.md), [`cp`](../cli/nexoctl_cp.md), [`mv`](../cli/nexoctl_mv.md), [`rm`](../cli/nexoctl_rm.md), [`put`](../cli/nexoctl_put.md), [`pipe`](../cli/nexoctl_pipe.md), [`cat`](../cli/nexoctl_cat.md), [`head`](../cli/nexoctl_head.md), [`stat`](../cli/nexoctl_stat.md), [`find`](../cli/nexoctl_find.md), [`du`](../cli/nexoctl_du.md), [`tree`](../cli/nexoctl_tree.md), [`diff`](../cli/nexoctl_diff.md), [`mirror`](../cli/nexoctl_mirror.md) |
| Condivisione e accesso | [`share`](../cli/nexoctl_share.md), [`presign`](../cli/nexoctl_presign.md), [`anonymous`](../cli/nexoctl_anonymous.md), [`access`](../cli/nexoctl_access.md), [`cors`](../cli/nexoctl_cors.md), [`dist`](../cli/nexoctl_dist.md), [`cache`](../cli/nexoctl_cache.md) |
| Impostazioni di bucket e oggetti | [`version`](../cli/nexoctl_version.md), [`ilm`](../cli/nexoctl_ilm.md), [`event`](../cli/nexoctl_event.md), [`encrypt`](../cli/nexoctl_encrypt.md), [`quota`](../cli/nexoctl_quota.md), [`retention`](../cli/nexoctl_retention.md), [`legalhold`](../cli/nexoctl_legalhold.md), [`tag`](../cli/nexoctl_tag.md) |
| Amministrazione | [`admin`](../cli/nexoctl_admin.md): `info`, `user`, `group`, `policy`, `accesskey`, `config`, `service`, `heal`, `usage`, `kms`, `notify`, `trace`, `logs`, `audit`, `metrics`, `prometheus`, `profile`, `speedtest`, `lock`, `cluster` |
| Configurazione | [`alias`](../cli/nexoctl_alias.md), [`completion`](../cli/nexoctl_completion.md) |

## Destinazioni

Un percorso remoto si scrive `ALIAS[/BUCKET[/PREFISSO]]`, dove `ALIAS` è un nome registrato con
`nexoctl alias set`; tutto il resto è un percorso locale.

```bash
nexoctl alias set local http://localhost:9000 admin supersegreta
nexoctl ls local                 # i bucket
nexoctl ls local/foto/2026/      # gli oggetti sotto un prefisso
nexoctl cp report.pdf local/docs/
```

Gli alias, con le credenziali, sono salvati in `~/.nexoctl/config.json` (permessi 0600). Le chiavi private
generate per i key group delle distribuzioni sono in `~/.nexoctl/keys/<alias>/`.

## Opzioni globali

| Opzione | Effetto |
|---------|---------|
| `--config-dir DIR` | directory di configurazione (default `$NEXOCTL_CONFIG_DIR` o `~/.nexoctl`) |
| `--json` | output JSON Lines: un oggetto per riga con un campo `status` (`success` o `error`), per gli script |
| `-q`, `--quiet` | nasconde conferme e progresso (risultati ed errori restano) |
| `--no-color` | disattiva i colori (anche con la variabile `NO_COLOR`) |
| `--debug` | stampa su stderr le richieste e le risposte HTTP, con i segreti redatti |
| `-v`, `--version` | versione di `nexoctl` |

## Variabili d'ambiente

| Variabile | Effetto |
|-----------|---------|
| `NEXOCTL_CONFIG_DIR` | directory di configurazione |
| `NEXOCTL_ENC_C` | chiavi SSE-C, `ALIAS/BUCKET[/PREFISSO]=CHIAVEBASE64` separate da virgola (vedi [KMS e SSE](../security/kms-sse.md)) |
| `NO_COLOR` | disattiva i colori |
| `SSL_CERT_FILE`, `SSL_CERT_DIR` | CA fidate per i server HTTPS con una CA privata |
| `HTTPS_PROXY`, `NO_PROXY` | proxy HTTP verso i server |

## Codici di uscita

| Codice | Significato |
|--------|-------------|
| `0` | successo |
| `1` | errore generico (rete, server, operazione fallita) |
| `2` | uso errato: argomenti o opzioni non validi |
| `3` | risorsa non trovata (alias, bucket, oggetto, utente, …) |
| `4` | accesso negato (credenziali o permessi) |
| `130` | interrotto (Ctrl-C) |

I codici fanno parte del contratto della CLI e non cambiano tra le versioni.

## Alias dei comandi

Molti sottocomandi hanno un nome breve, riportato nella sezione *Aliases* della loro pagina: per esempio
`ls` per `list` e `rm` per `remove` (`nexoctl admin user ls`, `nexoctl event rm`).

## Completamento della shell

```bash
nexoctl completion bash > /etc/bash_completion.d/nexoctl     # bash
nexoctl completion zsh  > "${fpath[1]}/_nexoctl"             # zsh
nexoctl completion fish > ~/.config/fish/completions/nexoctl.fish
```

## Generare la documentazione

Le pagine di [`docs/user/cli`](../cli/nexoctl.md) sono generate da `nexoctl` stesso e non vanno modificate
a mano. Dopo aver cambiato un comando:

```bash
make docs         # rigenera docs/user/cli (comando nascosto: nexoctl gendocs DIR)
make docs-check   # fallisce se le pagine nel repository non sono aggiornate (gira in CI)
make docs-site    # sito completo in bin/site: MkDocs (--strict) + riferimento Admin API
```

`make docs-site` richiede `mkdocs` e `mkdocs-material` (`pip install mkdocs==1.6.1 mkdocs-material==9.7.7`) e,
per il riferimento dell'Admin API, Node.js (`npx`).
