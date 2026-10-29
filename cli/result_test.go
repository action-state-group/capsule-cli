package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const resultTestContract = "ec:example-week@1"

func resultTestRefs(digests ...string) []any {
	out := make([]any, 0, len(digests))
	for _, d := range digests {
		out = append(out, map[string]any{"digest_alg": "SHA-256", "digest": d})
	}
	return out
}

// resultTestClaim is one requirement claim citing digests, its sufficiency
// following its verdict.
func resultTestClaim(id, verdict string, digests ...string) map[string]any {
	sufficiency := "SATISFIED"
	if verdict == "not_evaluable" {
		sufficiency = "GAP"
	}
	return map[string]any{
		"id": id, "contract_ref": resultTestContract, "requirement_ref": "req-" + id,
		"tier": "recomputed", "grade": "self-attested", "sufficiency": sufficiency, "verdict": verdict,
		"evidence": resultTestRefs(digests...), "proofs": []any{},
		"presentation": map[string]any{"kind": "disclosure", "status": "SATISFIED", "evidence": resultTestRefs(digests...)},
	}
}

// resultTestDoc is a Result whose buckets and coverage add up from its
// claims.
func resultTestDoc(claims ...map[string]any) map[string]any {
	buckets := map[string]any{"met": []any{}, "not_met": []any{}, "not_evaluable": []any{}}
	for _, c := range claims {
		verdict := c["verdict"].(string)
		buckets[verdict] = append(buckets[verdict].([]any), c["id"])
	}
	return map[string]any{
		"result_version": ResultVersion, "generated_at": "2026-10-07T12:00:00Z", "claims": claims,
		"aggregate": map[string]any{
			"coverage": map[string]any{"evaluated_population": len(claims), "excluded_not_applicable": 0, "unknown_count": 0},
			"buckets":  buckets,
		},
		"view": map[string]any{"spec_version": "presentation/v1", "title": "Example week (synthetic)"},
	}
}

func (d *disclosureProfile) writeJSON(name string, value any) string {
	d.t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(d.t, err)
	path := filepath.Join(d.dir, name)
	require.NoError(d.t, os.WriteFile(path, raw, 0o600))
	return path
}

// buildResult runs `result build` for doc and decodes its result, which a
// failure after sealing still prints.
func (d *disclosureProfile) buildResult(doc any, extra ...string) (resultBuildResult, string, error) {
	d.t.Helper()
	d.n++
	args := append([]string{"result", "build", "--profile", "rep", "--result", d.writeJSON("result-"+strings.Repeat("x", d.n)+".json", doc)}, extra...)
	out, err := invoke(d.t, "", args...)
	var result resultBuildResult
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		require.NoError(d.t, json.NewDecoder(strings.NewReader(out)).Decode(&result), out)
	}
	return result, out, err
}

