package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/spf13/cobra"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	aacpresentation "github.com/action-state-group/agent-action-capsule/go/presentation"
)

// `presentation list` says which presentation module a bundle resolves to,
// among every module this capsulectl can put in a page: the page's built-ins,
// its own deal view (capsulectl.deal-view/v0) and the modules the installed
// plugins offer. Every decision about those modules is agent-action-capsule's
// Go resolver's (go/presentation): the descriptor (section 4.2), the
// runtime's refusals (section 3.2), the ambiguity test at registration
// (section 4.5) and the resolution (section 4.3).
//
// One choice is this file's, and it is about which modules are candidates,
// not how they resolve: the deal view is a default that yields to an
// installed module. When the resolver would refuse to hold a plugin's module
// beside the deal view (same tier, and one descriptor can match both, as
// capsule-viewer's unilateral and seller receipts do),
// the deal view is left out and the plugin's module is the candidate. With no such module installed, the deal
// view is a candidate as before.

// presentationListRuntime is the page runtime's declaration
// (presentationRuntime) as the resolver takes it.
func presentationListRuntime() aacpresentation.Runtime {
	return aacpresentation.Runtime{
		PresentationAPIs: append([]string{}, presentationRuntime.PresentationAPIs...),
		RuntimeVersion:   presentationRuntime.Version,
	}
}

// presentationCandidate is one manifest the registry is given, and where it
// comes from: "builtin" (the page's built-ins), "capsulectl" (this tool's
// deal view) or "plugin".
type presentationCandidate struct {
	manifest aacpresentation.Manifest
	source   string
	plugin   string
}

// presentationListCandidates is presentationCandidates. A test replaces it;
// nothing else does.
var presentationListCandidates = presentationCandidates

// presentationCandidates is the page's built-ins, the deal view unless an
// installed module displaces it, and the installed plugins' modules, in that
// order, and the plugins whose presentations were refused.
func presentationCandidates() ([]presentationCandidate, []presentationRefusedPlugin, error) {
	var out []presentationCandidate
	entries, err := builtinPresentationFS.ReadDir("assets/presentation/builtin")
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		raw, err := builtinPresentationFS.ReadFile(path.Join("assets/presentation/builtin", entry.Name()))
		if err != nil {
			return nil, nil, err
		}
		m, err := aacpresentation.ParseManifest(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("built-in presentation manifest %s: %w", entry.Name(), err)
		}
		out = append(out, presentationCandidate{manifest: m, source: "builtin"})
	}
	dealView, err := aacpresentation.ParseManifest(DealViewManifestJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("deal view manifest: %w", err)
	}
	var plugins []presentationCandidate
	var refused []presentationRefusedPlugin
	for _, p := range discoverPlugins() {
		if p.refusal != nil {
			refused = append(refused, presentationRefusedPlugin{Plugin: p.Name, presentationRefusal: p.refusal})
			continue
		}
		for _, module := range p.presentations {
			raw, err := json.Marshal(module.Manifest)
			if err != nil {
				return nil, nil, err
			}
			m, err := aacpresentation.ParseManifest(raw)
			if err != nil {
				// The registry's structural rules (a dead manifest, say) are
				// stricter than the schema the plugin loader checks.
				return nil, refused, &presentationListRegistration{Module: module.ID, Plugin: p.Name, err: err}
			}
			plugins = append(plugins, presentationCandidate{manifest: m, source: "plugin", plugin: p.Name})
		}
	}
	isDisplaced, err := displaced(dealView, plugins)
	if err != nil {
		return nil, nil, fmt.Errorf("deal view manifest: %w", err)
	}
	if !isDisplaced {
		out = append(out, presentationCandidate{manifest: dealView, source: "capsulectl"})
	}
	return append(out, plugins...), refused, nil
}

// displaced is whether the resolver would refuse to hold m beside one of the
// plugins' modules: registering both in one registry is the resolver's
// ambiguity error (section 4.5). The test is the resolver's own Register, so
// list cannot drift from it. A module the runtime refuses still displaces m:
// Register applies the ambiguity test before the runtime's refusal, refused
// or not. Any other error registering a plugin's module (a duplicate of m's
// id) displaces nothing; the command's own registration reports it. The error
// is m's own, as m is registered first.
func displaced(m aacpresentation.Manifest, plugins []presentationCandidate) (bool, error) {
	for _, c := range plugins {
		registry := aacpresentation.NewRegistry(presentationListRuntime())
		if _, err := registry.Register(m); err != nil {
			return false, err
		}
		_, err := registry.Register(c.manifest)
		var ambiguity *aacpresentation.AmbiguityError
		if errors.As(err, &ambiguity) {
			return true, nil
		}
	}
	return false, nil
}

