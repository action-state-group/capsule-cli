package cli

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/emitter"
	"github.com/spf13/cobra"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
)

// `presentation build` writes the offline page with the trusted-executable
// presentation modules of the installed plugins in its module slots. Which
// module renders is decided in the page, by the vendored runtime's resolver
// (presentation contract section 4.3); capsulectl carries no copy of it. It
// runs only the two checks the contract puts on a builder that need no
// resolution, before anything is written:
//
//   - the page's runtime would refuse no module it is given (section 3.2:
//     presentation_api_unsupported, runtime_too_old), and
//   - no two manifests the page would hold, the built-ins and the given
//     modules, are of one tier and can match one descriptor (section 4.5),
//     and no id is given twice.
//
// `presentation list` (which module a bundle resolves to) waits for a
// resolver in agent-action-capsule's Go module.

// presentationRuntime is the vendored runtime's declaration
// (REFERENCE_PRESENTATION_RUNTIME in assets/evidence-graph.iife.js; pinned
// by TestThePresentationRuntimeDeclarationIsTheVendoredOne).
var presentationRuntime = struct {
	PresentationAPIs []string `json:"presentation_apis"`
	Version          string   `json:"version"`
}{[]string{"aac.presentation-api/v0"}, "0.1.0"}

// The page registry's built-in manifests, agent-action-capsule's
// schemas/examples/presentation-manifest-v0/builtin-*.json (see
// assets/presentation/README.md).
//
//go:embed assets/presentation/builtin/*.json
var builtinPresentationFS embed.FS

func builtinPresentationManifests() ([]map[string]interface{}, error) {
	entries, err := builtinPresentationFS.ReadDir("assets/presentation/builtin")
	if err != nil {
		return nil, err
	}
	var out []map[string]interface{}
	for _, entry := range entries {
		raw, err := builtinPresentationFS.ReadFile(path.Join("assets/presentation/builtin", entry.Name()))
		if err != nil {
			return nil, err
		}
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("built-in presentation manifest %s: %w", entry.Name(), err)
		}
		out = append(out, m)
	}
	return out, nil
}

// presentationBuildRefusal is why no page was written: a module the page's
// runtime would refuse, or manifests the page's registry would reject.
type presentationBuildRefusal struct {
	Reason string `json:"reason"`
	// presentation_api_unsupported, runtime_too_old
	Module          string      `json:"module,omitempty"`
	PresentationAPI string      `json:"presentation_api,omitempty"`
	RuntimeMin      string      `json:"runtime_min,omitempty"`
	Runtime         interface{} `json:"runtime,omitempty"`
	// ambiguous (tier specific|fallback), duplicate_id, dead_manifest
	Tier    string   `json:"tier,omitempty"`
	Modules []string `json:"modules,omitempty"`
	Detail  string   `json:"detail"`
}

func (r *presentationBuildRefusal) Error() string { return "no page written: " + r.Detail }

