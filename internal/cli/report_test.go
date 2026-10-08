package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sealedResultFixture = "testdata/result/result-root-bundle.sealed.json"

func TestIIFEIsPinned(t *testing.T) {
	sum := sha256.Sum256(evidenceGraphIIFE)
	assert.Equal(t, evidenceGraphIIFEDigest, hex.EncodeToString(sum[:]), "assets/evidence-graph.iife.js changed: pin the new digest in report.go and assets/README.md together")
	readme, err := os.ReadFile(filepath.Join("assets", "README.md"))
	require.NoError(t, err)
	assert.Contains(t, string(readme), evidenceGraphIIFEDigest, "assets/README.md records the same digest")
	text := string(evidenceGraphIIFE)
	assert.Contains(t, text, "globalThis.renderEvidenceGraph", "the IIFE exports the entry point the shell calls")
	assert.NotContains(t, text, "http://")
	assert.NotContains(t, text, "https://")
}

// The card set is the contract schema's own profile set, nothing invented.
func TestReportCardsAreTheContractProfiles(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "contract", "evidence-contract-v0.json"))
	require.NoError(t, err)
	var schemaRaw any
	require.NoError(t, json.Unmarshal(raw, &schemaRaw))
	assert.Equal(t, discriminatorValues(schemaRaw, "profile"), reportCards)
}

func runReportBuild(t *testing.T, bundlePath string, extra ...string) (reportBuildResult, string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "report.html")
	args := append([]string{"report", "build", "--bundle", bundlePath, "--out", out}, extra...)
	stdout, err := invoke(t, "", args...)
	var result reportBuildResult
	if err == nil {
		require.NoError(t, json.Unmarshal([]byte(stdout), &result))
	} else if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		// A refused page still reports the result, with page "not written".
		require.NoError(t, json.Unmarshal([]byte(stdout), &result))
	}
	return result, out, err
}

// signBundleRecords writes a copy of a bundle whose every record carries its
// producer signature inline (signature: the hex COSE_Sign1 envelope over its
// capsule_id; key_id: the signer's public key), the form `verify --bundle`
// checks. A book's bundle does not carry them yet, so it verifies INCOMPLETE
// (producer_signature_unclaimed); signed, it is VALID.
func signBundleRecords(t *testing.T, path string, key ed25519.PrivateKey) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	value, err := decodeBundleJSON(raw)
	require.NoError(t, err)
	identity, err := emit.NewEd25519SigningIdentity(key)
	require.NoError(t, err)
	public := hex.EncodeToString(key.Public().(ed25519.PublicKey))
	for _, r := range value["records"].([]interface{}) {
		record := r.(map[string]interface{})
		body, err := json.Marshal(record)
		require.NoError(t, err)
		envelope, err := emit.Sign(emit.BuiltPayload{CapsuleID: record["capsule_id"].(string), JSON: body}, identity)
		require.NoError(t, err)
		record["signature"], record["key_id"] = hex.EncodeToString(envelope), public
	}
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	signed := filepath.Join(t.TempDir(), "signed-bundle.json")
	require.NoError(t, os.WriteFile(signed, encoded, 0o600))
	return signed
}

// signedBookBundle is a book-form Result-root bundle, signed so that
// `verify --bundle` calls it VALID, and the unsigned bundle it came from.
func signedBookBundle(t *testing.T) (signed, unsigned string, root string) {
	t.Helper()
	b := newResultBook(t)
	built, _, err := runResultBuild(t, b.profile, resultDoc(testClaim("claim-1", "met", b.capsules[0]), testClaim("claim-2", "not_evaluable", b.capsules[1])))
	require.NoError(t, err)
	unsigned = filepath.Join(t.TempDir(), "bundle.json")
	_, err = invoke(t, "", "disclose", "--profile", b.profile.Name, "--root", built.RecordID, "--payloads", "selected", "--out", unsigned)
	require.NoError(t, err)
	signed = signBundleRecords(t, unsigned, b.key)
	result, err := verifyBundleOutput(t, signed)
	require.NoError(t, err)
	require.Equal(t, "VALID", result["verdict"])
	return signed, unsigned, built.RecordID
}

