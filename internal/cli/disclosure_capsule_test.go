package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disclosureProfile is a SQLite profile with a log and a checkpoint key, and
// a run helper for its commands.
type disclosureProfile struct {
	t   *testing.T
	dir string
	n   int
}

func newDisclosureProfile(t *testing.T) *disclosureProfile {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	d := &disclosureProfile{t: t, dir: dir}
	seed := filepath.Join(dir, "seed")
	public := d.run("key", "generate", "--output", seed)["public_key"].(string)
	d.run("profile", "create", "--name", "rep", "--type", "sqlite", "--sqlite-path", filepath.Join(dir, "store.db"),
		"--operator", "Example Operator", "--signing-key-file", seed, "--trusted-key", public,
		"--log-id", "rep-log", "--checkpoint-signing-key-file", seed, "--checkpoint-trusted-key", public)
	d.run("store", "init", "--profile", "rep")
	return d
}

func (d *disclosureProfile) run(args ...string) map[string]any {
	d.t.Helper()
	out, err := invoke(d.t, "", args...)
	require.NoError(d.t, err, out)
	var v map[string]any
	require.NoError(d.t, json.Unmarshal([]byte(out), &v), out)
	return v
}

func (d *disclosureProfile) publish(name, payload string) string {
	d.t.Helper()
	path := filepath.Join(d.dir, name+".json")
	require.NoError(d.t, os.WriteFile(path, []byte(fmt.Sprintf(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":%q,"ActionType":"fyi","Operator":"example-operator","Developer":"example-developer","Timestamp":"2026-10-07T00:00:00Z"},"payload":%s}`, name, payload)), 0o600))
	return d.run("publish", "--profile", "rep", "--request", path)["capsule_id"].(string)
}

func (d *disclosureProfile) checkpoint() {
	d.t.Helper()
	d.run("cll", "checkpoint", "create", "--profile", "rep")
}

// report runs bundle or disclose for root into a new file and returns its path.
func (d *disclosureProfile) report(verb, root string) string {
	d.t.Helper()
	d.n++
	path := filepath.Join(d.dir, fmt.Sprintf("%d-%s.json", d.n, verb))
	out, err := invoke(d.t, "", verb, "--profile", "rep", "--root", root, "--out", path)
	require.NoError(d.t, err, "%s #%d: %s", verb, d.n, out)
	return path
}

func (d *disclosureProfile) logIDs() []string {
	d.t.Helper()
	var ids []string
	for _, e := range d.run("cll", "list", "--profile", "rep")["entries"].([]any) {
		ids = append(ids, e.(map[string]any)["capsule_id"].(string))
	}
	return ids
}

// A second report on a profile works after a checkpoint covers a disclosure:
// each disclosure is a sealed capsule on the log, which a later bundle
// supplies like any record. The exact sequence a re-run of an install takes.
func TestABundleAfterACheckpointThatCoversADisclosureVerifies(t *testing.T) {
	d := newDisclosureProfile(t)
	root := d.publish("check-config", `{"check":"config file","result":"same"}`)
	d.checkpoint()
	d.report("bundle", root)
	d.report("bundle", root)
	d.report("disclose", root)
	d.report("bundle", root)
	d.checkpoint()
	last := d.report("bundle", root)
	result, err := verifyBundleOutput(t, last)
	require.NoError(t, err, result)
	assert.Equal(t, "VALID", result["verdict"])
}

// Disclose, checkpoint, disclose, checkpoint, then bundle: every step works,
// the bundle verifies, and it carries both disclosure capsules, each with a
// membership in the checkpointed interval.
func TestRepeatedDisclosuresAndCheckpointsStillBundle(t *testing.T) {
	d := newDisclosureProfile(t)
	root := d.publish("check-config", `{"check":"config file","result":"same"}`)
	d.checkpoint()
	d.report("bundle", root)
	d.report("disclose", root)
	d.checkpoint()
	d.report("disclose", root)
	d.checkpoint()
	last := d.report("bundle", root)
	result, err := verifyBundleOutput(t, last)
	require.NoError(t, err, result)
	assert.Equal(t, "VALID", result["verdict"])

	b := readBundle(t, last)
	ids := d.logIDs()
	require.Len(t, ids, 3, "the record, then one capsule per disclosure")
	memberships := b["completeness_certificate"].(map[string]any)["memberships"].(map[string]any)
	disclosures := 0
	for _, r := range b["records"].([]any) {
		record := r.(map[string]any)
		if record["action_id"] == disclosureActionID {
			disclosures++
			assert.Contains(t, memberships, record["capsule_id"])
		}
	}
	assert.Equal(t, 2, disclosures)
}

// What a disclose puts on the log is a capsule in the store whose input is
// the disclosure record: ids, digests and labels only, never a byte of what
// was disclosed.
func TestADisclosureIsASealedCapsuleOfIdsAndDigestsOnly(t *testing.T) {
	d := newDisclosureProfile(t)
	root := d.publish("check-config", `{"check":"config file","result":"a distinctive disclosed value"}`)
	d.checkpoint()
	disclosed := d.report("disclose", root)
	raw, err := os.ReadFile(disclosed)
	require.NoError(t, err)
	require.Contains(t, string(raw), "a distinctive disclosed value", "the disclosed copy carries the payload")

	ids := d.logIDs()
	require.Len(t, ids, 2)
	got := d.run("get", "--profile", "rep", "--capsule-id", ids[1])
	capsule := got["capsule"].(map[string]any)
	assert.Equal(t, disclosureActionID, capsule["action_id"])
	assert.Equal(t, "fyi", capsule["action_type"])
	var payload any
	for _, a := range got["artifacts"].([]any) {
		if a := a.(map[string]any); a["name"] == "payload" {
			payload = a["content"]
		}
	}
	require.NotNil(t, payload, "the disclosure record is retained")
	record := payload.(map[string]any)
	assert.Equal(t, "disclosure_record", record["type"])
	assert.Equal(t, root, record["root"])
	text, err := json.Marshal(record)
	require.NoError(t, err)
	assert.NotContains(t, string(text), "distinctive", "no disclosed payload bytes")
	assert.NotContains(t, string(text), "config file", "no disclosed payload bytes")
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for id, members := range record["revealed"].(map[string]any) {
		assert.Regexp(t, hex64, id)
		for member, digest := range members.(map[string]any) {
			assert.Contains(t, []string{"agent_input", "agent_output"}, member)
			assert.Regexp(t, hex64, digest)
		}
	}
	for key := range record {
		assert.Contains(t, []string{"type", "root", "payloads_mode", "suppressed_fields", "revealed"}, key, "only ids, digests and labels")
	}
}

// One report never reveals what another disclosed: in a later bundle, an
// earlier disclosure capsule is proven by its membership but its input is
// withheld, so a copy made for one party says nothing of what was disclosed
// to another. The bundle then states payloads selected, honestly.
func TestALaterReportWithholdsEarlierDisclosures(t *testing.T) {
	d := newDisclosureProfile(t)
	first := d.publish("for-the-first-party", `{"note":"shared with the first party only"}`)
	second := d.publish("for-the-second-party", `{"note":"shared with the second party"}`)
	d.checkpoint()
	d.report("disclose", first)
	d.checkpoint()
	path := d.report("disclose", second)

	result, err := verifyBundleOutput(t, path)
	require.NoError(t, err, result)
	assert.Equal(t, "VALID", result["verdict"])
	b := readBundle(t, path)
	ids := d.logIDs()
	require.Len(t, ids, 4, "two records, the first disclosure, then the second's own (sealed after its bundle)")
	earlier := ids[2]
	_, disclosed := b["disclosures"].(map[string]any)[earlier]
	assert.False(t, disclosed, "the earlier disclosure capsule's input is withheld")
	for _, s := range result["disclosures"].([]any) {
		if s := s.(map[string]any); s["capsule_id"] == earlier {
			assert.Equal(t, "withheld", s["status"])
		}
	}
	assert.Equal(t, "selected", b["completeness"].(map[string]any)["payloads_mode"])
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "disclosure_record", "nothing of the earlier disclosure record")
	assert.Contains(t, b["completeness_certificate"].(map[string]any)["memberships"], earlier, "its place in the log is still proven")
}

// A log that holds a disclosure an earlier release appended as a bare digest
// cannot be bundled once a checkpoint covers it: the error says so plainly.
func TestALegacyBareDisclosureDigestIsNamed(t *testing.T) {
	d := newDisclosureProfile(t)
	root := d.publish("check-config", `{"check":"config file","result":"same"}`)
	p, err := loadProfile("rep")
	require.NoError(t, err)
	target, err := openTarget(t.Context(), p, usePublication)
	require.NoError(t, err)
	_, err = appendRecordDigest(t.Context(), target.log, map[string]interface{}{"type": "disclosure_record", "root": root})
	require.NoError(t, err)
	require.NoError(t, target.close())
	d.checkpoint()

	_, err = invoke(t, "", "bundle", "--profile", "rep", "--root", root, "--out", filepath.Join(d.dir, "x.json"))
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, 2, ExitCode(err))
	assert.Contains(t, err.Error(), "log entry 2 is checkpointed but not in this profile's store: either a disclosure an earlier release appended as a bare digest, or a lost record; no bundle over this log can include it; start a new profile (or log id) for new reports")
	assert.False(t, strings.Contains(err.Error(), "sensitive details suppressed"))
}
