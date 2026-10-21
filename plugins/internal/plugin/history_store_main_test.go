package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
)

// TestMain runs the tests as the plugin binaries run: the book backend and
// the deal hooks installed in the core tree. It removes the History tests'
// shared stores (storeTemplate) once every test has run. It gives the
// package a private HOME: doctor heals the per-user plugin root's ancestry,
// and a test that sets no HOME of its own must never chmod the real one.
func TestMain(m *testing.M) {
	keepGoCaches()
	home, err := os.MkdirTemp("", "capsulectl-home-")
	if err == nil {
		err = os.Chmod(home, 0o755)
	}
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	cli.SetBookBackend(BookBackend{})
	InstallDealHooks()
	code := m.Run()
	removeInstalled()
	_ = os.RemoveAll(home)
	for _, s := range []*storeTemplate{&dealsTemplate, &historiesTemplate} {
		if s.dir != "" {
			_ = os.RemoveAll(s.dir)
		}
	}
	os.Exit(code)
}

// keepGoCaches pins the Go build and module caches to where they are under
// the real HOME before a test HOME replaces it: the tests that run `go build`
// would otherwise fill the test HOME with a fresh, read-only module cache.
func keepGoCaches() {
	if os.Getenv("GOPATH") == "" {
		if real, err := os.UserHomeDir(); err == nil {
			os.Setenv("GOPATH", filepath.Join(real, "go"))
		}
	}
	if os.Getenv("GOCACHE") == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			os.Setenv("GOCACHE", filepath.Join(cache, "go-build"))
		}
	}
}
