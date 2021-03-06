// Copyright 2019 The ABCDevOps Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/costexplorer"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "aws_billing" // For Prometheus metrics.
)

var (
	serverLabelNames = []string{"type", "unit"}
)

func newAwsBillingMetric(metricName string, docString string, constLabels prometheus.Labels) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(namespace, "server", metricName), docString, serverLabelNames, constLabels)
}

type metrics map[int]*prometheus.Desc
type awsMetrics map[int]string

func (m metrics) String() string {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	s := make([]string, len(keys))
	for i, k := range keys {
		s[i] = strconv.Itoa(k)
	}
	return strings.Join(s, ",")
}

/**
AWSMetrics are original metrics defined by AWS
**/
var (
	prometheusMetrics = metrics{
		1: newAwsBillingMetric("amortized_cost", "This cost metric reflects the effective cost of the upfront and monthly reservation fees spread across the billing period..", nil),
		2: newAwsBillingMetric("blended_cost", "This cost metric reflects the average cost of usage across the consolidated billing family.", nil),
		3: newAwsBillingMetric("net_amortized_cost", "This cost metric amortizes the upfront and monthly reservation fees while including discounts such as RI volume discounts.", nil),
		4: newAwsBillingMetric("net_unblended_cost", "This cost metric reflects the cost after discounts.", nil),
		5: newAwsBillingMetric("normalized_usage_amount", "Cost of amount of resource consumption like CPU.", nil),
		6: newAwsBillingMetric("unblended_cost", "Unblended costs separate discounts into their own line items. This enables you to view the amount of each discount received.", nil),
		7: newAwsBillingMetric("usage_quantity", "Usage of quantity like data in GB.", nil),
	}
	awsBillingUp = prometheus.NewDesc(prometheus.BuildFQName(namespace, "", "up"), "Was the last scrape of aws billing successful.", nil, nil)
	AWSMetrics   = awsMetrics{
		1: "AmortizedCost",
		2: "BlendedCost",
		3: "NetAmortizedCost",
		4: "NetUnblendedCost",
		5: "NormalizedUsageAmount",
		6: "UnblendedCost",
		7: "UsageQuantity",
	}
)

// Exporter collects AWS Billing stats and exports them using
// the prometheus metrics package.
type Exporter struct {
	mutex sync.RWMutex
	fetch func() (*costexplorer.GetCostAndUsageOutput, error)

	up                prometheus.Gauge
	totalScrapes      prometheus.Counter
	prometheusMetrics map[int]*prometheus.Desc
}

// NewExporter returns an initialized Exporter.
func NewExporter(filter string, selectedServerMetrics map[int]*prometheus.Desc) (*Exporter, error) {

	var fetch func() (*costexplorer.GetCostAndUsageOutput, error)
	selected := []string{}
	if len(filter) == 0 {
		for _, v := range AWSMetrics {
			selected = append(selected, v)
		}
	} else {
		for _, f := range strings.Split(filter, ",") {
			field, err := strconv.Atoi(f)
			if err != nil {
				return nil, fmt.Errorf("invalid server metric field number: %v", f)
			}
			selected = append(selected, AWSMetrics[field])
		}
	}

	fetch = fetchHTTP(selected)

	return &Exporter{
		fetch: fetch,
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the last scrape of aws cost and usage API successful.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_total_scrapes",
			Help:      "Current total aws cost and usage API scrapes.",
		}),
		prometheusMetrics: selectedServerMetrics,
	}, nil
}

// Describe describes all the metrics ever exported by the HAProxy exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {

	for _, m := range e.prometheusMetrics {
		ch <- m
	}
	ch <- awsBillingUp
	ch <- e.totalScrapes.Desc()
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) (up float64) {
	e.totalScrapes.Inc()

	response, err := e.fetch()
	if err != nil {
		log.Errorf("Can't scrape AWS Billing data: %v", err)
		return 0
	}

	for key, metric := range e.prometheusMetrics {
		for awsCostKey, cost := range response.ResultsByTime[0].Total {
			if awsCostKey == AWSMetrics[key] {
				if f, err := strconv.ParseFloat(*cost.Amount, 64); err == nil {
					ch <- prometheus.MustNewConstMetric(metric, prometheus.GaugeValue, f, awsCostKey, *cost.Unit)
				}
			}
		}
	}

	return 1
}

// Collect fetches the stats from configured AWS account and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()

	up := e.scrape(ch)

	ch <- prometheus.MustNewConstMetric(awsBillingUp, prometheus.GaugeValue, up)
	ch <- e.totalScrapes
}

func fetchHTTP(metrics []string) func() (*costexplorer.GetCostAndUsageOutput, error) {
	sess := session.Must(session.NewSession())
	client := costexplorer.New(sess)

	return func() (*costexplorer.GetCostAndUsageOutput, error) {
		input := &costexplorer.GetCostAndUsageInput{
			Metrics:     aws.StringSlice(metrics),
			Granularity: aws.String("DAILY"),
			TimePeriod: &costexplorer.DateInterval{
				Start: aws.String(time.Now().AddDate(0, 0, -1).Format("2006-01-02")),
				End:   aws.String(time.Now().Format("2006-01-02")),
			},
		}

		resp, err := client.GetCostAndUsage(input)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
}

// filterServerMetrics returns the set of server metrics specified by the comma
// separated filter.
func filterServerMetrics(filter string) (map[int]*prometheus.Desc, error) {
	metrics := map[int]*prometheus.Desc{}
	if len(filter) == 0 {
		return metrics, nil
	}

	selected := map[int]struct{}{}
	for _, f := range strings.Split(filter, ",") {
		field, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("invalid server metric field number: %v", f)
		}
		selected[field] = struct{}{}
	}

	for field, metric := range prometheusMetrics {
		if _, ok := selected[field]; ok {
			metrics[field] = metric
		}
	}
	return metrics, nil
}

func main() {

	var (
		listenAddress                = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9614").String()
		metricsPath                  = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		awsBillingServerMetricFields = kingpin.Flag("aws-billing.metrics", "Comma-separated list of billing metrics. Leave this argument if you want to scrape all available metrics. See https://docs.aws.amazon.com/aws-cost-management/latest/APIReference/API_GetCostAndUsage.html#API_GetCostAndUsage_RequestSyntax").Default(prometheusMetrics.String()).String()
	)

	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print("aws_billing_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	selectedServerMetrics, err := filterServerMetrics(*awsBillingServerMetricFields)
	if err != nil {
		log.Fatal(err)
	}

	log.Infoln("Starting aws_billing_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	exporter, err := NewExporter(*awsBillingServerMetricFields, selectedServerMetrics)
	if err != nil {
		log.Fatal(err)
	}
	prometheus.MustRegister(exporter)
	prometheus.MustRegister(version.NewCollector("aws_billing_exporter"))

	log.Infoln("Listening on", *listenAddress)
	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>AWS Billing Exporter</title></head>
             <body>
             <h1>AWS Billing Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
