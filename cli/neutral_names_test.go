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
	companyPluginNames = regexp.MustCompile(`(?i)capsulectl-book|capsulectl-deal|evidencebook|"capsulectl-"\s*\+\s*"(book|deal)"`)
	shortNameDispatch  = regexp.MustCompile(`(LogOwner\(\)|Owner|owner|Plugin|plugin)\s*[!=]=\s*"(book|deal)"|case "(book|deal)"|firstPartyVerbs`)
)

// neutralNamesAllowed lists, per file, the lines that may still name a company
// plugin, each pattern with its reason; every other line of the file is still
// checked. A new entry is a decision, not a convenience: the issue allows only
// these.
var neutralNamesAllowed = map[string][]*regexp.Regexp{
	// Sealed vocabulary: written into bundles that core verify checks without the plugin.
	"cli/composed.go":           {regexp.MustCompile(`EvidenceBookRefusalProfile|verifyEvidenceBookRefusal|"evidencebook-(refusal|artifact-response)"|book-library profile`)}, // signature profile ids sealed in composed bundles
	"cli/deal_sealed_report.go": {regexp.MustCompile(`"capsulectl-deal-report"|with capsulectl-deal installed`)},                                                              // the action id, and the NOT_CHECKED note's for-example hook (capsule-cli#599)
	"cli/deal_history.go":       {regexp.MustCompile(`"capsulectl-deal-history"`)},                                                                                            // the action id sealed in history bundles
	// The neutrality guards name what they guard against.
	".github/workflows/ci.yml":      {regexp.MustCompile(`grep -n evidencebook go.mod|requires evidencebook`)},
	"scripts/check-neutral-deps.sh": {regexp.MustCompile(`company-OSS module \(evidencebook`)},
	"scripts/test-weights":          {regexp.MustCompile(`TestEvidenceBookRefusalSignature`)}, // a test name
}

func lineAllowed(rel, line string) bool {
	for _, re := range neutralNamesAllowed[rel] {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// The patterns catch what a reviewer would: a name, a name split by a
// concatenation, and a short-name dispatch. They are a tripwire, not a parser.
func TestNeutralNamesPatternsCatchTheShapes(t *testing.T) {
	for _, line := range []string{
		`x := "capsulectl-deal"`,
		`x := "capsulectl-" + "deal"`,
		`switch p.LogOwner() { case "deal": }`,
		`if owner == "book" {`,
		`var firstPartyVerbs = []string{"deal"}`,
	} {
		assert.True(t, companyPluginNames.MatchString(line) || shortNameDispatch.MatchString(line), line)
	}
	for _, line := range []string{`"capsulectl-" + name`, `Kind: "deal"`} {
		assert.False(t, companyPluginNames.MatchString(line) || shortNameDispatch.MatchString(line), line)
	}
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
			if m := companyPluginNames.FindString(line); m != "" && !lineAllowed(rel, line) {
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
