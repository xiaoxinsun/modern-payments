package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"gtb.local/backend/internal/platform/value"
)

var ErrInvalidInstruction = errors.New("invalid payment instruction")

type Product string

const (
	BookTransfer    Product = "BOOK_TRANSFER"
	DomesticInstant Product = "DOMESTIC_INSTANT"
	RTGS            Product = "RTGS"
	CrossBorder     Product = "CROSS_BORDER"
)

func (product Product) Valid() bool {
	switch product {
	case BookTransfer, DomesticInstant, RTGS, CrossBorder:
		return true
	}
	return false
}

// Creditor identifies the beneficiary. A book transfer names a GTB account;
// external products name an account at another institution.
type Creditor struct {
	Name           string    `json:"name"`
	AccountID      *value.ID `json:"accountId,omitempty"`
	AccountNumber  string    `json:"accountNumber,omitempty"`
	BankIdentifier string    `json:"bankIdentifier,omitempty"`
	Country        string    `json:"country,omitempty"`
}

// Instruction is the immutable customer instruction. Its field order defines the
// canonical payload hash, so reordering fields changes duplicate detection.
type Instruction struct {
	CustomerID      value.ID    `json:"customerId"`
	Product         Product     `json:"product"`
	DebtorAccountID value.ID    `json:"debtorAccountId"`
	Amount          value.Money `json:"amount"`
	Creditor        Creditor    `json:"creditor"`
	Reference       string      `json:"reference,omitempty"`
}

var (
	accountNumberPattern = regexp.MustCompile(`^[0-9A-Za-z-]{1,34}$`)
	domesticBankPattern  = regexp.MustCompile(`^[A-Z0-9]{4,11}$`)
	bicPattern           = regexp.MustCompile(`^[A-Z]{6}[A-Z0-9]{2}([A-Z0-9]{3})?$`)
	countryPattern       = regexp.MustCompile(`^[A-Z]{2}$`)
)

// Validate enforces structural product rules only. Account ownership, controls,
// limits and funds belong to authorization, screening and Core Banking.
func (instruction Instruction) Validate() error {
	if instruction.CustomerID.Validate() != nil || instruction.DebtorAccountID.Validate() != nil {
		return invalid("customerId and debtorAccountId must be canonical UUIDv4 values")
	}
	if !instruction.Product.Valid() {
		return invalid("unsupported product")
	}
	if err := instruction.Amount.Validate(); err != nil || instruction.Amount.AmountMinor <= 0 {
		return invalid("amount must be positive in a supported currency")
	}
	if err := boundedText("creditor name", instruction.Creditor.Name, 1, 140); err != nil {
		return err
	}
	if err := boundedText("reference", instruction.Reference, 0, 140); err != nil {
		return err
	}
	creditor := instruction.Creditor
	if instruction.Product == BookTransfer {
		if creditor.AccountID == nil || creditor.AccountID.Validate() != nil {
			return invalid("book transfer requires a GTB creditor accountId")
		}
		if *creditor.AccountID == instruction.DebtorAccountID {
			return invalid("creditor account must differ from debtor account")
		}
		if creditor.AccountNumber != "" || creditor.BankIdentifier != "" || creditor.Country != "" {
			return invalid("book transfer creditor must not name an external institution")
		}
		return nil
	}
	if creditor.AccountID != nil {
		return invalid("external products name the creditor by account number and bank")
	}
	if !accountNumberPattern.MatchString(creditor.AccountNumber) {
		return invalid("creditor accountNumber must be 1 to 34 letters, digits or hyphens")
	}
	switch instruction.Product {
	case DomesticInstant, RTGS:
		if instruction.Amount.Currency != value.SGD {
			return invalid("domestic instant and RTGS payments must be in SGD")
		}
		if !domesticBankPattern.MatchString(creditor.BankIdentifier) || creditor.Country != "SG" {
			return invalid("domestic creditor requires an uppercase bank identifier and country SG")
		}
	case CrossBorder:
		if !bicPattern.MatchString(creditor.BankIdentifier) {
			return invalid("cross-border creditor bankIdentifier must be a BIC")
		}
		if !countryPattern.MatchString(creditor.Country) || creditor.Country == "SG" {
			return invalid("cross-border creditor country must be a non-SG ISO 3166 alpha-2 code")
		}
	}
	return nil
}

// Hash is the SHA-256 of the canonical instruction JSON. Equal hashes under one
// idempotency scope replay the original receipt; different hashes conflict.
func (instruction Instruction) Hash() (string, error) {
	if err := instruction.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(instruction)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

type Channel string

const CustomerAPI Channel = "CUSTOMER_API"

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// IdempotencyScope is the durable duplicate identity of a customer API command:
// channel, authenticated customer and caller key. It has no expiry.
type IdempotencyScope struct {
	Channel    Channel
	CustomerID value.ID
	Key        string
}

func (scope IdempotencyScope) Validate() error {
	if scope.Channel != CustomerAPI {
		return invalid("unsupported idempotency channel")
	}
	if err := scope.CustomerID.Validate(); err != nil {
		return invalid("idempotency customer must be a canonical UUIDv4")
	}
	if !idempotencyKeyPattern.MatchString(scope.Key) {
		return invalid("Idempotency-Key must be 1 to 128 letters, digits or . _ : -")
	}
	return nil
}

func boundedText(name, text string, minimum, maximum int) error {
	length := len([]rune(text))
	if length < minimum || length > maximum || strings.TrimSpace(text) != text {
		return invalid("%s must be %d to %d characters without surrounding spaces", name, minimum, maximum)
	}
	for _, character := range text {
		if unicode.IsControl(character) {
			return invalid("%s must not contain control characters", name)
		}
	}
	return nil
}

func invalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInstruction, fmt.Sprintf(format, arguments...))
}
