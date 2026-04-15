/*
Meteo TUI - Un'applicazione meteorologica professionale per terminale in Go.

Caratteristiche Principali (Advanced Features):
- Integrazione API Open-Meteo per dati in tempo reale e geocodifica.
- Caching tramite SQLite: TTL per chiave (meteo ~15 min, geocodifica ~24 h), purge globale delle righe più vecchie di 7 giorni all'avvio e ogni ora.
- In TUI, recupero meteo con retry (fino a 3 tentativi con backoff) entro un timeout sul context.
- Interfaccia utente avanzata (Bubble Tea/Lip Gloss) con layout adattivo (BIG_view e LittleView).
- Monitoraggio multi-città e preferiti persistenti; file preferiti corrotto rinominato con suffisso .corrupted.
- Localizzazione automatica (IT/EN) basata sul sistema.

Sicurezza ed Etica (Security & Ethics):
- Utilizzo responsabile di API pubbliche senza necessità di chiavi segrete.
- Sanificazione degli input (trim, lunghezza massima, rimozione caratteri non adatti ai nomi città).
- Prevenzione SQL Injection: query parametrizzate con placeholder '?' e argomenti separati (database/sql).
- Privacy-first: Nessun dato utente viene inviato a terze parti (eccetto le coordinate per l'API meteo).
- Codice documentato e conforme alle licenze open source delle dipendenze (MIT/BSD).
*/

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Styles
var (
	appStyle   = lipgloss.NewStyle().Padding(1, 2)
	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFDF5")).
			Background(lipgloss.Color("#25A065")).
			Padding(0, 1)
	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5F87"))
	weatherBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Padding(1, 2).
			Width(60).
			Height(8) // Altezza fissa per stabilità
)

// API Types
type City struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Country   string  `json:"country"`
	Admin1    string  `json:"admin1"`
}

// GeocodingResponse rappresenta la risposta dell'API di geocodifica di Open-Meteo.
type GeocodingResponse struct {
	Results []City `json:"results"`
}

// WeatherResponse rappresenta la risposta dell'API meteorologica di Open-Meteo.
type WeatherResponse struct {
	Current struct {
		Temperature   float64 `json:"temperature_2m"`
		Humidity      float64 `json:"relative_humidity_2m"`
		WindSpeed     float64 `json:"wind_speed_10m"`
		WeatherCode   int     `json:"weather_code"`
		Precipitation float64 `json:"precipitation"`
	} `json:"current"`
	Daily struct {
		Time             []string  `json:"time"`
		WeatherCode      []int     `json:"weather_code"`
		TemperatureMax   []float64 `json:"temperature_2m_max"`
		TemperatureMin   []float64 `json:"temperature_2m_min"`
		HumidityMax      []float64 `json:"relative_humidity_2m_max"`
		PrecipitationSum []float64 `json:"precipitation_sum"`
	} `json:"daily"`
}

// pinnedCity contiene i dati per una città monitorata.
type pinnedCity struct {
	City    City             `json:"city"`
	Weather *WeatherResponse `json:"weather"`
}

// item implementa l'interfaccia list.Item per la visualizzazione nella lista Bubble Tea.
type item struct {
	city City
}

func (i item) Title() string       { return i.city.Name }
func (i item) Description() string { return fmt.Sprintf("%s, %s", i.city.Admin1, i.city.Country) }
func (i item) FilterValue() string { return i.city.Name }

// model contiene lo stato dell'applicazione.
type model struct {
	textInput    textinput.Model
	cityList     list.Model
	cities       []City
	selectedCity *City
	weather      *WeatherResponse
	pinnedCities []pinnedCity
	pinnedIdx    int // Indice della città monitorata visualizzata in BIG_view
	selectedDay  int // 0 per oggi, 1-3 per previsioni
	statusMsg    string
	statusMsgID  int
	err          error
	loading      bool
	lastSearch   string
	searchTimer  *time.Timer
	refreshMin   int
	refreshing   bool
	refreshJobs  int
}

// Messaggi personalizzati per la gestione dello stato in Bubble Tea.
type gotCitiesMsg []City
type gotWeatherMsg struct {
	weather *WeatherResponse
	city    City
}
type errMsg error
type searchMsg string
type clearStatusMsg struct {
	id int
}

// initialModel crea lo stato iniziale dell'applicazione.
func initialModel() model {
	t := getT()
	ti := textinput.New()
	ti.Placeholder = t.SearchPlace
	ti.Focus()
	ti.CharLimit = 156
	ti.Width = 30

	l := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	l.Title = t.CitiesTitle
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.SetShowHelp(false)

	return model{
		textInput: ti,
		cityList:  l,
	}
}

// Init viene chiamato all'avvio dell'applicazione.
func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, fetchCities("Roma")}
	if m.refreshMin > 0 {
		cmds = append(cmds, tea.Tick(time.Duration(m.refreshMin)*time.Minute, func(t time.Time) tea.Msg {
			return tickMsg(t)
		}))
	}
	return tea.Batch(cmds...)
}

