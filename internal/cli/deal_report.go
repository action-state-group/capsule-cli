package cli

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
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
//
// A shared copy (audience counterparty or adjudicator) withholds every record
// that carries more than that audience may see, rewrites the deal section
// for it; the command then checks the final page (dealPageGate). Every copy
// states its scope and carries the command that verifies it.
//
// With sealReport, the copy's deal section is sealed as its own record on
// the deal's log (dealSealReport) and the bundle's x-deal-v0 extension only
// points at it; every earlier report's record is withheld, so a copy
// discloses its own text and no other copy's. Without it (a report that
// writes no file), nothing is put on the log and the section rides
// unsealed.
func (s *dealSession) dealReportBundle(ctx context.Context, events []sealedEvent, report dealReport, audience, verifyCommand string, sealReport bool) (map[string]interface{}, error) {
	if s.dp.Checkpoint.Signing == (Secret{}) {
		return nil, inputError("a deal report needs the profile's checkpoint key (see `deal init`)")
	}
	shared := audience != dealAudienceKeep
	var private dealPrivate
	withhold := map[string]bool{}
	if shared {
		private = dealPrivateValues(events)
		var err error
		if withhold, err = s.dealWithholdRecords(events, audience, private); err != nil {
			return nil, err
		}
	}
	dealID := events[0].Event.DealID
	reports, err := s.reportIDs(ctx, dealID)
	if err != nil {
		return nil, err
	}
	for id := range reports {
		withhold[id] = true
	}
	b, cadence, err := s.dealAssemble(ctx, events, withhold)
	if err != nil {
		return nil, err
	}
	coverage := witnessCoverage(events, cadence)
	ext, err := s.dealReportExtension(events, report, audience, verifyCommand, private, withhold, coverage)
	if err != nil {
		return nil, err
	}
	if sealReport {
		id, err := s.dealSealReport(ctx, dealID, audience, ext)
		if err != nil {
			return nil, err
		}
		if b, cadence, err = s.dealAssemble(ctx, events, withhold); err != nil {
			return nil, err
		}
		// The sealed section states the witness coverage of the steps, which
		// the report's own entry, after them, cannot change; a tick between
		// the two reads could, and the report is then made again.
		if after := witnessCoverage(events, cadence); !reflect.DeepEqual(after, coverage) {
			return nil, hint(ErrConflict, "the deal's witness state changed while the report was sealed: run the report again")
		}
		ext = map[string]interface{}{dealReportPointer: id}
	}
	b["extensions"] = map[string]interface{}{dealCadenceExtension: cadence, dealProfile: ext}
	if err = verifyProducedBundle(b, true); err != nil {
		return nil, err
	}
	return b, nil
}

// dealAssemble cuts the deal's checkpoint and assembles its bundle with the
// records in withhold withheld, the checkpoint's portable signature, a held
// witness receipt, and the deal's witness state through the cadence log.
func (s *dealSession) dealAssemble(ctx context.Context, events []sealedEvent, withhold map[string]bool) (map[string]interface{}, map[string]interface{}, error) {
	// Cut locally: a report, like every deal event, never queues anything
	// for the witness; only a due tick does.
	if _, err := cutCheckpoint(ctx, localOnly(s.dp), s.t.log); err != nil {
		return nil, nil, err
	}
	b, err := AssembleBundle(ctx, s.t.artifacts, s.t.log, s.dp.LogID, BundleOptions{
		Root: events[len(events)-1].CapsuleID, ClosureDepth: len(events) - 1, Payloads: "selected", WithDisclosure: true, Withhold: withhold,
	})
	if err != nil {
		return nil, nil, err
	}
	// The portable checkpoint signature lets a verifier authenticate the
	// checkpoint in the page instead of labelling it producer-asserted.
	cp, _ := b["checkpoint"].(map[string]interface{})
	statement, err := base64.StdEncoding.DecodeString(fmt.Sprint(cp["statement"]))
	if err != nil {
		return nil, nil, err
	}
	cp["cose"] = base64.RawURLEncoding.EncodeToString(statement)
	// A witness receipt already held for this checkpoint, re-verified now
	// under the profile's pinned witness key, rides in checkpoint.witnesses,
	// where `capsulectl verify --bundle --witness-directory` checks it. The
	// report itself never contacts the witness. It carries nothing private,
	// so a shared copy keeps it too.
	entry, err := s.heldWitnessReceipt(ctx, statement)
	if err != nil {
		return nil, nil, err
	}
	if entry != nil {
		cp["witnesses"] = []interface{}{entry}
	}
	// Where this checkpoint stands with the witness, through the cadence log.
	// The cadence chain is digests, salts and proofs, nothing private, so a
	// shared copy carries it too.
	cadence, err := s.dealWitnessState(ctx, events[0].Event.DealID, statement)
	if err != nil {
		return nil, nil, err
	}
	return b, cadence, nil
}

