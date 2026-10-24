package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	aacsettlement "github.com/action-state-group/agent-action-capsule/go/settlement"
	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/evidencebook/adapter"
	"github.com/action-state-group/evidencebook/adapter/acp"
	"github.com/action-state-group/evidencebook/adapter/ucp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const wrappedVectors = "testdata/wrapped-object"

type wrappedCase struct {
	Name string `json:"name"`
	Seal struct {
		Type         string  `json:"type"`
		Octets       string  `json:"octets"`
		Object       *string `json:"object"`
		ObjectDigest *string `json:"object_digest"`
		Headers      *string `json:"headers"`
	} `json:"seal"`
	Expect struct {
		Sealed         bool   `json:"sealed"`
		Error          string `json:"error"`
		Digest         string `json:"digest"`
		TypeRegistered bool   `json:"type_registered"`
		WrappedHeld    string `json:"wrapped_held"`
		ObjectDigest   string `json:"object_digest"`
		CarrierHeaders string `json:"carrier_headers"`
	} `json:"expect"`
	Verify []struct {
		Object  string            `json:"object"`
		Headers string            `json:"headers"`
		Expect  map[string]string `json:"expect"`
	} `json:"verify"`
}

type wrappedVectorFile struct {
	Cases     []wrappedCase `json:"cases"`
	Recipient []struct {
		Name   string            `json:"name"`
		Object string            `json:"object"`
		Digest string            `json:"digest"`
		Octets string            `json:"octets"`
		Expect map[string]string `json:"expect"`
	} `json:"recipient"`
}

func loadWrappedVectors(t *testing.T) wrappedVectorFile {
	t.Helper()
	var v wrappedVectorFile
	require.NoError(t, json.Unmarshal(mustRead(t, filepath.Join(wrappedVectors, "cases.json")), &v))
	require.NotEmpty(t, v.Cases)
	return v
}

func vectorPath(name string) string { return filepath.Join(wrappedVectors, name) }

// The vector files are the ones SHA256SUMS names, byte for byte: a fixture
// edited without regenerating fails here.
func TestWrappedVectorFilesMatchTheirSums(t *testing.T) {
	sums := strings.Split(strings.TrimSpace(string(mustRead(t, vectorPath("SHA256SUMS")))), "\n")
	require.NotEmpty(t, sums)
	for _, line := range sums {
		want, name, ok := strings.Cut(line, "  ")
		require.True(t, ok, line)
		sum := sha256.Sum256(mustRead(t, vectorPath(name)))
		assert.Equal(t, want, hex.EncodeToString(sum[:]), name)
	}
}

// Every registered wrapped type has its octets rule, and nothing else does:
// a module bump that registers a type fails here until its rule is added.
func TestWrappedOctetsRulesCoverTheRegistry(t *testing.T) {
	var registry, rules []string
	for k := range aacsettlement.WrappedTypes {
		registry = append(registry, k)
	}
	for k := range wrappedOctetsRules {
		rules = append(rules, k)
	}
	slices.Sort(registry)
	slices.Sort(rules)
	assert.Equal(t, registry, rules)
}

// noteWrapped seals a vector case on a deal and returns the note's output, or
// the error.
func noteWrapped(t *testing.T, dealID string, c wrappedCase) (map[string]any, error) {
	t.Helper()
	args := []string{"--profile", "deal", "deal", "note", "--deal", dealID, "--kind", "evidence", "--object-type", c.Seal.Type, "--octets", c.Seal.Octets}
	if c.Seal.Object != nil {
		args = append(args, "--object", vectorPath(*c.Seal.Object))
	}
	if c.Seal.ObjectDigest != nil {
		args = append(args, "--object-digest", *c.Seal.ObjectDigest)
	}
	if c.Seal.Headers != nil {
		args = append(args, "--headers", vectorPath(*c.Seal.Headers))
	}
	out, err := invoke(t, "", args...)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m, nil
}

