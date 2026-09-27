package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/spf13/cobra"
)

// `capsulectl deal` is the single choke point an agent goes through before it
// pays, books, signs or shares on a user's behalf. Every step is sealed as a
// Capsule in the profile's SQLite store and appended to its checkpointed log
// before anything is compared or shown:
//
//	snapshot -> seal -> diff -> show -> approval -> seal approval -> act
//
// The seed that signs these Capsules lives on the same machine as the agent,
// so the trail is tamper-evident (a later edit or deletion is detectable), not
// non-repudiation (it does not prove the user, rather than the machine, said
// something).

// dealClock is the time source for sealed timestamps; tests replace it.
var dealClock = func() time.Time { return time.Now().UTC() }

const dealIndexSchema = `CREATE TABLE IF NOT EXISTS deal_events (
	deal_id TEXT NOT NULL,
	n INTEGER NOT NULL,
	kind TEXT NOT NULL,
	capsule_id TEXT NOT NULL,
	cll_sequence INTEGER NOT NULL,
	PRIMARY KEY (deal_id, n)
)`

// dealSession holds the write lock, the opened store and the signing key for
// one deal command.
type dealSession struct {
	p      Profile
	t      *target
	key    ed25519.PrivateKey
	keys   []ed25519.PublicKey
	unlock func() error
}

func openDealSession(ctx context.Context, p Profile) (_ *dealSession, err error) {
	if p.Type != "sqlite" {
		return nil, inputError("deal commands need a sqlite profile; create one with `deal init`")
	}
	if p.ReadOnly {
		return nil, ErrReadOnlyCLL
	}
	key, err := privateKey(p.Signing)
	if err != nil {
		return nil, err
	}
	if err = requirePublisherKey(p, key); err != nil {
		return nil, err
	}
	keys, err := parseKeys(p.TrustedKeys)
	if err != nil {
		return nil, err
	}
	dbPath, err := filepath.Abs(p.Connection.Database)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(dbPath); err != nil {
		return nil, inputError("deal store does not exist; run `deal init`")
	}
	unlock, err := lockDealStore(dbPath)
	if err != nil {
		return nil, err
	}
	s := &dealSession{p: p, key: key, keys: keys, unlock: unlock}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	if s.t, err = openTarget(ctx, p, usePublication); err != nil {
		return nil, err
	}
	if _, err = s.t.db.ExecContext(ctx, dealIndexSchema); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *dealSession) close() error {
	var e error
	if s.t != nil {
		e = s.t.close()
	}
	return errors.Join(e, s.unlock())
}

