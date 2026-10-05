# L'interfaccia di monitoraggio

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

## I link di accesso

La tabella ha **una riga per container**, con colonne di larghezza fissa
(Container, Stato, Indirizzi, Nomi, Open) che non si spostano quando cambia il
contenuto. Gli indirizzi **IPv6 non sono mostrati** nella pagina (restano nel
file hosts e nell'API: `::1` per una porta pubblicata). Un indirizzo IPv6 compare
solo se è l'unico modo di raggiungere qualcosa.

L'ultima colonna, **Open**, ha **un solo link per porta**, con un tag che dice se
è **TCP** o **UDP** (a parole, in colore diverso e con bordo pieno o tratteggiato,
così non dipende dal solo colore):

| Porta | Link | Perché |
|---|---|---|
| Pubblicata (`-p`) | `http://<nome>:<porta dell'host>/` | È lo scopo del nome: `nginx.docker.local:8080` funziona come `localhost:8080` |
| Solo esposta (`--expose`, `EXPOSE`) | `http://<indirizzo del container>:<porta del container>/` | Si usa l'**indirizzo** e non il nome, perché in modalità `both` il nome risolve prima sull'host, e un servizio dell'host sulla stessa porta risponderebbe al posto del container. Con `TARGET_MODE=container-ip` il nome risolve solo sul container e si usa il nome |

Una porta pubblicata e in ascolto nel container non è elencata due volte: vale il
link pubblicato. Una porta legata sia a `0.0.0.0` sia a `::` ha un link solo.
Sotto i 900 px la tabella diventa un elenco di **schede** (nome e stato in testa,
poi ogni valore sotto il nome della colonna), con chip alti a sufficienza per il
tocco; nessuna larghezza di schermo produce scorrimento orizzontale.

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

[← README](../README.md)
