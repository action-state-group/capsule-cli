package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/evidencebook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testContract = "ec:airline-week:2026-09-14@1"

// resultBook is a jsonl profile whose book holds two published capsules
// (the "daily reports" a Result cites), committed at day0.
type resultBook struct {
	profile  Profile
	keys     bookKeys
	key      ed25519.PrivateKey
	capsules []string
	set      func(t time.Time)
}

func newResultBook(t *testing.T) resultBook {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	set := bookClock(t, day0)
	p, keys := bookProfile(t, "a")
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	key, err := privateKey(p.Signing)
	require.NoError(t, err)
	book := resultBook{profile: p, keys: keys, key: key, set: set}
	for _, action := range []string{"day-1", "day-2"} {
		book.capsules = append(book.capsules, publishCapsule(t, p, key, action))
	}
	return book
}

func publishCapsule(t *testing.T, p Profile, key ed25519.PrivateKey, actionID string) string {
	t.Helper()
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	request.Capsule.ActionID = actionID
	target, err := openTarget(t.Context(), p, usePublication)
	require.NoError(t, err)
	published, err := target.publish(t.Context(), request, key)
	require.NoError(t, target.close())
	require.NoError(t, err)
	return published.CapsuleID
}

// withBook runs fn over the profile's open book and releases it, so a CLI
// verb can take the lock afterwards.
func withBook(t *testing.T, p Profile, fn func(book *evidencebook.Book)) {
	t.Helper()
	opened, err := openBook(t.Context(), p, false)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	fn(opened.book)
}

func recordOfCapsule(t *testing.T, p Profile, capsuleID string) string {
	t.Helper()
	var id string
	withBook(t, p, func(book *evidencebook.Book) {
		record, ok, err := publishedRecord(t.Context(), book, capsuleID)
		require.NoError(t, err)
		require.True(t, ok)
		id = record.RecordID
	})
	return id
}

func digestRefs(digests ...string) []map[string]any {
	out := make([]map[string]any, 0, len(digests))
	for _, d := range digests {
		out = append(out, map[string]any{"digest_alg": "SHA-256", "digest": d})
	}
	return out
}

// claim builds one requirement claim; verdict decides sufficiency.
func claim(id, verdict string, digests ...string) map[string]any {
	sufficiency := "SATISFIED"
	if verdict == "not_evaluable" {
		sufficiency = "GAP"
	}
	return map[string]any{
		"id": id, "contract_ref": testContract, "requirement_ref": "req-" + id,
		"tier": "recomputed", "grade": "self-attested", "sufficiency": sufficiency, "verdict": verdict,
		"evidence": digestRefs(digests...), "proofs": []any{},
		"presentation": map[string]any{"kind": "disclosure", "status": "SATISFIED", "evidence": digestRefs(digests...)},
	}
}

// resultDoc assembles a Result whose buckets and coverage are computed from
// its claims, so it cross-checks unless a test breaks it on purpose.
func resultDoc(claims ...map[string]any) map[string]any {
	buckets := map[string]any{"met": []any{}, "not_met": []any{}, "not_evaluable": []any{}}
	unknown := 0
	for _, c := range claims {
		verdict := c["verdict"].(string)
		buckets[verdict] = append(buckets[verdict].([]any), c["id"])
		if c["sufficiency"] == "UNKNOWN" {
			unknown++
		}
	}
	return map[string]any{
		"result_version": resultVersion, "generated_at": "2026-09-26T00:00:00Z", "claims": claims,
		"aggregate": map[string]any{
			"coverage": map[string]any{"evaluated_population": len(claims), "excluded_not_applicable": 0, "unknown_count": unknown},
			"buckets":  buckets,
		},
	}
}

func writeJSON(t *testing.T, name string, value any) string {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func runResultBuild(t *testing.T, p Profile, doc any, extra ...string) (resultBuildResult, string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "result.json")
	args := append([]string{"result", "build", "--profile", p.Name, "--result", writeJSON(t, "input.json", doc), "--out", out}, extra...)
	stdout, err := invoke(t, "", args...)
	var result resultBuildResult
	if err == nil {
		require.NoError(t, json.Unmarshal([]byte(stdout), &result))
	}
	return result, out, err
}

func decodeJSONNumber(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	doc, err := decodeResultDocument(raw)
	require.NoError(t, err)
	return doc
}

func TestResultSchemaIsPinned(t *testing.T) {
	sum := sha256.Sum256(resultSchemaBytes)
	assert.Equal(t, resultSchemaDigest, hex.EncodeToString(sum[:]), "schema/evidence-result-v0.json changed: update resultSchemaDigest and resultSchemaSource together")
	_, _, err := resultSchema()
	require.NoError(t, err)
}

