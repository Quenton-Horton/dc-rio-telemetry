// Package main implements the unified DC & Rio Logistics Engine and Digital Modernist Widget.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

type TelemetryPayload struct {
	UpdatedAt      string         `json:"updated_at"`
	FlightSyncedAt string         `json:"flight_synced_at"`
	TargetDate     string         `json:"target_date"`
	QuotaUsed      int            `json:"quota_used"`
	IAD            WeatherMetrics `json:"iad"`
	GIG            WeatherMetrics `json:"gig"`
	SafePets       bool           `json:"safe_for_poodles"`
	USDBRL         float64        `json:"usd_brl"`
	Flights        []FlightOption `json:"flights"`
}

var (
	db    *sql.DB
	cache *redis.Client
	ctx   = context.Background()

	stateMu     sync.RWMutex
	latestState TelemetryPayload

	rioImages = []string{
		"https://images.unsplash.com/photo-1483729558449-99ef09a8c325?q=80&w=2070&auto=format&fit=crop",
		"https://images.unsplash.com/photo-1516306580123-e6e52b1b7b5f?q=80&w=2098&auto=format&fit=crop",
		"https://images.unsplash.com/photo-1590513904944-7770fa30d41e?q=80&w=2070&auto=format&fit=crop",
		"https://images.unsplash.com/photo-1548688402-99a38f3f88f2?q=80&w=2070&auto=format&fit=crop",
	}

	flightPriceMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "logistics_flight_price_usd",
			Help: "Current one-way Premium Economy & Business flight prices in USD.",
		},
		[]string{"origin", "destination", "carrier"},
	)
	fxRateMetric = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "logistics_fx_usd_brl",
			Help: "Current USD to BRL exchange rate.",
		},
	)
	tarmacTempMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "logistics_tarmac_temp_fahrenheit",
			Help: "Airport tarmac temperature in Fahrenheit.",
		},
		[]string{"airport", "code"},
	)
	humidityMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "logistics_weather_humidity_percent",
			Help: "Current relative humidity percentage.",
		},
		[]string{"location", "code"},
	)
	rainMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "logistics_weather_rain_inches",
			Help: "Current rain/precipitation in inches.",
		},
		[]string{"location", "code"},
	)
)

func init() {
	prometheus.MustRegister(
		flightPriceMetric,
		fxRateMetric,
		tarmacTempMetric,
		humidityMetric,
		rainMetric,
	)
}

func initDB() {
	dbHost := os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "localhost"
	}
	connStr := fmt.Sprintf("host=%s port=5432 user=rio_admin password=secure_password_here dbname=logistics sslmode=disable", dbHost)

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatalf("FATAL: Failed to open Postgres: %v", err)
	}
	db.SetMaxOpenConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err = db.Ping(); err != nil {
		log.Fatalf("FATAL: Postgres is unreachable: %v", err)
	}
	fmt.Println("SUCCESS: Connected to Postgres logistics database.")
}

func initCache() {
	redisHost := os.Getenv("REDIS_HOST")
	if redisHost == "" {
		redisHost = "localhost:6379"
	}
	cache = redis.NewClient(&redis.Options{Addr: redisHost, DB: 0})
	if _, err := cache.Ping(ctx).Result(); err != nil {
		log.Fatalf("FATAL: Redis cache is unreachable: %v", err)
	}
	fmt.Println("SUCCESS: Connected to Redis cache.")
}

func initSchema() {
	schema := `
    CREATE TABLE IF NOT EXISTS flight_routes (
        id SERIAL PRIMARY KEY,
        origin VARCHAR(3) NOT NULL,
        destination VARCHAR(3) NOT NULL,
        price_usd NUMERIC(10, 2) NOT NULL,
        carrier VARCHAR(120),
        date_logged TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );

    CREATE TABLE IF NOT EXISTS pet_embargoes (
        id SERIAL PRIMARY KEY,
        flight_date DATE NOT NULL,
        iad_temp_f NUMERIC(5, 2) NOT NULL,
        gig_temp_f NUMERIC(5, 2) NOT NULL,
        is_safe BOOLEAN NOT NULL 
    );

    CREATE TABLE IF NOT EXISTS fx_records (
        id SERIAL PRIMARY KEY,
        usd_brl NUMERIC(10, 4) NOT NULL,
        date_logged TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );
    `
	if _, err := db.Exec(schema); err != nil {
		log.Fatalf("FATAL: Failed to initialize schema: %v", err)
	}

	db.Exec(`ALTER TABLE flight_routes ALTER COLUMN carrier TYPE VARCHAR(120);`)
	db.Exec(`ALTER TABLE flight_routes ADD COLUMN IF NOT EXISTS departure_time TIMESTAMP;`)
	db.Exec(`ALTER TABLE flight_routes ADD COLUMN IF NOT EXISTS arrival_time TIMESTAMP;`)
	db.Exec(`ALTER TABLE flight_routes ADD COLUMN IF NOT EXISTS cabin VARCHAR(30) DEFAULT '';`)
	db.Exec(`ALTER TABLE flight_routes ADD COLUMN IF NOT EXISTS airplane VARCHAR(80) DEFAULT '';`)
	db.Exec(`ALTER TABLE flight_routes ADD COLUMN IF NOT EXISTS legroom VARCHAR(50) DEFAULT '';`)
	db.Exec(`ALTER TABLE pet_embargoes ADD COLUMN IF NOT EXISTS iad_humidity NUMERIC(5, 2) DEFAULT 0;`)
	db.Exec(`ALTER TABLE pet_embargoes ADD COLUMN IF NOT EXISTS gig_humidity NUMERIC(5, 2) DEFAULT 0;`)
	db.Exec(`ALTER TABLE pet_embargoes ADD COLUMN IF NOT EXISTS iad_rain NUMERIC(5, 2) DEFAULT 0;`)
	db.Exec(`ALTER TABLE pet_embargoes ADD COLUMN IF NOT EXISTS gig_rain NUMERIC(5, 2) DEFAULT 0;`)

	fmt.Println("SUCCESS: Postgres schema initialized and synced.")
}

