package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
)

const (
	namespace = "blackduck"
)

var (
	listeningAddress      = flag.String("telemetry.address", ":9125", "Address on which to expose metrics")
	metricsEndpoint       = flag.String("telemetry.endpoint", "/metrics", "Path under which to expose metrics")
	blackduckURL          = flag.String("blackduck.url", "https://blackduck/", "URL of blackduck server to scrape")
	blackduckUsername     = flag.String("blackduck.username", "", "BlackDuck username to use for API authentication")
	blackduckPasswordFile = flag.String("blackduck.password.file", "", "File (secret) containing BlackDuck password")
	blackduckPassword     = flag.String("blackduck.password", "", "BlackDuck password in plain text (blackduck.password.file is recommended instead)")
	showVersion           = flag.Bool("version", false, "Print version information")
)

// Exporter : Exported metrics data
type Exporter struct {
	URI    string
	mutex  sync.Mutex
	client *http.Client

	scrapeFailures prometheus.Counter

	jobsFailed         prometheus.Gauge
	jobs               *prometheus.GaugeVec
	scans              *prometheus.GaugeVec
	jobLastSeenRunning *prometheus.GaugeVec
}

// NewExporter : Creates a new collector/exporter using a blackduck url
func NewExporter(uri string) *Exporter {
	return &Exporter{
		URI: uri,
		scrapeFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_scrape_failures_total",
			Help:      "Number of errors while scraping blackduck.",
		}),
		jobsFailed: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "jobs_failed_total",
			Help:      "Number of jobs that have ever failed.",
		}),
		jobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "jobs",
			Help:      "Number of jobs currently in queue.",
		},
			[]string{"state"},
		),
		scans: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "scans",
			Help:      "Scans currently pending completion.",
		},
			[]string{"state"},
		),
		jobLastSeenRunning: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "job_last_seen_running",
			Help:      "Time of the jobs last seen running.",
		},
			[]string{"guid", "type"},
		),
	}
}

// Describe : Collector implementation for Prometheus
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.scrapeFailures.Describe(ch)
	e.jobsFailed.Describe(ch)
	e.jobs.Describe(ch)
	e.scans.Describe(ch)
	e.jobLastSeenRunning.Describe(ch)
}

// Collect : Collector implementation for Prometheus
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	err := e.collect(ch)
	if err != nil {
		log.Fatal(err)
		e.scrapeFailures.Inc()
		e.scrapeFailures.Collect(ch)
	}
}

func (e *Exporter) collect(ch chan<- prometheus.Metric) error {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	auth, err := getAuthTokens()
	if err != nil {
		return err
	}

	jobs, err := getJobs(auth)
	if err != nil {
		return err
	}
	jobStatusCounts := make(map[string]int)
	for _, job := range jobs.Items {
		if _, ok := jobStatusCounts[job.Status]; !ok {
			jobStatusCounts[job.Status] = 0
		}
		jobStatusCounts[job.Status]++
	}
	for _, key := range []string{
		"RUNNING",
		"SCHEDULED",
		"DISPATCHED",
	} {
		e.jobs.WithLabelValues(key).Set(0.0)
	}
	for key, count := range jobStatusCounts {
		e.jobs.WithLabelValues(key).Set(float64(count))
	}
	e.jobs.Collect(ch)

	for _, job := range jobs.Items {
		if job.Status == "RUNNING" {
			e.jobLastSeenRunning.WithLabelValues(job.ID, job.JobSpec.Type).SetToCurrentTime()
		}
	}
	e.jobLastSeenRunning.Collect(ch)

	numFailedJobs, err := getNumJobsFailed(auth)
	if err != nil {
		return err
	}
	e.jobsFailed.Set(float64(numFailedJobs))
	e.jobsFailed.Collect(ch)

	scans, err := getScans(auth)
	if err != nil {
		return err
	}
	for _, key := range []string{
		"IN_PROGRESS",
		"UNSTARTED",
	} {
		e.scans.WithLabelValues(key).Set(0.0)
	}
	for _, scan := range scans.Items {
		for _, status := range scan.Status {
			if status.OperationNameCode == "ServerScanning" {
				e.scans.WithLabelValues(status.Status).Inc()
			}
		}
	}
	e.scans.Collect(ch)

	return nil
}

type scanJSON struct {
	Items []struct {
		Name   string `json:"name"`
		Status []struct {
			OperationNameCode string `json:"operationNameCode"`
			Status            string `json:"status"`
		} `json:"status"`
	} `json:"items"`
	TotalCount   int    `json:"totalCount"`
	ErrorMessage string `json:"errorMessage"`
}

