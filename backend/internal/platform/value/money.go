// Package value contains transport-independent money and identity primitives.
package value

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// MaxMinorUnits bounds all monetary values to 18 decimal digits, within int64.
const MaxMinorUnits int64 = 999_999_999_999_999_999

type Currency string

const (
	SGD Currency = "SGD"
	USD Currency = "USD"
)

func (c Currency) Scale() (int, error) {
	switch c {
	case SGD, USD:
		return 2, nil
	default:
		return 0, fmt.Errorf("unsupported currency %q", c)
	}
}

// Money is signed minor units. Payment instructions must additionally be positive.
// Exported fields require Validate at application boundaries; JSON enforces it.
type Money struct {
	AmountMinor int64    `json:"amountMinor,string"`
	Currency    Currency `json:"currency"`
}

func (m Money) Validate() error {
	if _, err := m.Currency.Scale(); err != nil {
		return err
	}
	if m.AmountMinor < -MaxMinorUnits || m.AmountMinor > MaxMinorUnits {
		return errors.New("money exceeds supported minor-unit bounds")
	}
	return nil
}

// Add rejects mixed currencies and bounds violations without changing either input.
func (m Money) Add(other Money) (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	if err := other.Validate(); err != nil {
		return Money{}, err
	}
	if m.Currency != other.Currency {
		return Money{}, errors.New("currency mismatch")
	}
	// Both inputs are bounded to 18 digits; their sum cannot overflow int64.
	result := Money{AmountMinor: m.AmountMinor + other.AmountMinor, Currency: m.Currency}
	return result, result.Validate()
}

func (m Money) MarshalJSON() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	type wire Money
	return json.Marshal(wire(m))
}

var minorPattern = regexp.MustCompile(`^(0|-?[1-9][0-9]{0,17})$`)

func (m *Money) UnmarshalJSON(data []byte) error {
	var wire struct {
		AmountMinor string   `json:"amountMinor"`
		Currency    Currency `json:"currency"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if !minorPattern.MatchString(wire.AmountMinor) {
		return errors.New("amountMinor must be a canonical signed decimal string of at most 18 digits")
	}
	amount, err := strconv.ParseInt(wire.AmountMinor, 10, 64)
	if err != nil {
		return err
	}
	result := Money{AmountMinor: amount, Currency: wire.Currency}
	if err := result.Validate(); err != nil {
		return err
	}
	*m = result
	return nil
}
