package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/spf13/cobra"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// ErrSchemaInvalid reports that a document was well-formed JSON but did not
// satisfy the schema it was pointed at -- an ordinary, expected outcome (exit
// code 1), never the generic operational failure SafeError otherwise reports.
var ErrSchemaInvalid = errors.New("document does not satisfy the schema")

// maxSchemaFetch bounds a --schema URL/file read: a JSON Schema document, not
// an arbitrary data payload, so a small ceiling stops a misconfigured URL from
// stalling on an unbounded body.
const maxSchemaFetch = 4 << 20

var printer = message.NewPrinter(language.English)

// schemaLoadError reports a --schema value (path or URL) that could not be
// loaded or compiled. Like inputFileError, --schema is caller-typed on the
// command line, so naming it in full discloses nothing SafeError needs to
// suppress.
type schemaLoadError struct {
	loc string
	err error
}

func (e *schemaLoadError) Error() string {
	return "cannot load --schema " + e.loc + ": " + e.err.Error()
}
func (e *schemaLoadError) Unwrap() error { return e.err }

func contractCommands() *cobra.Command {
	contract := &cobra.Command{Use: "contract", Short: "Validate documents against a JSON Schema the caller supplies"}
	validate := &cobra.Command{
		Use:   "validate FILE",
		Short: "Validate FILE against --schema (a path or URL); capsulectl embeds no schema of its own",
		Args:  oneArg,
		RunE: func(c *cobra.Command, args []string) error {
			schemaLoc, _ := c.Flags().GetString("schema")
			if schemaLoc == "" {
				return inputError("--schema is required (a path or URL to a JSON Schema)")
			}
			asJSON, _ := c.Flags().GetBool("json")
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
			if asJSON {
				issues := make([]map[string]any, len(report.issues))
				for i, issue := range report.issues {
					issues[i] = map[string]any{"path": issue.Path, "message": issue.Message}
				}
				if oe := output(c, map[string]any{"file": args[0], "schema": schemaLoc, "valid": report.valid, "issues": issues}); oe != nil {
					return oe
				}
			} else {
				printReport(c, args[0], report)
			}
			if !report.valid {
				return ErrSchemaInvalid
			}
			return nil
		},
	}
	validate.Flags().String("schema", "", "Path or URL to the JSON Schema to validate against (required)")
	validate.Flags().Bool("json", false, "Emit a capsule-cli-result/v1 report instead of readable text")
	contract.AddCommand(validate)
	return contract
}

func printReport(c *cobra.Command, file string, report contractReport) {
	w := c.OutOrStdout()
	if report.valid {
		fmt.Fprintf(w, "%s: valid\n", file)
		return
	}
	fmt.Fprintf(w, "%s: INVALID\n", file)
	for _, issue := range report.issues {
		fmt.Fprintf(w, "  at %s: %s\n", issue.Path, issue.Message)
	}
}

type contractIssue struct {
	Path    string
	Message string
}

type contractReport struct {
	valid  bool
	issues []contractIssue
}

// validateContract renders one readable issue per irreducible schema
// violation. Where the schema declares a `profile`-discriminated union (a
// oneOf whose branches each pin `profile` to a distinct const), an unmatched
// oneOf is explained in terms of that single field instead of nine parallel,
// mostly-irrelevant branch failures -- an unknown profile value, or the
// specific missing block for the profile that was actually named.
func validateContract(schema *jsonschema.Schema, schemaRaw any, doc any) contractReport {
	err := schema.Validate(doc)
	if err == nil {
		return contractReport{valid: true}
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return contractReport{issues: []contractIssue{{Path: "<root>", Message: err.Error()}}}
	}
	discriminator := discriminatorValues(schemaRaw, "profile")
	var issues []contractIssue
	explainNode(ve, doc, discriminator, &issues)
	if len(issues) == 0 {
		// Defensive: Validate returned an error, so there must be something to
		// report even if the tree walk above found no leaf -- never claim valid.
		issues = append(issues, contractIssue{Path: pathString(ve.InstanceLocation), Message: ve.Error()})
	}
	return contractReport{issues: issues}
}

func explainNode(e *jsonschema.ValidationError, doc any, discriminator []string, issues *[]contractIssue) {
	if oneOf, ok := e.ErrorKind.(*kind.OneOf); ok && oneOf.Subschemas == nil && len(discriminator) > 0 {
		if issue, handled := explainProfileOneOf(e, doc, discriminator); handled {
			*issues = append(*issues, issue)
			return
		}
	}
	if len(e.Causes) == 0 {
		*issues = append(*issues, contractIssue{Path: pathString(e.InstanceLocation), Message: describeLeaf(e.ErrorKind)})
		return
	}
	for _, cause := range e.Causes {
		explainNode(cause, doc, discriminator, issues)
	}
}