func seedMetricsFromDB() {
	fmt.Println("Seeding Prometheus gauges from historical Postgres records...")
	stateMu.Lock()
	defer stateMu.Unlock()
	latestState.UpdatedAt = time.Now().Format("Jan 02 15:04 MST")
	latestState.TargetDate = time.Now().AddDate(0, 1, 0).Format("2006-01-02")

	monthKey := fmt.Sprintf("flights_api_calls:%s", time.Now().Format("2006-01"))
	callsThisMonth, _ := cache.Get(ctx, monthKey).Int()
	latestState.QuotaUsed = callsThisMonth

	rows, err := db.Query(`
		SELECT origin, destination, COALESCE(carrier, 'UNKNOWN'), price_usd,
		       COALESCE(cabin, ''), COALESCE(airplane, ''), COALESCE(legroom, ''),
		       COALESCE(TO_CHAR(departure_time, 'YYYY-MM-DD HH24:MI'), 'N/A'),
		       COALESCE(TO_CHAR(date_logged, 'Jan 02 15:04'), 'Seeded')
		FROM flight_routes 
		WHERE UPPER(carrier) NOT LIKE '%COPA%' AND UPPER(carrier) NOT LIKE '%CACHED%'
		ORDER BY date_logged DESC, price_usd ASC 
		LIMIT 6`)
	if err == nil {
		defer rows.Close()
		var seeded []FlightOption
		for rows.Next() {
			var opt FlightOption
			var loggedAt string
			if err := rows.Scan(&opt.Origin, &opt.Destination, &opt.Carrier, &opt.PriceUSD, &opt.Cabin, &opt.Airplane, &opt.Legroom, &opt.DepartureTime, &loggedAt); err == nil {
				seeded = append(seeded, opt)
				flightPriceMetric.WithLabelValues(opt.Origin, opt.Destination, opt.Carrier).Set(opt.PriceUSD)
				if latestState.FlightSyncedAt == "" {
					latestState.FlightSyncedAt = loggedAt
				}
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("[SEED] Row iteration error: %v", err)
		}
		latestState.Flights = seeded
		fmt.Printf("[SEED] Restored %d Wide-Body Flight Options from Postgres\n", len(seeded))
	}

	var usdBrl float64
	err = db.QueryRow(`SELECT usd_brl FROM fx_records ORDER BY date_logged DESC LIMIT 1`).Scan(&usdBrl)
	if err == nil {
		fxRateMetric.Set(usdBrl)
		latestState.USDBRL = usdBrl
	}

	var iadTemp, gigTemp, iadHum, gigHum, iadRain, gigRain float64
	var isSafe bool
	err = db.QueryRow(`SELECT iad_temp_f, gig_temp_f, COALESCE(iad_humidity, 0), COALESCE(gig_humidity, 0), COALESCE(iad_rain, 0), COALESCE(gig_rain, 0), is_safe FROM pet_embargoes ORDER BY id DESC LIMIT 1`).Scan(&iadTemp, &gigTemp, &iadHum, &gigHum, &iadRain, &gigRain, &isSafe)
	if err == nil {
		tarmacTempMetric.WithLabelValues("Washington Dulles", "IAD").Set(iadTemp)
		tarmacTempMetric.WithLabelValues("Rio de Janeiro Galeao", "GIG").Set(gigTemp)
		humidityMetric.WithLabelValues("Washington Dulles", "IAD").Set(iadHum)
		humidityMetric.WithLabelValues("Rio de Janeiro Galeao", "GIG").Set(gigHum)
		rainMetric.WithLabelValues("Washington Dulles", "IAD").Set(iadRain)
		rainMetric.WithLabelValues("Rio de Janeiro Galeao", "GIG").Set(gigRain)

		latestState.IAD = WeatherMetrics{TempF: iadTemp, Humidity: iadHum, Rain: iadRain}
		latestState.GIG = WeatherMetrics{TempF: gigTemp, Humidity: gigHum, Rain: gigRain}
		latestState.SafePets = isSafe
	}
}

func updateWeatherAndFX() {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		iadWeather, err1 := FetchAirportWeather("38.9531", "-77.4565")
		gigWeather, err2 := FetchAirportWeather("-22.8089", "-43.2436")

		if err1 == nil && err2 == nil {
			tarmacTempMetric.WithLabelValues("Washington Dulles", "IAD").Set(iadWeather.TempF)
			tarmacTempMetric.WithLabelValues("Rio de Janeiro Galeao", "GIG").Set(gigWeather.TempF)
			humidityMetric.WithLabelValues("Washington Dulles", "IAD").Set(iadWeather.Humidity)
			humidityMetric.WithLabelValues("Rio de Janeiro Galeao", "GIG").Set(gigWeather.Humidity)
			rainMetric.WithLabelValues("Washington Dulles", "IAD").Set(iadWeather.Rain)
			rainMetric.WithLabelValues("Rio de Janeiro Galeao", "GIG").Set(gigWeather.Rain)

			isSafe := (iadWeather.TempF >= 45 && iadWeather.TempF <= 85) && (gigWeather.TempF >= 45 && gigWeather.TempF <= 85)

			stateMu.Lock()
			latestState.UpdatedAt = time.Now().Format("Jan 02 15:04 MST")
			latestState.IAD = iadWeather
			latestState.GIG = gigWeather
			latestState.SafePets = isSafe
			stateMu.Unlock()

			_, _ = db.Exec(`INSERT INTO pet_embargoes (flight_date, iad_temp_f, gig_temp_f, iad_humidity, gig_humidity, iad_rain, gig_rain, is_safe) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				time.Now().Format("2006-01-02"), iadWeather.TempF, gigWeather.TempF, iadWeather.Humidity, gigWeather.Humidity, iadWeather.Rain, gigWeather.Rain, isSafe)
		} else {
			log.Printf("[METEO] API Error: %v, %v", err1, err2)
		}
	}()

	go func() {
		defer wg.Done()
		usd_brl, err := FetchUSDBRL()
		if err == nil {
			fxRateMetric.Set(usd_brl)
			stateMu.Lock()
			latestState.USDBRL = usd_brl
			stateMu.Unlock()
			_, _ = db.Exec(`INSERT INTO fx_records (usd_brl) VALUES ($1)`, usd_brl)
		} else {
			log.Printf("[FX] API Error: %v", err)
		}
	}()

	wg.Wait()
}

func updateFlights() {
	monthKey := fmt.Sprintf("flights_api_calls:%s", time.Now().Format("2006-01"))
	callsThisMonth, _ := cache.Get(ctx, monthKey).Int()

	if callsThisMonth >= 49 {
		log.Printf("[FLIGHTS] SKIPPED: Monthly API quota (%d/50) reached. Retaining seeded metrics.", callsThisMonth)
		return
	}

	date := time.Now().AddDate(0, 1, 0).Format("2006-01-02")
	options, callsUsed, err := FetchPremiumAndBusinessFlights("IAD", "GIG", date)

	if err == nil && len(options) > 0 {
		newCount, _ := cache.IncrBy(ctx, monthKey, int64(callsUsed)).Result()
		cache.Expire(ctx, monthKey, 35*24*time.Hour)

		flightPriceMetric.Reset()

		for _, opt := range options {
			flightPriceMetric.WithLabelValues(opt.Origin, opt.Destination, opt.Carrier).Set(opt.PriceUSD)
			_, dbErr := db.Exec(`INSERT INTO flight_routes (origin, destination, price_usd, carrier, cabin, airplane, legroom, departure_time, arrival_time) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				opt.Origin, opt.Destination, opt.PriceUSD, opt.Carrier, opt.Cabin, opt.Airplane, opt.Legroom, opt.DepartureTime, opt.ArrivalTime)
			if dbErr != nil {
				log.Printf("[FLIGHTS] DB Insert Failed for %s: %v", opt.Carrier, dbErr)
			}
		}

		syncTime := time.Now().Format("Jan 02 15:04")
		stateMu.Lock()
		latestState.Flights = options
		latestState.TargetDate = date
		latestState.FlightSyncedAt = syncTime
		latestState.QuotaUsed = int(newCount)
		stateMu.Unlock()

		fmt.Printf("[FLIGHTS] Quota %d/50 | Synced %d Wide-Body PremEcon/Business options for %s at %s\n",
			newCount, len(options), date, syncTime)
	} else {
		log.Printf("[FLIGHTS] API Error: %v", err)
	}
}

