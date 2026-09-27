// Command event-projector builds the payment query models from payment events.
//
//	event-projector            consume Kafka, fill ordering gaps, serve health
//	event-projector rebuild    rebuild into a shadow generation and switch reads
//
// It writes only the projections database. Journal reads go through the
// payment API with the event-projector service identity.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"gtb.local/backend/internal/adapters/paymentjournal"
	"gtb.local/backend/internal/eventing"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	platformtelemetry "gtb.local/backend/internal/platform/telemetry"
	"gtb.local/backend/internal/projections"
)

func main() {
	command, arguments := "serve", os.Args[1:]
	logOutput := os.Stdout
	if len(arguments) > 0 && arguments[0] == "rebuild" {
		// The rebuild report goes to stdout; logs go to stderr.
		command, arguments, logOutput = "rebuild", arguments[1:], os.Stderr
	}
	logger := slog.New(platformruntime.NewRedactingHandler(slog.NewJSONHandler(logOutput, nil)))
	flags := flag.NewFlagSet("event-projector "+command, flag.ExitOnError)
	abandon := flags.Bool("abandon-building", false, "rebuild: first abandon a BUILDING generation left by a crashed rebuild")
	_ = flags.Parse(arguments)

	config, err := platformruntime.LoadConfig("event-projector", "127.0.0.1:8085")
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	serviceClient, err := platformruntime.LoadServiceClient(config, "event-projector")
	if err != nil {
		logger.Error("invalid service client configuration", "error", err)
		os.Exit(1)
	}
	paymentAPIURL := os.Getenv("GTB_PAYMENT_API_URL")
	if paymentAPIURL == "" {
		paymentAPIURL = "http://127.0.0.1:8082"
	}
	brokers := eventing.Brokers()
	logger.Info("configuration loaded", append(config.LogValues(), "command", command, "service_client_id", serviceClient.ClientID,
		"payment_api_url", paymentAPIURL, "kafka_brokers", brokers, "topic", eventing.PaymentEventsTopic)...)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	telemetryProviders, err := platformtelemetry.Setup(config.ServiceName)
	if err != nil {
		logger.Error("telemetry setup failed", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
		defer cancel()
		if err := telemetryProviders.Shutdown(shutdownContext); err != nil {
			logger.Error("telemetry shutdown failed", "error", err)
		}
	}()

	pool, err := projections.Open(ctx, config.DatabaseURL)
	if err != nil {
		logger.Error("database setup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	store := projections.NewStore(pool)

	outbound := &http.Client{Timeout: 10 * time.Second, Transport: platformtelemetry.HTTPTransport(http.DefaultTransport)}
	credentials := clientcredentials.Config{
		ClientID: serviceClient.ClientID, ClientSecret: serviceClient.ClientSecret, TokenURL: serviceClient.TokenURL,
	}
	journalHTTP := credentials.Client(context.WithValue(ctx, oauth2.HTTPClient, outbound))
	journalHTTP.Timeout = 10 * time.Second
	journal, err := paymentjournal.NewClient(paymentAPIURL, journalHTTP)
	if err != nil {
		logger.Error("payment journal client configuration failed", "error", err)
		os.Exit(1)
	}
	filler := projections.NewGapFiller(store, journal, logger, projections.PaymentConsumer)

	if command == "rebuild" {
		os.Exit(rebuild(ctx, logger, store, projections.NewRebuilder(store, journal, filler, logger, projections.PaymentConsumer), *abandon))
	}

	client, err := projections.NewKafkaConsumer(brokers, "gtb-event-projector", eventing.PaymentEventsTopic, store, projections.PaymentConsumer)
	if err != nil {
		logger.Error("Kafka consumer configuration failed", "error", err)
		os.Exit(1)
	}
	defer client.Close()
	live := &projections.LiveConsumer{
		Name: projections.PaymentConsumer, Topic: eventing.PaymentEventsTopic, Store: store, Client: client, Logger: logger,
	}
	done := make(chan struct{}, 2)
	go func() { live.Run(ctx); done <- struct{}{} }()
	go func() { filler.Run(ctx); done <- struct{}{} }()
	defer func() { stop(); <-done; <-done }()

	// Kafka and the payment API are not readiness checks: while either is down
	// the projection goes stale, and the query API reports that staleness.
	health := platformruntime.NewHealth(map[string]platformruntime.ReadinessCheck{"database": store.Ready})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", health.Liveness)
	mux.HandleFunc("GET /health/ready", health.Readiness)
	server := &http.Server{
		Addr: config.HTTPAddress, Handler: platformruntime.WithCorrelation(mux), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8 * 1024,
	}
	if err := platformruntime.Run(ctx, logger, server, health, config.ShutdownTimeout); err != nil {
		logger.Error("health server stopped with error", "error", err)
		os.Exit(1)
	}
}

func rebuild(ctx context.Context, logger *slog.Logger, store *projections.Store, rebuilder *projections.Rebuilder, abandon bool) int {
	if abandon {
		abandoned, err := store.AbandonBuilding(ctx)
		if err != nil {
			logger.Error("abandoning the building generation failed", "error", err)
			return 1
		}
		logger.Info("building generation abandoned", "generation", abandoned)
	}
	report, err := rebuilder.Run(ctx)
	encoded, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(encoded))
	if err != nil {
		logger.Error("projection rebuild failed", "error", err)
		return 1
	}
	return 0
}