// walkNegatives are the upstream close vectors that SATISFY the schema by
// design and are rejected only by the close walk (aac
// schemas/check_evidence_result_examples.py LINK_NEGATIVES), each with the
// refusal the walk gives it.
var walkNegatives = map[string]string{
	"neg-close-agreed-relabelled-contested.json": "states close_state AGREED, but the counterparty's links to Close",
	"neg-close-agreed-self-acknowledged.json":    "is from this book, the Close's own",
	"neg-close-agreed-third-book.json":           `is not the claim's named peer "oo-sor"`,
	"neg-close-agreed-bookless-close.json":       "the Close names no book, so nothing can be its counterparty",
	"neg-close-ref-not-in-evidence.json":         "close.close_ref",
	"neg-close-peer-ref-not-in-evidence.json":    "close.peer_close_ref",
}

// The committed upstream vectors: every positive passes the schema, the
// cross-checks and -- for a close claim -- the walk over its records
// sidecar; every schema negative fails the schema for its one documented
// reason, before any book is involved; every walk negative passes the
// schema and the cross-checks and is refused by the walk.
func TestResultVectors(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "result"))
	require.NoError(t, err)
	seenWalk := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "pos-") && !strings.HasPrefix(name, "neg-") || strings.HasSuffix(name, ".records.json") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "result", name))
			require.NoError(t, err)
			doc := decodeJSONNumber(t, raw)
			err = checkResultSchema(doc)
			want, walkNegative := walkNegatives[name]
			if strings.HasPrefix(name, "neg-") && !walkNegative {
				require.ErrorIs(t, err, ErrInput)
				assert.Contains(t, SafeError(err), "does not satisfy evidence-result-v0")
				return
			}
			require.NoError(t, err)
			checked, err := crossCheckResult(doc, "")
			require.NoError(t, err)
			err = walkVector(t, name, checked)
			if walkNegative {
				seenWalk++
				require.ErrorIs(t, err, ErrInput, "the walk must refuse %s", name)
				assert.Contains(t, SafeError(err), want)
				return
			}
			assert.NoError(t, err)
		})
	}
	assert.Equal(t, len(walkNegatives), seenWalk, "every walk negative is vendored and run")
}

// walkVector runs result build's close walk -- checkCloseRefs,
// readCloseState, checkCloseState, the functions checkCloseClaim runs over
// the book -- over a vector's records sidecar, keyed as upstream keys it:
// the lowercase-hex SHA-256 of the JCS of each record header. Sidecars
// carry headers, not Producer Envelopes, so each record stands in as signed
// under a key of its own; the different-signer rule is exercised by the
// book tests below, not here.
func walkVector(t *testing.T, name string, doc resultDocument) error {
	t.Helper()
	var hasClose bool
	for _, claim := range doc.claims {
		hasClose = hasClose || claim.typ == claimTypeClose
	}
	if !hasClose {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "result", strings.TrimSuffix(name, ".json")+".records.json"))
	require.NoError(t, err, "a close vector ships its records sidecar")
	var records []map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&records))
	byDigest := make(map[string]map[string]interface{}, len(records))
	var digests []string
	for _, record := range records {
		digest, err := canonical.JSONDigest(record)
		require.NoError(t, err)
		byDigest[digest] = record
		digests = append(digests, digest)
	}
	for _, claim := range doc.claims {
		if claim.typ != claimTypeClose {
			continue
		}
		closeRecord, ok := byDigest[claim.closeRef]
		require.True(t, ok, "%s: close_ref resolves in the sidecar", claim.label())
		recordType, _ := closeRecord["record_type"].(string)
		if err := checkCloseRefs(claim, recordType); err != nil {
			return err
		}
		var linkers []closeLinkRecord
		for _, digest := range digests {
			book, _ := byDigest[digest]["book_id"].(string)
			links, _ := byDigest[digest]["links"].([]interface{})
			for _, l := range links {
				link, _ := l.(map[string]interface{})
				typ := evidencebook.LinkType(fmt.Sprint(link["type"]))
				if link["target"] == claim.closeRef && (typ == evidencebook.Acknowledges || typ == evidencebook.Rebuts) {
					linkers = append(linkers, closeLinkRecord{recordID: digest, link: typ, book: book, key: digest})
				}
			}
		}
		closeBook, _ := closeRecord["book_id"].(string)
		state, linker, ignored := readCloseState(closeBook, claim.closeRef, claim.closePeer, linkers)
		if err := checkCloseState(claim, claim.closeRef, state, linker, ignored); err != nil {
			return err
		}
	}
	return nil
}