// verifyObject runs deal verify-object and returns its one result and error.
func verifyObject(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", append([]string{"--profile", "deal", "deal", "verify-object"}, args...)...)
	var m map[string]any
	if out != "" {
		require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	}
	return m, err
}

// wrappedRecord is the last evidence record of a deal that carries a held
// object.
func wrappedRecord(t *testing.T, dealID string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, r := range records(t, dealID) {
		if body, ok := r["body"].(map[string]any); ok && body["wrapped"] != nil {
			found = body
		}
	}
	require.NotNil(t, found, "a record with a held object")
	return found
}

// Each vector seals (or is refused) on a buyer's book deal; a sealed one's
// record carries the expected entry, and verify-object, run in a new process
// on the reopened store, gives the expected results from the artifact store.
func TestWrappedObjectVectors(t *testing.T) {
	for _, c := range loadWrappedVectors(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			dealFixture(t)
			dealID := retailDeal(t)
			out, err := noteWrapped(t, dealID, c)
			if !c.Expect.Sealed {
				require.ErrorIs(t, err, cli.ErrInput)
				assert.Contains(t, err.Error(), c.Expect.Error)
				for _, r := range records(t, dealID) {
					assert.Nil(t, r["body"].(map[string]any)["wrapped"], "nothing sealed")
				}
				return
			}
			require.NoError(t, err)
			w := out["wrapped"].(map[string]any)
			assert.Equal(t, c.Seal.Type, w["type"])
			assert.Equal(t, c.Expect.TypeRegistered, w["type_registered"])
			assert.Equal(t, c.Expect.Digest, w["digest"])
			assert.Equal(t, c.Expect.WrappedHeld, w["wrapped_held"])

			body := wrappedRecord(t, dealID)
			assert.Equal(t, []any{map[string]any{"type": c.Seal.Type, "digest_alg": "SHA-256", "digest": c.Expect.Digest}}, body["wrapped"])
			assert.Equal(t, c.Seal.Octets, body["wrapped_octets"])
			assert.Equal(t, c.Expect.WrappedHeld, body["wrapped_held"])
			assert.Equal(t, wrappedSource, body["source"])
			_, committed := body["carrier_headers_commitment"]
			assert.Equal(t, c.Seal.Headers != nil, committed)
			raw, err := json.Marshal(records(t, dealID))
			require.NoError(t, err)
			assert.NotContains(t, string(raw), `"content"`, "the entry never carries the object")

			res, err := verifyObject(t, "--deal", dealID)
			require.NoError(t, err)
			assert.Equal(t, true, res["matches"])
			obj := res["objects"].([]any)[0].(map[string]any)
			assert.Equal(t, c.Expect.ObjectDigest, obj["object_digest"])
			assert.Equal(t, c.Expect.CarrierHeaders, obj["carrier_headers"])
			for _, v := range c.Verify {
				var args []string
				if v.Object != "" {
					args = append(args, "--object", vectorPath(v.Object))
				}
				if v.Headers != "" {
					args = append(args, "--headers", vectorPath(v.Headers))
				}
				res, err := verifyObject(t, append([]string{"--deal", dealID}, args...)...)
				got := res["objects"].([]any)[0].(map[string]any)
				for k, want := range v.Expect {
					assert.Equal(t, want, got[k], k)
				}
				if slices.Contains([]string{v.Expect["object_digest"], v.Expect["carrier_headers"]}, verifyFail) {
					require.ErrorIs(t, err, cli.ErrObjectUnverified)
					assert.Equal(t, false, res["matches"])
				} else {
					require.NoError(t, err)
				}
			}
			checkerPasses(t, dealID)
		})
	}
}

