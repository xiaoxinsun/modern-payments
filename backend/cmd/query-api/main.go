// Command query-api serves the customer and operations payment queries from the
// projections database, using the read-only query_reader login.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gtb.local/backend/internal/adapters/paymentreceipts"
	"gtb.local/backend/internal/platform/auth"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	platformtelemetry "gtb.local/backend/internal/platform/telemetry"
	"gtb.local/backend/internal/projections"
	"gtb.local/backend/internal/projections/api"
	"gtb.local/backend/internal/projections/query"
)

func main() {
	logger := slog.New(platformruntime.NewRedactingHandler(slog.NewJSONHandler(os.Stdout, nil)))
	config, err := platformruntime.LoadConfig("query-api", "127.0.0.1:8083")
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	// Commands and queries are one public /api/v1 surface, so customer and
	// operator tokens carry the payment-api audience for both services.
	if os.Getenv("GTB_AUTH_AUDIENCE") == "" {
		config.OIDCAudience = "payment-api"
	}
	paymentAPIURL := os.Getenv("GTB_PAYMENT_API_URL")
	if paymentAPIURL == "" {
		paymentAPIURL = "http://127.0.0.1:8082"
	}
	staleAfter := 10 * time.Second
	if raw := os.Getenv("GTB_PROJECTION_STALE_AFTER"); raw != "" {
		if staleAfter, err = time.ParseDuration(raw); err != nil || staleAfter <= 0 {
			logger.Error("invalid GTB_PROJECTION_STALE_AFTER", "value", raw)
			os.Exit(1)
		}
	}
	logger.Info("configuration loaded", append(config.LogValues(), "payment_api_url", paymentAPIURL, "stale_after", staleAfter.String())...)
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
	receipts, err := paymentreceipts.NewClient(paymentAPIURL, &http.Client{
		Timeout: 5 * time.Second, Transport: platformtelemetry.HTTPTransport(http.DefaultTransport),
	})
	if err != nil {
		logger.Error("payment receipt client configuration failed", "error", err)
		os.Exit(1)
	}
	service := query.NewService(pool, receipts, staleAfter)

	verifier, err := auth.NewJWTVerifier(auth.Config{
		Issuer: config.OIDCIssuer, Audience: config.OIDCAudience, JWKSURL: config.OIDCJWKSURL, Context: ctx,
	})
	if err != nil {
		logger.Error("authentication configuration failed", "error", err)
		os.Exit(1)
	}
	health := platformruntime.NewHealth(map[string]platformruntime.ReadinessCheck{
		"database": service.Ready, "oidc_signing_keys": verifier.Ready,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", health.Liveness)
	mux.HandleFunc("GET /health/ready", health.Readiness)
	if err := api.Register(mux, verifier, service); err != nil {
		logger.Error("query API contract setup failed", "error", err)
		os.Exit(1)
	}
	handler := platformruntime.WithCorrelation(platformruntime.AccessLog(logger,
		platformruntime.Recover(logger, platformtelemetry.HTTPHandler(config.ServiceName, mux))))
	server := &http.Server{
		Addr: config.HTTPAddress, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024,
	}
	if err := platformruntime.Run(ctx, logger, server, health, config.ShutdownTimeout); err != nil {
		logger.Error("http server stopped with error", "error", err)
		os.Exit(1)
	}
}
