package deal

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"testing"
)

// TestMain runs the tests as capsulectl-deal runs: the deal hooks installed
// in the core tree. It removes the History tests'
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
	// No test reaches a real resolver: the DoH tests name their own
	// endpoints, and the probe finds no proxy unless a test says so.
	os.Setenv(dohEnv, "off")
	systemProbe = func(string) ([]net.IP, error) { return nil, nil }
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

// keepGoCaches pins the go env file and the Go build and module caches, as
// go resolves them under the real HOME, before a test HOME replaces it: the
// tests that run `go build` would otherwise fill the test HOME with a fresh,
// read-only module cache and lose the user's `go env -w` settings.
func keepGoCaches() {
	out, err := exec.Command("go", "env", "-json", "GOENV", "GOPATH", "GOCACHE", "GOMODCACHE").Output()
	var env map[string]string
	if err != nil || json.Unmarshal(out, &env) != nil {
		return // no go to run: nothing under test builds with it
	}
	for name, value := range env {
		if value != "" && os.Getenv(name) == "" {
			os.Setenv(name, value)
		}
	}
}