// A recipient checks an object it was given against the digest and
// wrapped_octets in its copy's record, with no deal.
func TestWrappedObjectRecipientCheck(t *testing.T) {
	dealFixture(t)
	for _, c := range loadWrappedVectors(t).Recipient {
		t.Run(c.Name, func(t *testing.T) {
			res, err := verifyObject(t, "--object", vectorPath(c.Object), "--digest", c.Digest, "--octets", c.Octets)
			for k, want := range c.Expect {
				assert.Equal(t, want, res[k], k)
			}
			if c.Expect["object_digest"] == verifyFail {
				require.ErrorIs(t, err, cli.ErrObjectUnverified)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// --out writes the held object and its headers back out exactly: what an
// adapter reads to run the object's own checks later.
func TestWrappedObjectWritesTheHeldBytesBack(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", vectorPath("ucp-checkout-response.body"),
		"--headers", vectorPath("ucp-checkout-response.headers"), "--object-type", "ucp.checkout", "--octets", "jcs")
	dir := t.TempDir()
	_, err := verifyObject(t, "--deal", dealID, "--out", dir)
	require.NoError(t, err)
	assert.Equal(t, mustRead(t, vectorPath("ucp-checkout-response.body")), mustRead(t, filepath.Join(dir, "object")))
	assert.Equal(t, mustRead(t, vectorPath("ucp-checkout-response.headers")), mustRead(t, filepath.Join(dir, "headers")))
	_, err = verifyObject(t, "--deal", dealID, "--out", dir)
	require.ErrorIs(t, err, cli.ErrInput, "never over a file already there")
	// With only one of the two names taken, nothing is written.
	partial := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(partial, "headers"), nil, 0o600))
	_, err = verifyObject(t, "--deal", dealID, "--out", partial)
	require.ErrorIs(t, err, cli.ErrInput)
	_, statErr := os.Stat(filepath.Join(partial, "object"))
	assert.True(t, os.IsNotExist(statErr), "a refused --out leaves nothing behind")
}

// Header bytes that are not UTF-8 seal and verify: their commitment is over
// their base64url form.
func TestWrappedObjectHeadersNeedNotBeUTF8(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	headers := writeFile(t, "h", []byte("HTTP/1.1 200 OK\r\nX-Note: caf\xe9"))
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", vectorPath("delivery-proof.txt"),
		"--headers", headers, "--object-type", "delivery.proof")
	res, err := verifyObject(t, "--deal", dealID)
	require.NoError(t, err)
	assert.Equal(t, verifyPass, res["objects"].([]any)[0].(map[string]any)["carrier_headers"])
}

// The largest object fits a step's artifact record; one byte more is refused.
func TestWrappedObjectSizeLimit(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	big := bytes.Repeat([]byte("a"), maxWrappedObject)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", writeFile(t, "big", big), "--object-type", "delivery.proof")
	res, err := verifyObject(t, "--deal", dealID)
	require.NoError(t, err)
	assert.Equal(t, verifyPass, res["objects"].([]any)[0].(map[string]any)["object_digest"])
	err = dealRunErr(t, "note", "--deal", dealID, "--kind", "evidence", "--object", writeFile(t, "big", append(big, 'a')), "--object-type", "delivery.proof")
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, err.Error(), "is over")
}

