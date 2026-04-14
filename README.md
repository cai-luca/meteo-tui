# Meteo TUI (Go)

Questa applicazione recupera i dati meteorologici in tempo reale utilizzando l'API Open-Meteo. Gli utenti possono inserire il nome di una città per recuperare temperatura, velocità del vento e umidità.

## 🌟 Panoramica del Progetto

**Meteo TUI** è uno strumento leggero e veloce per consultare il meteo dal terminale. Supporta sia una modalità interattiva (TUI) che una modalità di avvio rapido (CLI). Il progetto è documentato in dettaglio nel file [presentazione.md](file:///Users/zyaen/Desktop/meteo%20AI/presentazione.md).

## 🛠️ Navigazione ed Esecuzione

### Struttura del Repository
- `main.go`: Contiene tutta la logica dell'applicazione (TUI, API, Cache, GUI).
- `main_test.go`: Suite di test unitari per garantire la correttezza del codice.
- `meteo.db`: Database SQLite per la cache locale (creato automaticamente).
- `favoriteCities`: File JSON dei preferiti (creato automaticamente).
- `presentazione.md`: Relazione dettagliata del progetto.

### Prerequisiti
- **Go 1.21+** installato sul sistema.

### Installazione ed Esecuzione
1. **Clona questo repository**:
   ```bash
   git clone https://github.com/cai-luca/meteo-tui
   cd "meteo-tui"
   ```
2. **Installa le dipendenze**:
   ```bash
   go mod tidy
   ```
3. **Esegui l'app in modalità TUI**:
   ```bash
   go run main.go
   ```
4. **Esegui i Test**:
   ```bash
   go test -v ./...
   ```

## 🚀 Utilizzo Dettagliato

L'applicazione può essere utilizzata in tre modi:

### 1. Modalità Quick Launch
Esegui il seguente comando per recuperare istantaneamente i dati meteorologici per una città:
```bash
go run main.go Londra
```

### 2. Modalità List (TUI temporanea)
Avvia la TUI con una lista di città specifica (separate da virgola o spazio), ignorando i preferiti salvati:
```bash
go run main.go --list "Tokyo,New York,Torino"
# Oppure
go run main.go --list Tokyo Paris Berlin
```

### 3. Modalità TUI (Interattiva)
Esegui l'app senza parametri per entrare nella modalità grafica con i tuoi preferiti:
```bash
go run main.go --refresh 15
```
- Usa le **frecce** per navigare tra i risultati.
- Premi **Enter** per aggiungere una città alla lista di monitoraggio temporanea.
- Premi **Ctrl+S** per salvare esplicitamente la lista attuale nei preferiti (file `favoriteCities`).
- Usa le frecce **Sinistra/Destra** per scorrere tra i giorni della settimana.
- Usa le frecce **Su/Giù** per scorrere tra le città monitorate.
- Premi **f** per alternare tra meteo corrente e previsioni.
- Premi **x** per svuotare la lista delle città monitorate.
- Premi **Esc** per uscire.

## ✨ Funzionalità Avanzate

- 🗄️ **SQLite Cache**: Tutte le richieste API sono memorizzate in `meteo.db` per 15 minuti, garantendo velocità e risparmio dati.
- 🔄 **Auto-Refresh**: La TUI si aggiorna automaticamente ogni N minuti (configurabile con `--refresh`).
- 📌 **Preferiti Persistenti**: Le città monitorate vengono salvate con le loro coordinate per evitare ricerche ridondanti.
- 📅 **GUI Evoluta**: Layout con `BIG_view` per i dettagli e `LittleView` per le previsioni a 5 giorni.
- 🌍 **Localizzazione Automatica**: Supporto per **Italiano** e **Inglese**. L'app rileva la lingua tramite la variabile d'ambiente `$LANG`.

## 📡 Informazioni API

L'app utilizza le API di **[Open-Meteo](https://open-meteo.com/)**. Nessuna chiave API richiesta.

## 🛡️ Sicurezza e Conformità

La sicurezza e la privacy degli utenti sono una priorità per questo progetto:

- **Gestione Dati Sensibili**: L'applicazione non richiede né memorizza chiavi API o password.
- **Sanificazione Input**: Tutti gli input dell'utente (nomi delle città) vengono puliti e sanificati per prevenire manipolazioni improprie.
- **Parametrizzazione SQL**: Tutte le query al database SQLite utilizzano parametri (prepared statements) per eliminare il rischio di SQL Injection.
- **Configurazione Sicura**: I percorsi del database e dei preferiti possono essere configurati tramite variabili d'ambiente:
  - `METEO_DB_PATH`: Percorso del file di cache SQLite (default: `./meteo.db`).
  - `METEO_FAVORITES_PATH`: Percorso del file dei preferiti (default: `./favoriteCities`).
- **Privacy**: L'applicazione opera interamente in locale. I dati delle città cercate e monitorate rimangono sul computer dell'utente e non vengono mai inviati a server di terze parti (eccetto le coordinate geografiche necessarie per interrogare l'API Open-Meteo).

## 📄 Licenze e Dipendenze

Questo progetto utilizza librerie open-source con licenze permissive (MIT/BSD):

- **[Bubble Tea](https://github.com/charmbracelet/bubbletea)** (MIT) - TUI framework.
- **[Bubbles](https://github.com/charmbracelet/bubbles)** (MIT) - Componenti UI.
- **[Lip Gloss](https://github.com/charmbracelet/lipgloss)** (MIT) - Styling e layout.
- **[modernc.org/sqlite](https://gitlab.com/cznic/sqlite)** (BSD-3-Clause) - Driver SQLite (C-go free).

Tutti i diritti dei rispettivi proprietari sono rispettati. L'utilizzo di queste librerie non impone restrizioni sulla distribuzione commerciale di questa app (licenze non-copyleft).

## 🧪 Test

Esegui i test per validare l'app:
```bash
go test -v main.go main_test.go
```
