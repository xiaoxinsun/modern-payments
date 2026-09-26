// Package contracts defines shared wire envelopes. It owns no persistence or
// authorization. Call Validate before publishing or accepting an envelope.
package contracts

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"gtb.local/backend/internal/platform/value"
)

const SchemaVersion = 1

// Event sequence is strictly increasing per aggregate even when one transition
// emits multiple events with the same aggregate version. Retries reuse event ID.
type Event struct {
	EventID          value.ID        `json:"eventId"`
	EventType        string          `json:"eventType"`
	SchemaVersion    int             `json:"schemaVersion"`
	AggregateType    string          `json:"aggregateType"`
	AggregateID      value.ID        `json:"aggregateId"`
	AggregateVersion int64           `json:"aggregateVersion,string"`
	EventSequence    int64           `json:"eventSequence,string"`
	OccurredAt       time.Time       `json:"occurredAt"`
	RecordedAt       time.Time       `json:"recordedAt"`
	CorrelationID    value.ID        `json:"correlationId"`
	CausationID      value.ID        `json:"causationId"`
	Source           string          `json:"source"`
	Payload          json.RawMessage `json:"payload"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)

func (e Event) Validate() error {
	for _, id := range []value.ID{e.EventID, e.AggregateID, e.CorrelationID, e.CausationID} {
		if err := id.Validate(); err != nil {
			return err
		}
	}
	if e.SchemaVersion != SchemaVersion || !namePattern.MatchString(e.EventType) || !namePattern.MatchString(e.AggregateType) || !namePattern.MatchString(e.Source) {
		return errors.New("invalid event version or name")
	}
	if e.AggregateVersion < 1 || e.AggregateVersion > value.MaxMinorUnits || e.EventSequence < 1 || e.EventSequence > value.MaxMinorUnits {
		return errors.New("event version and sequence must be positive 18-digit integers")
	}
	for _, timestamp := range []time.Time{e.OccurredAt, e.RecordedAt} {
		_, offset := timestamp.Zone()
		if timestamp.IsZero() || offset != 0 || timestamp.Year() < 1 || timestamp.Year() > 9999 {
			return errors.New("event timestamps must be nonzero UTC times")
		}
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(e.Payload, &payload); err != nil || payload == nil {
		return errors.New("event payload must be a JSON object")
	}
	return nil
}

type ErrorCode string

const (
	InvalidRequest        ErrorCode = "INVALID_REQUEST"
	Unauthenticated       ErrorCode = "UNAUTHENTICATED"
	Forbidden             ErrorCode = "FORBIDDEN"
	NotFound              ErrorCode = "NOT_FOUND"
	Conflict              ErrorCode = "CONFLICT"
	RateLimited           ErrorCode = "RATE_LIMITED"
	DependencyUnavailable ErrorCode = "DEPENDENCY_UNAVAILABLE"
	InternalError         ErrorCode = "INTERNAL_ERROR"
)

// APIError carries a safe message, never stack traces or protected evidence.
// Retryable permits only an idempotent retry; it does not describe a rail outcome.
type APIError struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
}

type ErrorEnvelope struct {
	Error         APIError `json:"error"`
	CorrelationID value.ID `json:"correlationId"`
}

func (e ErrorEnvelope) Validate() error {
	if err := e.CorrelationID.Validate(); err != nil {
		return err
	}
	switch e.Error.Code {
	case InvalidRequest, Unauthenticated, Forbidden, NotFound, Conflict, RateLimited, DependencyUnavailable, InternalError:
	default:
		return errors.New("unknown API error code")
	}
	if len([]rune(e.Error.Message)) < 1 || len([]rune(e.Error.Message)) > 256 {
		return errors.New("API error message must contain 1 to 256 characters")
	}
	return nil
}