func handleWidgetUI(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("Error loading index.html: %v", err), http.StatusInternalServerError)
		return
	}

	bgImage := rioImages[rand.Intn(len(rioImages))]

	stateMu.RLock()
	data := map[string]interface{}{
		"Temp":           fmt.Sprintf("%.1f", latestState.GIG.TempF),
		"Rain":           fmt.Sprintf("%.2f", latestState.GIG.Rain),
		"FX":             fmt.Sprintf("%.4f", latestState.USDBRL),
		"Humidity":       fmt.Sprintf("%.0f", latestState.GIG.Humidity),
		"BgImage":        bgImage,
		"IADTemp":        fmt.Sprintf("%.1f", latestState.IAD.TempF),
		"IADHumidity":    fmt.Sprintf("%.0f", latestState.IAD.Humidity),
		"SafePets":       fmt.Sprintf("%t", latestState.SafePets),
		"Flights":        latestState.Flights,
		"FlightSyncedAt": latestState.FlightSyncedAt,
		"TargetDate":     latestState.TargetDate,
		"QuotaUsed":      latestState.QuotaUsed,
	}
	stateMu.RUnlock()

	tmpl.Execute(w, data)
}

func handleTelemetryJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	stateMu.RLock()
	defer stateMu.RUnlock()
	json.NewEncoder(w).Encode(latestState)
}

