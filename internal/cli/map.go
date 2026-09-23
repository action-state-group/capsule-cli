package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/spf13/cobra"
)

// declaredEvidenceClass returns the set of source/tool names a requirement
// itself declares -- never a name map invents. `--schema` stays caller-
// supplied and generic, exactly like `contract validate`; map does not
// embed the Evidence Contract v0 schema. But joining needs SOME vocabulary
// for "declared evidence class", and this is the one this repo's own
// Evidence Contract v0 fixture (testdata/contract/evidence-contract-v0.json)
// already uses for it, wherever a requirement shape puts it: the
// generic/portable profiles' `evidence_requirements.required_sources[]`, and
// the native/flat profile's `evidence_instrument.name` (tool_call_name) or
// `.field` (structured_field). A requirement using neither field declares no
// evidence class at all, and gets no candidates -- never a default.
func declaredEvidenceClass(req map[string]any) []string {
	var names []string
	if er, ok := req["evidence_requirements"].(map[string]any); ok {
		if sources, ok := er["required_sources"].([]any); ok {
			for _, s := range sources {
				if name, ok := s.(string); ok && name != "" {
					names = append(names, name)
				}
			}
		}
	}
	if instrument, ok := req["evidence_instrument"].(map[string]any); ok {
		for _, key := range []string{"name", "field"} {
			if name, ok := instrument[key].(string); ok && name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// loadDiscoverEffects reads a `discover --effects --format json` output file.
// Unknown fields (`sealed`, `spec_version`) are ignored on purpose: map only
// ever needs the effects[] array, and tolerating the rest lets a hand-built
// fixture omit them.
func loadDiscoverEffects(path string) ([]effectBoundary, error) {
	raw, err := readInput(path)
	if err != nil {
		return nil, err
	}
	var inventory struct {
		Effects []effectBoundary `json:"effects"`
	}
	if err := json.Unmarshal(raw, &inventory); err != nil {
		return nil, inputError("--discover must be discover --effects --format json output (invalid JSON)")
	}
	if inventory.Effects == nil {
		return nil, inputError("--discover must be discover --effects --format json output (no effects[] array found)")
	}
	return inventory.Effects, nil
}

// matchingEffects is a pure exact-name join: a discovered effect satisfies a
// requirement only when its Surface is byte-identical to one of the
// requirement's own declared names. No fuzzy match, no synonym table, no
// SurfaceType-implied substitution -- map never infers semantic equivalence.
func matchingEffects(names []string, effects []effectBoundary) []effectBoundary {
	if len(names) == 0 {
		return nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var matches []effectBoundary
	for _, e := range effects {
		if want[e.Surface] {
			matches = append(matches, e)
		}
	}
	return matches
}

type mapRequirementResult struct {
	ID            string
	DeclaredNames []string
	Matches       []effectBoundary
}

func mapRequirements(doc any, effects []effectBoundary) ([]mapRequirementResult, error) {
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, inputError("validated document is not a JSON object")
	}
	reqsRaw, ok := root["requirements"].([]any)
	if !ok {
		return nil, inputError("validated document has no top-level requirements[] array; map expects an Evidence Contract shape (id, version, requirements[])")
	}
	results := make([]mapRequirementResult, 0, len(reqsRaw))
	for _, raw := range reqsRaw {
		req, ok := raw.(map[string]any)
		if !ok {
			return nil, inputError("a requirements[] entry is not a JSON object")
		}
		id, _ := req["id"].(string)
		names := declaredEvidenceClass(req)
		results = append(results, mapRequirementResult{ID: id, DeclaredNames: names, Matches: matchingEffects(names, effects)})
	}
	return results, nil
}

func renderMapText(results []mapRequirementResult) string {
	var b strings.Builder
	for _, r := range results {
		if len(r.Matches) == 0 {
			reason := "no evidence class declared"
			if len(r.DeclaredNames) > 0 {
				reason = "declared: " + strings.Join(r.DeclaredNames, ", ")
			}
			fmt.Fprintf(&b, "%s: no source found (%s)\n", r.ID, reason)
			continue
		}
		sources := make([]string, len(r.Matches))
		for i, m := range r.Matches {
			sources[i] = fmt.Sprintf("%s (%s)", m.Surface, m.Source)
		}
		fmt.Fprintf(&b, "%s: %s\n", r.ID, strings.Join(sources, "; "))
	}
	return b.String()
}

func mapResultRows(results []mapRequirementResult) []map[string]any {
	rows := make([]map[string]any, len(results))
	for i, r := range results {
		matches := make([]map[string]any, len(r.Matches))
		for j, m := range r.Matches {
			matches[j] = map[string]any{"source": m.Source, "surface": m.Surface, "surface_type": m.SurfaceType}
		}
		rows[i] = map[string]any{"id": r.ID, "declared_names": r.DeclaredNames, "matches": matches, "found": len(matches) > 0}
	}
	return rows
}

// mapCommand implements Steven's 2026-09-22 ruling: a deterministic,
// read-only join of a validated Evidence Contract to a discover inventory.
// It emits nothing (no --seal-output; unlike discover, a map run leaves no
// capsule) and is never licence-gated -- only execution capabilities
// (`run --dry-run`, which may consume this output) are.
func mapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "map CONTRACT",
		Short: "Read-only join: for each requirement, discovered systems whose declared evidence class matches (never infers equivalence; \"no source found\" otherwise)",
		Args:  oneArg,
		RunE: func(c *cobra.Command, args []string) error {
			schemaLoc, _ := c.Flags().GetString("schema")
			if schemaLoc == "" {
				return inputError("--schema is required (a path or URL to a JSON Schema, like contract validate)")
			}
			discoverPath, _ := c.Flags().GetString("discover")
			if discoverPath == "" {
				return inputError("--discover is required (a discover --effects --format json output file)")
			}
			format, _ := c.Flags().GetString("format")
			if format != "text" && format != "json" {
				return inputError("--format must be text or json")
			}

			raw, e := readInput(args[0])
			if e != nil {
				return e
			}
			doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if e != nil {
				return inputError("malformed JSON in " + args[0])
			}
			schema, schemaRaw, e := loadSchema(schemaLoc)
			if e != nil {
				return errors.Join(ErrInput, &schemaLoadError{loc: schemaLoc, err: e})
			}
			report := validateContract(schema, schemaRaw, doc)
			if !report.valid {
				printReport(c, args[0], report)
				return ErrSchemaInvalid
			}

			effects, e := loadDiscoverEffects(discoverPath)
			if e != nil {
				return e
			}
			results, e := mapRequirements(doc, effects)
			if e != nil {
				return e
			}

			if format == "json" {
				return output(c, map[string]any{"requirements": mapResultRows(results)})
			}
			_, e = fmt.Fprint(c.OutOrStdout(), renderMapText(results))
			return e
		},
	}
	cmd.Flags().String("schema", "", "Path or URL to the JSON Schema the contract must satisfy (required; capsulectl embeds no schema of its own)")
	cmd.Flags().String("discover", "", "discover --effects --format json output file (required)")
	cmd.Flags().String("format", "text", "Output format: text or json")
	return cmd
}
