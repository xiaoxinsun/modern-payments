// Command openapibundle writes the self-contained generation bundle of an
// OpenAPI contract. See contracts.BundleOpenAPI.
package main

import (
	"fmt"
	"os"

	"gtb.local/backend/internal/contracts"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: openapibundle <contract.json> <bundle.json>")
		os.Exit(2)
	}
	bundle, err := contracts.BundleOpenAPI(os.Args[1])
	if err == nil {
		err = os.WriteFile(os.Args[2], bundle, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
