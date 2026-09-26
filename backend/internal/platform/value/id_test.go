package value

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIDGenerationAndEncoding(t *testing.T) {
	seen := map[ID]bool{}
	for range 100 {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if err := id.Validate(); err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatal("duplicate generated ID")
		}
		seen[id] = true
		data, err := json.Marshal(id)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ID
		if err := json.Unmarshal(data, &decoded); err != nil || decoded != id {
			t.Fatalf("round trip: %s %v", data, err)
		}
	}
}

func TestIDRejectsNoncanonicalValues(t *testing.T) {
	valid := "d6282a46-c2ce-4c47-9308-f0c7bf9aa031"
	for _, raw := range []string{"", "00000000-0000-0000-0000-000000000000", strings.ToUpper(valid), " " + valid, strings.Replace(valid, "4c47", "7c47", 1), strings.Replace(valid, "9308", "7308", 1)} {
		if err := ID(raw).Validate(); err == nil {
			t.Fatalf("accepted %q", raw)
		}
		data, _ := json.Marshal(raw)
		original := ID(valid)
		if err := json.Unmarshal(data, &original); err == nil || original != ID(valid) {
			t.Fatalf("invalid decode changed ID: %q", raw)
		}
	}
}
