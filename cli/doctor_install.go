package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// installCheck is `doctor --install-check`: what a fresh install must be
// before its first deal, as one JSON object an agent can seal as evidence.
//
//   - the binary is the expected release (version, and commit when given);
//   - exactly one skill named `deal` is visible under the skills directory
//     (and, when given, it is the expected release's SKILL.md by sha256):
//     a backup copy left beside it (deal.bak-*/SKILL.md) is a second one, and
//     an agent may load either;
//   - the deal profile names both a witness endpoint and that witness's
//     public key, without which `deal open` fails closed.
//
// Any failed check exits 3 after printing the object, with every issue named.
// With --evidence-out it also writes the evidence-note body that seals it.
func installCheck(c *cobra.Command) error {
	wantVersion, _ := c.Flags().GetString("expect-version")
	wantCommit, _ := c.Flags().GetString("expect-commit")
	skillsDir, _ := c.Flags().GetString("skills-dir")
	wantSkill, _ := c.Flags().GetString("expect-skill-sha256")
	evidenceOut, _ := c.Flags().GetString("evidence-out")
	name, _ := c.Flags().GetString("profile")
	if wantVersion == "" || wantCommit == "" || skillsDir == "" || name == "" {
		return InputError("--install-check needs --expect-version, --expect-commit, --skills-dir and --profile")
	}
	var issues []string
	report := map[string]any{"binary_version": CLIVersion, "binary_commit": CLICommit, "expected_version": wantVersion, "expected_commit": wantCommit}
	// Before the plugin check, so it sees the root as healed. A refusal adds
	// no issue of its own: the plugin check fails when the profile's plugin
	// is the one refused.
	report["user_root"] = doctorUserRoot()
	if CLIVersion != wantVersion {
		issues = append(issues, "binary is "+CLIVersion+", expected "+wantVersion)
	}
	// The full commit, exactly: a short or partial value never passes.
	if CLICommit != wantCommit {
		issues = append(issues, "binary commit is "+CLICommit+", expected "+wantCommit)
	}

	skills, err := dealSkills(skillsDir)
	switch {
	case err != nil:
		issues = append(issues, "skills directory "+skillsDir+" cannot be read")
	case len(skills) == 0:
		issues = append(issues, "no deal skill under "+skillsDir)
	case len(skills) > 1:
		issues = append(issues, "more than one deal skill under "+skillsDir+": "+strings.Join(skills, ", ")+"; move every copy but the installed one out of the skills directory")
	}
	report["deal_skills"] = skills
	if len(skills) == 1 {
		data, err := os.ReadFile(filepath.Join(skillsDir, skills[0]))
		if err == nil {
			sum := sha256.Sum256(data)
			got := hex.EncodeToString(sum[:])
			report["skill_sha256"] = got
			if wantSkill != "" && !strings.EqualFold(wantSkill, got) {
				issues = append(issues, "the deal skill "+skills[0]+" has sha256 "+got+", expected "+strings.ToLower(wantSkill))
			}
		}
	}
	if wantSkill != "" {
		report["expected_skill_sha256"] = strings.ToLower(wantSkill)
	}

	witness := map[string]any{"endpoint_set": false, "public_key_ok": false}
	if p, err := LoadProfile(name); err != nil {
		issues = append(issues, "profile "+name+" does not load: "+SafeError(err))
	} else {
		witness["endpoint_set"] = p.Checkpoint.Endpoint != ""
		switch issue := witnessKeyIssue(p); {
		case p.Checkpoint.Endpoint == "":
			issues = append(issues, "profile "+name+" has no checkpoint.endpoint: nothing it seals can be witnessed")
		case issue != "":
			issues = append(issues, issue)
		default:
			witness["public_key_ok"] = true
		}
		if plugin := profilePlugin(p); plugin != "" {
			check, issue := pluginInstallCheck(plugin)
			report["plugin"] = check
			if issue != "" {
				issues = append(issues, issue)
			}
		}
	}
	report["profile_witness"] = witness
	if issues == nil {
		issues = []string{}
	}
	report["ok"], report["issues"] = len(issues) == 0, issues
	if evidenceOut != "" {
		// The body `deal note --kind evidence --input FILE` seals as is: the
		// object below, as one line, is the detail.
		line, err := json.Marshal(map[string]any{"install_check": report})
		if err != nil {
			return err
		}
		body, err := json.Marshal(map[string]string{"about": "install check", "source": "capsulectl doctor --install-check", "detail": string(line)})
		if err != nil {
			return err
		}
		if err := os.WriteFile(evidenceOut, append(body, '\n'), 0o600); err != nil {
			return Hint(ErrInput, "cannot write --evidence-out "+evidenceOut)
		}
	}
	if err := Output(c, map[string]any{"install_check": report}); err != nil {
		return err
	}
	if len(issues) > 0 {
		return ErrPartial
	}
	return nil
}

// dealSkills lists, relative to dir, every SKILL.md whose front matter names
// the skill `deal`, at most three directories down. Symbolic links are not
// followed.
func dealSkills(dir string) ([]string, error) {
	root := filepath.Clean(dir)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, errors.New("not a directory")
	}
	found := []string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() && rel != "." && strings.Count(rel, string(filepath.Separator)) >= 2 {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() || d.Name() != "SKILL.md" {
			return nil
		}
		if SkillName(path) == "deal" {
			found = append(found, rel)
		}
		return nil
	})
	return found, err
}

// skillName is the `name:` in a SKILL.md's leading front matter, or "".
func SkillName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return ""
	}
	for i := 0; i < 50 && scanner.Scan(); i++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			return ""
		}
		if v, ok := strings.CutPrefix(line, "name:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// profilePlugin is the first-party plugin a profile's commands run in: deal
// for a deal (sqlite) profile, book for a book profile, none for a plain one.
func profilePlugin(p Profile) string {
	switch {
	case p.Type == "sqlite":
		return DealPluginName
	case p.IsBook():
		return p.LogOwner()
	}
	return ""
}

// pluginInstallCheck reports the installed first-party plugin: present, and
// on a release core the same version. A development core reports the
// version without checking it.
func pluginInstallCheck(name string) (map[string]any, string) {
	check := map[string]any{"name": name, "installed": false}
	for _, info := range discoverPlugins() {
		if info.Name != name {
			continue
		}
		check["installed"], check["path"], check["version"] = true, info.path, info.Version
		switch {
		case !isReleaseCore():
			check["version_check"] = "not checked: development build " + CLIVersion
		case info.coreVersion() != CLIVersion:
			check["version_check"] = "mismatch"
			return check, fmt.Sprintf("capsulectl-%s is built for capsulectl %s, this capsulectl is %s: install the plugin file built for this release, then run capsulectl plugin install ./capsulectl-%s", name, FirstNonEmpty(info.coreVersion(), "unversioned"), CLIVersion, name)
		default:
			check["version_check"] = "matches"
		}
		return check, ""
	}
	return check, "capsulectl-" + name + " is not installed: install the plugin file from its release, then run capsulectl plugin install ./capsulectl-" + name
}