func TestResultCrossChecksRefuseUnrecomputableHeadlines(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	base := func() map[string]any { return resultDoc(claim("c1", "met", a), claim("c2", "not_met", b)) }
	aggregate := func(doc map[string]any) map[string]any { return doc["aggregate"].(map[string]any) }
	buckets := func(doc map[string]any) map[string]any { return aggregate(doc)["buckets"].(map[string]any) }
	coverage := func(doc map[string]any) map[string]any { return aggregate(doc)["coverage"].(map[string]any) }
	cases := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"bucket lists a claim under another verdict": {func(d map[string]any) { buckets(d)["met"] = []any{"c2"}; buckets(d)["not_met"] = []any{"c1"} }, `aggregate.buckets.met lists "c2", whose verdict is not_met`},
		"claim in two buckets":                       {func(d map[string]any) { buckets(d)["not_met"] = []any{"c2", "c1"} }, `already lists`},
		"claim in no bucket":                         {func(d map[string]any) { buckets(d)["not_met"] = []any{} }, `is in no bucket`},
		"bucket names a non-claim":                   {func(d map[string]any) { buckets(d)["not_evaluable"] = []any{"ghost"} }, `names "ghost", which is not a claim`},
		"evaluated_population off":                   {func(d map[string]any) { coverage(d)["evaluated_population"] = 3 }, `evaluated_population is 3, but the Result carries 2 claims`},
		"unknown_count off":                          {func(d map[string]any) { coverage(d)["unknown_count"] = 1 }, `unknown_count is 1, but 0 claims have sufficiency UNKNOWN`},
		"duplicate claim id": {func(d map[string]any) {
			d["claims"].([]map[string]any)[1]["id"] = "c1"
			buckets(d)["not_met"] = []any{"c1"}
		}, `duplicates claims[0]`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := base()
			tc.mutate(doc)
			raw, err := json.Marshal(doc)
			require.NoError(t, err)
			_, err = checkResult(decodeJSONNumber(t, raw), "")
			require.ErrorIs(t, err, ErrInput)
			assert.Contains(t, SafeError(err), tc.want)
			assert.NotContains(t, SafeError(err), "\n", "one line")
		})
	}
	raw, err := json.Marshal(base())
	require.NoError(t, err)
	_, err = checkResult(decodeJSONNumber(t, raw), "ec:other@1")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "not --contract ec:other@1")
	checked, err := checkResult(decodeJSONNumber(t, raw), "")
	require.NoError(t, err)
	assert.Equal(t, testContract, checked.contractRef, "the one contract the claims name is the Result's")
	mixed := base()
	mixed["claims"].([]map[string]any)[1]["contract_ref"] = "ec:other@1"
	raw, err = json.Marshal(mixed)
	require.NoError(t, err)
	checked, err = checkResult(decodeJSONNumber(t, raw), "")
	require.NoError(t, err, "claims may span contracts (the upstream close vectors do)")
	assert.Empty(t, checked.contractRef)
	assert.Equal(t, []string{testContract, "ec:other@1"}, checked.contracts)
}

func TestResultBuildSealsAValidatedResult(t *testing.T) {
	b := newResultBook(t)
	p := b.profile
	doc := resultDoc(claim("claim-1", "met", b.capsules[0], b.capsules[1]), claim("claim-2", "not_met", b.capsules[1]))
	capsulePath := filepath.Join(t.TempDir(), "result-record.json")
	result, out, err := runResultBuild(t, p, doc, "--contract", testContract, "--capsule-out", capsulePath)
	require.NoError(t, err)
	assert.False(t, result.AlreadyBuilt)
	assert.Equal(t, testContract, result.ContractRef)
	assert.Equal(t, []string{testContract}, result.ContractRefs)
	assert.Equal(t, 2, result.Claims)
	assert.Equal(t, []string{recordOfCapsule(t, p, b.capsules[0]), recordOfCapsule(t, p, b.capsules[1])}, result.Cites, "cites name the book records that carry the cited capsules, once each, in citation order")
	assert.Positive(t, result.Checkpoint, "a checkpoint covers the record at once")
	assert.Equal(t, out, result.Result)
	assert.Equal(t, capsulePath, result.Capsule)

	withBook(t, p, func(book *evidencebook.Book) {
		sealed, err := book.Get(t.Context(), result.RecordID)
		require.NoError(t, err)
		assert.Equal(t, resultRecordType, sealed.Header.RecordType)
		assert.Equal(t, evidencebook.DerivedMetric, sealed.Header.EpistemicType)
		assert.Equal(t, testContract, sealed.Header.SubjectRef)
		assert.Equal(t, result.Cites, sealed.Header.LinksTo(evidencebook.Cites))
		// The statement is the document, verbatim: its canonical form is
		// exactly what --out holds and what statement_digest covers.
		written, err := os.ReadFile(out)
		require.NoError(t, err)
		statement, err := canonical.JCS(decodeJSONNumber(t, sealed.Header.Statement))
		require.NoError(t, err)
		assert.Equal(t, string(written), string(statement))
		sum := sha256.Sum256(written)
		assert.Equal(t, hex.EncodeToString(sum[:]), result.StatementDigest)
		assert.Equal(t, resultVersion, decodeJSONNumber(t, sealed.Header.Statement)["result_version"])
	})

	verified, err := invoke(t, "", "verify", "--profile", p.Name, "--capsule", capsulePath)
	require.NoError(t, err, verified)

	// A repeat of the same document seals no second record.
	before := bookSize(t, p)
	again, _, err := runResultBuild(t, p, doc)
	require.NoError(t, err)
	assert.True(t, again.AlreadyBuilt)
	assert.Equal(t, result.RecordID, again.RecordID)
	assert.Equal(t, before, bookSize(t, p))
}

