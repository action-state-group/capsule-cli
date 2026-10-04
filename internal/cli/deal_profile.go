package cli

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// This file is the only place that knows the sealed wire shape of a deal step:
// the x-deal-v0 record (skills/deal/profile/PROFILE.md). Everything else works
// with dealEvent, which holds raw values and lives only in the local store.
// A record is derived deterministically from its local event, the steps before
// it, and the deal key, so the local store can be re-checked against every
// sealed record on every read.

const (
	dealProfile   = "x-deal-v0"
	dealPackID    = "capsule/marketplace-rentals-safety/0.1.0"
	dealFPAlg     = "hmac-sha256-deal-key"
	dealRecordRef = "deal-record"
)

// The profile schema ships in skills/deal/profile/; this is a byte-identical
// copy (a test keeps them equal) so every record is validated before sealing.
//
//go:embed assets/x-deal-v0.schema.json
var dealProfileSchema []byte

var (
	dealSchemaOnce sync.Once
	dealSchema     *jsonschema.Schema
	dealSchemaErr  error
)

func compiledDealSchema() (*jsonschema.Schema, error) {
	dealSchemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(dealProfileSchema))
		if err != nil {
			dealSchemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		const id = "x-deal-v0.schema.json"
		if err = c.AddResource(id, doc); err != nil {
			dealSchemaErr = err
			return
		}
		dealSchema, dealSchemaErr = c.Compile(id)
	})
	return dealSchema, dealSchemaErr
}

// The texts a step commits to, by nonce name. Each gets its own nonce.
func dealTexts(ev dealEvent) map[string]string {
	t := map[string]string{}
	switch {
	case ev.Open != nil:
		t["verbatim"] = ev.Open.Intent.Verbatim
	case ev.Intent != nil:
		t["verbatim"] = ev.Intent.Verbatim
	case ev.Message != nil:
		t["content"] = ev.Message.Text
	case ev.Evidence != nil:
		if ev.Evidence.Detail != "" {
			t["detail"] = ev.Evidence.Detail
		}
		if ev.Evidence.Email != nil && ev.Evidence.Email.Parsed.OrderID != "" {
			t["order_id"] = ev.Evidence.Email.Parsed.OrderID
		}
	case ev.Snapshot != nil && ev.Snapshot.Description != "":
		t["description"] = ev.Snapshot.Description
	case ev.Check != nil && ev.Check.Card != "":
		t["card"] = ev.Check.Card
	case ev.Approval != nil && ev.Approval.Approver == "user":
		t["said"] = ev.Approval.Said
	case ev.Act != nil:
		if ev.Act.Reference != "" {
			t["reference"] = ev.Act.Reference
		}
		if ev.Act.Description != "" {
			t["description"] = ev.Act.Description
		}
	case ev.Outcome != nil && ev.Outcome.Note != "":
		t["note"] = ev.Outcome.Note
	}
	return t
}

// dealLocalValues are the raw values the pre-seal scan must never find in a
// record: every identifier seen in the deal and every committed text long
// enough not to collide with a token.
func dealLocalValues(events []sealedEvent, ev dealEvent) []string {
	var out []string
	add := func(e dealEvent) {
		var whos []dealWho
		switch {
		case e.Open != nil:
			whos = append(whos, e.Open.Who)
		case e.Message != nil && e.Message.Who != nil:
			whos = append(whos, *e.Message.Who)
		case e.Evidence != nil && e.Evidence.Who != nil:
			whos = append(whos, *e.Evidence.Who)
		case e.Change != nil && e.Change.Who != nil:
			whos = append(whos, *e.Change.Who)
		case e.Snapshot != nil && e.Snapshot.Who != nil:
			whos = append(whos, *e.Snapshot.Who)
		case e.Act != nil && e.Act.Payee != "":
			whos = append(whos, dealWho{Payee: e.Act.Payee})
		}
		for _, w := range whos {
			for _, f := range whoFields(w) {
				if f.value != "" {
					out = append(out, f.value)
				}
			}
		}
		for _, text := range dealTexts(e) {
			if len(text) >= 24 {
				out = append(out, text)
			}
		}
	}
	for _, se := range events {
		add(se.Event)
	}
	add(ev)
	return out
}

