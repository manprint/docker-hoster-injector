# docker-hoster-injector

Rende raggiungibili i container Docker dall'host tramite un nome simbolico:
un container chiamato `nginx` diventa `nginx.docker.local`, con le porte
pubblicate che funzionano come sull'host (`nginx.docker.local:8080` se il
container è avviato con `-p 8080:80`).

L'agente osserva il ciclo di vita dei container e mantiene una sezione
dedicata di `/etc/hosts` dell'host. **Non tocca mai le entry dell'utente.**

Funziona allo stesso modo su Debian/Ubuntu, RHEL/CentOS/Fedora e Alpine,
con o senza `systemd`, perché non dipende dal resolver: la risoluzione passa
per `nss-files`, presente ovunque.

> **Stato** — Funzionante e verificato end to end. La suite di accettazione
> avvia una ventina di container reali in tutte le configurazioni
> significative, verifica la risoluzione dal vivo, ogni modo di uscita
> (`SIGTERM`, `SIGINT`, `SIGHUP`, `docker stop`, `kill -9`) e la web UI in un
> browser vero (Playwright), poi ripristina `/etc/hosts`.
> `sudo make test-integration` e `make test-e2e` per eseguirle.

**Alla chiusura l'agente toglie i suoi record dal file**: dopo un arresto
ordinato `/etc/hosts` torna identico, byte per byte, a com'era prima.

---

## Indice