// The documented repeat, literally: the same flags twice, --out and
// --capsule-out included. The second run seals nothing, rewrites nothing and
// exits 0; a file holding other bytes at either path is refused by name.
func TestResultBuildRepeatsWithTheSameOutputsAsANoOp(t *testing.T) {
	b := newResultBook(t)
	p := b.profile
	doc := resultDoc(claim("claim-1", "met", b.capsules[0]))
	dir := t.TempDir()
	out, capsuleOut := filepath.Join(dir, "result.json"), filepath.Join(dir, "record.json")
	run := func(t *testing.T, out, capsuleOut string) (resultBuildResult, string, error) {
		t.Helper()
		stdout, err := invoke(t, "", "result", "build", "--profile", p.Name, "--result", writeJSON(t, "input.json", doc), "--out", out, "--capsule-out", capsuleOut)
		var result resultBuildResult
		if i := strings.Index(stdout, "{"); i >= 0 {
			// The record's JSON is printed even when a file is refused.
			require.NoError(t, json.Unmarshal([]byte(stdout[i:]), &result), stdout)
		}
		return result, stdout, err
	}
	first, _, err := run(t, out, capsuleOut)
	require.NoError(t, err)
	assert.Equal(t, 0, ExitCode(err))
	assert.False(t, first.AlreadyBuilt)
	written, err := os.ReadFile(out)
	require.NoError(t, err)
	record, err := os.ReadFile(capsuleOut)
	require.NoError(t, err)
	before := bookSize(t, p)

	again, _, err := run(t, out, capsuleOut)
	require.NoError(t, err, "a repeat with the same --out and --capsule-out succeeds")
	assert.Equal(t, 0, ExitCode(err))
	assert.True(t, again.AlreadyBuilt)
	assert.Equal(t, first.RecordID, again.RecordID)
	assert.Equal(t, out, again.Result)
	assert.Equal(t, capsuleOut, again.Capsule)
	assert.Equal(t, before, bookSize(t, p), "no second record")
	unchanged, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, written, unchanged, "--out holds the same bytes")
	unchanged, err = os.ReadFile(capsuleOut)
	require.NoError(t, err)
	assert.Equal(t, record, unchanged, "--capsule-out holds the same bytes")

	// A different file already at --out is refused by name, left alone, and
	// the sealed record is still reported.
	stale := filepath.Join(dir, "stale.json")
	require.NoError(t, os.WriteFile(stale, []byte(`{"stale":true}`), 0o600))
	refused, stdout, err := run(t, stale, capsuleOut)
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, 2, ExitCode(err))
	assert.Equal(t, "invalid input or profile configuration: --out "+stale+" already exists and does not hold these bytes; nothing was overwritten (the record is sealed: see record_id); pass another path, or move that file, and repeat", SafeError(err))
	assert.Equal(t, first.RecordID, refused.RecordID, stdout)
	assert.True(t, refused.AlreadyBuilt)
	kept, err := os.ReadFile(stale)
	require.NoError(t, err)
	assert.Equal(t, `{"stale":true}`, string(kept))

	// Likewise at --capsule-out, after --out was accepted.
	staleRecord := filepath.Join(dir, "stale-record.json")
	require.NoError(t, os.WriteFile(staleRecord, []byte("not the record"), 0o600))
	_, _, err = run(t, out, staleRecord)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--capsule-out "+staleRecord+" already exists and does not hold these bytes")

	// A directory at --out is not a file to compare against.
	_, _, err = run(t, dir, capsuleOut)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--out "+dir+" exists and is not a regular file")
}