// embeddedBundle reads the bundle a report page embeds, as the shell does.
func reportEmbeddedBundle(t *testing.T, html string) map[string]interface{} {
	t.Helper()
	const open, close = "window.__BUNDLE__ = ", ";</script>"
	start := strings.Index(html, open)
	require.NotEqual(t, -1, start)
	rest := html[start+len(open):]
	end := strings.Index(rest, close)
	require.NotEqual(t, -1, end)
	value, err := decodeBundleJSON([]byte(rest[:end]))
	require.NoError(t, err)
	return value
}

func jcs(t *testing.T, value interface{}) string {
	t.Helper()
	encoded, err := canonical.JCS(value)
	require.NoError(t, err)
	return string(encoded)
}

// The payload-form fixture carries no checkpoint signature and no producer
// signatures: `verify --bundle` calls it INCOMPLETE, so no page is written
// (the viewer's banner would say it passed). The result is still reported,
// with the verdict, and nothing is on disk.
func TestReportBuildWritesNoPageForAnIncompleteBundle(t *testing.T) {
	result, out, err := runReportBuild(t, sealedResultFixture, "--card", "outcome", "--permalink")
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, 3, ExitCode(err))
	assert.Contains(t, SafeError(err), "no page written")
	assert.NoFileExists(t, out)
	assert.Equal(t, "not written", result.Page)
	assert.Equal(t, "INCOMPLETE", result.Verdict)
	assert.Equal(t, []string{"checkpoint withheld", "producer_signatures withheld"}, result.UnmetClaims)
	assert.Equal(t, "payload", result.Form)
	assert.Equal(t, "agent_input", result.Member)
	assert.Equal(t, 3, result.Claims)
	assert.Equal(t, []string{"ec:airline-week:2026-09-14@1"}, result.ContractRefs)
	assert.Empty(t, result.Report)
	assert.Empty(t, result.Permalink, "no page, no permalink")
}

// A book's bundle as it is built today carries no producer signatures:
// INCOMPLETE, no page. One whose record signature does not verify is
// INVALID: no page either, and the refusal says so.
func TestReportBuildWritesNoPageUnlessTheBundleIsValid(t *testing.T) {
	signed, unsigned, _ := signedBookBundle(t)
	result, out, err := runReportBuild(t, unsigned, "--card", "obligation")
	require.ErrorIs(t, err, ErrPartial)
	assert.NoFileExists(t, out)
	assert.Equal(t, "INCOMPLETE", result.Verdict)
	assert.Equal(t, []string{"producer_signatures withheld"}, result.UnmetClaims, "a book's bundle carries no producer signatures yet")

	raw, err := os.ReadFile(signed)
	require.NoError(t, err)
	value, err := decodeBundleJSON(raw)
	require.NoError(t, err)
	record := value["records"].([]interface{})[0].(map[string]interface{})
	sig := []byte(record["signature"].(string))
	if sig[len(sig)-1] == '0' {
		sig[len(sig)-1] = '1'
	} else {
		sig[len(sig)-1] = '0'
	}
	record["signature"] = string(sig)
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	tampered := filepath.Join(t.TempDir(), "tampered.json")
	require.NoError(t, os.WriteFile(tampered, encoded, 0o600))
	result, out, err = runReportBuild(t, tampered, "--card", "obligation")
	require.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, 1, ExitCode(err))
	assert.NoFileExists(t, out)
	assert.Equal(t, "INVALID", result.Verdict)
	assert.Equal(t, []string{"producer_signatures fail"}, result.UnmetClaims)
}

