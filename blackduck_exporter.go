package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
)

const (
	namespace = "blackduck"
)

var (
	listeningAddress  = flag.String("telemetry.address", ":9125", "Address on which to expose metrics")
	metricsEndpoint   = flag.String("telemetry.endpoint", "/metrics", "Path under which to expose metrics")
	blackduckURL      = flag.String("blackduck.url", "https://blackduck/", "URL of blackduck server to scrape")
	blackduckAPIToken = flag.String("blackduck.api.token", "", "BlackDuck API Token")
	insecure          = flag.Bool("insecure", false, "Ignore certificate errors")
	showVersion       = flag.Bool("version", false, "Print version information")
	test              = flag.Bool("test", false, "Test connection to Blackduck server and exit")
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

// Collect : Collector implementation for Prometheus
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
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