func TestResultBuildRefusesBeforeOpeningTheBook(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := bookProfile(t, "a") // no store init: any error from the book would be "no book yet"
	raw, err := os.ReadFile(filepath.Join("testdata", "result", "neg-met-with-sufficiency-gap.json"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	_, _, err = runResultBuild(t, p, doc)
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, 2, ExitCode(err))
	assert.Contains(t, SafeError(err), "does not satisfy evidence-result-v0")
	assert.Contains(t, SafeError(err), "claims/0/verdict")
	_, _, err = runResultBuild(t, p, map[string]any{"result_version": resultVersion})
	assert.Contains(t, SafeError(err), "missing required propert")
	_, err = invoke(t, "", "result", "build", "--profile", p.Name, "--result", "/missing.json", "--out", filepath.Join(t.TempDir(), "o.json"))
	assert.Contains(t, SafeError(err), "input file not found: /missing.json")
	_, err = invoke(t, "", "result", "build", "--profile", p.Name)
	require.ErrorIs(t, err, ErrInput)
}

func TestResultBuildRefusesACitationTheBookDoesNotHold(t *testing.T) {
	b := newResultBook(t)
	before := bookSize(t, b.profile)
	stranger := strings.Repeat("f", 64)
	_, _, err := runResultBuild(t, b.profile, resultDoc(claim("claim-1", "met", b.capsules[0], stranger)))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), `claim "claim-1" (claims[0]) cites `+stranger+" as evidence, which is not in this book")
	assert.Equal(t, before, bookSize(t, b.profile), "nothing appended")

	// The disclosure carrier's own evidence must resolve too.
	c := claim("claim-1", "met", b.capsules[0])
	c["presentation"] = map[string]any{"kind": "disclosure", "status": "SATISFIED", "evidence": digestRefs(stranger)}
	_, _, err = runResultBuild(t, b.profile, resultDoc(c))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "as presentation evidence, which is not in this book")

	// A claim citing nothing cannot be traced to evidence.
	_, _, err = runResultBuild(t, b.profile, resultDoc(claim("claim-1", "met")))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "cites no evidence")

	// A different document for the same contract is a new record, not a repeat.
	_, _, err = runResultBuild(t, b.profile, resultDoc(claim("claim-1", "met", b.capsules[0])))
	require.NoError(t, err)
	_, _, err = runResultBuild(t, b.profile, resultDoc(claim("claim-1", "not_met", b.capsules[0])))
	require.NoError(t, err)
	withBook(t, b.profile, func(book *evidencebook.Book) {
		records, err := book.Query(t.Context(), evidencebook.Filter{RecordType: resultRecordType})
		require.NoError(t, err)
		assert.Len(t, records, 2)
	})
}

func TestResultBuildRefusesHeadlinesThatDoNotCrossCheck(t *testing.T) {
	b := newResultBook(t)
	doc := resultDoc(claim("claim-1", "met", b.capsules[0]))
	doc["aggregate"].(map[string]any)["coverage"].(map[string]any)["evaluated_population"] = 7
	before := bookSize(t, b.profile)
	_, _, err := runResultBuild(t, b.profile, doc)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "do not cross-check: aggregate.coverage.evaluated_population is 7, but the Result carries 1 claims")
	assert.Equal(t, before, bookSize(t, b.profile))
	_, _, err = runResultBuild(t, b.profile, resultDoc(claim("claim-1", "met", b.capsules[0])), "--contract", "ec:other@2")
	assert.Contains(t, SafeError(err), "names contract "+testContract+", not --contract ec:other@2")
}

// closeFixture seals a Close in the book at day1 over one exchange of day0.
func closeFixture(t *testing.T, b resultBook) string {
	t.Helper()
	appendHalves(t, b.profile, half{"x1", "r1", "p1"})
	b.set(day1)
	result, err := runClose(t, "--profile", b.profile.Name, "--period", "day", "--counterparty", "peer-b")
	require.NoError(t, err)
	return result.RecordID
}