var (
	geocodingBaseURL = "https://geocoding-api.open-meteo.com/v1/search"
	weatherBaseURL   = "https://api.open-meteo.com/v1/forecast"
	httpClient       = &http.Client{Timeout: 10 * time.Second}
	verbose          bool
	db               *sql.DB
	favoritesFile    = getEnv("METEO_FAVORITES_PATH", "./favoriteCities")
	dbPath           = getEnv("METEO_DB_PATH", "./meteo.db")
	userLang         = "en" // Default
)

var dangerousPattern = regexp.MustCompile(`[<>{}\[\]$@%&;'"\\]`)

// getEnv restituisce il valore di una variabile d'ambiente o un valore di default.
func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// sanitizeInput normalizza l'input per la ricerca città: TrimSpace, limite di lunghezza e rimozione
// di caratteri non adatti (vedi dangerousPattern), per ridurre input anomali o rumorosi.
func sanitizeInput(input string) string {
	// Rimuove spazi bianchi all'inizio e alla fine
	trimmed := strings.TrimSpace(input)
	// Limita la lunghezza dell'input per prevenire attacchi DoS o buffer overflow (teorici in Go)
	if len(trimmed) > 100 {
		trimmed = trimmed[:100]
	}
	// Rimuove caratteri che non hanno senso nel contesto città e potrebbero sporcare query/log.
	return dangerousPattern.ReplaceAllString(trimmed, "")
}

type translations struct {
	Title        string
	SearchPlace  string
	CitiesTitle  string
	Loading      string
	SelectCity   string
	Temp         string
	Humidity     string
	Wind         string
	Rain         string
	Monitor      string
	Today        string
	Error        string
	Help         string
	Saved        string
	Added        string
	WeatherCodes map[string]string
}

var i18n = map[string]translations{
	"it": {
		Title:       "Meteo TUI",
		SearchPlace: "Cerca una città (es. Milano, Roma...)",
		CitiesTitle: "Città",
		Loading:     "Caricamento dati...",
		SelectCity:  "Seleziona una città per iniziare",
		Temp:        "Temp",
		Humidity:    "Umidità",
		Wind:        "Vento",
		Rain:        "Precipitazioni",
		Monitor:     "MONITOR",
		Today:       "OGGI",
		Error:       "Errore",
		Help:        "←/→: Naviga | Enter: Monitora | Ctrl+S: Salva | f: Forecast | x: Clear | esc: Exit",
		Saved:       "Preferiti salvati con successo!",
		Added:       "Città aggiunta al monitoraggio (Ctrl+S per salvare su disco)",
		WeatherCodes: map[string]string{
			"clear":   "Sereno",
			"cloudy":  "Nuvoloso",
			"fog":     "Nebbia",
			"rain":    "Pioggia",
			"snow":    "Neve",
			"showers": "Acquazzoni",
			"thunder": "Temporale",
			"unknown": "Sconosciuto",
		},
	},
	"en": {
		Title:       "Weather TUI",
		SearchPlace: "Search for a city (e.g. London, New York...)",
		CitiesTitle: "Cities",
		Loading:     "Loading data...",
		SelectCity:  "Select a city to start",
		Temp:        "Temp",
		Humidity:    "Humidity",
		Wind:        "Wind",
		Rain:        "Precipitation",
		Monitor:     "MONITOR",
		Today:       "TODAY",
		Error:       "Error",
		Help:        "←/→: Navigate | Enter: Monitor | Ctrl+S: Save | f: Forecast | x: Clear | esc: Exit",
		Saved:       "Favorites saved successfully!",
		Added:       "City added to monitor (Ctrl+S to save to disk)",
		WeatherCodes: map[string]string{
			"clear":   "Clear",
			"cloudy":  "Cloudy",
			"fog":     "Fog",
			"rain":    "Rain",
			"snow":    "Snow",
			"showers": "Showers",
			"thunder": "Thunderstorm",
			"unknown": "Unknown",
		},
	},
}

func getT() translations {
	if t, ok := i18n[userLang]; ok {
		return t
	}
	return i18n["en"]
}

func detectLanguage() {
	lang := os.Getenv("LANG")
	if strings.HasPrefix(lang, "it") {
		userLang = "it"
	} else {
		userLang = "en"
	}
}

const (
	weatherParams = "temperature_2m,relative_humidity_2m,wind_speed_10m,weather_code,precipitation"
	dailyParams   = "weather_code,temperature_2m_max,temperature_2m_min,relative_humidity_2m_max,precipitation_sum"
)

// initDB apre SQLite, crea la tabella cache se assente ed esegue subito una pulizia delle entry
// più vecchie della finestra globale (7 giorni). La pulizia periodica è avviata da startCacheCleaner.
func initDB() {
	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("Errore apertura DB: %v", err)
	}

	query := `
	CREATE TABLE IF NOT EXISTS cache (
		key TEXT PRIMARY KEY,
		data TEXT,
		timestamp DATETIME
	);`
	_, err = db.Exec(query)
	if err != nil {
		log.Fatalf("Errore creazione tabella cache: %v", err)
	}

	cleanExpiredCache()
}

func saveToCache(key string, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, err = db.Exec("INSERT OR REPLACE INTO cache (key, data, timestamp) VALUES (?, ?, ?)",
		key, string(jsonData), time.Now())
	if err != nil {
		debugLog("Errore salvataggio cache: %v", err)
	}
}

