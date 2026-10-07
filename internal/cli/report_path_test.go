package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reportFromSealedRecords is the README's worked example, "A self-checking
// report of records you sealed": on a SQLite profile with a log, publish two
// records and a report/v1 root citing them, cut one checkpoint, then
// disclose the root as a bundle and a self-checking page. It returns the
// bundle and page paths and the two records' Capsule IDs.
func reportFromSealedRecords(t *testing.T) (bundle, page string, records []string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	run := func(args ...string) map[string]interface{} {
		t.Helper()
		out, err := invoke(t, "", args...)
		require.NoError(t, err, out)
		var v map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(out), &v), out)
		return v
	}
	seed := filepath.Join(dir, "seed")
	public := run("key", "generate", "--output", seed)["public_key"].(string)
	run("profile", "create", "--name", "example", "--type", "sqlite", "--sqlite-path", filepath.Join(dir, "store.db"),
		"--operator", "Example Operator", "--signing-key-file", seed, "--trusted-key", public,
		"--log-id", "example-log", "--checkpoint-signing-key-file", seed, "--checkpoint-trusted-key", public)
	run("store", "init", "--profile", "example")

	publish := func(name string, capsule map[string]interface{}, payload interface{}) string {
		t.Helper()
		capsule["ActionID"], capsule["ActionType"], capsule["Operator"], capsule["Developer"] = name, "fyi", "example-operator", "example-developer"
		raw, err := json.Marshal(map[string]interface{}{"spec_version": "capsule-seal-request/v1", "capsule": capsule, "payload": payload})
		require.NoError(t, err)
		path := filepath.Join(dir, name+".json")
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		return run("publish", "--profile", "example", "--request", path)["capsule_id"].(string)
	}
	config := publish("check-config", map[string]interface{}{"Timestamp": "2026-10-07T00:00:00Z"}, map[string]interface{}{"check": "config file", "result": "same"})
	perms := publish("check-permissions", map[string]interface{}{"Timestamp": "2026-10-07T00:00:00Z"}, map[string]interface{}{"check": "permissions", "result": "different"})
	cite := func(id string) map[string]interface{} {
		return map[string]interface{}{"type": "agent-action-capsule", "digest_alg": "sha256", "digest": id, "citation_purpose": "acted_on"}
	}
	root := publish("report", map[string]interface{}{
		"Timestamp": "2026-10-07T00:01:00Z",
		"References": []map[string]interface{}{
			{"Type": "agent-action-capsule", "DigestAlg": "sha256", "Digest": config, "CitationPurpose": "acted_on"},
			{"Type": "agent-action-capsule", "DigestAlg": "sha256", "Digest": perms, "CitationPurpose": "acted_on"},
		},
	}, map[string]interface{}{"spec_version": "report/v1", "title": "Example checks", "rows": []interface{}{
		map[string]interface{}{"row_id": "config", "label": "Config file", "status": "same", "reason": "unchanged since the last run", "references": []interface{}{cite(config)}},
		map[string]interface{}{"row_id": "permissions", "label": "Permissions", "status": "different", "references": []interface{}{cite(perms)}},
	}})
	run("cll", "checkpoint", "create", "--profile", "example")
	bundle, page = filepath.Join(dir, "report-bundle.json"), filepath.Join(dir, "report-bundle.html")
	out, err := invoke(t, "", "disclose", "--profile", "example", "--root", root, "--out", bundle, "--html", page)
	require.NoError(t, err, out)
	return bundle, page, []string{config, perms}
}

// The report bundle verifies offline: every claim passes and the verdict is
// VALID. The page carries the bundle and the verifier, and the report's
// title and rows are in the bundle it checks.
func TestAReportOfSealedRecordsVerifies(t *testing.T) {
	bundle, page, records := reportFromSealedRecords(t)
	result, err := verifyBundleOutput(t, bundle)
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	for _, claim := range []string{"checkpoint", "interval_coverage", "per_record_membership", "graph_closure", "producer_signatures"} {
		assert.Equal(t, "pass", status(result, claim), claim)
	}

	raw, err := os.ReadFile(bundle)
	require.NoError(t, err)
	var b map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &b))
	ids := map[string]bool{}
	for _, r := range b["records"].([]interface{}) {
		ids[r.(map[string]interface{})["capsule_id"].(string)] = true
	}
	for _, id := range records {
		assert.True(t, ids[id], "the cited record %s is in the bundle", id)
	}
	report := b["disclosures"].(map[string]interface{})[b["root"].(string)].(map[string]interface{})["agent_input"].(map[string]interface{})
	assert.Equal(t, "report/v1", report["spec_version"])
	assert.Equal(t, "Example checks", report["title"])

	html, err := os.ReadFile(page)
	require.NoError(t, err)
	assert.Contains(t, string(html), string(evidenceGraphIIFE), "the page carries the verifier")
	assert.Contains(t, string(html), "Example checks", "and the bundle it checks")
}

// A record changed after the report was made fails verification: a cited
// record's disclosed payload no longer matches its sealed commitment, and an
// edited record no longer has its Capsule ID.
func TestATamperedReportFailsVerification(t *testing.T) {
	bundle, _, records := reportFromSealedRecords(t)
	raw, err := os.ReadFile(bundle)
	require.NoError(t, err)
	for name, edit := range map[string]func(b map[string]interface{}){
		"a cited record's disclosed payload": func(b map[string]interface{}) {
			b["disclosures"].(map[string]interface{})[records[1]].(map[string]interface{})["agent_input"].(map[string]interface{})["result"] = "same"
		},
		"a cited record": func(b map[string]interface{}) {
			for _, r := range b["records"].([]interface{}) {
				if r := r.(map[string]interface{}); r["capsule_id"] == records[0] {
					r["action_id"] = "check-something-else"
				}
			}
		},
	} {
		var b map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &b))
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

// The README's worked example is the one this test runs: same commands, same
// order.
func TestTheReadmeShowsTheReportPathTheTestRuns(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	readme := string(raw)
	start := strings.Index(readme, "## Example: a self-checking report of records you sealed")
	require.NotEqual(t, -1, start, "the README has the worked example")
	section := readme[start:]
	if end := strings.Index(section[3:], "\n## "); end >= 0 {
		section = section[:end+3]
	}
	block := section[strings.Index(section, "```bash"):]
	block = block[:strings.Index(block[3:], "```")+3]
	last := -1
	for _, step := range []string{"profile create", "store init", "publish", "cll checkpoint create", "disclose", "verify --bundle"} {
		at := strings.Index(block, step)
		require.NotEqual(t, -1, at, fmt.Sprintf("the example runs %q", step))
		assert.Greater(t, at, last, "the example runs %q in the test's order", step)
		last = at
	}
	for _, detail := range []string{"--type sqlite", "--log-id", "--checkpoint-signing-key-file", "--html"} {
		assert.Contains(t, block, detail)
	}
	for _, field := range []string{`"report/v1"`, `"References"`, `"rows"`, `"row_id"`, `"citation_purpose": "acted_on"`} {
		assert.Contains(t, section, field)
	}
}