// dealReportExtension is a copy's deal section: for a shared copy, the one
// rewritten for its audience; for the user's own copy, the full report.
func (s *dealSession) dealReportExtension(events []sealedEvent, report dealReport, audience, verifyCommand string, private dealPrivate, withhold map[string]bool, coverage map[string]interface{}) (map[string]interface{}, error) {
	if audience != dealAudienceKeep {
		ext := dealShareExtension(events, report, audience, private, withhold)
		if coverage != nil {
			ext["witness_coverage"] = coverage
		}
		ext["did_line"] = dealDidLine(dealDidSources(events))
		ext["verify_command"] = verifyCommand
		return ext, nil
	}
	cited := map[string]bool{}
	for _, item := range report.Anomalies {
		for _, id := range item.Steps {
			cited[id] = true
		}
	}
	steps := make([]interface{}, len(events))
	restated := restatedIntents(events)
	for i, se := range events {
		line := dealStepLine(se.Event, cited[se.CapsuleID])
		if restated[se.CapsuleID] {
			line = trailLineIn(events, restated, i)
			line = strings.ToUpper(line[:1]) + line[1:]
		}
		steps[i] = map[string]interface{}{
			"n": integer(uint64(se.Event.N)), "kind": se.Event.Kind, "capsule_id": se.CapsuleID, "at": se.Event.At,
			"line": line,
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
			if item.At != "" {
				m["at"] = item.At
			}
			out[i] = m
		}
		return out
	}
	ext := map[string]interface{}{
		"deal_id": events[0].Event.DealID, "steps": steps, "asked": report.Asked, "asked_step": report.AskedStep,
		// The opening of the user's own words, so the page can check them
		// against the baseline's sealed verbatim_commitment.
		"asked_opening": map[string]interface{}{"nonce": events[0].Event.Nonces["verbatim"], "text": events[0].Event.Open.Intent.Verbatim},
		// The openings of the materiality predicates' names and versions,
		// committed in the records (label_commitment): the user's own
		// copy says which predicate decided, a shared copy only its digest.
		"materiality_openings": materialityOpenings(events),
		"did":                  items(report.Did), "anomalies": items(report.Anomalies),
		"told":     toldItems(report.Told, true),
		"did_line": dealDidLine(dealDidSources(events)),
		// Which builds sealed the steps, and which one made this page:
		// the page compares the two from data it already holds. Nothing
		// is fetched to do it.
		"instructions": report.Instructions,
		"produced_by":  stringList(dealProducers(events)), "page_built_by": currentProducer().String(), "page_version": cliVersion,
		"scope": dealScopeLine, "audience": dealAudienceKeep, "verify_command": verifyCommand,
	}
	// The merchant rows carry only strings and booleans; round-trip them to
	// the generic JSON shape the bundle encoder takes.
	var merchant []interface{}
	raw, err := json.Marshal(report.Merchant)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &merchant); err != nil {
		return nil, err
	}
	ext["merchant"] = merchant
	ext["email_scope"] = emailScopeLine
	if coverage != nil {
		ext["witness_coverage"] = coverage
	}
	if report.Money != nil {
		money, err := bundleJSON(report.Money)
		if err != nil {
			return nil, err
		}
		ext["money"] = money
	}
	// The AUTHORITY block: the user's own copy only (it carries their words).
	if report.Authority != nil {
		authority, err := bundleJSON(report.Authority)
		if err != nil {
			return nil, err
		}
		ext["authority"], ext["authority_order"] = authority, dealAuthorityOrder
	}
	for key, v := range map[string]any{"deadlines": dealDeadlines(events, dealClock(), 2), "cancellations": dealCancellations(events), "lifecycle": buildDealLifecycle(events, dealClock())} {
		generic, err := bundleJSON(v)
		if err != nil {
			return nil, err
		}
		ext[key] = generic
	}
	return ext, nil
}

// witnessCoverage names, in the user's terms, what a receipt covering only
// part of a deal covers: the acts in the witnessed steps, and the acts in
// the steps still witness-pending ("the payment is witnessed; the
// cancellation is not yet"). Acts are named without payee or reference, so
// a shared copy carries it as it is. Nil unless the deal is witnessed in
// part.
func witnessCoverage(events []sealedEvent, cadence map[string]interface{}) map[string]interface{} {
	if cadence["state"] != "witnessed" || cadence["extent"] != "part" {
		return nil
	}
	k, err := jsonUint(cadence["steps_witnessed"])
	if err != nil || k == 0 || int(k) >= len(events) {
		return nil
	}
	currency := events[0].Event.Open.Terms.Currency
	acts := func(from []sealedEvent) string {
		var names []string
		for _, se := range from {
			if a := se.Event.Act; a != nil {
				names = append(names, dealShareAct(*a, currency))
			}
		}
		return strings.Join(names, "; ")
	}
	out := map[string]interface{}{"steps_witnessed": integer(k), "steps": integer(uint64(len(events)))}
	if covered := acts(events[:k]); covered != "" {
		out["witnessed_acts"] = covered
	}
	if pending := acts(events[k:]); pending != "" {
		out["pending_acts"] = pending
	}
	return out
}