// getFromCache legge dalla cache se la riga non è scaduta rispetto a maxAge. Se il JSON è malformato,
// la riga viene eliminata e la funzione restituisce false (così il prossimo fetch ripopola la cache).
func getFromCache(key string, target interface{}, maxAge time.Duration) bool {
	var data string
	var timestamp time.Time
	err := db.QueryRow("SELECT data, timestamp FROM cache WHERE key = ?", key).Scan(&data, &timestamp)
	if err != nil {
		return false
	}

	if time.Since(timestamp) > maxAge {
		db.Exec("DELETE FROM cache WHERE key = ?", key)
		return false
	}

	err = json.Unmarshal([]byte(data), target)
	if err != nil {
		debugLog("Corrupted cache for key %s: %v", key, err)
		if _, delErr := db.Exec("DELETE FROM cache WHERE key = ?", key); delErr != nil {
			debugLog("Failed to delete corrupted cache for key %s: %v", key, delErr)
		}
		return false
	}
	return true
}

// loadFavorites legge il file JSON dei preferiti. Se il file non esiste restituisce nil senza errore;
// errori di lettura o JSON corrotto vengono loggati in verbose; in caso di unmarshal fallito il file
// viene rinominato con suffisso .corrupted per evitare loop su dati invalidi.
func loadFavorites() []pinnedCity {
	data, err := os.ReadFile(favoritesFile)
	if err != nil {
		if !os.IsNotExist(err) {
			debugLog("Error reading favorites: %v", err)
		}
		return nil
	}
	var favs []pinnedCity
	if err := json.Unmarshal(data, &favs); err != nil {
		debugLog("Corrupted favorites file: %v", err)
		if renameErr := os.Rename(favoritesFile, favoritesFile+".corrupted"); renameErr != nil {
			debugLog("Unable to backup corrupted favorites file: %v", renameErr)
		}
		return nil
	}
	return favs
}

func saveFavorites(favs []pinnedCity) {
	data, err := json.Marshal(favs)
	if err != nil {
		return
	}
	os.WriteFile(favoritesFile, data, 0644)
}

// debugLog scrive un messaggio nel file di log se la modalità verbose è attiva.
func debugLog(format string, v ...interface{}) {
	if verbose {
		log.Printf("[DEBUG] "+format, v...)
	}
}

/*
TRACI Docstring - getWeather
----------------------------
Task: Recuperare i dati meteorologici correnti per una specifica città.
Role: Funzione di utilità per l'accesso ai dati esterni (API Open-Meteo).
Audience: Sviluppatori che lavorano sull'integrazione delle API.
Context: Utilizzata sia nella modalità interattiva (TUI) che nella modalità CLI rapida.
Intent: Fornire un'interfaccia unificata per il recupero dei dati meteo tramite cache (getFromCache) o HTTP.
Note: La validità cache è per maxAge; dati corrotti in tabella vengono rimossi in getFromCache.

Parametri:
  - ctx: context.Context per gestire la cancellazione e il timeout della richiesta.
  - city: struct City contenente nome, latitudine e longitudine della città.

Valori restituiti:
  - *WeatherResponse: Puntatore alla struttura contenente temperatura, umidità, vento e codice meteo.
  - error: Eventuale errore di rete, di creazione richiesta o di decodifica JSON.

Esempio di utilizzo:

	res, err := getWeather(ctx, city)
	if err != nil {
	    log.Fatal(err)
	}
	fmt.Printf("Temp: %.1f°C", res.Current.Temperature)
*/
func getWeather(ctx context.Context, city City) (*WeatherResponse, error) {
	apiURL := fmt.Sprintf("%s?latitude=%f&longitude=%f&current=%s&daily=%s&timezone=auto", weatherBaseURL, city.Latitude, city.Longitude, weatherParams, dailyParams)

	var cachedRes WeatherResponse
	if getFromCache(apiURL, &cachedRes, 15*time.Minute) {
		debugLog("Using cached weather for city: %s", city.Name)
		return &cachedRes, nil
	}

	debugLog("Fetching weather for city: %s (%f, %f)", city.Name, city.Latitude, city.Longitude)
	debugLog("Weather API URL: %s", apiURL)

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creazione richiesta fallita: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("richiesta HTTP fallita: %w", err)
	}
	defer resp.Body.Close()

	debugLog("Weather response status: %d", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("errore API meteo: stato %d", resp.StatusCode)
	}

	var res WeatherResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("decodifica risposta fallita: %w", err)
	}

	saveToCache(apiURL, res)
	debugLog("Successfully fetched weather: %.1f°C", res.Current.Temperature)
	return &res, nil
}

