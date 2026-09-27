// Command payment-worker runs the payment domain's background delivery. In B03
// that is the payment outbox relay to Kafka; B06 adds workflow dispatch and
// Temporal workers. It serves only health endpoints.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/payment/storage"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	platformtelemetry "gtb.local/backend/internal/platform/telemetry"
)

func main() {
	logger := slog.New(platformruntime.NewRedactingHandler(slog.NewJSONHandler(os.Stdout, nil)))
	config, err := platformruntime.LoadConfig("payment-worker", "127.0.0.1:8086")
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	brokers := eventing.Brokers()
	logger.Info("configuration loaded", append(config.LogValues(), "kafka_brokers", brokers, "topic", eventing.PaymentEventsTopic)...)
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

	pool, err := storage.Open(ctx, config.DatabaseURL)
	if err != nil {
		logger.Error("database setup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	store := storage.NewStore(pool)

	producer, err := eventing.NewProducer(brokers, config.ServiceName)
	if err != nil {
		logger.Error("Kafka producer configuration failed", "error", err)
		os.Exit(1)
	}
	defer producer.Close()
	relay := eventing.NewRelay("payment-events", eventing.PaymentEventsTopic, store, eventing.KafkaPublisher{Client: producer}, logger)
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); relay.Run(ctx) }()
	defer func() { stop(); <-relayDone }()

	// Kafka availability is deliberately not a readiness check: while it is down
	// events wait in the outbox and projection lag becomes visible.
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