// dealReportHTML renders one local, self-contained page: the bundle, the
// vendored verifier and the deal view. It needs no network to open or verify,
// and nothing is hosted: the agent attaches or hands over the file.
//
// The countersign rung was checked by capsulectl when the page was written
// (the page cannot check a signature against a directory); it rides as JSON
// beside the bundle, never inside it, so the bundle's digest is unchanged.
func dealReportHTML(b map[string]interface{}, countersign dealCountersignView) (string, error) {
	if err := pageGate(b); err != nil {
		return "", err
	}
	countersign = dealCountersignForPage(b, countersign)
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
	// json.Marshal escapes <, > and &, so the blob cannot close its element.
	cs, err := json.Marshal(countersign)
	if err != nil {
		return "", err
	}
	return page[:end] + `<script type="application/json" id="deal-countersign">` + string(cs) + "</script>\n  <script>" + dealViewJS + "</script>\n  " + page[end:], nil
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
#deal table.deal-merchant { border-collapse: collapse; width: 100%; margin: 6px 0; }
#deal table.deal-merchant th, #deal table.deal-merchant td { border: 1px solid var(--line); padding: 4px 8px; text-align: left; vertical-align: top; overflow-wrap: anywhere; }
#deal table.deal-merchant th { color: var(--muted); font-weight: 600; width: 40%; }
#deal .deal-scope { font-weight: 600; }
#deal .deal-ok { color: var(--ok); font-weight: 600; }
#deal code { color: var(--muted); font-size: 0.8rem; overflow-wrap: anywhere; }
#deal .deal-assurance { border: 1px solid var(--line); border-radius: 6px; padding: 8px 12px; margin: 8px 0 16px; }
#deal .deal-rung { font-weight: 600; margin: 0.2rem 0; }
#deal .deal-scope { font-weight: 600; margin: 0.2rem 0 0.6rem; }
#deal .deal-claims { border: none; padding: 0; }
#deal .deal-verify { background: transparent; border: 1px solid var(--line); border-radius: 4px; padding: 6px 8px; overflow-x: auto; font-size: 0.85rem; user-select: all; }
#app { max-width: 760px; margin: 0 auto; padding: 0 16px 16px; overflow-wrap: anywhere; }
`

// dealStepLine is one step in plain words, from the local store.
func dealStepLine(e dealEvent, showText bool) string {
	switch e.Kind {
	case "disclosure":
		// The user's own copy: the values the agent gave, as given.
		parts := make([]string, len(e.Disclosure.Fields))
		for i, f := range e.Disclosure.Fields {
			parts[i] = classWord(f.Class) + " " + f.Value
		}
		line := "Told " + e.Disclosure.recipientWord() + ": " + strings.Join(parts, "; ")
		if e.Disclosure.AuthorizedBy == "" {
			line = "⚠️ " + line + " (without your approval)"
		}
		return line
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

// bundleJSON turns a report value into the generic shape the bundle encoder
// takes. JSON numbers become integers: the bundle carries no floats, and
// every number here is a count or a step.
func bundleJSON(v any) (interface{}, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic interface{}
	if err = dec.Decode(&generic); err != nil {
		return nil, err
	}
	var walk func(interface{}) (interface{}, error)
	walk = func(x interface{}) (interface{}, error) {
		switch t := x.(type) {
		case json.Number:
			n, err := strconv.ParseUint(t.String(), 10, 64)
			if err != nil {
				return nil, errors.New("report value is not a count: " + t.String())
			}
			return integer(n), nil
		case map[string]interface{}:
			for k, c := range t {
				if t[k], err = walk(c); err != nil {
					return nil, err
				}
			}
		case []interface{}:
			for i, c := range t {
				if t[i], err = walk(c); err != nil {
					return nil, err
				}
			}
		}
		return x, nil
	}
	return walk(generic)
}

func stringList(values []string) []interface{} {
	out := make([]interface{}, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// toldItems is the report's "What your agent told whom", as the extension
// carries it. withValues is the user's own copy only; a shared copy names the
// class of each field and never its value.
func toldItems(told []dealToldItem, withValues bool) []interface{} {
	out := make([]interface{}, len(told))
	for i, t := range told {
		fields := make([]interface{}, len(t.Fields))
		classes := make([]string, len(t.Fields))
		for j, f := range t.Fields {
			m := map[string]interface{}{"class": f.Class, "label": classWord(f.Class)}
			if withValues {
				m["value"] = f.Value
			}
			fields[j] = m
			classes[j] = classWord(f.Class)
		}
		steps := make([]interface{}, len(t.Steps))
		for j, s := range t.Steps {
			steps[j] = s
		}
		text := "Told " + t.To + ": " + strings.Join(classes, ", ")
		if !withValues {
			who := "the other party"
			if t.ToKind == "other" {
				who = "someone other than the other party"
			}
			text = "Told " + who + ": " + strings.Join(classes, ", ")
		}
		authority := "covered by your approval"
		if t.Authority == "none" {
			authority = "without your approval (" + t.Reason + ")"
		}
		out[i] = map[string]interface{}{
			"at": t.At, "text": text, "fields": fields, "authority": t.Authority, "authority_text": authority, "steps": steps,
		}
	}
	return out
}
