package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// profileStep is a deal's counterparty_profile record and the check it
// follows.
type profileStep struct {
	payee, checkDigest, checkPayee string
	record                         map[string]any
}

// profileSteps reads every counterparty_profile record of a deal.
func profileSteps(t *testing.T, dealID string) []profileStep {
	t.Helper()
	events, records := recordsOf(t, dealID)
	var out []profileStep
	for i, se := range events {
		if se.Event.Kind != "counterparty_profile" {
			continue
		}
		block := records[i][dealProfile].(map[string]any)
		cp := block["counterparty_profile"].(map[string]any)
		require.Equal(t, dealProfileFPAlg, cp["fp_alg"])
		refs := block["refs"].([]any)
		require.Len(t, refs, 1)
		ref := refs[0].(map[string]any)
		require.Equal(t, "about", ref["rel"])
		require.Positive(t, i)
		check := events[i-1]
		require.Equal(t, "snapshot", check.Event.Kind, "the companion follows its check")
		// The check's per-deal fingerprints: in its x-deal-v0 block, or in a
		// typed deal's proposed action, where the typed record carries them.
		holder, _ := records[i-1][dealProfile].(map[string]any)
		if holder == nil {
			holder = bodyOf(records[i-1])
		}
		checkIDs := holder["counterparty"].(map[string]any)["ids"].(map[string]any)
		out = append(out, profileStep{
			payee: cp["ids"].(map[string]any)["payee"].(string), checkDigest: check.Digest,
			checkPayee: checkIDs["payee"].(string), record: records[i],
		})
		assert.Equal(t, check.Digest, ref["digest"], "about names the check's record digest")
		assert.Empty(t, bodyOf(records[i]), "the companion carries nothing else")
	}
	return out
}

// One merchant has one profile fingerprint across a profile's deals, while
// each deal's own fingerprint of it differs.
func TestTheSamePayeeHasOneProfileFingerprintAcrossDeals(t *testing.T) {
	dealFixture(t)
	first, second := profileSteps(t, retailDeal(t)), profileSteps(t, retailDeal(t))
	require.Len(t, first, 1, "the pay check")
	require.Len(t, second, 1)
	assert.Equal(t, first[0].payee, second[0].payee, "the same merchant, the same profile value")
	assert.NotEqual(t, first[0].checkPayee, second[0].checkPayee, "each deal's own fingerprint differs")
	assert.NotEqual(t, first[0].payee, first[0].checkPayee, "never the per-deal value")
}

// Another profile, with its own store, has another value for the merchant.
func TestAnotherProfileHasAnotherProfileFingerprint(t *testing.T) {
	dealFixture(t)
	mine := profileSteps(t, retailDeal(t))
	// A second fixture: a fresh configuration and a new store of its own.
	dealFixture(t)
	theirs := profileSteps(t, retailDeal(t))
	require.Len(t, theirs, 1)
	assert.NotEqual(t, mine[0].payee, theirs[0].payee)
}

