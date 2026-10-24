package plugin

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/cli"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statementWordsBanned are words no statement output may use: v0's verifier
// is the holder's own tool, and a statement is never a verified receipt.
var statementWordsBanned = []string{"independent", "third-party", "third party", "verified receipt"}

func merchantDNS(t *testing.T) func() {
	t.Helper()
	return stubDNS(t, map[string]string{merchantSelector + "._domainkey.shop.example": merchantKeyTXT(t), dmarcName: merchantDMARC(t)})
}

// issueStatement runs `deal verifier-statement` and returns its result, the
// statement file's bytes and the file's path.
func issueStatement(t *testing.T, dealID string, args ...string) (map[string]any, []byte, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "statement.json")
	res := dealRun(t, append([]string{"verifier-statement", "--deal", dealID, "--out", out}, args...)...)
	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	return res, raw, out
}

func verifyStatementFile(t *testing.T, path string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", "deal", "verify-statement", path)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m, err
}

func assertStatementWording(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, w := range statementWordsBanned {
			assert.NotContains(t, strings.ToLower(text), w)
		}
	}
}

// Test hooks 1, 5 (signature half) and 8, and the offline check: a passing
// email whose signature covers Date gets a statement with message_date; its
// email_digest is the message_digest the deal record already seals; it carries
// no header value; verify-statement checks it with no network and says it is
// V's statement, checked by the holder's own tool, and nothing more.
func TestVerifierStatementIssuedOverThePassingSealedEmail(t *testing.T) {
	dealFixture(t)
	offline := merchantDNS(t)
	dealID := openMerchantDeal(t)
	email := filepath.Join(merchantFixture, "confirmation.eml")
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", email)
	offline()

	res, raw, path := issueStatement(t, dealID)
	var statement map[string]any
	require.NoError(t, json.Unmarshal(raw, &statement))
	assert.Equal(t, "dkim-verification", statement["statement_type"])
	assert.Equal(t, "shop.example", statement["dkim_domain"])
	assert.Equal(t, "2026-10-03T21:14:05Z", statement["message_date"], "the fixture's signature covers Date")
	assert.Equal(t, "dns", statement["key_source"])
	assert.Equal(t, "2026-09-27T18:00:00Z", statement["checked_at"])
	assert.NotContains(t, statement, "key_resolver")

	// email_digest is the sealed record's own message_digest, named with its
	// algorithm: no new value is minted.
	var sealed map[string]any
	for _, r := range chainRecords(t, dealID) {
		if m, ok := r["body"].(map[string]any)["merchant_email"].(map[string]any); ok {
			sealed = m
		}
	}
	require.NotNil(t, sealed)
	assert.Equal(t, map[string]any{"alg": "sha-256", "value": sealed["message_digest"]}, statement["email_digest"])
	assert.Equal(t, sealed["key_records_digest"], statement["key_records_digest"])

	// The verifier is the holder's own signing key.
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	s, err := openDealSession(t.Context(), p)
	require.NoError(t, err)
	holder := hex.EncodeToString(s.key.Public().(ed25519.PublicKey))
	require.NoError(t, s.close())
	assert.Equal(t, map[string]any{"key_id": holder, "public_key": holder}, statement["verifier"])

	// No header value, selector or signed-header list travels.
	for _, v := range []string{merchantSelector, "s=", "h=", "From", "To:", "Subject", "orders@shop.example", "sam.customer@mail.example", "Sam Customer", "SE-104233", "DKIM-Signature"} {
		assert.NotContains(t, string(raw), v)
	}

	want := "V's statement: DKIM signature from shop.example verified over a message dated 2026-10-03T21:14:05Z, checked by the holder's own tool (key " + holder + ") at 2026-09-27T18:00:00Z."
	assert.Equal(t, want, res["says"])
	v, err := verifyStatementFile(t, path)
	require.NoError(t, err)
	assert.Equal(t, true, v["verified"])
	assert.Equal(t, want, v["says"])
	assert.Equal(t, false, v["dkim_rechecked"])
	assert.Contains(t, v["note"], "not re-checked")
	out, _ := json.Marshal([]any{res, v})
	assertStatementWording(t, string(out))
}

