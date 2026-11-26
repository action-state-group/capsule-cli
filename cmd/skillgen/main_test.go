package main

import (
	"os"
	"path/filepath"
	"regexp"
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
may: ["TBD-HUMAN"]
must_never: ["TBD-HUMAN"]
approval_points: []
evidence_policy: "TBD-HUMAN"
verbs:
  - name: verify
    five_verb: verify
    args: ["--profile NAME", "--capsule FILE"]
    when: Confirm a Capsule before trusting it.
    emits: (no capsule — local validation)
    emission_mode: not-consequential
  - name: publish
    five_verb: record
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

// TestCapsulectlSkillMatchesItsSpec is the CI gate for the generated skill:
// the checked-in skills/capsulectl/{SKILL.md,AGENTS.md} must be
// exactly what skills/capsulectl/spec.yaml renders right now. Run
// `go run ./cmd/skillgen` from the repo root to regenerate after any edit to
// the spec, and never hand-edit either rendered file.
func TestCapsulectlSkillMatchesItsSpec(t *testing.T) {
	spec, err := loadSpec("../../skills/capsulectl/spec.yaml", fixtureSchemaPath)
	require.NoError(t, err)
	rendered := renderedFiles(spec, skillDir+"/SKILL.md", skillDir+"/AGENTS.md", skillDir+"/references/expert.md")
	require.Len(t, rendered, 3, "the capsulectl spec has expert verbs, so references/expert.md is generated too")
	require.NoError(t, checkRendered(rendered))
}

const skillDir = "../../skills/capsulectl"

// TestSchemaHoldsTheFiveVerbSplit: a taught verb must name one of the five
// verbs, and an expert verb must not, so a verb cannot sit in both roles.
func TestSchemaHoldsTheFiveVerbSplit(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"taught verb without five_verb": strings.Replace(minimalSpecYAML("d"), "    five_verb: verify\n", "", 1),
		"five_verb outside the five":    strings.Replace(minimalSpecYAML("d"), "five_verb: verify", "five_verb: discover", 1),
		"expert verb with five_verb": minimalSpecYAML("d") + `expert_verbs:
  - name: plugin ls
    five_verb: show
    args: []
    when: List plugins.
    emits: (no capsule)
    emission_mode: not-consequential
`,
	}
	for name, doc := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".yaml")
		require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
		_, err := loadSpec(path, fixtureSchemaPath)
		assert.Error(t, err, name)
	}

	// Without expert verbs no reference is rendered, so none is written or
	// checked, and nothing links to it.
	spec, err := loadSpec(writeSpec(t, dir, "d"), fixtureSchemaPath)
	require.NoError(t, err)
	assert.Len(t, renderedFiles(spec, "S", "A", "E"), 2)
	assert.NotContains(t, renderSkillMD(spec), "expert.md")
	assert.NotContains(t, renderAgentsMD(spec), "expert.md")
}

// TestCapsulectlSkillTeachesOnlyTheFiveVerbs holds issue #650's split: the
// skill file's table teaches only
// the six core commands, and the expert verbs keep their instructions in
// references/expert.md, named but not taught in the skill file.
func TestCapsulectlSkillTeachesOnlyTheFiveVerbs(t *testing.T) {
	spec, err := loadSpec(skillDir+"/spec.yaml", fixtureSchemaPath)
	require.NoError(t, err)

	taught := map[string]string{}
	for _, v := range spec.Verbs {
		taught[v.Name] = v.FiveVerb
	}
	assert.Equal(t, map[string]string{
		"publish": "record", "cll append": "record", "result build": "record",
		"verify": "verify", "cll list": "show", "get": "show",
	}, taught)
	var expert []string
	for _, v := range spec.ExpertVerbs {
		expert = append(expert, v.Name)
	}
	assert.ElementsMatch(t, []string{
		"contract validate", "contract diff", "discover", "plugin ls",
		"judge pin", "judge drift pin", "judge drift reports", "calibration summarize",
	}, expert)

	expertMD := readFile(t, skillDir+"/references/expert.md")
	for _, file := range []string{"SKILL.md", "AGENTS.md"} {
		text := readFile(t, skillDir+"/"+file)
		section := between(t, text, "## The five verbs\n", "\n## Capsule emission")
		var rows []string
		for _, line := range strings.Split(section, "\n") {
			if strings.HasPrefix(line, "| ") && !strings.HasPrefix(line, "| Verb ") {
				cells := strings.Split(line, " | ")
				rows = append(rows, cells[0][2:]+" "+strings.Trim(cells[1], "`"))
			}
		}
		// The emission section teaches the five verbs' mechanisms only; an
		// expert verb's (discover's --seal-output, say) is in the reference.
		emission := between(t, text, "## Capsule emission\n", "\n## Exercise this skill")
		assert.NotContains(t, emission, "--seal-output", "%s teaches an expert verb's flag", file)
		for _, v := range spec.ExpertVerbs {
			assert.NotContains(t, emission, "`"+v.Name+"`", "%s: emission section names expert verb %q", file, v.Name)
			assert.Contains(t, expertMD, "`"+v.Name+"`")
		}
		assert.Contains(t, expertMD, "self-sealing** — the verb has its own `--seal-output` flag (`discover`")

		var want []string
		for _, v := range spec.Verbs {
			want = append(want, v.FiveVerb+" "+v.Name)
		}
		assert.Equal(t, want, rows, "%s: the five-verb table", file)

		for _, v := range spec.ExpertVerbs {
			assert.Contains(t, section, "`"+v.Name+"`", "%s names expert verb %q, as the first may item needs", file, v.Name)
			assert.NotContains(t, text, escapeCell(v.When), "%s teaches expert verb %q", file, v.Name)
			assert.Contains(t, expertMD, escapeCell(v.When), "references/expert.md lost %q's instructions", v.Name)
			for _, a := range v.Args {
				assert.Contains(t, expertMD, "`"+escapeCell(a)+"`", "references/expert.md lost %q's args", v.Name)
			}
		}
	}
}

var mdLink = regexp.MustCompile(`\]\(([^)]+)\)`)

// TestCapsulectlSkillLinksResolve: the skill is installed by copying
// skills/capsulectl alone (README, "Agent skill"), so every relative link in
// its Markdown must resolve inside that directory.
func TestCapsulectlSkillLinksResolve(t *testing.T) {
	root, err := filepath.Abs(skillDir)
	require.NoError(t, err)
	files, err := filepath.Glob(filepath.Join(root, "*.md"))
	require.NoError(t, err)
	refs, err := filepath.Glob(filepath.Join(root, "references", "*.md"))
	require.NoError(t, err)
	files = append(files, refs...)
	require.NotEmpty(t, refs)
	for _, file := range files {
		for _, m := range mdLink.FindAllStringSubmatch(readFile(t, file), -1) {
			target := strings.SplitN(m[1], "#", 2)[0]
			if target == "" || strings.Contains(target, "://") {
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(file), target))
			rel, err := filepath.Rel(root, resolved)
			require.NoError(t, err)
			assert.False(t, strings.HasPrefix(rel, ".."), "%s links outside the skill directory: %s", file, m[1])
			_, err = os.Stat(resolved)
			assert.NoError(t, err, "%s: broken link %s", file, m[1])
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	require.GreaterOrEqual(t, i, 0, "missing %q", start)
	j := strings.Index(s[i:], end)
	require.GreaterOrEqual(t, j, 0, "missing %q", end)
	return s[i : i+j]
}