// The book form, end to end: result build -> disclose -> report build ->
// both verifiers over the written bundle.
func TestReportBuildRendersABookFormResultRoot(t *testing.T) {
	bundlePath, unsigned, rootID := signedBookBundle(t)
	verifyTestBundleFile(t, unsigned)

	presentation := writeTestJSON(t, "presentation.json", map[string]any{"producer_display_name": "Demo Co", "title": "Week 38"})
	result, out, err := runReportBuild(t, bundlePath, "--card", "obligation", "--presentation", presentation, "--permalink")
	require.NoError(t, err)
	assert.Equal(t, "written", result.Page)
	assert.Equal(t, "VALID", result.Verdict)
	assert.Equal(t, "book", result.Form)
	assert.Equal(t, "agent_input", result.Member)
	assert.Equal(t, rootID, result.Root)
	assert.Equal(t, 2, result.Claims)
	assert.Equal(t, 0, result.UnsupportedClaims, "a claim citing a published capsule resolves through the disclosed record header's subject")
	assert.Equal(t, []string{testContract}, result.ContractRefs)
	assert.NotEmpty(t, result.Permalink)

	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	html := string(raw)
	assert.Contains(t, html, `renderEvidenceGraph(window.__BUNDLE__, document.getElementById("app"))`)
	assert.Contains(t, html, "globalThis.renderEvidenceGraph", "the runtime is inline")
	assert.NotContains(t, html, "http://")
	assert.NotContains(t, html, "https://", "the page loads nothing from the network")
	embedded := reportEmbeddedBundle(t, html)
	digest, err := aacbundle.BundleDigest(embedded)
	require.NoError(t, err)
	assert.Equal(t, digest, result.BundleDigest)
	extensions, _ := embedded["extensions"].(map[string]interface{})
	assert.Equal(t, map[string]interface{}{"card": "obligation"}, extensions[cardExtension])
	assert.Equal(t, map[string]interface{}{"producer_display_name": "Demo Co", "title": "Week 38"}, extensions[presentationExtension])
	_, hasPayloads := extensions["evidencebook/payloads"]
	assert.True(t, hasPayloads, "the book's own extensions are kept")
	disclosures, _ := embedded["disclosures"].(map[string]interface{})
	header, _ := disclosures[rootID].(map[string]interface{})["agent_input"].(map[string]interface{})
	statement, _ := header["statement"].(map[string]interface{})
	assert.Equal(t, resultVersion, statement["result_version"], "the page embeds the record header whose statement is the Result")

	// The written bundle on disk is untouched by the report.
	onDisk, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	value, err := decodeBundleJSON(onDisk)
	require.NoError(t, err)
	_, decorated := value["extensions"].(map[string]interface{})[cardExtension]
	assert.False(t, decorated)

	// The permalink carries the same bytes the page embeds.
	decoded, err := aacbundle.DecodeFragment(strings.TrimPrefix(result.Permalink, defaultBundleURL+"#"))
	require.NoError(t, err)
	assert.Equal(t, jcs(t, embedded), jcs(t, decoded))

	// A second build must state the same card and header, or none.
	_, _, err = runReportBuild(t, bundlePath, "--card", "process")
	require.NoError(t, err, "the file on disk carries no card block")
	withCard := filepath.Join(t.TempDir(), "with-card.json")
	value["extensions"].(map[string]interface{})[cardExtension] = map[string]interface{}{"card": "outcome"}
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(withCard, encoded, 0o600))
	_, _, err = runReportBuild(t, withCard, "--card", "process")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "already carries a different report-card/v1 block")
	_, _, err = runReportBuild(t, withCard, "--card", "outcome")
	require.NoError(t, err)
}

func TestReportBuildDryRunWritesADraftAndNoPermalink(t *testing.T) {
	bundle, _, _ := signedBookBundle(t)
	result, out, err := runReportBuild(t, bundle, "--card", "human_role", "--permalink", "--dry-run")
	require.NoError(t, err)
	assert.True(t, result.Draft)
	assert.Empty(t, result.Permalink)
	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	extensions, _ := reportEmbeddedBundle(t, string(raw))["extensions"].(map[string]interface{})
	assert.Equal(t, map[string]interface{}{"card": "human_role", "draft": true}, extensions[cardExtension])
	stamped, _, err := runReportBuild(t, bundle, "--card", "human_role")
	require.NoError(t, err)
	assert.NotEqual(t, stamped.BundleDigest, result.BundleDigest, "a draft page never has a stamped page's digest")
}

