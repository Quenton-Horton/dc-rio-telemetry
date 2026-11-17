// Package main - fetchers.go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var apiClient = &http.Client{
	Timeout: 20 * time.Second,
}

// --- 1. EXPANDED WEATHER & TARMAC FETCHER (Open-Meteo) ---
type WeatherMetrics struct {
	TempF         float64 `json:"temperature_2m"`
	Humidity      float64 `json:"relative_humidity_2m"`
	Precipitation float64 `json:"precipitation"`
	Rain          float64 `json:"rain"`
}

type MeteoResponse struct {
	Current WeatherMetrics `json:"current"`
}

func FetchAirportWeather(lat, lon string) (WeatherMetrics, error) {
	url := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%s&longitude=%s&current=temperature_2m,relative_humidity_2m,precipitation,rain&temperature_unit=fahrenheit&precipitation_unit=inch",
		lat, lon,
	)

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "DCRioLogisticsEngine/2.0")

	resp, err := apiClient.Do(req)
	if err != nil {
		return WeatherMetrics{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return WeatherMetrics{}, fmt.Errorf("HTTP %d error from Open-Meteo", resp.StatusCode)
	}

	var data MeteoResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return WeatherMetrics{}, err
	}
	return data.Current, nil
}

// --- 2. USD/BRL CURRENCY FETCHER (Frankfurter) ---
type FXResponse struct {
	Rates struct {
		BRL float64 `json:"BRL"`
	} `json:"rates"`
}

func FetchUSDBRL() (float64, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.frankfurter.app/latest?from=USD&to=BRL", nil)
	resp, err := apiClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var data FXResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, err
	}
	return data.Rates.BRL, nil
}

// --- 3. WIDE-BODY PREMIUM ECONOMY & BUSINESS CLASS FETCHER (SerpApi) ---
type FlightOption struct {
	Origin        string  `json:"origin"`
	Destination   string  `json:"destination"`
	Carrier       string  `json:"carrier"`
	Cabin         string  `json:"cabin"`
	PriceUSD      float64 `json:"price_usd"`
	DepartureTime string  `json:"departure_time"`
	ArrivalTime   string  `json:"arrival_time"`
	Airplane      string  `json:"airplane"`
	Legroom       string  `json:"legroom"`
	Stops         int     `json:"stops"`
}

type serpFlightLeg struct {
	Airline          string `json:"airline"`
	Airplane         string `json:"airplane"`
	Legroom          string `json:"legroom"`
	TravelClass      string `json:"travel_class"`
	DepartureAirport struct {
		Time string `json:"time"`
		ID   string `json:"id"`
	} `json:"departure_airport"`
	ArrivalAirport struct {
		Time string `json:"time"`
		ID   string `json:"id"`
	} `json:"arrival_airport"`
}

type serpFlightGroup struct {
	Price   float64         `json:"price"`
	Flights []serpFlightLeg `json:"flights"`
}

type SerpFlightResponse struct {
	BestFlights  []serpFlightGroup `json:"best_flights"`
	OtherFlights []serpFlightGroup `json:"other_flights"`
}

func isExcludedCarrier(airline string) bool {
	upper := strings.ToUpper(airline)
	excluded := []string{"COPA", "AVIANCA", "ARAJET", "GOL", "AZUL"}
	for _, ex := range excluded {
		if strings.Contains(upper, ex) {
			return true
		}
	}
	return false
}

func fetchClassOptions(origin, dest, date, travelClass, cabinLabel string, maxPerClass int) ([]FlightOption, error) {
	apiKey := os.Getenv("SERPAPI_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("SERPAPI_KEY is missing from environment")
	}

	url := fmt.Sprintf(
		"https://serpapi.com/search.json?engine=google_flights&departure_id=%s&arrival_id=%s&outbound_date=%s&type=2&travel_class=%s&currency=USD&hl=en&api_key=%s",
		origin, dest, date, travelClass, apiKey,
	)

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}

	var data SerpFlightResponse
	if err := json.Unmarshal(bodyBytes, &data); err != nil {
		return nil, fmt.Errorf("JSON unmarshal error: %v", err)
	}

	allGroups := append(data.BestFlights, data.OtherFlights...)
	var options []FlightOption
	seenCarrier := make(map[string]bool)

	for _, group := range allGroups {
		if len(group.Flights) == 0 || group.Price <= 0 {
			continue
		}

		firstLeg := group.Flights[0]
		lastLeg := group.Flights[len(group.Flights)-1]

		skip := false
		for _, leg := range group.Flights {
			if isExcludedCarrier(leg.Airline) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		longHaulLeg := lastLeg
		if longHaulLeg.Airplane == "" {
			longHaulLeg = firstLeg
		}

		primaryCarrier := firstLeg.Airline
		carrierKey := strings.ToUpper(primaryCarrier)
		if seenCarrier[carrierKey] {
			continue
		}
		seenCarrier[carrierKey] = true

		dep := strings.TrimSpace(firstLeg.DepartureAirport.Time)
		if dep == "" {
			dep = date + " 00:00"
		}
		arr := strings.TrimSpace(lastLeg.ArrivalAirport.Time)
		if arr == "" {
			arr = date + " 00:00"
		}

		displayCarrier := fmt.Sprintf("%s (%s)", primaryCarrier, cabinLabel)
		if longHaulLeg.Airplane != "" {
			displayCarrier = fmt.Sprintf("%s • %s (%s)", primaryCarrier, longHaulLeg.Airplane, cabinLabel)
		}

		options = append(options, FlightOption{
			Origin:        origin,
			Destination:   dest,
			Carrier:       displayCarrier,
			Cabin:         cabinLabel,
			PriceUSD:      group.Price,
			DepartureTime: dep,
			ArrivalTime:   arr,
			Airplane:      longHaulLeg.Airplane,
			Legroom:       longHaulLeg.Legroom,
			Stops:         len(group.Flights) - 1,
		})

		if len(options) >= maxPerClass {
			break
		}
	}

	return options, nil
}

func FetchPremiumAndBusinessFlights(origin, dest, date string) ([]FlightOption, int, error) {
	var combined []FlightOption
	apiCallsUsed := 0

	// 1. Business / Lie-Flat (travel_class=3)
	bizOptions, errBiz := fetchClassOptions(origin, dest, date, "3", "BUSINESS", 3)
	if errBiz == nil {
		apiCallsUsed++
		combined = append(combined, bizOptions...)
	}

	// 2. Premium Economy (travel_class=2)
	premOptions, errPrem := fetchClassOptions(origin, dest, date, "2", "PREM ECON", 3)
	if errPrem == nil {
		apiCallsUsed++
		combined = append(combined, premOptions...)
	}

	if len(combined) == 0 {
		return nil, apiCallsUsed, fmt.Errorf("no wide-body Prem Econ or Business flights returned (bizErr=%v, premErr=%v)", errBiz, errPrem)
	}

	return combined, apiCallsUsed, nil
}
