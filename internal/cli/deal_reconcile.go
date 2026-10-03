package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `deal reconcile` is the detective control behind the deal skill. Calling
// `deal check` is advisory: an agent host that has no hook before a tool call
// cannot make the agent call it. So this pass reads the host's own record of
// what was executed, at the layer where actions happen (the tool calls of the
// agent and of every sub-task it started, not the conversation), and lists
// each consequential action that has no deal record. It seals nothing and
// changes no deal.
//
// What it reads is a plain file in a documented format (skills/deal/
// RECONCILE.md), produced by a reader for the agent host. The pass holds no
// host-specific code: a host's tool-call and execution tables are turned into
// that format outside this repository, and the tests use fixture files.

// dealExecutionRecord is one executed tool call, as the agent host recorded it:
// metadata and digests only. Action is the reader's mechanical mapping of the
// call to a point of no return, or "other". There is no field for a form
// body, a card or payment detail, or a code; unknown fields are refused, and
// the identifier fields must look like identifiers (see guardMetadata), so
// such content cannot ride in on them either.
type dealExecutionRecord struct {
	ID              string `json:"id"`
	At              string `json:"at"`
	Task            string `json:"task,omitempty"`
	ParentTask      string `json:"parent_task,omitempty"`
	Tool            string `json:"tool"`
	Action          string `json:"action"`
	AmountMinor     *int64 `json:"amount_minor,omitempty"`
	Currency        string `json:"currency,omitempty"`
	MerchantDomain  string `json:"merchant_domain,omitempty"`
	ReferenceSHA256 string `json:"reference_sha256,omitempty"`
	Status          string `json:"status"`
	DealID          string `json:"deal_id,omitempty"`

	at time.Time
}

// dealExecutionReader yields the execution records of one period. The JSONL
// reader below is the only one in this repository; a host's own store is read
// by converting it to that format.
type dealExecutionReader interface {
	Read(ctx context.Context) ([]dealExecutionRecord, error)
}

// jsonlExecutionReader reads deal-execution-record/v0 lines: one JSON object
// per line, blank lines ignored, unknown fields refused.
type jsonlExecutionReader struct{ path string }

func (r jsonlExecutionReader) Read(_ context.Context) ([]dealExecutionRecord, error) {
	raw, err := readInput(r.path)
	if err != nil {
		return nil, err
	}
	var out []dealExecutionRecord
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), maxInput)
	for line := 1; sc.Scan(); line++ {
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		var rec dealExecutionRecord
		if err := decodeJSON(text, &rec); err != nil {
			return nil, inputError(fmt.Sprintf("execution record on line %d: invalid JSON or unknown field", line))
		}
		if err := rec.validate(); err != nil {
			return nil, inputError(fmt.Sprintf("execution record on line %d: %s", line, err))
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, inputError("execution records: " + err.Error())
	}
	return out, nil
}

// dealHostApproval is one approval request the agent host raised (its own
// approval card, often raised by a sub-task's spend approval), as the host
// recorded it. A host may not keep, or document, its history of resolved
// approvals, so this input is optional and the output says when it is absent.
type dealHostApproval struct {
	ID             string `json:"id"`
	At             string `json:"at"`
	Task           string `json:"task,omitempty"`
	ExecutionID    string `json:"execution_id,omitempty"`
	AmountMinor    *int64 `json:"amount_minor,omitempty"`
	Currency       string `json:"currency,omitempty"`
	MerchantDomain string `json:"merchant_domain,omitempty"`
	Decision       string `json:"decision"`

	at time.Time
}

func readHostApprovals(path string) ([]dealHostApproval, error) {
	raw, err := readInput(path)
	if err != nil {
		return nil, err
	}
	var out []dealHostApproval
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), maxInput)
	for line := 1; sc.Scan(); line++ {
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		var a dealHostApproval
		if err := decodeJSON(text, &a); err != nil {
			return nil, inputError(fmt.Sprintf("approval record on line %d: invalid JSON or unknown field", line))
		}
		at, err := time.Parse(time.RFC3339, a.At)
		if err != nil {
			return nil, inputError(fmt.Sprintf("approval record on line %d: at must be an RFC 3339 time", line))
		}
		a.at = at.UTC()
		if err := guardMetadata(map[string]string{"id": a.ID, "task": a.Task, "execution_id": a.ExecutionID, "merchant_domain": a.MerchantDomain}, "merchant_domain"); err != nil {
			return nil, inputError(fmt.Sprintf("approval record on line %d: %s", line, err))
		}
		if a.ID == "" {
			return nil, inputError(fmt.Sprintf("approval record on line %d: id is required", line))
		}
		if !slices.Contains([]string{"approved", "denied", "unknown"}, a.Decision) {
			return nil, inputError(fmt.Sprintf("approval record on line %d: decision must be approved, denied or unknown", line))
		}
		out = append(out, a)
	}
	if err := sc.Err(); err != nil {
		return nil, inputError("approval records: " + err.Error())
	}
	return out, nil
}

