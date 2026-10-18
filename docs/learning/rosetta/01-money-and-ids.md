# Rosetta 01: money and identifiers

**Payments idea.** Money is an integer count of minor units (cents) plus a
currency. Floats are never used, currencies never mix, and amounts are bounded
so arithmetic cannot overflow. Identifiers are UUIDv4 strings in one canonical
lowercase form, so the same ID always compares equal (idempotency depends on
it).

Files: `backend/internal/platform/value/money.go`, `id.go` and their tests.
This package is the only GTB package the domain layers may import.

## Go

From `backend/internal/platform/value/money.go:14`:

```go
const MaxMinorUnits int64 = 999_999_999_999_999_999

type Currency string

const (
	SGD Currency = "SGD"
	USD Currency = "USD"
)
```

From `backend/internal/platform/value/money.go:34`:

```go
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
```

From `backend/internal/platform/value/money.go:73`:

```go
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
```

From `backend/internal/platform/value/id.go:12`:

```go
type ID string

func NewID() (ID, error) {
	generated, err := uuid.NewV4()
	if err != nil {
		return "", err
	}
	return ID(generated.String()), nil
}

func (id ID) Validate() error {
	parsed, err := uuid.FromString(string(id))
	if err != nil || parsed.String() != string(id) || parsed.Version() != 4 || parsed.Variant() != uuid.VariantRFC9562 {
		return errors.New("identifier must be a canonical lowercase UUIDv4")
	}
	return nil
}
```

## Java 21

```java
enum Currency {
    SGD, USD;
    int scale() { return 2; }
}

record Money(long amountMinor, Currency currency) {
    static final long MAX_MINOR_UNITS = 999_999_999_999_999_999L;
    private static final Pattern MINOR = Pattern.compile("^(0|-?[1-9][0-9]{0,17})$");

    Money {                                         // compact constructor: invariant always holds
        Objects.requireNonNull(currency);
        if (Math.abs(amountMinor) > MAX_MINOR_UNITS)
            throw new IllegalArgumentException("money exceeds supported minor-unit bounds");
    }

    // Wire form {"amountMinor":"2500","currency":"SGD"}: a string amount, canonical syntax.
    static Money fromWire(String amountMinor, String currency) {
        if (!MINOR.matcher(amountMinor).matches())
            throw new IllegalArgumentException("amountMinor must be a canonical signed decimal string");
        return new Money(Long.parseLong(amountMinor), Currency.valueOf(currency));
    }
}

record Id(String value) {
    Id {
        UUID parsed = UUID.fromString(value);       // throws on garbage
        if (!parsed.toString().equals(value) || parsed.version() != 4)
            throw new IllegalArgumentException("identifier must be a canonical lowercase UUIDv4");
    }
    static Id newId() { return new Id(UUID.randomUUID().toString()); }
}
```

## Why the Go looks like this

- **No constructors, so no enforced invariants.** A Java record's compact
  constructor guarantees every `Money` is valid. In Go anyone can write
  `value.Money{AmountMinor: -5, Currency: "JPY"}`, because struct literals
  bypass any function. GTB therefore calls `Validate()` wherever data *enters*:
  JSON decoding, domain validation and database reads. Watch for this pattern
  everywhere; it replaces constructor-enforced invariants.
- **Named types instead of wrapper classes.** `type ID string` costs nothing at
  runtime (it *is* a string) but is a distinct type to the compiler. The Java
  equivalent needs a record wrapper, with an allocation per ID.
- **Value receivers (`func (m Money)`) make Money behave like an immutable
  record.** `UnmarshalJSON` is the exception: it takes a pointer (`*Money`)
  because it must fill in the caller's variable. It assigns `*m = result` only
  after everything validates, so a failed decode leaves the destination
  untouched.
- **`regexp.MustCompile` at package level.** `Must…` functions panic instead of
  returning an error, which is acceptable only for constants known at compile
  time. It is the equivalent of a `static final Pattern`.
- **Errors, not exceptions.** `NewID` returns `(ID, error)` because the OS
  random source could in theory fail. Java's `UUID.randomUUID()` hides that
  inside an unchecked exception.
- **`999_999_999_999_999_999`.** Underscores in numeric literals work the same
  as in Java.

## Try it

```sh
cd backend && go test -v -run 'Money|ID' ./internal/platform/value/
```
