package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSanitizeInput(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"  Milano  ", "Milano"},
		{"LongName" + strings.Repeat("a", 150), "LongName" + strings.Repeat("a", 92)}, // Truncation at 100
		{"Saint-Étienne", "Saint-Étienne"},
	}

	for _, tt := range tests {
		got := sanitizeInput(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeInput(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestMain(m *testing.M) {
	initDB()
	code := m.Run()
	db.Close()
	os.Remove("./meteo.db")
	os.Exit(code)
}

// TestFetchCities verifica la ricerca delle città con vari scenari, inclusi errori e caratteri speciali.
func TestFetchCities(t *testing.T) {
	// Mock server per simulare le risposte dell'API di geocodifica
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("name")
		if query == "Milano" || query == "Saint-Étienne" {
			res := GeocodingResponse{
				Results: []City{
					{ID: 1, Name: query, Country: "Italy", Latitude: 45.4642, Longitude: 9.1899},
				},
			}
			json.NewEncoder(w).Encode(res)
		} else if query == "Inesistente" {
			res := GeocodingResponse{Results: nil}
			json.NewEncoder(w).Encode(res)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	// Sovrascrive l'URL di base per indirizzare le richieste al server mock
	oldURL := geocodingBaseURL
	geocodingBaseURL = ts.URL
	defer func() { geocodingBaseURL = oldURL }()

	tests := []struct {
		name    string
		query   string
		wantLen int
		wantErr bool
	}{
		{"Città valida", "Milano", 1, false},
		{"Città inesistente", "Inesistente", 0, false},
		{"Caratteri speciali", "Saint-Étienne", 1, false},
		{"Query troppo corta", "M", 0, false},
		{"Errore del server", "Error", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := fetchCities(tt.query)
			msg := cmd()

			switch m := msg.(type) {
			case gotCitiesMsg:
				if len(m) != tt.wantLen {
					t.Errorf("fetchCities() ha ottenuto %d elementi, ne voleva %d", len(m), tt.wantLen)
				}
				if tt.wantErr {
					t.Errorf("fetchCities() si aspettava un errore ma ha avuto successo")
				}
			case errMsg:
				if !tt.wantErr {
					t.Errorf("fetchCities() errore inaspettato: %v", m)
				}
			default:
				t.Errorf("fetchCities() ha restituito un tipo di messaggio inaspettato: %T", msg)
			}
		})
	}
}

// TestFetchWeather verifica il recupero dei dati meteo per coordinate valide e non.
func TestFetchWeather(t *testing.T) {
	// Mock server per simulare le risposte dell'API meteorologica
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lat := r.URL.Query().Get("latitude")
		if lat == "45.464200" {
			res := WeatherResponse{}
			res.Current.Temperature = 20.5
			res.Current.Humidity = 65.0
			res.Current.WeatherCode = 0
			res.Current.Precipitation = 0.5
			// Aggiungi dati fittizi per la previsione
			res.Daily.Time = []string{"2026-04-13", "2026-04-14", "2026-04-15", "2026-04-16", "2026-04-17"}
			res.Daily.TemperatureMax = []float64{22.0, 23.0, 24.0, 25.0, 26.0}
			res.Daily.TemperatureMin = []float64{18.0, 19.0, 20.0, 21.0, 22.0}
			res.Daily.HumidityMax = []float64{70.0, 75.0, 80.0, 85.0, 90.0}
			res.Daily.PrecipitationSum = []float64{0.0, 1.0, 2.0, 3.0, 4.0}
			res.Daily.WeatherCode = []int{0, 1, 2, 3, 4}
			json.NewEncoder(w).Encode(res)
		} else if lat == "0.000000" {
			w.WriteHeader(http.StatusBadRequest)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	// Sovrascrive l'URL di base per il test
	oldURL := weatherBaseURL
	weatherBaseURL = ts.URL
	defer func() { weatherBaseURL = oldURL }()

	tests := []struct {
		name     string
		city     City
		wantTemp float64
		wantErr  bool
	}{
		{"Coordinate valide", City{Latitude: 45.4642, Longitude: 9.1899}, 20.5, false},
		{"Coordinate non valide", City{Latitude: 0, Longitude: 0}, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := fetchWeather(tt.city)
			msg := cmd()

			switch m := msg.(type) {
			case gotWeatherMsg:
				if m.weather.Current.Temperature != tt.wantTemp {
					t.Errorf("fetchWeather() ha ottenuto temp %.1f, voleva %.1f", m.weather.Current.Temperature, tt.wantTemp)
				}
				if tt.wantErr {
					t.Errorf("fetchWeather() si aspettava un errore ma ha avuto successo")
				}
			case errMsg:
				if !tt.wantErr {
					t.Errorf("fetchWeather() errore inaspettato: %v", m)
				}
			default:
				t.Errorf("fetchWeather() ha restituito un tipo di messaggio inaspettato: %T", msg)
			}
		})
	}
}

func TestGetWeatherDescription(t *testing.T) {
	oldLang := userLang
	userLang = "it"
	defer func() { userLang = oldLang }()

	tests := []struct {
		code int
		want string
	}{
		{0, "Sereno"},
		{2, "Nuvoloso"},
		{45, "Nebbia"},
		{51, "Pioggia"},
		{71, "Neve"},
		{80, "Acquazzoni"},
		{95, "Temporale"},
		{999, "Sconosciuto"},
	}

	for _, tt := range tests {
		got := getWeatherDescription(tt.code)
		if got != tt.want {
			t.Errorf("getWeatherDescription(%d) = %s, want %s", tt.code, got, tt.want)
		}
	}
}

func TestCache(t *testing.T) {
	initDB()
	// No defer db.Close() here because it's global and needed for other tests

	key := "test_key"
	data := City{Name: "TestCity"}

	saveToCache(key, data)

	var retrieved City
	if !getFromCache(key, &retrieved, 1*time.Minute) {
		t.Fatal("getFromCache failed to retrieve saved data")
	}

	if retrieved.Name != data.Name {
		t.Errorf("Retrieved name %s, want %s", retrieved.Name, data.Name)
	}

	// Test expiration
	if getFromCache(key, &retrieved, -1*time.Second) {
		t.Error("getFromCache should have failed for expired data")
	}
}

func TestFavorites(t *testing.T) {
	favs := []pinnedCity{
		{City: City{Name: "Milan"}},
		{City: City{Name: "Rome"}},
	}

	saveFavorites(favs)
	defer os.Remove(favoritesFile)

	loaded := loadFavorites()
	if len(loaded) != len(favs) {
		t.Errorf("Loaded %d favorites, want %d", len(loaded), len(favs))
	}

	if loaded[0].City.Name != "Milan" {
		t.Errorf("First favorite name %s, want Milan", loaded[0].City.Name)
	}
}

func TestUpdate(t *testing.T) {
	m := initialModel()

	// Test Quit
	msg := tea.KeyMsg{Type: tea.KeyEsc}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Error("Update(Esc) should return tea.Quit command")
	}

	// Test Search Debounce
	tiMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Milan")}
	m.textInput.SetValue("Milan")
	newModel, cmd := m.Update(tiMsg)
	if cmd == nil {
		t.Error("Update(typing) should return a debounce command")
	}
	m = newModel.(model)

	// Test gotCitiesMsg
	cities := []City{{ID: 1, Name: "Milan"}}
	newModel, cmd = m.Update(gotCitiesMsg(cities))
	m = newModel.(model)
	if len(m.cityList.Items()) != 1 {
		t.Errorf("Update(gotCitiesMsg) set %d items, want 1", len(m.cityList.Items()))
	}

	// Test navigation (Left/Right)
	m.weather = &WeatherResponse{}
	m.weather.Daily.Time = []string{"2026-04-14", "2026-04-15"}
	msg = tea.KeyMsg{Type: tea.KeyRight}
	newModel, _ = m.Update(msg)
	m = newModel.(model)
	if m.selectedDay != 1 {
		t.Errorf("Update(KeyRight) selectedDay = %d, want 1", m.selectedDay)
	}
}

func TestSyncFunctions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res := GeocodingResponse{Results: []City{{Name: "Milan"}}}
		json.NewEncoder(w).Encode(res)
	}))
	defer ts.Close()

	oldGeo := geocodingBaseURL
	geocodingBaseURL = ts.URL
	defer func() { geocodingBaseURL = oldGeo }()

	cities, err := fetchCitiesSync("Milan")
	if err != nil || len(cities) == 0 {
		t.Errorf("fetchCitiesSync failed: %v", err)
	}
}

// TestGetWeatherIcon verifica che la funzione restituisca icone valide per vari codici meteo.
func TestGetWeatherIcon(t *testing.T) {
	tests := []struct {
		code int
	}{
		{0},   // Sereno
		{3},   // Nuvoloso
		{45},  // Nebbia
		{51},  // Pioggerellina
		{71},  // Neve
		{80},  // Acquazzone
		{95},  // Temporale
		{999}, // Sconosciuto
	}

	for _, tt := range tests {
		got := getWeatherIcon(tt.code)
		if got == "" {
			t.Errorf("getWeatherIcon(%d) ha restituito una stringa vuota", tt.code)
		}
	}
}