/*
TRACI Docstring - getWeatherWithRetry
-------------------------------------
Task: Ottenere il meteo per una città restando resiliente a errori transitori (rete, timeout).
Role: Wrapper sopra getWeather per la TUI.
Context: Usa lo stesso ctx per tutti i tentativi; tra un tentativo e l'altro attende con backoff

	(1s, 2s, ...) salvo cancellazione del context.

Intent: Ridurre errori visibili all'utente dopo un singolo fallimento temporaneo.
*/
func getWeatherWithRetry(ctx context.Context, city City, maxRetries int) (*WeatherResponse, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		res, err := getWeather(ctx, city)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if i == maxRetries-1 {
			break
		}

		delay := time.Duration(i+1) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
			debugLog("Retry %d for city %s after error: %v", i+1, city.Name, err)
		}
	}
	return nil, fmt.Errorf("failed after %d retries: %w", maxRetries, lastErr)
}

/*
TRACI Docstring - fetchWeather
------------------------------
Task: Recuperare in modo asincrono i dati meteo di una città.
Role: Handler di caricamento dati per l'interfaccia Bubble Tea.
Audience: Sviluppatori che lavorano sulla visualizzazione dei dati.
Context: Chiamata quando l'utente naviga nella lista delle città o seleziona una città.
Intent: Aggiornare lo stato dell'applicazione con i dati meteorologici correnti (temp, umidità, vento).

Note implementative:
  - Crea un context con timeout (budget totale per tutti i tentativi) e chiama getWeatherWithRetry
    con maxRetries=3. La cache valida in getWeather può far completare al primo tentativo senza HTTP.

Valori restituiti:
  - tea.Cmd: Un comando Bubble Tea che restituisce gotWeatherMsg o errMsg.
*/
func fetchWeather(city City) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()

		res, err := getWeatherWithRetry(ctx, city, 3)
		if err != nil {
			log.Printf("[ERROR] %v", err)
			return errMsg(err)
		}
		return gotWeatherMsg{weather: res, city: city}
	}
}

/*
TRACI Docstring - fetchCities
-----------------------------
Task: Effettuare una ricerca asincrona di città tramite l'API di geocodifica.
Role: Handler di ricerca per l'interfaccia Bubble Tea.
Audience: Sviluppatori che si occupano della logica TUI.
Context: Chiamata ogni volta che l'input dell'utente cambia (con debounce).
Intent: Recuperare una lista di città candidate per permettere all'utente di selezionarne una.

Valori restituiti:
  - tea.Cmd: Un comando Bubble Tea che, una volta eseguito, restituisce gotCitiesMsg o errMsg.
*/
func fetchCities(query string) tea.Cmd {
	return func() tea.Msg {
		query = sanitizeInput(query)
		if len(query) < 2 {
			return gotCitiesMsg(nil)
		}
		apiURL := fmt.Sprintf("%s?name=%s&count=10&language=%s&format=json", geocodingBaseURL, url.QueryEscape(query), userLang)

		var cachedRes GeocodingResponse
		if getFromCache(apiURL, &cachedRes, 24*time.Hour) { // Cache geocoding for 24h
			debugLog("Using cached cities for query: %s", query)
			return gotCitiesMsg(cachedRes.Results)
		}

		debugLog("Fetching cities for query: %s", query)
		debugLog("Geocoding API URL: %s", apiURL)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			log.Printf("[ERROR] Creation request failed: %v", err)
			return errMsg(err)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			log.Printf("[ERROR] Geocoding request failed: %v", err)
			return errMsg(err)
		}
		defer resp.Body.Close()

		debugLog("Geocoding response status: %d", resp.StatusCode)

		if resp.StatusCode != http.StatusOK {
			err := fmt.Errorf("API geocoding error: status %d", resp.StatusCode)
			log.Printf("[ERROR] %v", err)
			return errMsg(err)
		}

		var res GeocodingResponse
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			log.Printf("[ERROR] Failed to decode geocoding response: %v", err)
			return errMsg(err)
		}

		saveToCache(apiURL, res)
		debugLog("Found %d cities", len(res.Results))
		return gotCitiesMsg(res.Results)
	}
}

// fetchWeatherSync effettua una chiamata sincrona per il meteo (usata in modalità Quick Launch).
func fetchWeatherSync(city City) (*WeatherResponse, error) {
	return getWeather(context.Background(), city)
}

// fetchCitiesSync effettua una chiamata sincrona per le città (usata in modalità Quick Launch).
func fetchCitiesSync(query string) ([]City, error) {
	query = sanitizeInput(query)
	apiURL := fmt.Sprintf("%s?name=%s&count=1&language=%s&format=json", geocodingBaseURL, url.QueryEscape(query), userLang)
	resp, err := httpClient.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API geocoding error: status %d", resp.StatusCode)
	}

	var res GeocodingResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res.Results, nil
}

// cleanExpiredCache rimuove dalla tabella cache tutte le righe con timestamp più vecchio di 7 giorni.
// Complementa la scadenza per-chiave gestita da getFromCache (maxAge) e limita la crescita del DB.
func cleanExpiredCache() {
	_, err := db.Exec("DELETE FROM cache WHERE timestamp < ?", time.Now().Add(-7*24*time.Hour))
	if err != nil {
		debugLog("Cache cleanup failed: %v", err)
	}
}

// startCacheCleaner avvia una goroutine che esegue cleanExpiredCache ogni ora finché ctx non viene
// cancellato (es. uscita da main). Il ticker viene fermato alla conclusione della goroutine.
func startCacheCleaner(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cleanExpiredCache()
			case <-ctx.Done():
				return
			}
		}
	}()
}

