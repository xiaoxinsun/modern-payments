package value

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// ID is a canonical lowercase UUIDv4. It conveys no authorization or ordering.
// External references and OIDC subjects are separate opaque boundary values.
type ID string

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func NewID() (ID, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = data[6]&0x0f | 0x40
	data[8] = data[8]&0x3f | 0x80
	return ID(fmt.Sprintf("%x-%x-%x-%x-%x", data[:4], data[4:6], data[6:8], data[8:10], data[10:])), nil
}

func (id ID) Validate() error {
	if !idPattern.MatchString(string(id)) {
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
