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
> avvia una quindicina di container reali in tutte le configurazioni
> significanti, verifica la risoluzione dal vivo e il recovery dopo `kill -9`,
> poi ripristina `/etc/hosts`. `sudo make test-integration` per eseguirla.

---

## Indice

- [Come funziona](#come-funziona)
- [Requisiti](#requisiti)
- [Installazione](#installazione)
- [Configurazione](#configurazione)
- [Riferimento dei nomi pubblicati](#riferimento-dei-nomi-pubblicati)
- [Sicurezza](#sicurezza)
- [Durata e crash safety](#durata-e-crash-safety)
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
2. **Non si tronca mai prima di scrivere.** Nella modalità `file` il contenuto
   completo viene scritto sopra il vecchio e solo dopo ritagliato. Un crash può
   lasciare byte del testo precedente in coda, mai un buco o un file azzerato.
3. **Il file viene `fsync`-ato prima di riportare successo**, e in modalità
   `dir` anche la directory, così il `rename` è durevole.
4. **Ogni scrittura è idempotente**: se il blocco non cambia, il file non viene
   toccato e il suo `mtime` non cambia. Uno stato stazionario non costa I/O.
5. **`flock` consultivo** serializza più istanze dell'agente sullo stesso file.
6. **Le entry dell'utente sono preservate byte per byte**, incluse le righe
   malformate, che vengono conservate invece di essere "pulite".

### Cosa fa il recovery

Al riavvio l'agente ispeziona il file e interviene se trova un blocco non
terminato (l'impronta di un crash), un file mancante o vuoto. Un blocco
lasciato aperto viene **ricostruito**, non esteso, così i record orfani non
sopravvivono.

```bash
docker restart docker-hoster-injector
```

### Il limite, dichiarato

Se **un altro processo** svuota il file o cancella entry dell'utente, un
processo nuovo non ha copia di quei dati e non può recuperarli. Non viene
inventato nulla: si perdono i record dei container, che verranno ricreati al
prossimo ciclo, ma l'agente non finge di ripristinare contenuto che non ha mai
visto. Questa distinzione è codificata nei test: i danni propri dell'agente
sono recuperabili, quelli esterni no.

---

## Sviluppo

```bash
make help          # elenco dei target
make build         # binario in ./bin
make test          # test unitari
make test-race     # test unitari con race detector
make cover         # copertura
make lint          # go vet + staticcheck
make test-integration
make image
```

Toolchain richiesta: **Go 1.25 o successivo**.

---

## Test

```bash
make test              # unitari, veloci
make test-race         # obbligatorio: la concorrenza è il cuore del progetto
make test-integration  # crash, concorrenza e naming, dietro build tag
```

Copertura attuale: **~77%** delle dichiarazioni (`version` 100%, `reconcile`
94%, `config` 93%, `naming` 93%, `apply` 91%, `webui` 90%, `watcher` 89%,
`hostsfile` 88%). `dockerclient` è più bassa di proposito: quasi tutto quel
package parla con il daemon, ed è coperto dai test di accettazione piuttosto
che da mock.

### Cosa coprono

| Area | Cosa è verificato |
|---|---|
| Configurazione | Default, override, valori vuoti, normalizzazione, **tutti gli errori in un colpo** |
| Naming | Corpus di nomi reali (Compose v1/v2, Swarm, k8s, non-ASCII, 300 caratteri, metacaratteri), idempotenza, stabilità, collisioni |
| Parsing hosts | Classificazione righe, byte-per-byte, CRLF, righe malformate, blocchi orfani |
| Scrittura | Idempotenza, preserva entry utente, entrambe le modalità, concorrenza, assenza di file temporanei, `flock` |
| **Crash** | `SIGKILL` reale durante le scritture, 15 round per modalità, recovery da ogni forma di danno |
| Iniezione | Un nome con `\n` **non può** iniettare record nel file dell'host |
| Accettazione | 15 container reali, dalla creazione al `kill -9`, con verifica HTTP reale |

### I test di accettazione

`sudo make test-integration` esegue la suite completa contro il Docker locale.
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
che il file resti utilizzabile e che un riavverso converga. Un `kill -9` vero,
non simulato: solo un vero kill lascia sul disco lo stato che conta.

```console
$ go test -tags=integration -v -run TestCrashDuringWrites ./test/integration/
--- PASS: TestCrashDuringWritesLeavesAStableFile (0.00s)
    --- PASS: TestCrashDuringWritesLeavesAStableFile/file (3.15s)
    --- PASS: TestCrashDuringWritesLeavesAStableFile/dir (3.16s)
```

### CI

`.github/workflows/ci.yml` esegue:

- **lint** — gofmt, `go vet`, staticcheck, test con race detector, build;
- **integration** — matrix Docker **25, 26, 27, 28, 29** (dind);
- **rhel** — smoke test su Fedora, dove `/etc/resolv.conf` è gestito da
  NetworkManager e non da systemd-resolved;
- **image** — build e ispezione dell'immagine.

---

## Architettura

```
cmd/docker-hoster-injector/    main: segnali, lifecycle
internal/
  config/      da ambiente a configurazione validata
  naming/      nomi Docker → nomi host, sanitizzazione, collisioni
  hostsfile/   parsing, rendering, scrittura atomica, recovery
  logging/     logger strutturato
  dockerclient/ [fase 3]  client Docker, eventi, resync
  reconcile/   [fase 4]  stato desiderato e diff
  webui/       [fase 5b] interfaccia di monitoraggio
test/integration/              test con crash reali
```

Le dipendenze sono volutamente minime: solo il client Docker ufficiale, più
`golang.org/x/sys` per `flock`. Niente framework di test.

### Note di implementazione

**Il writer non si fidava dell'input.** I nomi vengono validati in uscita: un
nome contenente un `\n` potrebbe iniettare un record arbitrario nel file di
sistema dell'host. Oggi i nomi arrivano da Docker e sono puliti, ma una
garanzia che dipende da un invariante esterna che vale per sempre non è una
garanzia. Un nome non sicuro viene **scartato intero**, mai troncato: un nome
troncato punterebbe silenziosamente a qualcos'altro.

**Il sanitizzatore non può essere iniettivo** e i test lo documentano
esplicitamente invece di nasconderlo, resorting a mostrare la proprietà che
conta (ogni container mantiene un nome unico).

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

L'agente non muore: riprova con backoff e registra l'errore. In un container
senza permessi è sufficiente aggiungere `--user`. Verificare con:

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

---

## Licenza

Da definire.
