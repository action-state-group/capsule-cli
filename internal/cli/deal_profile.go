package cli

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
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

// boundsText adds an intent's commercial-bounds/v0 document to the texts its
// step commits to, when it states a floor.
func boundsText(t map[string]string, i dealIntent) {
	if i.MinTotalMinor != nil {
		t["bounds"] = commercialBoundsText(*i.MinTotalMinor)
	}
}

// The texts a step commits to, by nonce name. Each gets its own nonce.
func dealTexts(ev dealEvent) map[string]string {
	t := map[string]string{}
	switch {
	case ev.Open != nil:
		t["verbatim"] = ev.Open.Intent.Verbatim
		boundsText(t, ev.Open.Intent)
		if ev.Kind == "sale" {
			t["item_ref"] = ev.Open.ItemRef
		}
		// A step sealed before claims were committed carries them in the
		// clear, so they are no committed text of it.
		if ev.ClaimCommit != "" {
			for i, c := range ev.Open.Claims {
				claimTexts(t, c, fmt.Sprintf("claim_text_%d", i), fmt.Sprintf("claim_source_%d", i))
			}
		}
	case ev.Claim != nil:
		if ev.ClaimCommit != "" {
			claimTexts(t, *ev.Claim, "claim_text", "claim_source")
		}
	case ev.Intent != nil:
		t["verbatim"] = ev.Intent.Verbatim
		boundsText(t, *ev.Intent)
	case ev.Message != nil:
		t["content"] = ev.Message.Text
	case ev.Evidence != nil:
		if ev.Evidence.Detail != "" {
			t["detail"] = ev.Evidence.Detail
		}
		if ev.Evidence.Email != nil && ev.Evidence.Email.Parsed.OrderID != "" {
			t["order_id"] = ev.Evidence.Email.Parsed.OrderID
		}
		if ev.Evidence.Obligation != nil && ev.Evidence.Obligation.Terms != "" {
			t["terms"] = ev.Evidence.Obligation.Terms
		}
	case ev.Snapshot != nil && (ev.Snapshot.Description != "" || ev.Snapshot.ItemRef != ""):
		if ev.Snapshot.Description != "" {
			t["description"] = ev.Snapshot.Description
		}
		if ev.Snapshot.ItemRef != "" {
			t["item_ref"] = ev.Snapshot.ItemRef
		}
	case ev.Check != nil && ev.Check.Card != "":
		t["card"] = ev.Check.Card
	case ev.Approval != nil && ev.Approval.Approver == "user":
		t["said"] = ev.Approval.Said
	case ev.TaskAuthority != nil:
		t["verbatim"] = ev.TaskAuthority.Verbatim
		boundsText(t, *ev.TaskAuthority)
		if ev.SaleAuthority != "" {
			t["sale_authority"] = ev.SaleAuthority
		}
	case ev.Acceptance != nil:
		t["accepted_words"] = ev.Acceptance.Words
	case ev.Platform != nil:
		t["displayed_text"] = ev.Platform.DisplayedText
		if ev.Platform.UserText != "" {
			t["returned_user_text"] = ev.Platform.UserText
		}
	case ev.Act != nil:
		if ev.Act.Reference != "" {
			t["reference"] = ev.Act.Reference
		}
		if ev.Act.Description != "" {
			t["description"] = ev.Act.Description
		}
	case ev.Outcome != nil && ev.Outcome.Note != "":
		t["note"] = ev.Outcome.Note
	case ev.Disclosure != nil:
		// Each disclosed value is committed, never recorded.
		for i, f := range ev.Disclosure.Fields {
			t[fmt.Sprintf("value_%d", i)] = f.Value
		}
	}
	// A materiality predicate's name and version describe the user's own
	// policy: committed, never recorded; the digest alone says which applied.
	var m dealMateriality
	if ev.Open != nil {
		m = ev.Open.Materiality
	} else if ev.Check != nil {
		m = ev.Check.Materiality
	}
	if m.Name != "" {
		t["materiality"] = materialityLabelText(m)
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
		case e.Disclosure != nil:
			if e.Disclosure.Who != nil {
				whos = append(whos, *e.Disclosure.Who)
			}
			// A disclosed value never reaches a record, however short.
			for _, f := range e.Disclosure.Fields {
				if len(strings.TrimSpace(f.Value)) >= 5 {
					out = append(out, strings.TrimSpace(f.Value))
				}
			}
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

// counterpartyProfileBlock is the sealed shape of a payee's profile-scoped
// fingerprint, the same in a counterparty_profile record and in a rules
// checker's input.
func counterpartyProfileBlock(payee string) map[string]interface{} {
	return map[string]interface{}{"fp_alg": dealProfileFPAlg, "ids": map[string]interface{}{"payee": payee}}
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
	if a.Direction != "" {
		m["direction"] = a.Direction
	}
	if a.FeeMinor != nil {
		m["fee_minor"] = *a.FeeMinor
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

// limitSetBody is a version of the user's limits as a record carries it:
// allowed absent means no restriction, present and empty means nothing is
// allowed yet.
func limitSetBody(l dealLimitSet) map[string]interface{} {
	m := map[string]interface{}{}
	if l.MaxTotalMinor != nil {
		m["max_total_minor"] = *l.MaxTotalMinor
	}
	if l.BoundsCommitment != "" {
		m["bounds_commitment"] = l.BoundsCommitment
	}
	if l.Allowed != nil {
		m["allowed"] = l.Allowed
	}
	return m
}

// rulesBody is what a verdict record carries about the profile's rules.
func rulesBody(r dealRules) (map[string]interface{}, error) {
	// No words the checker wrote are sealed in the clear: why it was not
	// evaluated is a token, and a finding is its id, verdict, limit and
	// value. The card the user saw, with every reason, is committed to.
	m := map[string]interface{}{"status": r.Status}
	for k, v := range map[string]string{"cause": r.Cause, "ruleset_id": r.RulesetID, "definition_digest": r.DefinitionDigest,
		"checker_sha256": r.CheckerSHA256, "verdict": r.Verdict} {
		if v != "" {
			m[k] = v
		}
	}
	if r.Status == "evaluated" {
		findings := make([]interface{}, len(r.Findings))
		for i, f := range r.Findings {
			fm := map[string]interface{}{"id": f.ID, "verdict": f.Verdict}
			if f.Check != "" {
				fm["check"] = f.Check
			}
			for k, raw := range map[string]json.RawMessage{"limit": f.Limit, "value": f.Value} {
				if len(raw) == 0 {
					continue
				}
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber()
				var v interface{}
				if err := decoder.Decode(&v); err != nil {
					return nil, err
				}
				fm[k] = v
			}
			findings[i] = fm
		}
		m["findings"] = findings
		if h := r.History; h != nil {
			m["history"] = map[string]interface{}{"days": h.Days, "acts": h.Acts, "complete": h.Complete}
		}
		// The tier as the checker reported it; "not_stated" when it did not
		// say, which reads as judged.
		m["tier"] = r.Tier
		if r.Tier == "" {
			m["tier"] = "not_stated"
		}
		if r.Grade != "" {
			m["grade"] = r.Grade
		}
	}
	return m, nil
}

// materialityBody is a materiality predicate as a record carries it: its
// digest, and a commitment to its name and version when there is one. A
// verdict sealed before the name and version were committed (with no nonce
// for them) carries them in the clear, and re-derives so.
func materialityBody(m dealMateriality, nonces map[string]string, commit func(string) (string, error)) (map[string]interface{}, error) {
	mb := map[string]interface{}{"digest": m.Digest}
	switch {
	case m.Name == "":
	case nonces["materiality"] != "":
		c, err := commit("materiality")
		if err != nil {
			return nil, err
		}
		mb["label_commitment"] = c
	default:
		mb["name"], mb["version"] = m.Name, m.Version
	}
	return mb, nil
}

// materialityLabelText is the text a materiality label commitment binds: the
// JCS bytes of the predicate's name and version.
func materialityLabelText(m dealMateriality) string {
	b, err := canonical.JCS(map[string]interface{}{"name": m.Name, "version": m.Version})
	if err != nil {
		return ""
	}
	return string(b)
}

func intentBody(i dealIntent, commit func(string) (string, error)) (map[string]interface{}, error) {
	c, err := commit("verbatim")
	if err != nil {
		return nil, err
	}
	m := map[string]interface{}{"verbatim_commitment": c}
	if i.PartyRole != "" {
		m["party_role"] = i.PartyRole
	}
	if asked := termsBody(i.Asked); len(asked) > 0 {
		m["asked"] = asked
	}
	if i.MaxTotalMinor != nil {
		m["max_total_minor"] = *i.MaxTotalMinor
	}
	if i.MinTotalMinor != nil {
		// The floor itself stays on this device: only its commitment.
		if m["bounds_commitment"], err = commit("bounds"); err != nil {
			return nil, err
		}
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
	if p.Kind != "" {
		parsed["kind"] = p.Kind
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
	// A step sealed before producers were recorded has none and re-derives
	// without one.
	if ev.Producer != nil {
		block["producer"] = map[string]interface{}{"name": ev.Producer.Name, "version": ev.Producer.Version, "commit": ev.Producer.Commit}
	}
	// The construction of the record's *_commitment values; absent, like
	// producer, on a step sealed before it was declared.
	if ev.CommitAlg != "" {
		block["commit_alg"] = ev.CommitAlg
	}
	var currency, dealType string
	if len(events) > 0 {
		currency = events[0].Event.Open.Terms.Currency
		dealType = events[0].Event.Open.Type
	}
	// classify seals the action's taxonomy class beside it, and the amount a
	// spend cap evaluates (dealSpendMinor), for a step that carries a
	// taxonomy version (dealActionClass).
	classify := func(m map[string]interface{}, action, direction string, amount *int64) {
		if ev.TaxonomyVersion == "" {
			return
		}
		// The role is fixed when the deal opens, and a seller's deal is
		// sealed by this build or later: no step sealed before keeps
		// different bytes.
		role := dealRole(events)
		m["action_class"] = dealRoleActionClass(role, dealType, action, direction)
		if action == offerAction && ev.OfferClass != "" {
			m["action_class"] = classMarketplaceOffer
		}
		m["taxonomy_version"] = ev.TaxonomyVersion
		if spend, ok := dealRoleSpendMinor(role, action, direction, amount); ok {
			m["spend_minor"] = spend
		}
		if action == "cancel" {
			sealCancelAmount(m, direction)
		}
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
	case "sale":
		// A sale's root: what is for sale and the seller's request, before
		// any buyer. No counterparty and no channel yet; the item reference
		// only as a commitment. The sale's one task authority follows it.
		o := ev.Open
		rtype = "sale"
		body["deal_type"] = o.Type
		if o.Demo {
			body["demo"] = true
		}
		if body["intent"], err = intentBody(o.Intent, commit); err != nil {
			return nil, err
		}
		body["terms"] = termsBody(o.Terms)
		body["recourse"] = recourseBody(o.Recourse)
		if body["item_ref_commitment"], err = commit("item_ref"); err != nil {
			return nil, err
		}
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
		if o.Materiality.Digest != "" {
			if body["materiality"], err = materialityBody(o.Materiality, ev.Nonces, commit); err != nil {
				return nil, err
			}
		}
		if o.Demo {
			body["demo"] = true
		}
		if body["intent"], err = intentBody(o.Intent, commit); err != nil {
			return nil, err
		}
		body["terms"] = termsBody(o.Terms)
		body["recourse"] = recourseBody(o.Recourse)
		if o.Skill != nil {
			body["skill"] = map[string]interface{}{"skill_md_digest": o.Skill.Digest, "other_copies": o.Skill.OtherCopies}
		}
		if len(o.Claims) > 0 {
			claims := make([]interface{}, len(o.Claims))
			for i, c := range o.Claims {
				if ev.ClaimCommit == "" {
					claims[i] = map[string]interface{}{"text": c.Text, "source": c.Source}
					continue
				}
				if claims[i], err = claimBody(c, fmt.Sprintf("claim_text_%d", i), fmt.Sprintf("claim_source_%d", i), commit); err != nil {
					return nil, err
				}
			}
			body["claims"] = claims
		}
		if o.Who.DomainAgeDays != nil {
			body["counterparty_facts"] = map[string]interface{}{"domain_age_days": *o.Who.DomainAgeDays}
		}
		if o.ExpectCloseBy != "" {
			body["expect_close_by"] = o.ExpectCloseBy
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
		if ev.ClaimCommit == "" {
			body = map[string]interface{}{"text": ev.Claim.Text, "source": ev.Claim.Source}
		} else if body, err = claimBody(*ev.Claim, "claim_text", "claim_source", commit); err != nil {
			return nil, err
		}
	case "evidence":
		rtype = "evidence"
		about := events[0].Digest
		for _, se := range events {
			if se.Event.Kind == "claim" && strings.EqualFold(se.Event.Claim.Text, ev.Evidence.About) {
				about = se.Digest
			}
		}
		refs := []interface{}{relRef("about", about)}
		if ev.Confirms != "" {
			// Committed to the closed deal's digest, not only its id: this
			// record cannot be reattached to another deal.
			refs = append(refs, relRef("confirms", digestOf(ev.Confirms)))
		}
		block["refs"] = refs
		if ev.Evidence.Resolves != "" {
			body["resolves_obligation"] = digestRef(digestOf(ev.Evidence.Resolves))
		}
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
		if o := ev.Evidence.Obligation; o != nil {
			ob := map[string]interface{}{"kind": o.Kind}
			if o.due() {
				ob["due_by"] = o.DueBy
			} else {
				ob["cancel_by"] = o.CancelBy
			}
			if o.TakesEffect != "" {
				ob["takes_effect"] = o.TakesEffect
			}
			if o.AmountMinor != nil {
				ob["amount_minor"] = *o.AmountMinor
				cur := o.Currency
				if cur == "" {
					cur = currency
				}
				if cur != "" {
					ob["currency"] = cur
				}
			}
			if o.Period != "" {
				ob["period"] = o.Period
			}
			if o.Terms != "" {
				if ob["terms_commitment"], err = commit("terms"); err != nil {
					return nil, err
				}
			}
			body["obligation"] = ob
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
		if sn.Action == offerAction {
			// A later offer supersedes the one before it (the registered
			// relation): only the latest offer can be accepted.
			for i := len(events) - 1; i >= 0; i-- {
				if p := events[i].Event.Snapshot; events[i].Event.Kind == "snapshot" && p != nil && p.Action == offerAction {
					block["refs"] = []interface{}{relRef("supersedes", events[i].Digest)}
					break
				}
			}
		}
		if sn.Who != nil {
			setIDs(counterpartyIDs(key, *sn.Who))
		}
		body["action"] = sn.Action
		// On a sale's thread, the item it is about, salted per check: equal
		// across a sale's threads only to whoever holds the openings.
		if sn.ItemRef != "" {
			if body["item_ref_commitment"], err = commit("item_ref"); err != nil {
				return nil, err
			}
		}
		var snapCurrency string
		if sn.AmountMinor != nil {
			body["amount_minor"] = *sn.AmountMinor
			cur := currency
			if sn.Terms != nil && sn.Terms.Currency != "" {
				cur = sn.Terms.Currency
			}
			if cur != "" {
				body["currency"] = cur
			}
			snapCurrency = cur
		}
		// The checked action's direction, as its act would be sealed with: a
		// cancel that returns a sealed payment is a refund.
		direction, reversed := actDirection(events, dealAct{Action: sn.Action, AmountMinor: sn.AmountMinor, Currency: snapCurrency}, currency)
		if sn.FeeMinor != nil {
			body["fee_minor"] = *sn.FeeMinor
		}
		classify(body, sn.Action, direction, sn.AmountMinor)
		// A refund's check names the pay it reverses, as its act will: on a
		// step that seals the refund's direction (classify).
		if ev.ReversesRef != "" && ev.TaxonomyVersion != "" && reversed != "" {
			body["reverses_ref"] = typedRef(digestOf(reversed))
		}
		// The most the payment may take, beside the expected charge: what a
		// limit binds, and (beside spend_minor) what a per-action cap reads.
		if sn.AuthorizedMaxMinor != nil {
			body["authorized_max_minor"] = *sn.AuthorizedMaxMinor
			if _, classed := body["spend_minor"]; classed {
				body["spend_authorized_minor"] = *sn.AuthorizedMaxMinor
			}
		}
		if sn.SeenItem != nil {
			body["seen_item"] = *sn.SeenItem
		}
		if len(sn.Disclosing) > 0 {
			classes := make([]interface{}, len(sn.Disclosing))
			for i, c := range sn.Disclosing {
				classes[i] = c
			}
			body["disclosing"] = classes
		}
		if sn.DisclosingTo != "" {
			body["disclosing_to"] = sn.DisclosingTo
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
		if ev.RuleInputs != "" {
			sealRuleInputs(body, events, sn)
		}
	case "counterparty_profile":
		// A companion of the check it is about: only the payee's
		// profile-scoped fingerprint, never in a shared copy.
		rtype = "counterparty_profile"
		cp := ev.CounterpartyProfile
		block["refs"] = []interface{}{relRef("about", digestOf(cp.Check))}
		block["counterparty_profile"] = counterpartyProfileBlock(cp.Payee)
	case "check":
		rtype = "verdict"
		ck := ev.Check
		block["refs"] = []interface{}{relRef("checks", digestOf(ck.Snapshot))}
		body["result"] = ck.Verdict
		body["differences"] = differencesBody(ck.Differences)
		// Which materiality predicate decided the pauses on the agent's
		// picks: a verifier knows what applied ("none": every pick paused).
		if m := ck.Materiality; m.Digest != "" {
			if body["materiality"], err = materialityBody(m, ev.Nonces, commit); err != nil {
				return nil, err
			}
		}
		// What the profile's rules said, as data: the ruleset the pinned
		// checker reported (by id and definition digest), its verdict and
		// findings; or that they were not evaluated, and why; or that no
		// checker is configured.
		if ck.Rules != nil {
			if body["rules"], err = rulesBody(*ck.Rules); err != nil {
				return nil, err
			}
		}
		options := make([]interface{}, len(ck.Options))
		for i, o := range ck.Options {
			options[i] = o.ID
		}
		body["options"] = options
		if len(ck.Unverified) > 0 {
			if ev.ClaimCommit == "" {
				body["unverified"] = ck.Unverified
			} else if refs := unverifiedClaimRefs(events, ck.Unverified); len(refs) > 0 {
				// The unverified claims by reference: their words are
				// commitments in their own records.
				body["unverified_claims"] = refs
			}
		}
		var notes []interface{}
		for _, n := range ck.Notes {
			notes = append(notes, asToken(n, "note"))
		}
		// What the counterparty memory found, as tokens: never the name.
		if rc := ck.Recipient; rc != nil {
			if rc.FirstTime {
				notes = append(notes, "first_time_counterparty")
			}
			if len(rc.Repeat) > 0 {
				notes = append(notes, "repeat_disclosure")
			}
			if rc.NewProfile && ck.Verdict == "pause" {
				notes = append(notes, "new_profile")
			}
		}
		if len(notes) > 0 {
			body["notes"] = notes
		}
		if ck.Card != "" {
			// The rendering commitment to the card (renderingCommitment, the
			// one mechanism for what a person was shown).
			if body["card_commitment"], err = renderingCommitment(ev.Nonces["card"], ck.Card); err != nil {
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
		if a.Limits != nil {
			body["limits"] = map[string]interface{}{"previous": limitSetBody(a.Limits.Previous), "new": limitSetBody(a.Limits.New)}
		}
		if a.Approver == "user" {
			if body["said_commitment"], err = commit("said"); err != nil {
				return nil, err
			}
		}
		if a.ShownCard != "" {
			// Under the check's own card nonce: equal commitments mean the
			// card shown is the card checked, recomputable without the text.
			if body["card_commitment"], err = shownCommitment(events, a.Check, a.ShownCard); err != nil {
				return nil, err
			}
		}
	case "act":
		act, err := actionBody(*ev.Act, currency, commit)
		if err != nil {
			return nil, err
		}
		if ev.Act.Unchecked {
			// Not authorized: sealed as an outcome, never as an action. A
			// cancel's amount still never reads as money paid out.
			if ev.TaxonomyVersion != "" && ev.Act.Action == "cancel" {
				sealCancelAmount(act, ev.Act.Direction)
			}
			rtype = "outcome"
			body = map[string]interface{}{
				"status": "unchecked_action", "outcome": "mismatch", "unchecked": act,
				"differences": []interface{}{map[string]interface{}{"question": "asked", "rule": ev.Act.Rule}},
			}
		} else {
			rtype = "action"
			refs := []interface{}{relRef("authorized_by", digestOf(ev.Act.AuthorizedBy))}
			if ev.Act.Reverses != "" {
				refs = append(refs, relRef("reverses", digestOf(ev.Act.Reverses)))
			}
			// A seller's commit cites the acceptance it rests on.
			if ev.Act.Accepted != "" {
				refs = append(refs, relRef("source", digestOf(ev.Act.Accepted)))
			}
			block["refs"] = refs
			if ev.Act.Payee != "" {
				setIDs(counterpartyIDs(key, dealWho{Payee: ev.Act.Payee}))
			}
			classify(act, ev.Act.Action, ev.Act.Direction, ev.Act.AmountMinor)
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
	case "disclosure":
		rtype = "disclosure"
		d := ev.Disclosure
		if d.Channel != "" {
			block["channel"] = d.Channel
		}
		if d.Who != nil {
			setIDs(counterpartyIDs(key, *d.Who))
		}
		fields := make([]interface{}, len(d.Fields))
		for i, f := range d.Fields {
			c, err := commit(fmt.Sprintf("value_%d", i))
			if err != nil {
				return nil, err
			}
			fields[i] = map[string]interface{}{"class": f.Class, "value_commitment": c}
		}
		body = map[string]interface{}{"to": d.To, "fields": fields, "authority": "approval"}
		classify(body, d.action(), "", nil)
		if d.AuthorizedBy != "" {
			block["refs"] = []interface{}{relRef("authorized_by", digestOf(d.AuthorizedBy))}
			if d.Accepted != "" {
				block["refs"] = append(block["refs"].([]interface{}), relRef("source", digestOf(d.Accepted)))
			}
		} else {
			body["authority"] = "none"
			body["rule"] = asToken(d.Rule, "no_check")
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
		if len(cl.Carried) > 0 {
			carried := make([]interface{}, len(cl.Carried))
			for i, c := range cl.Carried {
				item := map[string]interface{}{"obligation": digestRef(digestOf(c.CapsuleID))}
				if c.DueBy != "" {
					item["due_by"] = c.DueBy
				} else {
					item["cancel_by"] = c.CancelBy
				}
				carried[i] = item
			}
			body["carried_obligations"] = carried
		}
	case "task_authority":
		return taskAuthorityRecord(ev, events, commit, block)
	case "platform_approval":
		return platformApprovalRecord(ev, events, commit, block, currency)
	case "acceptance":
		return acceptanceRecord(ev, events, commit, block)
	default:
		return nil, inputError("unknown step kind " + ev.Kind)
	}
	block["record_type"] = rtype
	record := map[string]interface{}{"x-deal-v0": block, "body": body}
	// A deal sealed in the typed action records carries the typed record of
	// each step of a consequential action; its evidence steps stay x-deal-v0.
	if dealRecordSet(ev, events) == recordsTyped {
		typed, err := typedRecord(ev, events, record)
		if err != nil {
			return nil, err
		}
		if typed != nil {
			return typed, nil
		}
	}
	return record, nil
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
	profile := "the x-deal-v0 profile"
	if typeName, ok := docMap["type"].(string); ok {
		schema, err = recordSchema(typeName)
		profile = "the " + typeName + " record schema"
	}
	if err != nil {
		return nil, "", err
	}
	if err = schema.Validate(doc); err != nil {
		return nil, "", inputError("refusing to seal: the step does not fit " + profile + ": " + firstSchemaError(err))
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
		if err := normalizeClaim(&o.Claims[i]); err != nil {
			return err
		}
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
	case ev.Disclosure != nil:
		d := ev.Disclosure
		if d.To == "" {
			d.To = "counterparty"
		}
		d.Channel = channelToken(d.Channel)
		for i := range d.Fields {
			d.Fields[i].Class = strings.ToLower(strings.TrimSpace(d.Fields[i].Class))
		}
		err = d.validate()
	case ev.Message != nil:
		ev.Message.Channel = channelToken(ev.Message.Channel)
	case ev.Claim != nil:
		err = normalizeClaim(ev.Claim)
	case ev.Evidence != nil:
		if ev.Evidence.Obligation != nil {
			if err = ev.Evidence.Obligation.normalize(); err != nil {
				return err
			}
		}
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
		if err = validPartyRole(ev.Intent.PartyRole); err != nil {
			return err
		}
		if err = ev.Intent.validBounds(); err != nil {
			return err
		}
		err = normalizeTerms(&ev.Intent.Asked)
	}
	return err
}