func TestReportBuildRefusesATamperedBundle(t *testing.T) {
	b := newResultBook(t)
	built, _, err := runResultBuild(t, b.profile, resultDoc(testClaim("claim-1", "met", b.capsules[0])))
	require.NoError(t, err)
	bundlePath := filepath.Join(t.TempDir(), "bundle.json")
	_, err = invoke(t, "", "disclose", "--profile", b.profile.Name, "--root", built.RecordID, "--out", bundlePath)
	require.NoError(t, err)
	raw, err := os.ReadFile(bundlePath)
	require.NoError(t, err)

	tampered := func(t *testing.T, name string, mutate func(map[string]interface{})) string {
		t.Helper()
		value, err := decodeBundleJSON(raw)
		require.NoError(t, err)
		mutate(value)
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), name+".json")
		require.NoError(t, os.WriteFile(path, encoded, 0o600))
		return path
	}
	cases := map[string]func(map[string]interface{}){
		"a verdict edited inside the disclosed Result": func(v map[string]interface{}) {
			header := v["disclosures"].(map[string]interface{})[built.RecordID].(map[string]interface{})["agent_input"].(map[string]interface{})
			header["statement"].(map[string]interface{})["claims"].([]interface{})[0].(map[string]interface{})["verdict"] = "not_met"
		},
		"the checkpoint size edited": func(v map[string]interface{}) {
			v["checkpoint"].(map[string]interface{})["mmr_size"] = json.Number("99")
		},
		"a record dropped": func(v map[string]interface{}) {
			records := v["records"].([]interface{})
			v["records"] = records[1:]
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, out, err := runReportBuild(t, tampered(t, "tampered", mutate), "--card", "outcome")
			require.ErrorIs(t, err, ErrInput)
			assert.Equal(t, 2, ExitCode(err))
			assert.Contains(t, SafeError(err), "does not verify")
			assert.Contains(t, SafeError(err), "nothing was rendered")
			assert.NoFileExists(t, out)
		})
	}
}

func TestReportBuildRefusesANonResultRoot(t *testing.T) {
	b := newResultBook(t)
	p := b.profile
	disclosed := filepath.Join(t.TempDir(), "published.json")
	_, err := invoke(t, "", "disclose", "--profile", p.Name, "--root", b.capsules[0], "--out", disclosed)
	require.NoError(t, err)
	_, out, err := runReportBuild(t, disclosed, "--card", "outcome")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), `is not a sealed Evidence Result v0: its agent_input is a book record header of record_type "published_capsule", not "evidence_result"`)
	assert.NoFileExists(t, out)

	undisclosed := filepath.Join(t.TempDir(), "bundle.json")
	_, err = invoke(t, "", "bundle", "--profile", p.Name, "--root", b.capsules[0], "--out", undisclosed)
	require.NoError(t, err)
	_, _, err = runReportBuild(t, undisclosed, "--card", "outcome")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "the bundle discloses nothing for it (build the bundle with disclose, not bundle)")

	closeID := closeFixture(t, b)
	closeBundle := filepath.Join(t.TempDir(), "close-bundle.json")
	_, err = invoke(t, "", "disclose", "--profile", p.Name, "--root", closeID, "--payloads", "selected", "--out", closeBundle)
	require.NoError(t, err)
	_, _, err = runReportBuild(t, closeBundle, "--card", "outcome")
	assert.Contains(t, SafeError(err), `record header of record_type "close"`)
}

