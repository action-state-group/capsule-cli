package cli

import (
	"os"
	"testing"
)

// TestMain removes the History tests' shared stores (storeTemplate) once
// every test has run.
func TestMain(m *testing.M) {
	code := m.Run()
	for _, s := range []*storeTemplate{&dealsTemplate, &historiesTemplate} {
		if s.dir != "" {
			_ = os.RemoveAll(s.dir)
		}
	}
	os.Exit(code)
}
