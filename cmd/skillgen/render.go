package main

import (
	"fmt"
	"strings"
)

// boundaryPreamble is fixed template prose, not spec data: every skill this
// generator renders is the thin, tool-calling shape (see skills/SKILL-SPEC.md),
// so the boundary statement is identical for all of them and does not belong
// in the per-skill spec.
const boundaryPreamble = "This skill may call verbs, validate a contract, request evidence, verify a bundle, or inspect a result — nothing else. It never reasons about business meaning, and never invents a workflow beyond the verb surface below."

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
	return b.String()
}

func emissionModeLabel(mode string) string {
	switch mode {
	case "self-sealing":
		return "self-sealing (`--seal-output`)"
	case "primary-action":
		return "primary action (running it *is* the record)"
	case "skill-wraps":
		return "skill wraps with `capsulectl seal`"
	default:
		return mode
	}
}

func verbTable(verbs []Verb) string {
	var b strings.Builder
	b.WriteString("| Verb | Args | When | Emits | How |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, v := range verbs {
		args := "_(none)_"
		if len(v.Args) > 0 {
			escaped := make([]string, len(v.Args))
			for i, a := range v.Args {
				escaped[i] = "`" + escapeCell(a) + "`"
			}
			args = strings.Join(escaped, "<br>")
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
			escapeCell(v.Name), args, escapeCell(v.When), escapeCell(v.Emits), emissionModeLabel(v.EmissionMode))
	}
	return b.String()
}

func emissionSection() string {
	return `Every verb this skill calls leaves a capsule, by one of three mechanisms
(the ` + "`emission_mode`" + ` column above says which applies):

- **self-sealing** — the verb has its own ` + "`--seal-output`" + ` flag (` + "`discover`" + `).
  Nothing further is needed.
- **primary action** — the verb's entire job already is creating or chaining
  a capsule (` + "`publish`" + `, ` + "`cll append`" + `). Running it satisfies the
  invariant by itself.
- **skill wraps** — the verb only reads or checks something and has no seal of
  its own. After running it, build a ` + "`capsule-seal-request/v1`" + ` whose
  ` + "`payload`" + ` is the verb's own JSON result, add a ` + "`Chain`" + ` block pointing
  at the previous action's ` + "`capsule_id`" + ` in this same run (when there is
  one), and call:

  ` + "```sh" + `
  capsulectl seal --profile NAME --request REQUEST.json --output CAPSULE.json
  ` + "```" + `

  This needs no store connection and no CLL append; it produces a file
  independently verifiable with ` + "`capsulectl verify`" + `. Append it to a CLL
  later with ` + "`cll append`" + ` only when durable persistence is actually wanted.

See [references/capsule-cli.md](references/capsule-cli.md) for the full
request shape and the Chain/Relation fields.`
}

func exerciseSection() string {
	return `[scripts/run-scripted-demo.sh](scripts/run-scripted-demo.sh) runs every
verb above against a throwaway jsonl profile in a temp directory, seals or
publishes one capsule per action, verifies each one, and asserts the count
matches the verb surface — the "fresh-environment" test this skill ships
with. It never touches a real profile or a real CLL. Run it after any change
to the verb surface or the emission mechanism; it builds ` + "`capsulectl`" + ` from
source, so its own elapsed time is also this skill's install-through-first-
use timing.`
}

func referenceSection() string {
	return `- [Capsule CLI contract](references/capsule-cli.md) — the full command
  reference this skill is built against.
- [skills/SKILL-SPEC.md](../SKILL-SPEC.md) — the spec format this skill's
  ` + "`spec.yaml`" + ` is written against, and the regeneration contract that
  keeps this file and ` + "`AGENTS.md`" + ` byte-identical to what it renders.`
}

// renderSkillMD is the Claude Code shape: YAML frontmatter (name,
// description only, matching this workspace's other installed skills), then
// the shared body sections.
func renderSkillMD(spec Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n---\n\n", spec.Skill.Name, spec.Skill.Description)
	fmt.Fprintf(&b, "# %s\n\n", spec.Skill.Name)
	b.WriteString(boundaryPreamble)
	b.WriteString("\n\n## Guardrails\n\n")
	b.WriteString(guardrailsSection(spec))
	b.WriteString("\n## Verb surface\n\n")
	b.WriteString(verbTable(spec.Verbs))
	b.WriteString("\n## Capsule emission\n\n")
	b.WriteString(emissionSection())
	b.WriteString("\n\n## Exercise this skill\n\n")
	b.WriteString(exerciseSection())
	b.WriteString("\n\n## Reference\n\n")
	b.WriteString(referenceSection())
	b.WriteString("\n")
	return b.String()
}

// renderAgentsMD is the Codex shape: no YAML frontmatter, a plain heading
// carrying the same name/description, then the identical shared body
// sections renderSkillMD uses — the two differ only in the wrapper, so a
// spec edit changes both files together.
func renderAgentsMD(spec Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s agent skill\n\n%s\n\n", spec.Skill.Name, spec.Skill.Description)
	b.WriteString(boundaryPreamble)
	b.WriteString("\n\n## Guardrails\n\n")
	b.WriteString(guardrailsSection(spec))
	b.WriteString("\n## Verb surface\n\n")
	b.WriteString(verbTable(spec.Verbs))
	b.WriteString("\n## Capsule emission\n\n")
	b.WriteString(emissionSection())
	b.WriteString("\n\n## Exercise this skill\n\n")
	b.WriteString(exerciseSection())
	b.WriteString("\n\n## Reference\n\n")
	b.WriteString(referenceSection())
	b.WriteString("\n")
	return b.String()
}
