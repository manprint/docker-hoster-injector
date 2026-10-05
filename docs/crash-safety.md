# Scrittura di /etc/hosts, arresto e crash

## Le due modalità di montaggio

L'agente deve poter riscrivere `/etc/hosts`. Esistono due strategie, con
protezioni diverse.

**Opzione A — mount del singolo file (minor privilegio, default dell'immagine, `docker run`)**

```yaml
volumes:
  - /etc/hosts:/etc/hosts
environment:
  HOSTS_MOUNT_MODE: file      # default
```

Il contenuto nuovo viene scritto con **una singola `write(2)`** sopra il
vecchio e poi ritagliato. `rename(2)` non è utilizzabile: su un bind mount
restituisce `EBUSY`. Non si tronca mai prima di scrivere, quindi la finestra
di incoerenza è di microsecondi e non può mai lasciare un file vuoto.

**Opzione B — mount della directory (atomico, usata dal `docker-compose.yml`)**

```yaml
volumes:
  - /etc:/host/etc
environment:
  HOSTS_FILE: /host/etc/hosts
  HOSTS_MOUNT_MODE: dir
```

Scrive un file temporaneo e lo rinomina: la modifica è **atomica** e, poiché il
bind mount segue la directory e non l'inode, **continua a funzionare anche se
qualcosa sull'host sostituisce `/etc/hosts` con un rename**. Il prezzo è esporre
`/etc` in scrittura al container (l'agente crea e rimuove solo i propri file
temporanei `.hosts-docker-hoster-injector-*`). Per questo il compose di esempio
usa questa modalità: è la più robusta. L'opzione A resta valida dove si
preferisce il minor privilegio e si accetta il limite sull'inode (vedi sotto).

Entrambe le modalità sono coperte dall'intera suite di test, crash compresi.

## Durata e crash safety

`/etc/hosts` è un file di sistema critico: se la risoluzione dei nomi si rompe,
l'host diventa inutilizzabile. Le scelte progettuali mirano a rendere ogni
scrittura innocua anche sotto crash.

### Le garanzie

1. **Il blocco gestito sta in fondo al file.** Una scrittura interrotta può
   perdere al massimo i record dei container, mai le entry dell'utente.
2. **L'ordine di scrittura è scelto per il crash.** Il contenuto è sempre scritto
   in un'unica `write(2)` sopra il vecchio. Se il nuovo è più corto, il file è
   accorciato **prima**: un crash lascia al più un blocco senza marcatore finale,
   che il recovery riconosce e ricostruisce. Accorciare dopo lascerebbe righe
   complete del vecchio contenuto dopo `END`, indistinguibili da quelle
   dell'utente e quindi conservate per sempre.
3. **Il file viene `fsync`-ato prima di riportare successo**, e in modalità
   `dir` anche la directory, così il `rename` è durevole.
4. **Ogni scrittura è idempotente**: se il blocco non cambia, il file non viene
   toccato e il suo `mtime` non cambia. Uno stato stazionario non costa I/O.
5. **`flock` consultivo** serializza più istanze dell'agente sullo stesso file.
6. **Le entry dell'utente sono preservate byte per byte**, incluse le righe
   malformate, le righe vuote finali, il CRLF, i byte non UTF-8 e le righe lunghe
   quanto si vuole (nessun limite). Aggiungere il blocco e poi toglierlo è un
   **round trip esatto**: verificato anche con fuzzing. L'unica differenza
   ammessa è un `\n` finale, se il file non lo aveva.
7. **Permessi, proprietario e symlink del file sono preservati** nella modalità
   `dir`. Un `/etc/hosts` che è un link simbolico resta un link e si scrive il
   suo bersaglio.
8. **Nessun file temporaneo resta in giro**: quelli lasciati da un processo
   ucciso a metà (`.hosts-docker-hoster-injector-*`, più vecchi di un minuto)
   sono rimossi al successivo avvio.

### Cosa fa il recovery

Al riavvio l'agente ispeziona il file. Un blocco lasciato aperto (senza
marcatore finale: l'impronta di un crash) viene **ricostruito**, non esteso, così
i record orfani non sopravvivono. Del blocco aperto si scartano solo
l'intestazione generata e le righe i cui nomi stanno sotto `DNS_SUFFIX`:
**qualsiasi altra riga resta**, perché lo stesso aspetto lo ha un file in cui
qualcuno ha cancellato a mano il marcatore finale e ha continuato a scrivere.

```bash
docker restart docker-hoster-injector
```

### Comportamenti da conoscere

- **Mount del singolo file (`file`) e inode** (il compose di esempio non lo usa). Un bind mount segue l'inode, non il
  percorso: se qualcosa sull'host *sostituisce* `/etc/hosts` con un rename
  (alcuni editor, `sed -i`, certi tool di provisioning), il container continua a
  scrivere sul vecchio file e l'host non vede più i record. In quel caso
  `docker restart docker-hoster-injector`, oppure usare la modalità `dir`.
- **Il socket `:ro` non limita l'API.** `:ro` impedisce di modificare il file del
  socket, non le richieste che vi passano.
- **Dopo un riavvio del daemon Docker** lo stream di eventi viene riaperto da
  solo, con backoff, e ogni riconnessione innesca una riconciliazione.
- **Ogni chiamata a Docker ha un timeout** (15 s): un daemon che non risponde non
  blocca la scrittura del file per sempre.

### Il limite, dichiarato

Se **un altro processo** svuota il file o cancella entry dell'utente, un
processo nuovo non ha copia di quei dati e non può recuperarli. Non viene
inventato nulla: si perdono i record dei container, che verranno ricreati al
prossimo ciclo, ma l'agente non finge di ripristinare contenuto che non ha mai
visto. Questa distinzione è codificata nei test: i danni propri dell'agente
sono recuperabili, quelli esterni no.

## Arresto e uscite forzate

Un record che nessuno tiene più aggiornato prima o poi punta all'indirizzo di un
altro container. Per questo il blocco **non sopravvive** al processo che lo
mantiene.

| Come finisce l'agente | Cosa succede al file |
|---|---|
| `SIGTERM` (`docker stop`, systemd), `SIGINT` (Ctrl-C), `SIGHUP` | Il blocco viene **rimosso**; il file torna quello dell'operatore. Exit code 0 |
| Segnali ripetuti durante la pulizia | Assorbiti: la pulizia non viene interrotta |
| Errore fatale all'avvio (es. Docker non raggiungibile) | Il file viene aperto *prima* di contattare Docker, quindi un blocco rimasto da un'esecuzione precedente viene rimosso comunque |
| `panic` in qualunque goroutine | Lo stack viene registrato, l'agente si ferma e rimuove il blocco; exit code 1 |
| Arresto che non finisce | Dopo 8 s il processo esce da solo (sotto i 10 s di grazia di Docker), invece di ricevere un `SIGKILL` a metà scrittura |
| `SIGKILL`, OOM killer, perdita di alimentazione | **Nessun codice può girare**: il blocco resta. Il successivo avvio lo ripara e lo riallinea (nessun duplicato, nessuna riga spuria), oppure si usa `docker-hoster-injector clean` |

La rimozione è **idempotente**: farla due volte, o su un file senza blocco o
inesistente, non cambia nulla (nemmeno l'`mtime`) e non crea il file.

Se qualcosa riscrive il file mentre l'agente è fermo, l'agente non ne sa nulla:
alla ripartenza il file viene riletto e il blocco ricostruito sopra ciò che c'è.

[← README](../README.md)