func TestResultRootOfNamesWhatItFound(t *testing.T) {
	root := strings.Repeat("1", 64)
	bundle := func(members map[string]interface{}) map[string]interface{} {
		value := map[string]interface{}{"root": root, "records": []interface{}{map[string]interface{}{"capsule_id": root}}, "disclosures": map[string]interface{}{}}
		if members != nil {
			value["disclosures"].(map[string]interface{})[root] = members
		}
		return value
	}
	_, err := resultRootOf(bundle(map[string]interface{}{"agent_input": map[string]interface{}{"spec_version": "evaluation-summary/v1"}}))
	assert.Contains(t, SafeError(err), "its agent_input is a evaluation-summary/v1 document")
	_, err = resultRootOf(bundle(map[string]interface{}{"agent_output": map[string]interface{}{"spec_version": "report/v1"}}))
	assert.Contains(t, SafeError(err), "its agent_output is a report/v1 document")
	_, err = resultRootOf(bundle(map[string]interface{}{"agent_input": map[string]interface{}{"hello": "world"}}))
	assert.Contains(t, SafeError(err), "no disclosed member carries an evidence-result-v0 document or an evidence_result record header")
	_, err = resultRootOf(bundle(map[string]interface{}{"agent_input": map[string]interface{}{"record_type": resultRecordType}}))
	assert.Contains(t, SafeError(err), "header carries no statement object")
	_, err = resultRootOf(map[string]interface{}{"root": root, "records": []interface{}{}})
	assert.Contains(t, SafeError(err), "is not among the bundle's records")
	found, err := resultRootOf(bundle(map[string]interface{}{"agent_output": map[string]interface{}{"result_version": resultVersion}, "agent_input": map[string]interface{}{"record_type": recordTypePublished}}))
	require.NoError(t, err)
	assert.Equal(t, "agent_output", found.member)
	assert.Equal(t, "payload", found.form)

	// Exactly one Result: a root carrying one in both members, or any other
	// disclosed record carrying one, is refused with both named.
	_, err = resultRootOf(bundle(map[string]interface{}{"agent_output": map[string]interface{}{"result_version": resultVersion}, "agent_input": map[string]interface{}{"record_type": resultRecordType, "statement": map[string]interface{}{}}}))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "root "+root+" carries a Result in both its agent_output and its agent_input; a report is rooted on exactly one Result document")
	other := strings.Repeat("2", 64)
	two := bundle(map[string]interface{}{"agent_input": map[string]interface{}{"record_type": resultRecordType, "statement": map[string]interface{}{}}})
	two["records"] = append(two["records"].([]interface{}), map[string]interface{}{"capsule_id": other})
	two["disclosures"].(map[string]interface{})[other] = map[string]interface{}{"agent_output": map[string]interface{}{"result_version": resultVersion}}
	_, err = resultRootOf(two)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "the bundle carries two Result documents: root "+root+" and record "+other+" (its agent_output); a report is rooted on exactly one Result")
	two["disclosures"].(map[string]interface{})[other] = map[string]interface{}{"agent_input": map[string]interface{}{"record_type": recordTypePublished, "statement": map[string]interface{}{"result_version": resultVersion}}}
	_, err = resultRootOf(two)
	require.NoError(t, err, "a non-Result record whose statement merely mentions a result_version is not a Result")
}

// End to end: a Result that cites an earlier Result record pulls it into the
// disclosed closure, and the report refuses to choose between the two.
func TestReportBuildRefusesABundleCarryingTwoResults(t *testing.T) {
	b := newResultBook(t)
	p := b.profile
	prior, _, err := runResultBuild(t, p, resultDoc(testClaim("claim-1", "met", b.capsules[0])))
	require.NoError(t, err)
	latest, _, err := runResultBuild(t, p, resultDoc(testClaim("claim-1", "met", b.capsules[0]), testClaim("claim-2", "met", prior.RecordID)))
	require.NoError(t, err)
	assert.Contains(t, latest.Cites, prior.RecordID)
	bundlePath := filepath.Join(t.TempDir(), "bundle.json")
	_, err = invoke(t, "", "disclose", "--profile", p.Name, "--root", latest.RecordID, "--payloads", "selected", "--out", bundlePath)
	require.NoError(t, err)
	verifyTestBundleFile(t, bundlePath)
	_, out, err := runReportBuild(t, bundlePath, "--card", "outcome")
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, 2, ExitCode(err))
	assert.Contains(t, SafeError(err), "the bundle carries two Result documents: root "+latest.RecordID+" and record "+prior.RecordID+" (its agent_input)")
	assert.Contains(t, SafeError(err), "nothing was rendered")
	assert.NoFileExists(t, out)
}