func digestRef(d string) map[string]interface{} {
	return map[string]interface{}{"type": dealRecordRef, "digest_alg": "SHA-256", "digest": d}
}

func relRef(rel, d string) map[string]interface{} {
	r := digestRef(d)
	r["rel"] = rel
	return r
}

// counterpartyIDs fingerprints every identifier that can be normalized. One
// that cannot is not recorded; it stays local.
func counterpartyIDs(key []byte, w dealWho) map[string]interface{} {
	ids := map[string]interface{}{}
	for _, f := range whoFields(w) {
		if f.value == "" {
			continue
		}
		if fp, err := fingerprintID(key, f.key, f.value); err == nil {
			ids[f.key] = fp
		}
	}
	return ids
}

func termsBody(t dealTerms) map[string]interface{} {
	m := map[string]interface{}{}
	set := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	set("item", t.Item)
	set("currency", t.Currency)
	set("when", t.When)
	set("place", t.Place)
	if t.Quantity != 0 {
		m["quantity"] = t.Quantity
	}
	if t.PriceMinor != nil {
		m["price_minor"] = *t.PriceMinor
	}
	if t.DepositMinor != nil {
		m["deposit_minor"] = *t.DepositMinor
	}
	if len(t.Conditions) > 0 {
		m["conditions"] = t.Conditions
	}
	return m
}

func recourseBody(r dealRecourse) map[string]interface{} {
	m := map[string]interface{}{}
	if r.Rail != "" {
		m["rail"] = normRail(r.Rail)
	}
	if r.Refundable != nil {
		m["refundable"] = *r.Refundable
	}
	return m
}

var tokenPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// asToken makes a rule id, note or source a profile token.
func asToken(s, fallback string) string {
	t := strings.Trim(nonToken.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "_"), "_")
	if tokenPattern.MatchString(t) {
		return t
	}
	return fallback
}

var nonToken = regexp.MustCompile(`[^a-z0-9]+`)

func differencesBody(ds []dealDifference) []interface{} {
	out := make([]interface{}, 0, len(ds))
	for _, d := range ds {
		q := d.Question
		if q == "remote" {
			q = "safety"
		}
		m := map[string]interface{}{"question": q, "rule": asToken(d.Rule, "remote_pause")}
		if d.Field != "" {
			m["field"] = d.Field
		}
		out = append(out, m)
	}
	return out
}

func actionBody(a dealAct, currency string, commit func(string) (string, error)) (map[string]interface{}, error) {
	m := map[string]interface{}{"action": a.Action}
	if a.AmountMinor != nil {
		m["amount_minor"] = *a.AmountMinor
		cur := a.Currency
		if cur == "" {
			cur = currency
		}
		if cur != "" {
			m["currency"] = cur
		}
	}
	if a.Rail != "" {
		m["rail"] = normRail(a.Rail)
	}
	for name, field := range map[string]string{"reference": "reference_commitment", "description": "description_commitment"} {
		if (name == "reference" && a.Reference == "") || (name == "description" && a.Description == "") {
			continue
		}
		c, err := commit(name)
		if err != nil {
			return nil, err
		}
		m[field] = c
	}
	return m, nil
}

func intentBody(i dealIntent, commit func(string) (string, error)) (map[string]interface{}, error) {
	c, err := commit("verbatim")
	if err != nil {
		return nil, err
	}
	m := map[string]interface{}{"verbatim_commitment": c}
	if asked := termsBody(i.Asked); len(asked) > 0 {
		m["asked"] = asked
	}
	if i.MaxTotalMinor != nil {
		m["max_total_minor"] = *i.MaxTotalMinor
	}
	if i.Allowed != nil { // present and empty: nothing is allowed yet
		m["allowed"] = i.Allowed
	}
	return m, nil
}

