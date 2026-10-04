package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const freshSkill = "---\nname: deal\ndescription: the fresh skill\n---\n\n# deal\n\nCheck before every point of no return.\n"
const staleSkill = "---\nname: deal\ndescription: the old skill\n---\n\n# deal\n\nAn older procedure.\n"

func writeSkill(t *testing.T, root, dir, body string) string {
	t.Helper()
	path := filepath.Join(root, dir, "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func anomalyKinds(report map[string]any) []string {
	var kinds []string
	for _, a := range report["anomalies"].([]any) {
		kinds = append(kinds, a.(map[string]any)["kind"].(string))
	}
	return kinds
}

// The 2026-10-04 failure: a reinstall left the old skill in
// skills/deal.bak-<date>/, the host loaded that copy, and nothing in any
// record showed which instructions were followed.
func TestDealSkillDigestCatchesAStaleBackup(t *testing.T) {
	dealFixture(t)
	now := clockAt(t, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	skills := t.TempDir()
	fresh := writeSkill(t, skills, "deal", freshSkill)
	stale := writeSkill(t, skills, "deal.bak-20261003", staleSkill)
	writeSkill(t, skills, "capsulectl", "---\nname: capsulectl\n---\n") // another skill is not a copy

	first := dealRun(t, "open", "--skill", fresh, "--input", filepath.Join(retailDemo, "open.json"))
	assert.Equal(t, digestOf(freshSkill), first["skill"].(map[string]any)["skill_md_digest"])
	assert.Equal(t, float64(1), first["skill"].(map[string]any)["other_copies"])
	report := dealRun(t, "report", "--deal", first["deal_id"].(string))
	assert.Contains(t, anomalyKinds(report), "skill_copies_visible", "the planted backup is visible beside the skill")

	// The next deal follows the stale copy, as the host did on Oct 4.
	*now = now.Add(time.Hour)
	second := dealRun(t, "open", "--skill", stale, "--input", filepath.Join(retailDemo, "open.json"))
	assert.Equal(t, digestOf(staleSkill), second["skill"].(map[string]any)["skill_md_digest"])
	report = dealRun(t, "report", "--deal", second["deal_id"].(string), "--bundle", filepath.Join(t.TempDir(), "b.json"))
	assert.Contains(t, anomalyKinds(report), "skill_changed")
	var changed string
	for _, a := range report["anomalies"].([]any) {
		if a.(map[string]any)["kind"] == "skill_changed" {
			changed = a.(map[string]any)["text"].(string)
		}
	}
	assert.Contains(t, changed, "sha256:"+digestOf(freshSkill)[:12]+" → sha256:"+digestOf(staleSkill)[:12])
	a := report["assurance"].(map[string]any)
	assert.Contains(t, a["text"], "Instructions present when the deal opened: deal SKILL.md sha256:"+digestOf(staleSkill)[:12])
	assert.Contains(t, a["text"], "not proof the agent followed them")

	// The sealed baseline carries the digest and the count, never a path.
	export := filepath.Join(t.TempDir(), "records.json")
	dealRun(t, "export", "--deal", second["deal_id"].(string), "--output", export)
	raw := string(mustRead(t, export))
	var records []map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &records))
	assert.Equal(t, map[string]any{"skill_md_digest": digestOf(staleSkill), "other_copies": float64(1)}, records[0]["body"].(map[string]any)["skill"])
	assert.NotContains(t, raw, skills)
	assert.NotContains(t, raw, "deal.bak-20261003")
}

func TestDealSkillDigestFollowsTheFile(t *testing.T) {
	dealFixture(t)
	now := clockAt(t, time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	skill := writeSkill(t, t.TempDir(), "deal", freshSkill)
	first := dealRun(t, "open", "--skill", skill, "--input", filepath.Join(retailDemo, "open.json"))
	*now = now.Add(time.Hour)
	same := dealRun(t, "open", "--skill", skill, "--input", filepath.Join(retailDemo, "open.json"))
	assert.Equal(t, first["skill"].(map[string]any)["skill_md_digest"], same["skill"].(map[string]any)["skill_md_digest"])
	assert.Empty(t, dealRun(t, "report", "--deal", same["deal_id"].(string))["anomalies"], "the same single skill: nothing to flag")

	require.NoError(t, os.WriteFile(skill, []byte(freshSkill+"\nA changed line.\n"), 0o600))
	*now = now.Add(time.Hour)
	t.Setenv(dealSkillEnv, skill)
	changed := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))
	assert.NotEqual(t, first["skill"].(map[string]any)["skill_md_digest"], changed["skill"].(map[string]any)["skill_md_digest"], "a modified SKILL.md seals a different digest")
	assert.Contains(t, anomalyKinds(dealRun(t, "report", "--deal", changed["deal_id"].(string))), "skill_changed")
}

func TestDealWithoutSkillSaysSo(t *testing.T) {
	dealFixture(t)
	dealID := dealRun(t, "open", "--input", filepath.Join(retailDemo, "open.json"))["deal_id"].(string)
	report := dealRun(t, "report", "--deal", dealID)
	assert.Equal(t, "Which skill instructions were present was not recorded (the deal was opened without --skill).", report["instructions"])
	for _, k := range anomalyKinds(report) {
		assert.False(t, strings.HasPrefix(k, "skill_"), "no skill record is not an anomaly")
	}
	_, err := invoke(t, "", "--profile", "deal", "deal", "open", "--skill", filepath.Join(t.TempDir(), "missing.md"), "--input", filepath.Join(retailDemo, "open.json"))
	require.ErrorIs(t, err, ErrInput)
}