func TestReportBuildRefusesABadCardOrPresentation(t *testing.T) {
	_, out, err := runReportBuild(t, sealedResultFixture, "--card", "vibes")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), `--card must be one of attribution, human_role, obligation, outcome, process, quality, settlement (the contract's profile set); got "vibes"`)
	assert.NoFileExists(t, out)
	_, _, err = runReportBuild(t, sealedResultFixture)
	assert.Contains(t, SafeError(err), "--card must be one of")

	cases := map[string]struct {
		block any
		want  string
	}{
		"an extra member":   {map[string]any{"title": "x", "badge": "verified"}, "only producer_display_name, logo_data_url and title"},
		"a remote logo":     {map[string]any{"logo_data_url": "https://example.invalid/logo.png"}, "must be an inline data:image/ URL"},
		"a data: non-image": {map[string]any{"logo_data_url": "data:text/html,hi"}, "must be an inline data:image/ URL"},
		"nothing named":     {map[string]any{}, "names none of"},
		"a non-string":      {map[string]any{"title": 7}, "only producer_display_name, logo_data_url and title"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, out, err := runReportBuild(t, sealedResultFixture, "--card", "outcome", "--presentation", writeTestJSON(t, "p.json", tc.block))
			require.ErrorIs(t, err, ErrInput)
			assert.Contains(t, SafeError(err), tc.want)
			assert.NoFileExists(t, out)
		})
	}
	valid, _, _ := signedBookBundle(t)
	_, _, err = runReportBuild(t, valid, "--card", "outcome", "--presentation", writeTestJSON(t, "p.json", map[string]any{"logo_data_url": "data:image/png;base64,AAAA"}))
	require.NoError(t, err)

	_, err = invoke(t, "", "report", "build", "--card", "outcome")
	require.ErrorIs(t, err, ErrInput)
	_, _, err = runReportBuild(t, filepath.Join(t.TempDir(), "missing.json"), "--card", "outcome")
	assert.Contains(t, SafeError(err), "input file not found")
}

// graph_closure withheld (the bundle declares records of its closure
// missing) is accepted, and said so in the output; fail is refused, as is
// anything but pass on interval coverage or per-record membership. A sealed
// fixture cannot be withheld and still pass membership (its certificate
// covers every record), so the acceptance rule is pinned on the verdicts.
func TestHeldBundleAcceptanceNamesAWithheldClosure(t *testing.T) {
	verdict := func(graph, interval, membership string) aacbundle.VerificationResult {
		return aacbundle.VerificationResult{GraphClosure: aacbundle.ClaimResult{Status: graph}, IntervalCoverage: aacbundle.ClaimResult{Status: interval}, PerRecordMembership: aacbundle.ClaimResult{Status: membership}}
	}
	label, err := acceptHeldVerification(verdict("pass", "pass", "pass"))
	require.NoError(t, err)
	assert.Equal(t, "pass", label)
	label, err = acceptHeldVerification(verdict("withheld", "pass", "pass"))
	require.NoError(t, err)
	assert.Equal(t, "graph_closure withheld", label)
	for _, v := range []aacbundle.VerificationResult{verdict("fail", "pass", "pass"), verdict("pass", "withheld", "pass"), verdict("pass", "pass", "fail"), verdict("withheld", "pass", "withheld")} {
		_, err = acceptHeldVerification(v)
		require.ErrorIs(t, err, ErrInput, "%+v", v)
		assert.Contains(t, SafeError(err), "nothing was rendered")
	}
}

// --out is create-only: an existing file is refused in one sentence and
// left alone, and the digest is computed before anything is written.
func TestReportBuildRefusesAnExistingOut(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.html")
	require.NoError(t, os.WriteFile(out, []byte("keep"), 0o600))
	valid, _, _ := signedBookBundle(t)
	_, err := invoke(t, "", "report", "build", "--bundle", valid, "--card", "outcome", "--out", out)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--out "+out+" already exists")
	kept, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(kept))
}
