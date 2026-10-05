# Nomi e porte

## Perché il nome punta all'host (la questione delle porte)

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

## Quali nomi vengono pubblicati

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

[← README](../README.md)
