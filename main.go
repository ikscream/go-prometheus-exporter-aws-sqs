package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

// Default to checking queues every 30 seconds
const defaultMonitorInterval = 30

var svc = getSqsClient()

var monitorInterval = getMonitorInterval()

var labelNames = []string{"queue"}

var promMessages = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "sqs_approximatenumberofmessages",
	Help: "The approximate number of messages available for retrieval from the queue.",
},
	labelNames)
var promMessagesDelayed = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "sqs_approximatenumberofmessagesdelayed",
	Help: "The approximate number of messages in the queue that are delayed and not available for reading immediately.",
}, labelNames)
var promMessagesNotVisible = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "sqs_approximatenumberofmessagesnotvisible",
	Help: "The approximate number of messages that are in flight.",
}, labelNames)

// Struct to hold queue URL and name, as these aren't included in SQS response
type queueResult struct {
	QueueURL     string
	QueueName    string
	QueueResults *sqs.GetQueueAttributesOutput
}

func getSqsClient() *sqs.Client {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Error().Str("errorMessage", err.Error()).Msg("error loading AWS config")
		os.Exit(1)
	}

	return sqs.NewFromConfig(cfg)
}

func getMonitorInterval() time.Duration {
	monitorIntervalStr, varSet := os.LookupEnv("SQS_MONITOR_INTERVAL_SECONDS")
	if !varSet || monitorIntervalStr == "" {
		log.Warn().Msg(fmt.Sprintf("Monitor interval not set, defaulting to %v", defaultMonitorInterval))
		return time.Duration(defaultMonitorInterval)
	}

	monitorInterval, err := strconv.Atoi(monitorIntervalStr)
	if err != nil {
		log.Error().Str("errorMessage", err.Error()).Msg("bad value for SQS_MONITOR_INTERVAL")
		os.Exit(1)
	}
	return time.Duration(monitorInterval)
}

func monitorQueue(queueURL string, c chan queueResult) {
	queueComponents := strings.Split(queueURL, "/")
	queueName := queueComponents[len(queueComponents)-1]

	params := &sqs.GetQueueAttributesInput{
		QueueUrl: &queueURL,
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesDelayed,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	}

	resp, err := svc.GetQueueAttributes(context.TODO(), params)
	if err != nil {
		log.Error().Str("errorMessage", err.Error()).Msg("error checking queue")
		os.Exit(1)
	}

	c <- queueResult{queueURL, queueName, resp}
}

func monitorQueues(queueUrls []string) {
	c := make(chan queueResult)
	for {
		for _, queueURL := range queueUrls {
			go monitorQueue(queueURL, c)
		}

		for i := 0; i < len(queueUrls); i++ {
			queueResult := <-c
			for attrib := range queueResult.QueueResults.Attributes {
				prop := queueResult.QueueResults.Attributes[attrib]
				nMessages, _ := strconv.ParseFloat(prop, 64)
				switch attrib {
				case "ApproximateNumberOfMessages":
					promMessages.WithLabelValues(queueResult.QueueName).Set(nMessages)
				case "ApproximateNumberOfMessagesDelayed":
					promMessagesDelayed.WithLabelValues(queueResult.QueueName).Set(nMessages)
				case "ApproximateNumberOfMessagesNotVisible":
					promMessagesNotVisible.WithLabelValues(queueResult.QueueName).Set(nMessages)
				default:
					log.Error().Msg(fmt.Sprintf("unknown attribute %v", attrib))
					os.Exit(1)
				}
			}
		}

		time.Sleep(monitorInterval * time.Second)
	}
}

// Return an empty 200 response for healthchecks
func healthcheck(http.ResponseWriter, *http.Request) {
}

func main() {
	queueVar, varSet := os.LookupEnv("SQS_QUEUE_URLS")
	if !varSet || queueVar == "" {
		log.Error().Msg("No URLs supplied")
		os.Exit(1)
	}
	queueUrls := strings.Split(queueVar, ",")

	listenVar, listenSet := os.LookupEnv("LISTEN")

	if !listenSet || listenVar == "" {
		listenVar = ":8080"
	}

	log.Info().Int("interval", int(monitorInterval)).Strs("queueUrls", queueUrls).Msg("Starting queue monitors")

	go monitorQueues(queueUrls)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", healthcheck)
	err := http.ListenAndServe(listenVar, nil)
	if err != nil {
		log.Error().Str("errorMessage", err.Error()).Msg("Could not start http listener")
		os.Exit(1)
	}
}