// The acceptance flow of capsule-cli#324 with no plugin installed: records
// sealed into a profile's log, `result build` over them, a checkpoint, the
// bundle producer rooted on the Result, and `presentation build` writing a
// page whose root the built-in Result module renders.
func TestResultBuildCoreOnlyFlowRendersAResultPage(t *testing.T) {
	pluginRoot(t)
	d := newDisclosureProfile(t)
	day1 := d.publish("day-1", `{"report":"day one"}`)
	day2 := d.publish("day-2", `{"report":"day two"}`)
	doc := resultTestDoc(resultTestClaim("claim-1", "met", day1, day2), resultTestClaim("claim-2", "not_met", day2))
	out := filepath.Join(d.dir, "result.canonical.json")
	capsuleOut := filepath.Join(d.dir, "result.record.json")

	built, stdout, err := d.buildResult(doc, "--out", out, "--capsule-out", capsuleOut)
	require.NoError(t, err, stdout)
	assert.False(t, built.AlreadyBuilt)
	assert.Equal(t, []string{day1, day2}, built.Cites, "each cited record once, in citation order")
	assert.Equal(t, 2, built.Claims)
	assert.Equal(t, resultTestContract, built.ContractRef)
	assert.Equal(t, uint64(3), built.Seq)
	marshalled, err := json.Marshal(doc)
	require.NoError(t, err)
	decoded, err := DecodeResult(marshalled)
	require.NoError(t, err)
	canonicalDoc, err := canonical.JCS(decoded)
	require.NoError(t, err)
	written, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, canonicalDoc, written, "--out is the Result's JCS")
	_, wantDigest, err := ResultStatement(decoded)
	require.NoError(t, err)
	assert.Equal(t, wantDigest, built.StatementDigest)

	// The sealed Capsule: an fyi whose payload is the canonical Result and
	// whose references cite every record acted_on, spelled SHA-256 so a
	// bundle's closure follows them; --capsule-out verifies.
	verified, err := invoke(t, "", "verify", "--profile", "rep", "--capsule", capsuleOut)
	require.NoError(t, err, verified)
	assert.Contains(t, verified, `"capsule_identity":"passed"`)
	assert.Contains(t, verified, `"producer_signature_and_trust":"passed"`)
	record, err := readRecord(capsuleOut)
	require.NoError(t, err)
	assert.Equal(t, built.CapsuleID, record.CapsuleID)
	var capsule map[string]any
	require.NoError(t, json.Unmarshal(record.Capsule, &capsule))
	assert.Equal(t, "fyi", capsule["action_type"])
	assert.Equal(t, "evidence-result", capsule["action_id"])
	assert.Equal(t, "2026-10-07T12:00:00Z", capsule["timestamp"])
	assert.Equal(t, []any{
		map[string]any{"type": "agent-action-capsule", "digest_alg": "SHA-256", "digest": day1, "citation_purpose": "acted_on"},
		map[string]any{"type": "agent-action-capsule", "digest_alg": "SHA-256", "digest": day2, "citation_purpose": "acted_on"},
	}, capsule["references"])
	require.Len(t, record.Artifacts, 1)
	assert.Equal(t, canonicalDoc, []byte(record.Artifacts[0].Content), "the payload is the canonical Result")

	d.checkpoint()
	bundlePath := d.report("disclose", built.CapsuleID)
	verdict, err := invoke(t, "", "verify", "--bundle", bundlePath)
	require.NoError(t, err, verdict)
	var verification map[string]any
	require.NoError(t, json.Unmarshal([]byte(verdict), &verification), verdict)
	assert.Equal(t, "VALID", verification["verdict"], verdict)
	raw, err := os.ReadFile(bundlePath)
	require.NoError(t, err)
	var bundle map[string]any
	require.NoError(t, json.Unmarshal(raw, &bundle))
	assert.Equal(t, built.CapsuleID, bundle["root"])
	carried := map[string]bool{}
	for _, r := range bundle["records"].([]any) {
		carried[r.(map[string]any)["capsule_id"].(string)] = true
	}
	assert.True(t, carried[day1] && carried[day2], "the bundle carries every cited record")
	// The references are ones a bundle's closure follows: without a cited
	// record the same bundle is no longer VALID.
	var kept []any
	for _, r := range bundle["records"].([]any) {
		if r.(map[string]any)["capsule_id"] != day1 {
			kept = append(kept, r)
		}
	}
	trimmed := map[string]any{}
	for k, v := range bundle {
		trimmed[k] = v
	}
	trimmed["records"] = kept
	trimmedPath := d.writeJSON("trimmed.json", trimmed)
	verdict, _ = invoke(t, "", "verify", "--bundle", trimmedPath)
	require.NoError(t, json.Unmarshal([]byte(verdict), &verification), verdict)
	assert.NotEqual(t, "VALID", verification["verdict"], verdict)

	listed, err := invoke(t, "", "presentation", "list", "--bundle", bundlePath)
	require.NoError(t, err, listed)
	l := decodeList(t, listed)
	assert.Equal(t, "module", l.Resolution, listed)
	require.NotNil(t, l.Selected, listed)
	assert.Equal(t, "aac.builtin.result/v0", l.Selected["id"], "the Result module renders the root, not the generic fallback")

	page := filepath.Join(d.dir, "page.html")
	stdout, err = invoke(t, "", "presentation", "build", "--bundle", bundlePath, "--out", page)
	require.NoError(t, err, stdout)
	b := decodeBuild(t, stdout)
	assert.Equal(t, "written", b.Page)
	html, err := os.ReadFile(page)
	require.NoError(t, err)
	assert.Contains(t, string(html), built.CapsuleID)
	assert.Contains(t, string(html), "claim-1")
}

// The same document under the same key is the same Capsule: a repeat
// appends nothing and says so, and finds its --out file already right.
func TestResultBuildRepeatSealsNothingNew(t *testing.T) {
	d := newDisclosureProfile(t)
	day1 := d.publish("day-1", `{"report":"day one"}`)
	doc := resultTestDoc(resultTestClaim("claim-1", "met", day1))
	out := filepath.Join(d.dir, "result.json")
	first, stdout, err := d.buildResult(doc, "--out", out)
	require.NoError(t, err, stdout)
	again, stdout, err := d.buildResult(doc, "--out", out)
	require.NoError(t, err, stdout)
	assert.True(t, again.AlreadyBuilt)
	assert.Equal(t, first.CapsuleID, again.CapsuleID)
	assert.Equal(t, first.Seq, again.Seq)
	assert.Equal(t, []string{day1, first.CapsuleID}, d.logIDs(), "no second entry")
}

