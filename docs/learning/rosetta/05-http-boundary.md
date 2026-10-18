# Rosetta 05: the HTTP boundary

**Payments idea.** Every request is authenticated (an OIDC bearer token), then
authorized twice:
- a **route-level role check** (only `customer` may submit);
- a **record-level scope check** in the service: the token's `customer_ids`
  must include the payment's customer.

Errors come back in one safe envelope, `{error:{code,message,retryable},
correlationId}`. `retryable: true` means "retry with the same idempotency
key", never "the payment failed". Every response echoes a correlation ID, so a
support engineer can follow one request across services.

Files: `backend/internal/payment/api/handler.go`,
`platform/auth/middleware.go`, `platform/runtime/http.go`,
`cmd/payment-api/main.go`. The OpenAPI contract
`contracts/openapi/v1/payment.json` is authoritative; `api.gen.go` is
generated from it.

## Go: middleware is a function that wraps a handler

The standard library's whole web model is one interface:
`http.Handler { ServeHTTP(ResponseWriter, *Request) }`. Middleware is any
function that takes a handler and returns a handler:

From `backend/internal/platform/auth/middleware.go:21`:

```go
func Authenticate(verifier Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := request.Header.Get("Authorization")
		if len(header) < 8 || !strings.EqualFold(header[:7], "Bearer ") || strings.TrimSpace(header[7:]) == "" {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			platformruntime.WriteError(writer, request, http.StatusUnauthorized, contracts.Unauthenticated, "authentication required", false)
			return
		}
		principal, err := verifier.Verify(request.Context(), strings.TrimSpace(header[7:]))
		// …
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
```

Routes compose the wrappers explicitly. Go 1.22+'s `ServeMux` understands
methods and path parameters:

From `backend/internal/payment/api/handler.go:48`:

```go
func Register(mux *http.ServeMux, verifier auth.Verifier, service PaymentService, journal application.Journal) error {
	handler, err := NewHTTPHandler(service, journal)
	if err != nil {
		return err
	}
	submitters := auth.RequireAnyRole(application.CustomerRole)
	readers := auth.RequireAnyRole(append([]string{application.CustomerRole}, application.OperatorReadRoles...)...)
	mux.Handle("POST /api/v1/payments", auth.Authenticate(verifier, submitters(handler)))
	mux.Handle("GET /api/v1/payments/{paymentId}/receipt", auth.Authenticate(verifier, readers(handler)))
```

And the process wraps everything once more:

From `backend/cmd/payment-api/main.go:131`:

```go
	handler := platformruntime.WithCorrelation(platformruntime.AccessLog(logger,
		platformruntime.Recover(logger, platformtelemetry.HTTPHandler(config.ServiceName, mux))))
```

So a request passes through: correlation ID → access log → panic recovery →
tracing → router → authentication → role check → OpenAPI request validation →
generated parameter binding → your method.

## Go: the handler method, and mapping errors in one place

From `backend/internal/payment/api/handler.go:181`:

```go
func (server *server) SubmitPayment(writer http.ResponseWriter, request *http.Request, params SubmitPaymentParams) {
	var body PaymentRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, request, application.ErrInvalidRequest)
		return
	}
	// …
	principal, _ := auth.PrincipalFromContext(request.Context())
	receipt, err := server.service.Submit(request.Context(), application.SubmitRequest{
		Principal: principal, IdempotencyKey: params.IdempotencyKey, Instruction: instruction,
		CorrelationID: platformruntime.CorrelationID(request.Context()),
	})
	if err != nil {
		writeError(writer, request, err)
		return
	}
	writer.Header().Set("Idempotent-Replayed", strconv.FormatBool(receipt.Replayed))
```

From `backend/internal/payment/api/handler.go:316`:

```go
func writeError(writer http.ResponseWriter, request *http.Request, err error) {
	var pgError *pgconn.PgError
	switch {
	case errors.Is(err, application.ErrInvalidRequest), errors.Is(err, domain.ErrInvalidInstruction):
		platformruntime.WriteError(writer, request, http.StatusBadRequest, contracts.InvalidRequest, safeMessage(err), false)
	case errors.Is(err, application.ErrForbidden):
		platformruntime.WriteError(writer, request, http.StatusForbidden, contracts.Forbidden, "payment access denied", false)
	// …
	case errors.As(err, &pgError), errors.Is(err, context.DeadlineExceeded):
		platformruntime.WriteError(writer, request, http.StatusServiceUnavailable, contracts.DependencyUnavailable, "payment database unavailable; retry with the same Idempotency-Key", true)
	default:
		platformruntime.WriteError(writer, request, http.StatusInternalServerError, contracts.InternalError, "internal server error", false)
	}
}
```

## Java 21 (Servlet API, no framework)

```java
@FunctionalInterface interface Middleware { HttpHandler wrap(HttpHandler next); }

static HttpHandler authenticate(Verifier verifier, HttpHandler next) {
    return (req, res) -> {
        String header = req.getHeader("Authorization");
        if (header == null || !header.regionMatches(true, 0, "Bearer ", 0, 7)) {
            writeError(res, 401, "UNAUTHENTICATED", "authentication required", false); return;
        }
        Principal principal;
        try { principal = verifier.verify(header.substring(7).trim()); }
        catch (InvalidTokenException e) { writeError(res, 401, "UNAUTHENTICATED", "invalid access token", false); return; }
        req.setAttribute("principal", principal);            // Go: context value
        next.handle(req, res);
    };
}

static Middleware requireAnyRole(String... roles) {
    return next -> (req, res) -> {
        var p = (Principal) req.getAttribute("principal");
        if (p != null && Arrays.stream(roles).anyMatch(p::hasRole)) { next.handle(req, res); return; }
        writeError(res, 403, "FORBIDDEN", "insufficient role", false);
    };
}

// Error mapping: what Spring does with @ControllerAdvice, as one method.
static void writeError(HttpServletResponse res, Exception e) {
    switch (e) {
        case InvalidRequestException x -> writeError(res, 400, "INVALID_REQUEST", x.getMessage(), false);
        case ForbiddenException x      -> writeError(res, 403, "FORBIDDEN", "payment access denied", false);
        case SQLException x            -> writeError(res, 503, "DEPENDENCY_UNAVAILABLE", "…", true);
        default                        -> writeError(res, 500, "INTERNAL_ERROR", "internal server error", false);
    }
}
```

## Why the Go looks like this

- **`net/http` is enough.** GTB uses no web framework. Middleware is plain
  function composition, so `Authenticate(verifier, submitters(handler))` reads
  inside out, exactly like nested filters. Nothing is ordered by annotations or
  bean priority.
- **Functions returning functions.** `RequireAnyRole(roles...)` returns a
  middleware, the same curried shape as the Java `Middleware` above.
  `roles ...string` is a varargs parameter, like `String... roles`.
- **`http.HandlerFunc(func(...) {...})`** adapts a function literal to the
  `Handler` interface. It is a named function *type* that has a `ServeHTTP`
  method, which is Go's version of a functional interface.
- **Request-scoped data rides on `context`.**
  `request.WithContext(context.WithValue(...))` returns a new request with the
  principal attached, and downstream code reads it back with
  `auth.PrincipalFromContext`. The key is a private type (`contextKey`), so no
  other package can collide with it or forge it.
- **`principal, _ := auth.PrincipalFromContext(...)`** ignores the `ok` result,
  because `Authenticate` has already guaranteed it.
- **One error switch** replaces exception handlers. Unknown errors become a
  generic 500 with no internal detail, and database errors become a retryable
  503.
- **Generated boundary, hand-written behaviour.** `oapi-codegen` generates
  `PaymentRequest`, `SubmitPaymentParams` and the method-per-operation
  interface. The request validator rejects anything that does not match the
  contract *before* the handler runs, so the handler maps DTOs to domain types
  and calls the service.

## Try it

```sh
cd backend && go test -v -run 'Requests|Responses' ./internal/payment/api/
```

The tests call handlers with `httptest.NewRecorder()` (Go's `MockMvc`) and
validate every response against the OpenAPI contract.