func TestWrappedObjectRefusals(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	object := vectorPath("delivery-proof.txt")
	digest := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"not evidence", []string{"--kind", "message", "--object", object, "--object-type", "delivery.proof"}, "go with --kind evidence"},
		{"both forms", []string{"--kind", "evidence", "--object", object, "--object-digest", digest, "--object-type", "delivery.proof"}, "alternatives"},
		{"with an email", []string{"--kind", "evidence", "--object", object, "--email", object, "--object-type", "delivery.proof"}, "separate notes"},
		{"no type", []string{"--kind", "evidence", "--object", object}, "--object-type is required"},
		{"bad type", []string{"--kind", "evidence", "--object", object, "--object-type", "Bad Type"}, "lowercase"},
		{"bad rule", []string{"--kind", "evidence", "--object", object, "--object-type", "delivery.proof", "--octets", "raw"}, "as-transmitted"},
		{"bad digest", []string{"--kind", "evidence", "--object-digest", "ABC", "--object-type", "delivery.proof"}, "64 lowercase hex"},
		{"type without object", []string{"--kind", "evidence", "--object-type", "delivery.proof", "--input", writeJSON(t, `{"about":"x","source":"y"}`)}, "go with --object"},
		{"empty object", []string{"--kind", "evidence", "--object", writeFile(t, "e", nil), "--object-type", "delivery.proof"}, "empty"},
		{"NUL in headers", []string{"--kind", "evidence", "--object", object, "--object-type", "delivery.proof", "--headers", writeFile(t, "h", []byte("A: b\x00"))}, "no NUL"},
		{"oversize headers", []string{"--kind", "evidence", "--object", object, "--object-type", "delivery.proof", "--headers", writeFile(t, "h", bytes.Repeat([]byte("a"), maxCarrierHeaders+1))}, "is over"},
		{"input sets it", []string{"--kind", "evidence", "--input", writeJSON(t, `{"about":"x","source":"y","wrapped":{"type":"delivery.proof","octets":"as-transmitted","digest":"`+digest+`"}}`)}, "cannot set a held object"},
		{"input names another source", []string{"--kind", "evidence", "--object", object, "--object-type", "delivery.proof", "--input", writeJSON(t, `{"about":"x","source":"merchant_email"}`)}, "source is wrapped_object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := dealRunErr(t, append([]string{"note", "--deal", dealID}, tc.args...)...)
			require.ErrorIs(t, err, cli.ErrInput)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	for _, r := range records(t, dealID) {
		assert.Nil(t, r["body"].(map[string]any)["wrapped"], "nothing sealed")
	}

	// verify-object's own refusals.
	_, err := verifyObject(t, "--deal", dealID)
	require.ErrorIs(t, err, cli.ErrInput, "no held object in this deal")
	_, err = verifyObject(t, "--object", object)
	require.ErrorIs(t, err, cli.ErrInput, "a digest is needed without a deal")
	_, err = verifyObject(t, "--deal", dealID, "--digest", digest)
	require.ErrorIs(t, err, cli.ErrInput, "with a deal the digest is the sealed one")
}

// --input may give the note's about and detail; the source stays the fixed
// token, and a digest-only note says it holds a digest.
func TestWrappedObjectAboutAndDigestOnly(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	digest := strings.Repeat("cd", 32)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object-digest", digest, "--object-type", "delivery.proof")
	body := wrappedRecord(t, dealID)
	assert.Equal(t, wrappedHeldDigest, body["wrapped_held"])
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", vectorPath("delivery-proof.txt"), "--object-type", "delivery.proof",
		"--input", writeJSON(t, `{"about":"the courier's proof","detail":"left at the front desk"}`))
	body = wrappedRecord(t, dealID)
	assert.Equal(t, wrappedSource, body["source"])
	assert.NotEmpty(t, body["detail_commitment"])

	res, err := verifyObject(t, "--deal", dealID)
	require.NoError(t, err)
	objects := res["objects"].([]any)
	require.Len(t, objects, 2)
	assert.Equal(t, verifyNotPresent, objects[0].(map[string]any)["object_digest"])
	assert.Equal(t, verifyPass, objects[1].(map[string]any)["object_digest"])
	_, err = verifyObject(t, "--deal", dealID, "--out", t.TempDir())
	require.ErrorIs(t, err, cli.ErrInput, "--out needs --step with two held objects")
	_, err = verifyObject(t, "--deal", dealID, "--object", vectorPath("delivery-proof.txt"))
	require.ErrorIs(t, err, cli.ErrInput, "a copy is of one held object: --step names it")
	_, err = verifyObject(t, "--deal", dealID, "--octets", "jcs")
	require.ErrorIs(t, err, cli.ErrInput, "with --deal the rule is the sealed one")
}

