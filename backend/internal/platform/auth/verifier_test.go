package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestJWTVerifierValidatesSignatureClaimsRolesAndScope(t *testing.T) {
	privateKey, jwks := signingKey(t)
	now := time.Unix(1_800_000_000, 0)
	verifier, err := NewJWTVerifier(Config{
		Issuer: "https://issuer.example/realms/gtb", Audience: "payment-api", JWKSURL: "https://jwks.invalid",
		Client: jwksClient(jwks), Context: t.Context(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{
		"iss": "https://issuer.example/realms/gtb", "sub": "subject-1",
		"aud": []string{"account", "payment-api"}, "exp": now.Add(time.Minute).Unix(), "iat": now.Unix(),
		"azp": "gtb-cli", "realm_access": map[string]any{"roles": []string{"customer"}},
		"resource_access": map[string]any{
			"payment-api": map[string]any{"roles": []string{"api-reader"}},
			"other-api":   map[string]any{"roles": []string{"unrelated-admin"}},
		},
		"customer_ids": []string{"cust-demo-001"},
	}
	token := signedToken(t, privateKey, claims)
	principal, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Subject != "subject-1" || principal.ClientID != "gtb-cli" || !principal.HasRole("customer") || !principal.HasRole("api-reader") || principal.HasRole("unrelated-admin") || !principal.CanAccessCustomer("cust-demo-001") {
		t.Fatalf("unexpected principal: %#v", principal)
	}
}

func TestJWTVerifierRejectsInvalidTokens(t *testing.T) {
	privateKey, jwks := signingKey(t)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := time.Unix(1_800_000_000, 0)
	verifier, err := NewJWTVerifier(Config{
		Issuer: "issuer", Audience: "payment-api", JWKSURL: "https://jwks.invalid",
		Client: jwksClient(jwks), Context: t.Context(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"iss": "issuer", "sub": "subject", "aud": "payment-api", "exp": now.Add(time.Minute).Unix()}
	hs256, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims(base)).SignedString([]byte("not-an-rsa-key"))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"malformed":         "not-a-jwt",
		"bad signature":     signedToken(t, otherKey, base),
		"missing key ID":    signedTokenWithHeader(t, privateKey, base, map[string]string{"alg": "RS256", "typ": "JWT"}),
		"wrong algorithm":   hs256,
		"expired":           signedToken(t, privateKey, merge(base, "exp", now.Add(-time.Second).Unix())),
		"missing expiry":    signedToken(t, privateKey, without(base, "exp")),
		"missing subject":   signedToken(t, privateKey, merge(base, "sub", "")),
		"wrong issuer":      signedToken(t, privateKey, merge(base, "iss", "someone-else")),
		"wrong audience":    signedToken(t, privateKey, merge(base, "aud", "other-api")),
		"future not-before": signedToken(t, privateKey, merge(base, "nbf", now.Add(time.Minute).Unix())),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), token); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestJWTVerifierReadinessFailsWithoutSigningKeys(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	})}
	verifier, err := NewJWTVerifier(Config{
		Issuer: "issuer", Audience: "payment-api", JWKSURL: "https://jwks.invalid",
		Client: client, Context: t.Context(), RefreshInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Ready(context.Background()); err == nil {
		t.Fatal("expected readiness to fail while no signing keys are available")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jwksClient(document []byte) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(document)),
		}, nil
	})}
}

func signingKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	document := map[string]any{"keys": []map[string]any{{
		"kid": "test-key", "kty": "RSA", "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return key, data
}

func signedToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	return signedTokenWithHeader(t, key, claims, map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
}

func signedTokenWithHeader(t *testing.T, key *rsa.PrivateKey, claims map[string]any, values map[string]string) string {
	t.Helper()
	header, _ := json.Marshal(values)
	payload, _ := json.Marshal(claims)
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(encodedHeader + "." + encodedPayload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return encodedHeader + "." + encodedPayload + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func merge(source map[string]any, key string, value any) map[string]any {
	copy := make(map[string]any, len(source)+1)
	for name, item := range source {
		copy[name] = item
	}
	copy[key] = value
	return copy
}

func without(source map[string]any, key string) map[string]any {
	copy := make(map[string]any, len(source))
	for name, item := range source {
		if name != key {
			copy[name] = item
		}
	}
	return copy
}
