// Package auth validates OIDC access tokens and enforces reusable HTTP role and
// customer-scope policies. Caller-provided identifiers never grant authority.
package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

var ErrInvalidToken = errors.New("invalid access token")

type Principal struct {
	Subject     string
	ClientID    string
	Roles       map[string]struct{}
	CustomerIDs map[string]struct{}
}

func (principal Principal) HasRole(role string) bool {
	_, ok := principal.Roles[role]
	return ok
}

func (principal Principal) CanAccessCustomer(customerID string) bool {
	_, ok := principal.CustomerIDs[customerID]
	return ok
}

type Verifier interface {
	Verify(context.Context, string) (Principal, error)
}

type Config struct {
	Issuer          string
	Audience        string
	JWKSURL         string
	Client          *http.Client
	Context         context.Context
	Now             func() time.Time
	RefreshInterval time.Duration
}

type JWTVerifier struct {
	audience string
	keys     keyfunc.Keyfunc
	options  []jwt.ParserOption
}

func NewJWTVerifier(config Config) (*JWTVerifier, error) {
	if config.Issuer == "" || config.Audience == "" || config.JWKSURL == "" {
		return nil, errors.New("OIDC issuer, audience and JWKS URL are required")
	}
	if config.Context == nil {
		config.Context = context.Background()
	}
	if config.Client == nil {
		config.Client = &http.Client{Timeout: 3 * time.Second}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.RefreshInterval == 0 {
		config.RefreshInterval = 5 * time.Minute
	}
	if config.RefreshInterval < time.Second {
		return nil, errors.New("JWKS refresh interval must be at least one second")
	}

	remoteKeys, err := keyfunc.NewDefaultOverrideCtx(config.Context, []string{config.JWKSURL}, keyfunc.Override{
		Client:          config.Client,
		HTTPTimeout:     3 * time.Second,
		RefreshInterval: config.RefreshInterval,
	})
	if err != nil {
		return nil, err
	}
	keys, err := keyfunc.New(keyfunc.Options{
		Ctx:          config.Context,
		Storage:      remoteKeys.Storage(),
		UseWhitelist: []jwkset.USE{jwkset.UseSig},
	})
	if err != nil {
		return nil, err
	}
	return &JWTVerifier{
		audience: config.Audience,
		keys:     keys,
		options: []jwt.ParserOption{
			jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
			jwt.WithIssuer(config.Issuer),
			jwt.WithAudience(config.Audience),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithStrictDecoding(),
			jwt.WithTimeFunc(config.Now),
		},
	}, nil
}

type tokenClaims struct {
	jwt.RegisteredClaims
	Authorized  string   `json:"azp"`
	ClientID    string   `json:"client_id"`
	CustomerIDs []string `json:"customer_ids"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	ResourceAccess map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

func (verifier *JWTVerifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if len(raw) == 0 || len(raw) > 16*1024 {
		return Principal{}, ErrInvalidToken
	}
	claims := tokenClaims{}
	keyForToken := verifier.keys.KeyfuncCtx(ctx)
	token, err := jwt.ParseWithClaims(raw, &claims, func(token *jwt.Token) (any, error) {
		keyID, ok := token.Header[jwkset.HeaderKID].(string)
		if !ok || keyID == "" {
			return nil, ErrInvalidToken
		}
		return keyForToken(token)
	}, verifier.options...)
	if err != nil || !token.Valid || claims.Subject == "" {
		return Principal{}, ErrInvalidToken
	}

	roles := make(map[string]struct{})
	for _, role := range claims.RealmAccess.Roles {
		roles[role] = struct{}{}
	}
	// Client roles from unrelated resources cannot authorize this audience.
	for _, role := range claims.ResourceAccess[verifier.audience].Roles {
		roles[role] = struct{}{}
	}
	customers := make(map[string]struct{})
	for _, customerID := range claims.CustomerIDs {
		if customerID != "" {
			customers[customerID] = struct{}{}
		}
	}
	clientID := claims.Authorized
	if clientID == "" {
		clientID = claims.ClientID
	}
	return Principal{Subject: claims.Subject, ClientID: clientID, Roles: roles, CustomerIDs: customers}, nil
}

func (verifier *JWTVerifier) Ready(ctx context.Context) error {
	keys, err := verifier.keys.Storage().KeyReadAll(ctx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.Marshal().USE == jwkset.UseSig {
			return nil
		}
	}
	return errors.New("OIDC signing keys are unavailable")
}
