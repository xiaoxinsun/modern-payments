package domain

import (
	"errors"
	"fmt"
	"testing"

	"gtb.local/backend/internal/platform/value"
)

func testID(last byte) value.ID {
	return value.ID(fmt.Sprintf("00000000-0000-4000-8000-%012d", last))
}

func TestAccountingScenariosBalance(t *testing.T) {
	maya := testID(1)
	daniel := testID(2)
	clearing := testID(3)
	settlement := testID(4)
	suspense := testID(5)
	money := func(amount int64) value.Money { return value.Money{AmountMinor: amount, Currency: value.SGD} }

	scenarios := map[string][]Entry{
		"A2 outbound booking":    {{maya, Debit, money(10000)}, {clearing, Credit, money(10000)}},
		"A4 settlement":          {{clearing, Debit, money(10000)}, {settlement, Credit, money(10000)}},
		"B compensation":         {{clearing, Debit, money(10000)}, {maya, Credit, money(10000)}},
		"C returned funds":       {{settlement, Debit, money(10000)}, {maya, Credit, money(10000)}},
		"D book transfer":        {{maya, Debit, money(10000)}, {daniel, Credit, money(10000)}},
		"E1 cleared inbound":     {{settlement, Debit, money(10000)}, {daniel, Credit, money(10000)}},
		"E2a suspense inbound":   {{settlement, Debit, money(10000)}, {suspense, Credit, money(10000)}},
		"E2b suspense clearance": {{suspense, Debit, money(10000)}, {daniel, Credit, money(10000)}},
	}
	for name, entries := range scenarios {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEntries(entries); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateEntriesRejectsUnbalancedAndBounds(t *testing.T) {
	id1, id2 := testID(1), testID(2)
	if err := ValidateEntries([]Entry{
		{id1, Debit, value.Money{AmountMinor: 100, Currency: value.SGD}},
		{id2, Credit, value.Money{AmountMinor: 99, Currency: value.SGD}},
	}); !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("got %v", err)
	}
	if _, err := BalanceAfter(CustomerDeposit, 50, Debit, 51); err == nil {
		t.Fatal("expected negative customer balance rejection")
	}
}

func TestBalanceDirection(t *testing.T) {
	tests := []struct {
		kind AccountKind
		side Side
		want int64
	}{
		{CustomerDeposit, Credit, 110},
		{CustomerDeposit, Debit, 90},
		{OutgoingClearing, Credit, 110},
		{IncomingSuspense, Debit, 90},
		{SettlementNostro, Debit, 110},
		{SettlementNostro, Credit, 90},
	}
	for _, test := range tests {
		got, err := BalanceAfter(test.kind, 100, test.side, 10)
		if err != nil || got != test.want {
			t.Fatalf("%s %s: got %d, %v", test.kind, test.side, got, err)
		}
	}
}
