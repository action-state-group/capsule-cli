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
	out := map[string]interface{}{"asked": false, "representations": []interface{}{}, "task_intent": "none", "picks": []interface{}{}}
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
		state, err := taskIntentOpenings(report, input(step))
		if err != nil {
			return nil, err
		}
		out["task_intent"] = state
		picks, err := pickOpeningsChecked(report, input)
		if err != nil {
			return nil, err
		}
		out["picks"] = picks
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

// taskIntentOpenings checks the user's own copy's task intent against the
// baseline that cites it: the record the copy carries must be the one the
// baseline's cites ref names (its JCS digest), and the request's and each
// constraint's opening must recompute to the commitments in it. It returns
// "none" when the baseline cites no task intent, "unresolved" when it cites
// one the copy does not carry, and "checked" when every opening recomputes.
// An opening that does not, or a record that is not the cited one, refuses
// the page.
func taskIntentOpenings(report, baseline map[string]interface{}) (string, error) {
	cited := citedDigest(baseline)
	section, _ := report["task_intent"].(map[string]interface{})
	switch {
	case cited == "" && section == nil:
		return "none", nil
	case cited == "":
		return "", errors.New("refusing to write the page: the copy carries a task intent its deal does not cite")
	case section == nil || section["resolved"] != true:
		return "unresolved", nil
	}
	record, _ := section["record"].(map[string]interface{})
	rb, _ := record["x-deal-v0"].(map[string]interface{})
	if record == nil || rb["record_type"] != "task_intent" || jcsDigest(record) != cited {
		return "", errors.New("refusing to write the page: the task intent the copy carries is not the one the deal cites")
	}
	body, _ := record["body"].(map[string]interface{})
	opening, _ := section["verbatim_opening"].(map[string]interface{})
	if !openingRecomputes(opening, body["verbatim_commitment"]) {
		return "", errors.New("refusing to write the page: the words you asked when the task started do not match the commitment sealed then")
	}
	committed := map[string]interface{}{}
	fields, _ := body["fields"].([]interface{})
	for _, f := range fields {
		field, _ := f.(map[string]interface{})
		class, _ := field["class"].(string)
		committed[class] = field["value_commitment"]
	}
	given, _ := section["fields"].([]interface{})
	for _, f := range given {
		field, _ := f.(map[string]interface{})
		class, _ := field["class"].(string)
		if !openingRecomputes(map[string]interface{}{"nonce": field["nonce"], "text": field["value"]}, committed[class]) {
			return "", errors.New("refusing to write the page: a limit you stated when the task started does not match the commitment sealed then (" + class + ")")
		}
	}
	return "checked", nil
}

// pickOpeningsChecked checks each pick's openings (the user's words, the
// options shown, the agent's recommendation) against the commitments its
// sealed intent record carries. A pick is checked when every opening it
// carries recomputes; one that does not refuses the page.
func pickOpeningsChecked(report map[string]interface{}, input func(string) map[string]interface{}) ([]interface{}, error) {
	picks, _ := report["pick_openings"].([]interface{})
	out := make([]interface{}, len(picks))
	for i, p := range picks {
		pick, _ := p.(map[string]interface{})
		step, _ := pick["step"].(string)
		body, _ := input(step)["body"].(map[string]interface{})
		out[i] = false
		if body == nil {
			continue
		}
		for name, commitment := range map[string]string{"verbatim": "verbatim_commitment", "shown": "shown_commitment", "recommended": "recommended_commitment"} {
			opening, present := pick[name].(map[string]interface{})
			if !present {
				continue
			}
			if !openingRecomputes(opening, body[commitment]) {
				return nil, errors.New("refusing to write the page: a pick does not match the commitment sealed with it (step " + step + ")")
			}
		}
		out[i] = true
	}
	return out, nil
}