// closeClaim builds a close claim whose close_ref is its first evidence
// digest (the walk rule: close_ref is among evidence[]); peerRef, when
// given, is added to evidence[] too.
func closeClaim(id, state, peer, peerRef string, digests ...string) map[string]any {
	evidence := append([]string(nil), digests...)
	if peerRef != "" {
		evidence = append(evidence, peerRef)
	}
	c := claim(id, "met", evidence...)
	c["type"] = claimTypeClose
	c["requirement_ref"] = "close"
	if state == "CONTESTED" {
		c["verdict"] = "not_met" // a CONTESTED Close never counts as met (#140)
	}
	body := map[string]any{"period": map[string]any{"start": "2026-09-24T00:00:00Z", "end": "2026-09-25T00:00:00Z"}, "close_state": state}
	if len(digests) > 0 {
		body["close_ref"] = map[string]any{"digest_alg": "SHA-256", "digest": digests[0]}
	}
	if peer != "" {
		body["peer"] = peer
	}
	if peerRef != "" {
		body["peer_close_ref"] = map[string]any{"digest_alg": "SHA-256", "digest": peerRef}
	}
	c["close"] = body
	return c
}

func TestResultBuildReadsCloseStateFromLinks(t *testing.T) {
	b := newResultBook(t)
	closeID := closeFixture(t, b)
	p := b.profile

	_, _, err := runResultBuild(t, p, resultDoc(closeClaim("close-1", "UNILATERAL", "peer-b", "", closeID)))
	require.NoError(t, err, "no record links to the Close: UNILATERAL is what the book reads")

	_, _, err = runResultBuild(t, p, resultDoc(closeClaim("close-1", "AGREED", "peer-b", b.capsules[0], closeID)))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "states close_state AGREED, but the counterparty's links to Close "+closeID+" read UNILATERAL (no counterparty record acknowledges or rebuts it)")

	_, _, err = runResultBuild(t, p, resultDoc(closeClaim("close-1", "UNILATERAL", "someone-else", "", closeID)))
	assert.Contains(t, SafeError(err), `names peer "someone-else", but Close `+closeID+` was sealed against counterparty "peer-b"`)

	_, _, err = runResultBuild(t, p, resultDoc(closeClaim("close-1", "UNILATERAL", "", "", b.capsules[0])))
	assert.Contains(t, SafeError(err), "close.close_ref "+b.capsules[0]+" is a ")
	assert.Contains(t, SafeError(err), "record, not a Close")
}

// #140's walk rules (maintainer's second pass): close_ref, and
// peer_close_ref when present, are among the claim's evidence[].
func TestResultBuildRefsResolveInsideEvidence(t *testing.T) {
	b := newResultBook(t)
	closeID := closeFixture(t, b)
	before := bookSize(t, b.profile)

	notInEvidence := closeClaim("close-1", "UNILATERAL", "peer-b", "", closeID)
	notInEvidence["evidence"] = []any{map[string]any{"digest_alg": "SHA-256", "digest": b.capsules[0]}}
	notInEvidence["presentation"].(map[string]any)["evidence"] = notInEvidence["evidence"]
	_, _, err := runResultBuild(t, b.profile, resultDoc(notInEvidence))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), `claim "close-1" (claims[0]): close.close_ref `+closeID+" is not among its evidence[]; a claim reports only on a Close it puts in evidence")

	peerNotInEvidence := closeClaim("close-1", "UNILATERAL", "peer-b", "", closeID)
	peerNotInEvidence["close"].(map[string]any)["peer_close_ref"] = map[string]any{"digest_alg": "SHA-256", "digest": b.capsules[0]}
	_, _, err = runResultBuild(t, b.profile, resultDoc(peerNotInEvidence))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "close.peer_close_ref "+b.capsules[0]+" is not among its evidence[]")

	missing := closeClaim("close-1", "UNILATERAL", "peer-b", "", closeID)
	delete(missing["close"].(map[string]any), "close_ref")
	_, _, err = runResultBuild(t, b.profile, resultDoc(missing))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), `missing required property "close_ref"`)
	assert.Equal(t, before, bookSize(t, b.profile), "nothing is sealed")
}

