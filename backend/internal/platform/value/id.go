package value

import (
	"encoding/json"
	"errors"

	"github.com/gofrs/uuid/v5"
)

// ID is a canonical lowercase UUIDv4. It conveys no authorization or ordering.
// External references and OIDC subjects are separate opaque boundary values.
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

func (id ID) MarshalJSON() ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(string(id))
}

func (id *ID) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	parsed := ID(raw)
	if err := parsed.Validate(); err != nil {
		return err
	}
	*id = parsed
	return nil
}
