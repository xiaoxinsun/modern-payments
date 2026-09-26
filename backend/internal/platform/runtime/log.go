package runtime

import (
	"context"
	"log/slog"
	"strings"
)

type redactingHandler struct {
	next slog.Handler
}

func NewRedactingHandler(next slog.Handler) slog.Handler {
	return redactingHandler{next: next}
}

func (handler redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.next.Enabled(ctx, level)
}

func (handler redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	copy := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		copy.AddAttrs(redact(attribute))
		return true
	})
	return handler.next.Handle(ctx, copy)
}

func (handler redactingHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attributes))
	for index, attribute := range attributes {
		redacted[index] = redact(attribute)
	}
	return redactingHandler{next: handler.next.WithAttrs(redacted)}
}

func (handler redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{next: handler.next.WithGroup(name)}
}

func redact(attribute slog.Attr) slog.Attr {
	key := strings.ToLower(strings.ReplaceAll(attribute.Key, "-", "_"))
	for _, sensitive := range []string{"authorization", "credential", "password", "secret", "token", "database_url", "dsn"} {
		if strings.Contains(key, sensitive) {
			return slog.String(attribute.Key, "[REDACTED]")
		}
	}
	if attribute.Value.Kind() == slog.KindGroup {
		group := attribute.Value.Group()
		for index := range group {
			group[index] = redact(group[index])
		}
		return slog.Group(attribute.Key, attrsToAny(group)...)
	}
	return attribute
}

func attrsToAny(attributes []slog.Attr) []any {
	values := make([]any, len(attributes))
	for index := range attributes {
		values[index] = attributes[index]
	}
	return values
}
