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
	"sort"
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
	rulesMaxExecutable  = 256 << 20
	externalCheckInput  = "external-check-input/v0"
	// The history a checker is given: the profile's sealed acts with an
	// amount, from every deal, sealed in the last rulesHistoryDays days, the
	// most recent rulesHistoryMax of them.
	rulesHistoryDays = 31
)

// rulesHistoryMax is the most acts a checker's history holds (a variable
// for tests).
var rulesHistoryMax = 1000

// rulesBeforeExec runs between hashing the checker and running it (tests).
var rulesBeforeExec = func() {}

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
	// Tier is how the checker said it reached the verdict (recomputed,
	// judged or human), as reported; empty when it did not say, which reads
	// as judged. It is recorded and shown, and changes nothing.
	Tier string `json:"tier,omitempty"`
	// Grade is the grade of the evidence behind the result (self-attested,
	// witnessed or countersigned), as reported; empty when none was stated.
	// Recorded and shown; it changes nothing.
	Grade string `json:"grade,omitempty"`
	// History is what the checker was given beside the record.
	History *dealRulesHistory `json:"history,omitempty"`
}

// dealRulesHistory states the history a checker was given: how many earlier
// acts, over how many days, and whether that is all of them (false when
// more than the cap were sealed, or some could not be read).
type dealRulesHistory struct {
	Days     int  `json:"days"`
	Acts     int  `json:"acts"`
	Complete bool `json:"complete"`
}