func cfAccessMiddleware(verifier *oidc.IDTokenVerifier, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if verifier == nil {
			next(w, r)
			return
		}

		token := r.Header.Get("Cf-Access-Jwt-Assertion")
		if token == "" {
			if cookie, err := r.Cookie("CF_Authorization"); err == nil {
				token = cookie.Value
			}
		}

		if token == "" {
			http.Error(w, "Unauthorized: Missing Cloudflare Access JWT", http.StatusUnauthorized)
			return
		}

		if _, err := verifier.Verify(r.Context(), token); err != nil {
			log.Printf("[AUTH] Rejected invalid Cloudflare JWT from %s: %v", r.RemoteAddr, err)
			http.Error(w, "Unauthorized: Invalid Cloudflare Access JWT", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

func main() {
	fmt.Println("Booting Unified DC & Rio Logistics Engine (Wide-Body PremEcon & Business)...")
	initDB()
	initCache()
	initSchema()
	seedMetricsFromDB()

	var verifier *oidc.IDTokenVerifier
	teamDomain := os.Getenv("CF_TEAM_DOMAIN")
	policyAUD := os.Getenv("CF_POLICY_AUD")

	if teamDomain != "" && policyAUD != "" {
		keySet := oidc.NewRemoteKeySet(ctx, fmt.Sprintf("%s/cdn-cgi/access/certs", teamDomain))
		verifier = oidc.NewVerifier(teamDomain, keySet, &oidc.Config{
			ClientID: policyAUD,
		})
		log.Printf("SUCCESS: Cloudflare Zero Trust JWT verification enabled for %s", teamDomain)
	} else {
		log.Println("WARNING: CF_TEAM_DOMAIN or CF_POLICY_AUD unset; running without JWT enforcement")
	}

	http.HandleFunc("/", cfAccessMiddleware(verifier, handleWidgetUI))
	http.HandleFunc("/api/telemetry", cfAccessMiddleware(verifier, handleTelemetryJSON))
	http.HandleFunc("/api/refresh-flights", cfAccessMiddleware(verifier, func(w http.ResponseWriter, r *http.Request) {
		updateFlights()
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}))
	http.Handle("/metrics", promhttp.Handler())

	go func() {
		log.Printf("Starting telemetry & metrics server on :2112...")
		if err := http.ListenAndServe(":2112", nil); err != nil {
			log.Fatalf("Server on :2112 failed: %v", err)
		}
	}()
	go func() {
		log.Printf("Starting Digital Modernist Widget UI server on :8080...")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatalf("Server on :8080 failed: %v", err)
		}
	}()

	fmt.Println("\n--- Initiating Startup Telemetry Cycle ---")
	updateWeatherAndFX()
	updateFlights()
	fmt.Println("--- Startup Telemetry Cycle Complete ---")

	weatherTicker := time.NewTicker(15 * time.Minute)
	flightTicker := time.NewTicker(24 * time.Hour)

	for {
		select {
		case <-weatherTicker.C:
			updateWeatherAndFX()
		case <-flightTicker.C:
			updateFlights()
		}
	}
}
