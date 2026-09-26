package architecture_test

import (
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// Inspect direct standard-library imports and transitive project dependencies.
// Maintained value libraries may themselves implement database/sql or net
// interfaces, so those standard packages are forbidden at the domain source
// boundary rather than anywhere in a third-party dependency graph.
func TestDomainDependencies(t *testing.T) {
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = "../.."
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	checked := 0
	for {
		var pkg struct {
			ImportPath string
			Imports    []string
			Deps       []string
		}
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		isDomain := strings.HasSuffix(pkg.ImportPath, "/domain") || strings.Contains(pkg.ImportPath, "/domain/")
		if !isDomain && pkg.ImportPath != "gtb.local/backend/internal/platform/value" {
			continue
		}
		checked++
		for _, imported := range pkg.Imports {
			if imported == "net" || strings.HasPrefix(imported, "net/") || strings.HasPrefix(imported, "database/") {
				t.Errorf("%s directly imports infrastructure %s", pkg.ImportPath, imported)
			}
		}
		for _, dep := range pkg.Deps {
			if strings.HasPrefix(dep, "go.temporal.io/") {
				t.Errorf("%s depends on infrastructure %s", pkg.ImportPath, dep)
			}
			if strings.HasPrefix(dep, "gtb.local/backend/") && dep != "gtb.local/backend/internal/platform/value" && !strings.HasPrefix(dep, pkg.ImportPath+"/") {
				t.Errorf("%s depends on outer/other owner package %s", pkg.ImportPath, dep)
			}
		}
	}
	if checked < 2 {
		t.Fatal("expected payment domain and shared value packages")
	}
}
