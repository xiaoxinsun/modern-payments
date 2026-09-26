// Package domain defines Core Banking accounting rules without persistence or
// transport dependencies.
package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gtb.local/backend/internal/platform/value"
)

type AccountKind string

const (
	CustomerDeposit  AccountKind = "CUSTOMER_DEPOSIT"
	OutgoingClearing AccountKind = "OUTGOING_CLEARING"
	SettlementNostro AccountKind = "SETTLEMENT_NOSTRO"
	IncomingSuspense AccountKind = "INCOMING_SUSPENSE"
)

type Side string

const (
	Debit  Side = "DEBIT"
	Credit Side = "CREDIT"
)

type HoldStatus string

const (
	HoldActive   HoldStatus = "ACTIVE"
	HoldConsumed HoldStatus = "CONSUMED"
	HoldReleased HoldStatus = "RELEASED"
)

type Account struct {
	AccountID         value.ID       `json:"accountId"`
	ExternalReference string         `json:"externalReference"`
	CustomerID        *value.ID      `json:"customerId,omitempty"`
	Kind              AccountKind    `json:"accountKind"`
	Currency          value.Currency `json:"currency"`
	BookedMinor       int64          `json:"bookedMinor,string"`
	AvailableMinor    int64          `json:"availableMinor,string"`
	Status            string         `json:"status"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
}

type Hold struct {
	HoldID                value.ID    `json:"holdId"`
	AccountID             value.ID    `json:"accountId"`
	ReserveOperationID    value.ID    `json:"reserveOperationId"`
	Amount                value.Money `json:"amount"`
	Status                HoldStatus  `json:"status"`
	ExpiresAt             *time.Time  `json:"expiresAt,omitempty"`
	ConsumedByOperationID *value.ID   `json:"consumedByOperationId,omitempty"`
	ReleasedByOperationID *value.ID   `json:"releasedByOperationId,omitempty"`
	CreatedAt             time.Time   `json:"createdAt"`
	UpdatedAt             time.Time   `json:"updatedAt"`
}

type Entry struct {
	AccountID value.ID    `json:"accountId"`
	Side      Side        `json:"side"`
	Amount    value.Money `json:"amount"`
}

type Posting struct {
	PostingID           value.ID  `json:"postingId"`
	OperationID         value.ID  `json:"operationId"`
	Description         string    `json:"description"`
	ReversalOfPostingID *value.ID `json:"reversalOfPostingId,omitempty"`
	Entries             []Entry   `json:"entries"`
	EventID             value.ID  `json:"eventId"`
	CommittedAt         time.Time `json:"committedAt"`
}

type Operation struct {
	OperationID   value.ID  `json:"operationId"`
	OperationType string    `json:"operationType"`
	ResourceType  string    `json:"resourceType"`
	ResourceID    value.ID  `json:"resourceId"`
	CorrelationID value.ID  `json:"correlationId"`
	CommittedAt   time.Time `json:"committedAt"`
}

type StatementEntry struct {
	PostingID           value.ID    `json:"postingId"`
	OperationID         value.ID    `json:"operationId"`
	Description         string      `json:"description"`
	ReversalOfPostingID *value.ID   `json:"reversalOfPostingId,omitempty"`
	CommittedAt         time.Time   `json:"committedAt"`
	Side                Side        `json:"side"`
	Amount              value.Money `json:"amount"`
}

var (
	ErrUnbalanced = errors.New("posting must balance debits and credits within each currency")
	ErrBounds     = errors.New("posting totals exceed supported minor-unit bounds")
)

func ValidateEntries(entries []Entry) error {
	if len(entries) < 2 || len(entries) > 64 {
		return errors.New("posting must contain 2 to 64 entries")
	}
	type totals struct{ debit, credit int64 }
	byCurrency := make(map[value.Currency]totals)
	for _, entry := range entries {
		if err := entry.AccountID.Validate(); err != nil {
			return fmt.Errorf("account ID: %w", err)
		}
		if entry.Side != Debit && entry.Side != Credit {
			return fmt.Errorf("unsupported posting side %q", entry.Side)
		}
		if err := entry.Amount.Validate(); err != nil || entry.Amount.AmountMinor <= 0 {
			return errors.New("entry amount must be positive and valid")
		}
		current := byCurrency[entry.Amount.Currency]
		if entry.Side == Debit {
			if current.debit > value.MaxMinorUnits-entry.Amount.AmountMinor {
				return ErrBounds
			}
			current.debit += entry.Amount.AmountMinor
		} else {
			if current.credit > value.MaxMinorUnits-entry.Amount.AmountMinor {
				return ErrBounds
			}
			current.credit += entry.Amount.AmountMinor
		}
		byCurrency[entry.Amount.Currency] = current
	}
	for _, current := range byCurrency {
		if current.debit != current.credit {
			return ErrUnbalanced
		}
	}
	return nil
}

func BalanceAfter(kind AccountKind, booked int64, side Side, amount int64) (int64, error) {
	if booked < 0 || booked > value.MaxMinorUnits || amount <= 0 || amount > value.MaxMinorUnits {
		return 0, ErrBounds
	}
	creditNormal := kind == CustomerDeposit || kind == OutgoingClearing || kind == IncomingSuspense
	increase := (creditNormal && side == Credit) || (!creditNormal && kind == SettlementNostro && side == Debit)
	if !creditNormal && kind != SettlementNostro {
		return 0, fmt.Errorf("unsupported account kind %q", kind)
	}
	if increase {
		if booked > value.MaxMinorUnits-amount {
			return 0, ErrBounds
		}
		return booked + amount, nil
	}
	if booked < amount {
		return 0, errors.New("posting would make account balance negative")
	}
	return booked - amount, nil
}

// SortedAccountIDs returns unique account IDs in a stable order, preventing
// deadlocks when concurrent postings lock overlapping account sets.
func SortedAccountIDs(entries []Entry) []value.ID {
	set := make(map[value.ID]struct{}, len(entries))
	for _, entry := range entries {
		set[entry.AccountID] = struct{}{}
	}
	ids := make([]value.ID, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return strings.Compare(string(ids[i]), string(ids[j])) < 0 })
	return ids
}
