package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	platformruntime "gtb.local/backend/internal/platform/runtime"
)

type stubVerifier struct {
	principal Principal
	err       error
}

func (verifier stubVerifier) Verify(context.Context, string) (Principal, error) {
	return verifier.principal, verifier.err
}

func TestAuthenticationAndAuthorizationMiddleware(t *testing.T) {
	allowed := Principal{
		Subject: "subject", Roles: map[string]struct{}{"customer": {}},
		CustomerIDs: map[string]struct{}{"cust-1": {}},
	}
	endpoint := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	policy := func(verifier Verifier) http.Handler {
		return platformruntime.WithCorrelation(Authenticate(verifier,
			RequireAnyRole("customer")(RequireCustomerScope(func(*http.Request) string { return "cust-1" })(endpoint)),
		))
	}
	tests := []struct {
		name       string
		verifier   Verifier
		auth       string
		wantStatus int
	}{
		{"missing token", stubVerifier{}, "", http.StatusUnauthorized},
		{"invalid token", stubVerifier{err: errors.New("bad")}, "Bearer invalid", http.StatusUnauthorized},
		{"wrong role", stubVerifier{principal: Principal{Roles: map[string]struct{}{"auditor": {}}, CustomerIDs: map[string]struct{}{"cust-1": {}}}}, "Bearer token", http.StatusForbidden},
		{"wrong customer", stubVerifier{principal: Principal{Roles: map[string]struct{}{"customer": {}}, CustomerIDs: map[string]struct{}{"cust-2": {}}}}, "Bearer token", http.StatusForbidden},
		{"allowed", stubVerifier{principal: allowed}, "Bearer token", http.StatusNoContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", test.auth)
			response := httptest.NewRecorder()
			policy(test.verifier).ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if response.Header().Get(platformruntime.CorrelationHeader) == "" {
				t.Fatal("missing correlation response header")
			}
		})
	}
}