// load reads a deal's steps back from the store and re-verifies each one: the
// Capsule signature and trust, the payload binding, and the chain of prev
// links. An index row that disagrees with its sealed payload is a conflict.
func (s *dealSession) load(ctx context.Context, dealID string) (_ []sealedEvent, err error) {
	rows, err := s.t.db.QueryContext(ctx, `SELECT n, kind, capsule_id, cll_sequence FROM deal_events WHERE deal_id=? ORDER BY n`, dealID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	type row struct {
		n        int64
		kind, id string
		seq      uint64
	}
	var index []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.n, &r.kind, &r.id, &r.seq); err != nil {
			return nil, err
		}
		index = append(index, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	events := make([]sealedEvent, 0, len(index))
	prev := ""
	for i, r := range index {
		record, err := s.t.artifacts.Get(ctx, r.id)
		if err != nil {
			return nil, err
		}
		checks, err := artifact.Verify(record, s.keys)
		if err != nil {
			return nil, err
		}
		var content []byte
		for _, a := range record.Artifacts {
			if a.Binding == artifact.PayloadDigest && a.State == artifact.Present {
				content = a.Content
			}
		}
		if content == nil || !checks["payload"].Verified {
			return nil, ErrConflict
		}
		var ev dealEvent
		if err = decodeJSON(content, &ev); err != nil {
			return nil, ErrConflict
		}
		if ev.Spec != dealEventSpec || ev.DealID != dealID || ev.N != int64(i+1) || ev.N != r.n || ev.Kind != r.kind || ev.Prev != prev {
			return nil, ErrConflict
		}
		events = append(events, sealedEvent{CapsuleID: r.id, Sequence: r.seq, Event: ev})
		prev = r.id
	}
	return events, nil
}

// seal signs one step, persists it, appends it to the log and indexes it.
// Any failure is returned: a step that is not sealed did not pass the choke
// point.
func (s *dealSession) seal(ctx context.Context, dealID string, events []sealedEvent, ev dealEvent) (sealedEvent, error) {
	now := dealClock().Truncate(time.Second)
	ev.Spec = dealEventSpec
	ev.DealID = dealID
	ev.N = int64(len(events) + 1)
	ev.At = now.Format(time.RFC3339)
	if len(events) > 0 {
		ev.Prev = events[len(events)-1].CapsuleID
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return sealedEvent{}, err
	}
	request := Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{ActionID: fmt.Sprintf("%s/%d", dealID, ev.N), ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal", Timestamp: now},
		Payload: payload,
	}
	pub, err := s.t.publish(ctx, request, s.key)
	if err != nil {
		return sealedEvent{}, err
	}
	if _, err = s.t.db.ExecContext(ctx, `INSERT INTO deal_events (deal_id, n, kind, capsule_id, cll_sequence) VALUES (?,?,?,?,?)`, dealID, ev.N, ev.Kind, pub.CapsuleID, pub.Sequence); err != nil {
		return sealedEvent{}, err
	}
	return sealedEvent{CapsuleID: pub.CapsuleID, Sequence: pub.Sequence, Event: ev}, nil
}

// milestone cuts a signed checkpoint at a deal milestone (baseline, approval,
// close) when the profile has a checkpoint key, and offers it to the witness
// only when the operator configured one. A failed local checkpoint is an
// error; a witness that is slow or down leaves delivery pending, retryable
// with `cll checkpoint publish`.
func (s *dealSession) milestone(ctx context.Context) (map[string]any, error) {
	if s.p.Checkpoint.Signing == (Secret{}) {
		return map[string]any{"state": "not_configured"}, nil
	}
	cp, err := cutCheckpoint(ctx, s.p, s.t.log)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"state": "signed", "checkpoint": cp.Size, "witness": "not_configured"}
	service, err := serviceID(s.p)
	if err != nil {
		return nil, err
	}
	if service == "" {
		return out, nil
	}
	state, err := deliverWitness(ctx, s.p, s.t.log, service, cp.Size)
	if err != nil {
		out["witness"] = "pending"
		return out, nil
	}
	if err = verifyWitness(s.p, state); err != nil {
		return nil, err
	}
	out["witness"] = witnessResult(state)["state"]
	return out, nil
}

func dealCommands() *cobra.Command {
	deal := &cobra.Command{Use: "deal", Short: "Seal a deal's baseline and check every point of no return against it"}
	deal.AddCommand(dealInitCommand(), dealOpenCommand(), dealNoteCommand(), dealCheckCommand(), dealCloseCommand(), dealReportCommand())
	return deal
}

// runDeal selects the profile, opens a locked session, loads the named deal
// (when dealID is set) and refuses further steps on a finally closed deal.
func runDeal(c *cobra.Command, needDeal bool, fn func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error) (err error) {
	p, err := selected(c)
	if err != nil {
		return err
	}
	dealID := ""
	if needDeal {
		dealID, _ = c.Flags().GetString("deal")
		if dealID == "" {
			return inputError("--deal is required")
		}
	}
	ctx := c.Context()
	s, err := openDealSession(ctx, p)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	var events []sealedEvent
	if needDeal {
		if events, err = s.load(ctx, dealID); err != nil {
			return err
		}
		if len(events) == 0 {
			return inputError("unknown deal: " + dealID)
		}
	}
	return fn(ctx, s, dealID, events)
}

func dealFinallyClosed(events []sealedEvent) bool {
	last := events[len(events)-1].Event
	return last.Kind == "close" && last.Close.Outcome != "open"
}

func stepOutput(dealID string, se sealedEvent) map[string]any {
	return map[string]any{"deal_id": dealID, "step": se.Event.N, "kind": se.Event.Kind, "capsule_id": se.CapsuleID, "sequence": se.Sequence}
}

func writeSeed(path string) (ed25519.PrivateKey, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, inputError("cannot create key file (it must not already exist): " + path)
	}
	_, werr := f.WriteString(hex.EncodeToString(private.Seed()))
	if err = errors.Join(werr, f.Sync(), f.Close()); err != nil {
		return nil, err
	}
	return private, nil
}

func dealInitCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "init", Short: "Create a local deal profile: SQLite store, 0600 signing and checkpoint seeds", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		name, _ := c.Flags().GetString("profile")
		dir, _ := c.Flags().GetString("dir")
		if name == "" || dir == "" {
			return inputError("--profile and --dir are required")
		}
		if dir, err = filepath.Abs(dir); err != nil {
			return err
		}
		if path, e := profilePath(name); e != nil {
			return e
		} else if _, e = os.Lstat(path); e == nil {
			return inputError("profile already exists: " + name)
		}
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		signing, err := writeSeed(filepath.Join(dir, "signing.seed"))
		if err != nil {
			return err
		}
		cpKey, err := writeSeed(filepath.Join(dir, "checkpoint.seed"))
		if err != nil {
			return err
		}
		signer, err := checkpoint.NewEd25519Signer(cpKey)
		if err != nil {
			return err
		}
		public := hex.EncodeToString(signing.Public().(ed25519.PublicKey))
		p := Profile{Name: name, Type: "sqlite", LogID: "deal", Namespace: "deal"}
		p.Connection.Database = filepath.Join(dir, "deal.db")
		p.Signing.File = filepath.Join(dir, "signing.seed")
		p.TrustedKeys = []string{public}
		p.Checkpoint.Signing.File = filepath.Join(dir, "checkpoint.seed")
		p.Checkpoint.TrustedKeys = []string{signer.KeyID()}
		if err = saveProfile(p, false); err != nil {
			return err
		}
		t, err := openTarget(c.Context(), p, useInitialization)
		if err != nil {
			return err
		}
		if err = t.close(); err != nil {
			return err
		}
		if err = os.Chmod(p.Connection.Database, 0o600); err != nil {
			return err
		}
		return output(c, map[string]any{
			"profile": name, "store": p.Connection.Database, "public_key": public, "witness": "not_configured",
			"guarantee": "tamper-evident, not non-repudiation: the signing seed is on this machine",
		})
	}}
	cmd.Flags().String("dir", "", "Directory for the store and seeds (created 0700)")
	return cmd
}

func dealOpenCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "open", Short: "Seal the baseline: the user's verbatim words, who, terms, claims and recourse", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := readInput(path)
		if err != nil {
			return err
		}
		var o dealOpen
		if err = decodeJSON(raw, &o); err != nil {
			return err
		}
		if err = o.validate(); err != nil {
			return err
		}
		return runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			id := make([]byte, 8)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			dealID := "deal-" + hex.EncodeToString(id)
			se, err := s.seal(ctx, dealID, nil, dealEvent{Kind: "open", Open: &o})
			if err != nil {
				return err
			}
			cp, err := s.milestone(ctx)
			if err != nil {
				return err
			}
			out := stepOutput(dealID, se)
			out["demo"] = o.Demo
			out["points_of_no_return"] = dealPointsOfNoReturn[o.Type]
			out["checkpoint"] = cp
			out["remote_warm"] = dealRemoteWarm(ctx)
			return output(c, out)
		})
	}}
	cmd.Flags().String("input", "", "Baseline JSON: type, intent, who, terms, claims, recourse")
	return cmd
}

