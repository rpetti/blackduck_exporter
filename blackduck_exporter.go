package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"log"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
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
	blackduckAPIToken     = flag.String("blackduck.api.token", "", "API token to use instead of username/password")
	insecure              = flag.Bool("insecure", false, "Don't validate ssl")
	sslServerName         = flag.String("ssl.server.name", "", "Server Name of the Black Duck SSL cert")
	showVersion           = flag.Bool("version", false, "Print version information")
	debug                 = flag.Bool("debug", false, "Print debugging information")

	hc *http.Client
)

// Exporter : Exported metrics data
type Exporter struct {
	URI    string
	mutex  sync.Mutex
	client *http.Client

	scrapeFailures prometheus.Counter

	jobsFailed                   prometheus.Gauge
	longestJobRunning            prometheus.Gauge
	averageDurationOfRunningJobs prometheus.Gauge
	jobs                         *prometheus.GaugeVec
	scans                        *prometheus.GaugeVec
	jobLastSeenRunning           *prometheus.GaugeVec
	jobTypesRunning              *prometheus.GaugeVec
	scrapeTime                   prometheus.Gauge
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
		jobTypesRunning: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "job_types_running",
			Help:      "Number of jobs currently running by type.",
		},
			[]string{"type"},
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
		longestJobRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "longest_running_job_duration",
			Help:      "Longest duration of currently running jobs",
		}),
		averageDurationOfRunningJobs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "average_duration_of_running_jobs",
			Help:      "Average duration of all currently running jobs",
		}),
		scrapeTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "scrape_duration_seconds",
			Help:      "Time taken in seconds to scrape metrics from Black Duck.",
		}),
	}
}

// Describe : Collector implementation for Prometheus
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.scrapeFailures.Describe(ch)
	e.jobsFailed.Describe(ch)
	e.jobs.Describe(ch)
	e.scans.Describe(ch)
	e.jobLastSeenRunning.Describe(ch)
	e.jobTypesRunning.Describe(ch)
	e.averageDurationOfRunningJobs.Describe(ch)
	e.longestJobRunning.Describe(ch)
}

// Collect : Collector implementation for Prometheus
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	err := e.collect(ch)
	if err != nil {
		log.Printf("error collecting stats: %v", err)
		e.scrapeFailures.Inc()
	}
	e.scrapeFailures.Collect(ch)
}