// merchantEmailBody is what a record says about a sealed merchant email: the
// digest of the exact bytes, the digest of the key records it was checked
// against and where they came from, the DKIM result, and the parsed amounts.
// The message itself, its addresses and its order id stay on the device.
func merchantEmailBody(m *merchantEmail, first dealWho, commit func(string) (string, error)) (map[string]interface{}, error) {
	keys, err := keyRecordsDigest(m.Keys)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{
		"message_digest": emailDigest(m.Raw), "key_records_digest": keys, "key_source": m.KeySource,
		"dkim": m.DKIM.Result, "merchant_signed": m.DKIM.Merchant, "dmarc_policy": m.DKIM.Policy,
	}
	if d, err := m.DMARC.digest(); err != nil {
		return nil, err
	} else if d != "" {
		out["dmarc_record_digest"] = d
		out["dmarc_source"] = m.DMARC.Source
	}
	if same, known := signerMatchesBaseline(m.DKIM, first); known {
		out["signer_matches_baseline"] = same
	}
	parsed := map[string]interface{}{"method": "heuristic"}
	p := m.Parsed
	if p.TotalMinor != nil {
		parsed["total_minor"] = *p.TotalMinor
	}
	if p.Currency != "" {
		parsed["currency"] = p.Currency
	}
	if p.CancelBy != "" {
		parsed["cancel_by"] = p.CancelBy
	}
	if p.SentAt != "" {
		parsed["sent_at"] = p.SentAt
	}
	if len(p.Items) > 0 {
		parsed["item_count"] = int64(len(p.Items))
	}
	if p.OrderID != "" {
		if parsed["order_id_commitment"], err = commit("order_id"); err != nil {
			return nil, err
		}
	}
	out["parsed"] = parsed
	return out, nil
}

// signerMatchesBaseline compares the domain behind a passing signature with
// the counterparty's domain (or email domain) from first contact. known is
// false when first contact named neither, or nothing passed.
func signerMatchesBaseline(v merchantEmailVerdict, first dealWho) (same, known bool) {
	signer := v.signer()
	if signer == "" {
		return false, false
	}
	for _, raw := range []string{first.Domain, first.Email} {
		if raw == "" {
			continue
		}
		d, err := normDomain(raw)
		if err != nil {
			continue
		}
		known = true
		if d == signer || strings.HasSuffix(d, "."+signer) || strings.HasSuffix(signer, "."+d) {
			return true, true
		}
	}
	return false, known
}