// A number outside binary64's range has no RFC 8785 form: sealing it under
// jcs is refused, and it never verifies against the form of the largest
// finite number, which Canonicalize alone would clamp it to.
func TestWrappedObjectJCSRefusesAnOverflowingNumber(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	for _, n := range []string{"1e999", "-1e999"} {
		over := writeFile(t, "o.json", []byte(`{"n":`+n+`}`))
		err := dealRunErr(t, "note", "--deal", dealID, "--kind", "evidence", "--object", over, "--object-type", "x-example.object", "--octets", "jcs")
		require.ErrorIs(t, err, cli.ErrInput, n)
		assert.Contains(t, err.Error(), "outside binary64", n)
		largest := sha256Hex([]byte(`{"n":` + strings.TrimSuffix(n, "1e999") + `1.7976931348623157e+308}`))
		res, err := verifyObject(t, "--object", over, "--digest", largest, "--octets", "jcs")
		require.ErrorIs(t, err, cli.ErrObjectUnverified, n)
		assert.Equal(t, verifyFail, res["object_digest"], n)
	}
	// The largest finite number itself seals.
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", writeFile(t, "o.json", []byte(`{"n":1.7976931348623157e308}`)), "--object-type", "x-example.object", "--octets", "jcs")
}

// A held checkout states the deal's own total in minor units. Once the deal
// closes as received, the counterparty's copy shows that amount (its field
// openings), and the held object's JSON number must not refuse the copy; a
// number the copies never show stays private. Digits inside a string are
// never exempt: an id that holds the shown amount keeps it as a needle, so
// such a deal's copies are refused (fail closed) rather than let the id
// through.
func TestWrappedObjectStatingTheDealsTotalStillShares(t *testing.T) {
	closedDeal := func(object string) string {
		dealFixture(t)
		open := `{"type":"purchase","channel":"web","intent":{"verbatim":"buy two widgets","allowed":["pay"]},"who":{"name":"Example Merchant","domain":"merchant.example"},"terms":{"item":"Widget","quantity":2,"price_minor":5400,"currency":"USD"},"recourse":{"rail":"card","refundable":true}}`
		dealID := dealRun(t, "open", "--input", writeJSON(t, open))["deal_id"].(string)
		dealRun(t, "close", "--deal", dealID, "--input", writeJSON(t, `{"status":"received","delivered":{"item":"Widget","quantity":2,"price_minor":5400,"currency":"USD"}}`))
		dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", writeFile(t, "o.json", []byte(object)), "--object-type", "x-example.order")
		return dealID
	}

	dealID := closedDeal(`{"totals":[{"type":"total","amount":5400}],"buyer_phone":"5550104477"}`)
	for _, audience := range []string{cli.DealAudienceCounterparty, cli.DealAudienceAdjudicator} {
		_, raw := sharedCopy(t, dealID, audience, "x")
		assert.NotContains(t, raw, "5550104477", audience)
	}
	err := dealShareGate([]byte("<p>call 5550104477</p>"), chainSteps(t, dealID), cli.DealAudienceAdjudicator)
	require.Error(t, err, "a number only the held object states stays private")

	dealID = closedDeal(`{"totals":[{"type":"total","amount":5400}],"reference":"ref-5400-abc"}`)
	err = dealShareGate([]byte("<p>reference ref-5/4/0/0-abc</p>"), chainSteps(t, dealID), cli.DealAudienceAdjudicator)
	require.Error(t, err, "digits inside a string are never exempt")
	_, err = invoke(t, "", "--profile", "deal", "disclose", "--deal", dealID, "--share", cli.DealAudienceCounterparty, "--to", "x", "--out", filepath.Join(t.TempDir(), "c.json"))
	require.Error(t, err, "the copy showing 5400 is refused rather than let the id through")
}

// Only exactly one JSON value has number values to exempt: a stream of
// several, or any other text, is mined whole.
func TestBlankShownNumbersNeedsOneJSONValue(t *testing.T) {
	shown := map[int64]bool{5400: true}
	assert.Equal(t, `{"amount":    }`, string(blankShownNumbers([]byte(`{"amount":5400}`), shown)))
	assert.Equal(t, `{"id":"ref-5400"}`, string(blankShownNumbers([]byte(`{"id":"ref-5400"}`), shown)))
	for _, text := range []string{"5400 5400", `{"a":5400} {"b":5400}`, "total 5400", "5400,"} {
		assert.Equal(t, text, string(blankShownNumbers([]byte(text), shown)), text)
	}
}

