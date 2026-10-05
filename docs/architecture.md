# Architettura

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

## Note di implementazione

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

[← README](../README.md)