// Maintainer's third pass: a link makes a state only from the named peer's
// book under a different key. Every record in this book carries this
// book's book_id, so an acknowledgement or rebuttal held here is the
// Close's own book agreeing with itself -- it never makes AGREED or
// CONTESTED, and a claim asserting either over it is refused.
func TestResultBuildIgnoresThisBooksOwnLinksToItsClose(t *testing.T) {
	b := newResultBook(t)
	closeID := closeFixture(t, b)
	p := b.profile

	var ackID string
	withBook(t, p, func(book *evidencebook.Book) {
		record, err := book.Acknowledge(t.Context(), closeID, "peer-b")
		require.NoError(t, err)
		ackID = record.RecordID
	})
	before := bookSize(t, p)
	_, _, err := runResultBuild(t, p, resultDoc(closeClaim("close-1", "AGREED", "peer-b", ackID, closeID)))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "states close_state AGREED, but the counterparty's links to Close "+closeID+" read UNILATERAL (no counterparty record acknowledges or rebuts it; record "+ackID+" acknowledges it but is ignored: it is from this book, the Close's own (a producer cannot agree with itself; the named peer's links are in the peer's book))")
	assert.Equal(t, before, bookSize(t, p), "a self-acknowledged AGREED seals nothing")
	_, _, err = runResultBuild(t, p, resultDoc(closeClaim("close-1", "UNILATERAL", "peer-b", "", closeID)))
	require.NoError(t, err, "the book's own acknowledgement is ignored: the Close reads UNILATERAL")

	var rebutID string
	withBook(t, p, func(book *evidencebook.Book) {
		record, err := book.Rebut(t.Context(), closeID, "peer-b", json.RawMessage(`{"reason":"disputed"}`))
		require.NoError(t, err)
		rebutID = record.RecordID
	})
	_, _, err = runResultBuild(t, p, resultDoc(closeClaim("close-1", "CONTESTED", "peer-b", rebutID, closeID)))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "record "+rebutID+" rebuts it but is ignored: it is from this book")
}

// counterpartyReason is the three-part rule on its own: each part alone is
// not enough. (End to end, a book holds only its own records, so only the
// own-book branch is reachable through `result build` today.)
func TestCounterpartyReasonNeedsAllThreeParts(t *testing.T) {
	keyA, keyB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	assert.Equal(t, "", counterpartyReason("book-a", keyA, "peer-b", "peer-b", keyB), "another book, the named peer, another key")
	assert.Contains(t, counterpartyReason("book-a", keyA, "peer-b", "book-a", keyB), "from this book")
	assert.Contains(t, counterpartyReason("book-a", keyA, "peer-b", "book-c", keyB), `its book "book-c" is not the claim's named peer "peer-b"`)
	assert.Equal(t, "it is signed under the Close's own key", counterpartyReason("book-a", keyA, "peer-b", "peer-b", keyA))
	assert.Contains(t, counterpartyReason("book-a", keyA, "", "peer-b", keyB), "names no peer")
	assert.Contains(t, counterpartyReason("", keyA, "peer-b", "peer-b", keyB), "names no book")
}

// stubStore serves one StoredRecord, to exercise verifiedKeyID's refusals.
type stubStore struct{ record evidencebook.StoredRecord }

func (s stubStore) GetRecord(context.Context, string) (evidencebook.StoredRecord, error) {
	return s.record, nil
}

// Maintainer's fourth pass: the CLI sees the signer, so it VERIFIES the
// Producer Envelope under key_id; an unverifiable signer is refused.
func TestVerifiedKeyIDVerifiesTheEnvelope(t *testing.T) {
	b := newResultBook(t)
	closeID := closeFixture(t, b)
	otherRecord := recordOfCapsule(t, b.profile, b.capsules[0])
	opened, err := openBook(t.Context(), b.profile, false)
	require.NoError(t, err)
	defer func() { require.NoError(t, opened.release()) }()
	stored, err := opened.store.GetRecord(t.Context(), closeID)
	require.NoError(t, err)

	key, err := verifiedKeyID(t.Context(), opened.store, closeID)
	require.NoError(t, err, "the book's own Close verifies under its key")
	assert.Equal(t, hex.EncodeToString(b.key.Public().(ed25519.PublicKey)), key, "key_id is the signer's raw public key in hex")

	noEnvelope := stored
	noEnvelope.Envelope = nil
	_, err = verifiedKeyID(t.Context(), stubStore{noEnvelope}, closeID)
	assert.ErrorIs(t, err, errUnverifiedSigner, "no envelope: a stated key at best")

	flipped := stored
	flipped.Envelope = append([]byte(nil), stored.Envelope...)
	flipped.Envelope[len(flipped.Envelope)-1] ^= 1
	_, err = verifiedKeyID(t.Context(), stubStore{flipped}, closeID)
	assert.ErrorIs(t, err, errUnverifiedSigner, "a signature that does not verify")

	_, err = verifiedKeyID(t.Context(), stubStore{stored}, strings.Repeat("0", 64))
	assert.ErrorIs(t, err, errUnverifiedSigner, "an envelope over another record's id")

	otherStored, err := opened.store.GetRecord(t.Context(), otherRecord)
	require.NoError(t, err)
	swapped := stored
	swapped.Envelope = otherStored.Envelope
	_, err = verifiedKeyID(t.Context(), stubStore{swapped}, closeID)
	assert.ErrorIs(t, err, errUnverifiedSigner, "another record's valid envelope does not sign this capsule")
}

