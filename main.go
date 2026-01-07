package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

type APIResponse struct {
	Data   Data    `json:"data"`
	Error  *string `json:"error"`
	Status int     `json:"status"`
}

type Data struct {
	Metrics        map[string][]Metric `json:"metrics"`
	LatestTimeZone string              `json:"latest_time_zone"`
}

type Metric struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

type TimeValue struct {
	Value     float64 `json:"value"`
	Timestamp int64   `json:"timestamp"`
}

type TimeSeriesMetric struct {
	DayStartTimestamp int64       `json:"day_start_timestamp"`
	Title             string      `json:"title"`
	Values            []TimeValue `json:"values"`
	LastReading       float64     `json:"last_reading"`
	Unit              string      `json:"unit"`
	Subtitle          string      `json:"subtitle"`
	Avg               float64     `json:"avg"`
	Total             float64     `json:"total"`
}

type SimpleMetric struct {
	Value             *float64 `json:"value"`
	Title             string   `json:"title"`
	DayStartTimestamp int64    `json:"day_start_timestamp"`
}

type SleepMetric struct {
	DayStartTimestamp int64    `json:"day_start_timestamp"`
	Score             *float64 `json:"score"`
	TotalSleep        *float64 `json:"total_sleep"`
	Efficiency        *float64 `json:"efficiency"`
	TimeInBed         *float64 `json:"time_in_bed"`
	DeepSleep         *float64 `json:"deep_sleep"`
	LightSleep        *float64 `json:"light_sleep"`
	RemSleep          *float64 `json:"rem_sleep"`
}

// Track the latest timestamp seen across all metrics
var globalLatestTimestamp int64

// Track last pushed timestamp per metric to avoid duplicates
var (
	lastPushedTimestamp = make(map[string]int64)
	lastPushedMu        sync.Mutex
)

// RemoteWriteClient sends metrics to a Prometheus remote write endpoint
type RemoteWriteClient struct {
	url    string
	client *http.Client
}