func (e *Exporter) collect(ch chan<- prometheus.Metric) error {
	start := time.Now()
	e.mutex.Lock()
	defer e.mutex.Unlock()

	auth, err := getAuthTokens()
	if err != nil {
		return err
	}

	jobStats, err := getJobStats(auth)
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
		"ERROR",
	} {
		e.jobs.WithLabelValues(key).Set(0.0)
	}
	for key, count := range jobStatusCounts {
		e.jobs.WithLabelValues(key).Set(float64(count))
	}
	e.jobs.Collect(ch)

	jobTypeCounts := make(map[string]int)

	var supportedTypes []string
	for _, item := range jobStats.Items {
		supportedTypes = append(supportedTypes, item.JobType)
	}

	sort.Strings(supportedTypes)
	for _, job := range jobs.Items {
		i := sort.SearchStrings(supportedTypes, job.JobSpec.Type)
		if job.Status == "RUNNING" && i < len(supportedTypes) && supportedTypes[i] == job.JobSpec.Type {
			if _, ok := jobTypeCounts[job.JobSpec.Type]; !ok {
				jobTypeCounts[job.JobSpec.Type] = 0
			}
			jobTypeCounts[job.JobSpec.Type]++
		}
	}
	for _, key := range supportedTypes {
		e.jobTypesRunning.WithLabelValues(key).Set(0.0)
	}
	for key, count := range jobTypeCounts {
		e.jobTypesRunning.WithLabelValues(key).Set(float64(count))
	}
	e.jobTypesRunning.Collect(ch)

	for _, job := range jobs.Items {
		if job.Status == "RUNNING" {
			e.jobLastSeenRunning.WithLabelValues(job.ID, job.JobSpec.Type).SetToCurrentTime()
		}
	}
	e.jobLastSeenRunning.Collect(ch)

	var highestDuration float64
	var totalDuration float64
	highestDuration = 0.0
	totalDuration = 0.0
	totalRunning := 0
	for _, job := range jobs.Items {
		if job.Status == "RUNNING" {
			duration := time.Since(job.StartedAt.Time).Seconds()
			if duration > highestDuration {
				highestDuration = duration
			}
			totalDuration += duration
			totalRunning += 1
		}
	}
	var averageDuration float64
	if totalRunning > 0 {
		averageDuration = totalDuration / float64(totalRunning)
	}
	e.longestJobRunning.Set(highestDuration)
	e.longestJobRunning.Collect(ch)
	e.averageDurationOfRunningJobs.Set(averageDuration)
	e.averageDurationOfRunningJobs.Collect(ch)

	numFailedJobs := jobStats.TotalFailures()

	e.jobsFailed.Set(float64(numFailedJobs))
	e.jobsFailed.Collect(ch)

	scans, err := getScans(auth)
	if err != nil {
		return err
	}
	for _, key := range []string{
		"IN_PROGRESS",
		"UNSTARTED",
		"COMPLETED",
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

	elapsed := time.Now().Sub(start)
	e.scrapeTime.Set(elapsed.Seconds())
	e.scrapeTime.Collect(ch)
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

	if *debug {
		log.Print("fetching scans")
	}

	form := url.Values{}
	form.Add("limit", "1000")
	form.Add("offset", "0")
	form.Add("sort", "updatedAt DESC")
	form.Add("filter", "codeLocationStatus:in_progress")
	form.Add("filter", "codeLocationStatus:in_progress")
	req, err := http.NewRequest(
		"GET",
		fmt.Sprintf("%s/api/codelocations?%s", *blackduckURL, form.Encode()),
		nil)
	if err != nil {
		return j, err
	}
	auth.Auth(req)

	resp, err := hc.Do(req)
	if err != nil {
		return j, err
	}

	if *debug {
		log.Printf("scan response: %s", resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("could not read scan response body: %v", err)
		return j, err
	}

	if *debug {
		log.Printf("scan response body: %s", body)
	}

	err = json.Unmarshal(body, &j)
	if err != nil {
		log.Printf("could not get scans: %v", err)
		return j, err
	}
	if j.ErrorMessage != "" {
		log.Printf("server error when fetching scans: %s", j.ErrorMessage)
		return j, fmt.Errorf("Problem fetching scans: %s", j.ErrorMessage)
	}
	return j, nil
}

type BDTime struct {
	Time time.Time
}

const bdTimeLayout = "2006-01-02T15:04:05.999Z"

var _ json.Unmarshaler = &BDTime{}

func (bdt *BDTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), "\"")
	if s == "null" {
		bdt.Time = time.Time{}
		return nil
	}
	t, err := time.Parse(bdTimeLayout, s)
	bdt.Time = t
	return err
}

type jobStatsJSON struct {
	Items []struct {
		JobType         string `json:"jobType"`
		TotalFailures   int    `json:"totalFailures"`
		TotalInProgress int    `json:"totalInProgress"`
		TotalRuns       int    `json:"totalRuns"`
		TotalSuccesses  int    `json:"totalSuccesses`
	} `json:"items"`
	ErrorMessage string `json:"errorMessage"`
}

func (stats jobStatsJSON) TotalFailures() int {
	failures := 0
	for _, item := range stats.Items {
		failures += item.TotalFailures
	}
	return failures
}