func getScans(auth *authTokens) (scanJSON, error) {
	var j scanJSON
	hc := http.Client{}

	form := url.Values{}
	form.Add("limit", "1000")
	form.Add("offset", "0")
	form.Add("sort", "updatedAt DESC")
	form.Add("filter", "codeLocationStatus:in_progress")
	form.Add("filter", "codeLocationStatus:in_progress")
	req, err := http.NewRequest(
		"GET",
		fmt.Sprintf("%s/api/internal/codelocations?%s", *blackduckURL, form.Encode()),
		nil)
	if err != nil {
		return j, err
	}
	req.AddCookie(auth.Cookie)

	resp, err := hc.Do(req)
	if err != nil {
		return j, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return j, err
	}

	err = json.Unmarshal(body, &j)
	if err != nil {
		return j, err
	}
	if j.ErrorMessage != "" {
		return j, fmt.Errorf("Problem fetching jobs: %s", j.ErrorMessage)
	}
	return j, nil
}

type jobJSON struct {
	Items []struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		JobSpec struct {
			Type string `json:"jobType"`
		} `json:"jobSpec"`
	} `json:"items"`
	TotalCount   int    `json:"totalCount"`
	ErrorMessage string `json:"errorMessage"`
}

func getJobs(auth *authTokens) (jobJSON, error) {
	var j jobJSON
	hc := http.Client{}

	form := url.Values{}
	form.Add("limit", "1000")
	form.Add("offset", "0")
	form.Add("sortField", "scheduledAt")
	form.Add("ascending", "false")
	form.Add("filter", "jobStatus:scheduled")
	form.Add("filter", "jobStatus:dispatched")
	form.Add("filter", "jobStatus:running")
	req, err := http.NewRequest(
		"GET",
		fmt.Sprintf("%s/api/v1/jobs?%s", *blackduckURL, form.Encode()),
		nil)
	if err != nil {
		return j, err
	}
	req.AddCookie(auth.Cookie)

	resp, err := hc.Do(req)
	if err != nil {
		return j, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return j, err
	}

	err = json.Unmarshal(body, &j)
	if err != nil {
		return j, err
	}
	if j.ErrorMessage != "" {
		return j, fmt.Errorf("Problem fetching jobs: %s", j.ErrorMessage)
	}
	return j, nil
}

func getNumJobsFailed(auth *authTokens) (int, error) {
	var j jobJSON
	hc := http.Client{}

	form := url.Values{}
	form.Add("limit", "1")
	form.Add("offset", "0")
	form.Add("filter", "jobStatus:failed")
	req, err := http.NewRequest(
		"GET",
		fmt.Sprintf("%s/api/v1/jobs?%s", *blackduckURL, form.Encode()),
		nil)
	if err != nil {
		return -1, err
	}
	req.AddCookie(auth.Cookie)

	resp, err := hc.Do(req)
	if err != nil {
		return -1, err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return -1, err
	}

	err = json.Unmarshal(body, &j)
	if err != nil {
		return -1, err
	}
	if j.ErrorMessage != "" {
		return -1, fmt.Errorf("Problem fetching job error count: %s", j.ErrorMessage)
	}
	return j.TotalCount, nil
}

// getPassword : Returns the password from either the plain text string or the specified password file
func getPassword() string {
	var password string
	password = ""
	if *blackduckPassword != "" {
		password = *blackduckPassword
	}
	if *blackduckPasswordFile != "" {
		buf, err := ioutil.ReadFile(*blackduckPasswordFile)
		if err == nil {
			password = strings.TrimSpace(string(buf))
		}
	}
	return password
}

type authTokens struct {
	Cookie *http.Cookie
}

// getCookie : Uses credentials to get cookie from BlackDuck
func getAuthTokens() (*authTokens, error) {
	var a authTokens
	hc := http.Client{}

	form := url.Values{}
	form.Add("j_username", *blackduckUsername)
	form.Add("j_password", getPassword())

	req, err := http.NewRequest(
		"POST",
		fmt.Sprintf("%s/j_spring_security_check", *blackduckURL),
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Could not log in with %s and %s : error code %d", *blackduckUsername, getPassword(), resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if strings.Contains(cookie.String(), "JSESSIONID=") {
			a.Cookie = cookie
			return &a, nil
		}
	}
	err = errors.New("Could not get cookie from blackduck using credentials provided")
	return nil, err
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, version.Print("blackduck_exporter"))
		os.Exit(0)
	}

	prometheus.MustRegister(NewExporter(*blackduckURL))
	prometheus.MustRegister(version.NewCollector("blackduck_exporter"))

	http.Handle(*metricsEndpoint, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>BlackDuck Exporter</title></head>
			<body>
			<h1>BlackDuck Exporter</h1>
			<p><a href='` + *metricsEndpoint + `'>Metrics</a></p>
			</body>
			</html>`))
	})
	log.Fatal(http.ListenAndServe(*listeningAddress, nil))
}