// presentationListRegistration is a manifest the registry refused to hold:
// malformed, dead or a duplicate id (the resolver's RegistrationError).
type presentationListRegistration struct {
	Module string `json:"module"`
	Plugin string `json:"plugin,omitempty"`
	err    error
}

func (r *presentationListRegistration) Error() string { return r.err.Error() }

type presentationListModule struct {
	ID         string                   `json:"id"`
	Source     string                   `json:"source"`
	Plugin     string                   `json:"plugin,omitempty"`
	TrustClass string                   `json:"trust_class"`
	Fallback   bool                     `json:"fallback"`
	Matches    bool                     `json:"matches"`
	Refusal    *aacpresentation.Refusal `json:"refusal,omitempty"`
}

type presentationListRefused struct {
	aacpresentation.Refusal
	Line string `json:"line"`
}

type presentationListAmbiguity struct {
	// Stage is registration (section 4.5) or resolution (section 4.3).
	Stage   string   `json:"stage"`
	Modules []string `json:"modules"`
	Detail  string   `json:"detail"`
}

type presentationListResult struct {
	BundleDigest string                     `json:"bundle_digest"`
	Audience     string                     `json:"audience"`
	Format       string                     `json:"format"`
	Runtime      aacpresentation.Runtime    `json:"runtime"`
	Descriptor   aacpresentation.Descriptor `json:"descriptor"`
	// Resolution is the resolver's outcome: module, no-presentation or
	// refusal; or ambiguous, its named error, or registration_refused.
	Resolution     string                        `json:"resolution"`
	Selected       *presentationListModule       `json:"selected"`
	Refused        []presentationListRefused     `json:"refused"`
	Ambiguity      *presentationListAmbiguity    `json:"ambiguity,omitempty"`
	Registration   *presentationListRegistration `json:"registration_refused,omitempty"`
	Modules        []presentationListModule      `json:"modules"`
	PluginsRefused []presentationRefusedPlugin   `json:"plugins_refused,omitempty"`
	Detail         string                        `json:"detail"`
}

// listPresentations resolves value for audience and format. The error is
// nil, or the error the command exits with after it shows the result.
func listPresentations(value map[string]interface{}, audience string, format aacpresentation.Format) (presentationListResult, error) {
	runtime := presentationListRuntime()
	result := presentationListResult{Audience: audience, Format: string(format), Runtime: runtime,
		Refused: []presentationListRefused{}, Modules: []presentationListModule{}}
	var err error
	if result.BundleDigest, err = aacbundle.BundleDigest(value); err != nil {
		return result, err
	}
	result.Descriptor, _ = aacpresentation.DescribeBundle(value)
	// A History's deals are checked by capsule-cli's verifier, not by the
	// bundle verifier the descriptor runs: it is verified only when
	// `verify --bundle` calls it VALID, as the page gate requires.
	if exts, _ := value["extensions"].(map[string]interface{}); exts[HistorySelectionExtension] != nil && result.Descriptor.Verified {
		verdict, _ := BundleVerdict(value, nil)
		result.Descriptor.Verified = verdict == "VALID"
	}
	candidates, pluginsRefused, err := presentationListCandidates()
	result.PluginsRefused = pluginsRefused
	var registration *presentationListRegistration
	if errors.As(err, &registration) {
		return registrationRefused(result, registration)
	} else if err != nil {
		return result, err
	}
	registry := aacpresentation.NewRegistry(runtime)
	for _, c := range candidates {
		// The resolver matches nothing in a bundle that did not verify
		// (section 4.3, step 1).
		row := presentationListModule{ID: c.manifest.ID, Source: c.source, Plugin: c.plugin, TrustClass: c.manifest.TrustClass,
			Fallback: c.manifest.Fallback, Matches: result.Descriptor.Verified && aacpresentation.Matches(c.manifest, result.Descriptor, audience, format)}
		reg, err := registry.Register(c.manifest)
		var ambiguity *aacpresentation.AmbiguityError
		switch {
		case errors.As(err, &ambiguity):
			result.Modules = append(result.Modules, row)
			return ambiguous(result, "registration", ambiguity)
		case err != nil:
			return registrationRefused(result, &presentationListRegistration{Module: c.manifest.ID, Plugin: c.plugin, err: err})
		}
		if reg.Refused {
			refusal := reg.Refusal
			row.Refusal = &refusal
		}
		result.Modules = append(result.Modules, row)
	}
	resolution, err := registry.Resolve(result.Descriptor, audience, format)
	var ambiguity *aacpresentation.AmbiguityError
	if errors.As(err, &ambiguity) {
		return ambiguous(result, "resolution", ambiguity)
	} else if err != nil {
		return result, err
	}
	result.Resolution = string(resolution.Kind)
	for _, r := range resolution.Refused {
		result.Refused = append(result.Refused, presentationListRefused{Refusal: r, Line: aacpresentation.RefusalLine(r)})
	}
	switch resolution.Kind {
	case aacpresentation.ResolutionModule:
		for i := range result.Modules {
			if result.Modules[i].ID == resolution.Manifest.ID {
				selected := result.Modules[i]
				result.Selected = &selected
			}
		}
		result.Detail = fmt.Sprintf("the bundle resolves to %s for audience %s, format %s", resolution.Manifest.ID, audience, format)
	case aacpresentation.ResolutionNone:
		result.Detail = fmt.Sprintf("no presentation module matches the bundle for audience %s, format %s; a page shows its \"no presentation\" notice", audience, format)
	case aacpresentation.ResolutionRefusal:
		result.Detail = "the bundle does not pass the viewer's verification gate, so no presentation module is matched; run `capsulectl verify --bundle` for why"
		return result, Hint(ErrBundleInvalid, "no presentation: "+result.Detail)
	}
	return result, nil
}

