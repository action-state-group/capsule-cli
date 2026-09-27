package cli

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
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

// maxInlineFragment is the largest fragment carried inline in a link. Above
// it, the link carries a pointer to the bundle instead: a multi-megabyte URL
// does not open reliably.
var maxInlineFragment = 1 << 20

// dealWithheldKinds are the steps whose content a report withholds (shown as
// their capsule_id only): message text stays on the machine.
var dealWithheldKinds = map[string]bool{"message": true}

// dealReportBundle builds the deal's Evidence Bundle: every step of the deal's
// own log with membership proofs, the chain closed from the last step back to
// the opening one, and every step's content disclosed except messages.
func (s *dealSession) dealReportBundle(ctx context.Context, events []sealedEvent) (map[string]interface{}, error) {
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
	overlay, _ := b["disclosures"].(map[string]interface{})
	steps := make([]interface{}, len(events))
	for i, se := range events {
		if dealWithheldKinds[se.Event.Kind] {
			delete(overlay, se.CapsuleID)
		}
		steps[i] = map[string]interface{}{"n": integer(uint64(se.Event.N)), "kind": se.Event.Kind, "capsule_id": se.CapsuleID}
	}
	// The portable checkpoint signature lets a verifier authenticate the
	// checkpoint in the page instead of labelling it producer-asserted.
	cp, _ := b["checkpoint"].(map[string]interface{})
	statement, err := base64.StdEncoding.DecodeString(fmt.Sprint(cp["statement"]))
	if err != nil {
		return nil, err
	}
	cp["cose"] = base64.RawURLEncoding.EncodeToString(statement)
	b["extensions"] = map[string]interface{}{
		"x-deal-v0": map[string]interface{}{"deal_id": events[0].Event.DealID, "steps": steps},
	}
	if err = verifyProducedBundle(b, true); err != nil {
		return nil, err
	}
	return b, nil
}

// dealReportHTML renders one self-contained page: the bundle, the vendored
// verifier and the deal view. It needs no network to open or to verify.
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
	if page, err = replaceOnce(page, "<title>Evidence Graph</title>", "<title>Deal receipt</title>\n    <style>"+dealViewCSS+"</style>"); err != nil {
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

// dealReportFragment encodes the bundle as a link fragment with the Evidence
// Bundle fragment codec. A bundle over maxInlineFragment is replaced by a
// pointer (its digest, root and where it can be fetched); that pointer shape
// is a draft and is labelled so in the output.
func dealReportFragment(b map[string]interface{}, locations []string) (kind, fragment, digest string, err error) {
	if digest, err = aacbundle.BundleDigest(b); err != nil {
		return "", "", "", err
	}
	if fragment, err = aacbundle.EncodeFragment(b); err != nil {
		return "", "", "", err
	}
	if len(fragment) <= maxInlineFragment {
		return "inline", fragment, digest, nil
	}
	where := make([]interface{}, len(locations))
	for i, l := range locations {
		where[i] = l
	}
	fragment, err = aacbundle.EncodeFragment(map[string]interface{}{
		"bundle_ref": map[string]interface{}{"digest": digest, "root": b["root"], "locations": where},
	})
	return "pointer (draft)", fragment, digest, err
}

const dealViewCSS = `
:root { --fg: #1b1b1f; --muted: #5d5d66; --bg: #ffffff; --line: #d9d9e0; --warn: #9a3b00; --ok: #1d6b35; }
@media (prefers-color-scheme: dark) { :root { --fg: #ececf1; --muted: #a3a3ad; --bg: #16161a; --line: #34343c; --warn: #ffb07a; --ok: #7fd49a; } }
body { background: var(--bg); color: var(--fg); }
#deal { max-width: 760px; margin: 0 auto; padding: 16px; line-height: 1.45; }
#deal h1 { font-size: 1.5rem; margin: 0.2rem 0; }
#deal h2 { font-size: 1.1rem; margin-top: 1.5rem; }
#deal .deal-ask { font-size: 1.1rem; }
#deal .deal-note { color: var(--muted); font-size: 0.9rem; }
#deal .deal-demo { display: inline-block; border: 1px solid var(--warn); color: var(--warn); padding: 0 6px; border-radius: 4px; font-size: 0.8rem; }
#deal .deal-bad { color: var(--warn); font-weight: 600; }
#deal details { border: 1px solid var(--line); border-radius: 6px; padding: 8px 12px; margin: 8px 0; }
#deal summary { cursor: pointer; font-weight: 600; }
#deal table { border-collapse: collapse; width: 100%; margin-top: 8px; }
#deal th { text-align: left; vertical-align: top; color: var(--muted); font-weight: 500; padding: 4px 12px 4px 0; width: 9rem; }
#deal td { padding: 4px 0; overflow-wrap: anywhere; }
#deal code { color: var(--muted); font-size: 0.8rem; overflow-wrap: anywhere; }
#app { max-width: 760px; margin: 0 auto; padding: 0 16px 16px; overflow-wrap: anywhere; }
`
