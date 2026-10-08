package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runReportVariant runs the README's worked example with edit applied to the
// script, in an empty directory with this build's capsulectl on PATH, and
// returns that directory and the run's output and error (the script stops at
// its first failing command).
func runReportVariant(t *testing.T, edit func(string) string) (string, string, error) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("jq is required in CI: the README's report example uses it")
		}
		t.Skip("jq not available; the README's report example did not run")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "example.sh")
	require.NoError(t, os.WriteFile(script, []byte(edit(readmeReportScript(t))), 0o600))
	cmd := exec.Command("bash", "-e", "-o", "pipefail", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+capsulectlOnPath(t)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
	out, err := cmd.CombinedOutput()
	return dir, string(out), err
}

// replaced applies each old -> new replacement to s, each exactly once.
func replaced(t *testing.T, s string, pairs ...string) string {
	t.Helper()
	for i := 0; i+1 < len(pairs); i += 2 {
		require.Equal(t, 1, strings.Count(s, pairs[i]), "the README's example still has %q", pairs[i])
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return s
}

// exitCode is the exit code a run's error carries.
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

const (
	readmeSQLite     = "--type sqlite --sqlite-path ./store.db"
	readmeJSONL      = "--type jsonl --jsonl-path ./store --namespace example"
	readmeDiscloseOK = `capsulectl disclose --profile example --root "$report" --out bundle.json --html report.html`
	readmeOutOnly    = `capsulectl disclose --profile example --root "$report" --out bundle.json`
	readmeBundleHTML = `capsulectl bundle --profile example --root "$report" --out bundle.json --html report.html`
)

// The README's VALID bundle passes the gate: its page is the one CI renders.
func TestPageGateWritesTheReadmePage(t *testing.T) {
	bundle, page := runReadmeReport(t)
	require.NoError(t, pageGate(readBundle(t, bundle)))
	assert.FileExists(t, page)
}

// A jsonl profile writes no page: its bundle carries the evidence book's
// records about what was sealed, not the sealed records, so no row can be
// shown. Refused before anything is opened or put on record; and its bundle,
// written without a page, is INCOMPLETE, which the gate refuses on its own.
func TestPageGateRefusesAJSONLProfilesPage(t *testing.T) {
	dir, out, err := runReportVariant(t, func(s string) string {
		return replaced(t, s, readmeSQLite, readmeJSONL)
	})
	require.Error(t, err)
	assert.Contains(t, out, "no page written: a jsonl profile's bundle carries its evidence book's records")
	assert.NoFileExists(t, filepath.Join(dir, "report.html"))
	assert.NoFileExists(t, filepath.Join(dir, "bundle.json"), "disclose is refused before the bundle is built or put on record")

	// bundle --html on a jsonl profile writes the bundle and refuses the page.
	dir, out, err = runReportVariant(t, func(s string) string {
		return replaced(t, s, readmeSQLite, readmeJSONL, readmeDiscloseOK, readmeBundleHTML)
	})
	require.Error(t, err)
	assert.Contains(t, out, "no page written: a jsonl profile's bundle carries its evidence book's records")
	assert.NoFileExists(t, filepath.Join(dir, "report.html"))
	assert.FileExists(t, filepath.Join(dir, "bundle.json"))

	dir, out, err = runReportVariant(t, func(s string) string {
		return replaced(t, s, readmeSQLite, readmeJSONL, readmeDiscloseOK, readmeOutOnly)
	})
	require.Equal(t, 3, exitCode(err), "the jsonl bundle verifies INCOMPLETE:\n%s", out)
	gate := pageGate(readBundle(t, filepath.Join(dir, "bundle.json")))
	require.ErrorIs(t, gate, ErrPartial)
	assert.Contains(t, gate.Error(), "calls this bundle INCOMPLETE")
	assert.Contains(t, gate.Error(), "producer_signatures withheld")
}

// A report whose row cites a record the bundle does not carry (here a
// capsule id that was never sealed) is a VALID bundle, since verify checks
// what the records seal, not what a row cites; but its page would show 1 of 2
// rows: no page is written, and nothing is disclosed.
func TestPageGateRefusesAReportMissingARow(t *testing.T) {
	missing := func(s string) string {
		return replaced(t, s, `references: [cite($permissions)]`, `references: [cite("`+strings.Repeat("0", 64)+`")]`)
	}
	dir, out, err := runReportVariant(t, func(s string) string { return replaced(t, missing(s), readmeDiscloseOK, readmeOutOnly) })
	require.NoError(t, err, "the bundle itself is VALID:\n%s", out)
	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	require.NoError(t, err)
	b, err := decodeBundleJSON(raw) // as verify --bundle reads it
	require.NoError(t, err)
	verdict, _ := bundleVerdict(b, nil)
	require.Equal(t, "VALID", verdict)
	shown, total, ok := reportRowsShown(b)
	require.True(t, ok)
	assert.Equal(t, [2]int{1, 2}, [2]int{shown, total})
	gate := pageGate(b)
	require.ErrorIs(t, gate, ErrPartial)
	assert.Contains(t, gate.Error(), "would show 1 of 2 rows")

	// disclose --html writes neither: no disclosure goes on record without
	// its page.
	dir, out, err = runReportVariant(t, missing)
	require.Equal(t, 3, exitCode(err), "disclose --html is refused:\n%s", out)
	assert.Contains(t, out, "would show 1 of 2 rows")
	assert.NoFileExists(t, filepath.Join(dir, "report.html"))
	assert.NoFileExists(t, filepath.Join(dir, "bundle.json"))

	// (bundle carries no payloads, so its page shows no report rows to count;
	// bundle's own refusal, JSON written and page refused, is tested on a
	// jsonl profile above.)
}

// A deal's receipt still writes its page: every deal page measured is VALID,
// own copy and shared.
func TestPageGateKeepsDealReceipts(t *testing.T) {
	id := cancelAfterPurchase(t)
	page := filepath.Join(t.TempDir(), "receipt.html")
	dealRun(t, "report", "--deal", id, "--html", page)
	assert.FileExists(t, page)
	shared := filepath.Join(t.TempDir(), "shared.html")
	dealRun(t, "report", "--deal", id, "--share", "counterparty", "--to", "the shop's support desk", "--html", shared)
	assert.FileExists(t, shared)
}
