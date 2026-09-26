package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCorrelationMiddleware(t *testing.T) {
	endpoint := WithCorrelation(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		WriteJSON(writer, http.StatusOK, map[string]string{"correlationId": string(CorrelationID(request.Context()))})
	}))
	t.Run("generates", func(t *testing.T) {
		response := httptest.NewRecorder()
		endpoint.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusOK || response.Header().Get(CorrelationHeader) == "" {
			t.Fatalf("unexpected response: %d %#v", response.Code, response.Header())
		}
	})
	t.Run("accepts UUIDv4", func(t *testing.T) {
		const correlationID = "123e4567-e89b-42d3-a456-426614174000"
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set(CorrelationHeader, correlationID)
		response := httptest.NewRecorder()
		endpoint.ServeHTTP(response, request)
		if response.Header().Get(CorrelationHeader) != correlationID {
			t.Fatal("valid correlation ID was not echoed")
		}
	})
	t.Run("rejects unsafe input without echo", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set(CorrelationHeader, "unsafe\r\nvalue")
		response := httptest.NewRecorder()
		endpoint.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "unsafe") || response.Header().Get(CorrelationHeader) == "unsafe\r\nvalue" {
			t.Fatalf("unsafe correlation handling: %d %q %#v", response.Code, response.Body.String(), response.Header())
		}
	})
}

func TestReadiness(t *testing.T) {
	failure := errors.New("unavailable")
	health := NewHealth(map[string]ReadinessCheck{"oidc": func(context.Context) error { return failure }})
	response := httptest.NewRecorder()
	health.Readiness(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), failure.Error()) {
		t.Fatalf("dependency error leaked or wrong status: %d %q", response.Code, response.Body.String())
	}
	health = NewHealth(map[string]ReadinessCheck{"oidc": func(context.Context) error { return nil }})
	response = httptest.NewRecorder()
	health.Readiness(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("healthy status: %d", response.Code)
	}
	health.SetAcceptingRequests(false)
	response = httptest.NewRecorder()
	health.Readiness(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining status: %d", response.Code)
	}
}

func TestRedactingHandler(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(NewRedactingHandler(slog.NewJSONHandler(&output, nil)))
	logger.Info("test", "password", "top-secret", "access_token", "token-value", "safe", "visible")
	text := output.String()
	if strings.Contains(text, "top-secret") || strings.Contains(text, "token-value") || !strings.Contains(text, "[REDACTED]") || !strings.Contains(text, "visible") {
		t.Fatalf("unexpected redaction: %s", text)
	}
}

func TestConfigRejectsDevelopmentControlsOutsideLocal(t *testing.T) {
	t.Setenv("GTB_PROFILE", ProfileProduction)
	t.Setenv("GTB_ENABLE_DEV_CONTROLS", "true")
	if _, err := LoadConfig("payment-api", "127.0.0.1:8082"); err == nil {
		t.Fatal("expected production development controls to be rejected")
	}
}

func TestConfigRejectsCredentialsInIdentityURL(t *testing.T) {
	t.Setenv("GTB_AUTH_ISSUER", "https://user:secret@issuer.example/realms/gtb")
	if _, err := LoadConfig("payment-api", "127.0.0.1:8082"); err == nil {
		t.Fatal("expected identity URL user information to be rejected")
	}
}

func TestConfigRequiresHTTPSForProductionIdentity(t *testing.T) {
	t.Setenv("GTB_PROFILE", ProfileProduction)
	if _, err := LoadConfig("payment-api", "127.0.0.1:8082"); err == nil {
		t.Fatal("expected insecure production identity URL to be rejected")
	}
}

func TestConfigLogValuesOmitSecrets(t *testing.T) {
	config := Config{ServiceName: "payment-api", HTTPAddress: "127.0.0.1:8082", Profile: ProfileLocal, OIDCIssuer: "issuer", OIDCAudience: "audience", DatabaseURL: "secret-dsn", IntegrationSecret: "secret-value"}
	text := slog.GroupValue(anyToAttrs(config.LogValues())...).String()
	if strings.Contains(text, "secret-dsn") || strings.Contains(text, "secret-value") {
		t.Fatalf("secret in config log values: %s", text)
	}
}

func anyToAttrs(values []any) []slog.Attr {
	attributes := make([]slog.Attr, 0, len(values)/2)
	for index := 0; index < len(values); index += 2 {
		attributes = append(attributes, slog.Any(values[index].(string), values[index+1]))
	}
	return attributes
}

func TestLoadServiceClient(t *testing.T) {
	config := Config{Profile: ProfileLocal, OIDCIssuer: "http://localhost:8080/realms/gtb"}
	if _, err := LoadServiceClient(config, "payment-service"); err == nil {
		t.Fatal("expected a missing client secret to be rejected")
	}
	t.Setenv("GTB_SERVICE_CLIENT_SECRET", "local-secret")
	client, err := LoadServiceClient(config, "payment-service")
	if err != nil || client.ClientID != "payment-service" || client.TokenURL != "http://localhost:8080/realms/gtb/protocol/openid-connect/token" {
		t.Fatalf("got %+v %v", client, err)
	}
	t.Setenv("GTB_SERVICE_TOKEN_URL", "https://user:secret@issuer.example/token")
	if _, err := LoadServiceClient(config, "payment-service"); err == nil {
		t.Fatal("expected token URL credentials to be rejected")
	}
}