// hostApprovalFor finds the host approval raised for rec: one that names its
// execution id, or else one before it within window, in the same task when
// both name one, with the same amount, currency and merchant domain wherever
// both carry one.
func hostApprovalFor(rec dealExecutionRecord, approvals []dealHostApproval, window time.Duration) *dealHostApproval {
	for i := range approvals {
		if approvals[i].ExecutionID != "" && approvals[i].ExecutionID == rec.ID {
			return &approvals[i]
		}
	}
	for i := range approvals {
		a := &approvals[i]
		d := rec.at.Sub(a.at)
		switch {
		case a.ExecutionID != "", d < 0 || d > window:
		case a.Task != "" && rec.Task != "" && a.Task != rec.Task:
		case a.AmountMinor != nil && rec.AmountMinor != nil && *a.AmountMinor != *rec.AmountMinor:
		case a.Currency != "" && rec.Currency != "" && !strings.EqualFold(a.Currency, rec.Currency):
		case a.MerchantDomain != "" && rec.MerchantDomain != "" && normalDomain(a.MerchantDomain) != normalDomain(rec.MerchantDomain):
		default:
			return a
		}
	}
	return nil
}

// dealConsequentialActions is the mechanical trigger: every point of no return
// of any deal type. Whether an action is consequential depends only on what it
// does, never on who the other side is.
var dealConsequentialActions = func() []string {
	var all []string
	for _, actions := range dealPointsOfNoReturn {
		for _, a := range actions {
			if !slices.Contains(all, a) {
				all = append(all, a)
			}
		}
	}
	sort.Strings(all)
	return all
}()

var (
	metadataToken       = regexp.MustCompile(`^[A-Za-z0-9._:@/+-]{1,200}$`)
	metadataHost        = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	panLike             = regexp.MustCompile(`\d(?:[ -]?\d){12,18}`)
	execReferenceSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	currencyCode        = regexp.MustCompile(`^[A-Za-z]{3}$`)
)

// guardMetadata refuses an identifier field that is not shaped like one, or
// that carries a card-number-like run of digits. The refusal names the field,
// never the value.
func guardMetadata(fields map[string]string, hosts ...string) error {
	for name, v := range fields {
		if v == "" {
			continue
		}
		shape := metadataToken
		if slices.Contains(hosts, name) {
			shape = metadataHost
		}
		if !shape.MatchString(v) || panLike.MatchString(v) {
			return errors.New(name + " must be an identifier (metadata only: no card numbers, codes or form content)")
		}
	}
	return nil
}

func (r *dealExecutionRecord) validate() error {
	if err := guardMetadata(map[string]string{"id": r.ID, "task": r.Task, "parent_task": r.ParentTask, "tool": r.Tool, "merchant_domain": r.MerchantDomain}, "merchant_domain"); err != nil {
		return err
	}
	if r.ID == "" {
		return errors.New("id is required")
	}
	if r.ReferenceSHA256 != "" && !execReferenceSHA256.MatchString(r.ReferenceSHA256) {
		return errors.New("reference_sha256 must be 64 lowercase hex digits")
	}
	if r.Currency != "" && !currencyCode.MatchString(r.Currency) {
		return errors.New("currency must be a 3-letter code")
	}
	at, err := time.Parse(time.RFC3339, r.At)
	if err != nil {
		return errors.New("at must be an RFC 3339 time")
	}
	r.at = at.UTC()
	if strings.TrimSpace(r.Tool) == "" {
		return errors.New("tool is required")
	}
	if r.Action != "other" && !slices.Contains(dealConsequentialActions, r.Action) {
		return errors.New("action must be one of " + strings.Join(dealConsequentialActions, ", ") + " or other")
	}
	if !slices.Contains([]string{"succeeded", "failed", "unknown"}, r.Status) {
		return errors.New("status must be succeeded, failed or unknown")
	}
	if r.DealID != "" && !dealIDPattern.MatchString(r.DealID) {
		return errors.New("deal_id is not a deal id")
	}
	return nil
}

// dealCoverageGaps is printed with every result: what this pass cannot see.
var dealCoverageGaps = []string{
	"Only the execution records in the file it was given: a session, side conversation or sub-task whose records were not exported is not seen.",
	"Anything the agent host summarised away or never wrote to its execution records is not seen.",
	"A failed or ambiguous attempt that left no local trace is not seen.",
	"Which tool calls are consequential is decided by the reader that produced the file; a call it mapped to \"other\" is not checked.",
	"Matching uses the deal id, or the action, amount, currency, merchant domain and time; it does not see the merchant's or the payment provider's records.",
	"It runs on the agent's own machine, over the agent host's own records: it can list actions that have no deal record, but it cannot prove that nothing else happened.",
}

