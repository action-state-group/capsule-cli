package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A deal profile may pin an external rules checker: a command the user
// chose, run at every deal check on the record of what is about to happen.
// It reports a neutral result (external-check-result/v0: the ruleset it
// evaluated, by id and definition digest, a verdict and its findings), and
// the check seals those values as data. capsulectl knows no ruleset of its
// own: an id appears in a check only because the pinned checker reported it.
//
// Pinning is the user's policy (profile update --rules-checker FILE): the
// command's executable must live under a trusted plugin root and is pinned
// by its SHA-256, and the ruleset's definition digest may be pinned too. A
// checker that was swapped, fails, times out or reports anything else pauses
// the check, saying why; a profile with no checker says so and does not.

const (
	externalCheckSchema = "external-check-result/v0"
	rulesTimeoutDefault = 10 * time.Second
	rulesTimeoutMax     = 60 * time.Second
	rulesMaxOutput      = 1 << 20
	// The checker is given this one record and no history, so a limit over
	// a rolling window is evaluated over this action alone: sealed and said.
	rulesWindowThisActionOnly = "this_action_only"
)

// rulesCheckerPin is the file `profile update --rules-checker` reads.
type rulesCheckerPin struct {
	Command          []string `json:"command"`
	Timeout          string   `json:"timeout,omitempty"`
	DefinitionDigest string   `json:"definition_digest,omitempty"`
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pinRulesChecker reads a pin file and pins its checker in p: the command,
// the SHA-256 of its executable, the timeout and, when given, the ruleset's
// definition digest. An empty path removes the checker.
func pinRulesChecker(p *Profile, path string) error {
	if path == "" {
		p.RulesChecker = ProfileRulesChecker{}
		return nil
	}
	raw, err := readInput(path)
	if err != nil {
		return err
	}
	var pin rulesCheckerPin
	if err = decodeJSONAs("--rules-checker "+path, raw, &pin); err != nil {
		return err
	}
	if len(pin.Command) == 0 || !filepath.IsAbs(pin.Command[0]) {
		return inputError(`--rules-checker needs {"command": ["/absolute/path/to/checker", "arg", ...]}: the first element an absolute path`)
	}
	if _, err = rulesTimeout(pin.Timeout); err != nil {
		return err
	}
	if pin.DefinitionDigest != "" && !hex64.MatchString(pin.DefinitionDigest) {
		return inputError("--rules-checker definition_digest must be 64 lowercase hex characters")
	}
	sum, err := trustedExecutableDigest(pin.Command[0])
	if err != nil {
		return err
	}
	p.RulesChecker = ProfileRulesChecker{Command: pin.Command, SHA256: sum, Timeout: pin.Timeout, DefinitionDigest: pin.DefinitionDigest}
	return nil
}

func rulesTimeout(value string) (time.Duration, error) {
	if value == "" {
		return rulesTimeoutDefault, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 || d > rulesTimeoutMax {
		return 0, inputError("the rules checker's timeout must be a duration above 0 and at most 60s, such as 10s")
	}
	return d, nil
}

// trustedExecutableDigest is the SHA-256 of an executable under a trusted
// plugin root, refused anywhere else (verifyTrustedPath).
func trustedExecutableDigest(path string) (string, error) {
	if err := verifyTrustedPath(path); err != nil {
		return "", inputError("the rules checker must live under a trusted plugin root (" + strings.Join(trustedPluginRoots(), ", ") + "): " + err.Error())
	}
	f, err := os.Open(path)
	if err != nil {
		return "", inputError("the rules checker cannot be read: " + err.Error())
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// dealRulesFinding is one rule's result, as the checker reported it.
type dealRulesFinding struct {
	ID      string          `json:"id"`
	Check   string          `json:"check,omitempty"`
	Verdict string          `json:"verdict"`
	Reason  string          `json:"reason,omitempty"`
	Limit   json.RawMessage `json:"limit,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
}

// dealRules is what a check says about the profile's rules, sealed in its
// verdict: evaluated (with the ruleset, the verdict and every finding), not
// evaluated (a configured checker that could not answer, and why), or not
// configured.
type dealRules struct {
	Status string `json:"status"`
	// Cause is why a configured checker's answer was not evaluated, as a
	// token (the sealed form); Reason says it in words, for the prompt.
	Cause            string             `json:"cause,omitempty"`
	Reason           string             `json:"reason,omitempty"`
	RulesetID        string             `json:"ruleset_id,omitempty"`
	DefinitionDigest string             `json:"definition_digest,omitempty"`
	CheckerSHA256    string             `json:"checker_sha256,omitempty"`
	Verdict          string             `json:"verdict,omitempty"`
	Findings         []dealRulesFinding `json:"findings,omitempty"`
	Window           string             `json:"window,omitempty"`
}

// externalCheckResult is the neutral shape a checker prints.
type externalCheckResult struct {
	Schema           string             `json:"schema"`
	RulesetID        string             `json:"ruleset_id"`
	DefinitionDigest string             `json:"definition_digest"`
	Verdict          string             `json:"verdict"`
	Findings         []dealRulesFinding `json:"findings"`
}

//go:embed assets/external-check-result-v0.schema.json
var externalCheckSchemaJSON []byte

var (
	externalCheckOnce     sync.Once
	externalCheckCompiled *jsonschema.Schema
	externalCheckErr      error
)

func compiledExternalCheckSchema() (*jsonschema.Schema, error) {
	externalCheckOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(externalCheckSchemaJSON))
		if err != nil {
			externalCheckErr = err
			return
		}
		c := jsonschema.NewCompiler()
		const id = "external-check-result-v0.schema.json"
		if err = c.AddResource(id, doc); err != nil {
			externalCheckErr = err
			return
		}
		externalCheckCompiled, externalCheckErr = c.Compile(id)
	})
	return externalCheckCompiled, externalCheckErr
}

// parseExternalCheck reads a checker's output: exactly one JSON value, valid
// against the shipped external-check-result/v0 schema, or a reason it is not.
func parseExternalCheck(out []byte) (externalCheckResult, error) {
	var r externalCheckResult
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.UseNumber()
	var doc interface{}
	if err := decoder.Decode(&doc); err != nil {
		return r, errors.New("its output is not one " + externalCheckSchema + " JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return r, errors.New("its output holds more than one JSON value")
	}
	if m, ok := doc.(map[string]interface{}); !ok || m["schema"] != externalCheckSchema {
		return r, errors.New("its output is not " + externalCheckSchema)
	}
	schema, err := compiledExternalCheckSchema()
	if err != nil {
		return r, err
	}
	if err = schema.Validate(doc); err != nil {
		return r, errors.New("its output does not fit " + externalCheckSchema + ": " + firstSchemaError(err))
	}
	if err = json.Unmarshal(out, &r); err != nil {
		return r, errors.New("its output is not one " + externalCheckSchema + " JSON object")
	}
	return r, nil
}

// runRulesChecker runs the profile's pinned checker on one record (the
// capsule with its disclosed input) and says what came of it. It never
// errors: a checker that could not answer is a result too.
func runRulesChecker(ctx context.Context, p Profile, record []byte) dealRules {
	pin := p.RulesChecker
	if len(pin.Command) == 0 {
		return dealRules{Status: "not_configured"}
	}
	notEvaluated := func(cause, why string) dealRules {
		return dealRules{Status: "not_evaluated", Cause: cause, Reason: why, CheckerSHA256: pin.SHA256}
	}
	sum, err := trustedExecutableDigest(pin.Command[0])
	switch {
	case err != nil:
		return notEvaluated("checker_unavailable", "the pinned rules checker cannot be run: "+SafeError(err))
	case sum != pin.SHA256:
		return notEvaluated("checker_changed", "the rules checker changed since it was pinned; re-pin it with profile update --rules-checker FILE")
	}
	timeout, err := rulesTimeout(pin.Timeout)
	if err != nil {
		return notEvaluated("checker_unavailable", SafeError(err))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, pin.Command[0], pin.Command[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	cmd.Stdin = bytes.NewReader(record)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitedWriter{w: &stdout, n: rulesMaxOutput}, &limitedWriter{w: &stderr, n: 4096}
	killGroupOnCancel(cmd)
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return notEvaluated("timeout", "the rules checker did not answer within "+timeout.String())
	case err != nil:
		why := strings.TrimSpace(stderr.String())
		if why == "" {
			why = err.Error()
		}
		return notEvaluated("refused", "the rules checker refused the record: "+firstLine(why))
	}
	result, err := parseExternalCheck(stdout.Bytes())
	if err != nil {
		return notEvaluated("unreadable", "the rules checker's answer could not be read: "+err.Error())
	}
	if pin.DefinitionDigest != "" && result.DefinitionDigest != pin.DefinitionDigest {
		return notEvaluated("ruleset_changed", "the ruleset changed since it was pinned ("+result.RulesetID+" reports another definition digest); re-pin it with profile update --rules-checker FILE")
	}
	return dealRules{Status: "evaluated", RulesetID: result.RulesetID, DefinitionDigest: result.DefinitionDigest, CheckerSHA256: pin.SHA256,
		Verdict: result.Verdict, Findings: result.Findings, Window: rulesWindowThisActionOnly}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// limitedWriter keeps at most n bytes.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		keep := p
		if len(keep) > l.n {
			keep = keep[:l.n]
		}
		l.n -= len(keep)
		if _, err := l.w.Write(keep); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// rulesStatusLine is the rules line every check prompt leads with.
func rulesStatusLine(r *dealRules) string {
	if r == nil {
		return ""
	}
	switch r.Status {
	case "evaluated":
		short := r.DefinitionDigest
		if len(short) > 8 {
			short = short[:8]
		}
		words := map[string]string{"allow": "allowed", "deny": "not allowed", "escalate": "needs your approval", "not_evaluable": "not fully checked"}[r.Verdict]
		return fmt.Sprintf("Your rules (%s, digest %s): %s. Any weekly limit was checked against this action alone.", r.RulesetID, short, words)
	case "not_configured":
		return "Your rules were not checked: no rules checker configured."
	default:
		return "Your rules were not checked: " + r.Reason + "."
	}
}

// rulesDifferences are the differences the rules add to a check: a deny, an
// escalation, a rule left unevaluated, or a checker that could not answer.
// An allow adds none; a profile with no checker adds none.
func rulesDifferences(r *dealRules) []dealDifference {
	if r == nil {
		return nil
	}
	if r.Status == "not_evaluated" {
		return []dealDifference{{Question: "safety", Rule: "rules_not_checked", Text: "Your rules were not checked: " + r.Reason}}
	}
	if r.Status != "evaluated" || r.Verdict == "allow" {
		return nil
	}
	var out []dealDifference
	for _, f := range r.Findings {
		if f.Verdict != "fail" && f.Verdict != "not_evaluable" {
			continue
		}
		rule := "rules_" + r.Verdict
		text := fmt.Sprintf("%s: %s", f.ID, f.Reason)
		if len(f.Limit) > 0 || len(f.Value) > 0 {
			text += fmt.Sprintf(" (limit %s; value %s)", compactJSON(f.Limit), compactJSON(f.Value))
		}
		switch r.Verdict {
		case "deny":
			text = "Not allowed by your rules: " + text
		case "escalate":
			text = "Your rules ask for approval: " + text
		default:
			rule, text = "rules_not_evaluable", "Your rules were not fully checked: "+text
		}
		out = append(out, dealDifference{Question: "safety", Rule: rule, Field: findingField(f.ID), Text: text})
	}
	if len(out) == 0 {
		// A verdict with no failing finding named: still not an allow.
		rule := map[string]string{"deny": "rules_deny", "escalate": "rules_escalate"}[r.Verdict]
		if rule == "" {
			rule = "rules_not_evaluable"
		}
		out = append(out, dealDifference{Question: "safety", Rule: rule, Text: "Your rules (" + r.RulesetID + "): " + r.Verdict})
	}
	return out
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "not stated"
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return "not stated"
	}
	return b.String()
}

var findingFieldChars = regexp.MustCompile(`[^a-z0-9_]+`)

// findingField is a finding's id as a difference field (lowercase, a-z0-9_).
func findingField(id string) string {
	f := strings.Trim(findingFieldChars.ReplaceAllString(strings.ToLower(id), "_"), "_")
	if f == "" || f[0] < 'a' || f[0] > 'z' {
		f = "rule_" + f
	}
	return f
}

// rulesInput is the one record a rules checker reads: the sealed capsule of
// the step about to be checked, with its disclosed agent_input (the deal
// record, whose body is what is about to happen), as a bundle carries it.
func (s *dealSession) rulesInput(ctx context.Context, capsuleID string) ([]byte, error) {
	capsule, err := getCapsule(ctx, s.t.artifacts, capsuleID)
	if err != nil {
		return nil, err
	}
	overlay, err := disclosureOverlay(ctx, s.t.artifacts, []string{capsuleID}, nil, nil)
	if err != nil {
		return nil, err
	}
	members, _ := overlay[capsuleID].(map[string]interface{})
	input, ok := members["agent_input"]
	if !ok {
		return nil, errors.New("the checked step's record is not retained")
	}
	capsule["agent_input"] = input
	return json.Marshal(capsule)
}
