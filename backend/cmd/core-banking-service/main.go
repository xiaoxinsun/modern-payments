// Command core-banking-service serves the authenticated Core Banking port and
// relays its posting outbox to Kafka.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gtb.local/backend/internal/corebanking"
	"gtb.local/backend/internal/eventing"
	"gtb.local/backend/internal/platform/auth"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	platformtelemetry "gtb.local/backend/internal/platform/telemetry"
)

func main() {
	logger := slog.New(platformruntime.NewRedactingHandler(slog.NewJSONHandler(os.Stdout, nil)))
	config, err := platformruntime.LoadConfig("core-banking-service", "127.0.0.1:8084")
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	logger.Info("configuration loaded", config.LogValues()...)
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

	pool, err := corebanking.Open(ctx, config.DatabaseURL)
	if err != nil {
		logger.Error("database setup failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	service := corebanking.NewService(pool)
	verifier, err := auth.NewJWTVerifier(auth.Config{
		Issuer: config.OIDCIssuer, Audience: config.OIDCAudience, JWKSURL: config.OIDCJWKSURL, Context: ctx,
	})
	if err != nil {
		logger.Error("authentication configuration failed", "error", err)
		os.Exit(1)
	}

	// Kafka availability does not affect readiness: undelivered postings stay in
	// the outbox and the relay retries with backoff.
	brokers := eventing.Brokers()
	producer, err := eventing.NewProducer(brokers, config.ServiceName)
	if err != nil {
		logger.Error("Kafka producer configuration failed", "error", err)
		os.Exit(1)
	}
	defer producer.Close()
	relay := eventing.NewRelay("corebanking-postings", eventing.CoreBankingPostingsTopic, service, eventing.KafkaPublisher{Client: producer}, logger)
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); relay.Run(ctx) }()
	defer func() { stop(); <-relayDone }()
	logger.Info("posting outbox relay configured", "kafka_brokers", brokers, "topic", eventing.CoreBankingPostingsTopic)

	health := platformruntime.NewHealth(map[string]platformruntime.ReadinessCheck{
		"database": service.Ready, "oidc_signing_keys": verifier.Ready,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", health.Liveness)
	mux.HandleFunc("GET /health/ready", health.Readiness)
	mux.Handle("/integrations/v1/core-banking/",
		auth.Authenticate(verifier,
			auth.RequireAnyRole("internal_service")(corebanking.NewHTTPHandler(service)),
		),
	)
	handler := platformruntime.WithCorrelation(platformruntime.AccessLog(logger,
		platformruntime.Recover(logger, platformtelemetry.HTTPHandler(config.ServiceName, mux))))
	server := &http.Server{
		Addr: config.HTTPAddress, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024,
	}
	if err := platformruntime.Run(ctx, logger, server, health, config.ShutdownTimeout); err != nil {
		logger.Error("HTTP server stopped with error", "error", err)
		os.Exit(1)
	}
}
