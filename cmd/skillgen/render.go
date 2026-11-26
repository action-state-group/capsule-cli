package main

import (
	"fmt"
	"strings"
)

// boundaryPreamble is fixed template prose, not spec data: every skill this
// generator renders is the thin, tool-calling shape (see skills/SKILL-SPEC.md),
// so the boundary statement is identical for all of them and does not belong
// in the per-skill spec.
const boundaryPreamble = "This skill may call verbs, validate a contract, request evidence, verify a bundle, or open a result — nothing else. It never reasons about business meaning, and never invents a workflow beyond the verb surface below."

func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func guardrailList(label string, items []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s:**\n\n", label)
	if len(items) == 0 {
		b.WriteString("_(none declared)_\n")
		return b.String()
	}
	for _, item := range items {
		fmt.Fprintf(&b, "- %s\n", item)
	}
	return b.String()
}

func guardrailsSection(spec Spec) string {
	var b strings.Builder
	b.WriteString(guardrailList("May", spec.May))
	b.WriteString("\n")
	b.WriteString(guardrailList("Must never", spec.MustNever))
	b.WriteString("\n")
	b.WriteString(guardrailList("Approval points", spec.ApprovalPoints))
	b.WriteString("\n")
	fmt.Fprintf(&b, "**Evidence policy:**\n\n%s\n", spec.EvidencePolicy)
	return b.String()
}

func emissionModeLabel(mode string) string {
	switch mode {
	case "self-sealing":
		return "self-sealing (`--seal-output`)"
	case "primary-action":
		return "primary action (running it *is* the record)"
	case "not-consequential":
		return "not consequential — no capsule"
	default:
		return mode
	}
}

// verbNames renders names as a comma-separated list of code spans, in spec
// order; the prose below computes its lists from the spec with it, so a verb
// moved between lists or modes never leaves a stale name behind.
func verbNames(verbs []Verb) string {
	names := make([]string, len(verbs))
	for i, v := range verbs {
		names[i] = "`" + v.Name + "`"
	}
	return strings.Join(names, ", ")
}

func verbsWithMode(verbs []Verb, modes ...string) []Verb {
	var out []Verb
	for _, v := range verbs {
		for _, m := range modes {
			if v.EmissionMode == m {
				out = append(out, v)
			}
		}
	}
	return out
}

func allVerbs(spec Spec) []Verb {
	return append(append([]Verb{}, spec.Verbs...), spec.ExpertVerbs...)
}

func verbRow(b *strings.Builder, lead string, v Verb) {
	args := "_(none)_"
	if len(v.Args) > 0 {
		escaped := make([]string, len(v.Args))
		for i, a := range v.Args {
			escaped[i] = "`" + escapeCell(a) + "`"
		}
		args = strings.Join(escaped, "<br>")
	}
	fmt.Fprintf(b, "| %s`%s` | %s | %s | %s | %s |\n",
		lead, escapeCell(v.Name), args, escapeCell(v.When), escapeCell(v.Emits), emissionModeLabel(v.EmissionMode))
}

// fiveVerbTable lists the taught verbs, each under the one of the five verbs
// it carries out, in spec order.
func fiveVerbTable(verbs []Verb) string {
	var b strings.Builder
	b.WriteString("| Verb | Command | Args | When | Emits | How |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, v := range verbs {
		verbRow(&b, v.FiveVerb+" | ", v)
	}
	return b.String()
}

func expertTable(verbs []Verb) string {
	var b strings.Builder
	b.WriteString("| Command | Args | When | Emits | How |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, v := range verbs {
		verbRow(&b, "", v)
	}
	return b.String()
}

func fiveVerbsSection(spec Spec) string {
	var b strings.Builder
	b.WriteString("This skill teaches five verbs and nothing else: **check** before you act,\n" +
		"**record** what was done, **ask** for evidence or answer a request, **verify**\n" +
		"what you get, and **show** a record or report. Each row maps a verb to the\n" +
		"command that carries it out.\n\n")
	b.WriteString(fiveVerbTable(spec.Verbs))
	if spec.FiveVerbsElsewhere != "" {
		b.WriteString("\n" + spec.FiveVerbsElsewhere + "\n")
	}
	if len(spec.ExpertVerbs) > 0 {
		fmt.Fprintf(&b, "\n**Expert verbs.** %s are verbs of this file too, but are\n"+
			"not taught here: read [references/expert.md](references/expert.md), which\n"+
			"keeps each one's instructions, before you call one. The guardrails above bind\n"+
			"them as they bind the five.\n", verbNames(spec.ExpertVerbs))
	}
	return b.String()
}

