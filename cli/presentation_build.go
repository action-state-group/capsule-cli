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

	"github.com/action-state-group/capsule-viewer/go/emitter"
	"github.com/spf13/cobra"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
)

// `presentation build` writes the offline page with the trusted-executable
// presentation modules of the installed plugins in its module slots. Which
// module renders is decided in the page, by the vendored runtime's resolver
// (presentation contract section 4.3); capsulectl carries no copy of it. A
// module whose manifest audiences exclude --audience can never render on the
// page, so the page does not carry it (presentationSelection). It runs only
// the two checks the contract puts on a builder that need no resolution,
// before anything is written:
//
//   - the page's runtime would refuse no module it is given (section 3.2:
//     presentation_api_unsupported, runtime_too_old), and
//   - no two manifests the page would hold, the built-ins and the given
//     modules, are of one tier and can match one descriptor (section 4.5),
//     and no id is given twice.
//
// `presentation list` (presentation_list.go) says which module a bundle
// resolves to, with agent-action-capsule's Go resolver.

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

// contractReportBuiltinID is agent-action-capsule's built-in manifest for a
// contract-report/v1 Result, and contractReportPageID the module the page's
// runtime (capsule-viewer's CONTRACT_REPORT_MANIFEST) registers in its place,
// never beside it (spec/contract-report-v1.md section 4). The manifest is
// otherwise the same, so this tool lists and checks it under the id the page
// runs.
const (
	contractReportBuiltinID = "aac.builtin.result-contract-report/v0"
	contractReportPageID    = "capsuleviewer.contract-report/v0"
)

// pageModuleID is the id the page registers for a built-in manifest's id.
func pageModuleID(id string) string {
	if id == contractReportBuiltinID {
		return contractReportPageID
	}
	return id
}

