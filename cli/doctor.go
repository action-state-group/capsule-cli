package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// checkpointReachability is a doctor-only best-effort probe: an unauthenticated
// GET of the witness's health endpoint (<endpoint>/health), read-only, with
// no Authorization header attached (doctor never carries a token onto the
// wire, so a captured request never discloses one) and a short timeout so a
// misconfigured or unreachable witness cannot hang the whole report. A
// witness that answers is reachable; only a 2xx answer is ok (a HEAD, or a
// path the service does not serve, can be answered 405 by a witness that is
// up, and that is not a witness that works).
func checkpointReachability(ctx context.Context, endpoint string) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	url := strings.TrimRight(endpoint, "/") + "/health"
	request := map[string]any{"method": http.MethodGet, "url": url}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return map[string]any{"checked": true, "request": request, "reachable": false}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return map[string]any{"checked": true, "request": request, "reachable": false}
	}
	defer resp.Body.Close()
	return map[string]any{"checked": true, "request": request, "reachable": true, "status_code": resp.StatusCode}
}

// keyFilePermissions reports whether a signing key's --file source is a regular,
// owner-only file, without ever opening it -- doctor confirms the file is
// protected, it never reads (and so never risks printing) the key material.
func keyFilePermissions(path string) map[string]any {
	info, err := os.Lstat(path)
	if err != nil {
		return map[string]any{"path": path, "ok": false, "reason": "cannot stat file"}
	}
	if !info.Mode().IsRegular() {
		return map[string]any{"path": path, "ok": false, "reason": "not a regular file"}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return map[string]any{"path": path, "ok": false, "reason": "accessible beyond its owner"}
	}
	return map[string]any{"path": path, "ok": true}
}

// signingKeySummary reports where a profile's signing secret comes from and,
// for each source, only what doctor can confirm without ever resolving the
// secret's value: a file's permissions, or whether an environment variable
// name is set -- never the file's bytes or the variable's value.
func signingKeySummary(s Secret) map[string]any {
	switch {
	case s.File != "":
		return map[string]any{"source": "file", "file": keyFilePermissions(s.File)}
	case s.Env != "":
		_, set := os.LookupEnv(s.Env)
		return map[string]any{"source": "environment", "env_var": s.Env, "set": set}
	case s.Value != "":
		return map[string]any{"source": "inline_value", "note": "stored directly in the profile file (owner-only permissions already required to load the profile)"}
	default:
		return map[string]any{"source": "none", "note": "no signing key configured"}
	}
}

func doctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Environment sanity: binary version, plugin trust, profile presence, key permissions; never prints a token",
		Args:  NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if install, _ := c.Flags().GetBool("install-check"); install {
				return installCheck(c)
			}
			// Heal first, so the plugins listed are the ones that now load.
			userRoot := doctorUserRoot()
			plugins := discoverPlugins()
			pluginRows := make([]map[string]any, 0, len(plugins))
			// A plugin whose metadata declares a doctor subcommand checks
			// what it owns itself; the core names it, never what it checks.
			// Only a plugin this tree dispatches to is listed: one named like
			// a core command, or not a single token, is never registered
			// (addPluginCommands), so its command would not reach it.
			dispatched := map[string]bool{}
			for _, cmd := range c.Root().Commands() {
				if name, ok := cmd.Annotations[pluginAnnotation]; ok {
					dispatched[name] = true
				}
			}
			doctors := []string{}
			for _, p := range plugins {
				pluginRows = append(pluginRows, map[string]any{"name": p.Name, "vendor": p.Vendor, "version": p.Version, "plugin_api": p.PluginAPI})
				if dispatched[p.Name] && slices.Contains(p.Subcommands, "doctor") {
					doctors = append(doctors, "capsulectl "+p.Name+" doctor")
				}
			}
			report := map[string]any{
				"binary_version": CLIVersion,
				"binary_commit":  CLICommit,
				"plugins": map[string]any{
					"roots":      trustedPluginRoots(),
					"discovered": pluginRows,
					"user_root":  userRoot,
					"doctors":    doctors,
				},
			}

			name, _ := c.Flags().GetString("profile")
			checkWitness, _ := c.Flags().GetBool("check-witness")
			profileReport := map[string]any{"selected": name}
			if name == "" {
				dir, e := ProfilesDir()
				count := 0
				if e == nil {
					if entries, e := os.ReadDir(dir); e == nil {
						for _, entry := range entries {
							if entry.Type().IsRegular() && filepath.Ext(entry.Name()) == ".yaml" && profileName.MatchString(strings.TrimSuffix(entry.Name(), ".yaml")) {
								count++
							}
						}
					}
				}
				profileReport["present"] = false
				profileReport["configured_profiles"] = count
				report["profile"] = profileReport
				report["witness"] = map[string]any{"checked": false, "reason": "no --profile selected"}
				return Output(c, report)
			}

			p, e := LoadProfile(name)
			if e != nil {
				profileReport["present"] = false
				profileReport["issue"] = SafeError(e)
				report["profile"] = profileReport
				report["witness"] = map[string]any{"checked": false, "reason": "profile did not load"}
				return Output(c, report)
			}
			profileReport["present"] = true
			profileReport["type"] = p.Type
			profileReport["signing_key"] = signingKeySummary(p.Signing)
			report["profile"] = profileReport
			// The materiality predicate of a profile a plugin owns: which of the agent's own
			// picks pause a check. None is a notice, not an issue: checks fail
			// safe. One that cannot be read refuses every check: an issue.
			unreadablePredicate := false
			if p.HasOwnedStore() || p.Materiality.Predicate != "" || p.Materiality.Digest != "" {
				materiality := map[string]any{"configured": p.Materiality.Predicate != ""}
				switch predicate, err := PinnedMateriality(p); {
				case p.Materiality.Predicate == "" && p.Materiality.Digest == "":
					materiality["notice"] = "no materiality predicate: every attribute the agent picks pauses the check (set one with deal init --materiality)"
				case err != nil:
					materiality["ok"], materiality["issue"] = false, SafeError(err)
					unreadablePredicate = true
				default:
					materiality["ok"], materiality["name"], materiality["version"], materiality["digest"] = true, predicate.Name, predicate.Version, predicate.Digest
				}
				profileReport["materiality"] = materiality
			}
			// A pinned rules checker: its executable's digest and every
			// definition digest its ruleset may report.
			if pin := p.RulesChecker; len(pin.Command) > 0 {
				digests := pin.PinnedDigests()
				checker := map[string]any{"sha256": pin.SHA256, "definition_digests": digests}
				if len(digests) == 0 {
					checker["definition_digests"] = []string{}
					checker["notice"] = "no definition digest pinned: whatever ruleset the checker reports is evaluated"
				}
				profileReport["rules_checker"] = checker
			}
			// The wording pack the profile speaks with. A pinned pack that
			// cannot be used is an issue: the whole built-in pack speaks
			// instead (narration.go).
			if p.Wording.Path != "" {
				wording := map[string]any{"pinned": p.Wording.Path, "sha256": p.Wording.SHA256}
				if w, problem := ActiveWording(p); problem != "" {
					wording["used"], wording["issue"] = w.Source, problem
				} else {
					wording["used"], wording["id"] = w.Source, w.ID
				}
				profileReport["wording"] = wording
			}

			switch {
			case !checkWitness:
				report["witness"] = map[string]any{"checked": false, "reason": "opt-in: pass --check-witness"}
			case p.Checkpoint.Endpoint == "":
				report["witness"] = map[string]any{"checked": false, "reason": "profile has no checkpoint.endpoint configured"}
			default:
				witness := checkpointReachability(c.Context(), p.Checkpoint.Endpoint)
				if witness["reachable"] != true {
					witness["consent"] = WitnessConsentText(p.Checkpoint.Endpoint)
				} else {
					witness["consent"] = WitnessConsentReached(p.Checkpoint.Endpoint)
				}
				if issue := witnessKeyIssue(p); issue != "" {
					// Reachable or not, no receipt from this endpoint can be
					// verified: report it as a failure, with the fix.
					witness["ok"], witness["public_key"] = false, map[string]any{"ok": false, "issue": issue}
					report["witness"] = witness
					if e := Output(c, report); e != nil {
						return e
					}
					return ErrPartial
				}
				// ok needs both a well-formed key and an endpoint that answered
				// the probe with success: any 4xx or 5xx is not ok, and says so.
				witness["public_key"] = map[string]any{"ok": true}
				status, _ := witness["status_code"].(int)
				witness["ok"] = witness["reachable"] == true && status >= 200 && status < 300
				switch {
				case witness["reachable"] != true:
					witness["issue"] = "the checkpoint endpoint did not answer the probe: check the URL and the network"
				case witness["ok"] != true:
					req, _ := witness["request"].(map[string]any)
					witness["issue"] = fmt.Sprintf("the checkpoint endpoint answered HTTP %d to %s %s: a witness that works answers it with success; check the URL", status, req["method"], req["url"])
				}
				report["witness"] = witness
			}
			if e := Output(c, report); e != nil || !unreadablePredicate {
				return e
			}
			return ErrPartial
		},
	}
	cmd.Flags().Bool("install-check", false, "Check a fresh install before its first deal: the expected release, exactly one deal skill, a witness endpoint with its key; one JSON object, exit 3 on any issue")
	cmd.Flags().String("expect-version", "", "With --install-check: the release tag this binary must report")
	cmd.Flags().String("expect-commit", "", "With --install-check: the full commit this binary must report, exactly")
	cmd.Flags().String("evidence-out", "", "With --install-check: also write the body for `deal note --kind evidence --input FILE`, which seals this result")
	cmd.Flags().String("expect-skill-sha256", "", "With --install-check: the sha256 of the release's deal SKILL.md")
	cmd.Flags().String("skills-dir", "", "With --install-check: the directory the agent loads skills from")
	cmd.Flags().Bool("check-witness", false, "Also probe the profile's checkpoint endpoint with an unauthenticated, read-only GET of its /health (opt-in; prints exactly what would leave); ok only on a 2xx answer")
	return cmd
}
