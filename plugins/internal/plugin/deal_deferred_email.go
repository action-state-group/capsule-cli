package plugin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/spf13/cobra"
)

// A merchant's email whose key records cannot be read now (DNS is not
// reachable from this device) is not refused, and the user is never asked to
// look the records up. The email waits in the local store, unsealed, and the
// next cadence tick (`deal tick`) reads the records again and seals it on its
// deal as the deal stands then (after a close, it is later evidence linked to
// the close, as any evidence sealed after a close is). Nothing about the email
// is sealed until its key records are read: a deferred capture is never
// presented as a checked one, and its bytes are never deleted unsealed.

// deal_deferred_emails is local only, like deal_steps.local: the raw email
// and the evidence note it came with, until a tick seals them. Ids are never
// reused (AUTOINCREMENT), so a tick that read a row before another tick
// sealed it cannot mistake a later row for it. refused holds why the deal
// would not take the email when a tick tried; such a row is not retried.
var dealDeferredEmailSchema = `CREATE TABLE IF NOT EXISTS deal_deferred_emails (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	deal_id TEXT NOT NULL,
	queued_at TEXT NOT NULL,
	evidence TEXT NOT NULL,
	raw BLOB NOT NULL,
	refused TEXT NOT NULL DEFAULT ''
)`

// evidenceAgainst binds an evidence note to the deal as it stands: on a
// closed deal it confirms the close (and carries no new obligation), and a
// resolves names a step of this deal that holds an obligation.
func evidenceAgainst(events []sealedEvent, ev *dealEvent) error {
	ev.Confirms = ""
	if i := finalClose(events); i >= 0 {
		if ev.Evidence.Obligation != nil {
			return cli.InputError("this deal is closed; a new cancel-by date belongs on a new deal (`deal open`)")
		}
		ev.Confirms = events[i].CapsuleID
	}
	if ev.Evidence.ResolvesStep != 0 || ev.Evidence.Resolves != "" {
		target := ev.Evidence.Resolves
		for _, se := range events {
			if se.Event.N == ev.Evidence.ResolvesStep || (target != "" && se.CapsuleID == target) {
				target = se.CapsuleID
				if se.Event.Evidence == nil || se.Event.Evidence.Obligation == nil {
					return cli.InputError("resolves names a step that holds no obligation")
				}
			}
		}
		if target == "" {
			return cli.InputError("resolves names no step of this deal")
		}
		ev.Evidence.Resolves, ev.Evidence.ResolvesStep = target, 0
	}
	return nil
}

// evidenceOutput adds what a sealed evidence step reports about its
// merchant email and its obligation, for `deal note` and for a tick alike.
func evidenceOutput(out map[string]any, e dealEvidence, events []sealedEvent) {
	if w := e.Wrapped; w != nil {
		wrappedOutput(out, w)
	}
	if m := e.Email; m != nil {
		out["dkim"] = m.DKIM.Result
		out["merchant_signed"] = m.DKIM.Merchant
		out["key_source"] = m.KeySource
		out["dmarc_policy"] = m.DKIM.Policy
		out["scope"] = emailScopeLine
		out["merchant_says"] = emailVerdictWords(m.DKIM)
		out["we_say"] = ourSealWords
		out["parsed"] = m.Parsed
		if hint := obligationHint(m.Parsed); hint != nil && e.Obligation == nil {
			// Proposed, never sealed by itself: the agent checks it
			// against the email and seals it as evidence.
			out["obligation_hint"] = hint
		}
	}
	if o := e.Obligation; o != nil {
		deadline := map[string]any{"text": o.sentence(events[0].Event.Open.Terms.Currency), "note": deadlineNotEnforced}
		if o.due() {
			deadline["due_by"] = o.DueBy
		} else {
			deadline["cancel_by"] = o.CancelBy
		}
		out["deadline"] = deadline
	}
}

