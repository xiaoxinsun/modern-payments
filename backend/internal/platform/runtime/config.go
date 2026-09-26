// Package runtime provides shared process configuration and HTTP lifecycle
// primitives. It contains no business or authorization policy.
package runtime

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ProfileLocal      = "local"
	ProfileProduction = "production"
)

type Config struct {
	ServiceName       string
	HTTPAddress       string
	Profile           string
	ShutdownTimeout   time.Duration
	DevControls       bool
	OIDCIssuer        string
	OIDCAudience      string
	OIDCJWKSURL       string
	DatabaseURL       string
	IntegrationSecret string
}

func LoadConfig(serviceName, defaultAddress string) (Config, error) {
	config := Config{
		ServiceName:       serviceName,
		HTTPAddress:       envOrDefault("GTB_HTTP_ADDRESS", defaultAddress),
		Profile:           envOrDefault("GTB_PROFILE", ProfileLocal),
		ShutdownTimeout:   10 * time.Second,
		DevControls:       false,
		OIDCIssuer:        envOrDefault("GTB_AUTH_ISSUER", "http://localhost:8080/realms/gtb"),
		OIDCAudience:      envOrDefault("GTB_AUTH_AUDIENCE", serviceName),
		OIDCJWKSURL:       os.Getenv("GTB_AUTH_JWKS_URL"),
		DatabaseURL:       os.Getenv("GTB_DATABASE_URL"),
		IntegrationSecret: os.Getenv("GTB_INTEGRATION_SECRET"),
	}
	if config.OIDCJWKSURL == "" {
		config.OIDCJWKSURL = strings.TrimRight(config.OIDCIssuer, "/") + "/protocol/openid-connect/certs"
	}
	if raw := os.Getenv("GTB_SHUTDOWN_TIMEOUT"); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("GTB_SHUTDOWN_TIMEOUT: %w", err)
		}
		config.ShutdownTimeout = value
	}
	if raw := os.Getenv("GTB_ENABLE_DEV_CONTROLS"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("GTB_ENABLE_DEV_CONTROLS: %w", err)
		}
		config.DevControls = value
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if c.ServiceName == "" || c.HTTPAddress == "" || c.OIDCIssuer == "" || c.OIDCAudience == "" || c.OIDCJWKSURL == "" {
		return errors.New("service name, HTTP address, OIDC issuer, audience and JWKS URL are required")
	}
	if c.Profile != ProfileLocal && c.Profile != ProfileProduction {
		return fmt.Errorf("unsupported GTB_PROFILE %q", c.Profile)
	}
	if c.ShutdownTimeout <= 0 || c.ShutdownTimeout > time.Minute {
		return errors.New("shutdown timeout must be greater than zero and at most one minute")
	}
	if c.DevControls && c.Profile != ProfileLocal {
		return errors.New("development controls are allowed only in the local profile")
	}
	if err := validateIdentityURL("OIDC issuer", c.OIDCIssuer, c.Profile); err != nil {
		return err
	}
	if err := validateIdentityURL("OIDC JWKS URL", c.OIDCJWKSURL, c.Profile); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(c.HTTPAddress)
	if err != nil {
		return fmt.Errorf("invalid HTTP address: %w", err)
	}
	if c.Profile == ProfileLocal && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return errors.New("local profile HTTP address must bind to loopback")
	}
	return nil
}

func validateIdentityURL(name, raw, profile string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL", name)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must not contain user information, query parameters or a fragment", name)
	}
	if profile == ProfileProduction && parsed.Scheme != "https" {
		return fmt.Errorf("%s must use HTTPS in the production profile", name)
	}
	return nil
}

// ServiceClient is an OAuth client-credentials identity used for calls to other
// GTB services. The secret is never logged.
type ServiceClient struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
}

func LoadServiceClient(config Config, defaultClientID string) (ServiceClient, error) {
	client := ServiceClient{
		TokenURL:     envOrDefault("GTB_SERVICE_TOKEN_URL", strings.TrimRight(config.OIDCIssuer, "/")+"/protocol/openid-connect/token"),
		ClientID:     envOrDefault("GTB_SERVICE_CLIENT_ID", defaultClientID),
		ClientSecret: os.Getenv("GTB_SERVICE_CLIENT_SECRET"),
	}
	if client.ClientID == "" || client.ClientSecret == "" {
		return ServiceClient{}, errors.New("GTB_SERVICE_CLIENT_ID and GTB_SERVICE_CLIENT_SECRET are required")
	}
	if err := validateIdentityURL("service token URL", client.TokenURL, config.Profile); err != nil {
		return ServiceClient{}, err
	}
	return client, nil
}

// LogValues deliberately omits every credential and connection string.
func (c Config) LogValues() []any {
	return []any{
		"service", c.ServiceName,
		"http_address", c.HTTPAddress,
		"profile", c.Profile,
		"dev_controls", c.DevControls,
		"oidc_issuer", c.OIDCIssuer,
		"oidc_audience", c.OIDCAudience,
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
