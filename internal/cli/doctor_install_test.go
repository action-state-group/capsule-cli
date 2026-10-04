package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dealSkillText = "---\nname: deal\ndescription: test\n---\n# deal\n"

func skillsDir(t *testing.T, skills map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, text := range skills {
		path := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	}
	return dir
}

func releaseBuild(t *testing.T, version, commit string) {
	t.Helper()
	oldV, oldC := cliVersion, cliCommit
	cliVersion, cliCommit = version, commit
	t.Cleanup(func() { cliVersion, cliCommit = oldV, oldC })
}

func witnessedDealProfile(t *testing.T) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cadenceFixture(t, "https://witness.example", pub, "1h", "10m", 1)
}

func installCheckRun(t *testing.T, dir string, extra ...string) (map[string]any, string, error) {
	t.Helper()
	args := append([]string{"doctor", "--install-check", "--profile", "deal", "--expect-version", "v0.1.0-rc3", "--skills-dir", dir}, extra...)
	out, err := invoke(t, "", args...)
	var m struct {
		Check map[string]any `json:"install_check"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m.Check, out, err
}

func TestInstallCheckPassesAFreshInstall(t *testing.T) {
	witnessedDealProfile(t)
	releaseBuild(t, "v0.1.0-rc3", "abc1234def")
	dir := skillsDir(t, map[string]string{
		"deal/SKILL.md":     dealSkillText,
		"calendar/SKILL.md": "---\nname: calendar\n---\n",
		"notes/README.md":   "not a skill",
	})
	check, out, err := installCheckRun(t, dir, "--expect-commit", "abc1234")
	require.NoError(t, err, out)
	assert.Equal(t, 1, strings.Count(out, "\n"), "one JSON line, for the run to seal")
	assert.Equal(t, true, check["ok"])
	assert.Equal(t, []any{}, check["issues"])
	assert.Equal(t, []any{filepath.Join("deal", "SKILL.md")}, check["deal_skills"])
	sum := sha256.Sum256([]byte(dealSkillText))
	assert.Equal(t, hex.EncodeToString(sum[:]), check["skill_sha256"])
	assert.Equal(t, map[string]any{"endpoint_set": true, "public_key_ok": true}, check["profile_witness"])
}

func TestInstallCheckFindsABackupSkillLeftInTheSkillsDir(t *testing.T) {
	witnessedDealProfile(t)
	releaseBuild(t, "v0.1.0-rc3", "abc1234def")
	dir := skillsDir(t, map[string]string{
		"deal/SKILL.md":              dealSkillText,
		"deal.bak-20261003/SKILL.md": dealSkillText,
	})
	check, _, err := installCheckRun(t, dir)
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, 3, ExitCode(err))
	assert.Equal(t, false, check["ok"])
	assert.Equal(t, []any{
		"more than one deal skill under " + dir + ": deal/SKILL.md, deal.bak-20261003/SKILL.md; move every copy but the installed one out of the skills directory",
	}, check["issues"])
	assert.Nil(t, check["skill_sha256"], "no digest when it is not clear which skill runs")
}

func TestInstallCheckFindsADriftedBinary(t *testing.T) {
	witnessedDealProfile(t)
	releaseBuild(t, "v0.1.0-rc2", "0ld0ld0")
	dir := skillsDir(t, map[string]string{"deal/SKILL.md": dealSkillText})
	check, _, err := installCheckRun(t, dir, "--expect-commit", "abc1234")
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, []any{
		"binary is v0.1.0-rc2, expected v0.1.0-rc3",
		"binary commit is 0ld0ld0, expected abc1234",
	}, check["issues"])
}

func TestInstallCheckFindsASkillBehindTheRelease(t *testing.T) {
	witnessedDealProfile(t)
	releaseBuild(t, "v0.1.0-rc3", "abc1234def")
	dir := skillsDir(t, map[string]string{"deal/SKILL.md": dealSkillText})
	sum := sha256.Sum256([]byte(dealSkillText))
	_, out, err := installCheckRun(t, dir, "--expect-skill-sha256", strings.ToUpper(hex.EncodeToString(sum[:])))
	require.NoError(t, err, out)

	check, _, err := installCheckRun(t, dir, "--expect-skill-sha256", strings.Repeat("ab", 32))
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, []any{"the deal skill deal/SKILL.md has sha256 " + hex.EncodeToString(sum[:]) + ", expected " + strings.Repeat("ab", 32)}, check["issues"])
}

func TestInstallCheckFindsAWitnessWithoutItsKey(t *testing.T) {
	witnessedDealProfile(t)
	releaseBuild(t, "v0.1.0-rc3", "abc1234def")
	p, err := loadProfile("deal")
	require.NoError(t, err)
	p.Checkpoint.PublicKey = ""
	require.NoError(t, saveProfile(p, true))
	dir := skillsDir(t, map[string]string{"deal/SKILL.md": dealSkillText})
	check, _, err := installCheckRun(t, dir)
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, map[string]any{"endpoint_set": true, "public_key_ok": false}, check["profile_witness"])
	issues := check["issues"].([]any)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0], "checkpoint.public_key")
}

func TestInstallCheckFindsAProfileWithNoWitness(t *testing.T) {
	dealFixture(t) // no witness
	releaseBuild(t, "v0.1.0-rc3", "abc1234def")
	dir := skillsDir(t, map[string]string{"deal/SKILL.md": dealSkillText})
	check, _, err := installCheckRun(t, dir)
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, []any{"profile deal has no checkpoint.endpoint: nothing it seals can be witnessed"}, check["issues"])
}

func TestInstallCheckNeedsItsInputs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := invoke(t, "", "doctor", "--install-check", "--profile", "deal")
	require.ErrorIs(t, err, ErrInput)
}
