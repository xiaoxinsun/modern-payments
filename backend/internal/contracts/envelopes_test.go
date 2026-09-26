package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gtb.local/backend/internal/platform/value"
)

const schemaRoot = "https://gtb.local/contracts/"

func readJSON(t *testing.T, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func compiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	for _, path := range []string{"common/v1/types.schema.json", "events/v1/envelope.schema.json", "simulators/v1/message.schema.json"} {
		if err := c.AddResource(schemaRoot+path, readJSON(t, "../../../contracts/"+path)); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func compile(t *testing.T, c *jsonschema.Compiler, path string) *jsonschema.Schema {
	t.Helper()
	schema, err := c.Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestSchemasAndExamples(t *testing.T) {
	c := compiler(t)
	for _, tc := range []struct{ file, schema string }{
		{"money.json", "common/v1/types.schema.json#/$defs/Money"},
		{"error.json", "common/v1/types.schema.json#/$defs/ErrorEnvelope"},
		{"event.json", "events/v1/envelope.schema.json"},
		{"simulator-message.json", "simulators/v1/message.schema.json"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			schema := compile(t, c, schemaRoot+tc.schema)
			instance := readJSON(t, "../../../contracts/examples/"+tc.file)
			if err := schema.Validate(instance); err != nil {
				t.Fatal(err)
			}
			object := instance.(map[string]any)
			// Every documented field is required, and unknown top-level fields fail.
			for key, value := range object {
				delete(object, key)
				if err := schema.Validate(object); err == nil {
					t.Errorf("accepted missing %s", key)
				}
				object[key] = value
			}
			object["unexpected"] = true
			if err := schema.Validate(object); err == nil {
				t.Fatal("accepted unknown field")
			}
		})
	}
}

func TestMoneySchemaMatchesGo(t *testing.T) {
	schema := compile(t, compiler(t), schemaRoot+"common/v1/types.schema.json#/$defs/Money")
	for _, amount := range []any{"0", "1", "-1", "9007199254740993", "999999999999999999", "-999999999999999999", "1000000000000000000", "-1000000000000000000", "01", "-0", "1.1", "1e2", "+1", "", nil, float64(1)} {
		for _, currency := range []string{"SGD", "USD", "JPY", "sgd", ""} {
			object := map[string]any{"amountMinor": amount, "currency": currency}
			data, _ := json.Marshal(object)
			var money value.Money
			goErr := json.Unmarshal(data, &money)
			schemaErr := schema.Validate(object)
			if (goErr == nil) != (schemaErr == nil) {
				t.Fatalf("schema/Go mismatch: %s: %v / %v", data, goErr, schemaErr)
			}
		}
	}
}

func TestTimestampSchema(t *testing.T) {
	schema := compile(t, compiler(t), schemaRoot+"common/v1/types.schema.json#/$defs/Timestamp")
	for _, raw := range []string{"0000-01-01T00:00:00Z", "0001-01-01T00:00:00Z", "0001-01-01T00:00:00.000Z", "2026-02-30T00:00:00Z", "2026-09-26T08:00:00+00:00", "2026-09-26T08:00:00.1234567890Z"} {
		if err := schema.Validate(raw); err == nil {
			t.Errorf("accepted invalid/noncanonical timestamp %s", raw)
		}
	}
	for _, raw := range []string{"2024-02-29T00:00:00Z", "2026-09-26T08:00:00.123456789Z", "0001-01-01T00:00:00.000000001Z"} {
		if err := schema.Validate(raw); err != nil {
			t.Errorf("rejected valid timestamp %s: %v", raw, err)
		}
	}
}

func TestEnvelopeSemanticValidation(t *testing.T) {
	data, err := os.ReadFile("../../../contracts/examples/event.json")
	if err != nil {
		t.Fatal(err)
	}
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	c := compiler(t)
	eventSchema := compile(t, c, schemaRoot+"events/v1/envelope.schema.json")
	for name, mutate := range map[string]func(*Event){
		"schema version":    func(e *Event) { e.SchemaVersion = 2 },
		"aggregate version": func(e *Event) { e.AggregateVersion = 0 },
		"sequence":          func(e *Event) { e.EventSequence = -1 },
		"sequence bounds":   func(e *Event) { e.EventSequence = value.MaxMinorUnits + 1 },
		"time":              func(e *Event) { e.OccurredAt = time.Time{} },
		"offset":            func(e *Event) { e.RecordedAt = e.RecordedAt.In(time.FixedZone("offset", 3600)) },
		"source":            func(e *Event) { e.Source = "" },
		"event type":        func(e *Event) { e.EventType = "bad type" },
		"null payload":      func(e *Event) { e.Payload = json.RawMessage(`null`) },
		"array payload":     func(e *Event) { e.Payload = json.RawMessage(`[]`) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := event
			mutate(&bad)
			if bad.Validate() == nil {
				t.Fatal("accepted invalid envelope")
			}
			encoded, err := json.Marshal(bad)
			if err != nil {
				t.Fatal(err)
			}
			var object any
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatal(err)
			}
			if err := eventSchema.Validate(object); err == nil {
				t.Fatal("schema accepted invalid envelope")
			}
		})
	}
	var apiError ErrorEnvelope
	errorBytes, _ := os.ReadFile("../../../contracts/examples/error.json")
	if err := json.Unmarshal(errorBytes, &apiError); err != nil {
		t.Fatal(err)
	}
	if err := apiError.Validate(); err != nil {
		t.Fatal(err)
	}
	apiError.Error.Code = "UNKNOWN"
	if apiError.Validate() == nil {
		t.Fatal("accepted undefined error code")
	}
	apiError.Error.Code = InvalidRequest
	apiError.Error.Message = strings.Repeat("x", 257)
	if apiError.Validate() == nil {
		t.Fatal("accepted oversized error message")
	}
	// A large JSON integer remains exact through the Go event wire encoding.
	event.AggregateVersion = 9007199254740993
	event.EventSequence = 9007199254740994
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Event
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.AggregateVersion != event.AggregateVersion || roundTrip.EventSequence != event.EventSequence {
		t.Fatal("lost integer precision")
	}
	var object any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if err := eventSchema.Validate(object); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAPIAndReferences(t *testing.T) {
	c := compiler(t)
	meta := readJSON(t, "testdata/openapi-3.1-schema.json").(map[string]any)
	metaID := meta["$id"].(string)
	if err := c.AddResource(metaID, meta); err != nil {
		t.Fatal(err)
	}
	api := readJSON(t, "../../../contracts/openapi/v1/shared.json").(map[string]any)
	if err := compile(t, c, metaID).Validate(api); err != nil {
		t.Fatal(err)
	}
	// The official OpenAPI meta-schema does not validate Schema Objects. Compile
	// every schema and resolve references offline, including shared component refs.
	apiURL := schemaRoot + "openapi/v1/shared.json"
	if err := c.AddResource(apiURL, api); err != nil {
		t.Fatal(err)
	}
	var visit func(any, string)
	visit = func(node any, pointer string) {
		switch node := node.(type) {
		case map[string]any:
			for key, child := range node {
				escaped := strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
				childPointer := pointer + "/" + escaped
				if key == "schema" || pointer == "/components/schemas" {
					compile(t, c, apiURL+"#"+childPointer)
				}
				visit(child, childPointer)
			}
		}
	}
	visit(api, "")
	// Guard against accidentally dropping the offline meta-schema fixture.
	if filepath.Base(metaID) != "2022-10-07" {
		t.Fatal("unexpected OpenAPI meta-schema version")
	}
}
