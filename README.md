# docker-hoster-injector

Rende raggiungibili i container Docker dall'host con un nome simbolico: un
container chiamato `nginx` diventa `nginx.docker.local`, e le porte pubblicate
funzionano come sull'host (`nginx.docker.local:8080` se il container è avviato
con `-p 8080:80`).

L'agente osserva i container e mantiene **un blocco dedicato** di `/etc/hosts`
dell'host. Le righe dell'utente non vengono mai toccate, e quando l'agente si
ferma **il file torna identico, byte per byte, a com'era prima**.

Funziona su qualunque Linux con Docker (Debian/Ubuntu, RHEL/Fedora, Alpine, con
o senza `systemd`): non dipende dal resolver, usa `/etc/hosts`.

## Avvio rapido

```bash
git clone https://github.com/manprint/docker-hoster-injector.git
cd docker-hoster-injector
docker compose up -d
```

L'immagine è pubblicata su GitHub Container Registry
(`ghcr.io/manprint/docker-hoster-injector`, `linux/amd64` e `linux/arm64`).

Poi, per provare:

```bash
docker run -d --name nginx -p 8081:80 nginx:alpine
curl http://nginx.docker.local:8081/
```

La pagina di monitoraggio è su <http://127.0.0.1:8080>. Per fermare e restituire
il file: `docker compose down`.

## Requisiti

- Linux con Docker Engine **19.03 o successivo** (API 1.40+). Un daemon più
  vecchio viene rifiutato con un messaggio chiaro. I dettagli sulle versioni
  provate sono in [docs/development.md](docs/development.md#compatibilità-con-docker-più-vecchi).
- Docker rootless: impostare `DOCKER_HOST`.

## Installazione

### Docker Compose (consigliato)

Il `docker-compose.yml` incluso monta il socket Docker e la directory `/etc`
(modalità `dir`), toglie ogni capability, imposta il filesystem in sola lettura
e lega la pagina di monitoraggio a loopback. Basta `docker compose up -d`. Per
costruire l'immagine da questa copia del codice invece di scaricarla:
`docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build`.

### Docker CLI

```bash
docker run -d --name docker-hoster-injector \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /etc:/host/etc \
  -e HOSTS_FILE=/host/etc/hosts -e HOSTS_MOUNT_MODE=dir \
  -p 127.0.0.1:8080:8080 \
  ghcr.io/manprint/docker-hoster-injector:latest
```

In alternativa `docker build -t docker-hoster-injector:dev .` e si usa quell'immagine.

### Montare la directory, non il file

Si consiglia di montare `/etc` (modalità `dir`): il file viene sostituito con un
rename, quindi l'aggiornamento è atomico e continua a funzionare anche se
qualcosa sull'host riscrive `/etc/hosts` con un rename (editor, `sed -i`, tool di
provisioning). Montare il solo file (`-v /etc/hosts:/etc/hosts`, modalità `file`,
predefinita dell'immagine) richiede meno privilegi, ma un bind mount di file
segue l'inode: se l'host lo sostituisce, il container continua a scrivere sul
vecchio file. In quel caso basta `docker restart docker-hoster-injector`.
Dettagli in [docs/crash-safety.md](docs/crash-safety.md).

## Come funziona

1. Ascolta gli eventi del Docker Engine (`start`, `stop`, `die`, `destroy`, …).
2. Ogni `RESYNC_INTERVAL` rilegge tutto lo stato, per riparare eventi persi.
3. Riscrive il blocco in `/etc/hosts` solo quando il contenuto cambia.

Per ogni container in esecuzione si pubblicano il suo nome e i suoi alias di rete.

| Container | Record |
|---|---|
| `nginx` | `nginx.docker.local` |
| `myproject_web_1` | `myproject_web_1.docker.local` e `myproject--web--1.docker.local` |

Non vengono pubblicati i container fermi o in pausa, con rete `none` o `host`, né
nomi riservati come `localhost`. Perché `_` diventa `--` e come si risolvono i
nomi contesi: [docs/naming.md](docs/naming.md).

### `TARGET_MODE`

Con `-p 8080:80` il nome deve puntare all'**host**, non all'IP del container,
altrimenti la porta 8080 non risponderebbe.

| Valore | Comportamento |
|---|---|
| `both` *(default)* | Prima l'indirizzo dell'host, poi quello del container: `nome:8080` e `nome:80` funzionano entrambi |
| `published` | Solo l'indirizzo dell'host |
| `container-ip` | Solo l'IP del container (utile se i record devono servire altri host della rete) |

## Configurazione

Tutte le variabili sono opzionali.

| Variabile | Default | Descrizione |
|---|---|---|
| `DNS_SUFFIX` | `docker.local` | Dominio base (almeno due label) |
| `TARGET_MODE` | `both` | `both`, `published` o `container-ip` |
| `HOSTS_FILE` | `/etc/hosts` | Percorso assoluto del file da gestire |
| `HOSTS_MOUNT_MODE` | `file` | `file` o `dir`; il compose usa `dir` |
| `RESYNC_INTERVAL` | `30s` | Periodo della riconciliazione completa |
| `EVENT_DEBOUNCE` | `250ms` | Coalescenza dei burst di eventi |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `json` | `json` o `text` |
| `WEB_ENABLED` | `true` | Abilita la pagina di monitoraggio |
| `WEB_ADDR` | `:8080` | Indirizzo di ascolto dentro il container |
| `WEB_EVENTS` | `true` | Aggiornamento via SSE (`false`: polling) |
| `DOCKER_HOST` | — | Passato al client Docker (rootless) |

Una configurazione non valida impedisce l'avvio e riporta **tutti** i campi
errati insieme.

## La pagina di monitoraggio

Su `http://127.0.0.1:8080`, si aggiorna da sola: una riga per container con
stato, indirizzi IPv4, nomi e **un link cliccabile per ogni porta**, marcata
**TCP** o **UDP**. Le porte pubblicate si aprono con il nome e la porta
dell'host, quelle solo esposte con l'indirizzo del container. Le porte che non
parlano HTTP (database, SSH, …) e le UDP sono mostrate ma non sono link. Su
schermi stretti le righe diventano schede.

Non ha autenticazione: è **sola lettura** (ogni metodo diverso da `GET`/`HEAD`
riceve `405`) e va tenuta su loopback. Rotte, regole dei link e sicurezza della
pagina: [docs/web-ui.md](docs/web-ui.md).

## Arresto e comandi

Alla chiusura ordinata (`docker stop`, `SIGTERM`, `SIGINT`, `SIGHUP`) l'agente
**toglie il suo blocco** da `/etc/hosts`. Se viene ucciso (`kill -9`, OOM, perdita
di alimentazione) nessun codice può girare e il blocco resta: al riavvio viene
riparato e riallineato, oppure lo si toglie con `clean`.

```text
docker-hoster-injector [run]        mantiene il file (predefinito)
docker-hoster-injector clean        toglie il blocco e i temporanei di un'esecuzione uccisa
docker-hoster-injector healthcheck  esce con 0 se l'agente risponde 200 su /healthz
docker-hoster-injector version
```

`clean` non richiede Docker né un agente in esecuzione e si può ripetere:

```bash
docker run --rm -v /etc:/host/etc -e HOSTS_FILE=/host/etc/hosts -e HOSTS_MOUNT_MODE=dir \
  ghcr.io/manprint/docker-hoster-injector:latest clean
```

L'immagine ha un `HEALTHCHECK` (il binario è la propria sonda, non ci sono shell né
curl); un agente la cui ultima scrittura è fallita risulta `unhealthy`. Le garanzie
complete sul file e sulle uscite: [docs/crash-safety.md](docs/crash-safety.md).

