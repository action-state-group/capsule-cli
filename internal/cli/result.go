package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// resultShapeNote explains why `result open` only does a structural
// best-effort check rather than schema validation: Result v0
// (batch4-evidence-result-schema-v0, spec lane) is still DRAFT pending
// Steven's sign-off on its semantics section, so no frozen schema exists yet
// to validate against. This command checks for the one shape that item's own
// text already commits to regardless of the open questions -- a mandatory
// `aggregate.coverage` statement ("an aggregate without coverage fails
// validation") -- and nothing more. It also stands in for the real viewer
// (`[batch4-capsule-viewer-three-buckets]`, not yet built) by printing the
// aggregate and coverage as text.
const resultShapeNote = "Result v0 schema is still DRAFT (batch4-evidence-result-schema-v0, spec lane); this is a best-effort structural check, not schema validation"

func decodeResultDocument(raw []byte) (map[string]interface{}, error) {
	if len(raw) > maxInput {
		return nil, inputError("input exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc map[string]interface{}
	if err := decoder.Decode(&doc); err != nil || doc == nil {
		return nil, inputError("Result v0 document must be a JSON object")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, inputError("input must contain one JSON value")
	}
	return doc, nil
}

// validateResultDocument enforces only the one requirement the DRAFT Result
// v0 item already states plainly: an aggregate carrying a coverage statement.
func validateResultDocument(doc map[string]interface{}) (aggregate, coverage map[string]interface{}, err error) {
	aggregate, ok := doc["aggregate"].(map[string]interface{})
	if !ok {
		return nil, nil, inputError("Result v0 requires a top-level aggregate object")
	}
	coverage, ok = aggregate["coverage"].(map[string]interface{})
	if !ok {
		return nil, nil, inputError("Result v0 requires aggregate.coverage (an aggregate without a coverage statement fails validation)")
	}
	return aggregate, coverage, nil
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func renderResultText(aggregate, coverage map[string]interface{}) string {
	var b strings.Builder
	fmt.Fprintln(&b, "Aggregate:")
	for _, key := range sortedKeys(aggregate) {
		if key == "coverage" {
			continue
		}
		fmt.Fprintf(&b, "  %s: %v\n", key, aggregate[key])
	}
	fmt.Fprintln(&b, "Coverage:")
	for _, key := range sortedKeys(coverage) {
		fmt.Fprintf(&b, "  %s: %v\n", key, coverage[key])
	}
	return b.String()
}

// resultCommands implements `result open`, text mode at minimum: until
// capsule-viewer exists, this validates the one settled shape requirement and
// prints the aggregate/coverage statement itself rather than rendering it.
func resultCommands() *cobra.Command {
	group := &cobra.Command{Use: "result", Short: "Inspect a Result v0 document"}
	open := &cobra.Command{
		Use:   "open FILE",
		Short: "Validate + print a Result v0's aggregate and coverage statement (text mode: capsule-viewer not yet built, batch4-capsule-viewer-three-buckets)",
		Args:  oneArg,
		RunE: func(c *cobra.Command, args []string) error {
			format, _ := c.Flags().GetString("format")
			if format != "text" && format != "json" {
				return inputError("--format must be text or json")
			}
			raw, e := readInput(args[0])
			if e != nil {
				return e
			}
			doc, e := decodeResultDocument(raw)
			if e != nil {
				return e
			}
			aggregate, coverage, e := validateResultDocument(doc)
			if e != nil {
				return e
			}
			if format == "json" {
				return output(c, map[string]any{"aggregate": aggregate, "note": resultShapeNote})
			}
			if _, e := fmt.Fprint(c.OutOrStdout(), renderResultText(aggregate, coverage)); e != nil {
				return e
			}
			_, e = fmt.Fprintln(c.OutOrStdout(), "note: "+resultShapeNote)
			return e
		},
	}
	open.Flags().String("format", "text", "Output format: text or json")
	group.AddCommand(open)
	return group
}
