package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

// discoverScope is the human-authored "may / may not touch" list --
// discover refuses to run without one (see the boundary note on
// [batch3-connector-interface-and-discover]: humans write this list first,
// the tool never invents it). Roots are the only directories discover may
// descend into; Deny adds operator-specific exclusions on top of the
// built-in credential/secret deny-list, which always applies regardless of
// what Scope says.
type discoverScope struct {
	Roots []string `yaml:"roots"`
	Deny  []string `yaml:"deny,omitempty"`
}

func loadScope(path string) (discoverScope, error) {
	if path == "" {
		return discoverScope{}, inputError(
			"--scope is required: discover refuses to guess which directories it may read; " +
				"pass a YAML file with a roots: [] list an operator wrote")
	}
	raw, err := readInput(path)
	if err != nil {
		return discoverScope{}, err
	}
	var scope discoverScope
	if err := yaml.Unmarshal(raw, &scope); err != nil {
		return discoverScope{}, inputError("--scope is not valid YAML")
	}
	if len(scope.Roots) == 0 {
		return discoverScope{}, inputError("--scope roots: [] must name at least one directory")
	}
	return scope, nil
}

// discoveredFile is one row of the inventory: `discover` never records raw
// file content, only what classifyFile decided and, for a scanned file, a
// digest -- see [batch3-connector-interface-and-discover]'s boundary line
// ("MUST NOT read payloads, secrets or credentials").
type discoveredFile struct {
	Root        string `json:"root"`
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Disposition string `json:"disposition"`
	DigestAlg   string `json:"digest_alg,omitempty"`
	Digest      string `json:"digest,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

const (
	dispositionScanned         = "scanned"
	dispositionExcludedSecret  = "excluded_secret"
	dispositionExcludedSymlink = "excluded_symlink"
	dispositionPresentNotRead  = "present_not_read"
	dispositionError           = "error"
)

// denyDirs are never even descended into -- their contents are not walked,
// let alone opened. Kept separate from denyFilePatterns because a directory
// match should prune the whole subtree (fs.SkipDir), not just skip one file.
var denyDirs = map[string]bool{
	".ssh": true, ".aws": true, ".gnupg": true, ".git": true, ".kube": true,
}

// denyFilePatterns match a file's base name (case-insensitive) and always
// win over an allow match -- a file named ".env.yaml" is excluded as a
// secret even though *.yaml is otherwise an allowed config extension.
var denyFilePatterns = []string{
	".env", ".env.*", "*.pem", "*.key", "*_rsa", "*_ed25519", "id_rsa*", "id_ed25519*",
	"*.p12", "*.pfx", "*.jks", "credentials", "credentials.*", "*credentials*",
	"*secret*", "*token*", ".netrc", ".git-credentials", ".npmrc", ".pypirc",
	"kubeconfig*", "*.pgpass", "*.pfx", "*password*",
}

// allowExtensions and allowBasenames are the only files discover will ever
// open, per the boundary's "may inventory OTel config, MCP servers,
// gateways, repos, CI and named systems" -- config-shaped files for those
// categories. A file that matches neither is recorded present_not_read:
// its path and size are inventoried, its bytes are never touched.
var allowExtensions = map[string]bool{
	".yaml": true, ".yml": true, ".json": true, ".toml": true, ".proto": true,
}
var allowBasenames = map[string]bool{
	"dockerfile": true, "go.mod": true, "go.sum": true, "pyproject.toml": true, "package.json": true,
}

func matchAny(patterns []string, name string) bool {
	lower := strings.ToLower(name)
	for _, pattern := range patterns {
		if ok, _ := filepath.Match(strings.ToLower(pattern), lower); ok {
			return true
		}
	}
	return false
}

func isDenyFile(name string, extraDeny []string) (bool, string) {
	if matchAny(denyFilePatterns, name) {
		return true, "matches the built-in credential/secret deny pattern"
	}
	if matchAny(extraDeny, name) {
		return true, "matches a --scope deny pattern"
	}
	return false, ""
}

func isAllowFile(name string) bool {
	if allowBasenames[strings.ToLower(name)] {
		return true
	}
	return allowExtensions[strings.ToLower(filepath.Ext(name))]
}

// classifyKind labels an allow-listed file by the system category the
// boundary names, from its path -- best-effort naming for the inventory,
// never a security decision (that is isAllowFile/isDenyFile's job, already
// applied before classifyKind is ever called).
func classifyKind(relPath string) string {
	lower := strings.ToLower(relPath)
	switch {
	case strings.Contains(lower, "otel") || strings.Contains(lower, "opentelemetry"):
		return "otel-config"
	case strings.Contains(lower, "mcp"):
		return "mcp-manifest"
	case strings.Contains(lower, "gateway"):
		return "gateway-config"
	case strings.HasPrefix(lower, ".github/workflows/") || strings.Contains(lower, "/ci/") || strings.HasPrefix(lower, "ci/"):
		return "ci-config"
	case strings.Contains(lower, "openapi") || strings.Contains(lower, "swagger"):
		return "openapi-spec"
	case strings.HasSuffix(lower, "go.mod") || strings.HasSuffix(lower, "go.sum") ||
		strings.HasSuffix(lower, "pyproject.toml") || strings.HasSuffix(lower, "package.json"):
		return "repo-config"
	default:
		return "unknown-config"
	}
}

// maxConfigFileBytes bounds what discover will digest per file -- config
// files are small; anything larger is treated the same as an unreadable
// file rather than read in full.
const maxConfigFileBytes = 8 << 20

func digestFile(path string) (digest string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxConfigFileBytes+1))
	if err != nil {
		return "", 0, err
	}
	if n > maxConfigFileBytes {
		return "", 0, fmt.Errorf("exceeds %d byte scan limit", maxConfigFileBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// scanRoot walks one scope root read-only. The deny check runs BEFORE any
// open() of a candidate file -- a file that matches isDenyFile is recorded
// and skipped without ever being passed to digestFile; this ordering is
// exactly what TestDiscoverNeverOpensADeniedSecret proves against a
// permission-denied fixture (a file digestFile could not open even if it
// tried).
func scanRoot(root string, extraDeny []string) ([]discoveredFile, error) {
	root = filepath.Clean(root)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve root %s: %w", root, err)
	}
	var out []discoveredFile
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, do not fail the whole scan
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			if denyDirs[strings.ToLower(d.Name())] {
				return fs.SkipDir
			}
			return nil
		}
		// checkName is what isDenyFile/isAllowFile classify against. For a
		// symlink it MUST be the resolved target's basename, never the
		// link's own name -- otherwise a symlink named e.g. "config.yaml"
		// pointing at "id_rsa" would pass the deny check on its link name
		// and digestFile would open the real secret. d.Name() is correct
		// for every non-symlink entry.
		checkName := d.Name()
		if d.Type()&fs.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !(resolved == resolvedRoot || strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator))) {
				out = append(out, discoveredFile{Root: root, Path: rel, Kind: classifyKind(rel),
					Disposition: dispositionExcludedSymlink, Reason: "symlink target is unresolvable or escapes the scope root"})
				return nil
			}
			checkName = filepath.Base(resolved)
		}
		if deny, reason := isDenyFile(checkName, extraDeny); deny {
			out = append(out, discoveredFile{Root: root, Path: rel, Kind: classifyKind(rel),
				Disposition: dispositionExcludedSecret, Reason: reason})
			return nil
		}
		if !isAllowFile(checkName) {
			info, statErr := d.Info()
			var size int64
			if statErr == nil {
				size = info.Size()
			}
			out = append(out, discoveredFile{Root: root, Path: rel, Kind: classifyKind(rel),
				Disposition: dispositionPresentNotRead, SizeBytes: size,
				Reason: "not a recognized config file type -- inventoried by path only"})
			return nil
		}
		digest, size, digestErr := digestFile(path)
		if digestErr != nil {
			out = append(out, discoveredFile{Root: root, Path: rel, Kind: classifyKind(rel),
				Disposition: dispositionError, Reason: digestErr.Error()})
			return nil
		}
		out = append(out, discoveredFile{Root: root, Path: rel, Kind: classifyKind(rel),
			Disposition: dispositionScanned, DigestAlg: "SHA-256", Digest: digest, SizeBytes: size})
		return nil
	})
	if walkErr != nil {
		return out, walkErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Root+out[i].Path < out[j].Root+out[j].Path })
	return out, nil
}

func renderDiscoverTable(files []discoveredFile) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ROOT\tPATH\tKIND\tDISPOSITION\tDIGEST\tREASON")
	for _, f := range files {
		digest := f.Digest
		if len(digest) > 12 {
			digest = digest[:12] + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Root, f.Path, f.Kind, f.Disposition, digest, f.Reason)
	}
	w.Flush()
	return b.String()
}

func renderEffectsTable(boundaries []effectBoundary) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SOURCE\tSURFACE\tTYPE\tCLASSIFICATION\tSIGNAL")
	for _, e := range boundaries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Source, e.Surface, e.SurfaceType, e.Classification, e.Signal)
	}
	w.Flush()
	return b.String()
}

// randomActionSuffix avoids adding a UUID dependency for a value only ever
// compared for uniqueness, never parsed.
func randomActionSuffix() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// sealScanManifest builds the capsule every discover scan must emit
// (Do item 4: "digests of config, never contents"). For a plain scan the
// Payload IS the {path, kind, disposition, digest} manifest scanRoot
// already reduced every file to -- never a scanned file's raw bytes.
//
// For an --effects scan the manifest is instead the []effectBoundary list:
// each row's Surface/Source is a STRUCTURED NAME extracted from an already
// allow-listed (non-secret) config file's content -- an MCP tool name, an
// "METHOD path" pair, a bus topic name -- not a digest, and not the raw
// file either. This is a narrower claim than "digests of config": it is
// "small, named structural facts about config, never a secret's bytes and
// never a whole file's contents" -- the boundary discover exists to hold is
// about payloads/secrets/credentials, not about every string this command
// ever seals.
func sealScanManifest(p Profile, kind string, manifest any, outputPath string) (map[string]string, error) {
	key, err := privateKey(p.Signing)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	suffix, err := randomActionSuffix()
	if err != nil {
		return nil, err
	}
	r := Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{
			ActionID:   kind + "/" + suffix,
			ActionType: emit.ActionTypeFYI,
			Operator:   p.Name,
			Developer:  "capsulectl/discover",
			Domain:     emit.DomainAction,
			Provenance: emit.ProvenanceCollector,
			Timestamp:  time.Now().UTC(),
		},
		Payload: payload,
	}
	record, err := seal(r, key)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if err := atomicFile(outputPath, b, false); err != nil {
		return nil, err
	}
	return map[string]string{"capsule_id": record.CapsuleID, "artifact": outputPath}, nil
}

func discoverCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Read-only inventory of OTel/MCP/gateway/CI/repo config -- never payloads, secrets or credentials",
		Args:  noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			scopePath, _ := c.Flags().GetString("scope")
			scope, err := loadScope(scopePath)
			if err != nil {
				return err
			}
			effectsMode, _ := c.Flags().GetBool("effects")
			format, _ := c.Flags().GetString("format")
			if format != "table" && format != "json" {
				return inputError("--format must be table or json")
			}
			sealOutput, _ := c.Flags().GetString("seal-output")
			if sealOutput == "" {
				return inputError("--seal-output is required: every scan seals a capsule of what it scanned")
			}
			p, err := selected(c)
			if err != nil {
				return err
			}

			var allFiles []discoveredFile
			for _, root := range scope.Roots {
				files, err := scanRoot(root, scope.Deny)
				if err != nil {
					return inputError(fmt.Sprintf("cannot scan root %s: %s", root, err.Error()))
				}
				allFiles = append(allFiles, files...)
			}

			if !effectsMode {
				result, err := sealScanManifest(p, "discover-scan", allFiles, sealOutput)
				if err != nil {
					return err
				}
				if format == "table" {
					fmt.Fprint(c.OutOrStdout(), renderDiscoverTable(allFiles))
					fmt.Fprintf(c.OutOrStdout(), "\nsealed: capsule_id=%s artifact=%s\n", result["capsule_id"], result["artifact"])
					return nil
				}
				return output(c, map[string]any{"files": allFiles, "sealed": result})
			}

			var boundaries []effectBoundary
			for _, f := range allFiles {
				if f.Disposition != dispositionScanned {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(f.Root, f.Path))
				if err != nil {
					continue // already digested successfully above; a rare race is not fatal here
				}
				boundaries = append(boundaries, extractEffects(filepath.Join(f.Root, f.Path), raw)...)
			}
			result, err := sealScanManifest(p, "discover-effects-scan", boundaries, sealOutput)
			if err != nil {
				return err
			}
			if format == "table" {
				fmt.Fprint(c.OutOrStdout(), renderEffectsTable(boundaries))
				fmt.Fprintf(c.OutOrStdout(), "\nsealed: capsule_id=%s artifact=%s\n", result["capsule_id"], result["artifact"])
				return nil
			}
			return output(c, map[string]any{"effects": boundaries, "sealed": result})
		},
	}
	cmd.Flags().String("scope", "", "YAML file naming the roots discover may read (required, operator-authored)")
	cmd.Flags().Bool("effects", false, "Build an effect-boundary map from MCP/OpenAPI/bus config instead of a plain inventory")
	cmd.Flags().String("format", "table", "Output format: table or json")
	cmd.Flags().String("seal-output", "", "Path to write the sealed scan capsule (required)")
	return cmd
}