func getJobStats(auth *authTokens) (jobStatsJSON, error) {
	var j jobStatsJSON

	if *debug {
		log.Print("fetching job stats")
	}

	form := url.Values{}
	form.Add("limit", "1000")
	form.Add("sortField", "jobType")
	req, err := http.NewRequest(
		"GET",
		fmt.Sprintf("%s/api/job-statistics?%s", *blackduckURL, form.Encode()),
		nil)
	if err != nil {
		return j, err
	}
	auth.Auth(req)

	resp, err := hc.Do(req)
	if err != nil {
		return j, err
	}

	if *debug {
		log.Printf("job stats response: %s", resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("could not read job stats response body: %v", err)
		return j, err
	}

	if *debug {
		log.Printf("job stats response body: %s", body)
	}

	err = json.Unmarshal(body, &j)
	if err != nil {
		log.Printf("could not get job stats: %v", err)
		return j, err
	}
	if j.ErrorMessage != "" {
		log.Printf("server error when fetching job stats: %s", j.ErrorMessage)
		return j, fmt.Errorf("Problem fetching job stats: %s", j.ErrorMessage)
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
		StartedAt BDTime `json:"startedAt"`
	} `json:"items"`
	TotalCount   int    `json:"totalCount"`
	ErrorMessage string `json:"errorMessage"`
}

func getJobs(auth *authTokens) (jobJSON, error) {
	var j jobJSON

	if *debug {
		log.Print("fetching jobs")
	}

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
	auth.Auth(req)

	resp, err := hc.Do(req)
	if err != nil {
		return j, err
	}

	if *debug {
		log.Printf("job response: %s", resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("could not read job response body: %v", err)
		return j, err
	}

	if *debug {
		log.Printf("job response body: %s", body)
	}

	err = json.Unmarshal(body, &j)
	if err != nil {
		log.Printf("could not get jobs: %v", err)
		return j, err
	}
	if j.ErrorMessage != "" {
		log.Printf("server error when fetching jobs: %s", j.ErrorMessage)
		return j, fmt.Errorf("Problem fetching jobs: %s", j.ErrorMessage)
	}
	return j, nil
}

func getNumJobsFailed(auth *authTokens) (int, error) {
	var j jobJSON

	if *debug {
		log.Print("fetching job failed count")
	}

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
	auth.Auth(req)

	resp, err := hc.Do(req)
	if err != nil {
		return -1, err
	}

	if *debug {
		log.Printf("job failed count response: %s", resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("could not read job failed count response body: %v", err)
		return -1, err
	}

	if *debug {
		log.Printf("job failed count response body: %s", body)
	}

	err = json.Unmarshal(body, &j)
	if err != nil {
		log.Printf("could not get job error count: %v", err)
		return -1, err
	}
	if j.ErrorMessage != "" {
		log.Printf("server error when fetching job error count: %s", j.ErrorMessage)
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
	Cookie      *http.Cookie
	BearerToken string `json:"bearerToken"`
}

func getAuthTokens() (*authTokens, error) {
	if *blackduckUsername != "" && getPassword() != "" {
		return getAuthTokensBasic()
	}
	if *blackduckAPIToken != "" {
		return getAuthTokensAPIKey()
	}
	return nil, fmt.Errorf("No authentication information available!")
}

// getCookie : Uses credentials to get cookie from BlackDuck
func getAuthTokensBasic() (*authTokens, error) {
	var a authTokens

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
		return nil, fmt.Errorf("Could not log in with user '%s' and provided password : error code %d", *blackduckUsername, resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if strings.Contains(cookie.String(), "JSESSIONID=") || strings.Contains(cookie.String(), "AUTHORIZATION_BEARER=") {
			a.Cookie = cookie
			return &a, nil
		}
	}
	err = errors.New("Could not get cookie from blackduck using credentials provided")
	return nil, err
}

func getAuthTokensAPIKey() (*authTokens, error) {
	var a authTokens

	if *debug {
		log.Println("authenticating with api key")
	}

	req, err := http.NewRequest(
		"POST",
		fmt.Sprintf("%s/api/tokens/authenticate", *blackduckURL),
		nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", fmt.Sprintf("token %s", *blackduckAPIToken))
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("could not authenticate: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	if *debug {
		log.Printf("auth request status: %s", resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("could not read auth tokens: %v", err)
		return nil, err
	}

	if *debug {
		log.Printf("auth response body: %s", body)
	}

	err = json.Unmarshal(body, &a)
	if err != nil {
		log.Printf("could not decode auth response json: %v", err)
		return nil, err
	}
	return &a, nil
}

func (a *authTokens) Auth(req *http.Request) {
	if a.Cookie != nil {
		req.AddCookie(a.Cookie)
	}
	if a.BearerToken != "" {
		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", a.BearerToken))
	}
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Fprintln(os.Stdout, version.Print("blackduck_exporter"))
		os.Exit(0)
	}

	tlsConfig := &tls.Config{InsecureSkipVerify: *insecure}
	if *sslServerName != "" {
		tlsConfig.ServerName = *sslServerName
	}
	hc = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}
	http.DefaultTransport.(*http.Transport).TLSClientConfig = tlsConfig

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
	glog.Fatal(http.ListenAndServe(*listeningAddress, nil))
}