// buildDealRecord derives the x-deal-v0 record for ev, which follows events.
func buildDealRecord(ev dealEvent, events []sealedEvent, key []byte) (map[string]interface{}, error) {
	digestOf := func(capsuleID string) string {
		for _, se := range events {
			if se.CapsuleID == capsuleID {
				return se.Digest
			}
		}
		return ""
	}
	commit := func(name string) (string, error) {
		nonce := ev.Nonces[name]
		if nonce == "" {
			return "", inputError("missing commitment nonce for " + name)
		}
		return commitText(nonce, dealTexts(ev)[name])
	}
	block := map[string]interface{}{
		"profile": dealProfile, "canonicalization": "jcs", "deal_id": ev.DealID, "seq": ev.N, "at": ev.At,
	}
	if ev.N > 1 {
		block["prev"] = digestRef(events[len(events)-1].Digest)
		block["baseline_ref"] = digestRef(events[0].Digest)
	}
	var currency string
	if len(events) > 0 {
		currency = events[0].Event.Open.Terms.Currency
	}
	setIDs := func(ids map[string]interface{}) {
		if len(ids) > 0 {
			block["counterparty"] = map[string]interface{}{"fp_alg": dealFPAlg, "ids": ids}
		}
	}
	body := map[string]interface{}{}
	var rtype string
	var err error
	switch ev.Kind {
	case "open":
		o := ev.Open
		rtype = "baseline"
		block["channel"] = o.Channel
		first := o.Who
		if first.Payee == "" {
			first.Payee = first.Name
		}
		ids := counterpartyIDs(key, first)
		if len(ids) == 0 {
			return nil, inputError("who has no identifier that can be recorded (check the phone, email, domain or name)")
		}
		setIDs(ids)
		body["deal_type"] = o.Type
		if o.Demo {
			body["demo"] = true
		}
		if body["intent"], err = intentBody(o.Intent, commit); err != nil {
			return nil, err
		}
		body["terms"] = termsBody(o.Terms)
		body["recourse"] = recourseBody(o.Recourse)
		if len(o.Claims) > 0 {
			claims := make([]interface{}, len(o.Claims))
			for i, c := range o.Claims {
				claims[i] = map[string]interface{}{"text": c.Text, "source": c.Source}
			}
			body["claims"] = claims
		}
		if o.Who.DomainAgeDays != nil {
			body["counterparty_facts"] = map[string]interface{}{"domain_age_days": *o.Who.DomainAgeDays}
		}
	case "intent":
		rtype = "intent"
		if body, err = intentBody(*ev.Intent, commit); err != nil {
			return nil, err
		}
	case "message":
		rtype = "message"
		block["channel"] = ev.Message.Channel
		if ev.Message.Who != nil {
			setIDs(counterpartyIDs(key, *ev.Message.Who))
		}
		body["from"] = ev.Message.From
		if body["content_commitment"], err = commit("content"); err != nil {
			return nil, err
		}
	case "claim":
		rtype = "claim"
		body = map[string]interface{}{"text": ev.Claim.Text, "source": ev.Claim.Source}
	case "evidence":
		rtype = "evidence"
		about := events[0].Digest
		for _, se := range events {
			if se.Event.Kind == "claim" && strings.EqualFold(se.Event.Claim.Text, ev.Evidence.About) {
				about = se.Digest
			}
		}
		block["refs"] = []interface{}{relRef("about", about)}
		body["source"] = ev.Evidence.Source
		body["verified"] = ev.Evidence.Verified
		if ev.Evidence.Detail != "" {
			if body["detail_commitment"], err = commit("detail"); err != nil {
				return nil, err
			}
		}
		if m := ev.Evidence.Email; m != nil {
			if body["merchant_email"], err = merchantEmailBody(m, events[0].Event.Open.Who, commit); err != nil {
				return nil, err
			}
		}
		if ev.Evidence.Who != nil {
			setIDs(counterpartyIDs(key, *ev.Evidence.Who))
			if ev.Evidence.Who.DomainAgeDays != nil {
				body["counterparty_facts"] = map[string]interface{}{"domain_age_days": *ev.Evidence.Who.DomainAgeDays}
			}
		}
	case "change":
		rtype = "detail_change"
		ch := ev.Change
		var changed []interface{}
		if ch.Who != nil {
			ids := counterpartyIDs(key, *ch.Who)
			setIDs(ids)
			for _, f := range whoFields(*ch.Who) {
				if _, ok := ids[f.key]; ok {
					changed = append(changed, f.key)
				}
			}
		}
		if ch.Terms != nil {
			t := termsBody(*ch.Terms)
			for _, k := range []string{"item", "quantity", "price_minor", "deposit_minor", "currency", "when", "place", "conditions"} {
				if _, ok := t[k]; ok {
					changed = append(changed, strings.TrimSuffix(k, "_minor"))
				}
			}
			if len(t) > 0 {
				body["terms"] = t
			}
		}
		if ch.Recourse != nil {
			r := recourseBody(*ch.Recourse)
			for _, k := range []string{"rail", "refundable"} {
				if _, ok := r[k]; ok {
					changed = append(changed, k)
				}
			}
			if len(r) > 0 {
				body["recourse"] = r
			}
		}
		body["source"] = ch.Source
		body["changed"] = changed
	case "snapshot":
		rtype = "check"
		sn := ev.Snapshot
		if sn.Who != nil {
			setIDs(counterpartyIDs(key, *sn.Who))
		}
		body["action"] = sn.Action
		body["pack_id"] = dealPackID
		if sn.AmountMinor != nil {
			body["amount_minor"] = *sn.AmountMinor
			cur := currency
			if sn.Terms != nil && sn.Terms.Currency != "" {
				cur = sn.Terms.Currency
			}
			if cur != "" {
				body["currency"] = cur
			}
		}
		if sn.SeenItem != nil {
			body["seen_item"] = *sn.SeenItem
		}
		if sn.Terms != nil {
			if t := termsBody(*sn.Terms); len(t) > 0 {
				body["terms"] = t
			}
		}
		if sn.Recourse != nil {
			if r := recourseBody(*sn.Recourse); len(r) > 0 {
				body["recourse"] = r
			}
		}
		if sn.Description != "" {
			if body["description_commitment"], err = commit("description"); err != nil {
				return nil, err
			}
		}
	case "check":
		rtype = "verdict"
		ck := ev.Check
		block["refs"] = []interface{}{relRef("checks", digestOf(ck.Snapshot))}
		body["result"] = ck.Verdict
		body["pack_id"] = dealPackID
		body["differences"] = differencesBody(ck.Differences)
		options := make([]interface{}, len(ck.Options))
		for i, o := range ck.Options {
			options[i] = o.ID
		}
		body["options"] = options
		if len(ck.Unverified) > 0 {
			body["unverified"] = ck.Unverified
		}
		if len(ck.Notes) > 0 {
			notes := make([]interface{}, len(ck.Notes))
			for i, n := range ck.Notes {
				notes[i] = asToken(n, "note")
			}
			body["notes"] = notes
		}
		if ck.Card != "" {
			if body["card_commitment"], err = commit("card"); err != nil {
				return nil, err
			}
		}
		judge := map[string]interface{}{"kind": "rules"}
		switch ck.Remote.Status {
		case "used":
			judge = map[string]interface{}{"kind": "remote", "status": "used", "response_digest": ck.Remote.ResponseSHA256}
		case "unavailable":
			judge["status"] = "remote_unavailable"
		}
		body["judge"] = judge
	case "approval":
		rtype = "approval"
		a := ev.Approval
		block["refs"] = []interface{}{relRef("approves", digestOf(a.Check))}
		body = map[string]interface{}{"choice": a.Choice, "proceed": a.Proceed, "approver": a.Approver}
		if a.Approver == "user" {
			if body["said_commitment"], err = commit("said"); err != nil {
				return nil, err
			}
		}
	case "act":
		act, err := actionBody(*ev.Act, currency, commit)
		if err != nil {
			return nil, err
		}
		if ev.Act.Unchecked {
			// Not authorized: sealed as an outcome, never as an action.
			rtype = "outcome"
			body = map[string]interface{}{
				"status": "unchecked_action", "outcome": "mismatch", "unchecked": act,
				"differences": []interface{}{map[string]interface{}{"question": "asked", "rule": ev.Act.Rule}},
			}
		} else {
			rtype = "action"
			block["refs"] = []interface{}{relRef("authorized_by", digestOf(ev.Act.AuthorizedBy))}
			if ev.Act.Payee != "" {
				setIDs(counterpartyIDs(key, dealWho{Payee: ev.Act.Payee}))
			}
			body = act
		}
	case "outcome":
		rtype = "outcome"
		oc := ev.Outcome
		body = map[string]interface{}{"status": oc.Status, "outcome": oc.Outcome, "differences": differencesBody(oc.Differences)}
		if oc.Delivered != nil {
			if t := termsBody(*oc.Delivered); len(t) > 0 {
				body["delivered"] = t
			}
		}
		if oc.Note != "" {
			if body["note_commitment"], err = commit("note"); err != nil {
				return nil, err
			}
		}
	case "close":
		rtype = "close"
		cl := ev.Close
		for i := len(events) - 1; i >= 0; i-- {
			e := events[i].Event
			if e.Kind == "outcome" || (e.Kind == "act" && e.Act.Unchecked) {
				block["refs"] = []interface{}{relRef("outcome", events[i].Digest)}
				break
			}
		}
		body = map[string]interface{}{"outcome": cl.Outcome, "unchecked_actions": cl.UncheckedActions}
		if len(cl.Differences) > 0 {
			body["differences"] = differencesBody(cl.Differences)
		}
	default:
		return nil, inputError("unknown step kind " + ev.Kind)
	}
	block["record_type"] = rtype
	return map[string]interface{}{"x-deal-v0": block, "body": body}, nil
}

