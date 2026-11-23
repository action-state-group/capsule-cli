package cli

import (
	"go/parser"
	"go/token"
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
	companyPluginNames = regexp.MustCompile(`(?i)capsulectl-book|capsulectl-deal|evidencebook|deal standard-history|"capsulectl-"\s*\+\s*"(book|deal)"`)
	// communityNames are the communities the core's code and help must not name:
	// a community is a deployment's word (capsule-cli#428 step 5).
	communityNames    = regexp.MustCompile(`(?i)\bmesh-llm\b|\bmuse\b|\bgoose\b`)
	shortNameDispatch = regexp.MustCompile(`(LogOwner\(\)|Owner|owner|Plugin|plugin)\s*[!=]=\s*"(book|deal)"|case "(book|deal)"|slices\.Contains\([^)]*"(book|deal)"|firstPartyVerbs`)
)

// neutralNamesAllowed lists, per file, the lines that may still name a company
// plugin, each pattern with its reason; every other line of the file is still
// checked. A new entry is a decision, not a convenience: the issue allows only
// these.
var neutralNamesAllowed = map[string][]*regexp.Regexp{
	// Sealed vocabulary: written into bundles that core verify checks without the plugin.
	"cli/composed.go": {regexp.MustCompile(`EvidenceBookRefusalProfile|verifyEvidenceBookRefusal|"evidencebook-(refusal|artifact-response)"|book-library profile`)}, // signature profile ids sealed in composed bundles
	// The neutrality guards name what they guard against.
	".github/workflows/ci.yml":      {regexp.MustCompile(`grep -n evidencebook go.mod|requires evidencebook`)},
	"scripts/check-neutral-deps.sh": {regexp.MustCompile(`company-OSS module \(evidencebook`)},
	"scripts/test-weights":          {regexp.MustCompile(`TestEvidenceBookRefusalSignature`)}, // a test name
}

// attributionInCode matches, in a code comment, a person's name, a Buzz message id,
// an ops id or an internal unit id: the code cites a spec section or an issue
// instead, so nothing in it points at a private conversation.
var attributionInCode = regexp.MustCompile(`(?i)\bsherlock2?\b|\bsteven\b|\bethan\b|\bbuzz [0-9a-f]{6,}|\bops[ -][0-9a-f]{4,}|\bu\d{3,4}\b`)

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
	assert.True(t, communityNames.MatchString("--community is one of mesh-llm, muse or goose"))
	assert.False(t, communityNames.MatchString("a museum of goosebumps"))
	for _, cite := range []string{"Sherlock2", "Sherlock", "Steven requested this", "(Steven's ruling)", "Ethan, see below", "Buzz 71efdf5a", "ops-123abc", "ops c2438a45", "u947", "u1000"} {
		assert.True(t, attributionInCode.MatchString("// "+cite), cite)
	}
	for _, ok := range []string{"REGISTRY.md and history-frame-v0 §6, capsule-cli#511", "a Buzz message", "stevenson and ethanol", "buses", "a unit of 12", "u12"} {
		assert.False(t, attributionInCode.MatchString("// "+ok), ok)
	}
	for _, line := range []string{`"capsulectl-" + name`, `Kind: "deal"`} {
		assert.False(t, companyPluginNames.MatchString(line) || shortNameDispatch.MatchString(line), line)
	}
}

func TestCoreNamesNoCompanyPlugin(t *testing.T) {
	root := ".."
	skipDirs := map[string]bool{".git": true, ".claude": true, "testdata": true, "design": true, "changelog.d": true, "node_modules": true}
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
			if strings.HasPrefix(rel, "cli/") && strings.HasSuffix(rel, ".go") && communityNames.MatchString(line) {
				assert.Failf(t, "community named in the core", "%s:%d names a community (%q)", rel, i+1, communityNames.FindString(line))
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

// The comments of the core's Go files, tests included, cite a spec section or an
// issue and never a person, a Buzz message, an ops id or an internal unit id.
// Only comment text is read: a string or an identifier is not a citation.
func TestCodeCommentsCitePrivateConversationsNowhere(t *testing.T) {
	checked := 0
	for _, dir := range []string{"../cli", "../settlement"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		for _, file := range files {
			if filepath.Base(file) == "neutral_names_test.go" { // its samples are the patterns
				continue
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
			require.NoError(t, err)
			for _, group := range f.Comments {
				for _, c := range group.List {
					for i, line := range strings.Split(c.Text, "\n") {
						checked++
						if m := attributionInCode.FindString(line); m != "" {
							assert.Failf(t, "private attribution in a comment", "%s:%d cites %q: cite a spec section or an issue", file, fset.Position(c.Pos()).Line+i, m)
						}
					}
				}
			}
		}
	}
	assert.Greater(t, checked, 1000, "the scan read the comments of the core's Go files")
}

func TestAttributionGuardReadsOnlyComments(t *testing.T) {
	src := "package p\n\nconst id = \"u947\" // a Sherlock2 cite\nconst name = \"Steven\"\n"
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.ParseComments)
	require.NoError(t, err)
	var hits int
	for _, g := range f.Comments {
		for _, c := range g.List {
			if attributionInCode.MatchString(c.Text) {
				hits++
			}
		}
	}
	assert.Equal(t, 1, hits, "the string literals are not read, the comment is")
}