func TestResultBuildRefusesWhatItCannotSeal(t *testing.T) {
	d := newDisclosureProfile(t)
	day1 := d.publish("day-1", `{"report":"day one"}`)
	before := d.logIDs()

	unknown := make([]byte, 32)
	_, err := rand.Read(unknown)
	require.NoError(t, err)
	notInLog := hex.EncodeToString(unknown)

	// A Capsule ID the log lists but whose Capsule the store does not hold.
	_, err = rand.Read(unknown)
	require.NoError(t, err)
	logOnly := hex.EncodeToString(unknown)
	p, err := LoadProfile("rep")
	require.NoError(t, err)
	target, err := OpenTarget(t.Context(), p, UseCLL)
	require.NoError(t, err)
	_, err = appendRecord(t.Context(), target.Log, logOnly)
	require.NoError(t, errorsJoinClose(err, target))
	before = append(before, logOnly)

	// A close claim whose close_ref is not among its evidence: the core
	// still checks the record it names, and it is not in the log.
	closeClaim := resultTestClaim("close-1", "met", day1)
	closeClaim["type"], closeClaim["requirement_ref"] = "close", "close"
	closeClaim["close"] = map[string]any{
		"close_ref": map[string]any{"digest_alg": "SHA-256", "digest": notInLog}, "close_state": "UNILATERAL",
		"period": map[string]any{"start": "2026-10-06T00:00:00Z", "end": "2026-10-07T00:00:00Z"},
	}

	badCount := resultTestDoc(resultTestClaim("claim-1", "met", day1))
	badCount["aggregate"].(map[string]any)["coverage"].(map[string]any)["evaluated_population"] = 2
	noCoverage := resultTestDoc(resultTestClaim("claim-1", "met", day1))
	delete(noCoverage["aggregate"].(map[string]any), "coverage")
	for name, c := range map[string]struct {
		doc  any
		args []string
		want string
	}{
		"schema":                   {noCoverage, nil, "does not satisfy evidence-result-v0"},
		"cross-check":              {badCount, nil, "evaluated_population is 2, but the Result carries 1 claims"},
		"--contract":               {resultTestDoc(resultTestClaim("claim-1", "met", day1)), []string{"--contract", "ec:other@1"}, "not --contract ec:other@1"},
		"not in the log":           {resultTestDoc(resultTestClaim("claim-1", "met", notInLog)), nil, "cites " + notInLog + ", which is not a record in this profile's log"},
		"not in the store":         {resultTestDoc(resultTestClaim("claim-1", "met", logOnly)), nil, "whose Capsule its artifact store does not hold"},
		"close_ref not in the log": {resultTestDoc(closeClaim), nil, "cites " + notInLog + ", which is not a record in this profile's log"},
		"not one object":           {[]any{1}, nil, "must be a JSON object"},
	} {
		t.Run(name, func(t *testing.T) {
			_, out, err := d.buildResult(c.doc, c.args...)
			require.ErrorIs(t, err, ErrInput, out)
			assert.Contains(t, SafeError(err), c.want)
		})
	}
	assert.Equal(t, before, d.logIDs(), "nothing is sealed when a check fails")

	_, err = invoke(t, "", "result", "build", "--profile", "rep")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--result is required")
}

// Every claim cites at least one record (Result v0 section 1, aac #353): a
// story claim that cites nothing is refused and nothing is sealed. The same
// claim citing the recorded absence it narrates is sealed, and the Capsule
// references that record.
func TestResultBuildRefusesAClaimThatCitesNoRecord(t *testing.T) {
	d := newDisclosureProfile(t)
	day1 := d.publish("day-1", `{"report":"day one"}`)
	before := d.logIDs()
	story := map[string]any{"kind": "story", "status": "NOT_FOUND", "narrative": "a request was made and nothing arrived in its window"}
	uncited := resultTestClaim("claim-2", "not_evaluable")
	uncited["presentation"] = story
	_, stdout, err := d.buildResult(resultTestDoc(resultTestClaim("claim-1", "met", day1), uncited))
	require.ErrorIs(t, err, ErrInput, stdout)
	assert.Contains(t, SafeError(err), "does not satisfy evidence-result-v0")
	assert.Equal(t, before, d.logIDs(), "nothing is sealed when a claim cites no record")

	absence := d.publish("absence", `{"outcome":"recorded_absence","request_digest":"`+strings.Repeat("ab", 32)+`","window":{"opened":"2026-10-12T00:00:05Z","closed":"2026-10-12T00:01:05Z"},"recorded_at":"2026-10-12T00:01:05Z"}`)
	cited := resultTestClaim("claim-2", "not_evaluable", absence)
	cited["presentation"] = story
	built, stdout, err := d.buildResult(resultTestDoc(resultTestClaim("claim-1", "met", day1), cited))
	require.NoError(t, err, stdout)
	assert.Equal(t, []string{day1, absence}, built.Cites)
	assert.Equal(t, 2, built.Claims)
}