func reconcileClaim(id, peer string, tallies map[string]any, digests ...string) map[string]any {
	c := claim(id, "met", digests...)
	c["type"] = claimTypeReconcile
	c["reconcile"] = map[string]any{
		"join_key": "exchange_id", "peer": peer, "state_of_record": "none",
		"period":  map[string]any{"start": "2026-09-24T00:00:00Z", "end": "2026-09-25T00:00:00Z"},
		"tallies": tallies,
	}
	return c
}

func TestResultBuildRecomputesReconcileTalliesFromTheClose(t *testing.T) {
	b := newResultBook(t)
	closeID := closeFixture(t, b)
	sealed := map[string]any{"matched": 0, "a_only": 0, "b_only": 0, "conflicting": 0, "insufficient": 1, "unresolved": 0}
	_, _, err := runResultBuild(t, b.profile, resultDoc(reconcileClaim("rec-1", "peer-b", sealed, closeID)))
	require.NoError(t, err, "one exchange, no peer account: INSUFFICIENT 1 is what the Close sealed")

	restated := map[string]any{"matched": 1, "a_only": 0, "b_only": 0, "conflicting": 0, "insufficient": 0, "unresolved": 0}
	_, _, err = runResultBuild(t, b.profile, resultDoc(reconcileClaim("rec-1", "peer-b", restated, closeID)))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "tallies matched=1, but Close "+closeID+" recorded MATCHED=0")

	_, _, err = runResultBuild(t, b.profile, resultDoc(reconcileClaim("rec-1", "other", sealed, closeID)))
	assert.Contains(t, SafeError(err), `names peer "other", but Close`)
	_, _, err = runResultBuild(t, b.profile, resultDoc(reconcileClaim("rec-1", "peer-b", sealed, b.capsules[0])))
	assert.Contains(t, SafeError(err), "is a reconcile claim but cites no Close record")
}

// verifyBundleFile runs both verifiers over a written bundle.
func verifyBundleFile(t *testing.T, path string) (map[string]interface{}, evidencebook.VerifiedBundle) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	value, err := decodeBundleJSON(raw)
	require.NoError(t, err)
	verdict := aacbundle.VerifyBundle(value)
	for name, claim := range map[string]aacbundle.ClaimResult{"graph_closure": verdict.GraphClosure, "interval_coverage": verdict.IntervalCoverage, "per_record_membership": verdict.PerRecordMembership} {
		assert.Equal(t, "pass", claim.Status, "%s: %v", name, claim.Findings)
	}
	verified, err := evidencebook.VerifyBundle(raw)
	require.NoError(t, err)
	assert.True(t, verified.AnchorAuthenticated)
	return value, verified
}

func TestDiscloseRootsOnTheResult(t *testing.T) {
	b := newResultBook(t)
	p := b.profile
	result, _, err := runResultBuild(t, p, resultDoc(claim("claim-1", "met", b.capsules[0]), claim("claim-2", "not_met", b.capsules[1])))
	require.NoError(t, err)

	bundlePath := filepath.Join(t.TempDir(), "bundle.json")
	_, err = invoke(t, "", "disclose", "--profile", p.Name, "--root", result.RecordID, "--payloads", "selected", "--out", bundlePath)
	require.NoError(t, err)
	value, verified := verifyBundleFile(t, bundlePath)
	assert.Equal(t, result.RecordID, value["root"])
	held := make(map[string]evidencebook.PeerRecord)
	for _, r := range verified.Records {
		held[r.RecordID] = r
	}
	root := held[result.RecordID]
	require.True(t, root.HeaderVerified && root.CapsuleOK, "the root's header is disclosed and verified")
	assert.Equal(t, resultRecordType, root.Header.RecordType)
	assert.Equal(t, resultVersion, decodeJSONNumber(t, root.Header.Statement)["result_version"])
	for _, id := range result.Cites {
		cited, ok := held[id]
		require.True(t, ok, "the closure followed cites to %s", id)
		assert.True(t, cited.HeaderVerified)
		assert.Equal(t, recordTypePublished, cited.Header.RecordType)
		assert.NotEmpty(t, verified.Payloads[cited.Header.PayloadCommitments[0]], "the cited daily report's capsule rides as a payload")
	}
	disclosures, _ := value["disclosures"].(map[string]interface{})
	rootMembers, _ := disclosures[result.RecordID].(map[string]interface{})
	header, _ := rootMembers["agent_input"].(map[string]interface{})
	assert.Equal(t, resultRecordType, header["record_type"], "the book form: agent_input is the header, statement is the Result")

	_, err = invoke(t, "", "disclose", "--profile", p.Name, "--root", result.RecordID, "--suppress", "agent_input")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "evidence_result record, whose agent_input carries the Result itself")
}
