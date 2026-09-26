package contracts

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"
)

// BundleOpenAPI validates a contract and resolves its cross-file references into
// one self-contained document for code generation and runtime request
// validation. The source under contracts/ remains authoritative.
func BundleOpenAPI(source string) ([]byte, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	document, err := loader.LoadFromFile(source)
	if err != nil {
		return nil, err
	}
	if err := document.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("validate %s: %w", source, err)
	}
	document.InternalizeRefs(context.Background(), nil)
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
