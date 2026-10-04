//go:build !actionstate

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunIsUnavailableInTheDefaultBuild(t *testing.T) {
	pluginRoot(t)
	_, err := invoke(t, "", "run", "--dry-run")
	require.Error(t, err)
	assert.Equal(t, 2, ExitCode(err))
	assert.Equal(t, runUnavailable, SafeError(err))
}

func TestRunStaysReservedAndHiddenInTheDefaultBuild(t *testing.T) {
	// A launcher literally named `run` must not take the name, even though the
	// default build ships no dispatch behind it.
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-run", strings.Replace(fakePlugin, `"name":"guard"`, `"name":"run"`, 1), 0o755)
	c := NewCommand()
	count := 0
	for _, cmd := range c.Commands() {
		if cmd.Name() == "run" {
			count++
			assert.True(t, cmd.Hidden, "run is not offered in the default build's help")
			assert.Equal(t, runUnavailable, cmd.Short)
		}
	}
	assert.Equal(t, 1, count)

	help, err := invoke(t, "", "--help")
	require.NoError(t, err)
	assert.NotRegexp(t, `(?m)^\s+run\s`, help)
}

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

func TestDefaultBinaryCarriesNoCompanyOrLicenceWords(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	out := filepath.Join(t.TempDir(), "capsulectl")
	build := exec.Command(goTool, "build", "-trimpath", "-o", out, "../../cmd/capsulectl")
	build.Env = append(os.Environ(), "GOFLAGS=")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("default build failed: %v\n%s", err, output)
	}
	binary, err := os.ReadFile(out)
	require.NoError(t, err)

	allowed := bytes.Index(binary, []byte(dependencyRegistryLine))
	for _, match := range forbiddenWords.FindAllIndex(binary, -1) {
		inside := allowed >= 0 && match[0] >= allowed && match[1] <= allowed+len(dependencyRegistryLine)
		if !inside {
			start, end := max(0, match[0]-60), min(len(binary), match[1]+60)
			t.Errorf("default build carries %q in %q", binary[match[0]:match[1]], binary[start:end])
		}
	}
}