type tickMsg time.Time

/*
TRACI Docstring - Update
------------------------
Task: Gestire gli eventi di input e i messaggi asincroni per aggiornare lo stato del modello.
Role: Motore logico dell'applicazione (Elm Architecture).
Audience: Sviluppatori che lavorano sulla logica dell'applicazione.
Context: Chiamata ogni volta che si verifica un evento (tasto premuto, risposta API ricevuta).
Intent: Assicurare che lo stato del modello sia sempre aggiornato in modo deterministico e gestire la logica di debounce della ricerca.

Note: Per tickMsg (auto-refresh), se un ciclo di refresh è ancora in corso (refreshing), il tick viene ignorato e si ripianifica il prossimo tick dopo refreshMin per evitare richieste HTTP sovrapposte.

Parametri:
  - msg: Un valore tea.Msg che può essere un evento di tastiera, un aggiornamento di dimensione finestra o un messaggio personalizzato (gotWeatherMsg, gotCitiesMsg, searchMsg).

Valori restituiti:
  - tea.Model: Il modello aggiornato.
  - tea.Cmd: Un eventuale nuovo comando da eseguire (es. nuova chiamata API).
*/
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tickMsg:
		if m.refreshMin > 0 {
			if m.refreshing {
				debugLog("Skip refresh tick: previous refresh still running")
				return m, tea.Tick(time.Duration(m.refreshMin)*time.Minute, func(t time.Time) tea.Msg {
					return tickMsg(t)
				})
			}

			var cmds []tea.Cmd
			jobs := 0
			// Aggiorna città selezionata
			if m.selectedCity != nil {
				cmds = append(cmds, fetchWeather(*m.selectedCity))
				jobs++
			}
			// Aggiorna città monitorate
			for _, pc := range m.pinnedCities {
				cmds = append(cmds, fetchWeather(pc.City))
				jobs++
			}
			if jobs > 0 {
				m.refreshing = true
				m.refreshJobs = jobs
			}
			cmds = append(cmds, tea.Tick(time.Duration(m.refreshMin)*time.Minute, func(t time.Time) tea.Msg {
				return tickMsg(t)
			}))
			return m, tea.Batch(cmds...)
		}

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit

		case tea.KeyUp:
			// Se abbiamo città monitorate, le scorriamo
			if len(m.pinnedCities) > 0 {
				m.pinnedIdx = (m.pinnedIdx - 1 + len(m.pinnedCities)) % len(m.pinnedCities)
				m.selectedCity = &m.pinnedCities[m.pinnedIdx].City
				m.weather = m.pinnedCities[m.pinnedIdx].Weather
				return m, nil
			}
			// Altrimenti scorriamo la lista di ricerca
			var listCmd tea.Cmd
			m.cityList, listCmd = m.cityList.Update(msg)
			if len(m.cityList.Items()) > 0 {
				selected := m.cityList.SelectedItem().(item).city
				if m.selectedCity == nil || m.selectedCity.ID != selected.ID {
					m.loading = true
					m.selectedDay = 0
					return m, tea.Batch(listCmd, fetchWeather(selected))
				}
			}
			return m, listCmd

		case tea.KeyDown:
			// Se abbiamo città monitorate, le scorriamo
			if len(m.pinnedCities) > 0 {
				m.pinnedIdx = (m.pinnedIdx + 1) % len(m.pinnedCities)
				m.selectedCity = &m.pinnedCities[m.pinnedIdx].City
				m.weather = m.pinnedCities[m.pinnedIdx].Weather
				return m, nil
			}
			// Altrimenti scorriamo la lista di ricerca
			var listCmd tea.Cmd
			m.cityList, listCmd = m.cityList.Update(msg)
			if len(m.cityList.Items()) > 0 {
				selected := m.cityList.SelectedItem().(item).city
				if m.selectedCity == nil || m.selectedCity.ID != selected.ID {
					m.loading = true
					m.selectedDay = 0
					return m, tea.Batch(listCmd, fetchWeather(selected))
				}
			}
			return m, listCmd

		case tea.KeyLeft:
			if m.selectedDay > 0 {
				m.selectedDay--
			}
			return m, nil

		case tea.KeyRight:
			if m.selectedDay < 4 {
				m.selectedDay++
			}
			return m, nil

		case tea.KeyEnter:
			if m.selectedCity != nil && m.weather != nil {
				// Aggiungi ai monitorati se non già presente
				exists := false
				for _, pc := range m.pinnedCities {
					if pc.City.ID == m.selectedCity.ID {
						exists = true
						break
					}
				}
				if !exists {
					m.pinnedCities = append(m.pinnedCities, pinnedCity{
						City:    *m.selectedCity,
						Weather: m.weather,
					})
					return m, m.withStatus(getT().Added)
				}
			}
			return m, nil

		case tea.KeyCtrlS:
			saveFavorites(m.pinnedCities)
			return m, m.withStatus(getT().Saved)
		}

		switch msg.String() {
		case "f":
			// Toggle forecast (handled by selectedDay > 0 now)
			if m.selectedDay == 0 {
				m.selectedDay = 1
			} else {
				m.selectedDay = 0
			}
			return m, nil
		case "x":
			m.pinnedCities = nil
			saveFavorites(m.pinnedCities)
			return m, nil
		}

	case searchMsg:
		if string(msg) == m.textInput.Value() {
			return m, fetchCities(string(msg))
		}
		return m, nil

	case clearStatusMsg:
		if msg.id == m.statusMsgID {
			m.statusMsg = ""
		}
		return m, nil

	case gotCitiesMsg:
		m.loading = false
		items := make([]list.Item, len(msg))
		for i, c := range msg {
			items[i] = item{city: c}
		}
		m.cityList.SetItems(items)

		// Carica automaticamente il meteo per la prima città se presente
		if len(items) > 0 {
			selected := items[0].(item).city
			m.loading = true
			return m, fetchWeather(selected)
		} else {
			m.weather = nil
			m.selectedCity = nil
		}
		return m, nil

	case gotWeatherMsg:
		m.loading = false
		if m.refreshing && m.refreshJobs > 0 {
			m.refreshJobs--
			if m.refreshJobs == 0 {
				m.refreshing = false
			}
		}
		m.weather = msg.weather
		m.selectedCity = &msg.city
		// Update pinned city data if it matches
		for i, pc := range m.pinnedCities {
			if pc.City.ID == msg.city.ID {
				m.pinnedCities[i].Weather = msg.weather
			}
		}
		return m, nil

	case errMsg:
		m.err = msg
		m.loading = false
		if m.refreshing && m.refreshJobs > 0 {
			m.refreshJobs--
			if m.refreshJobs == 0 {
				m.refreshing = false
			}
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.cityList.SetSize(msg.Width-4, 8) // Smaller list for new GUI
	}

	// Gestione input testo per la ricerca
	oldValue := m.textInput.Value()
	m.textInput, cmd = m.textInput.Update(msg)
	newValue := m.textInput.Value()

	if oldValue != newValue {
		if len(newValue) >= 2 {
			// Debounce di 300ms
			return m, tea.Batch(cmd, func() tea.Msg {
				time.Sleep(300 * time.Millisecond)
				return searchMsg(newValue)
			})
		} else if len(newValue) == 0 {
			m.cityList.SetItems(nil)
			m.weather = nil
			m.selectedCity = nil
		}
	}

	return m, cmd
}