// dealReconcileScope is printed with every result: what this pass is about.
const dealReconcileScope = "This pass covers the execution records it was given, for this period. It is not a record of everything the agent did."

// dealNoApprovalsGap is added when no approval history was given.
const dealNoApprovalsGap = "The agent host's history of approvals was not available, so whether the user approved each action on the host is not shown."

// dealCoverStep is a sealed step that can account for an executed action: an
// act noted after it, or a check made before it.
type dealCoverStep struct {
	dealID, kind, action, currency, domain, capsuleID string
	n                                                 int64
	amount                                            *int64
	at                                                time.Time
	used                                              bool
}

func dealCoverSteps(dealID string, events []sealedEvent) []*dealCoverStep {
	open := events[0].Event.Open
	snapshots := map[string]*dealSnapshot{}
	var out []*dealCoverStep
	for _, se := range events {
		e := se.Event
		at, err := time.Parse(time.RFC3339, e.At)
		if err != nil {
			continue
		}
		step := &dealCoverStep{dealID: dealID, kind: e.Kind, currency: open.Terms.Currency, domain: open.Who.Domain, capsuleID: se.CapsuleID, n: e.N, at: at.UTC()}
		switch e.Kind {
		case "snapshot":
			snapshots[se.CapsuleID] = e.Snapshot
			continue
		case "act":
			step.action, step.amount = e.Act.Action, e.Act.AmountMinor
			if e.Act.Currency != "" {
				step.currency = e.Act.Currency
			}
		case "check":
			step.action = e.Check.Action
			if snap := snapshots[e.Check.Snapshot]; snap != nil {
				step.amount = snap.AmountMinor
				if snap.Who != nil && snap.Who.Domain != "" {
					step.domain = snap.Who.Domain
				}
			}
		default:
			continue
		}
		out = append(out, step)
	}
	return out
}

func normalDomain(d string) string {
	return strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), "."), "www.")
}

// covers reports whether step accounts for rec: the same action, and the
// amount, currency and merchant domain wherever both sides carry one. Without
// an explicit deal id, an act must be within window of the action and a check
// within window before it.
func (step *dealCoverStep) covers(rec dealExecutionRecord, window time.Duration) bool {
	if step.used || step.action != rec.Action {
		return false
	}
	if rec.AmountMinor != nil && step.amount != nil && *rec.AmountMinor != *step.amount {
		return false
	}
	if rec.Currency != "" && step.currency != "" && !strings.EqualFold(rec.Currency, step.currency) {
		return false
	}
	if rec.MerchantDomain != "" && step.domain != "" && normalDomain(rec.MerchantDomain) != normalDomain(step.domain) {
		return false
	}
	if rec.DealID != "" {
		return rec.DealID == step.dealID
	}
	d := rec.at.Sub(step.at)
	if step.kind == "check" {
		return d >= 0 && d <= window
	}
	return d >= -window && d <= window
}

func (rec dealExecutionRecord) row() map[string]any {
	m := map[string]any{"id": rec.ID, "at": rec.At, "tool": rec.Tool, "action": rec.Action, "status": rec.Status}
	for k, v := range map[string]string{"task": rec.Task, "parent_task": rec.ParentTask, "currency": rec.Currency, "merchant_domain": rec.MerchantDomain, "reference_sha256": rec.ReferenceSHA256, "deal_id": rec.DealID} {
		if v != "" {
			m[k] = v
		}
	}
	if rec.AmountMinor != nil {
		m["amount_minor"] = *rec.AmountMinor
	}
	return m
}