func ambiguous(result presentationListResult, stage string, err *aacpresentation.AmbiguityError) (presentationListResult, error) {
	result.Resolution = "ambiguous"
	result.Ambiguity = &presentationListAmbiguity{Stage: stage, Modules: err.IDs, Detail: err.Error()}
	result.Detail = err.Error()
	return result, Hint(ErrInput, "no presentation resolved: "+err.Error())
}

func registrationRefused(result presentationListResult, r *presentationListRegistration) (presentationListResult, error) {
	result.Resolution = "registration_refused"
	result.Registration = r
	result.Detail = r.Error()
	return result, Hint(ErrInput, "no presentation resolved: "+r.Error())
}

// printPresentationList is the short human form of a result.
func printPresentationList(w io.Writer, r presentationListResult) {
	fmt.Fprintf(w, "%s: %s\n", r.Resolution, r.Detail)
	if r.Selected != nil {
		fmt.Fprintf(w, "selected: %s\n", moduleLine(*r.Selected))
	}
	for _, refused := range r.Refused {
		fmt.Fprintf(w, "refused: %s\n", refused.Line)
	}
	for _, m := range r.Modules {
		mark := " "
		if m.Matches {
			mark = "*"
		}
		line := mark + " " + moduleLine(m)
		if m.Refusal != nil {
			line += " [refused: " + string(m.Refusal.Reason) + "]"
		}
		fmt.Fprintln(w, line)
	}
	for _, p := range r.PluginsRefused {
		fmt.Fprintf(w, "plugin %s: presentations refused (%s): %s\n", p.Plugin, p.Reason, p.Detail)
	}
}

func moduleLine(m presentationListModule) string {
	source := m.Source
	if m.Plugin != "" {
		source += " " + m.Plugin
	}
	tier := "specific"
	if m.Fallback {
		tier = "fallback"
	}
	return strings.Join([]string{m.ID, "(" + source + ", " + m.TrustClass + ", " + tier + ")"}, " ")
}

func presentationListCommand() *cobra.Command {
	list := &cobra.Command{Use: "list", Short: "Say which presentation module a bundle resolves to, among the built-ins, the deal view and the plugins' modules", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		bundlePath, _ := c.Flags().GetString("bundle")
		audience, _ := c.Flags().GetString("audience")
		format, _ := c.Flags().GetString("format")
		text, _ := c.Flags().GetBool("text")
		if bundlePath == "" {
			return Hint(ErrInput, "--bundle is required")
		}
		if audience == "" {
			return Hint(ErrInput, "--audience must not be empty; \"*\" is no particular audience")
		}
		known := false
		for _, f := range aacpresentation.Formats {
			known = known || string(f) == format
		}
		if !known {
			return Hint(ErrInput, fmt.Sprintf("--format %s is not a presentation format (html, fragment, embedded)", format))
		}
		raw, err := ReadBundleInput(bundlePath)
		if err != nil {
			return err
		}
		value, err := DecodeBundleJSON(raw)
		if err != nil {
			return err
		}
		result, listErr := listPresentations(value, audience, aacpresentation.Format(format))
		if result.Resolution == "" && listErr != nil {
			return listErr
		}
		if text {
			printPresentationList(c.OutOrStdout(), result)
		} else if err := Output(c, result); err != nil {
			return err
		}
		return listErr
	}}
	list.Flags().String("bundle", "", "An Evidence Bundle to resolve")
	list.Flags().String("audience", "*", "Audience token to resolve for; \"*\" is no particular audience")
	list.Flags().String("format", "html", "Presentation format: html, fragment or embedded")
	list.Flags().Bool("text", false, "Print a short readable summary instead of the capsule-cli-result/v1 report")
	return list
}
