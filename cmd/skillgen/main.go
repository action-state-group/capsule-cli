package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	specPath := flag.String("spec", "skills/capsulectl/spec.yaml", "skill-spec/v0 YAML source")
	schemaPath := flag.String("schema", "schemas/skill-spec-v0.json", "skill-spec/v0 JSON Schema")
	skillMDPath := flag.String("skill-md", "skills/capsulectl/SKILL.md", "rendered Claude Code skill file")
	agentsMDPath := flag.String("agents-md", "skills/capsulectl/AGENTS.md", "rendered Codex skill file")
	expertMDPath := flag.String("expert-md", "skills/capsulectl/references/expert.md", "rendered expert-verbs reference, written only when the spec has expert_verbs")
	check := flag.Bool("check", false, "fail if the rendered files on disk differ from what the spec renders now, instead of writing them")
	flag.Parse()

	if err := run(*specPath, *schemaPath, *skillMDPath, *agentsMDPath, *expertMDPath, *check); err != nil {
		fmt.Fprintln(os.Stderr, "skillgen:", err)
		os.Exit(1)
	}
}

func run(specPath, schemaPath, skillMDPath, agentsMDPath, expertMDPath string, check bool) error {
	spec, err := loadSpec(specPath, schemaPath)
	if err != nil {
		return err
	}
	rendered := renderedFiles(spec, skillMDPath, agentsMDPath, expertMDPath)
	if check {
		return checkRendered(rendered)
	}
	paths := make([]string, 0, len(rendered))
	for path := range rendered {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(rendered[path]), 0o644); err != nil {
			return fmt.Errorf("cannot write %s: %w", path, err)
		}
	}
	fmt.Printf("wrote %s from %s\n", strings.Join(paths, ", "), specPath)
	return nil
}

// renderedFiles maps each generated file's path to what the spec renders for
// it. The expert reference is included only when the spec has expert verbs.
func renderedFiles(spec Spec, skillMDPath, agentsMDPath, expertMDPath string) map[string]string {
	files := map[string]string{
		skillMDPath:  renderSkillMD(spec),
		agentsMDPath: renderAgentsMD(spec),
	}
	if expert := renderExpertMD(spec); expert != "" {
		files[expertMDPath] = expert
	}
	return files
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