func BuiltinPresentationManifests() ([]map[string]interface{}, error) {
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
		if id, ok := m["id"].(string); ok {
			m["id"] = pageModuleID(id)
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

func ManifestString(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

func ManifestSet(v interface{}) map[string]bool {
	out := map[string]bool{}
	for _, item := range AsList(v) {
		if s, ok := item.(string); ok {
			out[s] = true
		}
	}
	return out
}

func ManifestMap(m map[string]interface{}, key string) map[string]interface{} {
	v, _ := m[key].(map[string]interface{})
	return v
}

func requiredProfiles(m map[string]interface{}) map[string]bool {
	return ManifestSet(ManifestMap(m, "requires")["profiles"])
}

func requiredExtensions(m map[string]interface{}) map[string]bool {
	return ManifestSet(ManifestMap(ManifestMap(m, "requires"), "extensions")["required"])
}

func forbiddenProfiles(m map[string]interface{}) map[string]bool {
	return ManifestSet(ManifestMap(m, "forbids")["profiles"])
}

func forbiddenExtensions(m map[string]interface{}) map[string]bool {
	return ManifestSet(ManifestMap(m, "forbids")["extensions"])
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
	if ManifestString(ManifestMap(a, "requires"), "bundle_kind") != ManifestString(ManifestMap(b, "requires"), "bundle_kind") {
		return false
	}
	audA, audB := ManifestSet(a["audiences"]), ManifestSet(b["audiences"])
	if !audA["*"] && !audB["*"] && !meets(audA, audB) {
		return false
	}
	if !meets(ManifestSet(a["formats"]), ManifestSet(b["formats"])) {
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
func StaticPresentationCheck(manifests []map[string]interface{}) error {
	seen := map[string]bool{}
	for _, m := range manifests {
		id := ManifestString(m, "id")
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
				ids := []string{ManifestString(a, "id"), ManifestString(b, "id")}
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
func RuntimeRefusal(m map[string]interface{}) *presentationBuildRefusal {
	id, api, min := ManifestString(m, "id"), ManifestString(m, "presentation_api"), ManifestString(m, "runtime_min")
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
	Page   string `json:"page"`
	Out    string `json:"out,omitempty"`
	Format string `json:"format"`
	// Fragment is the token's length and the budget it was packaged within
	// (--format fragment); Unavailable is why the packaging was not written.
	FragmentLength int              `json:"fragment_length,omitempty"`
	FragmentLimit  int              `json:"fragment_limit,omitempty"`
	Unavailable    *packagingReason `json:"unavailable,omitempty"`
	// ScriptSources and StyleSources are the CSP hash sources a host page's
	// script-src and style-src must allow for an embedded snippet.
	ScriptSources  []string                    `json:"script_sources,omitempty"`
	StyleSources   []string                    `json:"style_sources,omitempty"`
	Audience       string                      `json:"audience"`
	Depth          string                      `json:"depth,omitempty"`
	BundleDigest   string                      `json:"bundle_digest,omitempty"`
	Runtime        interface{}                 `json:"runtime"`
	Modules        []presentationBuildModule   `json:"modules"`
	NotIncluded    []presentationNotIncluded   `json:"not_included,omitempty"`
	PluginsRefused []presentationRefusedPlugin `json:"plugins_refused,omitempty"`
	Refusal        *presentationBuildRefusal   `json:"refusal,omitempty"`
}

// excludesAudience is the audience condition of the resolver's declarative
// match (agent-action-capsule go/presentation Matches, section 4.3 step 2):
// a manifest whose audiences hold neither "*" nor the page's audience never
// renders on that page.
func excludesAudience(m map[string]interface{}, audience string) bool {
	audiences := ManifestSet(m["audiences"])
	return !audiences["*"] && !audiences[audience]
}

func audienceList(m map[string]interface{}) string {
	var out []string
	for _, a := range AsList(m["audiences"]) {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

// presentationSelection is what the page will carry: the trusted-executable
// modules of the installed plugins (or those --module names) that can render
// for the audience, in plugin then manifest order, and what was left out and
// why. A module for other audiences is left out, so its script never reaches
// a page built for someone it is not for; naming one with --module is
// refused.
func presentationSelection(only []string, audience string) ([]presentationModule, presentationBuildResult, error) {
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
			if excludesAudience(m.Manifest, audience) {
				if want[m.ID] {
					return nil, result, Hint(ErrInput, fmt.Sprintf("--module %s: the module is for audiences %s, not --audience %s; no page written", m.ID, audienceList(m.Manifest), audience))
				}
				result.NotIncluded = append(result.NotIncluded, presentationNotIncluded{ID: m.ID, Plugin: p.Name,
					Reason: fmt.Sprintf("the module is for audiences %s, not %s, so the page leaves it out", audienceList(m.Manifest), audience)})
				continue
			}
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
			return nil, result, Hint(ErrInput, fmt.Sprintf("--module %s: no installed plugin offers a presentation module with that id that loaded (see `capsulectl plugin ls`)", id))
		}
	}
	return modules, result, nil
}

// packagingTargets are the presentation builder's packaging targets, in the
// order the builder's availability lists them (presentation builder section 1.1).
var packagingTargets = []string{"html", "fragment", "embedded", "static"}

// packagingReason is why a packaging is unavailable, in the builder's terms
// (section 1.1): fragment-too-large, with what was too long, its length and
// the limit; or no-document for static, which this tool cannot render.
type packagingReason struct {
	Code    string `json:"code"`
	Subject string `json:"subject,omitempty"`
	Length  int    `json:"length,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Message string `json:"message"`
}

func (r *packagingReason) Error() string { return r.Message }

const noDocumentMessage = "format static is unavailable (no-document): this tool has no document to render the page with at build time; build --format html"

// presentationSettings are the flags every packaging of a page is built
// with; `presentation build` and `presentation list` read the same ones, so
// list's availability is build's.
type presentationSettings struct {
	audience, depth, title string
	only                   []string
	maxFragmentLength      int
	viewerURL              string
}

func addPresentationSettingsFlags(c *cobra.Command) {
	c.Flags().String("depth", "", "Depth level: L0, L1 or L2 (default: the module's own)")
	c.Flags().String("title", "", "Page title (wins over the wording pack's page.title; html and fragment only: an embedded snippet takes its host page's title)")
	c.Flags().StringSlice("module", nil, "Carry only this presentation module id (repeatable; default: every trusted-executable module the installed plugins offer for --audience)")
	c.Flags().Int("max-fragment-length", 0, fmt.Sprintf("fragment only: the token budget in characters (default %d, at most %d)", emitter.FragmentTokenDefaultBudget, emitter.FragmentTokenMaxLength))
	c.Flags().String("viewer-url", "", "fragment only: the viewer address the token is appended to; the page is then the whole URL, at most 1 MiB")
}

func readPresentationSettings(c *cobra.Command) (presentationSettings, error) {
	var s presentationSettings
	s.audience, _ = c.Flags().GetString("audience")
	s.depth, _ = c.Flags().GetString("depth")
	s.title, _ = c.Flags().GetString("title")
	s.only, _ = c.Flags().GetStringSlice("module")
	s.maxFragmentLength, _ = c.Flags().GetInt("max-fragment-length")
	s.viewerURL, _ = c.Flags().GetString("viewer-url")
	if s.audience == "" {
		return s, Hint(ErrInput, "--audience must not be empty; \"*\" is no particular audience")
	}
	switch s.depth {
	case "", "L0", "L1", "L2":
	default:
		return s, Hint(ErrInput, "--depth must be L0, L1 or L2")
	}
	if s.maxFragmentLength < 0 || s.maxFragmentLength > emitter.FragmentTokenMaxLength {
		return s, Hint(ErrInput, fmt.Sprintf("--max-fragment-length must be from 1 to %d", emitter.FragmentTokenMaxLength))
	}
	if strings.Contains(s.viewerURL, "#") {
		return s, Hint(ErrInput, "--viewer-url must not already carry a fragment")
	}
	return s, nil
}

// pageGateRefusal is the page gate's error: the bundle is not one any page
// may be written for.
type pageGateRefusal struct{ err error }

func (e *pageGateRefusal) Error() string { return e.err.Error() }
func (e *pageGateRefusal) Unwrap() error { return e.err }

// preparePresentationPage runs everything a page of any packaging waits on,
// in order: the module selection; the runtime's refusals and the registry
// checks, which need only the manifests and so run before the bundle is read;
// the page gate; and the wording rule. It fills result as it goes. Its error
// is a *presentationBuildRefusal, a *pageGateRefusal or the command's own.
func preparePresentationPage(s presentationSettings, readBundle func() (map[string]interface{}, error), result *presentationBuildResult) (map[string]interface{}, emitter.OfflineOptions, error) {
	var options emitter.OfflineOptions
	modules, selection, err := presentationSelection(s.only, s.audience)
	result.NotIncluded, result.PluginsRefused = selection.NotIncluded, selection.PluginsRefused
	if err != nil {
		return nil, options, err
	}
	manifests, err := BuiltinPresentationManifests()
	if err != nil {
		return nil, options, err
	}
	for _, m := range modules {
		if r := RuntimeRefusal(m.Manifest); r != nil {
			return nil, options, r
		}
		manifests = append(manifests, m.Manifest)
	}
	if err := StaticPresentationCheck(manifests); err != nil {
		return nil, options, err
	}
	value, err := readBundle()
	if err != nil {
		return nil, options, err
	}
	if result.BundleDigest, err = aacbundle.BundleDigest(value); err != nil {
		return nil, options, err
	}
	// The page checks itself; it is written only when `verify --bundle`
	// calls the bundle VALID, as for every page this tool writes.
	if err := PageGate(value); err != nil {
		return nil, options, &pageGateRefusal{err}
	}
	// One wording pack per page: with one module, its own; with several
	// none, as the page cannot know before resolving which one renders.
	var wording *emitter.Wording
	slots := make([]emitter.Module, 0, len(modules))
	for _, m := range modules {
		row := presentationBuildModule{ID: m.ID, Plugin: m.plugin, ScriptSHA256: m.Script.SHA256, StyleSHA256: []string{}, Wording: "none"}
		for _, st := range m.Styles {
			row.StyleSHA256 = append(row.StyleSHA256, st.SHA256)
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
	return value, emitter.OfflineOptions{
		Audience: s.audience, Depth: s.depth, Title: s.title, Wording: wording,
		CoreRuntimeSHA256: EvidenceGraphIIFEDigest, Modules: slots,
	}, nil
}

// packagePresentation packages a prepared page as one target, the Go
// builder's (capsule-viewer go/emitter). Its error is a *packagingReason
// when the target is unavailable, or the builder's refusal of the request.
func packagePresentation(value map[string]interface{}, options emitter.OfflineOptions, s presentationSettings, format string, result *presentationBuildResult) ([]byte, error) {
	switch format {
	case "html":
		html, err := emitter.BuildOfflineHTML(value, EvidenceGraphIIFE, options)
		return []byte(html), err
	case "fragment":
		// The fragment's presentation member is informational; the page's
		// runtime resolves when it opens, so this tool names none.
		built, err := emitter.BuildPresentationFragment(value, EvidenceGraphIIFE, emitter.FragmentOptions{
			OfflineOptions: options, MaxLength: s.maxFragmentLength, ViewerURL: s.viewerURL,
		})
		var tooLarge *emitter.FragmentTooLargeError
		if errors.As(err, &tooLarge) {
			return nil, &packagingReason{Code: "fragment-too-large", Subject: tooLarge.Subject, Length: tooLarge.Length, Limit: tooLarge.MaxLength, Message: tooLarge.Error()}
		} else if err != nil {
			return nil, err
		}
		result.FragmentLength = len(built.Fragment)
		result.FragmentLimit = s.maxFragmentLength
		if result.FragmentLimit == 0 {
			result.FragmentLimit = emitter.FragmentTokenDefaultBudget
		}
		if built.URL != "" {
			return []byte(built.URL + "\n"), nil
		}
		return []byte(built.Fragment + "\n"), nil
	case "embedded":
		built, err := emitter.BuildEmbeddedHTML(value, EvidenceGraphIIFE, options)
		if err != nil {
			return nil, err
		}
		result.ScriptSources, result.StyleSources = built.ScriptSources, built.StyleSources
		return []byte(built.HTML), nil
	case "static":
		return nil, &packagingReason{Code: "no-document", Message: noDocumentMessage}
	}
	return nil, fmt.Errorf("unknown packaging target %s", format)
}

func presentationCommands() *cobra.Command {
	group := &cobra.Command{Use: "presentation", Short: "Build pages with the installed plugins' presentation modules, and say which module a bundle resolves to and which packagings are available"}
	build := &cobra.Command{Use: "build", Short: "Write a bundle's page as an offline file, a fragment permalink or an embedded snippet, with the plugins' presentation modules in its module slots", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		bundlePath, _ := c.Flags().GetString("bundle")
		out, _ := c.Flags().GetString("out")
		format, _ := c.Flags().GetString("format")
		if bundlePath == "" || out == "" {
			return Hint(ErrInput, "--bundle and --out are required")
		}
		s, err := readPresentationSettings(c)
		if err != nil {
			return err
		}
		switch format {
		case "html", "fragment", "embedded":
		case "static":
			return Hint(ErrInput, "--"+noDocumentMessage)
		default:
			return Hint(ErrInput, fmt.Sprintf("--format %s is not a packaging target (html, fragment, embedded, static)", format))
		}
		result := presentationBuildResult{Format: format, Audience: s.audience, Depth: s.depth, Runtime: presentationRuntime,
			Page: "not written", Modules: []presentationBuildModule{}}
		value, options, err := preparePresentationPage(s, func() (map[string]interface{}, error) {
			raw, err := ReadBundleInput(bundlePath)
			if err != nil {
				return nil, err
			}
			return DecodeBundleJSON(raw)
		}, &result)
		var refusal *presentationBuildRefusal
		var gate *pageGateRefusal
		switch {
		case errors.As(err, &refusal):
			result.Refusal = refusal
			if err := Output(c, result); err != nil {
				return err
			}
			return Hint(ErrInput, refusal.Error())
		case errors.As(err, &gate):
			if outErr := Output(c, result); outErr != nil {
				return outErr
			}
			return gate.err
		case err != nil:
			return err
		}
		page, err := packagePresentation(value, options, s, format, &result)
		var unavailable *packagingReason
		if errors.As(err, &unavailable) {
			result.Unavailable = unavailable
			if err := Output(c, result); err != nil {
				return err
			}
			return Hint(ErrInput, fmt.Sprintf("--format %s is unavailable (%s): %s; nothing written", format, unavailable.Code, unavailable.Message))
		} else if err != nil {
			return Hint(ErrInput, "no page written: "+err.Error())
		}
		if err = AtomicFile(out, page, false); errors.Is(err, os.ErrExist) {
			return Hint(ErrInput, fmt.Sprintf("--out %s already exists (or was created by another process while this one ran); nothing was overwritten; pass another path", out))
		} else if err != nil {
			return err
		}
		result.Page, result.Out = "written", out
		return Output(c, result)
	}}
	build.Flags().String("bundle", "", "A held, disclosed Evidence Bundle (the page is written only when `verify --bundle` calls it VALID)")
	build.Flags().String("out", "", "Write the page here (new file): the offline .html, the fragment token (or URL) with a newline, or the embedded snippet")
	build.Flags().String("audience", "*", "Audience token the page resolves for; \"*\" is no particular audience")
	build.Flags().String("format", "html", "Packaging target: html (offline file), fragment (permalink), embedded (host-page snippet); static is unavailable")
	addPresentationSettingsFlags(build)
	group.AddCommand(build, presentationListCommand())
	return group
}