func emissionSection(spec Spec) string {
	all := allVerbs(spec)
	return `**Capsules are for consequential actions only** (see Evidence policy
under Guardrails, above). A capsule is never manufactured for ` + "`verify`" + `
or any other local validation — reading, checking, validating or opening
something is not a consequential action, and this skill produces no capsule
for it. The How column above, and in [references/expert.md](references/expert.md),
says which of the two real mechanisms applies to an actually-consequential verb:

- **self-sealing** — the verb has its own ` + "`--seal-output`" + ` flag (` + verbNames(verbsWithMode(all, "self-sealing")) + `,
  mandated by the binary itself). Nothing further is needed.
- **primary action** — the verb's entire job already is creating or persisting
  a capsule (` + verbNames(verbsWithMode(all, "primary-action")) + `). Running it
  satisfies the invariant by itself; nothing wraps it.

Every other verb this skill calls (` + verbNames(verbsWithMode(all, "not-consequential")) + `)
is ` + "`not-consequential`" + `: it runs, and
that is the end of it — **no wrapper seal, no synthetic evidence record.**

See [references/capsule-cli.md](references/capsule-cli.md) for the ` + "`publish`" + `/
` + "`cll append`" + ` request shape and the Chain/Relation fields, and for the bare
` + "`capsulectl seal`" + ` command this skill does not use (it exists for a skill
whose verb surface actually needs it — this one does not).`
}

func exerciseSection(spec Spec) string {
	all := allVerbs(spec)
	return `[scripts/run-scripted-demo.sh](scripts/run-scripted-demo.sh) runs every
verb above and in [references/expert.md](references/expert.md) against a
throwaway sqlite profile in a temp directory: the
consequential actions (` + verbNames(verbsWithMode(all, "self-sealing", "primary-action")) + `) each seal or persist a capsule and every one of those is
verified; the not-consequential
actions (` + verbNames(verbsWithMode(all, "not-consequential")) + `)
run and produce no capsule at all — the script asserts both halves, not just
that capsules exist. It never touches a real profile or a real CLL. It is a
contributor check, run after any change to the verb surface or the emission
mechanism: it builds ` + "`capsulectl`" + ` from a source checkout, so it needs Go
and this repository. Using the skill needs neither: install a release binary
and this skill from the same tag (the repository README, "Agent skill").`
}

func referenceSection(spec Spec) string {
	expert := ""
	if len(spec.ExpertVerbs) > 0 {
		expert = `- [Expert verbs](references/expert.md) — the verbs this skill may call
  but does not teach, each with its instructions.
`
	}
	return expert + `- [Capsule CLI contract](references/capsule-cli.md) — the full command
  reference this skill is built against.
- [skills/SKILL-SPEC.md](https://github.com/action-state-group/capsule-cli/blob/main/skills/SKILL-SPEC.md) — the spec format this skill's
  ` + "`spec.yaml`" + ` is written against, and the regeneration contract that
  keeps this file and ` + "`AGENTS.md`" + ` byte-identical to what it renders.`
}

// body is the part SKILL.md and AGENTS.md share after their wrappers.
func body(spec Spec) string {
	var b strings.Builder
	b.WriteString(boundaryPreamble)
	b.WriteString("\n\n## Guardrails\n\n")
	b.WriteString(guardrailsSection(spec))
	b.WriteString("\n## The five verbs\n\n")
	b.WriteString(fiveVerbsSection(spec))
	b.WriteString("\n## Capsule emission\n\n")
	b.WriteString(emissionSection(spec))
	b.WriteString("\n\n## Exercise this skill\n\n")
	b.WriteString(exerciseSection(spec))
	b.WriteString("\n\n## Reference\n\n")
	b.WriteString(referenceSection(spec))
	b.WriteString("\n")
	return b.String()
}

// renderExpertMD is the generated references/expert.md: the expert verbs with
// their instructions, the same columns as the five-verb table minus the verb.
// It returns "" when the spec has no expert verbs, and run then writes no file.
func renderExpertMD(spec Spec) string {
	if len(spec.ExpertVerbs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Expert verbs\n\n")
	fmt.Fprintf(&b, `The %s skill ([SKILL.md](../SKILL.md); [AGENTS.md](../AGENTS.md) for
Codex) teaches five verbs: check, record, ask, verify and show. The verbs here
sit outside the five; their instructions are kept as they were taught before.
The skill's guardrails bind them, and its approval points still stop you
before the actions they name. This file is generated from `+"`spec.yaml`"+`
(`+"`expert_verbs`"+`); never edit it by hand.

`, spec.Skill.Name)
	b.WriteString(expertTable(spec.ExpertVerbs))
	b.WriteString(`
[capsule-cli.md](capsule-cli.md) gives the full contract of the commands it
documents, with their output and exit codes; ` + "`capsulectl <command> --help`" + `
gives every flag of each.
`)
	return b.String()
}

// renderSkillMD is the Claude Code shape: YAML frontmatter (name,
// description only, matching this workspace's other installed skills), then
// the shared body sections.
func renderSkillMD(spec Spec) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n# %s\n\n", spec.Skill.Name, spec.Skill.Description, spec.Skill.Name) + body(spec)
}

// renderAgentsMD is the Codex shape: no YAML frontmatter, a plain heading
// carrying the same name/description, then the identical shared body
// sections renderSkillMD uses — the two differ only in the wrapper, so a
// spec edit changes both files together.
func renderAgentsMD(spec Spec) string {
	return fmt.Sprintf("# %s agent skill\n\n%s\n\n", spec.Skill.Name, spec.Skill.Description) + body(spec)
}