// A wrapper step whose type label would carry a private value is withheld
// whole from a copy: the copy shows its place in the log, never its type.
func TestWrappedObjectWithheldStepCarriesNoType(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	object := writeFile(t, "o.json", []byte(`{"order": "ZQ7341-PL"}`))
	note := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", object, "--object-type", "x-zq7341-pl")
	path := filepath.Join(t.TempDir(), "shared.json")
	_, err := invoke(t, "", "--profile", "deal", "disclose", "--deal", dealID, "--share", cli.DealAudienceCounterparty, "--to", "x", "--out", path)
	require.NoError(t, err)
	raw := string(mustRead(t, path))
	assert.NotContains(t, strings.ToLower(raw), "zq7341")
	assert.NotContains(t, raw, sha256Hex(mustRead(t, object)), "the withheld record's body is not in the copy")
	var shared map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &shared))
	assert.NotContains(t, shared["disclosures"], note["capsule_id"], "withheld, not disclosed")
	assert.Contains(t, raw, note["capsule_id"], "its place in the log is still proven")
	result, err := verifyWithDirectory(t, path, "")
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
}

// --input that gives only detail keeps the default about, for an object and
// for a digest alike.
func TestWrappedObjectDetailOnlyKeepsTheDefaultAbout(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	detail := writeJSON(t, `{"detail":"received from the courier"}`)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", vectorPath("delivery-proof.txt"), "--object-type", "delivery.proof", "--input", detail)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object-digest", strings.Repeat("ef", 32), "--object-type", "delivery.proof", "--input", detail)
	var abouts []string
	for _, se := range chainSteps(t, dealID) {
		if e := se.Event.Evidence; e != nil && e.Wrapped != nil {
			abouts = append(abouts, e.About)
			assert.Equal(t, "received from the courier", e.Detail)
		}
	}
	assert.Equal(t, []string{"held object", "held digest"}, abouts)
}

// verify-object without --deal takes no step, headers or output directory;
// headers given for a step that sealed none are reported as not checked.
func TestWrappedObjectVerifyFlagScope(t *testing.T) {
	dealFixture(t)
	object, digest := vectorPath("delivery-proof.txt"), sha256Hex(mustRead(t, vectorPath("delivery-proof.txt")))
	for _, extra := range [][]string{{"--step", "2"}, {"--headers", vectorPath("ucp-checkout-response.headers")}, {"--out", t.TempDir()}} {
		_, err := verifyObject(t, append([]string{"--object", object, "--digest", digest}, extra...)...)
		require.ErrorIs(t, err, cli.ErrInput, extra[0])
	}
	dealID := retailDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", object, "--object-type", "delivery.proof")
	res, err := verifyObject(t, "--deal", dealID, "--headers", vectorPath("ucp-checkout-response.headers"))
	require.NoError(t, err)
	got := res["objects"].([]any)[0].(map[string]any)
	assert.Equal(t, verifyNotPresent, got["carrier_headers"])
	assert.Contains(t, got["headers_given"], "sealed no carrier headers")
}

// A held object is sealed on a buyer's deal kept on a book: an older deal
// takes no new kind of step, and a seller's deal has its own legs.
func TestWrappedObjectOnlyOnABuyersBookDeal(t *testing.T) {
	args := []string{"--kind", "evidence", "--object", vectorPath("delivery-proof.txt"), "--object-type", "delivery.proof"}
	t.Run("legacy", func(t *testing.T) {
		dealFixture(t)
		legacyDeals(t)
		dealID := retailDeal(t)
		err := dealRunErr(t, append([]string{"note", "--deal", dealID}, args...)...)
		require.ErrorIs(t, err, cli.ErrInput)
		assert.Contains(t, err.Error(), "kept on a book")
	})
	t.Run("seller", func(t *testing.T) {
		dealFixture(t)
		dealID := dealRun(t, "open", "--records", "typed", "--input", writeJSON(t, sellerOpen))["deal_id"].(string)
		err := dealRunErr(t, append([]string{"note", "--deal", dealID}, args...)...)
		require.ErrorIs(t, err, cli.ErrInput)
		assert.Contains(t, err.Error(), "where the user buys")
	})
}

