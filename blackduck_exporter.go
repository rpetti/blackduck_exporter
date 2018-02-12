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
	insecure              = flag.Bool("insecure", false, "Ignore certificate errors")
	showVersion           = flag.Bool("version", false, "Print version information")
	test                  = flag.Bool("test", false, "Test connection to Blackduck server and exit")
)

// Exporter : Exported metrics data
type Exporter struct {
	URI    string
	mutex  sync.Mutex
	client *http.Client

	scrapeFailures prometheus.Counter

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
			[]string{"guid"},
		),
	}
}

// Describe : Collector implementation for Prometheus
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.scrapeFailures.Describe(ch)
	e.jobs.Describe(ch)
	e.scans.Describe(ch)
	e.jobLastSeenRunning.Describe(ch)
}

func (e *Exporter) recover(ch chan<- prometheus.Metric) {
	err := recover()
	if err != nil {
		log.Fatal(err)
		e.scrapeFailures.Inc()
		e.scrapeFailures.Collect(ch)
	}
}

// Collect : Collector implementation for Prometheus
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	defer e.recover(ch)

	auth, err := getAuthTokens()
	if err != nil {
		panic(err)
	}

	jobs, err := getJobs(auth)
	if err != nil {
		panic(err)
	}
	jobStatusCounts := make(map[string]int)
	for _, job := range jobs.Items {
		if _, ok := jobStatusCounts[job.Status]; !ok {
			jobStatusCounts[job.Status] = 0
		}
		jobStatusCounts[job.Status]++
	}
	for key, count := range jobStatusCounts {
		e.jobs.WithLabelValues(key).Set(float64(count))
	}
	e.jobs.Collect(ch)

	e.jobLastSeenRunning.Collect(ch)
	e.scans.Collect(ch)
}

type jobJSON struct {
	Items []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
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
	req.Header.Add("Cookie", fmt.Sprintf("%s=%s", auth.Cookie.Name, auth.Cookie.Value))
	req.Header.Add("X-CSRF-TOKEN", auth.XCSRFToken)

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
		return j, errors.New(fmt.Sprintf("Problem fetching jobs: %s", j.ErrorMessage))
	}
	return j, nil
}

// getPassword : Returns the password from either the plain text string or the specified password file
func getPassword() string {
	var password string
	password = ""
	if *blackduckPassword != "" {
		password = *blackduckPassword
	}
	if *blackduckPasswordFile != "" {
		_, err := os.Stat(*blackduckPasswordFile)
		if os.IsExist(err) {
			buf, err := ioutil.ReadFile(*blackduckPasswordFile)
			if err == nil {
				password = strings.TrimSpace(string(buf))
			}
		}
	}
	return password
}

type authTokens struct {
	XCSRFToken string
	Cookie     *http.Cookie
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
		return nil, errors.New(fmt.Sprintf("Could not log in with %s and %s : error code %d", *blackduckUsername, getPassword(), resp.StatusCode))
	}
	for _, cookie := range resp.Cookies() {
		if strings.Contains(cookie.String(), "JSESSIONID=") {
			a.XCSRFToken = resp.Header.Get("X-CSRF-TOKEN")
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
	//prometheus.MustRegister(version.NewCollector("blackduck_exporter"))

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