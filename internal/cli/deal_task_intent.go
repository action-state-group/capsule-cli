package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/spf13/cobra"
)

// A task intent is what the user asked for when a buying or booking task
// started, before any search and before there is a merchant: their request
// exactly as given, and the constraints they stated (what, for what use, a
// budget). It is its own log, one record (task_intent), like a sale's root:
// the deal that follows names no merchant yet when it is sealed. The deal's
// baseline cites it (a `cites` ref to its record digest), which proves it
// existed before the deal opened: a digest cannot be cited before its bytes
// exist. Nothing is in the clear: the request is a salted commitment
// (verbatim_commitment), and each constraint is a field whose class is in the
// clear and whose value is committed under its own nonce, so a copy can open
// one without the others. The user's own copy of a deal carries the
// openings; a shared copy carries none.
//
// A task intent is not an acceptance by anyone and not an acknowledgment:
// it records what the user asked for, nothing more. A deal opened without
// one stays valid; its task bounds are then not available, never passed.

var taskIDPattern = regexp.MustCompile(`^task-[0-9a-f]{16}$`)

// dealTaskIntent is the user's request at the start of a task, as the agent
// reports it: their words exactly as given (never trimmed or normalized),
// and the constraints they stated, each optional.
type dealTaskIntent struct {
	Verbatim  string          `json:"verbatim"`
	What      string          `json:"what,omitempty"`
	Use       string          `json:"use,omitempty"`
	BudgetMax *dealTaskBudget `json:"budget_max,omitempty"`
}

// dealTaskBudget is a budget limit, in the form of x-deal-v0's amounts: minor
// units and an ISO 4217 currency.
type dealTaskBudget struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// taskIntentClasses are the constraint classes, in the order a record lists
// them. Classes are added only as the task-bounds rule needs them.
var taskIntentClasses = []string{"what", "use", "budget_max"}

// taskFieldNonce is the nonce name of a constraint's value commitment.
func taskFieldNonce(class string) string { return "field_" + class }

// values are the committed texts of the stated constraints, by class: the
// words as given, and a budget as the JCS bytes of {amount_minor, currency}.
func (t dealTaskIntent) values() map[string]string {
	out := map[string]string{}
	if t.What != "" {
		out["what"] = t.What
	}
	if t.Use != "" {
		out["use"] = t.Use
	}
	if b := t.BudgetMax; b != nil {
		if text := jcsText(map[string]interface{}{"amount_minor": b.AmountMinor, "currency": b.Currency}); text != "" {
			out["budget_max"] = text
		}
	}
	return out
}

func (t *dealTaskIntent) validate() error {
	if strings.TrimSpace(t.Verbatim) == "" {
		return inputError("verbatim (the user's request, exactly as they gave it) is required")
	}
	if b := t.BudgetMax; b != nil {
		b.Currency = strings.ToUpper(strings.TrimSpace(b.Currency))
		if b.AmountMinor < 0 || b.AmountMinor > maxSafeInteger || !currencyCode.MatchString(b.Currency) || t.values()["budget_max"] == "" {
			return inputError("budget_max is {\"amount_minor\": the limit in minor units, \"currency\": a three-letter code}")
		}
	}
	return nil
}

const maxSafeInteger = 9007199254740991

// taskIntentBody is the task_intent record's body: the request's commitment
// and one field per stated constraint.
func taskIntentBody(t dealTaskIntent, commit func(string) (string, error)) (map[string]interface{}, error) {
	c, err := commit("verbatim")
	if err != nil {
		return nil, err
	}
	body := map[string]interface{}{"verbatim_commitment": c}
	values := t.values()
	fields := []interface{}{}
	for _, class := range taskIntentClasses {
		if _, ok := values[class]; !ok {
			continue
		}
		vc, err := commit(taskFieldNonce(class))
		if err != nil {
			return nil, err
		}
		fields = append(fields, map[string]interface{}{"class": class, "value_commitment": vc})
	}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	return body, nil
}