// externalCheckResult is the neutral shape a checker prints.
type externalCheckResult struct {
	Schema           string             `json:"schema"`
	RulesetID        string             `json:"ruleset_id"`
	DefinitionDigest string             `json:"definition_digest"`
	Verdict          string             `json:"verdict"`
	Tier             string             `json:"tier"`
	Grade            string             `json:"grade"`
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

// runRulesChecker runs the profile's pinned checker on its input (the
// record of what is about to happen, with the history) and says what came of
// it. It never errors: a checker that could not answer is a result too.
//
// It runs exactly the bytes it hashed: the executable is read once, hashed,
// and run from a private copy, so a file swapped after the check is not what
// runs.
func runRulesChecker(ctx context.Context, p Profile, input []byte, history *dealRulesHistory) dealRules {
	pin := p.RulesChecker
	if len(pin.Command) == 0 {
		return dealRules{Status: "not_configured"}
	}
	notEvaluated := func(cause, why string) dealRules {
		return dealRules{Status: "not_evaluated", Cause: cause, Reason: why, CheckerSHA256: pin.SHA256}
	}
	copied, sum, cleanup, err := privateExecutableCopy(pin.Command[0])
	defer cleanup()
	switch {
	case err != nil:
		return notEvaluated("checker_unavailable", "the pinned rules checker cannot be run: "+SafeError(err))
	case sum != pin.SHA256:
		return notEvaluated("checker_changed", "the rules checker changed since it was pinned; re-pin it with profile update --rules-checker FILE")
	}
	rulesBeforeExec()
	timeout, err := rulesTimeout(pin.Timeout)
	if err != nil {
		return notEvaluated("checker_unavailable", SafeError(err))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, copied, pin.Command[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	cmd.Stdin = bytes.NewReader(input)
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
		Verdict: result.Verdict, Tier: result.Tier, Grade: result.Grade, Findings: result.Findings, History: history}
}

// privateExecutableCopy reads the executable at path once (under a trusted
// plugin root), hashes those bytes and writes them to a new file in a private
// directory: the copy runs, so what runs is what was hashed. cleanup removes
// it.
func privateExecutableCopy(path string) (copied, sum string, cleanup func(), err error) {
	cleanup = func() {}
	if err = verifyTrustedPath(path); err != nil {
		return "", "", cleanup, err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", cleanup, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, rulesMaxExecutable+1))
	f.Close()
	if err != nil {
		return "", "", cleanup, err
	}
	if len(raw) > rulesMaxExecutable {
		return "", "", cleanup, errors.New("the rules checker is larger than 256 MiB")
	}
	digest := sha256.Sum256(raw)
	sum = hex.EncodeToString(digest[:])
	base := ""
	if cache, e := os.UserCacheDir(); e == nil {
		// Under the user's cache, not the system temp directory, which may
		// not allow running files.
		base = filepath.Join(cache, "capsulectl")
		if e = os.MkdirAll(base, 0o700); e != nil {
			base = ""
		}
	}
	dir, err := os.MkdirTemp(base, "rules-checker-")
	if err != nil {
		return "", "", cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	copied = filepath.Join(dir, filepath.Base(path))
	out, err := os.OpenFile(copied, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return "", "", cleanup, err
	}
	if _, err = out.Write(raw); err != nil {
		out.Close()
		return "", "", cleanup, err
	}
	if err = out.Close(); err != nil {
		return "", "", cleanup, err
	}
	return copied, sum, cleanup, nil
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

// sealableRules is r, or, when what the checker said cannot be sealed (it
// would carry a phone number, an email address, a counterparty's details or
// a word a record may not carry), the rules not evaluated: the check pauses
// and says why, and is sealed. A checker's answer never fails the check.
func sealableRules(r dealRules, localValues []string) dealRules {
	if r.Status != "evaluated" {
		return r
	}
	body, err := rulesBody(r)
	if err == nil {
		err = scanRecord(map[string]interface{}{"body": map[string]interface{}{"rules": body}}, localValues)
	}
	if err == nil {
		return r
	}
	why := SafeError(err)
	if i := strings.Index(why, "refusing to seal: "); i >= 0 {
		why = why[i+len("refusing to seal: "):]
	}
	return dealRules{Status: "not_evaluated", Cause: "unreadable", CheckerSHA256: r.CheckerSHA256,
		Reason: "the rules checker's answer could not be sealed (" + why + ")"}
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
		line := fmt.Sprintf("Your rules (%s, digest %s): %s, %s.", r.RulesetID, short, words, rulesProvenance(r.Tier, r.Grade))
		if h := r.History; h != nil && !h.Complete {
			line += fmt.Sprintf(" They were given only %d of your earlier payments from the last %d days.", h.Acts, h.Days)
		}
		return line
	case "not_configured":
		return "Your rules were not checked: no rules checker configured."
	default:
		return "Your rules were not checked: " + r.Reason + "."
	}
}

// rulesProvenance says how the checker reached its verdict, and the grade of
// the evidence behind it, as it said.
func rulesProvenance(tier, grade string) string {
	words := "judged (not stated)"
	switch tier {
	case "recomputed":
		words = "computed by your rules"
	case "judged":
		words = "judged"
	}
	if grade != "" {
		words += ", " + grade
	}
	return words
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
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
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

// rulesInput is what a rules checker reads on stdin, one
// external-check-input/v0 object: the record (the sealed capsule of the step
// about to be checked, with its disclosed agent_input, whose body is what is
// about to happen) and the history (the profile's earlier sealed acts with an
// amount, from every deal, in the same shape), as data. capsulectl names no
// rule: a checker with a limit over a rolling window evaluates it over the
// history, and reports it not_evaluable when that is not enough.
func (s *dealSession) rulesInput(ctx context.Context, capsuleID string) ([]byte, *dealRulesHistory, error) {
	record, err := s.capsuleWithInput(ctx, capsuleID)
	if err != nil {
		return nil, nil, err
	}
	if record == nil {
		return nil, nil, errors.New("the checked step's record is not retained")
	}
	history, scope, err := s.rulesHistory(ctx)
	if err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(map[string]interface{}{"schema": externalCheckInput, "record": record, "history": history,
		"history_scope": map[string]interface{}{"days": scope.Days, "max_records": rulesHistoryMax, "complete": scope.Complete}})
	return raw, scope, err
}

// capsuleWithInput is a sealed capsule with its disclosed agent_input; nil
// when the input is not retained.
func (s *dealSession) capsuleWithInput(ctx context.Context, capsuleID string) (map[string]interface{}, error) {
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
		return nil, nil
	}
	capsule["agent_input"] = input
	return capsule, nil
}

// rulesHistory is the profile's sealed acts with an amount, from every deal
// on this profile's own store, sealed in the last rulesHistoryDays days,
// newest first and at most rulesHistoryMax of them, with what it covers.
func (s *dealSession) rulesHistory(ctx context.Context) ([]interface{}, *dealRulesHistory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT capsule_id, local FROM deal_steps WHERE kind='act'`)
	if err != nil {
		return nil, nil, err
	}
	type act struct {
		id string
		at time.Time
	}
	var acts []act
	since := dealClock().Add(-rulesHistoryDays * 24 * time.Hour)
	for rows.Next() {
		var id, local string
		if err = rows.Scan(&id, &local); err != nil {
			return nil, nil, errors.Join(err, rows.Close())
		}
		var ev dealEvent
		if json.Unmarshal([]byte(local), &ev) != nil || ev.Act == nil || ev.Act.AmountMinor == nil {
			continue
		}
		at, e := time.Parse(time.RFC3339, ev.At)
		if e != nil || at.Before(since) {
			continue
		}
		acts = append(acts, act{id, at})
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	sort.SliceStable(acts, func(i, j int) bool { return acts[i].at.After(acts[j].at) })
	scope := &dealRulesHistory{Days: rulesHistoryDays, Complete: true}
	if len(acts) > rulesHistoryMax {
		acts, scope.Complete = acts[:rulesHistoryMax], false
	}
	history := []interface{}{}
	for _, a := range acts {
		record, err := s.capsuleWithInput(ctx, a.id)
		if err != nil || record == nil {
			scope.Complete = false
			continue
		}
		history = append(history, record)
	}
	scope.Acts = len(history)
	return history, scope, nil
}