// reconcileDeals matches the period's consequential records to sealed steps.
// Acts are preferred to checks, and each step accounts for one action.
// approvals is nil when the host's approval history was not given.
func reconcileDeals(records []dealExecutionRecord, approvals []dealHostApproval, steps []*dealCoverStep, from, to time.Time, window time.Duration) map[string]any {
	sort.SliceStable(records, func(i, j int) bool { return records[i].at.Before(records[j].at) })
	recorded, unrecorded, failed := []map[string]any{}, []map[string]any{}, []map[string]any{}
	read, outside, other := 0, 0, 0
	for _, rec := range records {
		if rec.at.Before(from) || !rec.at.Before(to) {
			outside++
			continue
		}
		read++
		if rec.Action == "other" {
			other++
			continue
		}
		if rec.Status == "failed" {
			failed = append(failed, rec.row())
			continue
		}
		row := rec.row()
		if approvals != nil {
			row["host_approval"] = nil
			if a := hostApprovalFor(rec, approvals, window); a != nil {
				row["host_approval"] = map[string]any{"id": a.ID, "at": a.At, "decision": a.Decision}
			}
		}
		var match *dealCoverStep
		for _, kind := range []string{"act", "check"} {
			for _, step := range steps {
				if step.kind == kind && step.covers(rec, window) {
					match = step
					break
				}
			}
			if match != nil {
				break
			}
		}
		if match == nil {
			unrecorded = append(unrecorded, row)
			continue
		}
		match.used = true
		row["deal_id"], row["matched_by"], row["step"], row["capsule_id"] = match.dealID, match.kind, match.n, match.capsuleID
		recorded = append(recorded, row)
	}
	consequential := len(recorded) + len(unrecorded)
	gaps := dealCoverageGaps
	hostApprovals := map[string]any{"available": approvals != nil, "read": len(approvals)}
	if approvals == nil {
		gaps = append(slices.Clone(gaps), dealNoApprovalsGap)
	}
	summary := fmt.Sprintf("%d of %d consequential actions have no deal record (%d execution records read for %s to %s). This lists what is missing from the records read; it cannot prove that nothing else happened.",
		len(unrecorded), consequential, read, from.Format(time.RFC3339), to.Format(time.RFC3339))
	return map[string]any{
		"scope":             dealReconcileScope,
		"period":            map[string]any{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		"summary":           summary,
		"records_read":      read,
		"outside_period":    outside,
		"not_consequential": other,
		"consequential":     consequential,
		"recorded":          recorded,
		"unrecorded":        unrecorded,
		"failed_attempts":   failed,
		"coverage":          map[string]any{"reads": "the agent host's execution records (tool calls of the agent and its sub-tasks), not the conversation", "host_approvals": hostApprovals, "cannot_see": gaps},
	}
}

func dealReconcileCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "reconcile", Short: "List consequential actions in the agent host's execution records that have no deal record; seals nothing", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("executions")
		approvalsPath, _ := c.Flags().GetString("approvals")
		window, _ := c.Flags().GetDuration("window")
		if window <= 0 {
			return inputError("--window must be positive")
		}
		to := dealClock()
		if v, _ := c.Flags().GetString("to"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return inputError("--to must be an RFC 3339 time")
			}
			to = t.UTC()
		}
		from := to.Add(-24 * time.Hour)
		if v, _ := c.Flags().GetString("from"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return inputError("--from must be an RFC 3339 time")
			}
			from = t.UTC()
		}
		if !from.Before(to) {
			return inputError("--from must be before --to")
		}
		records, err := jsonlExecutionReader{path: path}.Read(c.Context())
		if err != nil {
			return err
		}
		var approvals []dealHostApproval
		if approvalsPath != "" {
			if approvals, err = readHostApprovals(approvalsPath); err != nil {
				return err
			}
			if approvals == nil {
				approvals = []dealHostApproval{}
			}
		}
		var result map[string]any
		err = runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			steps, err := s.allCoverSteps(ctx)
			if err != nil {
				return err
			}
			result = reconcileDeals(records, approvals, steps, from, to, window)
			return nil
		})
		if err != nil {
			return err
		}
		if err = output(c, result); err != nil {
			return err
		}
		if len(result["unrecorded"].([]map[string]any)) > 0 {
			return ErrPartial
		}
		return nil
	}}
	cmd.Flags().String("executions", "", "deal-execution-record/v0 JSONL file of the agent host's executed tool calls (see skills/deal/RECONCILE.md)")
	cmd.Flags().String("approvals", "", "Optional deal-host-approval-record/v0 JSONL file of the approvals the agent host raised; without it the output says approvals were not available")
	cmd.Flags().String("from", "", "Start of the period, RFC 3339 (default: 24 hours before --to)")
	cmd.Flags().String("to", "", "End of the period, RFC 3339, exclusive (default: now)")
	cmd.Flags().Duration("window", 30*time.Minute, "How far apart an action and its deal step may be when the record names no deal")
	return cmd
}

// allCoverSteps reads every deal in the store, verifying each as `deal report`
// does, and returns the steps that can account for an executed action.
func (s *dealSession) allCoverSteps(ctx context.Context) (_ []*dealCoverStep, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT deal_id FROM deal_steps ORDER BY deal_id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var steps []*dealCoverStep
	for _, id := range ids {
		if err = s.useDeal(ctx, id, false); err != nil {
			return nil, err
		}
		events, err := s.load(ctx, id)
		err = errors.Join(err, s.t.close())
		s.t = nil
		if err != nil {
			return nil, err
		}
		steps = append(steps, dealCoverSteps(id, events)...)
	}
	return steps, nil
}
