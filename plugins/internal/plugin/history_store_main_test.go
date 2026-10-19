package plugin

import (
	"os"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
)

// TestMain runs the tests as the plugin binaries run: the book backend and
// the deal hooks installed in the core tree. It removes the History tests'
// shared stores (storeTemplate) once every test has run.
func TestMain(m *testing.M) {
	cli.SetBookBackend(BookBackend{})
	InstallDealHooks()
	code := m.Run()
	removeInstalled()
	for _, s := range []*storeTemplate{&dealsTemplate, &historiesTemplate} {
		if s.dir != "" {
			_ = os.RemoveAll(s.dir)
		}
	}
	os.Exit(code)
}