// dealActInput is what an agent reports after acting. Authorization fields are
// computed by the wrapper and cannot be supplied.
type dealActInput struct {
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
	AmountMinor *int64 `json:"amount_minor,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Payee       string `json:"payee,omitempty"`
	Rail        string `json:"rail,omitempty"`
	Reference   string `json:"reference,omitempty"`
}

func dealNoteCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "note", Short: "Seal a message, claim, evidence, detail change, the user's answer to a check, or an action taken", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		kind, _ := c.Flags().GetString("kind")
		ev := dealEvent{Kind: kind}
		var act dealActInput
		switch kind {
		case "message", "claim", "evidence", "change", "act":
			path, _ := c.Flags().GetString("input")
			raw, err := readInput(path)
			if err != nil {
				return err
			}
			var target any
			switch kind {
			case "message":
				ev.Message = &dealMessage{}
				target = ev.Message
			case "claim":
				ev.Claim = &dealClaim{}
				target = ev.Claim
			case "evidence":
				ev.Evidence = &dealEvidence{}
				target = ev.Evidence
			case "change":
				ev.Change = &dealChange{}
				target = ev.Change
			case "act":
				target = &act
			}
			if err = decodeJSON(raw, target); err != nil {
				return err
			}
		case "approval":
			check, _ := c.Flags().GetString("check")
			choice, _ := c.Flags().GetString("choice")
			said, _ := c.Flags().GetString("said")
			if check == "" || choice == "" {
				return inputError("approval needs --check and --choice")
			}
			ev.Approval = &dealApproval{Check: check, Choice: choice, Said: said}
		default:
			return inputError("--kind must be message, claim, evidence, change, approval or act")
		}
		switch {
		case ev.Message != nil && (!slices.Contains([]string{"counterparty", "user", "agent"}, ev.Message.From) || strings.TrimSpace(ev.Message.Text) == ""):
			return inputError("message needs from (counterparty, user or agent) and text")
		case ev.Claim != nil:
			if err := ev.Claim.validate(); err != nil {
				return err
			}
		case ev.Evidence != nil && (ev.Evidence.About == "" || ev.Evidence.Source == ""):
			return inputError("evidence needs what it is about and its source")
		case ev.Change != nil && (ev.Change.Source == "" || (ev.Change.Who == nil && ev.Change.Terms == nil && ev.Change.Recourse == nil)):
			return inputError("change needs its source and at least one of who, terms or recourse")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			if dealFinallyClosed(events) {
				return inputError("deal is closed")
			}
			switch kind {
			case "approval":
				if err := judgeApproval(events, ev.Approval); err != nil {
					return err
				}
			case "act":
				if !slices.Contains(dealPointsOfNoReturn[events[0].Event.Open.Type], act.Action) {
					return inputError("act.action is not a point of no return for this deal type")
				}
				ev.Act = &dealAct{Action: act.Action, Description: act.Description, AmountMinor: act.AmountMinor, Currency: act.Currency, Payee: act.Payee, Rail: act.Rail, Reference: act.Reference}
				ev.Act.AuthorizedBy, ev.Act.Reason = authorizeAct(events, *ev.Act)
				ev.Act.Unchecked = ev.Act.AuthorizedBy == ""
			}
			se, err := s.seal(ctx, dealID, events, ev)
			if err != nil {
				return err
			}
			out := stepOutput(dealID, se)
			switch kind {
			case "approval":
				out["proceed"] = ev.Approval.Proceed
				out["reason"] = ev.Approval.Reason
				cp, err := s.milestone(ctx)
				if err != nil {
					return err
				}
				out["checkpoint"] = cp
			case "act":
				out["unchecked"] = ev.Act.Unchecked
				out["authorized_by"] = ev.Act.AuthorizedBy
				out["reason"] = ev.Act.Reason
			}
			return output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("kind", "", "message, claim, evidence, change, approval or act")
	cmd.Flags().String("input", "", "JSON body for message, claim, evidence, change or act")
	cmd.Flags().String("check", "", "approval: the check_id being answered")
	cmd.Flags().String("choice", "", "approval: the option id the user chose")
	cmd.Flags().String("said", "", "approval: the user's own words")
	return cmd
}

// judgeApproval binds an answer to one sealed check. It proceeds only when the
// user chose proceed and nothing was sealed since that check that could
// change what they were shown.
func judgeApproval(events []sealedEvent, a *dealApproval) error {
	for i, se := range events {
		if se.CapsuleID != a.Check {
			continue
		}
		if se.Event.Kind != "check" {
			return inputError("--check does not name a check of this deal")
		}
		check := se.Event.Check
		if check.Verdict == "pass" {
			return inputError("this check passed; no answer is needed")
		}
		if !slices.ContainsFunc(check.Options, func(o dealOption) bool { return o.ID == a.Choice }) {
			return inputError("--choice is not one of the options the check offered")
		}
		for _, later := range events[i+1:] {
			switch later.Event.Kind {
			case "change", "snapshot", "check":
				a.Reason = "details changed after this check; check again"
			}
		}
		a.Proceed = a.Choice == "proceed" && a.Reason == ""
		return nil
	}
	return inputError("--check does not name a step of this deal")
}

func dealCheckCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "check", Short: "Seal what is about to happen, compare it with the baseline, and return the difference card", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := readInput(path)
		if err != nil {
			return err
		}
		var snap dealSnapshot
		if err = decodeJSON(raw, &snap); err != nil {
			return err
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			if dealFinallyClosed(events) {
				return inputError("deal is closed")
			}
			open := events[0].Event.Open
			if !slices.Contains(dealPointsOfNoReturn[open.Type], snap.Action) {
				return inputError("action is not a point of no return for a " + open.Type + " deal; use one of " + strings.Join(dealPointsOfNoReturn[open.Type], ", "))
			}
			// snapshot -> seal
			snapped, err := s.seal(ctx, dealID, events, dealEvent{Kind: "snapshot", Snapshot: &snap})
			if err != nil {
				return err
			}
			events = append(events, snapped)
			// -> diff
			state, err := foldDeal(events)
			if err != nil {
				return err
			}
			result := evaluateDeal(state, snap)
			if result.Remote, err = dealRemoteCheck(ctx, state, snap, result); err != nil {
				return err
			}
			result.Differences = append(result.Differences, result.Remote.Differences...)
			settleCheck(&result, state)
			result.Snapshot = snapped.CapsuleID
			result.Card = renderCard(result, open.Demo)
			// -> seal the result before it is shown
			checked, err := s.seal(ctx, dealID, events, dealEvent{Kind: "check", Check: &result})
			if err != nil {
				return err
			}
			out := stepOutput(dealID, checked)
			out["check_id"] = checked.CapsuleID
			out["snapshot_id"] = snapped.CapsuleID
			out["verdict"] = result.Verdict
			out["proceed"] = result.Verdict == "pass"
			out["card"] = result.Card
			out["options"] = result.Options
			out["differences"] = result.Differences
			out["unverified"] = result.Unverified
			out["remote"] = result.Remote.Status
			out["demo"] = open.Demo
			return output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("input", "", "Snapshot JSON: the action and exactly what is about to happen")
	return cmd
}

func dealCloseCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "close", Short: "Compare what was delivered with what was agreed: completed, mismatch or open", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := readInput(path)
		if err != nil {
			return err
		}
		var in dealCloseInput
		if err = decodeJSON(raw, &in); err != nil {
			return err
		}
		if !slices.Contains([]string{"received", "pending", "not_received"}, in.Status) {
			return inputError("status must be received, pending or not_received")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			if dealFinallyClosed(events) {
				return inputError("deal is closed")
			}
			state, err := foldDeal(events)
			if err != nil {
				return err
			}
			result := closeDeal(state, in)
			for _, se := range events {
				if se.Event.Kind == "act" && se.Event.Act.Unchecked {
					result.UncheckedActions++
				}
			}
			se, err := s.seal(ctx, dealID, events, dealEvent{Kind: "close", Close: &result})
			if err != nil {
				return err
			}
			cp, err := s.milestone(ctx)
			if err != nil {
				return err
			}
			out := stepOutput(dealID, se)
			out["outcome"] = result.Outcome
			out["differences"] = result.Differences
			out["unchecked_actions"] = result.UncheckedActions
			out["checkpoint"] = cp
			return output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("input", "", "Close JSON: status and what was delivered")
	return cmd
}

func dealReportCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "report", Short: "Print the deal's sealed trail in plain words (the offline HTML receipt is not built yet)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		return runDeal(c, true, func(_ context.Context, _ *dealSession, dealID string, events []sealedEvent) error {
			steps := make([]map[string]any, 0, len(events))
			lines := make([]string, 0, len(events))
			unchecked := 0
			outcome := "open"
			for _, se := range events {
				e := se.Event
				line := trailLine(e)
				if e.Kind == "act" && e.Act.Unchecked {
					unchecked++
				}
				if e.Kind == "close" {
					outcome = e.Close.Outcome
				}
				steps = append(steps, map[string]any{"step": e.N, "kind": e.Kind, "at": e.At, "capsule_id": se.CapsuleID, "sequence": se.Sequence, "line": line})
				lines = append(lines, fmt.Sprintf("%d. %s %s", e.N, e.At, line))
			}
			return output(c, map[string]any{
				"deal_id": dealID, "demo": events[0].Event.Open.Demo, "outcome": outcome, "unchecked_actions": unchecked,
				"steps": steps, "text": strings.Join(lines, "\n"), "receipt": "not_available",
			})
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	return cmd
}