## Sicurezza

- **Il socket Docker equivale a root sull'host.** È montato `:ro`, ma `:ro` non
  limita le richieste che vi passano: non esporlo oltre il necessario.
- La pagina di monitoraggio rivela nomi, indirizzi e porte e non ha
  autenticazione: tienila su `127.0.0.1`.
- Il container non ha capability (`cap_drop: ALL`, `no-new-privileges`, root
  filesystem in sola lettura); il binario è statico e gira su `scratch`.
- Con la modalità `dir` il container vede `/etc` in scrittura. L'agente crea e
  rimuove solo i propri file `.hosts-docker-hoster-injector-*`.
- Se l'utente non è nel gruppo `docker`: `--group-add "$(getent group docker | cut -d: -f3)"`.

## Risoluzione dei problemi

**Il nome non risolve.**

```bash
getent hosts nginx.docker.local               # come farebbe curl o il browser
grep -A20 'BEGIN docker-hoster-injector' /etc/hosts   # il record è nel file?
docker logs docker-hoster-injector            # l'agente è vivo?
grep '^hosts:' /etc/nsswitch.conf             # include "files"?
```

Alcuni resolver (provider con ricerca per dominio o risposta jolly) rispondono a
qualunque nome: se `getent` risolve ma il nome non è nel blocco, la risposta viene
dal DNS, non da questo agente.

**Funziona con `ping` ma non con `curl`.** Con `nss-myhostname` o ricerca `ndots`
il DNS può avere la precedenza. Il record è comunque in `/etc/hosts`: verifica
con `getent hosts`.

**I record spariscono dopo una modifica di `/etc/hosts` dall'host.** Stai usando il
mount del singolo file e l'host ha sostituito il file: passa alla modalità `dir`
oppure `docker restart docker-hoster-injector`.

**Un container non compare.** Deve essere `running`, non avere rete `none`/`host`
e un nome non riservato. Il motivo è nella tabella «Not published» della pagina e
nei log (campo `container`).

**`/etc/hosts` non scrivibile.** L'agente non si ferma: registra l'errore e riprova
al prossimo evento o resync (`docker logs docker-hoster-injector | grep -i
'write\|permission'`).

**Dopo un crash o un `kill -9`.** `docker restart docker-hoster-injector` ripara il
blocco; per toglierlo senza riavviare l'agente usa `clean` (vedi sopra).

## Documentazione tecnica

| | |
|---|---|
| [docs/naming.md](docs/naming.md) | Come si formano i nomi, collisioni, perché `_` diventa `--` |
| [docs/web-ui.md](docs/web-ui.md) | Rotte, regole dei link, sicurezza della pagina |
| [docs/crash-safety.md](docs/crash-safety.md) | Modalità di montaggio, garanzie di scrittura, recovery, uscite |
| [docs/architecture.md](docs/architecture.md) | Struttura del codice e scelte implementative |
| [docs/development.md](docs/development.md) | Build, test, CI, compatibilità con Docker più vecchi |

## Licenza

Da definire.
