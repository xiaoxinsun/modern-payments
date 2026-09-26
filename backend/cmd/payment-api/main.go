// Command payment-api exposes the shared I03 runtime and authentication
// primitives. Payment business endpoints arrive in later backend phases.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gtb.local/backend/internal/platform/auth"
	platformruntime "gtb.local/backend/internal/platform/runtime"
)

func main() {
	logger := slog.New(platformruntime.NewRedactingHandler(slog.NewJSONHandler(os.Stdout, nil)))
	config, err := platformruntime.LoadConfig("payment-api", "127.0.0.1:8082")
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	logger.Info("configuration loaded", config.LogValues()...)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	verifier, err := auth.NewJWTVerifier(auth.Config{
		Issuer: config.OIDCIssuer, Audience: config.OIDCAudience, JWKSURL: config.OIDCJWKSURL, Context: ctx,
	})
	if err != nil {
		logger.Error("authentication configuration failed", "error", err)
		os.Exit(1)
	}

	health := platformruntime.NewHealth(map[string]platformruntime.ReadinessCheck{
		"oidc_signing_keys": verifier.Ready,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", health.Liveness)
	mux.HandleFunc("GET /health/ready", health.Readiness)

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

	handler := platformruntime.WithCorrelation(platformruntime.AccessLog(logger, platformruntime.Recover(logger, mux)))
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