func (m *model) withStatus(msg string) tea.Cmd {
	m.statusMsgID++
	currentID := m.statusMsgID
	m.statusMsg = msg
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return clearStatusMsg{id: currentID}
	})
}

func getWeatherDescription(code int) string {
	t := getT().WeatherCodes
	// Codici WMO: https://open-meteo.com/en/docs
	switch {
	case code == 0:
		return t["clear"]
	case code >= 1 && code <= 3:
		return t["cloudy"]
	case code >= 45 && code <= 48:
		return t["fog"]
	case code >= 51 && code <= 67:
		return t["rain"]
	case code >= 71 && code <= 77:
		return t["snow"]
	case code >= 80 && code <= 82:
		return t["showers"]
	case code >= 95 && code <= 99:
		return t["thunder"]
	default:
		return t["unknown"]
	}
}

// getWeatherIcon restituisce una rappresentazione ASCII art basata sul codice meteo.
func getWeatherIcon(code int) string {
	// Weather codes: https://open-meteo.com/en/docs
	// 0: Clear sky
	// 1, 2, 3: Mainly clear, partly cloudy, and overcast
	// 45, 48: Fog and depositing rime fog
	// 51, 53, 55: Drizzle: Light, moderate, and dense intensity
	// 61, 63, 65: Rain: Slight, moderate and heavy intensity
	// 71, 73, 75: Snow fall: Slight, moderate, and heavy intensity
	// 80, 81, 82: Rain showers: Slight, moderate, and violent
	// 95, 96, 99: Thunderstorm: Slight or moderate

	yellow := lipgloss.NewStyle().Foreground(lipgloss.Color("226"))
	blue := lipgloss.NewStyle().Foreground(lipgloss.Color("33"))
	gray := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	white := lipgloss.NewStyle().Foreground(lipgloss.Color("255"))

	switch {
	case code == 0: // Clear sky
		return yellow.Render(`    \  /
  _ /"".\_
    \_  _/
    /    \`)
	case code >= 1 && code <= 3: // Mainly clear, partly cloudy, and overcast
		return gray.Render(`     .--.
  .-(    ).
 (___.__)__)`)
	case code >= 45 && code <= 48: // Fog
		return gray.Render(` _  _  _  _
  _  _  _  _
 _  _  _  _`)
	case code >= 51 && code <= 67: // Drizzle, Rain
		return blue.Render(`     .--.
  .-(    ).
 (___.__)__)
  '  '  '  '
  '  '  '  '`)
	case code >= 71 && code <= 77: // Snow
		return white.Render(`     .--.
  .-(    ).
 (___.__)__)
  *  *  *  *
  *  *  *  *`)
	case code >= 80 && code <= 82: // Rain showers
		return blue.Render(`     .--.
  .-(    ).
 (___.__)__)
  /// /// ///`)
	case code >= 95 && code <= 99: // Thunderstorm
		return yellow.Render(`     .--.
  .-(    ).
 (___.__)__)
    /_  /_
   /   /`)
	default:
		return "    ?    "
	}
}