func dealTaskCommands() *cobra.Command {
	task := &cobra.Command{Use: "task", Short: "A buying or booking task: what the user asked for, sealed when it starts"}
	task.AddCommand(dealTaskNewCommand())
	return task
}

func dealTaskNewCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "new", Short: "Seal what the user asked for when a task starts, before any search: their words and stated limits", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := readInput(path)
		if err != nil {
			return err
		}
		var t dealTaskIntent
		if err = decodeJSONAs("--input", raw, &t); err != nil {
			return err
		}
		if err = t.validate(); err != nil {
			return err
		}
		return runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			id := make([]byte, 8)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			taskID := "task-" + hex.EncodeToString(id)
			if err := s.useDeal(ctx, taskID, true); err != nil {
				return err
			}
			se, err := s.seal(ctx, taskID, nil, dealEvent{Kind: "task_intent", TaskIntent: &t})
			if err != nil {
				return err
			}
			cp, err := s.milestone(ctx)
			if err != nil {
				return err
			}
			return output(c, map[string]any{"task_id": taskID, "capsule_id": se.CapsuleID, "record_digest": se.Digest, "checkpoint": cp,
				"next": "when the user is ready to buy or book, open the deal with deal open --task " + taskID + " (it cites this record)"})
		})
	}}
	cmd.Flags().String("input", "", "Task JSON: verbatim (the user's request, exactly as given), and what, use, budget_max {amount_minor, currency} when they stated them")
	return cmd
}

