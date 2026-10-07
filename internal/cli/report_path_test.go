package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reportExampleHeading = "## Example: a self-checking report of records you sealed"

// readmeReportScript is the README's worked example exactly as a reader
// copies it: the first bash block under its heading.
func readmeReportScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	readme := string(raw)
	start := strings.Index(readme, reportExampleHeading)
	require.NotEqual(t, -1, start, "the README has the worked example")
	section := readme[start+len(reportExampleHeading):]
	section = section[:strings.Index(section, "\n## ")]
	open := strings.Index(section, "```bash\n")
	require.NotEqual(t, -1, open, "the example has a bash block")
	block := section[open+len("```bash\n"):]
	return block[:strings.Index(block, "\n```")+1]
}

var (
	builtCapsulectl     string
	buildCapsulectlOnce sync.Once
	buildCapsulectlErr  error
)

// capsulectlOnPath builds this capsulectl once per test binary and returns a
// directory holding it as `capsulectl`, for scripts that run it by name.
func capsulectlOnPath(t *testing.T) string {
	t.Helper()
	buildCapsulectlOnce.Do(func() {
		dir, err := os.MkdirTemp("", "capsulectl-bin-")
		if err != nil {
			buildCapsulectlErr = err
			return
		}
		builtCapsulectl = dir
		out, err := exec.Command("go", "build", "-o", filepath.Join(dir, "capsulectl"), "../../cmd/capsulectl").CombinedOutput()
		if err != nil {
			buildCapsulectlErr = err
			builtCapsulectl = string(out)
		}
	})
	require.NoError(t, buildCapsulectlErr, builtCapsulectl)
	return builtCapsulectl
}

// runReadmeReport runs the README's worked example, as written, in an empty
// directory with this build's capsulectl on PATH. It returns the bundle and
// page it writes. jq is required in CI; elsewhere the test is skipped
// without it.
func runReadmeReport(t *testing.T) (bundle, page string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jq is required in CI: the README's report example uses it")
		}
		t.Skip("jq not available; the README's report example did not run")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "example.sh")
	require.NoError(t, os.WriteFile(script, []byte(readmeReportScript(t)), 0o600))
	cmd := exec.Command("bash", "-e", "-o", "pipefail", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+capsulectlOnPath(t)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the README's example fails as written:\n%s", out)
	return filepath.Join(dir, "bundle.json"), filepath.Join(dir, "report.html")
}

// The README's example runs as written and its bundle verifies offline:
// VALID, with every claim passing; the cited records and the report/v1 root
// are in the bundle; the page carries the verifier and the bundle. VALID
// rests on keys the bundle carries: here every one is the profile's key.
func TestTheReadmeReportExampleRunsAndVerifies(t *testing.T) {
	bundle, page := runReadmeReport(t)
	result, err := verifyBundleOutput(t, bundle)
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	for _, claim := range []string{"checkpoint", "interval_coverage", "per_record_membership", "graph_closure", "producer_signatures"} {
		assert.Equal(t, "pass", status(result, claim), claim)
	}
	assert.Equal(t, "withheld", result["witnesses"].(map[string]interface{})["status"], "no witness receipt, and the verdict is VALID")

	b := readBundle(t, bundle)
	report := b["disclosures"].(map[string]interface{})[b["root"].(string)].(map[string]interface{})["agent_input"].(map[string]interface{})
	assert.Equal(t, "report/v1", report["spec_version"])
	assert.Equal(t, "Example checks", report["title"])
	cited := map[string]bool{}
	for _, row := range report["rows"].([]interface{}) {
		for _, ref := range row.(map[string]interface{})["references"].([]interface{}) {
			cited[ref.(map[string]interface{})["digest"].(string)] = true
		}
	}
	require.Len(t, cited, 2)
	keys := map[string]bool{}
	for _, r := range b["records"].([]interface{}) {
		record := r.(map[string]interface{})
		delete(cited, record["capsule_id"].(string))
		keys[record["key_id"].(string)] = true
	}
	assert.Empty(t, cited, "every record a row cites is in the bundle")
	keys[b["checkpoint"].(map[string]interface{})["key_id"].(string)] = true
	assert.Len(t, keys, 1, "the records and the checkpoint carry one key: the README's jq line prints it")

	html, err := os.ReadFile(page)
	require.NoError(t, err)
	assert.Contains(t, string(html), string(evidenceGraphIIFE), "the page carries the verifier")
	assert.Contains(t, string(html), "Example checks", "and the bundle it checks")
}

// A record changed after the report was made fails verification: a cited
// record's disclosed payload no longer matches what it sealed, and an edited
// record no longer has its Capsule ID.
func TestATamperedReportFailsVerification(t *testing.T) {
	bundle, _ := runReadmeReport(t)
	original := readBundle(t, bundle)
	var cited string
	for _, r := range original["records"].([]interface{}) {
		if id := r.(map[string]interface{})["capsule_id"].(string); id != original["root"] {
			cited = id
			break
		}
	}
	require.NotEmpty(t, cited)
	for name, edit := range map[string]func(b map[string]interface{}){
		"a cited record's disclosed payload": func(b map[string]interface{}) {
			b["disclosures"].(map[string]interface{})[cited].(map[string]interface{})["agent_input"].(map[string]interface{})["result"] = "edited"
		},
		"a cited record": func(b map[string]interface{}) {
			for _, r := range b["records"].([]interface{}) {
				if r := r.(map[string]interface{}); r["capsule_id"] == cited {
					r["action_id"] = "check-something-else"
				}
			}
		},
	} {
		b := readBundle(t, bundle)
		edit(b)
		out, err := json.Marshal(b)
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "tampered.json")
		require.NoError(t, os.WriteFile(path, out, 0o600))
		result, err := verifyBundleOutput(t, path)
		assert.ErrorIs(t, err, ErrBundleInvalid, name)
		assert.Equal(t, "INVALID", result["verdict"], name)
		assert.Equal(t, 1, ExitCode(err), name)
	}
}

func readBundle(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var b map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}