// A copy never carries the held octets or headers, and shows the wrapper
// record whole. The share gate, reading the reloaded steps, refuses a copy
// that quotes an id or a header value from the held object, which it would
// not refuse without the held object.
func TestWrappedObjectInCopies(t *testing.T) {
	object := writeFile(t, "o.json", []byte(`{"order": "ZQ7341-PL", "note": "kept"}`))
	headers := writeFile(t, "h", []byte("HTTP/1.1 200 OK\r\nRequest-Id: req-88412-xk\r\nX-Token: orchidwillow"))
	leaks := []string{"the order reference is ZQ7341-PL", "request req-88412-xk", "token orchidwillow"}

	dealFixture(t)
	dealID := retailDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", object, "--headers", headers, "--object-type", "x-example.order")
	for _, audience := range []string{cli.DealAudienceCounterparty, cli.DealAudienceAdjudicator} {
		_, raw := sharedCopy(t, dealID, audience, "x")
		assert.NotContains(t, raw, "ZQ7341", audience)
		assert.NotContains(t, raw, "req-88412", audience)
		assert.NotContains(t, raw, "orchidwillow", audience)
		assert.Contains(t, raw, sha256Hex([]byte(`{"order": "ZQ7341-PL", "note": "kept"}`)), audience+": the wrapper record is disclosed whole")
		assert.Contains(t, raw, `"x-example.order"`, audience)
		for _, leak := range leaks {
			err := dealShareGate([]byte("<p>"+leak+"</p>"), chainSteps(t, dealID), audience)
			require.Error(t, err, leak)
			assert.Contains(t, err.Error(), "refusing", leak)
		}
	}

	dealFixture(t)
	other := retailDeal(t)
	for _, leak := range leaks {
		assert.NoError(t, dealShareGate([]byte("<p>"+leak+"</p>"), chainSteps(t, other), cli.DealAudienceAdjudicator), "without the held object: "+leak)
	}
}