func manifestString(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

func manifestSet(v interface{}) map[string]bool {
	out := map[string]bool{}
	for _, item := range asList(v) {
		if s, ok := item.(string); ok {
			out[s] = true
		}
	}
	return out
}

func manifestMap(m map[string]interface{}, key string) map[string]interface{} {
	v, _ := m[key].(map[string]interface{})
	return v
}

func requiredProfiles(m map[string]interface{}) map[string]bool {
	return manifestSet(manifestMap(m, "requires")["profiles"])
}

func requiredExtensions(m map[string]interface{}) map[string]bool {
	return manifestSet(manifestMap(manifestMap(m, "requires"), "extensions")["required"])
}

func forbiddenProfiles(m map[string]interface{}) map[string]bool {
	return manifestSet(manifestMap(m, "forbids")["profiles"])
}

func forbiddenExtensions(m map[string]interface{}) map[string]bool {
	return manifestSet(manifestMap(m, "forbids")["extensions"])
}

func union(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func meets(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// onePerProfileKey: a descriptor holds one token per profile key (the part
// before the first ":").
func onePerProfileKey(tokens map[string]bool) bool {
	keys := map[string]bool{}
	for t := range tokens {
		key, _, _ := strings.Cut(t, ":")
		if keys[key] {
			return false
		}
		keys[key] = true
	}
	return true
}

// coMatchable is section 4.5's five conditions: true exactly when some
// descriptor matches both manifests.
func coMatchable(a, b map[string]interface{}) bool {
	if manifestString(manifestMap(a, "requires"), "bundle_kind") != manifestString(manifestMap(b, "requires"), "bundle_kind") {
		return false
	}
	audA, audB := manifestSet(a["audiences"]), manifestSet(b["audiences"])
	if !audA["*"] && !audB["*"] && !meets(audA, audB) {
		return false
	}
	if !meets(manifestSet(a["formats"]), manifestSet(b["formats"])) {
		return false
	}
	rp := union(requiredProfiles(a), requiredProfiles(b))
	re := union(requiredExtensions(a), requiredExtensions(b))
	if meets(rp, union(forbiddenProfiles(a), forbiddenProfiles(b))) || meets(re, union(forbiddenExtensions(a), forbiddenExtensions(b))) {
		return false
	}
	return onePerProfileKey(rp)
}

func manifestTier(m map[string]interface{}) string {
	if fallback, _ := m["fallback"].(bool); fallback {
		return "fallback"
	}
	return "specific"
}

// staticPresentationCheck rejects the set of manifests a page's registry
// would hold when an id is given twice, a manifest can never match (section
// 4.5, last paragraph) or two manifests of one tier can match one
// descriptor.
func staticPresentationCheck(manifests []map[string]interface{}) error {
	seen := map[string]bool{}
	for _, m := range manifests {
		id := manifestString(m, "id")
		if seen[id] {
			return &presentationBuildRefusal{Reason: "duplicate_id", Modules: []string{id},
				Detail: fmt.Sprintf("the presentation module id %s is given twice (a built-in, or two plugins)", id)}
		}
		seen[id] = true
		var dead string
		switch {
		case meets(requiredProfiles(m), forbiddenProfiles(m)):
			dead = "its requires and forbids share a profile token"
		case meets(requiredExtensions(m), forbiddenExtensions(m)):
			dead = "its requires and forbids share an extension"
		case !onePerProfileKey(requiredProfiles(m)):
			dead = "it requires two tokens of one profile key"
		}
		if dead != "" {
			return &presentationBuildRefusal{Reason: "dead_manifest", Modules: []string{id},
				Detail: fmt.Sprintf("presentation module %s can never match: %s", id, dead)}
		}
	}
	for i, a := range manifests {
		for _, b := range manifests[i+1:] {
			if manifestTier(a) == manifestTier(b) && coMatchable(a, b) {
				ids := []string{manifestString(a, "id"), manifestString(b, "id")}
				sort.Strings(ids)
				return &presentationBuildRefusal{Reason: "ambiguous", Tier: manifestTier(a), Modules: ids,
					Detail: fmt.Sprintf("presentation modules %s and %s are both %s and can match the same bundle, so the page could not choose between them (priority never breaks the tie)", ids[0], ids[1], manifestTier(a))}
			}
		}
	}
	return nil
}

func runtimeVersion(text string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// runtimeRefusal is section 3.2's refusal of one module by the page's
// runtime, in its order of reasons, or nil.
func runtimeRefusal(m map[string]interface{}) *presentationBuildRefusal {
	id, api, min := manifestString(m, "id"), manifestString(m, "presentation_api"), manifestString(m, "runtime_min")
	declared := fmt.Sprintf("the page's runtime implements %s and is version %s", strings.Join(presentationRuntime.PresentationAPIs, ", "), presentationRuntime.Version)
	refusal := &presentationBuildRefusal{Module: id, PresentationAPI: api, RuntimeMin: min, Runtime: presentationRuntime}
	supported := false
	for _, a := range presentationRuntime.PresentationAPIs {
		supported = supported || a == api
	}
	if !supported {
		refusal.Reason = "presentation_api_unsupported"
		refusal.Detail = fmt.Sprintf("presentation module %s needs presentation API %s (runtime_min %s); %s", id, api, min, declared)
		return refusal
	}
	need, okNeed := runtimeVersion(min)
	have, _ := runtimeVersion(presentationRuntime.Version)
	if !okNeed || compareVersions(have, need) < 0 {
		refusal.Reason = "runtime_too_old"
		refusal.Detail = fmt.Sprintf("presentation module %s needs runtime %s or later (presentation API %s); %s", id, min, api, declared)
		return refusal
	}
	return nil
}

func compareVersions(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

type presentationBuildModule struct {
	ID           string   `json:"id"`
	Plugin       string   `json:"plugin"`
	ScriptSHA256 string   `json:"script_sha256"`
	StyleSHA256  []string `json:"style_sha256"`
	Wording      string   `json:"wording"`
}

type presentationNotIncluded struct {
	ID     string `json:"id"`
	Plugin string `json:"plugin"`
	Reason string `json:"reason"`
}

type presentationRefusedPlugin struct {
	Plugin string `json:"plugin"`
	*presentationRefusal
}

type presentationBuildResult struct {
	Page           string                      `json:"page"`
	Out            string                      `json:"out,omitempty"`
	Format         string                      `json:"format"`
	Audience       string                      `json:"audience"`
	Depth          string                      `json:"depth,omitempty"`
	BundleDigest   string                      `json:"bundle_digest,omitempty"`
	Runtime        interface{}                 `json:"runtime"`
	Modules        []presentationBuildModule   `json:"modules"`
	NotIncluded    []presentationNotIncluded   `json:"not_included,omitempty"`
	PluginsRefused []presentationRefusedPlugin `json:"plugins_refused,omitempty"`
	Refusal        *presentationBuildRefusal   `json:"refusal,omitempty"`
}

// presentationSelection is what the page will carry: the trusted-executable
// modules of the installed plugins (or those --module names), in plugin
// then manifest order, and what was left out and why.
func presentationSelection(only []string) ([]presentationModule, presentationBuildResult, error) {
	var result presentationBuildResult
	want := map[string]bool{}
	for _, id := range only {
		want[id] = true
	}
	found := map[string]bool{}
	var modules []presentationModule
	for _, p := range discoverPlugins() {
		if p.refusal != nil {
			result.PluginsRefused = append(result.PluginsRefused, presentationRefusedPlugin{Plugin: p.Name, presentationRefusal: p.refusal})
			continue
		}
		for _, m := range p.presentations {
			if len(want) > 0 && !want[m.ID] {
				continue
			}
			found[m.ID] = true
			if m.Script == nil {
				result.NotIncluded = append(result.NotIncluded, presentationNotIncluded{ID: m.ID, Plugin: p.Name,
					Reason: "a declarative module has no script for the page's module slots, and this tool does not yet package one"})
				continue
			}
			modules = append(modules, m)
		}
	}
	for _, id := range only {
		if !found[id] {
			return nil, result, hint(ErrInput, fmt.Sprintf("--module %s: no installed plugin offers a presentation module with that id that loaded (see `capsulectl plugin ls`)", id))
		}
	}
	return modules, result, nil
}

func presentationCommands() *cobra.Command {
	group := &cobra.Command{Use: "presentation", Short: "Build offline pages with the installed plugins' presentation modules"}
	build := &cobra.Command{Use: "build", Short: "Write the offline page for a bundle, with the plugins' presentation modules in its module slots", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		bundlePath, _ := c.Flags().GetString("bundle")
		out, _ := c.Flags().GetString("out")
		audience, _ := c.Flags().GetString("audience")
		format, _ := c.Flags().GetString("format")
		depth, _ := c.Flags().GetString("depth")
		title, _ := c.Flags().GetString("title")
		only, _ := c.Flags().GetStringSlice("module")
		if bundlePath == "" || out == "" {
			return hint(ErrInput, "--bundle and --out are required")
		}
		if audience == "" {
			return hint(ErrInput, "--audience must not be empty; \"*\" is no particular audience")
		}
		switch format {
		case "html":
		case "static":
			return hint(ErrInput, "--format static is unavailable (no-document): this tool has no document to render the page with at build time; build --format html")
		case "fragment", "embedded":
			return hint(ErrInput, fmt.Sprintf("--format %s is not built by this tool yet; build --format html", format))
		default:
			return hint(ErrInput, fmt.Sprintf("--format %s is not a packaging target (html, fragment, embedded, static)", format))
		}
		switch depth {
		case "", "L0", "L1", "L2":
		default:
			return hint(ErrInput, "--depth must be L0, L1 or L2")
		}
		modules, result, err := presentationSelection(only)
		if err != nil {
			return err
		}
		result.Format, result.Audience, result.Depth, result.Runtime = format, audience, depth, presentationRuntime
		result.Page, result.Modules = "not written", []presentationBuildModule{}
		refuse := func(r *presentationBuildRefusal) error {
			result.Refusal = r
			if err := output(c, result); err != nil {
				return err
			}
			return hint(ErrInput, r.Error())
		}
		// The two checks need only the manifests, so they run before the
		// bundle is read.
		manifests, err := builtinPresentationManifests()
		if err != nil {
			return err
		}
		for _, m := range modules {
			if r := runtimeRefusal(m.Manifest); r != nil {
				return refuse(r)
			}
			manifests = append(manifests, m.Manifest)
		}
		if err := staticPresentationCheck(manifests); err != nil {
			var r *presentationBuildRefusal
			if errors.As(err, &r) {
				return refuse(r)
			}
			return err
		}
		raw, err := readInput(bundlePath)
		if err != nil {
			return err
		}
		value, err := decodeBundleJSON(raw)
		if err != nil {
			return err
		}
		if result.BundleDigest, err = aacbundle.BundleDigest(value); err != nil {
			return err
		}
		// The page checks itself; it is written only when `verify --bundle`
		// calls the bundle VALID, as for every page this tool writes.
		if err := pageGate(value); err != nil {
			if outErr := output(c, result); outErr != nil {
				return outErr
			}
			return err
		}
		// One wording pack per page: with one module, its own; with several
		// none, as the page cannot know before resolving which one renders.
		var wording *emitter.Wording
		slots := make([]emitter.Module, 0, len(modules))
		for _, m := range modules {
			row := presentationBuildModule{ID: m.ID, Plugin: m.plugin, ScriptSHA256: m.Script.SHA256, StyleSHA256: []string{}, Wording: "none"}
			for _, s := range m.Styles {
				row.StyleSHA256 = append(row.StyleSHA256, s.SHA256)
			}
			if m.Wording != nil {
				row.Wording = "not passed: the page carries one wording pack, and it holds more than one module"
				if len(modules) == 1 {
					wording = &emitter.Wording{Pack: string(m.Wording.Bytes), SHA256: m.Wording.SHA256}
					row.Wording = "passed"
				}
			}
			result.Modules = append(result.Modules, row)
			slots = append(slots, emitter.Module{Code: m.Script.Bytes, SHA256: m.Script.SHA256, StyleSHA256: row.StyleSHA256})
		}
		html, err := emitter.BuildOfflineHTML(value, evidenceGraphIIFE, emitter.OfflineOptions{
			Audience: audience, Depth: depth, Title: title, Wording: wording,
			CoreRuntimeSHA256: evidenceGraphIIFEDigest, Modules: slots,
		})
		if err != nil {
			return hint(ErrInput, "no page written: "+err.Error())
		}
		if err = atomicFile(out, []byte(html), false); errors.Is(err, os.ErrExist) {
			return hint(ErrInput, fmt.Sprintf("--out %s already exists (or was created by another process while this one ran); nothing was overwritten; pass another path", out))
		} else if err != nil {
			return err
		}
		result.Page, result.Out = "written", out
		return output(c, result)
	}}
	build.Flags().String("bundle", "", "A held, disclosed Evidence Bundle (the page is written only when `verify --bundle` calls it VALID)")
	build.Flags().String("out", "", "Write the offline page here (new file)")
	build.Flags().String("audience", "*", "Audience token the page resolves for; \"*\" is no particular audience")
	build.Flags().String("format", "html", "Packaging target: html (fragment and embedded are not built yet; static is unavailable)")
	build.Flags().String("depth", "", "Depth level: L0, L1 or L2 (default: the module's own)")
	build.Flags().String("title", "", "Page title (wins over the wording pack's page.title)")
	build.Flags().StringSlice("module", nil, "Carry only this presentation module id (repeatable; default: every trusted-executable module the installed plugins offer)")
	group.AddCommand(build)
	return group
}
