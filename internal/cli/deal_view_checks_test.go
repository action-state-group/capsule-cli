package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealViewChecksDriver loads the deal view under node with the runtime's
// two context readers stubbed, and prints the checks buildModel derives for
// each context it is given on stdin.
const dealViewChecksDriver = `
const fs = require("fs");
const [script] = process.argv.slice(2);
let registered;
const contexts = JSON.parse(fs.readFileSync(0, "utf8"));
const out = {};
for (const [name, c] of Object.entries(contexts)) {
  const payloads = new Map(Object.entries(c.payloads));
  globalThis.EvidenceGraph = {
    registerPresentation: (m) => { registered = m; },
    verifiedPayload: (_ctx, id) => payloads.get(id),
    disclosureOf: (_ctx, id) => ({ state: payloads.has(id) ? "disclosed" : "withheld" }),
  };
  globalThis.document = undefined;
  new Function(fs.readFileSync(script, "utf8"))();
  const context = { ...c, recordIndex: new Map(c.records.map((r) => [r.capsule_id, r])) };
  out[name] = registered.buildModel(context).checks;
}
process.stdout.write(JSON.stringify(out));
`

// The lists say what the page's verification recorded and what the file
// carries, never more: the agent's signature is checked here only on the
// records the page cites as signers; a producer-key/v1 block does not make
// the page check the records' signatures; the checkpoint's signature is
// "checked here" only when the verification did not record
// checkpoint_unverified.
func TestTheDealViewSaysWhichChecksRan(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	sealed := "s"
	report := map[string]any{"type": "deal_report", "report": map[string]any{"audience": "counterparty", "steps": []any{}}}
	base := func() map[string]any {
		return map[string]any{
			"verification": map[string]any{
				"capsuleResults":      map[string]any{sealed: map[string]any{"ok": true}},
				"perRecordMembership": map[string]any{"status": "pass", "findings": []any{"checkpoint_unverified"}},
				"intervalCoverage":    map[string]any{"status": "pass", "findings": []any{"checkpoint_unverified"}},
				"graphClosure":        map[string]any{"status": "pass"},
				"disclosures":         []any{map[string]any{"member": "agent_input", "capsuleId": sealed, "status": "disclosure_match"}},
				"countersignatures":   []any{},
			},
			"records":  []any{map[string]any{"capsule_id": sealed, "signature": "sig", "action_id": "capsulectl-deal-report"}},
			"payloads": map[string]any{sealed: report},
			"bundle": map[string]any{
				"extensions": map[string]any{"x-deal-v0": map[string]any{"sealed_report": sealed}},
				"checkpoint": map[string]any{"cose": "c"},
			},
		}
	}
	contexts := map[string]map[string]any{"base": base()}

	verified := base()
	for _, c := range []string{"perRecordMembership", "intervalCoverage"} {
		verified["verification"].(map[string]any)[c] = map[string]any{"status": "pass", "findings": []any{}}
	}
	contexts["checkpoint checked"] = verified

	cites := base()
	cites["records"] = append(cites["records"].([]any), map[string]any{"capsule_id": "l", "signature": "sig"})
	cites["payloads"].(map[string]any)["l"] = map[string]any{"links": []any{map[string]any{"type": "acknowledges", "target": "c"}}}
	contexts["cites a signer"] = cites

	producerKey := base()
	producerKey["bundle"].(map[string]any)["extensions"].(map[string]any)["producer-key/v1"] = map[string]any{"public_key": "ab"}
	contexts["producer-key block"] = producerKey

	countersigned := base()
	countersigned["verification"].(map[string]any)["countersignatures"] = []any{map[string]any{"status": "unverified"}}
	contexts["countersigned"] = countersigned

	witnessed := base()
	witnessed["bundle"].(map[string]any)["extensions"].(map[string]any)["x-cadence-witness/v0"] = map[string]any{
		"state": "witnessed", "extent": "part", "steps_witnessed": 2, "steps": 3, "checkpoint_at": "2026-10-04T09:00:00Z",
		"cadence": map[string]any{"witnesses": []any{map[string]any{"ts_url": "https://witness.example"}}},
		"earlier": map[string]any{"checkpoint": map[string]any{}, "consistency_proof": map[string]any{}},
	}
	contexts["witnessed in part"] = witnessed

	driver := filepath.Join(t.TempDir(), "driver.js")
	require.NoError(t, os.WriteFile(driver, []byte(dealViewChecksDriver), 0o600))
	script, err := filepath.Abs("assets/deal-view.js")
	require.NoError(t, err)
	input, err := json.Marshal(contexts)
	require.NoError(t, err)
	cmd := exec.Command(node, driver, script)
	cmd.Stdin = bytes.NewReader(input)
	raw, err := cmd.Output()
	require.NoError(t, err)
	var got map[string]struct {
		Page []string `json:"page"`
		CLI  []string `json:"cli"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))

	pageChecks := []string{"digests", "membership", "range", "disclosures"}
	assert.Equal(t, pageChecks, got["base"].Page)
	assert.Equal(t, []string{"checkpoint-signature", "producer-signatures"}, got["base"].CLI)

	assert.Equal(t, append(append([]string{}, pageChecks...), "checkpoint-signature"), got["checkpoint checked"].Page)
	assert.Equal(t, []string{"producer-signatures"}, got["checkpoint checked"].CLI)

	assert.Equal(t, append(append([]string{}, pageChecks...), "cited-signers"), got["cites a signer"].Page)
	assert.Equal(t, []string{"checkpoint-signature", "producer-signatures"}, got["cites a signer"].CLI, "the signature on every record is still the command's")

	assert.Equal(t, got["base"], got["producer-key block"], "a producer-key/v1 block does not make the page check the records' signatures")

	assert.Equal(t, append(append([]string{}, pageChecks...), "countersignatures"), got["countersigned"].Page)

	assert.Equal(t, pageChecks, got["witnessed in part"].Page)
	assert.Equal(t, []string{"checkpoint-signature", "producer-signatures", "witness-receipt", "consistency"}, got["witnessed in part"].CLI)
}
