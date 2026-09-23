package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	specPath := flag.String("spec", "skills/capsulectl/spec.yaml", "skill-spec/v0 YAML source")
	schemaPath := flag.String("schema", "schemas/skill-spec-v0.json", "skill-spec/v0 JSON Schema")
	skillMDPath := flag.String("skill-md", "skills/capsulectl/SKILL.md", "rendered Claude Code skill file")
	agentsMDPath := flag.String("agents-md", "skills/capsulectl/AGENTS.md", "rendered Codex skill file")
	check := flag.Bool("check", false, "fail if the rendered files on disk differ from what the spec renders now, instead of writing them")
	flag.Parse()

	if err := run(*specPath, *schemaPath, *skillMDPath, *agentsMDPath, *check); err != nil {
		fmt.Fprintln(os.Stderr, "skillgen:", err)
		os.Exit(1)
	}
}

func run(specPath, schemaPath, skillMDPath, agentsMDPath string, check bool) error {
	spec, err := loadSpec(specPath, schemaPath)
	if err != nil {
		return err
	}
	skillMD := renderSkillMD(spec)
	agentsMD := renderAgentsMD(spec)

	if check {
		return checkRendered(map[string]string{
			skillMDPath:  skillMD,
			agentsMDPath: agentsMD,
		})
	}
	if err := os.WriteFile(skillMDPath, []byte(skillMD), 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", skillMDPath, err)
	}
	if err := os.WriteFile(agentsMDPath, []byte(agentsMD), 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", agentsMDPath, err)
	}
	fmt.Printf("wrote %s and %s from %s\n", skillMDPath, agentsMDPath, specPath)
	return nil
}

// checkRendered fails loudly, naming every stale or missing file, rather than
// stopping at the first mismatch — a hand-edit to either rendered file must
// be caught in the same CI run, not one bounce at a time.
func checkRendered(wantByPath map[string]string) error {
	var stale []string
	for path, want := range wantByPath {
		got, err := os.ReadFile(path)
		if err != nil {
			stale = append(stale, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if string(got) != want {
			stale = append(stale, fmt.Sprintf("%s does not match what the spec renders — regenerate, do not hand-edit", path))
		}
	}
	if len(stale) > 0 {
		msg := "stale generated file(s):"
		for _, s := range stale {
			msg += "\n  - " + s
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
