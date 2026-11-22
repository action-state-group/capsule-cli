package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// companyPluginNames is what the neutral core's code, help and docs must not
// name (capsule-cli#428): the company plugins and the company library behind
// them. The short plugin names `book` and `deal` are also vocabulary (a
// profile type, a deal), so for them only code that picks a plugin by its
// short name is refused.
var (
	companyPluginNames = regexp.MustCompile(`(?i)capsulectl-book|capsulectl-deal|evidencebook`)
	shortNameDispatch  = regexp.MustCompile(`(LogOwner\(\)|Owner|owner|Plugin|plugin)\s*[!=]=\s*"(book|deal)"|firstPartyVerbs`)
)

// neutralNamesAllowed is every file that may still name a company plugin, each
// with its reason. A new entry is a decision, not a convenience: the issue
// allows only these.
var neutralNamesAllowed = map[string]string{
	// Sealed vocabulary: written into bundles that core verify checks without the plugin.
	"cli/commitment_publish_cmd.go": "the --binding default is written into a signed commitment",
	"cli/composed.go":               "signature profile ids evidencebook-refusal and evidencebook-artifact-response, sealed in composed bundles",
	"cli/deal_sealed_report.go":     "action id capsulectl-deal-report, and the NOT_CHECKED note's for-example hook (capsule-cli#599)",
	"cli/deal_history.go":           "action id capsulectl-deal-history, sealed in history bundles",
	// The neutrality guards name what they guard against.
	".github/workflows/ci.yml":      "the core go.mod must not require the company library",
	"scripts/check-neutral-deps.sh": "lists the company library as a refused dependency",
	"scripts/test-weights":          "test names",
}

func TestCoreNamesNoCompanyPlugin(t *testing.T) {
	root := ".."
	skipDirs := map[string]bool{".git": true, "testdata": true, "design": true, "changelog.d": true, "node_modules": true}
	skipFiles := map[string]bool{"CHANGELOG.md": true, "go.sum": true}
	var seen int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if skipFiles[d.Name()] || strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "docs/design/") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil || strings.ContainsRune(string(raw[:min(len(raw), 512)]), 0) {
			return nil // unreadable or binary
		}
		seen++
		for i, line := range strings.Split(string(raw), "\n") {
			if _, ok := neutralNamesAllowed[rel]; ok {
				continue
			}
			if m := companyPluginNames.FindString(line); m != "" {
				assert.Failf(t, "company plugin name in the core", "%s:%d names %q; the core is neutral (capsule-cli#428): say \"a plugin\" or add the file to neutralNamesAllowed with a reason", rel, i+1, m)
			}
			if strings.HasSuffix(rel, ".go") && shortNameDispatch.MatchString(line) {
				assert.Failf(t, "plugin picked by its short name", "%s:%d dispatches on a plugin's short name", rel, i+1)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, seen, 50, "the scan found the repository's files")
	for rel := range neutralNamesAllowed {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		assert.NoError(t, err, "allowlisted file %s exists", rel)
	}
}