// explainProfileOneOf handles a `profile`-discriminated oneOf specifically.
// It returns handled=false when the instance at this location is not an
// object (the discriminator pattern does not apply), leaving the generic
// per-branch walk to describe it instead.
func explainProfileOneOf(e *jsonschema.ValidationError, doc any, discriminator []string) (contractIssue, bool) {
	obj, ok := valueAt(doc, e.InstanceLocation).(map[string]any)
	if !ok {
		return contractIssue{}, false
	}
	path := pathString(e.InstanceLocation)
	value, present := obj["profile"]
	if !present {
		return contractIssue{Path: path, Message: fmt.Sprintf("missing required property \"profile\" (expected one of: %s)", strings.Join(discriminator, ", "))}, true
	}
	name, ok := value.(string)
	if !ok {
		return contractIssue{}, false
	}
	var candidates []*jsonschema.ValidationError
	for _, cause := range e.Causes {
		if profileMismatch(cause) {
			continue
		}
		candidates = append(candidates, cause)
	}
	if len(candidates) == 0 {
		return contractIssue{Path: path, Message: fmt.Sprintf("unknown profile %q (expected one of: %s)", name, strings.Join(discriminator, ", "))}, true
	}
	best := leaves(candidates[0])
	for _, c := range candidates[1:] {
		if l := leaves(c); len(l) < len(best) {
			best = l
		}
	}
	parts := make([]string, len(best))
	for i, leaf := range best {
		parts[i] = fmt.Sprintf("at %s: %s", pathString(leaf.InstanceLocation), describeLeaf(leaf.ErrorKind))
	}
	return contractIssue{Path: path, Message: fmt.Sprintf("profile %q: %s", name, strings.Join(parts, "; "))}, true
}

// profileMismatch reports whether e's subtree contains a failure of the
// `profile` property itself (const or enum) -- the signal that this oneOf
// branch does not apply to the instance's actual profile value, as opposed to
// a branch the profile matches but whose other required fields are absent.
func profileMismatch(e *jsonschema.ValidationError) bool {
	if loc := e.InstanceLocation; len(loc) > 0 && loc[len(loc)-1] == "profile" {
		switch e.ErrorKind.(type) {
		case *kind.Const, *kind.Enum:
			return true
		}
	}
	for _, cause := range e.Causes {
		if profileMismatch(cause) {
			return true
		}
	}
	return false
}

func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, cause := range e.Causes {
		out = append(out, leaves(cause)...)
	}
	return out
}

func describeLeaf(k jsonschema.ErrorKind) string {
	switch v := k.(type) {
	case *kind.Required:
		if len(v.Missing) == 1 {
			return fmt.Sprintf("missing required property %q", v.Missing[0])
		}
		return "missing required properties " + joinQuoted(v.Missing)
	case *kind.Const:
		return fmt.Sprintf("expected %s, got %s", jsonDisplay(v.Want), jsonDisplay(v.Got))
	case *kind.Enum:
		wants := make([]string, len(v.Want))
		for i, w := range v.Want {
			wants[i] = jsonDisplay(w)
		}
		return fmt.Sprintf("expected one of [%s], got %s", strings.Join(wants, ", "), jsonDisplay(v.Got))
	case *kind.Type:
		return fmt.Sprintf("expected type %s, got %s", strings.Join(v.Want, " or "), v.Got)
	case *kind.Pattern:
		return fmt.Sprintf("value %q does not match pattern %q", v.Got, v.Want)
	case *kind.FalseSchema:
		return "field is not permitted here"
	case *kind.AdditionalProperties:
		return "additional properties not allowed: " + joinQuoted(v.Properties)
	case *kind.MinItems:
		return fmt.Sprintf("expected at least %d item(s), got %d", v.Want, v.Got)
	case *kind.MaxItems:
		return fmt.Sprintf("expected at most %d item(s), got %d", v.Want, v.Got)
	case *kind.MinLength:
		return fmt.Sprintf("expected length at least %d, got %d", v.Want, v.Got)
	case *kind.MaxLength:
		return fmt.Sprintf("expected length at most %d, got %d", v.Want, v.Got)
	case *kind.MinProperties:
		return fmt.Sprintf("expected at least %d propertie(s), got %d", v.Want, v.Got)
	case *kind.MaxProperties:
		return fmt.Sprintf("expected at most %d propertie(s), got %d", v.Want, v.Got)
	default:
		return k.LocalizedString(printer)
	}
}

