package plugin

import (
	"os"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
)

// TestMain runs the tests as the plugin binaries run: the book backend and
// the deal hooks installed in the core tree. It removes the History tests'
// shared stores (storeTemplate) once every test has run. It gives the
// package a private HOME: doctor heals the per-user plugin root's ancestry,
// and a test that sets no HOME of its own must never chmod the real one.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "capsulectl-home-")
	if err == nil {
		err = os.Chmod(home, 0o755)
	}
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	defer os.RemoveAll(home)
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
