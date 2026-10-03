package cli

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/emitter"
)

// The browser verifier is agent-action-capsule's own evidence-graph runtime,
// vendored unmodified and rebuilt reproducibly by
// scripts/build-evidence-graph-iife.sh. deal-view.js adds the deal section.
var (
	//go:embed assets/evidence-graph.iife.js
	evidenceGraphIIFE []byte
	//go:embed assets/evidence-graph.iife.js.sha256
	evidenceGraphIIFESHA256 string
	//go:embed assets/deal-view.js
	dealViewJS string
)

// dealReportBundle builds the deal's Evidence Bundle: every step of the deal's
// own log with membership proofs, the chain closed from the last step back to
// the opening one, and every step's x-deal-v0 record disclosed (records carry
// fingerprints and commitments, never raw values). The three-part report and
// one plain-words line per step, read from this device's local store, ride
// along as the x-deal-v0 extension. The page shows a step's line only when
// that step's record verified. Message text is in a line only when an
// anomaly cites that message.
func (s *dealSession) dealReportBundle(ctx context.Context, events []sealedEvent, report dealReport) (map[string]interface{}, error) {
	if s.dp.Checkpoint.Signing == (Secret{}) {
		return nil, inputError("a deal report needs the profile's checkpoint key (see `deal init`)")
	}
	if _, err := cutCheckpoint(ctx, s.dp, s.t.log); err != nil {
		return nil, err
	}
	b, err := AssembleBundle(ctx, s.t.artifacts, s.t.log, s.dp.LogID, BundleOptions{
		Root: events[len(events)-1].CapsuleID, ClosureDepth: len(events) - 1, Payloads: "selected", WithDisclosure: true,
	})
	if err != nil {
		return nil, err
	}
	cited := map[string]bool{}
	for _, item := range report.Anomalies {
		for _, id := range item.Steps {
			cited[id] = true
		}
	}
	steps := make([]interface{}, len(events))
	for i, se := range events {
		steps[i] = map[string]interface{}{
			"n": integer(uint64(se.Event.N)), "kind": se.Event.Kind, "capsule_id": se.CapsuleID, "at": se.Event.At,
			"line": dealStepLine(se.Event, cited[se.CapsuleID]),
		}
	}
	items := func(list []dealReportItem) []interface{} {
		out := make([]interface{}, len(list))
		for i, item := range list {
			ids := make([]interface{}, len(item.Steps))
			for j, id := range item.Steps {
				ids[j] = id
			}
			m := map[string]interface{}{"kind": item.Kind, "text": item.Text, "steps": ids}
			if item.Side != "" {
				m["side"] = item.Side
			}
			out[i] = m
		}
		return out
	}
	// The portable checkpoint signature lets a verifier authenticate the
	// checkpoint in the page instead of labelling it producer-asserted.
	cp, _ := b["checkpoint"].(map[string]interface{})
	statement, err := base64.StdEncoding.DecodeString(fmt.Sprint(cp["statement"]))
	if err != nil {
		return nil, err
	}
	cp["cose"] = base64.RawURLEncoding.EncodeToString(statement)
	// A witness receipt already held for this checkpoint, re-verified now
	// under the profile's pinned witness key, rides in checkpoint.witnesses,
	// where `capsulectl verify --bundle --witness-directory` checks it. The
	// report itself never contacts the witness.
	entry, err := s.heldWitnessReceipt(ctx, statement)
	if err != nil {
		return nil, err
	}
	if entry != nil {
		cp["witnesses"] = []interface{}{entry}
	}
	b["extensions"] = map[string]interface{}{
		"x-deal-v0": map[string]interface{}{
			"deal_id": events[0].Event.DealID, "steps": steps, "asked": report.Asked, "asked_step": report.AskedStep,
			// The opening of the user's own words, so the page can check them
			// against the baseline's sealed verbatim_commitment.
			"asked_opening": map[string]interface{}{"nonce": events[0].Event.Nonces["verbatim"], "text": events[0].Event.Open.Intent.Verbatim},
			"did":           items(report.Did), "anomalies": items(report.Anomalies),
		},
	}
	if err = verifyProducedBundle(b, true); err != nil {
		return nil, err
	}
	return b, nil
}

