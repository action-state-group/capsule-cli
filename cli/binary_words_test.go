package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// forbiddenWords are the company and licence phrases the default build must
// not carry: "Action State plugin", "actionstate plugin", "licence", "license",
// and "Action State" itself, all matched case-insensitively. The generic
// plugin subsystem (`plugin ls`, CAPSULECTL_PLUGIN_ROOTS, cli-plugin/v1) is not
// a company plugin and is deliberately not matched.
var forbiddenWords = regexp.MustCompile(`(?i)action state plugin|actionstate plugin|licence|license|action state`)

// dependencyRegistryLine is the one allowed "Action State" match, because this
// repository does not own it: the AAC Go library embeds the registry document,
// and its preamble names the registry's interim change controller. Any other
// match fails the test.
const dependencyRegistryLine = "Change controller: **Action State Group, Inc.** (interim)"

// vendoredNotice is the third-party notice block the page runtime carries
// (capsule-viewer 0.4.0 bundles @noble/curves for CLL's browser checkpoint
// verifier, and esbuild keeps the library's MIT notice as a "Bundled license
// information" comment). It is the dependency's own text, which its licence
// requires to travel with the code, not ours: matches inside it are allowed.
var vendoredNotice = regexp.MustCompile(`(?s)/\*! Bundled license information:.*?\*/`)

func TestDefaultBinaryCarriesNoCompanyOrLicenceWords(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	out := filepath.Join(t.TempDir(), "capsulectl")
	build := exec.Command(goTool, "build", "-trimpath", "-o", out, "../cmd/capsulectl")
	build.Env = append(os.Environ(), "GOFLAGS=")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("default build failed: %v\n%s", err, output)
	}
	binary, err := os.ReadFile(out)
	require.NoError(t, err)

	allowed := bytes.Index(binary, []byte(dependencyRegistryLine))
	notices := vendoredNotice.FindAllIndex(binary, -1)
	for _, match := range forbiddenWords.FindAllIndex(binary, -1) {
		inside := allowed >= 0 && match[0] >= allowed && match[1] <= allowed+len(dependencyRegistryLine)
		for _, n := range notices {
			inside = inside || match[0] >= n[0] && match[1] <= n[1]
		}
		if !inside {
			start, end := max(0, match[0]-60), min(len(binary), match[1]+60)
			t.Errorf("default build carries %q in %q", binary[match[0]:match[1]], binary[start:end])
		}
	}
}