// A file already at --out that holds other bytes is refused after the seal,
// and the Capsule's id is still reported.
func TestResultBuildReportsTheCapsuleWhenAnOutputIsRefused(t *testing.T) {
	d := newDisclosureProfile(t)
	day1 := d.publish("day-1", `{"report":"day one"}`)
	out := filepath.Join(d.dir, "taken.json")
	require.NoError(t, os.WriteFile(out, []byte("something else"), 0o600))
	built, stdout, err := d.buildResult(resultTestDoc(resultTestClaim("claim-1", "met", day1)), "--out", out)
	require.ErrorIs(t, err, ErrInput, stdout)
	assert.Contains(t, SafeError(err), "does not hold these bytes")
	assert.Contains(t, SafeError(err), "see capsule_id")
	require.NotEmpty(t, built.CapsuleID)
	assert.Equal(t, []string{day1, built.CapsuleID}, d.logIDs())
	kept, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "something else", string(kept))

	// The same for --capsule-out, on a repeat: the Capsule is reported.
	capsuleOut := filepath.Join(d.dir, "taken-record.json")
	require.NoError(t, os.WriteFile(capsuleOut, []byte("something else"), 0o600))
	again, stdout, err := d.buildResult(resultTestDoc(resultTestClaim("claim-1", "met", day1)), "--capsule-out", capsuleOut)
	require.ErrorIs(t, err, ErrInput, stdout)
	assert.Contains(t, SafeError(err), "--capsule-out "+capsuleOut+" already exists and does not hold these bytes")
	assert.Equal(t, built.CapsuleID, again.CapsuleID)
	assert.True(t, again.AlreadyBuilt)
	assert.Equal(t, []string{day1, built.CapsuleID}, d.logIDs())
}

func TestResultBuildRefusesAReadOnlyProfile(t *testing.T) {
	d := newDisclosureProfile(t)
	day1 := d.publish("day-1", `{"report":"day one"}`)
	d.run("profile", "update", "--profile", "rep", "--read-only")
	_, out, err := d.buildResult(resultTestDoc(resultTestClaim("claim-1", "met", day1)))
	require.ErrorIs(t, err, ErrReadOnlyCLL, out)
}

// The vendored schema is the one its digest pins, and compiles.
func TestResultSchemaCompilesAtItsPin(t *testing.T) {
	sum := sha256.Sum256(ResultSchemaBytes())
	assert.Equal(t, ResultSchemaDigest, hex.EncodeToString(sum[:]), "schemas/evidence-result-v0.json changed: update ResultSchemaDigest and ResultSchemaSource together (and the book plugin's testdata/result vectors)")
	_, _, err := resultSchema()
	require.NoError(t, err)
}

// plan_ref and dataset_ref (aac spec section 7.3) pass together, as digest
// refs; either alone, an inline plan in place of the digest, and any other
// unknown member are still refused.
func TestResultSchemaAdmitsPlanAndDatasetRefs(t *testing.T) {
	ref := func(digest string) map[string]any {
		return map[string]any{"digest_alg": "SHA-256", "digest": strings.Repeat(digest, 64)}
	}
	check := func(mutate func(map[string]any)) error {
		doc := resultTestDoc(resultTestClaim("claim-1", "met", strings.Repeat("a", 64)))
		mutate(doc)
		raw, err := json.Marshal(doc)
		require.NoError(t, err)
		decoded, err := DecodeResult(raw)
		require.NoError(t, err)
		return CheckResultSchema(decoded)
	}
	require.NoError(t, check(func(map[string]any) {}))
	require.NoError(t, check(func(doc map[string]any) { doc["plan_ref"], doc["dataset_ref"] = ref("b"), ref("c") }))
	for name, mutate := range map[string]func(map[string]any){
		"plan_ref alone":    func(doc map[string]any) { doc["plan_ref"] = ref("b") },
		"dataset_ref alone": func(doc map[string]any) { doc["dataset_ref"] = ref("c") },
		"an inline plan": func(doc map[string]any) {
			doc["plan_ref"], doc["dataset_ref"] = map[string]any{"header": map[string]any{}, "nodes": []any{}}, ref("c")
		},
		"another digest_alg": func(doc map[string]any) {
			doc["plan_ref"], doc["dataset_ref"] = map[string]any{"digest_alg": "SHA-512", "digest": strings.Repeat("b", 64)}, ref("c")
		},
		"an unknown member": func(doc map[string]any) {
			doc["plan_ref"], doc["dataset_ref"], doc["dataset"] = ref("b"), ref("c"), []any{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := check(mutate)
			require.ErrorIs(t, err, ErrInput)
			assert.Contains(t, SafeError(err), "does not satisfy evidence-result-v0")
		})
	}
}

func errorsJoinClose(err error, t *Target) error {
	if closeErr := t.Close(); err == nil {
		return closeErr
	}
	return err
}