// encodeDealRecord returns the sealed payload (the record's JCS bytes) and its
// record digest, after the schema check and the privacy and wording scans. A
// record that fails any of them is not sealed.
func encodeDealRecord(ev dealEvent, events []sealedEvent, key []byte) ([]byte, string, error) {
	record, err := buildDealRecord(ev, events, key)
	if err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, "", err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	docMap, _ := doc.(map[string]interface{})
	if err = scanRecord(docMap, dealLocalValues(events, ev)); err != nil {
		return nil, "", err
	}
	schema, err := compiledDealSchema()
	if err != nil {
		return nil, "", err
	}
	if err = schema.Validate(doc); err != nil {
		return nil, "", inputError("refusing to seal: the step does not fit the x-deal-v0 profile: " + firstSchemaError(err))
	}
	payload, err := canonical.JCS(doc)
	if err != nil {
		return nil, "", err
	}
	digest, err := canonical.JSONDigest(doc)
	return payload, digest, err
}

func firstSchemaError(err error) string {
	var v *jsonschema.ValidationError
	if errors.As(err, &v) {
		leaves := leaves(v)
		if len(leaves) > 0 {
			l := leaves[0]
			return "/" + strings.Join(l.InstanceLocation, "/") + ": " + describeLeaf(l.ErrorKind)
		}
	}
	return "invalid record"
}