// taskIntentOf is the task intent a task id names, read and verified from
// its own log on this device: its sealed step. It leaves the session off
// any log.
func (s *dealSession) taskIntentOf(ctx context.Context, taskID string) (sealedEvent, error) {
	if !taskIDPattern.MatchString(taskID) {
		return sealedEvent{}, inputError("--task must be a task id as printed by `deal task new`: task- followed by 16 hex characters")
	}
	var held int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_keys WHERE deal_id=?`, taskID).Scan(&held); err != nil {
		return sealedEvent{}, err
	}
	if held == 0 {
		return sealedEvent{}, inputError("unknown task: " + taskID)
	}
	if err := s.useDeal(ctx, taskID, false); err != nil {
		return sealedEvent{}, err
	}
	events, err := s.load(ctx, taskID)
	err = errors.Join(err, s.t.close())
	s.t = nil
	if err != nil {
		return sealedEvent{}, err
	}
	if len(events) != 1 || events[0].Event.Kind != "task_intent" || events[0].Event.TaskIntent == nil {
		return sealedEvent{}, inputError("the task " + taskID + " holds no task intent")
	}
	return events[0], nil
}

// citedTaskIntent is the task intent a deal's baseline cites, found on this
// device by its record digest: its task id and local step. ok is false when
// the deal cites none; found is false when it cites one this device does not
// hold (an unresolved citation).
func (s *dealSession) citedTaskIntent(ctx context.Context, events []sealedEvent) (taskID string, ev dealEvent, ok, found bool, err error) {
	if len(events) == 0 || events[0].Event.Open == nil || events[0].Event.Open.TaskIntent == "" {
		return "", dealEvent{}, false, false, nil
	}
	var local string
	err = s.db.QueryRowContext(ctx, `SELECT deal_id, local FROM deal_steps WHERE record_digest=? AND kind='task_intent' AND n=1`, events[0].Event.Open.TaskIntent).Scan(&taskID, &local)
	if errors.Is(err, sql.ErrNoRows) {
		return "", dealEvent{}, true, false, nil
	}
	if err != nil {
		return "", dealEvent{}, true, false, err
	}
	if err = json.Unmarshal([]byte(local), &ev); err != nil || ev.TaskIntent == nil {
		return "", dealEvent{}, true, false, errors.Join(ErrConflict, err)
	}
	return taskID, ev, true, true, nil
}

// taskIntentSection is the user's own copy's task_intent: the cited record as
// sealed (re-derived from its local step and checked against the cited
// digest), the opening of the request, and each constraint's opening with a
// line saying it. A deal that cites none has no section; one whose citation
// this device cannot resolve says so, and opens nothing.
func (s *dealSession) taskIntentSection(ctx context.Context, events []sealedEvent) (map[string]interface{}, error) {
	taskID, ev, ok, found, err := s.citedTaskIntent(ctx, events)
	if err != nil || !ok {
		return nil, err
	}
	cited := events[0].Event.Open.TaskIntent
	if !found {
		return map[string]interface{}{"record_digest": cited, "resolved": false}, nil
	}
	payload, digest, err := encodeDealRecord(ev, nil, nil)
	if err != nil {
		return nil, err
	}
	if digest != cited {
		return nil, errors.Join(ErrConflict, errors.New("the task intent this deal cites does not re-derive to the cited digest"))
	}
	record, err := bundleJSON(json.RawMessage(payload))
	if err != nil {
		return nil, err
	}
	t := ev.TaskIntent
	values := t.values()
	fields := []interface{}{}
	for _, class := range taskIntentClasses {
		v, present := values[class]
		if !present {
			continue
		}
		fields = append(fields, map[string]interface{}{"class": class, "nonce": ev.Nonces[taskFieldNonce(class)], "value": v, "line": taskConstraintLine(class, *t)})
	}
	return map[string]interface{}{
		"task_id": taskID, "record_digest": digest, "record": record, "at": ev.At, "resolved": true,
		"verbatim_opening": map[string]interface{}{"nonce": ev.Nonces["verbatim"], "text": t.Verbatim},
		"fields":           fields,
	}, nil
}

// taskConstraintLine is one stated constraint in words.
func taskConstraintLine(class string, t dealTaskIntent) string {
	switch class {
	case "what":
		return "What: " + t.What
	case "use":
		return "For: " + t.Use
	case "budget_max":
		return "Budget: at most " + formatMoney(t.BudgetMax.AmountMinor, t.BudgetMax.Currency)
	}
	return ""
}

// taskBound is what a reader of a deal's task bounds finds for one stated
// constraint: available (opened and recomputed), not_checked (no opening),
// or failed (an opening that does not recompute).
type taskBound struct {
	Class  string          `json:"class"`
	State  string          `json:"state"`
	Value  string          `json:"value,omitempty"`
	Budget *dealTaskBudget `json:"budget,omitempty"`
}

// taskBounds reads a deal's task bounds as the task-bounds rule will: from
// the baseline's cited task intent, resolved among the records given (by
// record digest), its digest recomputed, each constraint opened with the
// holder's openings. state is "not_available" when the baseline cites none,
// "unresolved" when the cited record is not among those given, "failed"
// when the request's opening does not recompute, and "read" otherwise; then
// each bound says what was found. A missing opening is never satisfied.
func taskBounds(baseline map[string]interface{}, records []map[string]interface{}, openings map[string]interface{}) (string, []taskBound) {
	cited := citedDigest(baseline)
	if cited == "" {
		return "not_available", nil
	}
	var record map[string]interface{}
	for _, r := range records {
		if jcsDigest(r) == cited {
			record = r
		}
	}
	rb, _ := record["x-deal-v0"].(map[string]interface{})
	if record == nil || rb["record_type"] != "task_intent" {
		return "unresolved", nil
	}
	body, _ := record["body"].(map[string]interface{})
	if v, _ := openings["verbatim_opening"].(map[string]interface{}); v != nil {
		if !openingRecomputes(v, body["verbatim_commitment"]) {
			return "failed", nil
		}
	}
	given := map[string]map[string]interface{}{}
	list, _ := openings["fields"].([]interface{})
	for _, f := range list {
		if m, _ := f.(map[string]interface{}); m != nil {
			class, _ := m["class"].(string)
			given[class] = m
		}
	}
	fields, _ := body["fields"].([]interface{})
	var out []taskBound
	for _, f := range fields {
		field, _ := f.(map[string]interface{})
		class, _ := field["class"].(string)
		b := taskBound{Class: class, State: "not_checked"}
		if o := given[class]; o != nil {
			opening := map[string]interface{}{"nonce": o["nonce"], "text": o["value"]}
			switch {
			case !openingRecomputes(opening, field["value_commitment"]):
				b.State = "failed"
			default:
				b.State, b.Value = "available", o["value"].(string)
				if class == "budget_max" {
					var budget dealTaskBudget
					if decodeJSON([]byte(b.Value), &budget) == nil {
						b.Budget = &budget
					}
				}
			}
		}
		out = append(out, b)
	}
	return "read", out
}

// citedDigest is the record digest a baseline record's cites ref names, or
// "" when it cites none.
func citedDigest(baseline map[string]interface{}) string {
	block, _ := baseline["x-deal-v0"].(map[string]interface{})
	refs, _ := block["refs"].([]interface{})
	for _, r := range refs {
		if ref, _ := r.(map[string]interface{}); ref["rel"] == "cites" {
			digest, _ := ref["digest"].(string)
			return digest
		}
	}
	return ""
}

// jcsText is v's JCS bytes, through the generic JSON shape the canonicalizer
// takes (as commercialBoundsText does); "" when v cannot be encoded.
func jcsText(v interface{}) string {
	doc, err := bundleJSON(v)
	if err != nil {
		return ""
	}
	raw, err := canonical.JCS(doc)
	if err != nil {
		return ""
	}
	return string(raw)
}

// openingRecomputes is whether an opening {nonce, text} recomputes to commitment.
func openingRecomputes(opening map[string]interface{}, commitment interface{}) bool {
	nonce, ok1 := opening["nonce"].(string)
	text, ok2 := opening["text"].(string)
	want, ok3 := commitment.(string)
	if !ok1 || !ok2 || !ok3 {
		return false
	}
	got, err := commitText(nonce, text)
	return err == nil && got == want
}

// pickOpenings are the openings of each pick (an intent note) in the user's
// own copy: the user's words for it, and, when sealed, the options shown
// (the JCS bytes of their list) and the one the agent recommended.
func pickOpenings(events []sealedEvent) []interface{} {
	out := []interface{}{}
	for _, se := range events {
		i := se.Event.Intent
		if se.Event.Kind != "intent" || i == nil {
			continue
		}
		texts := map[string]string{}
		pickTexts(texts, *i)
		p := map[string]interface{}{"step": se.CapsuleID, "verbatim": map[string]interface{}{"nonce": se.Event.Nonces["verbatim"], "text": i.Verbatim}}
		for _, name := range []string{"shown", "recommended"} {
			if text, ok := texts[name]; ok {
				p[name] = map[string]interface{}{"nonce": se.Event.Nonces[name], "text": text}
			}
		}
		out = append(out, p)
	}
	return out
}

// The email receipt's notes when it shows no original request.
const (
	dealNoTaskIntentNote         = "No record of your original request was sealed before this deal opened. These are your words when it opened."
	dealUnresolvedTaskIntentNote = "This deal cites a record of your original request that this device does not hold. These are your words when it opened."
)

// emailAsked is what the email receipt shows as asked: the original request
// the deal cites, then the user's words when it opened as their pick; or,
// with none to show, their words when it opened and a note saying so.
func (s *dealSession) emailAsked(ctx context.Context, events []sealedEvent, opened string) (asked, picked, note string, err error) {
	_, ev, ok, found, err := s.citedTaskIntent(ctx, events)
	switch {
	case err != nil:
		return "", "", "", err
	case ok && found:
		return ev.TaskIntent.Verbatim, opened, "", nil
	case ok:
		return opened, "", dealUnresolvedTaskIntentNote, nil
	}
	return opened, "", dealNoTaskIntentNote, nil
}