// Test hook 2: a passing signature whose h= does not cover Date gets a
// statement with no message_date, and the words say the date is not covered.
func TestVerifierStatementLeavesOutADateTheSignatureDoesNotCover(t *testing.T) {
	dealFixture(t)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	msg := simpleMessage("orders@shop.example", "Sat, 03 Oct 2026 21:14:05 +0000", "<nodate@shop.example>", "Your order SE-1 is confirmed", "Thanks for your order!\r\n")
	var signed bytes.Buffer
	require.NoError(t, dkim.Sign(&signed, strings.NewReader(msg), &dkim.SignOptions{
		Domain: "shop.example", Selector: "nodate", Signer: key, Hash: crypto.SHA256,
		HeaderKeys: []string{"From", "To", "Subject", "Message-ID"},
	}))
	email := filepath.Join(t.TempDir(), "nodate.eml")
	require.NoError(t, os.WriteFile(email, signed.Bytes(), 0o600))
	offline := stubDNS(t, map[string]string{"nodate._domainkey.shop.example": "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub), dmarcName: merchantDMARC(t)})
	dealID := openMerchantDeal(t)
	sealed := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", email)
	require.Equal(t, "pass", sealed["dkim"])
	offline()

	res, raw, path := issueStatement(t, dealID)
	var statement map[string]any
	require.NoError(t, json.Unmarshal(raw, &statement))
	assert.NotContains(t, statement, "message_date")
	assert.Contains(t, res["says"], "over a message (date not covered by the signature),")
	v, err := verifyStatementFile(t, path)
	require.NoError(t, err)
	assert.Equal(t, res["says"], v["says"])
	assert.NotContains(t, string(raw), "nodate", "the selector does not travel")
}

// Test hooks 3 and 4: no statement over an email that fails or carries no
// signature, and none over a key supplied by hand.
func TestVerifierStatementRefusedWithoutAPassingDNSKeyCheck(t *testing.T) {
	dealFixture(t)
	merchantDNS(t)
	b, err := os.ReadFile(filepath.Join(merchantFixture, "confirmation.eml"))
	require.NoError(t, err)
	dir := t.TempDir()
	altered := filepath.Join(dir, "altered.eml")
	require.NoError(t, os.WriteFile(altered, bytes.Replace(b, []byte("Order total: $64.00"), []byte("Order total: $6.40"), 1), 0o600))
	unsigned := filepath.Join(dir, "unsigned.eml")
	require.NoError(t, os.WriteFile(unsigned, []byte(simpleMessage("orders@shop.example", "Sat, 03 Oct 2026 21:14:05 +0000", "<u@shop.example>", "Order", "Thanks\r\n")), 0o600))

	for _, c := range []struct {
		name, verdict string
		args          []string
	}{
		{"fail", "fail", []string{"--email", altered}},
		{"none", "none", []string{"--email", unsigned}},
		{"supplied", "pass", []string{"--email", filepath.Join(merchantFixture, "confirmation.eml"), "--key-record", filepath.Join(merchantFixture, "key-record.txt")}},
	} {
		dealID := openMerchantDeal(t)
		sealed := dealRun(t, append([]string{"note", "--deal", dealID, "--kind", "evidence"}, c.args...)...)
		require.Equal(t, c.verdict, sealed["dkim"], c.name)
		out := filepath.Join(dir, c.name+".json")
		err := dealRunErr(t, "verifier-statement", "--deal", dealID, "--out", out)
		require.Error(t, err, c.name)
		assert.Contains(t, cli.SafeError(err), "no statement", c.name)
		assert.NoFileExists(t, out, c.name)
	}
}

// The statement is issued from a re-check of the full sealed bytes, never from
// the verdict sealed earlier: a store whose sealed verdict says pass over bytes
// that no longer check out gets no statement.
func TestVerifierStatementRechecksTheFullSealedEmail(t *testing.T) {
	merchantDNS(t)
	b, err := os.ReadFile(filepath.Join(merchantFixture, "confirmation.eml"))
	require.NoError(t, err)
	e, err := captureEmail(b, nil)
	require.NoError(t, err)
	require.Equal(t, "pass", e.DKIM.Result)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, err = issueVerifierStatement(e, key, time.Now())
	require.NoError(t, err)

	// The body changed; the headers, and the sealed verdict, did not.
	e.Raw = bytes.Replace(b, []byte("Order total: $64.00"), []byte("Order total: $6.40"), 1)
	_, err = issueVerifierStatement(e, key, time.Now())
	require.Error(t, err)
	assert.Contains(t, cli.SafeError(err), "does not check out")
}

// Test hooks 5 and 6: one changed byte in any member fails verify-statement,
// and so does a signature by the same key over the same JCS without the domain
// tag, or over its digest as a capsule's is.
func TestVerifyStatementRejectsChangedMembersAndUntaggedSignatures(t *testing.T) {
	merchantDNS(t)
	b, err := os.ReadFile(filepath.Join(merchantFixture, "confirmation.eml"))
	require.NoError(t, err)
	e, err := captureEmail(b, nil)
	require.NoError(t, err)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	statement, err := issueVerifierStatement(e, key, time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	good, err := json.Marshal(statement)
	require.NoError(t, err)
	_, err = verifyStatement(good)
	require.NoError(t, err)

	// Each member changed to another value the schema accepts: only the
	// signature can catch it. A member the schema fixes (statement_type,
	// key_source, email_digest.alg) is caught by the schema.
	change := func(edit func(map[string]any)) []byte {
		var m map[string]any
		require.NoError(t, json.Unmarshal(good, &m))
		edit(m)
		raw, err := json.Marshal(m)
		require.NoError(t, err)
		return raw
	}
	flipHex := func(s string) string {
		if s[len(s)-1] == '0' {
			return s[:len(s)-1] + "1"
		}
		return s[:len(s)-1] + "0"
	}
	for name, edit := range map[string]func(map[string]any){
		"dkim_domain":     func(m map[string]any) { m["dkim_domain"] = "other.example" },
		"message_date":    func(m map[string]any) { m["message_date"] = "2026-10-03T21:14:06Z" },
		"no message_date": func(m map[string]any) { delete(m, "message_date") },
		"email_digest": func(m map[string]any) {
			d := m["email_digest"].(map[string]any)
			d["value"] = flipHex(d["value"].(string))
		},
		"key_records_digest": func(m map[string]any) { m["key_records_digest"] = flipHex(m["key_records_digest"].(string)) },
		"checked_at":         func(m map[string]any) { m["checked_at"] = "2026-09-27T18:00:01Z" },
		"signature":          func(m map[string]any) { m["signature"] = flipHex(m["signature"].(string)) },
	} {
		_, err := verifyStatement(change(edit))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "signature does not verify", name)
	}
	for name, edit := range map[string]func(map[string]any){
		"statement_type":   func(m map[string]any) { m["statement_type"] = "dkim-verificatio0" },
		"key_source":       func(m map[string]any) { m["key_source"] = "supplied" },
		"email_digest.alg": func(m map[string]any) { m["email_digest"].(map[string]any)["alg"] = "sha-512" },
	} {
		_, err := verifyStatement(change(edit))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "not an x-deal-v0 verifier_statement", name)
	}
	// Both key members changed together, to another key: the signature is
	// still the old key's.
	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(good, &m))
	m["verifier"] = map[string]any{"key_id": hex.EncodeToString(other), "public_key": hex.EncodeToString(other)}
	swapped, err := json.Marshal(m)
	require.NoError(t, err)
	_, err = verifyStatement(swapped)
	assert.Error(t, err, "another verifier key")

	// The same key's signature over the same JCS, untagged, and over its
	// SHA-256 hex digest (as a capsule's is signed): neither is a statement.
	unsigned := map[string]any{}
	for k, v := range statement {
		if k != "signature" {
			unsigned[k] = v
		}
	}
	jcs, err := canonical.JCS(unsigned)
	require.NoError(t, err)
	sum := sha256.Sum256(jcs)
	for name, signed := range map[string][]byte{"untagged JCS": jcs, "capsule-style digest": []byte(hex.EncodeToString(sum[:]))} {
		forged := map[string]any{}
		for k, v := range statement {
			forged[k] = v
		}
		forged["signature"] = hex.EncodeToString(ed25519.Sign(key, signed))
		raw, err := json.Marshal(forged)
		require.NoError(t, err)
		_, err = verifyStatement(raw)
		assert.Error(t, err, name)
	}

	// Signed correctly by the key it names, and still refused: a member the
	// schema does not know (a header value), and a key_id that is not the
	// public key.
	resign := func(change func(map[string]any)) []byte {
		var m map[string]any
		require.NoError(t, json.Unmarshal(good, &m))
		change(m)
		input, err := statementSigningInput(m)
		require.NoError(t, err)
		m["signature"] = hex.EncodeToString(ed25519.Sign(key, input))
		raw, err := json.Marshal(m)
		require.NoError(t, err)
		return raw
	}
	_, err = verifyStatement(resign(func(m map[string]any) { m["selector"] = merchantSelector }))
	assert.Error(t, err, "an unknown member")
	_, err = verifyStatement(resign(func(m map[string]any) {
		m["verifier"].(map[string]any)["key_id"] = hex.EncodeToString(other)
	}))
	assert.Error(t, err, "a key_id that is not the public key")
}