// View renderizza l'interfaccia utente dell'applicazione.
func (m model) View() string {
	t := getT()
	s := strings.Builder{}

	s.WriteString(titleStyle.Render(t.Title))
	s.WriteString("\n\n")
	s.WriteString(m.textInput.View())
	s.WriteString("\n\n")

	if m.err != nil {
		s.WriteString(statusStyle.Render(fmt.Sprintf("%s: %v", t.Error, m.err)))
		s.WriteString("\n")
	}

	if m.statusMsg != "" {
		s.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true).Render(m.statusMsg))
		s.WriteString("\n")
	}

	// --- BIG_view (Top) ---
	bigView := ""
	if m.weather != nil && m.selectedCity != nil {
		dayIdx := m.selectedDay
		var title, temp, humidity, wind, rain, icon string

		if dayIdx == 0 {
			// Today
			title = fmt.Sprintf("%s: %s (%s)", t.Today, m.selectedCity.Name, m.selectedCity.Country)
			temp = fmt.Sprintf("%.1f°C", m.weather.Current.Temperature)
			humidity = fmt.Sprintf("%.0f%%", m.weather.Current.Humidity)
			wind = fmt.Sprintf("%.1f km/h", m.weather.Current.WindSpeed)
			rain = fmt.Sprintf("%.1f mm", m.weather.Current.Precipitation)
			icon = getWeatherIcon(m.weather.Current.WeatherCode)
		} else if dayIdx < len(m.weather.Daily.Time) {
			// Forecast
			date, _ := time.Parse("2006-01-02", m.weather.Daily.Time[dayIdx])
			dayName := date.Format("Monday")
			if userLang == "it" {
				// Traduzione manuale semplice per i giorni se necessario o usare locale
				daysIT := map[string]string{
					"Monday": "Lunedì", "Tuesday": "Martedì", "Wednesday": "Mercoledì",
					"Thursday": "Giovedì", "Friday": "Venerdì", "Saturday": "Sabato", "Sunday": "Domenica",
				}
				dayName = daysIT[dayName]
			}
			title = fmt.Sprintf("%s: %s", strings.ToUpper(dayName), m.selectedCity.Name)
			temp = fmt.Sprintf("%.1f°C / %.1f°C", m.weather.Daily.TemperatureMin[dayIdx], m.weather.Daily.TemperatureMax[dayIdx])
			humidity = fmt.Sprintf("Max %.0f%%", m.weather.Daily.HumidityMax[dayIdx])
			rain = fmt.Sprintf("%.1f mm", m.weather.Daily.PrecipitationSum[dayIdx])
			wind = "" // Rimuoviamo il vento per le previsioni poiché non disponibile o inutile
			icon = getWeatherIcon(m.weather.Daily.WeatherCode[dayIdx])
		}

		// Costruiamo le righe di dettaglio filtrando quelle vuote
		details := []string{
			lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render(title),
			fmt.Sprintf("%s: %s", t.Temp, temp),
			fmt.Sprintf("%s: %s", t.Humidity, humidity),
			fmt.Sprintf("%s: %s", t.Rain, rain),
		}
		if wind != "" {
			details = append(details, fmt.Sprintf("%s: %s", t.Wind, wind))
		}

		bigView = weatherBoxStyle.Render(
			lipgloss.JoinHorizontal(lipgloss.Top,
				lipgloss.NewStyle().
					Width(15).
					Height(5).
					MaxHeight(5).
					PaddingTop(0). // Forza l'allineamento in alto
					Render(icon),
				lipgloss.JoinVertical(lipgloss.Left, details...),
			),
		)
	} else if m.loading {
		bigView = weatherBoxStyle.Render("\n  " + t.Loading)
	} else {
		bigView = weatherBoxStyle.Render("\n  " + t.SelectCity)
	}

	// Favorites/Monitor beside BIG_view
	pinnedContent := ""
	if len(m.pinnedCities) > 0 {
		pinnedRows := []string{lipgloss.NewStyle().Bold(true).Underline(true).Render(t.Monitor + ":")}
		for i, pc := range m.pinnedCities {
			prefix := "  "
			if i == m.pinnedIdx {
				prefix = "> "
			}
			desc := getWeatherDescription(pc.Weather.Current.WeatherCode)
			// Aggiungiamo l'orario di aggiornamento se disponibile nel log, altrimenti usiamo l'ora corrente per simulare
			updateTime := time.Now().Format("15:04")

			row := fmt.Sprintf("%s%s: %.1f°C - %s (%s)",
				prefix,
				pc.City.Name,
				pc.Weather.Current.Temperature,
				desc,
				updateTime,
			)
			pinnedRows = append(pinnedRows, row)
		}
		pinnedContent = lipgloss.NewStyle().MarginLeft(2).Render(strings.Join(pinnedRows, "\n"))
	}

	s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, bigView, pinnedContent))
	s.WriteString("\n\n")

	// --- LittleView (Bottom - 5 days) ---
	if m.weather != nil {
		var dayCards []string
		for i := 0; i < 5 && i < len(m.weather.Daily.Time); i++ {
			date, _ := time.Parse("2006-01-02", m.weather.Daily.Time[i])
			dayName := date.Format("Mon")
			if i == 0 {
				dayName = t.Today
			} else if userLang == "it" {
				daysIT := map[string]string{
					"Mon": "Lun", "Tue": "Mar", "Wed": "Mer", "Thu": "Gio", "Fri": "Ven", "Sat": "Sab", "Sun": "Dom",
				}
				dayName = daysIT[dayName]
			}

			style := lipgloss.NewStyle().
				Border(lipgloss.NormalBorder()).
				Padding(0, 1).
				Width(14).
				Height(3). // Altezza fissa per le card
				Align(lipgloss.Center)

			if i == m.selectedDay {
				style = style.BorderForeground(lipgloss.Color("201")).Bold(true)
			}

			card := style.Render(fmt.Sprintf("%s\n%.0f/%.0f°C",
				dayName,
				m.weather.Daily.TemperatureMin[i],
				m.weather.Daily.TemperatureMax[i]))
			dayCards = append(dayCards, card)
		}
		s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, dayCards...))
	}

	s.WriteString("\n\n")

	// Footer with Search and Help
	footer := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(30).Render(m.cityList.View()),
		lipgloss.NewStyle().MarginLeft(4).Foreground(lipgloss.Color("240")).Render(t.Help),
	)
	s.WriteString(footer)

	return appStyle.Render(s.String())
}

