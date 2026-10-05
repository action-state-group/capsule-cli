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

// goBuildOutput finds the -o target of a `go build` line.
var goBuildOutput = regexp.MustCompile(`go build\b[^\n]*?\s-o\s+("[^"]+"|\S+)`)

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
	checked := 0
	for _, file := range files {
		body, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, m := range goBuildOutput.FindAllStringSubmatch(string(body), -1) {
			checked++
			base := filepath.Base(strings.Trim(m[1], `"`))
			if releaseArtifact.MatchString(base) {
				continue
			}
			assert.False(t, strings.HasPrefix(base, pluginLauncherPrefix), "%s builds %q, a name plugin discovery reserves for launchers", file, base)
		}
	}
	assert.Positive(t, checked, "found no go build lines to check")
}
