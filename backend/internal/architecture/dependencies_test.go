package architecture_test

import (
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// Inspect transitive dependencies, so an indirect transport import cannot bypass
// the domain boundary. New domain subpackages are covered automatically.
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
		for _, dep := range pkg.Deps {
			if dep == "net" || strings.HasPrefix(dep, "net/") || strings.HasPrefix(dep, "database/") || strings.HasPrefix(dep, "go.temporal.io/") {
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
