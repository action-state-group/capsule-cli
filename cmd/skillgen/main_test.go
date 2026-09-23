package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixtureSchemaPath = "../../schemas/skill-spec-v0.json"

func minimalSpecYAML(description string) string {
	return `spec_version: skill-spec/v0
skill:
  name: example
  description: ` + description + `
may: ["TBD-STEVEN"]
must_never: ["TBD-STEVEN"]
approval_points: []
evidence_policy: "TBD-STEVEN"
verbs:
  - name: verify
    args: ["--profile NAME", "--capsule FILE"]
    when: Confirm a Capsule before trusting it.
    emits: (no capsule — local validation)
    emission_mode: not-consequential
  - name: publish
    args: ["--profile NAME --request FILE"]
    when: Persist a sealed Capsule durably.
    emits: (publish's own result IS the capsule)
    emission_mode: primary-action
`
}

func writeSpec(t *testing.T, dir, description string) string {
	t.Helper()
	path := filepath.Join(dir, "spec.yaml")
	require.NoError(t, os.WriteFile(path, []byte(minimalSpecYAML(description)), 0o600))
	return path
}

// TestRenderIsDeterministic pins the byte-stable regeneration contract
// skills/SKILL-SPEC.md promises: the same spec renders identical bytes on
// every run, so a checked-in SKILL.md/AGENTS.md pair either matches exactly
// or the CI --check gate below must catch the drift -- there is no
// "close enough".
func TestRenderIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	specPath := writeSpec(t, dir, "One line description.")
	spec, err := loadSpec(specPath, fixtureSchemaPath)
	require.NoError(t, err)

	skillA, agentsA := renderSkillMD(spec), renderAgentsMD(spec)
	spec2, err := loadSpec(specPath, fixtureSchemaPath)
	require.NoError(t, err)
	skillB, agentsB := renderSkillMD(spec2), renderAgentsMD(spec2)

	assert.Equal(t, skillA, skillB)
	assert.Equal(t, agentsA, agentsB)
}

// TestEditingSpecChangesBothRenderedFiles is the first half of the
// regeneration contract's acceptance check: edit spec -> both files change.
func TestEditingSpecChangesBothRenderedFiles(t *testing.T) {
	dir := t.TempDir()
	before, err := loadSpec(writeSpec(t, dir, "Before description."), fixtureSchemaPath)
	require.NoError(t, err)
	after, err := loadSpec(writeSpec(t, dir, "After description."), fixtureSchemaPath)
	require.NoError(t, err)

	assert.NotEqual(t, renderSkillMD(before), renderSkillMD(after))
	assert.NotEqual(t, renderAgentsMD(before), renderAgentsMD(after))
}

// TestCheckFailsOnHandEdit is the second half: edit a rendered file by hand
// -> --check fails, rather than silently accepting the drift.
func TestCheckFailsOnHandEdit(t *testing.T) {
	dir := t.TempDir()
	specPath := writeSpec(t, dir, "Some description.")
	spec, err := loadSpec(specPath, fixtureSchemaPath)
	require.NoError(t, err)
	skillMD, agentsMD := renderSkillMD(spec), renderAgentsMD(spec)

	skillPath := filepath.Join(dir, "SKILL.md")
	agentsPath := filepath.Join(dir, "AGENTS.md")
	require.NoError(t, os.WriteFile(skillPath, []byte(skillMD), 0o600))
	require.NoError(t, os.WriteFile(agentsPath, []byte(agentsMD), 0o600))

	// Freshly generated: check passes.
	require.NoError(t, checkRendered(map[string]string{skillPath: skillMD, agentsPath: agentsMD}))

	// A human hand-edits the generated file directly.
	require.NoError(t, os.WriteFile(skillPath, []byte(skillMD+"\nhand-added line\n"), 0o600))
	err = checkRendered(map[string]string{skillPath: skillMD, agentsPath: agentsMD})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SKILL.md")
	assert.Contains(t, err.Error(), "regenerate, do not hand-edit")

	// A missing rendered file is caught the same way, not silently created.
	require.NoError(t, os.Remove(agentsPath))
	err = checkRendered(map[string]string{skillPath: skillMD, agentsPath: agentsMD})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AGENTS.md")
}

// TestSchemaRejectsUnknownFieldAndMissingVerb exercises the
// additionalProperties:false / required[] gate a spec author actually hits:
// a stray field or an omitted verb key fails validation before rendering
// ever runs, instead of silently dropping data.
func TestSchemaRejectsUnknownFieldAndMissingVerb(t *testing.T) {
	dir := t.TempDir()
	badField := filepath.Join(dir, "bad-field.yaml")
	require.NoError(t, os.WriteFile(badField, []byte(strings.Replace(minimalSpecYAML("d"), "spec_version: skill-spec/v0", "spec_version: skill-spec/v0\nextra_top_level_field: nope", 1)), 0o600))
	_, err := loadSpec(badField, fixtureSchemaPath)
	require.Error(t, err)

	badVerb := filepath.Join(dir, "bad-verb.yaml")
	require.NoError(t, os.WriteFile(badVerb, []byte(strings.Replace(minimalSpecYAML("d"), "emission_mode: not-consequential", "", 1)), 0o600))
	_, err = loadSpec(badVerb, fixtureSchemaPath)
	require.Error(t, err)

	badEnum := filepath.Join(dir, "bad-enum.yaml")
	require.NoError(t, os.WriteFile(badEnum, []byte(strings.Replace(minimalSpecYAML("d"), "emission_mode: not-consequential", "emission_mode: not-a-real-mode", 1)), 0o600))
	_, err = loadSpec(badEnum, fixtureSchemaPath)
	require.Error(t, err)
}

// TestCapsulectlSkillMatchesItsSpec is the real CI gate this item's DONE
// depends on: the checked-in skills/capsulectl/{SKILL.md,AGENTS.md} must be
// exactly what skills/capsulectl/spec.yaml renders right now. Run
// `go run ./cmd/skillgen` from the repo root to regenerate after any edit to
// the spec, and never hand-edit either rendered file.
func TestCapsulectlSkillMatchesItsSpec(t *testing.T) {
	spec, err := loadSpec("../../skills/capsulectl/spec.yaml", fixtureSchemaPath)
	require.NoError(t, err)
	require.NoError(t, checkRendered(map[string]string{
		"../../skills/capsulectl/SKILL.md":  renderSkillMD(spec),
		"../../skills/capsulectl/AGENTS.md": renderAgentsMD(spec),
	}))
}