- [Come funziona](#come-funziona)
- [L'interfaccia di monitoraggio](#l-interfaccia-di-monitoraggio)
- [Requisiti](#requisiti)
- [Installazione](#installazione)
- [Configurazione](#configurazione)
- [Riferimento dei nomi pubblicati](#riferimento-dei-nomi-pubblicati)
- [Sicurezza](#sicurezza)
- [Durata e crash safety](#durata-e-crash-safety)
- [Arresto e uscite forzate](#arresto-e-uscite-forzate)
- [Comandi](#comandi)
- [Sviluppo](#sviluppo)
- [Test](#test)
- [Architettura](#architettura)
- [Risoluzione dei problemi](#risoluzione-dei-problemi)
- [Licenza](#licenza)

---

## Come funziona

```
┌──────────────┐   /events + list    ┌─────────────────────┐
│ Docker Engine│◄────────────────────│                    │
└──────────────┘                     │  docker-hoster-    │
                                     │  injector          │
┌──────────────┐   write             │                    │
│ /etc/hosts   │◄────────────────────│                    │
│ dell'host    │  (blocco gestito)   └─────────────────────┘
└──────────────┘
```

1. Ascolta gli eventi del Docker Engine (`start`, `stop`, `die`, `destroy`, …).
2. Ricalcola in modo completo la lista dei container ogni `RESYNC_INTERVAL`, per
   riparare qualsiasi evento perso.
3. Aggiorna un blocco delimitato in `/etc/hosts` dell'host, riscrivendolo solo
   quando il contenuto cambia davvero.

### La questione delle porte

Con `-p 8080:80 nginx`, il nome `nginx.docker.local` **non** può puntare
all'IP del container: l'host chiederebbe la porta 8080 *dentro* il container,
dove non c'è nulla. Perché `nginx.docker.local:8080` funzioni come da specifica,
il nome deve risolvere dal lato host, dove vale la regola di port publishing.

`TARGET_MODE` controlla questo comportamento:

| Valore | Comportamento | Quando usarlo |
|---|---|---|
| `both` *(default)* | Prima l'indirizzo raggiungibile dall'host, poi quello del container | Caso normale: soddisfa la spec e permette anche l'accesso diretto alle porte interne |
| `published` | Solo indirizzo raggiungibile dall'host | Se si vuole essere espliciti |
| `container-ip` | Solo IP del container | Quando i record devono risolvere **da altri host** della rete |

Con `both`, un resolver restituisce più indirizzi e i client (curl, Go, browser)
li provano in ordine: `nginx.docker.local:8080` risolve subito, e
`nginx.docker.local:80` funziona comunque sul secondo indirizzo.

---

## L'interfaccia di monitoraggio

Su `http://127.0.0.1:8080` c'è una pagina con la tabella dei record
pubblicati, che si aggiorna da sola.

| Rotta | Contenuto |
|---|---|
| `GET /` | La pagina, con lo snapshot già incorporato: la tabella è popolata al primo rendering |
| `GET /api/entries` | Lo stesso dato in JSON, per script e per il fallback |
| `GET /api/config` | La configurazione della distribuzione |
| `GET /api/events` | Stream SSE: ogni messaggio è uno snapshot completo. Assente con `WEB_EVENTS=false`: la pagina interroga allora `/api/entries` ogni 5 s |
| `GET /healthz` | `503` se l'ultima applicazione è fallita; è anche ciò che interroga l'`HEALTHCHECK` dell'immagine |
| `GET /metrics` | Contatori in formato Prometheus |

### I link di accesso

L'ultima colonna, **Open**, elenca le porte raggiungibili attraverso quell'indirizzo,
come link cliccabili che si aprono in una nuova scheda:

| Riga | Link | Perché |
|---|---|---|
| Indirizzo dell'host (`127.0.0.1`, o l'interfaccia scelta con `-p IP:…`) | `http://<nome>:<porta pubblicata>/` | È lo scopo del nome: `nginx.docker.local:8080` funziona come `localhost:8080` |
| Indirizzo del container | `http://<indirizzo>:<porta del container>/` | Si usa l'**indirizzo** e non il nome, perché in modalità `both` il nome risolve prima sull'host, e un servizio dell'host sulla stessa porta risponderebbe al posto del container. Con `TARGET_MODE=container-ip` il nome risolve solo sul container e si usa il nome |

Regole:

- Le porte `443`, `4443`, `8443`, `9443` sono `https`; le altre `http`.
- Le porte di protocolli noti come non web (SSH, database, broker, …) e le porte
  UDP **si vedono ma non sono link**. Il protocollo si giudica dalla porta
  *interna* del servizio: un Postgres pubblicato su `49153` resta un database.
- Docker conosce solo le porte **dichiarate**: pubblicate (`-p`) o esposte
  (`EXPOSE`, `--expose`). Un server che ascolta su una porta mai dichiarata non
  compare, e non c'è modo di scoprirlo da fuori.
- I link si compongono nel server e il browser li accetta solo se iniziano per
  `http://` o `https://`: un valore diverso (`javascript:`, `data:`) viene
  mostrato come testo e mai come link. Hanno `rel="noopener noreferrer"`.
- Un container in pausa non ha record: ha un indirizzo ma non risponde, e un
  nome che risolve e poi si blocca è peggio di un nome che non risolve.

Ogni messaggio SSE è uno **snapshot completo**, non un diff: il payload è
piccolo e un client che si riconnette in qualsiasi momento è subito corretto,
senza dover riprodurre una cronologia che potrebbe aver perso.

La pagina è interamente autosufficiente: foglio di stile e logica sono
incorporati, nessuna richiesta esterna. Serve perché il servizio gira spesso
su reti isolate.

Ogni risposta porta una `Content-Security-Policy` restrittiva (niente risorse
esterne, niente frame), `X-Content-Type-Options: nosniff` e
`Referrer-Policy: no-referrer`.

**Non ha autenticazione**, come richiesto. È accettabile solo perché è
**read-only**: qualsiasi metodo diverso da `GET` e `HEAD` riceve `405`, quindi
non esiste una rotta che possa cambiare qualcosa. Va comunque legata a
loopback.

## Requisiti

- Linux con Docker Engine **25 o successivo** (API 1.44+). Il client effettua
  la negoziazione automatica della versione, quindi le versioni più recenti
  funzionano senza configurazione.
- Funziona anche con **Docker rootless** impostando `DOCKER_HOST`.
- Nessuna dipendenza Go oltre al solo client Docker ufficiale.

---

## Installazione

### Docker Compose (consigliato)

```bash
cp docker-compose.yml docker-compose.override.yml
docker compose up -d
```

Il compose di esempio è già configurato con le impostazioni sicure: capability
droppate, `no-new-privileges`, root filesystem read-only, web UI legata solo a
loopback.

### Docker CLI

```bash
docker run -d --name docker-hoster-injector \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /etc/hosts:/etc/hosts \
  -e DNS_SUFFIX=docker.local \
  -p 127.0.0.1:8080:8080 \
  docker-hoster-injector:dev
```

### Le due modalità di montaggio

L'agente deve poter riscrivere `/etc/hosts`. Esistono due strategie, con
protezioni diverse.

**Opzione A — mount del singolo file (minor privilegio, default)**

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

**Opzione B — mount della directory (atomico)**

```yaml
volumes:
  - /etc:/host/etc
environment:
  HOSTS_FILE: /host/etc/hosts
  HOSTS_MOUNT_MODE: dir
```

Scrive un file temporaneo e lo rinomina: la modifica è **atomica**. Il prezzo
è esporre `/etc` in scrittura al container. Da usare quando la priorità è
l'atomicità assoluta e si accetta l'ampiezza del montaggio.

Entrambe le modalità sono coperte dall'intera suite di test, crash compresi.

---

## Configurazione

Tutte le variabili sono opzionali. Un valore vuoto equivale a non impostato e
ripiega sul default.

| Variabile | Default | Descrizione |
|---|---|---|
| `DNS_SUFFIX` | `docker.local` | Dominio base. Deve avere almeno due label validi |
| `HOSTS_FILE` | `/etc/hosts` | Path assoluto del file da gestire |
| `HOSTS_MOUNT_MODE` | `file` | `file` oppure `dir` (vedi sopra) |
| `TARGET_MODE` | `both` | `both`, `published` o `container-ip` |
| `RESYNC_INTERVAL` | `30s` | Periodo della riconciliazione completa |
| `EVENT_DEBOUNCE` | `250ms` | Coalescenza dei burst di eventi |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `json` | `json` oppure `text` |
| `WEB_ENABLED` | `true` | Abilita il server di monitoraggio |
| `WEB_ADDR` | `:8080` | Indirizzo di ascolto dentro il container |
| `WEB_EVENTS` | `true` | Canale SSE (disattiva per il polling) |
| `DOCKER_HOST` | — | Passato al client Docker (rootless) |

Una configurazione non valida **non** avvia il processo e riporta **tutti** i
campi errati in una volta sola:

```console
$ DNS_SUFFIX='!!' TARGET_MODE=sideways LOG_LEVEL=loud docker-hoster-injector
docker-hoster-injector: DNS_SUFFIX: must contain at least two labels, e.g. docker.local
TARGET_MODE="sideways": must be one of: both, published, container-ip
LOG_LEVEL="loud": must be one of: debug, info, warn, error
```

---

## Riferimento dei nomi pubblicati

Per ogni container in esecuzione vengono pubblicati il nome del container e
tutti gli alias di rete. Ogni nome è offerto in due forme quando differiscono:

| Container | Record pubblicati |
|---|---|
| `nginx` | `nginx.docker.local` |
| `myproject_web_1` | `myproject_web_1.docker.local`, `myproject--web--1.docker.local` |

### Perché l'underscore diventa un doppio trattino

La sostituzione naturale sarebbe `_` → `-`, ma **è sbagliata**: Compose genera
nomi come `web_1` e `web-1` che sono container distinti, e con quella mappa
finirebbero con lo stesso record, sovrascrivendosi a vicenda. Il traffico
finirebbe sul container sbagliato, che è il modo peggiore in cui può rompersi.

`_` diventa quindi `--`, e i trattini esistenti vengono preservati: i nomi
distinti restano distinti.

```
web_1       →  web--1
web-1       →  web-1        (invariato)
proj_web_1  →  proj--web--1
proj-web-1  →  proj-web-1
```

### Sui limiti di questa garanzia

Docker ammette `[a-zA-Z0-9][a-zA-Z0-9_.-]*`, mentre un host name RFC 1123
ammette solo `[a-z0-9-]`: l'alfabeto di uscita è più piccolo di quello di
ingresso, quindi **nessuna sanitizzazione può essere iniettiva**. `a__b` e
`a--b` finiscono inevitabilmente sullo stesso nome.

Ciò che garantisce il progetto è la proprietà che conta davvero:

- ogni container pubblica **sempre il proprio nome grezzo**, che Docker
  garantisce unico, quindi nessun container resta irraggiungibile;
- ogni nome conteso viene assegnato in modo **deterministico** (priorità alla
  creazione più antica, poi ID minimo), quindi il file non "sbatte" tra un
  riavvio e l'altro;
- i nomi riservati (`localhost`, `broadcasthost`, `ip6-*`, …) vengono rifiutati,
  perché sovrascriverli romperebbe la risoluzione dell'host stesso.

### Nomi esclusi

- Non pubblicati: container fermi, in `paused`, con rete `none` o `host`.
- Non pubblicati se sono già nel loro ciclo di morte (`dead`, `removing`).
- Esclusi i nomi riservati del sistema.

---

## Sicurezza

- **Il socket Docker è equivalente a root sull'host.** Chi vi accede può
  montare qualsiasi volume. È montato in sola lettura (`ro`), ma va trattato
  come una superficie privilegiata: non esporlo oltre il necessario.
- L'API web **non ha autenticazione**, come richiesto. Per compensare è
  read-only e pubblica solo nomi, indirizzi e porte. Va comunque legata a
  loopback (`127.0.0.1:8080:8080`), perché rivela informazioni sulla topologia
  interna.
- Il container non ha bisogno di capacità Linux: `cap_drop: ALL` +
  `no-new-privileges` + root filesystem read-only sono applicati nel compose.
- Il binario è statico (`CGO_ENABLED=0`) e gira su `scratch`: nessuna shell,
  nessuna libreria da mantenere.

```bash
# Se l'utente non è nel gruppo docker:
--group-add "$(getent group docker | cut -d: -f3)"
```

---

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

- **Mount del singolo file (`file`) e inode.** Un bind mount segue l'inode, non il
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

---

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

## Comandi

```text
docker-hoster-injector [run]        mantiene il file (predefinito)
docker-hoster-injector clean        toglie il blocco e i temporanei lasciati da un'esecuzione uccisa
docker-hoster-injector healthcheck  esce con 0 se l'agente in esecuzione risponde 200 su /healthz
docker-hoster-injector version
```

`clean` non richiede né Docker né un agente in esecuzione, e si può lanciare più
volte. Con l'immagine:

```bash
docker run --rm -v /etc/hosts:/etc/hosts docker-hoster-injector:dev clean
```

L'immagine definisce un `HEALTHCHECK` che usa `healthcheck`: non ha shell né curl,
quindi il binario è la propria sonda. Un agente la cui ultima scrittura è fallita
risponde `503` ed è segnalato `unhealthy`.

---

## Sviluppo

```bash
make help          # elenco dei target
make build         # binario in ./bin
make test          # test unitari
make test-race     # test unitari con race detector
make cover         # copertura
make lint          # go vet + golangci-lint (staticcheck, errcheck, govet, revive, gosec)
make fuzz          # fuzzing del round trip del file hosts (30 s)
sudo make test-integration
make test-e2e      # test del browser sulla web UI (Playwright)
make image
```

Le regole di stile e di robustezza che il progetto si impone sono in
[`BestPractice.md`](BestPractice.md); il set di linter che le verifica è in
`.golangci.yml`. Le direttive `//nolint` portano sempre la motivazione.

Toolchain: il progetto dichiara `go 1.25` come minimo ed è sviluppato e testato con
**Go 1.27** (riga `toolchain` di `go.mod`, immagine `golang:1.27-alpine`).

---

## Test

```bash
make test              # unitari, veloci
make test-race         # obbligatorio: la concorrenza è il cuore del progetto
make test-integration  # crash, uscite, immagine, web UI, estate di container (build tag, root)
make test-e2e          # web UI in un browser vero (Playwright + Chrome di sistema)
make fuzz              # round trip del file hosts
```

Copertura attuale dei test unitari: **~85%** delle dichiarazioni (`version` 100%,
`reconcile` 95%, `naming` 93%, `config` 93%, `webui` 93%, `watcher` 92%, `apply`
91%, `hostsfile` 86%, `agent` 78%, `dockerclient` 60%). `dockerclient` è più bassa perché quasi
tutto quel package parla con il daemon; la parte più delicata, lo stream di
eventi, è però coperta da un finto daemon HTTP nei test unitari: un test di
accettazione non basta a vedere uno stream che si chiude senza avvisare.

### Cosa coprono

| Area | Cosa è verificato |
|---|---|
| Configurazione | Default, override, valori vuoti, normalizzazione, **tutti gli errori in un colpo** |
| Naming | Corpus di nomi reali (Compose v1/v2, Swarm, k8s, non-ASCII, 300 caratteri, metacaratteri), idempotenza, stabilità, collisioni |
| Parsing hosts | Classificazione righe, byte-per-byte, CRLF, righe malformate, blocchi orfani |
| Scrittura | Idempotenza, preserva entry utente, entrambe le modalità, concorrenza, assenza di file temporanei, `flock` |
| **Round trip** | Aggiungi + togli = byte originali (CRLF, righe vuote finali, senza `\n` finale, byte non UTF-8, riga da 6 MB), con **fuzzing**; nessun residuo dopo `END`; symlink e permessi preservati; pulizia dei temporanei |
| **Ciclo di vita** | `internal/agent` con un Docker finto: arresto rimuove il blocco, riavvio dopo kill converge, panic, errore all'avvio, `clean` idempotente |
| Link della web UI | Porta pubblicata, porta del container, IPv6, container-ip, porte non web e UDP, nome preferito, escape di `</script>` |
| **Crash** | `SIGKILL` reale durante le scritture, 15 round per modalità, recovery da ogni forma di danno |
| Iniezione | Un nome con `\n` **non può** iniettare record nel file dell'host |
| Accettazione | ~20 container reali, dalla creazione al `kill -9`, con verifica HTTP reale |
| **Uscite** | Processo vero: `SIGTERM`/`SIGINT`/`SIGHUP` in entrambe le modalità, segnali ripetuti, `kill -9` + riavvio, `clean`, Docker irraggiungibile, permessi del file |
| **Immagine** | `docker stop` restituisce il file all'operatore (bind mount di file e di directory), `docker kill` + `clean`/riavvio, `HEALTHCHECK` healthy |
| **Browser** | Playwright: tabella = API = file hosts, link cliccabili e funzionanti, filtro, aggiornamenti dal vivo, polling, dati ostili, schermo stretto, tema scuro, tastiera, riconnessione |

### I test di accettazione

`sudo make test-integration` esegue la suite completa contro il Docker locale.
**Modifica il vero `/etc/hosts`** (con backup, vedi sotto): va lanciata su una
macchina di sviluppo.
L'estate comprende quindici container che coprono ogni regola:

| Container | Cosa verifica |
|---|---|
| `dhi-nginx -p N:80` | Il caso della specifica: porta pubblicata e porta diretta |
| `dhi-nginx-hostport -p 127.0.0.1:N:80` | Bind esplicito su loopback |
| `dhi-nginx-multi -p N:80 -p M:80` | Due porte pubblicate sotto un solo nome |
| `dhi-python -p N:8000` | Porta non standard |
| `dhi-busy -p N:8080` | Altro server, altra porta |
| `dhi-compose_web_1` | Nome con underscore: forma grezza e sanitizzata |
| `dhi-alias-svc` | Alias di rete (`--network-alias`) |
| `dhi-noports -P` | Porta esposta ma non pubblicata |
| `dhi-noalias` | Nessuna porta pubblicata |
| `dhi-weird_name` | Caratteri illegali in un host name |
| `dhi-created` | Creato ma non avviato |
| `dhi-stopped` | Avviato e poi fermato |
| `dhi-hostnet --network host` | **Deve essere escluso** |
| `dhi-nonet --network none` | **Deve essere escluso** |
| `localhost` | **Nome riservato: deve essere escluso** |

Altri container, creati dai singoli test, coprono UDP, una porta di database, una
porta TLS, un container su due reti, un container in pausa e due container che
vogliono lo stesso alias. In tutto, oltre venti configurazioni.

La suite verifica che i nomi risolvano davvero (`getent ahostsv4`), che le
porte giuste rispondano `HTTP 200`, che il ciclo di vita segua Docker, che
l'agente sopravviva a un `kill -9`, e che la web UI rifletta lo stato.

**Nota sulla risoluzione.** Su molte reti il resolver del provider risponde
`127.0.0.1` a qualunque nome inesistente, per intercettare refusi. In quel caso
"il nome risolve" non dimostra nulla, perché la risposta arriva dal DNS e non da
`/etc/hosts`. La suite lo rileva da sola e in quel caso usa il file come
verifica.

**Sicurezza della suite.** `/etc/hosts` viene messo in backup in
`/tmp/docker-hoster-injector.hosts.backup` e ripristinato al termine, anche in
 caso di panic. Senza i permessi di scrittura la suite si rifiuta di partire:
metà delle verifiche non avrebbe significato.

### Il test di crash

Il test più importante avvia un **processo figlio** che riscrive il file in
continuo e lo termina con `SIGKILL` a un momento scelto dall'OS, poi verifica
che il file resti utilizzabile e che un riavvio converga. Un `kill -9` vero,
non simulato: solo un vero kill lascia sul disco lo stato che conta.

```console
$ go test -tags=integration -v -run TestCrashDuringWrites ./test/integration/
--- PASS: TestCrashDuringWritesLeavesAStableFile (0.00s)
    --- PASS: TestCrashDuringWritesLeavesAStableFile/file (3.15s)
    --- PASS: TestCrashDuringWritesLeavesAStableFile/dir (3.16s)
```

### CI

`.github/workflows/ci.yml` esegue:

- **lint** — gofmt, `go vet`, golangci-lint, test con race detector, build;
- **integration** — matrix Docker **25, 26, 27, 28, 29** (dind);
- **e2e** — la web UI in Chrome con Playwright, contro un agente e container veri;
- **rhel** — smoke test su Fedora, dove `/etc/resolv.conf` è gestito da
  NetworkManager e non da systemd-resolved;
- **image** — build e ispezione dell'immagine.

---

## Architettura

```
cmd/docker-hoster-injector/    main: comandi, segnali, watchdog di arresto
internal/
  agent/        ciclo di vita: avvio, esecuzione, arresto che rimuove il blocco, clean, healthcheck
  config/       da ambiente a configurazione validata
  naming/       nomi Docker → nomi host, sanitizzazione, collisioni
  hostsfile/    parsing, rendering, scrittura atomica, recovery
  logging/      logger strutturato
  version/      confronto numerico delle versioni API
  dockerclient/ l'unico package che conosce i tipi del Docker Engine
  watcher/      eventi + resync, con backoff
  reconcile/    stato desiderato, regole di inclusione, collisioni
  apply/        scrittura singola con debounce
  webui/        pagina, API JSON, link di accesso, stream SSE, metriche
test/integration/              acceptance test: crash, uscite, immagine, web UI
test/e2e/                      Playwright: la web UI in un browser vero
```

Le dipendenze sono volutamente minime: solo il client Docker ufficiale e
le sue dipendenze transitive. Il `flock` usa `syscall`. Niente framework di test.

### Note di implementazione

**Il writer non si fidava dell'input.** I nomi vengono validati in uscita: un
nome contenente un `\n` potrebbe iniettare un record arbitrario nel file di
sistema dell'host. Oggi i nomi arrivano da Docker e sono puliti, ma una
garanzia che dipende da un invariante esterna che vale per sempre non è una
garanzia. Un nome non sicuro viene **scartato intero**, mai troncato: un nome
troncato punterebbe silenziosamente a qualcos'altro.

**Il sanitizzatore non può essere iniettivo** e i test lo documentano
esplicitamente invece di nasconderlo, mostrando la proprietà che conta: ogni
container mantiene un nome proprio, e i nomi contesi sono assegnati in modo
deterministico.

**Gli alias di rete richiedono una richiesta per container.** Il Docker Engine
restituisce `null` per `Aliases` e `DNSNames` in `/containers/json`: esistono
solo in `/containers/{id}/json`. Leggerli dal primo endpoint significa pubblicare
niente, e la funzionalità si rompe in silenzio. L'agente elenca i container e poi
li ispeziona a concorrenza limitata.

**Gli eventi dicono solo che qualcosa è successo, mai cosa.** Ogni trigger
provoca una rilettura completa dello stato: trattare un evento come un fatto da
applicare sarebbe un bug che si manifesta solo al rare eventi persi. Il resync
periodico è la garanzia di correttezza, gli eventi sono solo latenza.

---

## Risoluzione dei problemi

### Il nome non risolve

```bash
getent hosts nginx.docker.local    # come farebbe curl o il browser
```

Se non restituisce nulla:

```bash
# 1. il record è nel file?
grep -A20 'BEGIN docker-hoster-injector' /etc/hosts

# 2. l'agente è vivo?
docker logs docker-hoster-injector

# 3. nsswitch include "files"?
grep '^hosts:' /etc/nsswitch.conf
```

### Funziona con `ping` ma non con `curl`

Su sistemi con `nss-myhostname` o con ricerca `ndots`, un resolver può
preferire il DNS. Il record è comunque in `/etc/hosts`; verificare con
`getent hosts`, che usa esattamente `nss-files`.

### `/etc/hosts` non scrivibile

L'agente non muore: registra l'errore e riprova al prossimo evento o resync.
Con `cap_drop: ALL` il container (root) può scrivere solo un file di cui è
proprietario: `/etc/hosts` dell'host è di root, quindi va bene, mentre un file
di un altro utente no. Verificare con:

```bash
docker logs docker-hoster-injector | grep -i 'write\|permission'
```

### Un container non compare

Controllare che sia `running` e non abbia rete `none`/`host`, e che il nome non
sia riservato (`localhost`, `ip6-*`). I dettagli sono nei log con il campo
`container`.

### Lo stato non converge dopo un crash

```bash
docker restart docker-hoster-injector
docker logs -f docker-hoster-injector
```

Al riavvio il recovery ripara i blocchi non terminati; la riconciliazione
successiva ricostruisce tutti i record.

Per togliere i record senza far ripartire l'agente (dopo un `kill -9`, o prima di
disinstallarlo):

```bash
docker run --rm -v /etc/hosts:/etc/hosts docker-hoster-injector:dev clean
```

---

## Licenza

Da definire.
