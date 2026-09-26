package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"gtb.local/backend/internal/contracts"
	"gtb.local/backend/internal/platform/value"
)

const CorrelationHeader = "X-Correlation-ID"

type contextKey string

const correlationKey contextKey = "correlation-id"

func CorrelationID(ctx context.Context) value.ID {
	id, _ := ctx.Value(correlationKey).(value.ID)
	return id
}

func WithCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		correlationID, err := correlationFromRequest(request)
		writer.Header().Set(CorrelationHeader, string(correlationID))
		if err != nil {
			WriteError(writer, request.WithContext(context.WithValue(request.Context(), correlationKey, correlationID)), http.StatusBadRequest, contracts.InvalidRequest, "invalid correlation ID", false)
			return
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), correlationKey, correlationID)))
	})
}

func correlationFromRequest(request *http.Request) (value.ID, error) {
	generated, err := value.NewID()
	if err != nil {
		panic("operating system random source unavailable")
	}
	raw := request.Header.Get(CorrelationHeader)
	if raw == "" {
		return generated, nil
	}
	provided := value.ID(raw)
	if err := provided.Validate(); err != nil {
		return generated, err
	}
	return provided, nil
}

func WriteJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func WriteError(writer http.ResponseWriter, request *http.Request, status int, code contracts.ErrorCode, message string, retryable bool) {
	WriteJSON(writer, status, contracts.ErrorEnvelope{
		Error:         contracts.APIError{Code: code, Message: message, Retryable: retryable},
		CorrelationID: CorrelationID(request.Context()),
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (writer *responseRecorder) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func AccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(recorder, request)
		logger.InfoContext(request.Context(), "http request",
			"method", request.Method,
			"path", request.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds(),
			"correlation_id", CorrelationID(request.Context()),
		)
	})
}

func Recover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(request.Context(), "request panic recovered",
					"correlation_id", CorrelationID(request.Context()),
					"panic_type", strings.TrimPrefix(strings.SplitN(string(debug.Stack()), "\n", 2)[0], "panic: "),
				)
				WriteError(writer, request, http.StatusInternalServerError, contracts.InternalError, "internal server error", false)
			}
		}()
		next.ServeHTTP(writer, request)
	})
}

type ReadinessCheck func(context.Context) error

type Health struct {
	ready  atomic.Bool
	checks map[string]ReadinessCheck
}

func NewHealth(checks map[string]ReadinessCheck) *Health {
	health := &Health{checks: checks}
	health.ready.Store(true)
	return health
}

func (health *Health) SetAcceptingRequests(ready bool) {
	health.ready.Store(ready)
}

func (health *Health) Liveness(writer http.ResponseWriter, _ *http.Request) {
	WriteJSON(writer, http.StatusOK, map[string]string{"status": "alive"})
}

func (health *Health) Readiness(writer http.ResponseWriter, request *http.Request) {
	if !health.ready.Load() {
		WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	for name, check := range health.checks {
		ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		err := check(ctx)
		cancel()
		if err != nil {
			WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "dependency": name})
			return
		}
	}
	WriteJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func Run(ctx context.Context, logger *slog.Logger, server *http.Server, health *Health, shutdownTimeout time.Duration) error {
	errorsChannel := make(chan error, 1)
	go func() {
		logger.Info("http server starting", "address", server.Addr)
		errorsChannel <- server.ListenAndServe()
	}()

	select {
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		health.SetAcceptingRequests(false)
		logger.Info("shutdown requested")
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			return err
		}
		err := <-errorsChannel
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		logger.Info("shutdown complete")
		return nil
	}
}