func joinQuoted(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = strconv.Quote(v)
	}
	return strings.Join(quoted, ", ")
}

func jsonDisplay(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// pathString renders an instance location as a readable JSON Pointer,
// `<root>` for the document itself -- the same convention the ledger lane's
// Python validator (`contract_validate.py`) uses, so a report reads the same
// from either implementation.
func pathString(loc []string) string {
	if len(loc) == 0 {
		return "<root>"
	}
	parts := make([]string, len(loc))
	for i, p := range loc {
		p = strings.ReplaceAll(p, "~", "~0")
		p = strings.ReplaceAll(p, "/", "~1")
		parts[i] = p
	}
	return strings.Join(parts, "/")
}

func valueAt(doc any, loc []string) any {
	cur := doc
	for _, seg := range loc {
		switch v := cur.(type) {
		case map[string]any:
			cur = v[seg]
		case []any:
			idx, e := strconv.Atoi(seg)
			if e != nil || idx < 0 || idx >= len(v) {
				return nil
			}
			cur = v[idx]
		default:
			return nil
		}
	}
	return cur
}

// discriminatorValues scans the raw schema document (not the compiled form,
// which has no API for listing a union's branch consts) for every subschema
// shaped `"properties": {"<key>": {"const": "<value>"}}`, anywhere in the
// document. This is a generic const-discriminator scan keyed only on the
// property name the caller names (here "profile") -- it does not assume or
// hardcode which values exist, so it stays correct as the schema's closed set
// changes.
func discriminatorValues(schemaRaw any, key string) []string {
	seen := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if props, ok := v["properties"].(map[string]any); ok {
				if sub, ok := props[key].(map[string]any); ok {
					if c, ok := sub["const"].(string); ok {
						seen[c] = true
					}
				}
			}
			for _, vv := range v {
				walk(vv)
			}
		case []any:
			for _, vv := range v {
				walk(vv)
			}
		}
	}
	walk(schemaRaw)
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	sort.Strings(values)
	return values
}

func schemaIsURL(loc string) bool {
	u, e := url.Parse(loc)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// loadSchema reads the schema twice by design: once as a plain generic
// document (discriminatorValues has no use for a compiled *Schema), and once
// through the compiler (which needs to resolve internal $refs). Never reads
// from a path or URL other than the one the caller supplied in --schema --
// capsulectl embeds no schema of its own.
func loadSchema(loc string) (*jsonschema.Schema, any, error) {
	raw, e := fetchSchemaBytes(loc)
	if e != nil {
		return nil, nil, e
	}
	var schemaRaw any
	if e := json.Unmarshal(raw, &schemaRaw); e != nil {
		return nil, nil, e
	}
	compiler := jsonschema.NewCompiler()
	if schemaIsURL(loc) {
		httpLoader := boundedHTTPLoader{client: &http.Client{Timeout: 15 * time.Second}}
		compiler.UseLoader(jsonschema.SchemeURLLoader{"file": jsonschema.FileLoader{}, "http": httpLoader, "https": httpLoader})
	}
	sch, e := compiler.Compile(loc)
	if e != nil {
		return nil, nil, e
	}
	return sch, schemaRaw, nil
}

func fetchSchemaBytes(loc string) ([]byte, error) {
	if !schemaIsURL(loc) {
		b, e := os.ReadFile(loc)
		if e != nil {
			return nil, e
		}
		if int64(len(b)) > maxSchemaFetch {
			return nil, fmt.Errorf("schema exceeds size limit")
		}
		return b, nil
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, e := client.Get(loc)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d", loc, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxSchemaFetch+1))
}

// boundedHTTPLoader is the compiler's own $ref loader for http(s) schema
// locations -- distinct from fetchSchemaBytes's plain read, since the
// compiler drives it once per resolved URL as $refs are followed.
type boundedHTTPLoader struct{ client *http.Client }

func (l boundedHTTPLoader) Load(u string) (any, error) {
	resp, e := l.client.Get(u)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d", u, resp.StatusCode)
	}
	return jsonschema.UnmarshalJSON(io.LimitReader(resp.Body, maxSchemaFetch+1))
}