// No record holds the merchant's id in the clear, or a bare digest of it.
func TestNoPlainDigestOfThePayeeIsSealed(t *testing.T) {
	dealFixture(t)
	var open struct {
		Who struct{ Name string } `json:"who"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, filepath.Join(retailDemo, "open.json")), &open))
	require.NotEmpty(t, open.Who.Name)
	id := retailDeal(t)
	require.Len(t, profileSteps(t, id), 1)
	normalized, err := normalizeID("payee", open.Who.Name)
	require.NoError(t, err)
	var forbidden []string
	for _, v := range []string{open.Who.Name, normalized, strings.ToLower(open.Who.Name)} {
		sum := sha256.Sum256([]byte(v))
		forbidden = append(forbidden, hex.EncodeToString(sum[:]))
	}
	_, records := recordsOf(t, id)
	for _, r := range records {
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		for _, f := range forbidden {
			assert.NotContains(t, string(raw), f, "no bare digest of the merchant id")
		}
		assert.NotContains(t, string(raw), open.Who.Name)
	}
}

// No shared copy, for any audience, carries the profile value: the
// companion is withheld, and the check it follows is shared as before.
func TestNoShareCarriesTheProfileFingerprint(t *testing.T) {
	dealFixture(t)
	id := retailDeal(t)
	steps := profileSteps(t, id)
	require.Len(t, steps, 1)
	events := chainSteps(t, id)
	private := dealPrivateValues(events)
	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		assert.False(t, dealRecordShareable(steps[0].record, "", audience, private), audience)
		_, raw := sharedCopy(t, id, audience, "x")
		assert.NotContains(t, raw, steps[0].payee, audience)
		assert.NotContains(t, raw, dealProfileFPAlg, audience)
	}
}

// The profile key and fingerprint for a fixed store secret: the vector an
// independent implementation recomputes.
func TestTheProfileFingerprintVector(t *testing.T) {
	secret, err := hex.DecodeString(strings.Repeat("11", 32))
	require.NoError(t, err)
	key := profileKeyFor(secret)
	fp, err := fingerprintID(key, "payee", "Example Stickers")
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(hmacSHA256(secret, "x-deal-v0/profile-key\x00")), hex.EncodeToString(key))
	assert.Equal(t, hex.EncodeToString(hmacSHA256(key, "x-deal-v0/fp\x00", "payee", "\x00", mustNormalize(t, "payee", "Example Stickers"))), fp)
	assert.NotEqual(t, hex.EncodeToString(dealKeyFor(secret, "deal-0000000000000000")), hex.EncodeToString(key), "never a deal's key")
}

func mustNormalize(t *testing.T, kind, raw string) string {
	t.Helper()
	n, err := normalizeID(kind, raw)
	require.NoError(t, err)
	return n
}

// wantProfileShape holds a counterparty_profile block to exactly the agreed
// shape: fp_alg hmac-sha256-profile-key and one 64-lowercase-hex payee.
func wantProfileShape(t *testing.T, v any) string {
	t.Helper()
	cp, ok := v.(map[string]any)
	require.True(t, ok, "a counterparty_profile object")
	require.Len(t, cp, 2)
	require.Equal(t, "hmac-sha256-profile-key", cp["fp_alg"])
	ids, ok := cp["ids"].(map[string]any)
	require.True(t, ok)
	require.Len(t, ids, 1)
	payee, _ := ids["payee"].(string)
	require.Regexp(t, `^[0-9a-f]{64}$`, payee)
	return payee
}

// The rules checker is given the checked record's payee keyed per profile,
// beside the sealed capsule (never in it), in exactly the agreed shape, the
// value the check's companion seals right after it.
func TestTheCheckerIsGivenTheCheckedRecordsProfileFingerprint(t *testing.T) {
	for name, typed := range map[string]bool{"x-deal-v0": false, "typed": true} {
		t.Run(name, func(t *testing.T) {
			id := stickerDeal(t, "card", typed)
			_, _, input := ruleInputs(t, id, stickerPay)
			record := input["record"].(map[string]any)
			payee := wantProfileShape(t, record["counterparty_profile"])
			steps := profileSteps(t, id)
			require.Len(t, steps, 1)
			assert.Equal(t, steps[0].payee, payee, "the companion seals the value the checker was given")
			sealed, err := json.Marshal(record["agent_input"])
			require.NoError(t, err)
			assert.NotContains(t, string(sealed), "counterparty_profile", "envelope-supplied, never in the sealed record")
		})
	}
}

// Each history act carries its own check's companion value, so a merchant
// paid in an earlier deal is the same payee in this one; an act without an
// approval (so no check) carries none.
func TestTheHistoryCarriesEachActsProfileFingerprint(t *testing.T) {
	first := stickerDeal(t, "card", false)
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	require.Equal(t, "pass", dealRun(t, "check", "--deal", first, "--input", writeJSON(t, stickerPay))["verdict"])
	approved := payNow(t, first, 454)
	require.Equal(t, false, approved["unchecked"])
	unapproved := payNow(t, first, 100)
	require.Equal(t, true, unapproved["unchecked"])

	second := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"another otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
	_, _, input := ruleInputs(t, second, stickerPay)
	now := wantProfileShape(t, input["record"].(map[string]any)["counterparty_profile"])
	byID := map[string]map[string]any{}
	for _, h := range input["history"].([]any) {
		entry := h.(map[string]any)
		byID[entry["capsule_id"].(string)] = entry
	}
	require.Contains(t, byID, approved["capsule_id"])
	require.Contains(t, byID, unapproved["capsule_id"])
	assert.Equal(t, now, wantProfileShape(t, byID[approved["capsule_id"].(string)]["counterparty_profile"]), "the same merchant across the profile's deals")
	assert.NotContains(t, byID[unapproved["capsule_id"].(string)], "counterparty_profile", "no approval, no check, no companion")
	assert.Equal(t, profileSteps(t, first)[0].payee, now)
}

// profileVectorsPath holds the bridge vectors for the payee keyed per
// profile: two deals of one profile paying one merchant, each check and its
// counterparty_profile companion as their exact sealed (JCS) bytes, and the
// target a reader keys the check on.
var profileVectorsPath = filepath.Join("testdata", "payee-fp-profile", "vectors.json")

type profileVector struct {
	Name           string `json:"name"`
	CheckJCS       string `json:"check_jcs"`
	CompanionJCS   string `json:"companion_jcs"`
	ExpectedTarget string `json:"expected_target"`
}

// CAPSULECTL_UPDATE_PROFILE_VECTORS=1 seals two real deals and writes them.
func TestRegenerateProfileVectors(t *testing.T) {
	if os.Getenv("CAPSULECTL_UPDATE_PROFILE_VECTORS") != "1" {
		t.Skip("set CAPSULECTL_UPDATE_PROFILE_VECTORS=1 to regenerate")
	}
	dealFixture(t)
	var cases []profileVector
	for n, id := range []string{retailDeal(t), retailDeal(t)} {
		events := chainSteps(t, id)
		p, err := loadProfile("deal")
		require.NoError(t, err)
		s, err := openDealSession(t.Context(), p)
		require.NoError(t, err)
		require.NoError(t, s.useDeal(t.Context(), id, false))
		for i, se := range events {
			if se.Event.Kind != "counterparty_profile" {
				continue
			}
			check, _, err := encodeDealRecord(events[i-1].Event, events[:i-1], s.dkey)
			require.NoError(t, err)
			companion, _, err := encodeDealRecord(se.Event, events[:i], s.dkey)
			require.NoError(t, err)
			cases = append(cases, profileVector{
				Name: "deal " + strconv.Itoa(n+1) + ": its pay check", CheckJCS: string(check), CompanionJCS: string(companion),
				ExpectedTarget: "payee-fp:" + dealProfileFPAlg + ":" + se.Event.CounterpartyProfile.Payee,
			})
		}
		require.NoError(t, s.close())
	}
	raw, err := json.MarshalIndent(map[string]any{
		"description": "Two deals of one profile paying one merchant. Each case: a check's sealed x-deal-v0 record and its counterparty_profile companion, as their exact JCS bytes. The companion's about ref digest is SHA-256 of check_jcs; the target is payee-fp:<fp_alg>:<payee> from the companion; both cases share it, while each check's own (per-deal) payee fingerprint differs.",
		"cases":       cases,
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(profileVectorsPath), 0o755))
	require.NoError(t, os.WriteFile(profileVectorsPath, append(raw, '\n'), 0o644))
}

// The committed vectors hold: the companion names its check by the SHA-256
// of the check's exact bytes, the target comes from the companion, one
// merchant has one target across the two deals, and each check's per-deal
// fingerprint differs.
func TestTheProfileVectorsHold(t *testing.T) {
	var file struct {
		Cases []profileVector `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, profileVectorsPath), &file))
	require.Len(t, file.Cases, 2)
	var targets, perDeal []string
	for _, c := range file.Cases {
		var check, companion map[string]any
		require.NoError(t, json.Unmarshal([]byte(c.CheckJCS), &check), c.Name)
		require.NoError(t, json.Unmarshal([]byte(c.CompanionJCS), &companion), c.Name)
		block := companion[dealProfile].(map[string]any)
		require.Equal(t, "counterparty_profile", block["record_type"])
		ref := block["refs"].([]any)[0].(map[string]any)
		require.Equal(t, "about", ref["rel"])
		sum := sha256.Sum256([]byte(c.CheckJCS))
		assert.Equal(t, hex.EncodeToString(sum[:]), ref["digest"], c.Name)
		payee := wantProfileShape(t, block["counterparty_profile"])
		assert.Equal(t, "payee-fp:hmac-sha256-profile-key:"+payee, c.ExpectedTarget, c.Name)
		require.Equal(t, "check", check[dealProfile].(map[string]any)["record_type"])
		perDeal = append(perDeal, check[dealProfile].(map[string]any)["counterparty"].(map[string]any)["ids"].(map[string]any)["payee"].(string))
		targets = append(targets, c.ExpectedTarget)
	}
	assert.Equal(t, targets[0], targets[1], "one merchant, one target across the profile's deals")
	assert.NotEqual(t, perDeal[0], perDeal[1], "each deal's own fingerprint differs")
}