// verify-statement fails with its own exit, says why, and still never claims
// more than V's statement.
func TestVerifyStatementCommandOnAChangedStatement(t *testing.T) {
	dealFixture(t)
	offline := merchantDNS(t)
	dealID := openMerchantDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	offline()
	_, raw, path := issueStatement(t, dealID)
	require.NoError(t, os.WriteFile(path, bytes.Replace(raw, []byte(`"shop.example"`), []byte(`"shop.exampl3"`), 1), 0o600))
	v, err := verifyStatementFile(t, path)
	require.ErrorIs(t, err, cli.ErrStatementInvalid)
	assert.Equal(t, false, v["verified"])
	assert.Contains(t, v["reason"], "signature does not verify")
	out, _ := json.Marshal(v)
	assertStatementWording(t, string(out))
}

// More than one sealed email: --step picks one.
func TestVerifierStatementNeedsStepWithTwoEmails(t *testing.T) {
	dealFixture(t)
	merchantDNS(t)
	dealID := openMerchantDeal(t)
	first := dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation-2.eml"))
	err := dealRunErr(t, "verifier-statement", "--deal", dealID, "--out", filepath.Join(t.TempDir(), "s.json"))
	require.Error(t, err)
	assert.Contains(t, cli.SafeError(err), "--step")
	_, raw, _ := issueStatement(t, dealID, "--step", strconv.Itoa(int(first["step"].(float64))))
	assert.Contains(t, string(raw), "2026-10-03T21:14:05Z", "the first email's date")
}

