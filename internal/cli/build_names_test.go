package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goBuildOutput finds the -o target of a `go build` command, once its
// backslash-newline continuations are joined (buildOutputs).
var goBuildOutput = regexp.MustCompile(`go build\b[^\n]*?\s-o\s+("[^"]+"|\S+)`)

// lineContinuation is a shell backslash-newline, with the next line's indent.
var lineContinuation = regexp.MustCompile(`\\\r?\n[ \t]*`)

// buildOutputs returns the base name of every `go build -o` target in a shell
// script, Makefile or workflow body, reading a command split over several lines
// with backslash continuations as the one command it is.
func buildOutputs(body string) []string {
	joined := lineContinuation.ReplaceAllString(body, " ")
	var outs []string
	for _, m := range goBuildOutput.FindAllStringSubmatch(joined, -1) {
		outs = append(outs, filepath.Base(strings.Trim(m[1], `"`)))
	}
	return outs
}

// releaseArtifact is the one allowed capsulectl-* build output: the versioned
// release download, capsulectl-<version>-<os>-<arch>. It is installed under the
// name `capsulectl` and never placed on a plugin root; renaming it would change
// the published release format that checksums, attestations and `release
// watch` all name.
var releaseArtifact = regexp.MustCompile(`^capsulectl-\$VERSION-\$os-\$arch$`)

// TestNoBuildOutputIsNamedLikeAPluginLauncher keeps every binary this repository
// builds from taking the capsulectl-<name> form that plugin discovery reserves
// for launchers: a build output with that name, copied onto a plugin root, would
// be read as a plugin.
func TestNoBuildOutputIsNamedLikeAPluginLauncher(t *testing.T) {
	var files []string
	for _, pattern := range []string{"../../Makefile", "../../action.yml", "../../scripts/*.sh", "../../.github/workflows/*.yml"} {
		matches, err := filepath.Glob(pattern)
		require.NoError(t, err)
		files = append(files, matches...)
	}
	require.NotEmpty(t, files)
	checked, exempted := 0, 0
	for _, file := range files {
		body, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, base := range buildOutputs(string(body)) {
			checked++
			if releaseArtifact.MatchString(base) {
				exempted++
				continue
			}
			assert.False(t, strings.HasPrefix(base, pluginLauncherPrefix), "%s builds %q, a name plugin discovery reserves for launchers", file, base)
		}
	}
	assert.Positive(t, checked, "found no go build lines to check")
	assert.Positive(t, exempted, "the release build was not found, so its exemption never ran")
}

// The release build spans several lines; the guard must see it, and exempt it.
func TestBuildOutputsFindsTheReleaseBuild(t *testing.T) {
	body, err := os.ReadFile("../../scripts/release-build.sh")
	require.NoError(t, err)
	outs := buildOutputs(string(body))
	require.Contains(t, outs, "capsulectl-$VERSION-$os-$arch")
	assert.True(t, releaseArtifact.MatchString("capsulectl-$VERSION-$os-$arch"))
}

// A launcher-shaped output split over continuation lines is still caught.
func TestBuildOutputsJoinsLineContinuations(t *testing.T) {
	body := "go build \\\n    -trimpath \\\n    -o capsulectl-x ./cmd/capsulectl\n"
	outs := buildOutputs(body)
	require.Equal(t, []string{"capsulectl-x"}, outs)
	assert.True(t, strings.HasPrefix(outs[0], pluginLauncherPrefix))
	assert.False(t, releaseArtifact.MatchString(outs[0]))
	// and a CRLF continuation too
	assert.Equal(t, []string{"capsulectl-y"}, buildOutputs("go build \\\r\n  -o \"$OUT/capsulectl-y\" ./cmd/capsulectl\r\n"))
}
