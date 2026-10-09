package cli

import (
	"encoding/json"
	"errors"
)

// dealPageOpenings checks, before a deal page is written, the words the page
// shows against the commitments the deal sealed to them: the user's words
// (the report's asked_opening, against the baseline's
// intent.verbatim_commitment) and each statement the agent made (the
// report's representations, against the claim's text_commitment), each
// SHA-256 over JCS({nonce, text}). The page's own script does not recompute
// them: it shows what this check found, worded as checked when the page was
// built.
//
// It reads the bundle the page embeds, as the page does: the report is the
// sealed report the x-deal-v0 extension names (or, in a bundle written before
// reports were sealed, the extension itself), and each payload is the
// record's disclosed agent_input, which pageGate has already checked against
// its committed digest. An opening that is present but does not recompute
// refuses the page; the error names what, never the words. The result says,
// for the asked words and for each representation in order, whether it was
// checked; one that is absent, or whose sealed claim is not the agent's, is
// not checked, and the page says so.
func dealPageOpenings(b map[string]interface{}) (map[string]interface{}, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	bundle, err := decodeBundleJSON(raw)
	if err != nil {
		return nil, err
	}
	disclosures, _ := bundle["disclosures"].(map[string]interface{})
	input := func(id string) map[string]interface{} {
		d, _ := disclosures[id].(map[string]interface{})
		in, _ := d["agent_input"].(map[string]interface{})
		return in
	}
	exts, _ := bundle["extensions"].(map[string]interface{})
	report, _ := exts[dealProfile].(map[string]interface{})
	if id, ok := report["sealed_report"].(string); ok {
		sealed, _ := input(id)["report"].(map[string]interface{})
		report = sealed
	}
	out := map[string]interface{}{"asked": false, "representations": []interface{}{}}
	if report == nil {
		return out, nil
	}
	opens := func(opening map[string]interface{}, commitment interface{}) (bool, bool) {
		nonce, ok1 := opening["nonce"].(string)
		text, ok2 := opening["text"].(string)
		want, ok3 := commitment.(string)
		if !ok1 || !ok2 || !ok3 {
			return false, false
		}
		got, err := commitText(nonce, text)
		return true, err == nil && got == want
	}

	audience, _ := report["audience"].(string)
	if audience != dealAudienceCounterparty && audience != dealAudienceAdjudicator {
		opening, _ := report["asked_opening"].(map[string]interface{})
		step, _ := report["asked_step"].(string)
		body, _ := input(step)["body"].(map[string]interface{})
		intent, _ := body["intent"].(map[string]interface{})
		present, ok := opens(opening, intent["verbatim_commitment"])
		if present && !ok {
			return nil, errors.New("refusing to write the page: the words you asked do not match the commitment sealed when the deal opened (step " + step + ")")
		}
		out["asked"] = ok
	}

	said, _ := report["representations"].([]interface{})
	checked := make([]interface{}, len(said))
	for i, r := range said {
		checked[i] = false
		rep, _ := r.(map[string]interface{})
		step, _ := rep["step"].(string)
		body, _ := input(step)["body"].(map[string]interface{})
		claim := body
		if index, ok := rep["index"].(json.Number); ok {
			n, err := index.Int64()
			claims, _ := body["claims"].([]interface{})
			if err != nil || n < 0 || n >= int64(len(claims)) {
				continue
			}
			claim, _ = claims[n].(map[string]interface{})
		}
		if claim == nil || claim["source_kind"] != "agent" {
			continue
		}
		present, ok := opens(rep, claim["text_commitment"])
		if present && !ok {
			return nil, errors.New("refusing to write the page: a statement the agent made does not match the commitment sealed when it was made (step " + step + ")")
		}
		checked[i] = ok
	}
	out["representations"] = checked
	return out, nil
}