func NewRemoteWriteClient(url string) *RemoteWriteClient {
	return &RemoteWriteClient{
		url:    url,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *RemoteWriteClient) Write(timeseries []prompb.TimeSeries) error {
	req := &prompb.WriteRequest{Timeseries: timeseries}
	data, err := req.Marshal()
	if err != nil {
		return fmt.Errorf("marshaling write request: %w", err)
	}

	compressed := snappy.Encode(nil, data)
	httpReq, err := http.NewRequest("POST", c.url, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("remote write failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func buildTimeSeries(metricName string, value float64, timestampMs int64) prompb.TimeSeries {
	return prompb.TimeSeries{
		Labels: []prompb.Label{
			{Name: "__name__", Value: metricName},
		},
		Samples: []prompb.Sample{
			{Value: value, Timestamp: timestampMs},
		},
	}
}

// MetricConfig defines how to extract and display a metric
type MetricConfig struct {
	MetricType     string // "timeseries", "simple", "sleep"
	Field          string // "last", "avg", "total", "value"
	DisplayName    string
	Unit           string
	IsDuration     bool
	PrometheusName string // metric name for remote write
}

// metricRegistry maps metric type names to their configurations
var metricRegistry = map[string]MetricConfig{
	// Heart & Activity - TimeSeriesMetric
	"hr":    {MetricType: "timeseries", Field: "last", DisplayName: "HEART RATE", Unit: "BPM", PrometheusName: "ultrahuman_heart_rate_bpm"},
	"hrv":   {MetricType: "timeseries", Field: "last", DisplayName: "HEART RATE VARIABILITY", Unit: "ms", PrometheusName: "ultrahuman_hrv_ms"},
	"temp":  {MetricType: "timeseries", Field: "last", DisplayName: "SKIN TEMPERATURE", Unit: "°C", PrometheusName: "ultrahuman_skin_temperature_celsius"},
	"spo2":  {MetricType: "timeseries", Field: "avg", DisplayName: "SPO2 (Blood Oxygen)", Unit: "%", PrometheusName: "ultrahuman_spo2_percent"},
	"steps": {MetricType: "timeseries", Field: "total", DisplayName: "STEPS", Unit: "", PrometheusName: "ultrahuman_steps_total"},

	// Activity - SimpleMetric
	"movement_index":  {MetricType: "simple", DisplayName: "MOVEMENT INDEX", Unit: "", PrometheusName: "ultrahuman_movement_index"},
	"active_minutes":  {MetricType: "simple", DisplayName: "ACTIVE MINUTES", Unit: "min", PrometheusName: "ultrahuman_active_minutes"},
	"recovery_index":  {MetricType: "simple", DisplayName: "RECOVERY INDEX", Unit: "", PrometheusName: "ultrahuman_recovery_index"},
	"recovery":        {MetricType: "simple", DisplayName: "RECOVERY", Unit: "", PrometheusName: "ultrahuman_recovery"},
	"vo2_max":         {MetricType: "simple", DisplayName: "VO2 MAX", Unit: "ml/kg/min", PrometheusName: "ultrahuman_vo2_max"},

	// Temperature - SimpleMetric
	"temperature_deviation":    {MetricType: "simple", DisplayName: "TEMPERATURE DEVIATION", Unit: "°C", PrometheusName: "ultrahuman_temperature_deviation_celsius"},
	"average_body_temperature": {MetricType: "simple", DisplayName: "AVG BODY TEMP", Unit: "°C", PrometheusName: "ultrahuman_avg_body_temperature_celsius"},

	// Sleep - SimpleMetric
	"sleep_score":       {MetricType: "simple", DisplayName: "SLEEP SCORE", Unit: "", PrometheusName: "ultrahuman_sleep_score"},
	"total_sleep":       {MetricType: "simple", DisplayName: "TOTAL SLEEP", Unit: "", IsDuration: true, PrometheusName: "ultrahuman_total_sleep_minutes"},
	"sleep_efficiency":  {MetricType: "simple", DisplayName: "SLEEP EFFICIENCY", Unit: "%", PrometheusName: "ultrahuman_sleep_efficiency_percent"},
	"deep_sleep":        {MetricType: "simple", DisplayName: "DEEP SLEEP", Unit: "", IsDuration: true, PrometheusName: "ultrahuman_deep_sleep_minutes"},
	"light_sleep":       {MetricType: "simple", DisplayName: "LIGHT SLEEP", Unit: "", IsDuration: true, PrometheusName: "ultrahuman_light_sleep_minutes"},
	"rem_sleep":         {MetricType: "simple", DisplayName: "REM SLEEP", Unit: "", IsDuration: true, PrometheusName: "ultrahuman_rem_sleep_minutes"},
	"time_in_bed":       {MetricType: "simple", DisplayName: "TIME IN BED", Unit: "", IsDuration: true, PrometheusName: "ultrahuman_time_in_bed_minutes"},
	"sleep_rhr":         {MetricType: "simple", DisplayName: "SLEEP RESTING HR", Unit: "BPM", PrometheusName: "ultrahuman_sleep_rhr_bpm"},
	"night_rhr":         {MetricType: "simple", DisplayName: "SLEEP RESTING HR", Unit: "BPM", PrometheusName: "ultrahuman_sleep_rhr_bpm"},
	"avg_sleep_hrv":     {MetricType: "simple", DisplayName: "SLEEP HRV", Unit: "ms", PrometheusName: "ultrahuman_avg_sleep_hrv_ms"},
	"hr_drop":           {MetricType: "simple", DisplayName: "HR DROP (Sleep)", Unit: "BPM", PrometheusName: "ultrahuman_hr_drop_bpm"},
	"restorative_sleep": {MetricType: "simple", DisplayName: "RESTORATIVE SLEEP", Unit: "", PrometheusName: "ultrahuman_restorative_sleep"},
	"morning_alertness": {MetricType: "simple", DisplayName: "MORNING ALERTNESS", Unit: "", PrometheusName: "ultrahuman_morning_alertness"},
	"full_sleep_cycles": {MetricType: "simple", DisplayName: "SLEEP CYCLES", Unit: "", PrometheusName: "ultrahuman_full_sleep_cycles"},
	"tosses_and_turns":  {MetricType: "simple", DisplayName: "TOSSES & TURNS", Unit: "", PrometheusName: "ultrahuman_tosses_and_turns"},
	"movements":         {MetricType: "simple", DisplayName: "MOVEMENTS (Sleep)", Unit: "", PrometheusName: "ultrahuman_sleep_movements"},

	// Glucose - TimeSeriesMetric
	"glucose": {MetricType: "timeseries", Field: "last", DisplayName: "GLUCOSE", Unit: "mg/dL", PrometheusName: "ultrahuman_glucose_mg_dl"},

	// Glucose - SimpleMetric
	"average_glucose":     {MetricType: "simple", DisplayName: "AVERAGE GLUCOSE", Unit: "mg/dL", PrometheusName: "ultrahuman_avg_glucose_mg_dl"},
	"glucose_variability": {MetricType: "simple", DisplayName: "GLUCOSE VARIABILITY", Unit: "%", PrometheusName: "ultrahuman_glucose_variability_percent"},
	"time_in_target":      {MetricType: "simple", DisplayName: "TIME IN TARGET", Unit: "%", PrometheusName: "ultrahuman_time_in_target_percent"},
	"hba1c":               {MetricType: "simple", DisplayName: "HbA1c (Estimated)", Unit: "%", PrometheusName: "ultrahuman_hba1c_percent"},
	"metabolic_score":     {MetricType: "simple", DisplayName: "METABOLIC SCORE", Unit: "", PrometheusName: "ultrahuman_metabolic_score"},
}

// getLatestTimestamp finds the most recent timestamp from a slice of TimeValues
func getLatestTimestamp(values []TimeValue) int64 {
	var latest int64
	for _, v := range values {
		if v.Timestamp > latest {
			latest = v.Timestamp
		}
	}
	return latest
}

// updateGlobalTimestamp updates the global timestamp if the new one is more recent
func updateGlobalTimestamp(ts int64) {
	if ts > globalLatestTimestamp {
		globalLatestTimestamp = ts
	}
}

// pushMetrics pushes time series metrics via remote write with their original timestamps
func pushMetrics(metrics []Metric, rwClient *RemoteWriteClient) error {
	if rwClient == nil {
		return nil
	}

	var timeseries []prompb.TimeSeries

	lastPushedMu.Lock()
	defer lastPushedMu.Unlock()

	for _, m := range metrics {
		config, ok := metricRegistry[m.Type]
		if !ok || config.PrometheusName == "" || config.MetricType != "timeseries" {
			continue
		}

		var v TimeSeriesMetric
		if err := json.Unmarshal(m.Object, &v); err != nil {
			continue
		}

		lastTs := lastPushedTimestamp[m.Type]

		// Steps: push daily total instead of cumulative readings
		if m.Type == "steps" {
			if v.DayStartTimestamp > lastTs {
				timestampMs := v.DayStartTimestamp * 1000
				timeseries = append(timeseries, buildTimeSeries(config.PrometheusName, v.Total, timestampMs))
				lastPushedTimestamp[m.Type] = v.DayStartTimestamp
				updateGlobalTimestamp(v.DayStartTimestamp)
			}
			continue
		}

		// Push each individual reading with its timestamp
		for _, reading := range v.Values {
			if reading.Timestamp <= lastTs {
				continue
			}
			timestampMs := reading.Timestamp * 1000
			timeseries = append(timeseries, buildTimeSeries(config.PrometheusName, reading.Value, timestampMs))
			if reading.Timestamp > lastPushedTimestamp[m.Type] {
				lastPushedTimestamp[m.Type] = reading.Timestamp
			}
			updateGlobalTimestamp(reading.Timestamp)
		}
	}

	if len(timeseries) == 0 {
		return nil
	}

	log.Printf("Pushing %d data points via remote write", len(timeseries))
	return rwClient.Write(timeseries)
}

func makeRequest(baseURL string, params map[string]string, token string) (*APIResponse, error) {
	u, _ := url.Parse(baseURL)
	q := u.Query()
	for key, value := range params {
		q.Set(key, value)
	}
	u.RawQuery = q.Encode()

	req, _ := http.NewRequest("GET", u.String(), nil)
	req.Header.Add("Authorization", token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, err
	}

	return &apiResp, nil
}

func formatTimestamp(ts int64) string {
	return time.Unix(ts, 0).Format("15:04")
}

func formatDuration(minutes float64) string {
	h := int(minutes) / 60
	m := int(minutes) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func printSection(title string) {
	fmt.Printf("\n  %s\n", title)
}

func getMetricValue(metrics []Metric, metricType string) string {
	for _, m := range metrics {
		if m.Type != metricType {
			continue
		}

		// Handle special cases
		if metricType == "sleep" {
			var v SleepMetric
			if err := json.Unmarshal(m.Object, &v); err != nil {
				return "null"
			}
			if v.Score != nil {
				return fmt.Sprintf("%.0f", *v.Score)
			}
			return "null"
		}

		if metricType == "motion" {
			var v TimeSeriesMetric
			if err := json.Unmarshal(m.Object, &v); err != nil {
				return "null"
			}
			return fmt.Sprintf("%d", len(v.Values))
		}

		// Look up in registry
		config, ok := metricRegistry[metricType]
		if !ok {
			return "not found"
		}

		switch config.MetricType {
		case "timeseries":
			var v TimeSeriesMetric
			if err := json.Unmarshal(m.Object, &v); err != nil {
				return "null"
			}
			var value float64
			switch config.Field {
			case "last":
				value = v.LastReading
			case "avg":
				value = v.Avg
			case "total":
				value = v.Total
			}
			if config.Unit == "°C" {
				return fmt.Sprintf("%.1f", value)
			}
			return fmt.Sprintf("%.0f", value)

		case "simple":
			var v SimpleMetric
			if err := json.Unmarshal(m.Object, &v); err != nil {
				return "null"
			}
			if v.Value == nil {
				return "null"
			}
			if config.IsDuration {
				return formatDuration(*v.Value)
			}
			// Use decimal for temperature, percentages with decimals, and vo2_max
			if config.Unit == "°C" || metricType == "hba1c" || metricType == "glucose_variability" || metricType == "vo2_max" {
				return fmt.Sprintf("%.1f", *v.Value)
			}
			return fmt.Sprintf("%.0f", *v.Value)
		}
	}
	return "not found"
}

func displayMetrics(resp *APIResponse) {
	fmt.Println("══════════════════════════════════════════════════════════")
	fmt.Printf("  ULTRAHUMAN METRICS | Timezone: %s\n", resp.Data.LatestTimeZone)
	fmt.Println("══════════════════════════════════════════════════════════")

	for date, metrics := range resp.Data.Metrics {
		fmt.Printf("\n  Date: %s\n", date)
		fmt.Println("──────────────────────────────────────────────────────────")

		for _, m := range metrics {
			displayMetric(m)
		}
	}
	fmt.Println("\n══════════════════════════════════════════════════════════")
}

func displayMetric(m Metric) {
	// Handle special "sleep" composite type
	if m.Type == "sleep" {
		var v SleepMetric
		if err := json.Unmarshal(m.Object, &v); err != nil {
			return
		}
		if v.Score == nil && v.TotalSleep == nil {
			return
		}
		printSection("SLEEP")
		if v.Score != nil {
			fmt.Printf("      Score: %.0f\n", *v.Score)
		}
		if v.TotalSleep != nil {
			fmt.Printf("      Total: %s\n", formatDuration(*v.TotalSleep))
		}
		if v.Efficiency != nil {
			fmt.Printf("      Efficiency: %.0f%%\n", *v.Efficiency)
		}
		return
	}

	// Handle "motion" special case
	if m.Type == "motion" {
		var v TimeSeriesMetric
		if err := json.Unmarshal(m.Object, &v); err != nil || len(v.Values) == 0 {
			return
		}
		printSection("MOTION")
		fmt.Printf("      Readings: %d\n", len(v.Values))
		return
	}

	// Handle "steps" special case (shows total and avg)
	if m.Type == "steps" {
		var v TimeSeriesMetric
		if err := json.Unmarshal(m.Object, &v); err != nil {
			return
		}
		if v.Total > 0 || v.Avg > 0 {
			printSection("STEPS")
			fmt.Printf("      Total: %.0f | Avg: %.0f\n", v.Total, v.Avg)
		}
		return
	}

	// Look up in registry
	config, ok := metricRegistry[m.Type]
	if !ok {
		return
	}

	switch config.MetricType {
	case "timeseries":
		var v TimeSeriesMetric
		if err := json.Unmarshal(m.Object, &v); err != nil {
			return
		}
		if v.Title == "" {
			return
		}
		printSection(config.DisplayName)
		unit := config.Unit
		if unit == "" {
			unit = v.Unit
		}
		// Print summary value
		switch config.Field {
		case "last":
			if config.Unit == "°C" {
				fmt.Printf("      Last: %.1f%s\n", v.LastReading, unit)
			} else {
				fmt.Printf("      Last: %.0f %s\n", v.LastReading, unit)
			}
		case "avg":
			fmt.Printf("      Average: %.0f%s\n", v.Avg, unit)
		case "total":
			fmt.Printf("      Total: %.0f\n", v.Total)
		}
		// Print individual time series values
		for _, r := range v.Values {
			if config.Unit == "°C" {
				fmt.Printf("      - %.1f%s @ %s\n", r.Value, unit, formatTimestamp(r.Timestamp))
			} else if config.Unit == "%" || m.Type == "spo2" {
				fmt.Printf("      - %.0f%% @ %s\n", r.Value, formatTimestamp(r.Timestamp))
			} else {
				fmt.Printf("      - %.0f %s @ %s\n", r.Value, unit, formatTimestamp(r.Timestamp))
			}
		}

	case "simple":
		var v SimpleMetric
		if err := json.Unmarshal(m.Object, &v); err != nil || v.Value == nil {
			return
		}
		printSection(config.DisplayName)
		if config.IsDuration {
			fmt.Printf("      Duration: %s\n", formatDuration(*v.Value))
		} else if config.Unit == "°C" {
			fmt.Printf("      Value: %.1f%s\n", *v.Value, config.Unit)
		} else if config.Unit == "%" {
			fmt.Printf("      Value: %.0f%%\n", *v.Value)
		} else if config.Unit == "ml/kg/min" || m.Type == "hba1c" || m.Type == "glucose_variability" {
			fmt.Printf("      Value: %.1f %s\n", *v.Value, config.Unit)
		} else if config.Unit != "" {
			fmt.Printf("      Value: %.0f %s\n", *v.Value, config.Unit)
		} else {
			fmt.Printf("      Score: %.0f\n", *v.Value)
		}
	}
}

func printUsage() {
	fmt.Println(`Usage: uh-ring [options] [command]

Options:
  --api-token <token>       API token (or set ULTRAHUMAN_API_TOKEN env var)
  --port <port>             Port for Prometheus server (default: 8080)
  --interval <seconds>      Metric refresh interval in seconds (default: 60)
  --remote-write-url <url>  Prometheus remote write URL for historical data
                            (e.g., http://localhost:9090/api/v1/write)

Commands:
  (no command)          Show all metrics
  serve                 Start Prometheus metrics server

  Heart & Activity:
    hr                  Heart rate (BPM)
    hrv                 Heart rate variability (ms)
    spo2                Blood oxygen (%)
    steps               Step count
    motion              Motion readings count
    movement_index      Movement index score
    active_minutes      Active minutes
    recovery_index      Recovery index score
    recovery            Recovery score
    vo2_max             VO2 max (ml/kg/min)

  Sleep:
    sleep               Sleep score
    sleep_score         Sleep score
    total_sleep         Total sleep duration
    sleep_efficiency    Sleep efficiency (%)
    deep_sleep          Deep sleep duration
    light_sleep         Light sleep duration
    rem_sleep           REM sleep duration
    time_in_bed         Time in bed
    sleep_rhr           Sleep resting HR (BPM)
    night_rhr           Night resting HR (BPM)
    avg_sleep_hrv       Sleep HRV average (ms)
    hr_drop             HR drop during sleep
    restorative_sleep   Restorative sleep score
    morning_alertness   Morning alertness score
    full_sleep_cycles   Sleep cycles count
    tosses_and_turns    Tosses and turns count
    movements           Sleep movements count

  Temperature:
    temp                Skin temperature (°C)
    temperature_deviation  Temp deviation (°C)
    average_body_temperature  Avg body temp (°C)

  Glucose:
    glucose             Glucose (mg/dL)
    average_glucose     Average glucose (mg/dL)
    glucose_variability Glucose variability (%)
    time_in_target      Time in target (%)
    hba1c               Estimated HbA1c (%)
    metabolic_score     Metabolic score`)
}

func fetchAndPushMetrics(baseURL, token string, rwClient *RemoteWriteClient) error {
	dateParams := map[string]string{
		"date": time.Now().Format("2006-01-02"),
	}

	resp, err := makeRequest(baseURL, dateParams, token)
	if err != nil {
		return err
	}

	if resp.Error != nil {
		return fmt.Errorf("API error: %s", *resp.Error)
	}

	for _, metrics := range resp.Data.Metrics {
		if err := pushMetrics(metrics, rwClient); err != nil {
			return fmt.Errorf("push metrics: %w", err)
		}
		break
	}

	return nil
}

func startMetricsPusher(token string, port int, interval int, remoteWriteURL string) {
	baseURL := "https://partner.ultrahuman.com/api/v1/partner/daily_metrics"

	if remoteWriteURL == "" {
		log.Fatal("--remote-write-url is required for serve mode")
	}

	rwClient := NewRemoteWriteClient(remoteWriteURL)
	log.Printf("Remote write target: %s", remoteWriteURL)

	// Initial fetch
	if err := fetchAndPushMetrics(baseURL, token, rwClient); err != nil {
		log.Printf("Initial fetch error: %v", err)
	}

	// Start background pusher
	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		for range ticker.C {
			if err := fetchAndPushMetrics(baseURL, token, rwClient); err != nil {
				log.Printf("Fetch error: %v", err)
			}
		}
	}()

	// Simple health endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok\n")
	})
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"running","last_data_timestamp":%d,"interval_seconds":%d}`, globalLatestTimestamp, interval)
	})

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Starting metrics pusher on %s", addr)
	log.Printf("Pushing metrics every %d seconds", interval)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func main() {
	apiToken := flag.String("api-token", "", "API token for Ultrahuman")
	port := flag.Int("port", 8080, "Port for Prometheus server")
	interval := flag.Int("interval", 60, "Metric refresh interval in seconds")
	remoteWriteURL := flag.String("remote-write-url", "", "Prometheus remote write URL (e.g., http://localhost:9090/api/v1/write)")
	flag.Usage = printUsage
	flag.Parse()

	args := flag.Args()

	// Allow help without token
	if len(args) > 0 && args[0] == "help" {
		printUsage()
		return
	}

	// Get token from flag or environment variable
	token := *apiToken
	if token == "" {
		token = os.Getenv("ULTRAHUMAN_API_TOKEN")
	}
	if token == "" {
		fmt.Println("Error: API token required. Use --api-token or set ULTRAHUMAN_API_TOKEN env var")
		os.Exit(1)
	}

	// Handle serve command
	if len(args) > 0 && args[0] == "serve" {
		startMetricsPusher(token, *port, *interval, *remoteWriteURL)
		return
	}

	baseURL := "https://partner.ultrahuman.com/api/v1/partner/daily_metrics"

	dateParams := map[string]string{
		"date": time.Now().Format("2006-01-02"),
	}

	resp, err := makeRequest(baseURL, dateParams, token)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	if resp.Error != nil {
		fmt.Printf("API Error: %s\n", *resp.Error)
		os.Exit(1)
	}

	// Get metrics for today
	var metrics []Metric
	for _, m := range resp.Data.Metrics {
		metrics = m
		break
	}

	if len(args) < 1 {
		displayMetrics(resp)
		return
	}

	value := getMetricValue(metrics, args[0])
	fmt.Println(value)
}
