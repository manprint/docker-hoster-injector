# Sviluppo e test

```bash
make help          # elenco dei target
make build         # binario in ./bin
make test          # test unitari
make test-race     # test unitari con race detector
make cover         # copertura
make lint          # go vet + golangci-lint (staticcheck, errcheck, govet, revive, gosec)
make fuzz          # fuzzing del round trip del file hosts (30 s)
sudo make test-integration   # o: make test-integration SUDO=sudo
make test-e2e      # test del browser sulla web UI (Playwright)
make image
make smoke         # avvia il vero docker-compose.yml e verifica un nome (docker, sudo)
```

Il set di linter è in `.golangci.yml` (staticcheck, errcheck, govet, revive,
gosec). Le direttive `//nolint` portano sempre la motivazione.

Toolchain: il progetto dichiara `go 1.25` come minimo ed è sviluppato e testato con
**Go 1.27** (riga `toolchain` di `go.mod`, immagine `golang:1.27-alpine`).

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

## Cosa coprono

| Area | Cosa è verificato |
|---|---|
| Configurazione | Default, override, valori vuoti, normalizzazione, **tutti gli errori in un colpo** |
| Naming | Corpus di nomi reali (Compose v1/v2, Swarm, k8s, non-ASCII, 300 caratteri, metacaratteri), idempotenza, stabilità, collisioni |
| Parsing hosts | Classificazione righe, byte-per-byte, CRLF, righe malformate, blocchi orfani |
| Scrittura | Idempotenza, preserva entry utente, entrambe le modalità, concorrenza, assenza di file temporanei, `flock` |
| **Round trip** | Aggiungi + togli = byte originali (CRLF, righe vuote finali, senza `\n` finale, byte non UTF-8, riga da 6 MB), con **fuzzing**; nessun residuo dopo `END`; symlink e permessi preservati; pulizia dei temporanei |
| **Ciclo di vita** | `internal/agent` con un Docker finto: arresto rimuove il blocco, riavvio dopo kill converge, panic, errore all'avvio, `clean` idempotente |
| Link della web UI | Una porta un link (pubblicata → nome:porta host, esposta → indirizzo:porta container), IPv6 nascosto, container-ip, porte non web e UDP, nome preferito, escape di `</script>` |
| **Crash** | `SIGKILL` reale durante le scritture, 15 round per modalità, recovery da ogni forma di danno |
| Iniezione | Un nome con `\n` **non può** iniettare record nel file dell'host |
| Accettazione | ~20 container reali, dalla creazione al `kill -9`, con verifica HTTP reale |
| **Uscite** | Processo vero: `SIGTERM`/`SIGINT`/`SIGHUP` in entrambe le modalità, segnali ripetuti, `kill -9` + riavvio, `clean`, Docker irraggiungibile, permessi del file |
| **Immagine** | `docker stop` restituisce il file all'operatore (bind mount di file e di directory), `docker kill` + `clean`/riavvio, `HEALTHCHECK` healthy |
| **Browser** | Playwright: tabella = API = file hosts, niente IPv6, tag TCP/UDP, link cliccabili e funzionanti, colonne dimensionate, filtro, aggiornamenti dal vivo, polling, dati ostili, schede su schermo stretto (320–768 px), tema scuro, tastiera, riconnessione |

## I test di accettazione

`sudo make test-integration` (o `make test-integration SUDO=sudo`) esegue la suite completa contro il Docker locale.
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

## Il test di crash

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

## CI

`.github/workflows/ci.yml` esegue:

- **lint** — gofmt, `go vet`, golangci-lint (versione fissata), test con race
  detector, copertura, build e verifica che il binario sia statico;
- **integration** — la suite completa come root (`make test-integration
  SUDO="sudo -E"`) contro Docker **25, 26, 27, 28, 29** (dind). Le versioni
  **20.10** e **23** sono nella matrice come *sperimentali*: un loro
  fallimento è segnalato ma non fa fallire il workflow, finché non hanno uno
  storico di successi;
- **e2e** — la web UI in Chrome con Playwright, contro un agente e container veri;
- **smoke** — `make smoke`: il vero `docker-compose.yml`, un container reale, la
  risoluzione del nome, la sostituzione di `/etc/hosts` con un rename e il file
  restituito da `compose down`;
- **image** — build e ispezione dell'immagine.

## Compatibilità con Docker più vecchi

Il minimo è l'API **1.40** (Docker 19.03). L'agente usa solo l'elenco dei
container, lo stream di eventi e la versione del motore. Il limite è verificato
con richieste legate a ciascuna versione API da 1.40 a 1.44
(`DOCKER_API_VERSION`) contro un daemon reale
(`test/integration/apiversion_test.go`) e con un test unitario sulla soglia. Non
è stato provato su un motore realmente così vecchio dalla macchina di sviluppo:
è lo scopo delle voci sperimentali della matrice CI.

[← README](../README.md)
