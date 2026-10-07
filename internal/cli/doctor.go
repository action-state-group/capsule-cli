package cli

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// checkpointReachability is a doctor-only best-effort probe: an unauthenticated
// HEAD request against the profile's checkpoint endpoint, with no Authorization
// header attached (doctor never carries a token onto the wire, so a captured
// request never discloses one) and a short timeout so a misconfigured or
// unreachable witness cannot hang the whole report.
func checkpointReachability(ctx context.Context, endpoint string) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request := map[string]any{"method": http.MethodHead, "url": endpoint}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
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
		Args:  noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if install, _ := c.Flags().GetBool("install-check"); install {
				return installCheck(c)
			}
			plugins := discoverPlugins()
			pluginRows := make([]map[string]any, 0, len(plugins))
			for _, p := range plugins {
				pluginRows = append(pluginRows, map[string]any{"name": p.Name, "vendor": p.Vendor, "version": p.Version, "plugin_api": p.PluginAPI})
			}
			report := map[string]any{
				"binary_version": cliVersion,
				"binary_commit":  cliCommit,
				"plugins": map[string]any{
					"roots":      trustedPluginRoots(),
					"discovered": pluginRows,
				},
			}

			name, _ := c.Flags().GetString("profile")
			checkWitness, _ := c.Flags().GetBool("check-witness")
			profileReport := map[string]any{"selected": name}
			if name == "" {
				dir, e := profilesDir()
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
				return output(c, report)
			}

			p, e := loadProfile(name)
			if e != nil {
				profileReport["present"] = false
				profileReport["issue"] = SafeError(e)
				report["profile"] = profileReport
				report["witness"] = map[string]any{"checked": false, "reason": "profile did not load"}
				return output(c, report)
			}
			profileReport["present"] = true
			profileReport["type"] = p.Type
			profileReport["signing_key"] = signingKeySummary(p.Signing)
			report["profile"] = profileReport
			// A deal profile's materiality predicate: which of the agent's own
			// picks pause a check. None is a notice, not an issue: checks fail
			// safe. One that cannot be read refuses every check: an issue.
			unreadablePredicate := false
			if p.Namespace == "deal" {
				materiality := map[string]any{"configured": p.Materiality.Predicate != ""}
				switch predicate, err := pinnedMateriality(p); {
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

			switch {
			case !checkWitness:
				report["witness"] = map[string]any{"checked": false, "reason": "opt-in: pass --check-witness"}
			case p.Checkpoint.Endpoint == "":
				report["witness"] = map[string]any{"checked": false, "reason": "profile has no checkpoint.endpoint configured"}
			default:
				witness := checkpointReachability(c.Context(), p.Checkpoint.Endpoint)
				if witness["reachable"] != true {
					witness["consent"] = witnessConsentText(p.Checkpoint.Endpoint)
				} else {
					witness["consent"] = witnessConsentReached(p.Checkpoint.Endpoint)
				}
				if issue := witnessKeyIssue(p); issue != "" {
					// Reachable or not, no receipt from this endpoint can be
					// verified: report it as a failure, with the fix.
					witness["ok"], witness["public_key"] = false, map[string]any{"ok": false, "issue": issue}
					report["witness"] = witness
					if e := output(c, report); e != nil {
						return e
					}
					return ErrPartial
				}
				// ok needs both a well-formed key and an endpoint that answered.
				witness["public_key"] = map[string]any{"ok": true}
				witness["ok"] = witness["reachable"] == true
				if witness["ok"] != true {
					witness["issue"] = "the checkpoint endpoint did not answer the probe: check the URL and the network"
				}
				report["witness"] = witness
			}
			if e := output(c, report); e != nil || !unreadablePredicate {
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
	cmd.Flags().Bool("check-witness", false, "Also probe the profile's checkpoint endpoint with an unauthenticated HEAD request (opt-in; prints exactly what would leave)")
	return cmd
}
