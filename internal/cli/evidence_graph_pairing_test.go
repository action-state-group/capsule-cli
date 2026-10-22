package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The page's Content-Security-Policy lists the runtime's stylesheets by the
// hashes capsule-viewer's Go module carries (go/emitter/runtime-style-hashes.txt),
// and capsule-viewer's own tests hold that list to its TypeScript stylesheets at
// every commit. So the vendored runtime and the Go module must come from the same
// capsule-viewer commit; built from another, a stylesheet can be blocked on every
// page. (The runtime builds its stylesheets from parts, so their hashes cannot be
// recomputed from the vendored file itself.)
func TestRuntimeAndShellComeFromOneViewerCommit(t *testing.T) {
	m := regexp.MustCompile(`capsule-viewer main@([0-9a-f]{40})`).FindStringSubmatch(evidenceGraphIIFESource)
	require.NotNil(t, m, "evidenceGraphIIFESource names a capsule-viewer commit")
	runtimeCommit := m[1]

	info, ok := debug.ReadBuildInfo()
	require.True(t, ok)
	version := ""
	for _, dep := range info.Deps {
		if dep.Path == "github.com/action-state-group/capsule-viewer/go" {
			version = dep.Version
			if dep.Replace != nil {
				t.Skipf("capsule-viewer/go is replaced by %s: pair the runtime by hand", dep.Replace.Path)
			}
		}
	}
	require.NotEmpty(t, version, "the build depends on capsule-viewer/go")

	moduleCommit := ""
	if p := regexp.MustCompile(`-[0-9]{14}-([0-9a-f]{12})$`).FindStringSubmatch(version); p != nil {
		moduleCommit = p[1] // a pseudo-version names its commit
	} else {
		// A tag: the module cache records the commit it resolved to.
		out, err := exec.Command("go", "env", "GOMODCACHE").Output()
		require.NoError(t, err)
		raw, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "cache", "download",
			"github.com", "action-state-group", "capsule-viewer", "go", "@v", version+".info"))
		require.NoError(t, err, "capsule-viewer/go %s: the module cache records no commit for it; pin a pseudo-version, which names its commit", version)
		var meta struct{ Origin struct{ Hash string } }
		require.NoError(t, json.Unmarshal(raw, &meta))
		require.NotEmpty(t, meta.Origin.Hash, "capsule-viewer/go %s: the module cache records no commit for it; pin a pseudo-version, which names its commit", version)
		moduleCommit = meta.Origin.Hash
	}
	require.True(t, strings.HasPrefix(runtimeCommit, moduleCommit),
		"the vendored runtime is from capsule-viewer %s but go.mod's capsule-viewer/go %s is %s: move them together",
		runtimeCommit, version, moduleCommit)
}

// deal report and presentation build resolve capsule-viewer through the
// plugins module, so it must pin the same version the test above checks.
func TestPluginsPinTheSameViewerModule(t *testing.T) {
	line := regexp.MustCompile(`(?m)^\s*github\.com/action-state-group/capsule-viewer/go (\S+)`)
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		m := line.FindSubmatch(raw)
		require.NotNil(t, m, "%s requires capsule-viewer/go", path)
		return string(m[1])
	}
	require.Equal(t, read(filepath.Join("..", "..", "go.mod")), read(filepath.Join("..", "..", "plugins", "go.mod")))
}