// The ceiling gate still holds: a held object may state a total equal to the
// user's limit, the counterparty's copy names the limit nowhere, and the
// gate still refuses a counterparty copy that writes it.
func TestWrappedObjectKeepsTheLimitFromTheCounterparty(t *testing.T) {
	dealFixture(t)
	open := strings.Replace(crossPartyBuyerOpen, `"price_minor":190000`, `"price_minor":150000`, 1)
	dealID := dealRun(t, "open", "--input", writeJSON(t, open))["deal_id"].(string)
	object := writeFile(t, "o.json", []byte(`{"total": "1900.00", "amount_minor": 190000}`))
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", object, "--object-type", "x-example.order")
	_, raw := sharedCopy(t, dealID, cli.DealAudienceCounterparty, "x")
	assert.NotContains(t, raw, "max_total_minor")
	assert.NotContains(t, raw, "1900.00")
	assert.NotContains(t, raw, `"total"`)
	err := dealCeilingGate([]byte("<p>at most 1900.00</p>"), chainSteps(t, dealID), cli.DealAudienceCounterparty)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spending limit")
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// The held bytes are stored in the step's deal_local opening: a changed
// byte there fails the store's integrity check and the deal's load, never
// reads as a wrapper mismatch.
func TestWrappedObjectStoreTamperFailsTheLoad(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", vectorPath("delivery-proof.txt"), "--object-type", "delivery.proof")
	db, _, err := cli.SqliteConnection(dealProfile(t))
	require.NoError(t, err)
	var id string
	var content []byte
	require.NoError(t, db.QueryRow(`SELECT capsule_id, content_bytes FROM capsule_store_artifacts WHERE name = ? AND instr(content_bytes, ?) > 0`,
		dealOpeningPrefix+dealLocalField, `\"wrapped\"`).Scan(&id, &content))
	content[len(content)/2] ^= 1
	_, err = db.Exec(`UPDATE capsule_store_artifacts SET content_bytes = ? WHERE capsule_id = ? AND name = ?`, content, id, dealOpeningPrefix+dealLocalField)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = verifyObject(t, "--deal", dealID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, cli.ErrObjectUnverified, "a store integrity failure, not a wrapper mismatch")
}

// A changed sealed record (its payload, which carries the wrapped digest)
// fails the deal's load, never reads as another digest.
func TestWrappedObjectRecordTamperFailsTheLoad(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	note := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", vectorPath("delivery-proof.txt"), "--object-type", "delivery.proof")
	db, _, err := cli.SqliteConnection(dealProfile(t))
	require.NoError(t, err)
	var content []byte
	require.NoError(t, db.QueryRow(`SELECT content_bytes FROM capsule_store_artifacts WHERE capsule_id = ? AND name = 'payload'`, note["capsule_id"]).Scan(&content))
	digest := note["wrapped"].(map[string]any)["digest"].(string)
	changed := bytes.Replace(content, []byte(digest), []byte(strings.Repeat("0", 64)), 1)
	require.NotEqual(t, content, changed)
	_, err = db.Exec(`UPDATE capsule_store_artifacts SET content_bytes = ? WHERE capsule_id = ? AND name = 'payload'`, changed, note["capsule_id"])
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = verifyObject(t, "--deal", dealID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, cli.ErrObjectUnverified)
}

// The held bytes are enough to run the objects' own checks later: what --out
// writes, given to evidencebook's ACP adapter with the sealed digest as the
// carried digest, and to its UCP adapter as the response held whole, passes
// the digest check and the RFC 9421 response signature.
func TestWrappedObjectFeedsTheAdapters(t *testing.T) {
	dealFixture(t)
	dealID := retailDeal(t)
	acpNote := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", vectorPath("acp-checkout-session-with-order.json"),
		"--object-type", "acp.checkout-session")
	ucpNote := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--object", vectorPath("ucp-checkout-response.body"),
		"--headers", vectorPath("ucp-checkout-response.headers"), "--object-type", "ucp.checkout", "--octets", "jcs")

	acpDir := t.TempDir()
	_, err := verifyObject(t, "--deal", dealID, "--step", jsonInt(int64(acpNote["step"].(float64))), "--out", acpDir)
	require.NoError(t, err)
	sealed := acpNote["wrapped"].(map[string]any)["digest"].(string)
	results := acp.Adapter{}.Check(mustRead(t, filepath.Join(acpDir, "object")), adapter.TrustPolicy{Pins: map[string]string{acp.PinCarriedDigest: sealed}})
	got, ok := adapter.Find(results, acp.PropDigest)
	require.True(t, ok)
	assert.Equal(t, adapter.Pass, got.Status)

	ucpDir := t.TempDir()
	_, err = verifyObject(t, "--deal", dealID, "--step", jsonInt(int64(ucpNote["step"].(float64))), "--out", ucpDir)
	require.NoError(t, err)
	message := append(append(mustRead(t, filepath.Join(ucpDir, "headers")), "\r\n\r\n"...), mustRead(t, filepath.Join(ucpDir, "object"))...)
	policy, err := adapter.LoadTrustPolicy(vectorPath("ucp-trust-policy.json"))
	require.NoError(t, err)
	got, ok = adapter.FindCheck(ucp.Adapter{}.Check(message, policy), ucp.PropResponseSignature)
	require.True(t, ok)
	assert.Equal(t, adapter.Pass, got.Status)
}
