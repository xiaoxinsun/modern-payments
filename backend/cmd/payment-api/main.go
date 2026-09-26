// Command payment-api durably receives customer payment instructions and serves
// command-store receipts. It keeps the I03 runtime demonstration routes.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"gtb.local/backend/internal/adapters/corebanking"
	"gtb.local/backend/internal/payment/api"
	"gtb.local/backend/internal/payment/application"
	"gtb.local/backend/internal/payment/storage"
	"gtb.local/backend/internal/platform/auth"
	platformruntime "gtb.local/backend/internal/platform/runtime"
	platformtelemetry "gtb.local/backend/internal/platform/telemetry"
)

func main() {
	logger := slog.New(platformruntime.NewRedactingHandler(slog.NewJSONHandler(os.Stdout, nil)))
	config, err := platformruntime.LoadConfig("payment-api", "127.0.0.1:8082")
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	serviceClient, err := platformruntime.LoadServiceClient(config, "payment-service")
	if err != nil {
		logger.Error("invalid service client configuration", "error", err)
		os.Exit(1)
	}
	coreBankingURL := os.Getenv("GTB_COREBANKING_URL")
	if coreBankingURL == "" {
		coreBankingURL = "http://127.0.0.1:8084"
	}
	logger.Info("configuration loaded", append(config.LogValues(), "service_client_id", serviceClient.ClientID, "core_banking_url", coreBankingURL)...)
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

	// Tokens are fetched and cached per client-credentials grant; the base
	// transport instruments both token and Core Banking calls.
	outbound := &http.Client{Timeout: 5 * time.Second, Transport: platformtelemetry.HTTPTransport(http.DefaultTransport)}
	credentials := clientcredentials.Config{
		ClientID: serviceClient.ClientID, ClientSecret: serviceClient.ClientSecret, TokenURL: serviceClient.TokenURL,
	}
	coreBankingHTTP := credentials.Client(context.WithValue(ctx, oauth2.HTTPClient, outbound))
	coreBankingHTTP.Timeout = 5 * time.Second
	accounts, err := corebanking.NewClient(coreBankingURL, coreBankingHTTP)
	if err != nil {
		logger.Error("Core Banking client configuration failed", "error", err)
		os.Exit(1)
	}

	verifier, err := auth.NewJWTVerifier(auth.Config{
		Issuer: config.OIDCIssuer, Audience: config.OIDCAudience, JWKSURL: config.OIDCJWKSURL, Context: ctx,
	})
	if err != nil {
		logger.Error("authentication configuration failed", "error", err)
		os.Exit(1)
	}

	health := platformruntime.NewHealth(map[string]platformruntime.ReadinessCheck{
		"database": store.Ready, "oidc_signing_keys": verifier.Ready,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", health.Liveness)
	mux.HandleFunc("GET /health/ready", health.Readiness)
	if err := api.Register(mux, verifier, application.NewService(store, accounts)); err != nil {
		logger.Error("payment API contract setup failed", "error", err)
		os.Exit(1)
	}

	jsonOK := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, _ := auth.PrincipalFromContext(request.Context())
		platformruntime.WriteJSON(writer, http.StatusOK, map[string]string{
			"status": "ok", "subject": principal.Subject,
		})
	})
	mux.Handle("GET /api/v1/runtime/customers/{customerId}",
		auth.Authenticate(verifier,
			auth.RequireAnyRole("customer")(
				auth.RequireCustomerScope(func(request *http.Request) string { return request.PathValue("customerId") })(jsonOK),
			),
		),
	)
	mux.Handle("GET /api/v1/runtime/operations",
		auth.Authenticate(verifier,
			auth.RequireAnyRole("ops_viewer", "investigator", "approver", "compliance_reviewer", "compliance_approver", "auditor")(jsonOK),
		),
	)
	mux.Handle("POST /integrations/v1/runtime/messages",
		auth.Authenticate(verifier, auth.RequireAnyRole("integration_ingress")(jsonOK)),
	)
	if config.DevControls {
		mux.Handle("GET /dev/v1/runtime/config",
			auth.Authenticate(verifier, auth.RequireAnyRole("platform_admin")(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				platformruntime.WriteJSON(writer, http.StatusOK, map[string]any{
					"service": config.ServiceName, "profile": config.Profile, "devControls": true,
				})
			}))),
		)
	}

	handler := platformruntime.WithCorrelation(platformruntime.AccessLog(logger,
		platformruntime.Recover(logger, platformtelemetry.HTTPHandler(config.ServiceName, mux))))
	server := &http.Server{
		Addr:              config.HTTPAddress,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 * 1024,
	}
	if err := platformruntime.Run(ctx, logger, server, health, config.ShutdownTimeout); err != nil {
		logger.Error("http server stopped with error", "error", err)
		os.Exit(1)
	}
}
