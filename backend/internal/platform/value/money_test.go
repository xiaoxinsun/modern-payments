package value

import (
	"encoding/json"
	"testing"
)

func TestMoneyRoundTrip(t *testing.T) {
	for _, currency := range []Currency{SGD, USD} {
		for _, amount := range []int64{0, 1, -1, 9007199254740993, MaxMinorUnits, -MaxMinorUnits} {
			want := Money{AmountMinor: amount, Currency: currency}
			encoded, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var got Money
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("round trip %s: got %+v, want %+v", encoded, got, want)
			}
		}
	}
}

func TestMoneyRejectsInvalidWireValues(t *testing.T) {
	for _, raw := range []string{
		`{"amountMinor":1,"currency":"SGD"}`, `{"amountMinor":"1.01","currency":"SGD"}`,
		`{"amountMinor":"01","currency":"SGD"}`, `{"amountMinor":"-0","currency":"SGD"}`,
		`{"amountMinor":"+1","currency":"SGD"}`, `{"amountMinor":"1e2","currency":"SGD"}`,
		`{"amountMinor":"1000000000000000000","currency":"SGD"}`,
		`{"amountMinor":"1","currency":"JPY"}`, `{"amountMinor":"1","currency":"sgd"}`,
		`{"amountMinor":"1","currency":"SGD","scale":2}`, `{"currency":"SGD"}`,
		`{"amountMinor":"1"}`, `null`,
	} {
		t.Run(raw, func(t *testing.T) {
			original := Money{AmountMinor: 17, Currency: USD}
			got := original
			if err := json.Unmarshal([]byte(raw), &got); err == nil {
				t.Fatal("accepted invalid money")
			}
			if got != original {
				t.Fatal("failed decode mutated destination")
			}
		})
	}
}

func TestMoneyCheckedAddition(t *testing.T) {
	for _, tc := range []struct {
		a, b, want Money
		fail       bool
	}{
		{Money{100, SGD}, Money{-25, SGD}, Money{75, SGD}, false},
		{Money{MaxMinorUnits, SGD}, Money{1, SGD}, Money{}, true},
		{Money{-MaxMinorUnits, USD}, Money{-1, USD}, Money{}, true},
		{Money{1, SGD}, Money{1, USD}, Money{}, true},
		{Money{1, "JPY"}, Money{1, "JPY"}, Money{}, true},
		{Money{MaxMinorUnits + 1, SGD}, Money{-1, SGD}, Money{}, true},
	} {
		got, err := tc.a.Add(tc.b)
		if (err != nil) != tc.fail || (!tc.fail && got != tc.want) {
			t.Fatalf("%+v: got %+v, %v", tc, got, err)
		}
	}
	if _, err := json.Marshal(Money{MaxMinorUnits + 1, SGD}); err == nil {
		t.Fatal("marshaled out-of-range money")
	}
	for _, currency := range []Currency{SGD, USD} {
		if scale, err := currency.Scale(); err != nil || scale != 2 {
			t.Fatalf("invalid scale: %d %v", scale, err)
		}
	}
}