func main() {
	detectLanguage()
	// Parse flags
	flag.BoolVar(&verbose, "verbose", false, "enable verbose logging to debug.log")
	listMode := flag.Bool("list", false, "use positional arguments as temporary city list")
	refreshMin := flag.Int("refresh", 15, "auto-refresh interval in minutes")
	flag.Parse()
	if *refreshMin < 0 {
		*refreshMin = 0
	}

	initDB()
	defer db.Close()
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startCacheCleaner(appCtx)

	// Handle Quick Launch or List Mode
	if flag.NArg() > 0 {
		if *listMode {
			// List mode: Start TUI with these cities as pinned
			var pinned []pinnedCity
			var cityNames []string

			// Supporta sia "città1" "città2" che "città1,città2"
			for _, arg := range flag.Args() {
				if strings.Contains(arg, ",") {
					cityNames = append(cityNames, strings.Split(arg, ",")...)
				} else {
					cityNames = append(cityNames, arg)
				}
			}

			for _, cityName := range cityNames {
				cityName = strings.TrimSpace(cityName)
				if cityName == "" {
					continue
				}
				cities, err := fetchCitiesSync(cityName)
				if err == nil && len(cities) > 0 {
					weather, err := fetchWeatherSync(cities[0])
					if err == nil {
						pinned = append(pinned, pinnedCity{City: cities[0], Weather: weather})
					}
				}
			}
			runTUI(pinned, *refreshMin)
			return
		}

		// Quick Launch mode: single city, print and exit
		cityName := strings.Join(flag.Args(), " ")
		cities, err := fetchCitiesSync(cityName)
		if err != nil {
			fmt.Printf("Errore durante la ricerca della città: %v\n", err)
			os.Exit(1)
		}
		if len(cities) == 0 {
			fmt.Printf("Città '%s' non trovata.\n", cityName)
			os.Exit(0) // Uscita pulita se non trovata, non è un errore di sistema
		}

		city := cities[0]
		weather, err := fetchWeatherSync(city)
		if err != nil {
			fmt.Printf("Errore nel recupero del meteo per %s: %v\n", city.Name, err)
			os.Exit(1)
		}

		// Stampa localizzata dei risultati
		t := getT()
		fmt.Printf("\n%s: %s (%s)\n", t.Title, city.Name, city.Country)
		fmt.Printf("- %s: %.1f°C\n", t.Temp, weather.Current.Temperature)
		fmt.Printf("- %s: %.0f%%\n", t.Humidity, weather.Current.Humidity)
		if weather.Current.WindSpeed > 0 {
			fmt.Printf("- %s: %.1f km/h\n", t.Wind, weather.Current.WindSpeed)
		}
		fmt.Printf("- %s: %s\n\n", t.Today, getWeatherDescription(weather.Current.WeatherCode))
		return
	}

	// Normal TUI mode
	favs := loadFavorites()
	runTUI(favs, *refreshMin)
}

func runTUI(pinned []pinnedCity, refresh int) {
	// Setup logging per modalità TUI
	f, err := os.OpenFile("debug.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("Error opening log file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	if verbose {
		log.SetOutput(f)
	} else {
		log.SetOutput(io.Discard)
	}

	log.Println("--- Meteo TUI started ---")

	m := initialModel()
	m.pinnedCities = pinned
	m.refreshMin = refresh

	if len(pinned) > 0 {
		m.selectedCity = &pinned[0].City
		m.weather = pinned[0].Weather
		m.pinnedIdx = 0
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Printf("[FATAL] %v", err)
		fmt.Printf("Alas, there's been an error: %v", err)
		os.Exit(1)
	}
}