// dealReportHTML renders one local, self-contained page: the bundle, the
// vendored verifier and the deal view. It needs no network to open or verify,
// and nothing is hosted: the agent attaches or hands over the file.
func dealReportHTML(b map[string]interface{}) (string, error) {
	page, err := emitter.EmitEvidenceGraphHTML(b, evidenceGraphIIFE)
	if err != nil {
		return "", err
	}
	replaceOnce := func(page, old, replacement string) (string, error) {
		if strings.Count(page, old) != 1 {
			return "", errors.New("report page shell changed; cannot place the deal section")
		}
		return strings.Replace(page, old, replacement, 1), nil
	}
	if page, err = replaceOnce(page, "<title>Evidence Graph</title>", "<title>Deal report</title>\n    <style>"+dealViewCSS+"</style>"); err != nil {
		return "", err
	}
	if page, err = replaceOnce(page, `<div id="app"></div>`, `<div id="deal"></div>`+"\n    "+`<div id="app"></div>`); err != nil {
		return "", err
	}
	end := strings.LastIndex(page, "</body>")
	if end < 0 || strings.Contains(dealViewJS, "</script") {
		return "", errors.New("report page shell changed; cannot place the deal section")
	}
	return page[:end] + "<script>" + dealViewJS + "</script>\n  " + page[end:], nil
}

const dealViewCSS = `
:root { --fg: #1b1b1f; --muted: #5d5d66; --bg: #ffffff; --line: #d9d9e0; --warn: #9a3b00; --ok: #1d6b35; }
@media (prefers-color-scheme: dark) { :root { --fg: #ececf1; --muted: #a3a3ad; --bg: #16161a; --line: #34343c; --warn: #ffb07a; --ok: #7fd49a; } }
body { background: var(--bg); color: var(--fg); }
#deal { max-width: 760px; margin: 0 auto; padding: 16px; line-height: 1.45; }
#deal h1 { font-size: 1.5rem; margin: 0.2rem 0; }
#deal h2 { font-size: 1.15rem; margin-top: 1.6rem; }
#deal h3 { font-size: 0.95rem; color: var(--muted); margin: 1rem 0 0.3rem; }
#deal .deal-flag summary { color: var(--warn); }
#deal .deal-steps { margin: 8px 0 0; padding-left: 1.4rem; }
#deal .deal-steps li { margin: 4px 0; overflow-wrap: anywhere; }
#deal .deal-at { color: var(--muted); font-size: 0.85rem; }
#deal .deal-note { color: var(--muted); font-size: 0.9rem; }
#deal .deal-rung { font-weight: 600; margin: 4px 0; }
#deal .deal-demo { display: inline-block; border: 1px solid var(--warn); color: var(--warn); padding: 0 6px; border-radius: 4px; font-size: 0.8rem; }
#deal .deal-bad { color: var(--warn); font-weight: 600; }
#deal details { border: 1px solid var(--line); border-radius: 6px; padding: 8px 12px; margin: 8px 0; }
#deal summary { cursor: pointer; font-weight: 600; }
#deal code { color: var(--muted); font-size: 0.8rem; overflow-wrap: anywhere; }
#app { max-width: 760px; margin: 0 auto; padding: 0 16px 16px; overflow-wrap: anywhere; }
`

// dealStepLine is one step in plain words, from the local store.
func dealStepLine(e dealEvent, showText bool) string {
	switch e.Kind {
	case "open":
		return fmt.Sprintf("Opened: %q", e.Open.Intent.Verbatim)
	case "message":
		if showText {
			return fmt.Sprintf("%s: %q", e.Message.From, e.Message.Text)
		}
		return "Message from " + e.Message.From + " (text kept on the device)"
	case "snapshot":
		line := "About to " + e.Snapshot.Action
		if e.Snapshot.Description != "" {
			line += ": " + e.Snapshot.Description
		}
		return line
	case "check":
		if e.Check.Verdict == "pass" {
			return "Check: no differences"
		}
		var texts []string
		for _, d := range e.Check.Differences {
			if d.Text != "" {
				texts = append(texts, d.Text)
			}
		}
		return "Check flagged: " + strings.Join(texts, " · ")
	default:
		line := trailLine(e)
		return strings.ToUpper(line[:1]) + line[1:]
	}
}