// The engine's bridge, run over this repository's vectors, keys each check
// on the target the vectors expect; and the checker-input schema accepts
// exactly the counterparty_profile values the engine takes, refusing every
// one it ignores. The two halves agree on the wire.
func TestTheEngineAgreesWithTheProfileVectors(t *testing.T) {
	var ours struct {
		Cases []profileVector `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, profileVectorsPath), &ours))
	var engine struct {
		Replay map[string]struct {
			Decisions []struct {
				RecordType    string   `json:"record_type"`
				Target        *string  `json:"target"`
				IgnoredInputs []string `json:"ignored_inputs"`
				DedupeResult  string   `json:"dedupe_result"`
			} `json:"decisions"`
		} `json:"replay"`
		LiveTarget map[string]struct {
			Entry         map[string]any `json:"entry"`
			Target        string         `json:"target"`
			IgnoredInputs []string       `json:"ignored_inputs"`
		} `json:"live_target"`
		LiveSeenBefore map[string]struct {
			SeenBeforeResult   string `json:"seen_before_result"`
			SeenBeforeEvidence struct {
				FoldKey struct {
					Value string `json:"value"`
				} `json:"fold_key"`
				PriorCount int `json:"prior_count"`
			} `json:"seen_before_evidence"`
		} `json:"live_seen_before"`
	}
	require.NoError(t, json.Unmarshal(mustRead(t, filepath.Join("testdata", "payee-fp-profile", "engine-vectors.json")), &engine))
	want := ours.Cases[0].ExpectedTarget

	// Replay: both deals' checks key on the profile target the vectors expect.
	var checks []string
	for _, d := range engine.Replay["two-deals-with-companions"].Decisions {
		if d.RecordType == "check" {
			require.NotNil(t, d.Target)
			checks = append(checks, *d.Target)
			assert.Empty(t, d.IgnoredInputs)
		}
	}
	assert.Equal(t, []string{ours.Cases[0].ExpectedTarget, ours.Cases[1].ExpectedTarget}, checks)
	// Live: deal 2's payee was seen before, on the profile target.
	seen := engine.LiveSeenBefore["with-profile"]
	assert.Equal(t, "pass", seen.SeenBeforeResult)
	assert.Equal(t, want, seen.SeenBeforeEvidence.FoldKey.Value)
	assert.Equal(t, 1, seen.SeenBeforeEvidence.PriorCount)

	// Live input: the schema accepts what the engine keys on, and refuses
	// every value the engine ignores.
	compiler := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(mustRead(t, filepath.Join("..", "..", "skills", "deal", "profile", "external-check-input-v0.schema.json"))))
	require.NoError(t, err)
	require.NoError(t, compiler.AddResource("external-check-input-v0.schema.json", doc))
	schema, err := compiler.Compile("external-check-input-v0.schema.json")
	require.NoError(t, err)
	require.Len(t, engine.LiveTarget, 8)
	for name, c := range engine.LiveTarget {
		raw, err := json.Marshal(map[string]any{"schema": externalCheckInput, "record": c.Entry, "history": []any{},
			"history_scope": map[string]any{"days": 31, "max_records": 1000, "complete": true}})
		require.NoError(t, err)
		input, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		require.NoError(t, err)
		valid := schema.Validate(input) == nil
		_, present := c.Entry["counterparty_profile"]
		switch {
		case name == "good":
			assert.True(t, valid, name)
			assert.Equal(t, want, c.Target)
			assert.Empty(t, c.IgnoredInputs)
		case !present:
			assert.True(t, valid, name)
			assert.True(t, strings.HasPrefix(c.Target, "payee-fp:hmac-sha256-deal-key:"), name)
		default:
			assert.False(t, valid, "%s: the engine ignores it, so the schema refuses it", name)
			assert.Equal(t, []string{"counterparty_profile"}, c.IgnoredInputs, name)
			assert.True(t, strings.HasPrefix(c.Target, "payee-fp:hmac-sha256-deal-key:"), name)
		}
	}
}

// beforeCompanions seals what fn seals as a release from before checks had a
// counterparty_profile companion would.
func beforeCompanions(t *testing.T, fn func()) {
	t.Helper()
	dealSealsCounterpartyProfile = false
	defer func() { dealSealsCounterpartyProfile = true }()
	fn()
}

// noPayeeOpen opens a purchase whose counterparty is named by its domain
// only, so its pay check names no payee.
const noPayeeOpen = `{"type":"purchase","channel":"web",
	"intent":{"verbatim":"buy me an otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay"]},
	"who":{"domain":"stickers.example"},
	"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
	"recourse":{"rail":"card","refundable":true}}`

// A check that names no payee seals no companion.
func TestACheckNamingNoPayeeSealsNoCompanion(t *testing.T) {
	dealFixture(t)
	id := dealRun(t, "open", "--input", writeJSON(t, noPayeeOpen))["deal_id"].(string)
	dealRun(t, "check", "--deal", id, "--input", writeJSON(t, stickerPay))
	events := chainSteps(t, id)
	checked := 0
	for _, se := range events {
		assert.NotEqual(t, "counterparty_profile", se.Event.Kind, "step %d", se.Event.N)
		if se.Event.Kind == "snapshot" {
			checked++
			assert.True(t, se.Event.Snapshot.Who == nil || se.Event.Snapshot.Who.Payee == "", "the check names no payee")
		}
	}
	assert.Equal(t, 1, checked)
}

// payWithChecker opens a deal from open, checks and pays it under a passing
// rules checker, and returns the authorized act's capsule id.
func payWithChecker(t *testing.T, open string) string {
	t.Helper()
	id := dealRun(t, "open", "--input", writeJSON(t, open))["deal_id"].(string)
	require.Equal(t, "pass", dealRun(t, "check", "--deal", id, "--input", writeJSON(t, stickerPay))["verdict"])
	paid := payNow(t, id, 454)
	require.Equal(t, false, paid["unchecked"])
	return paid["capsule_id"].(string)
}

// History carries no counterparty_profile for an act whose check has no
// companion: one checked by an earlier release, or one whose check named no
// payee. Only an act checked with a companion carries one.
func TestHistoryHasNoProfileFingerprintWithoutACompanion(t *testing.T) {
	dealFixture(t)
	checker := stubChecker(t, "rules", checkerPrints("allow", passFinding))
	pinChecker(t, map[string]any{"command": []string{checker}})
	stickers := `{"type":"purchase","channel":"web",
		"intent":{"verbatim":"buy me an otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay"]},
		"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
		"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
		"recourse":{"rail":"card","refundable":true}}`
	var old string
	beforeCompanions(t, func() { old = payWithChecker(t, stickers) })
	noPayee := payWithChecker(t, noPayeeOpen)
	current := payWithChecker(t, stickers)

	_, _, input := ruleInputs(t, dealRun(t, "open", "--input", writeJSON(t, stickers))["deal_id"].(string), stickerPay)
	byID := map[string]map[string]any{}
	for _, h := range input["history"].([]any) {
		entry := h.(map[string]any)
		byID[entry["capsule_id"].(string)] = entry
	}
	for name, id := range map[string]string{"an act checked before checks had a companion": old, "an act whose check named no payee": noPayee} {
		require.Contains(t, byID, id, name)
		assert.NotContains(t, byID[id], "counterparty_profile", name)
	}
	require.Contains(t, byID, current)
	wantProfileShape(t, byID[current]["counterparty_profile"])
}

// sharedRecordTypes is the record types a shared copy discloses, in log
// order, and how many it withholds.
func sharedRecordTypes(t *testing.T, b map[string]any) ([]string, int) {
	t.Helper()
	disclosures, _ := b["disclosures"].(map[string]any)
	var types []string
	withheld := 0
	for _, r := range b["records"].([]any) {
		rec := r.(map[string]any)
		d, ok := disclosures[rec["capsule_id"].(string)].(map[string]any)
		if !ok {
			withheld++
			continue
		}
		input := d["agent_input"].(map[string]any)
		if blk, ok := input[dealProfile].(map[string]any); ok {
			types = append(types, blk["record_type"].(string))
		} else if typ, ok := input["type"].(string); ok {
			types = append(types, typ)
		}
	}
	return types, withheld
}

// A shared copy of a deal with companions discloses the same records as a
// shared copy of the same deal without: the checks included; only the
// withheld companions differ.
func TestASharedCopyShowsTheSameChecksWithCompanions(t *testing.T) {
	for _, audience := range []string{dealAudienceCounterparty, dealAudienceAdjudicator} {
		t.Run(audience, func(t *testing.T) {
			// A sticker deal: its pay check carries no free text, so a shared
			// copy shows it.
			deal := func() string {
				id := dealRun(t, "open", "--input", writeJSON(t, `{"type":"purchase","channel":"web",
					"intent":{"verbatim":"buy me an otter sticker for at most 5 dollars","max_total_minor":500,"allowed":["pay"]},
					"who":{"name":"Sticker Marketplace","domain":"stickers.example"},
					"terms":{"item":"otter sticker","price_minor":454,"currency":"USD"},
					"recourse":{"rail":"card","refundable":true}}`))["deal_id"].(string)
				dealRun(t, "check", "--deal", id, "--input", writeJSON(t, stickerPay))
				payNow(t, id, 454)
				return id
			}
			dealFixture(t)
			var without string
			beforeCompanions(t, func() { without = deal() })
			with := deal()
			require.Len(t, profileSteps(t, with), 1)
			require.Empty(t, profileSteps(t, without))
			bWith, _ := sharedCopy(t, with, audience, "x")
			bWithout, _ := sharedCopy(t, without, audience, "x")
			typesWith, withheldWith := sharedRecordTypes(t, bWith)
			typesWithout, withheldWithout := sharedRecordTypes(t, bWithout)
			assert.Equal(t, typesWithout, typesWith, "the same records disclosed")
			assert.Contains(t, typesWith, "check", "the check is shown")
			assert.Equal(t, withheldWithout+1, withheldWith, "only the companion is added, withheld")
		})
	}
}

// A deal sealed before checks had a companion re-derives, under this
// release, every record byte for byte: its stored digests still hold.
func TestADealSealedBeforeCompanionsReDerivesUnchanged(t *testing.T) {
	dealFixture(t)
	var id string
	beforeCompanions(t, func() {
		id = retailDeal(t)
		dealRun(t, "close", "--deal", id, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"cat sticker"}}`))
	})
	events := chainSteps(t, id)
	p, err := loadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	require.NoError(t, s.useDeal(t.Context(), id, false))
	require.NotEmpty(t, events)
	for i, se := range events {
		assert.NotEqual(t, "counterparty_profile", se.Event.Kind)
		_, digest, err := encodeDealRecord(se.Event, events[:i], s.dkey)
		require.NoError(t, err, "step %d", se.Event.N)
		assert.Equal(t, se.Digest, digest, "step %d (%s) re-derives byte for byte", se.Event.N, se.Event.Kind)
	}
	// The session holds the store's lock: release it before the next command.
	require.NoError(t, s.close())
	report := dealRun(t, "report", "--deal", id)
	assert.NotEmpty(t, report["steps"], "and it still reports")
}
