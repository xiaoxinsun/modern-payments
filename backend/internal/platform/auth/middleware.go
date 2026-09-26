package auth

import (
	"context"
	"net/http"
	"strings"

	"gtb.local/backend/internal/contracts"
	platformruntime "gtb.local/backend/internal/platform/runtime"
)

type contextKey string

const principalKey contextKey = "principal"

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey).(Principal)
	return principal, ok
}

func Authenticate(verifier Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := request.Header.Get("Authorization")
		if len(header) < 8 || !strings.EqualFold(header[:7], "Bearer ") || strings.TrimSpace(header[7:]) == "" {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			platformruntime.WriteError(writer, request, http.StatusUnauthorized, contracts.Unauthenticated, "authentication required", false)
			return
		}
		principal, err := verifier.Verify(request.Context(), strings.TrimSpace(header[7:]))
		if err != nil {
			writer.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			platformruntime.WriteError(writer, request, http.StatusUnauthorized, contracts.Unauthenticated, "invalid access token", false)
			return
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), principalKey, principal)))
	})
}

func RequireAnyRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			principal, ok := PrincipalFromContext(request.Context())
			if ok {
				for _, role := range roles {
					if principal.HasRole(role) {
						next.ServeHTTP(writer, request)
						return
					}
				}
			}
			platformruntime.WriteError(writer, request, http.StatusForbidden, contracts.Forbidden, "insufficient role", false)
		})
	}
}

func RequireCustomerScope(customerID func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			principal, ok := PrincipalFromContext(request.Context())
			if !ok || !principal.CanAccessCustomer(customerID(request)) {
				platformruntime.WriteError(writer, request, http.StatusForbidden, contracts.Forbidden, "customer scope denied", false)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}
}