// The covered Date is stated only when it is the email's one Date header: DKIM
// signs the bottom-most, a reader takes the first. A repeated or unreadable
// covered Date gets no statement.
func TestVerifierStatementRefusesAnAmbiguousSignedDate(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	stubDNS(t, map[string]string{"dated._domainkey.shop.example": "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub), dmarcName: merchantDMARC(t)})
	sign := func(msg string) []byte {
		var signed bytes.Buffer
		require.NoError(t, dkim.Sign(&signed, strings.NewReader(msg), &dkim.SignOptions{
			Domain: "shop.example", Selector: "dated", Signer: key, Hash: crypto.SHA256,
			HeaderKeys: []string{"From", "To", "Subject", "Date", "Message-ID"},
		}))
		return signed.Bytes()
	}
	_, holder, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	for name, raw := range map[string][]byte{
		"an unsigned Date above the signed one": append([]byte("Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n"), sign(simpleMessage("orders@shop.example", "Sat, 03 Oct 2026 21:14:05 +0000", "<d1@shop.example>", "Order", "Thanks\r\n"))...),
		"an unreadable signed Date":             sign(simpleMessage("orders@shop.example", "the third of October", "<d2@shop.example>", "Order", "Thanks\r\n")),
	} {
		e, err := captureEmail(raw, nil)
		require.NoError(t, err, name)
		require.Equal(t, "pass", e.DKIM.Result, name)
		_, err = issueVerifierStatement(e, holder, time.Now())
		require.Error(t, err, name)
		assert.Contains(t, cli.SafeError(err), "Date header", name)
	}
}

// The date follows the signature the statement rests on, the passing one
// aligned with the sender, wherever it sits: another signature on the same
// email that covers Date, one that fails or one from another domain that
// passes, lends the statement no message_date, above or below the aligned one.
func TestVerifierStatementTakesTheDateFromTheSignatureItRestsOn(t *testing.T) {
	shopPub, shopKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	stalePub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, staleKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	relayPub, relayKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	txt := func(pub ed25519.PublicKey) string {
		return "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)
	}
	stubDNS(t, map[string]string{
		"aligned._domainkey.shop.example": txt(shopPub),
		// The stale selector publishes another key than the one that signs.
		"stale._domainkey.shop.example":  txt(stalePub),
		"relay._domainkey.relay.example": txt(relayPub),
		dmarcName:                        merchantDMARC(t),
	})
	sign := func(msg []byte, domain, selector string, key ed25519.PrivateKey, headers ...string) []byte {
		var signed bytes.Buffer
		require.NoError(t, dkim.Sign(&signed, bytes.NewReader(msg), &dkim.SignOptions{
			Domain: domain, Selector: selector, Signer: key, Hash: crypto.SHA256, HeaderKeys: headers,
		}))
		return signed.Bytes()
	}
	aligned := func(msg []byte) []byte {
		return sign(msg, "shop.example", "aligned", shopKey, "From", "To", "Subject", "Message-ID")
	}
	others := map[string]struct {
		result string
		sign   func([]byte) []byte
	}{
		"a failing signature from the same domain": {"fail", func(msg []byte) []byte {
			return sign(msg, "shop.example", "stale", staleKey, "From", "To", "Subject", "Date", "Message-ID")
		}},
		"a passing signature from another domain": {"pass", func(msg []byte) []byte {
			return sign(msg, "relay.example", "relay", relayKey, "From", "To", "Subject", "Date", "Message-ID")
		}},
	}
	_, holder, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	for name, other := range others {
		msg := []byte(simpleMessage("orders@shop.example", "Sat, 03 Oct 2026 21:14:05 +0000", "<two@shop.example>", "Your order SE-1 is confirmed", "Thanks for your order!\r\n"))
		// A signature is added above the headers it signs, so the one signed
		// last is the first DKIM-Signature header.
		for order, raw := range map[string][]byte{
			"above the aligned one": other.sign(aligned(msg)),
			"below the aligned one": aligned(other.sign(msg)),
		} {
			label := name + " " + order
			e, err := captureEmail(raw, nil)
			require.NoError(t, err, label)
			require.Equal(t, "pass", e.DKIM.Result, label)
			require.Len(t, e.DKIM.Signatures, 2, label)
			first := 0
			if order == "below the aligned one" {
				first = 1
			}
			require.Equal(t, other.result, e.DKIM.Signatures[first].Result, label)
			require.True(t, dateSigned(e.DKIM.Signatures[first].Signed), label)
			require.False(t, dateSigned(e.DKIM.Signatures[1-first].Signed), label)

			statement, err := issueVerifierStatement(e, holder, time.Now())
			require.NoError(t, err, label)
			assert.Equal(t, "shop.example", statement["dkim_domain"], label)
			assert.NotContains(t, statement, "message_date", label)
		}
	}
}

// verify-statement --key matches the statement's key to the holder's; without
// it the result says the key is not matched to any holder.
func TestVerifyStatementKeyFlag(t *testing.T) {
	dealFixture(t)
	offline := merchantDNS(t)
	dealID := openMerchantDeal(t)
	dealRun(t, "note", "--deal", dealID, "--kind", "evidence", "--email", filepath.Join(merchantFixture, "confirmation.eml"))
	offline()
	_, raw, path := issueStatement(t, dealID)
	var statement map[string]any
	require.NoError(t, json.Unmarshal(raw, &statement))
	holder := statement["verifier"].(map[string]any)["key_id"].(string)

	v, err := verifyStatementFile(t, path)
	require.NoError(t, err)
	assert.Equal(t, verifierKeyUnmatched, v["key"])
	out, err := invoke(t, "", "deal", "verify-statement", path, "--key", strings.ToUpper(holder))
	require.NoError(t, err, out)
	assert.Contains(t, out, `"key":"matches --key"`)
	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	out, err = invoke(t, "", "deal", "verify-statement", path, "--key", hex.EncodeToString(other))
	require.ErrorIs(t, err, cli.ErrStatementInvalid)
	assert.Contains(t, out, "signed by another key than --key")
	assert.NotContains(t, out, `"says"`)
}

// A pinned vector for another implementation: a fixed key and statement give
// these signing-input and signature bytes. The input begins with the domain
// tag and 0x00; a capsule's or a countersignature's signed bytes never do.
func TestVerifierStatementSigningVector(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x01}, ed25519.SeedSize))
	public := hex.EncodeToString(key.Public().(ed25519.PublicKey))
	statement := map[string]interface{}{
		"statement_type": "dkim-verification", "dkim_domain": "shop.example", "message_date": "2026-10-03T21:14:05Z",
		"email_digest":       map[string]interface{}{"alg": "sha-256", "value": strings.Repeat("ab", 32)},
		"key_records_digest": strings.Repeat("cd", 32), "key_source": "dns", "checked_at": "2026-09-27T18:00:00Z",
		"verifier": map[string]interface{}{"key_id": public, "public_key": public},
	}
	input, err := statementSigningInput(statement)
	require.NoError(t, err)
	want := "x-deal-v0/verifier-statement/v1\x00" +
		`{"checked_at":"2026-09-27T18:00:00Z","dkim_domain":"shop.example","email_digest":{"alg":"sha-256","value":"` + strings.Repeat("ab", 32) +
		`"},"key_records_digest":"` + strings.Repeat("cd", 32) + `","key_source":"dns","message_date":"2026-10-03T21:14:05Z","statement_type":"dkim-verification","verifier":{"key_id":"` + public + `","public_key":"` + public + `"}}`
	assert.Equal(t, want, string(input))
	assert.Equal(t, signingVectorSignature, hex.EncodeToString(ed25519.Sign(key, input)))
}

// signingVectorSignature is the Ed25519 signature, under the seed 0x01 * 32, of
// TestVerifierStatementSigningVector's signing input, cross-checked with
// Python's cryptography Ed25519 over its own sorted-key JSON (2026-10-13).
const signingVectorSignature = "22ebc86cb767bf18d16231705342b2cfddbc97f650c21a00805d00e1c1c4dfdf69dc897c757745c41f3338079dec694f09cdbe3d773345b27df69100a1430a03"