// deferEmail queues an email whose key records could not be read, and says so.
func (s *dealSession) deferEmail(ctx context.Context, c *cobra.Command, dealID string, ev dealEvidence, raw []byte) error {
	ev.Email, ev.Verified = nil, false
	note, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO deal_deferred_emails (deal_id, queued_at, evidence, raw) VALUES (?,?,?,?)`,
		dealID, cli.DealClock().UTC().Format("2006-01-02T15:04:05Z"), string(note), raw); err != nil {
		return err
	}
	out := map[string]any{
		"deal_id": dealID, "state": "deferred", "sealed": false,
		"message_digest": emailDigest(raw),
		"next":           "nothing to do now: the signing domain's key could not be read from this device, so the email is kept here unsealed and the next `deal tick` reads the key and seals it; on the next turn about this deal, run deal report --html and send the updated receipt if the email is sealed by then",
	}
	cli.Speak(out, s.p, func(w cli.Wording) cli.Narration { return line(w.Line("say.email.deferred", nil)) })
	// Read from the email alone, no key needed: still only a proposal.
	if hint := obligationHint(parseMerchantEmail(raw)); hint != nil && ev.Obligation == nil {
		out["obligation_hint"] = hint
	}
	return cli.Output(c, out)
}

type deferredEmail struct {
	id       int64
	dealID   string
	queuedAt string
	note     string
	raw      []byte
	email    *merchantEmail
	err      error // the capture failed, other than for want of DNS
}

func (d deferredEmail) about() map[string]any {
	return map[string]any{"deal_id": d.dealID, "queued_at": d.queuedAt, "message_digest": emailDigest(d.raw)}
}

// sealDeferredEmails runs at each `deal tick`, before the tick is cut: it
// reads the key records of every deferred email again, with the store
// unlocked, then seals each one it could read. One still unreachable stays
// waiting, with no limit: it is sealed whenever its key can be read, and the
// verdict is then whatever that key says. One its deal will not take is kept,
// marked refused, and listed at every tick. A failure on one email is listed
// and never stops the others. Returns nil when nothing was queued.
func sealDeferredEmails(c *cobra.Command) (map[string]any, error) {
	var queued []deferredEmail
	refused := []map[string]any{}
	err := runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
		rows, err := s.db.QueryContext(ctx, `SELECT id, deal_id, queued_at, evidence, raw, refused FROM deal_deferred_emails ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d deferredEmail
			var why string
			if err := rows.Scan(&d.id, &d.dealID, &d.queuedAt, &d.note, &d.raw, &why); err != nil {
				return err
			}
			if why != "" {
				r := d.about()
				r["reason"] = why
				refused = append(refused, r)
				continue
			}
			queued = append(queued, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(queued) == 0 && len(refused) == 0 {
		return nil, nil
	}
	waiting := []map[string]any{}
	var ready []deferredEmail
	for _, d := range queued {
		m, err := captureEmail(d.raw, nil)
		var unreachable *keysUnreachableError
		if errors.As(err, &unreachable) {
			waiting = append(waiting, d.about())
			continue
		}
		d.email, d.err = m, err
		ready = append(ready, d)
	}
	sealed, failed := []map[string]any{}, []map[string]any{}
	if len(ready) > 0 {
		err = runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			for _, d := range ready {
				out, err := s.sealDeferredEmail(ctx, d)
				var why *refusedEmail
				switch {
				case errors.As(err, &why):
					if _, err := s.db.ExecContext(ctx, `UPDATE deal_deferred_emails SET refused=? WHERE id=?`, why.Error(), d.id); err != nil {
						return err
					}
					r := d.about()
					r["reason"] = why.Error()
					refused = append(refused, r)
				case err != nil:
					// Kept, and tried again at the next tick.
					r := d.about()
					r["error"] = err.Error()
					failed = append(failed, r)
				case out != nil:
					sealed = append(sealed, out)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := map[string]any{"sealed": sealed, "waiting": waiting, "refused": refused}
	if len(failed) > 0 {
		out["failed"] = failed
	}
	if len(refused) > 0 {
		out["refused_note"] = "not sealed, and no tick will seal them: the deal would not take these emails (reason given). They are kept on this device; tell the user the merchant's email could not be added to the deal, and why"
	}
	return out, nil
}

// refusedEmail is a deferred email its deal will not take.
type refusedEmail struct{ err error }

func (e *refusedEmail) Error() string { return e.err.Error() }

// sealDeferredEmail seals one deferred email on its deal and checkpoints at
// once. The row is removed as soon as the step is sealed. An email whose
// exact bytes the deal already holds (a tick that sealed it and stopped
// before removing the row) is not sealed again. Returns a nil result when
// the row is gone (another tick took it).
func (s *dealSession) sealDeferredEmail(ctx context.Context, d deferredEmail) (map[string]any, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deal_deferred_emails WHERE id=? AND refused=''`, d.id).Scan(&n); err != nil || n == 0 {
		return nil, err
	}
	if d.err != nil {
		return nil, &refusedEmail{d.err}
	}
	events, err := s.loadOther(ctx, d.dealID)
	if errors.Is(err, cli.ErrInput) {
		return nil, &refusedEmail{err}
	} else if err != nil {
		return nil, err
	}
	digest := emailDigest(d.raw)
	for _, se := range events {
		if e := se.Event.Evidence; e != nil && e.Email != nil && emailDigest(e.Email.Raw) == digest {
			_, err := s.db.ExecContext(ctx, `DELETE FROM deal_deferred_emails WHERE id=?`, d.id)
			out := stepOutput(d.dealID, se)
			out["already_sealed"] = true
			return out, err
		}
	}
	var note dealEvidence
	if err := json.Unmarshal([]byte(d.note), &note); err != nil {
		return nil, &refusedEmail{errors.New("its note could not be read back: " + err.Error())}
	}
	note.Email = d.email
	note.Verified = d.email.DKIM.Result == "pass" && d.email.DKIM.Merchant
	ev := dealEvent{Kind: "evidence", Evidence: &note}
	if err := evidenceAgainst(events, &ev); err != nil {
		return nil, &refusedEmail{err}
	}
	se, err := s.seal(ctx, d.dealID, events, ev)
	if errors.Is(err, cli.ErrInput) {
		return nil, &refusedEmail{err}
	} else if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM deal_deferred_emails WHERE id=?`, d.id); err != nil {
		return nil, err
	}
	out := stepOutput(d.dealID, se)
	// The step is sealed whatever the checkpoint does: a failed checkpoint
	// is reported with it, never as the email not being sealed.
	if cp, err := s.milestone(ctx); err != nil {
		out["checkpoint"] = map[string]any{"state": "failed", "error": err.Error()}
	} else {
		out["checkpoint"] = cp
	}
	evidenceOutput(out, note, events)
	return out, nil
}