// Inputs are normalized to the profile's tokens before anything is sealed, so
// the local step and its record agree.

var seededChannels = map[string]bool{
	"marketplace": true, "web": true, "sms": true, "phone_call": true, "email": true, "app_chat": true,
	"whatsapp": true, "telegram": true, "signal": true, "in_person": true, "other": true,
}

var channelAliases = map[string]string{
	"messenger": "app_chat", "facebook_messenger": "app_chat", "chat": "app_chat", "dm": "app_chat", "instagram": "app_chat",
	"facebook_marketplace": "marketplace", "craigslist": "marketplace", "listing": "marketplace",
	"text": "sms", "text_message": "sms", "imessage": "sms", "phone": "phone_call", "call": "phone_call",
	"gmail": "email", "mail": "email", "website": "web", "site": "web", "in_person_meeting": "in_person",
}

var namespacedToken = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+\.[a-z][a-z0-9_]{0,63}$`)

// channelToken maps a free-text channel to a profile channel token.
func channelToken(s string) string {
	low := strings.ToLower(strings.TrimSpace(s))
	if low == "" {
		return ""
	}
	if namespacedToken.MatchString(low) {
		return low
	}
	t := strings.Trim(nonToken.ReplaceAllString(low, "_"), "_")
	if seededChannels[t] {
		return t
	}
	if a, ok := channelAliases[t]; ok {
		return a
	}
	return "other"
}

func sourceToken(s string) (string, error) {
	low := strings.ToLower(strings.TrimSpace(s))
	if namespacedToken.MatchString(low) {
		return low, nil
	}
	if t := asToken(low, ""); t != "" {
		return t, nil
	}
	return "", inputError("source must be a short word, like seller_message or listing_photo")
}

func normalizeTerms(t *dealTerms) error {
	if t == nil {
		return nil
	}
	t.Currency = strings.ToUpper(strings.TrimSpace(t.Currency))
	for k := range t.Conditions {
		if !tokenPattern.MatchString(k) {
			return inputError("condition names must be lowercase words joined by _: " + k)
		}
	}
	return nil
}

func normalizeOpen(o *dealOpen) error {
	o.Channel = channelToken(o.Channel)
	if o.Channel == "" {
		o.Channel = "other"
	}
	for i := range o.Claims {
		t, err := sourceToken(o.Claims[i].Source)
		if err != nil {
			return err
		}
		o.Claims[i].Source = t
	}
	o.Recourse.Rail = normRail(o.Recourse.Rail)
	if err := normalizeTerms(&o.Terms); err != nil {
		return err
	}
	return normalizeTerms(&o.Intent.Asked)
}

func normalizeNote(ev *dealEvent) error {
	var err error
	switch {
	case ev.Message != nil:
		ev.Message.Channel = channelToken(ev.Message.Channel)
	case ev.Claim != nil:
		ev.Claim.Source, err = sourceToken(ev.Claim.Source)
	case ev.Evidence != nil:
		ev.Evidence.Source, err = sourceToken(ev.Evidence.Source)
	case ev.Change != nil:
		if ev.Change.Who != nil && ev.Change.Who.DomainAgeDays != nil {
			return inputError("record the website's age as evidence, not as a change")
		}
		if ev.Change.Source, err = sourceToken(ev.Change.Source); err != nil {
			return err
		}
		if ev.Change.Recourse != nil {
			ev.Change.Recourse.Rail = normRail(ev.Change.Recourse.Rail)
		}
		err = normalizeTerms(ev.Change.Terms)
	case ev.Intent != nil:
		if strings.TrimSpace(ev.Intent.Verbatim) == "" {
			return inputError("intent.verbatim (the user's own words) is required")
		}
		err = normalizeTerms(&ev.Intent.Asked)
	}
	return err
}
