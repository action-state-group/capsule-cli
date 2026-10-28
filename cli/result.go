package cli

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	emit "github.com/action-state-group/capsule-emit/go"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/spf13/cobra"
)

// `result build` seals a caller-supplied Evidence Result v0 into the
// profile's log as one Capsule whose payload is the Result, the root a
// bundle and its page are built on (capsule-cli#324). The Result's document
// gate -- the schema, then the cross-checks of its headline values -- is
// here, and is the one gate every Result this tool seals passes: the book
// plugin's `book result build` and the deal plugin's `deal terms-result` run
// it too, then add what only their stores can check. This verb checks only
// what is neutral: the document, and that every record it cites is in the
// profile's log, so a bundle rooted on it carries them.

// The Evidence Result v0 schema is vendored, not fetched: `result build`
// seals what it validates, so the schema it validates against is pinned
// here by digest, unlike `contract validate`, which validates against
// whatever --schema the caller names. The copy is agent-action-capsule's
// schemas/evidence-result-v0.json at the commit ResultSchemaSource names --
// the one carrying the optional claim `type` (requirement / reconcile /
// close) of the 2026-09-25 ruling, the `x-` extension members on the
// document and on a claim (spec section 8a, aac #297) and the optional
// document member coverage_report (aac #157), the PROPOSED aggregate member
// coverage_declaration (aac #312), and a claim's evidence[]
// holding at least one record (aac #353); the pinned aac Go module does
// not ship the schema (it lives outside the module's go/ directory), which
// is why a copy is here at all. Refresh it by replacing the file, the digest
// and the source together, with the vectors in the book plugin's
// testdata/result from the same commit (testdata/result/UPSTREAM);
// TestResultSchemaIsPinned keeps them in step.
//
//go:embed schemas/evidence-result-v0.json
var resultSchemaBytes []byte

const (
	ResultVersion  = "evidence-result-v0"
	resultSchemaID = "https://agentactioncapsule.org/schemas/evidence-result-v0.json"
	// ResultSchemaDigest is the SHA-256 of schemas/evidence-result-v0.json.
	ResultSchemaDigest = "288d17a659ee416670819d5517c7f6824d1638c6eaf75dcc76a37c868fa8b85f"
	// ResultSchemaSource names where the vendored copy came from.
	ResultSchemaSource = "agent-action-capsule main@4b24505b67f406cb7109c4fb7a2ebdc99137b500 schemas/evidence-result-v0.json"

	ClaimTypeRequirement = "requirement"
	ClaimTypeReconcile   = "reconcile"
	ClaimTypeClose       = "close"

	// resultActionID and resultDeveloper name the Capsule `result build`
	// seals; its operator is the profile's.
	resultActionID  = "evidence-result"
	resultDeveloper = "capsulectl"
)

var resultVerdicts = []string{"met", "not_met", "not_evaluable"}

var resultSchemaOnce struct {
	sync.Once
	schema *jsonschema.Schema
	raw    any
	err    error
}

// ResultSchemaBytes is the vendored schema, for the test that pins it to
// its vectors.
func ResultSchemaBytes() []byte { return bytes.Clone(resultSchemaBytes) }

// resultSchema compiles the vendored schema once, after checking that the
// embedded bytes are the ones the digest pins. Formats are asserted
// (generated_at and a period's start/end are RFC 3339 date-times), which
// `contract validate` does not do for a caller's schema; a Result is sealed
// from what passes here, so it is checked as tightly as the schema allows.
func resultSchema() (*jsonschema.Schema, any, error) {
	resultSchemaOnce.Do(func() {
		sum := sha256.Sum256(resultSchemaBytes)
		if hex.EncodeToString(sum[:]) != ResultSchemaDigest {
			resultSchemaOnce.err = errors.New("the embedded evidence-result-v0 schema does not match its pinned digest")
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(resultSchemaBytes))
		if err != nil {
			resultSchemaOnce.err = err
			return
		}
		if err = json.Unmarshal(resultSchemaBytes, &resultSchemaOnce.raw); err != nil {
			resultSchemaOnce.err = err
			return
		}
		compiler := jsonschema.NewCompiler()
		compiler.AssertFormat()
		if err = compiler.AddResource(resultSchemaID, doc); err != nil {
			resultSchemaOnce.err = err
			return
		}
		resultSchemaOnce.schema, resultSchemaOnce.err = compiler.Compile(resultSchemaID)
	})
	return resultSchemaOnce.schema, resultSchemaOnce.raw, resultSchemaOnce.err
}

// DecodeResult reads one Result document: a single JSON object, its numbers
// kept as json.Number so a count is never read through a float.
func DecodeResult(raw []byte) (map[string]interface{}, error) {
	if len(raw) > MaxInput {
		return nil, InputError("the input exceeds the 12 MiB size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var doc map[string]interface{}
	if err := decoder.Decode(&doc); err != nil || doc == nil {
		return nil, InputError("Result v0 document must be a JSON object")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, InputError("the input must hold exactly one JSON value")
	}
	return doc, nil
}

// CheckResultSchema validates one decoded Result document against the
// vendored schema and reports every violation in one actionable line.
func CheckResultSchema(doc map[string]interface{}) error {
	schema, raw, err := resultSchema()
	if err != nil {
		return err
	}
	report := ValidateContract(schema, raw, doc)
	if report.Valid {
		return nil
	}
	parts := make([]string, len(report.Issues))
	for i, issue := range report.Issues {
		parts[i] = "at " + issue.Path + ": " + issue.Message
	}
	return Hint(ErrInput, fmt.Sprintf("the Result does not satisfy %s: %s", ResultVersion, strings.Join(parts, "; ")))
}

// ResultClaim is one claim as the gate reads it, after the schema has
// accepted the document: the members the cross-checks, the citation checks
// and the book's walk need, nothing more. The document itself is sealed
// verbatim.
type ResultClaim struct {
	Index       int
	ID          string
	Type        string
	ContractRef string
	Sufficiency string
	Verdict     string
	// Evidence is claims[].evidence[].digest in order; CarrierEvidence is
	// presentation.evidence[].digest when the carrier is a disclosure.
	Evidence        []string
	CarrierEvidence []string
	// close members, when Type is close.
	CloseState   string
	CloseRef     string
	ClosePeer    string
	ClosePeerRef string
	// reconcile members, when Type is reconcile.
	ReconcilePeer    string
	ReconcileTallies map[string]int64
	// PeriodStart and PeriodEnd are close.period or reconcile.period, as
	// stated.
	PeriodStart, PeriodEnd string
}

// Label names the claim in a message.
func (c ResultClaim) Label() string {
	return fmt.Sprintf("claim %q (claims[%d])", c.ID, c.Index)
}

// Cited is every digest the claim cites, in the order it cites them:
// evidence, a disclosure carrier's evidence, then close.close_ref and
// close.peer_close_ref. Both close refs belong in evidence too, but only
// the book's walk refuses one that is not, so they are listed here as well.
func (c ResultClaim) Cited() []string {
	cited := append(append([]string(nil), c.Evidence...), c.CarrierEvidence...)
	for _, ref := range []string{c.CloseRef, c.ClosePeerRef} {
		if ref != "" {
			cited = append(cited, ref)
		}
	}
	return cited
}

// CheckedResult is what the cross-checks establish about a schema-valid
// Result: the contracts its claims name and its claims. ContractRef is the
// contract --contract named, or the one contract every claim names; a
// Result whose claims span contracts (a requirement claim beside a close
// claim, as the upstream vectors do) has none.
type CheckedResult struct {
	Contracts   []string
	ContractRef string
	Claims      []ResultClaim
}

// readResult is CheckedResult's claims plus what the cross-checks count.
type readResult struct {
	CheckedResult
	byID    map[string]*ResultClaim
	unknown int
	// nonInteger lists every count the document writes as something other
	// than an integer literal (7.0, 7e0): a headline count is never read
	// leniently, so each is a finding.
	nonInteger []string
}

func readDigests(value interface{}) []string {
	items, _ := value.([]interface{})
	out := make([]string, 0, len(items))
	for _, item := range items {
		ref, _ := item.(map[string]interface{})
		digest, _ := ref["digest"].(string)
		out = append(out, digest)
	}
	return out
}

func readInteger(value interface{}) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := number.Int64()
	return n, err == nil
}

// readCount reads a count member the schema requires to be an integer. The
// schema accepts 7.0 and 7e0 as integers (JSON Schema compares values, not
// spellings), but a count this tool recomputes is compared only when it is
// an integer literal: anything else is recorded as a finding at path, never
// skipped, so no check fails open on a spelling.
func readCount(value interface{}, path string, nonInteger *[]string) (int64, bool) {
	if value == nil {
		return 0, false
	}
	n, ok := readInteger(value)
	if !ok {
		*nonInteger = append(*nonInteger, fmt.Sprintf("%s is %v, not an integer literal; counts are written as integers (7, not 7.0 or 7e0)", path, value))
	}
	return n, ok
}

func readPeriod(body map[string]interface{}) (start, end string) {
	period, _ := body["period"].(map[string]interface{})
	start, _ = period["start"].(string)
	end, _ = period["end"].(string)
	return start, end
}

// readResultClaims turns a schema-valid document into ResultClaims. It is
// deliberately tolerant of shape: the schema already refused anything
// malformed, so a missing member here is read as its zero value.
func readResultClaims(doc map[string]interface{}) readResult {
	rawClaims, _ := doc["claims"].([]interface{})
	out := readResult{CheckedResult: CheckedResult{Claims: make([]ResultClaim, 0, len(rawClaims))}, byID: make(map[string]*ResultClaim, len(rawClaims))}
	for i, raw := range rawClaims {
		object, _ := raw.(map[string]interface{})
		claim := ResultClaim{Index: i, Type: ClaimTypeRequirement}
		claim.ID, _ = object["id"].(string)
		if typ, ok := object["type"].(string); ok {
			claim.Type = typ
		}
		claim.ContractRef, _ = object["contract_ref"].(string)
		claim.Sufficiency, _ = object["sufficiency"].(string)
		claim.Verdict, _ = object["verdict"].(string)
		claim.Evidence = readDigests(object["evidence"])
		if presentation, ok := object["presentation"].(map[string]interface{}); ok && presentation["kind"] == "disclosure" {
			claim.CarrierEvidence = readDigests(presentation["evidence"])
		}
		if closeBody, ok := object["close"].(map[string]interface{}); ok {
			claim.CloseState, _ = closeBody["close_state"].(string)
			claim.ClosePeer, _ = closeBody["peer"].(string)
			if ref, ok := closeBody["close_ref"].(map[string]interface{}); ok {
				claim.CloseRef, _ = ref["digest"].(string)
			}
			if ref, ok := closeBody["peer_close_ref"].(map[string]interface{}); ok {
				claim.ClosePeerRef, _ = ref["digest"].(string)
			}
			claim.PeriodStart, claim.PeriodEnd = readPeriod(closeBody)
		}
		if reconcile, ok := object["reconcile"].(map[string]interface{}); ok {
			claim.ReconcilePeer, _ = reconcile["peer"].(string)
			if tallies, ok := reconcile["tallies"].(map[string]interface{}); ok {
				claim.ReconcileTallies = make(map[string]int64, len(tallies))
				states := make([]string, 0, len(tallies))
				for state := range tallies {
					states = append(states, state)
				}
				sort.Strings(states)
				for _, state := range states {
					claim.ReconcileTallies[state], _ = readCount(tallies[state], fmt.Sprintf("claims[%d].reconcile.tallies.%s", i, state), &out.nonInteger)
				}
			}
			claim.PeriodStart, claim.PeriodEnd = readPeriod(reconcile)
		}
		if claim.Sufficiency == "UNKNOWN" {
			out.unknown++
		}
		out.Claims = append(out.Claims, claim)
	}
	for i := range out.Claims {
		if _, dup := out.byID[out.Claims[i].ID]; !dup {
			out.byID[out.Claims[i].ID] = &out.Claims[i]
		}
	}
	return out
}

// CrossCheckResult enforces what the schema cannot express and an
// adversarial reader would recompute: every claim names the contract
// --contract names, when given; claim ids are unique; the three buckets
// partition the claims, each entry under its own verdict; and the coverage
// counts equal what the claims add up to. Every finding is reported, in one
// line, so a producer fixes the document once.
func CrossCheckResult(doc map[string]interface{}, contract string) (CheckedResult, error) {
	result := readResultClaims(doc)
	var findings []string
	seen := make(map[string]int, len(result.Claims))
	for _, claim := range result.Claims {
		if first, dup := seen[claim.ID]; dup {
			findings = append(findings, fmt.Sprintf("claims[%d].id %q duplicates claims[%d]; claim ids must be unique", claim.Index, claim.ID, first))
		} else {
			seen[claim.ID] = claim.Index
		}
	}
	contracts := make(map[string]bool)
	for _, claim := range result.Claims {
		contracts[claim.ContractRef] = true
		if contract != "" && claim.ContractRef != contract {
			findings = append(findings, fmt.Sprintf("%s names contract %s, not --contract %s", claim.Label(), claim.ContractRef, contract))
		}
	}
	for name := range contracts {
		result.Contracts = append(result.Contracts, name)
	}
	sort.Strings(result.Contracts)
	result.ContractRef = contract
	if contract == "" && len(result.Contracts) == 1 {
		result.ContractRef = result.Contracts[0]
	}

	aggregate, _ := doc["aggregate"].(map[string]interface{})
	buckets, _ := aggregate["buckets"].(map[string]interface{})
	coverage, _ := aggregate["coverage"].(map[string]interface{})
	bucketed := make(map[string]string, len(result.Claims))
	for _, verdict := range resultVerdicts {
		entries, _ := buckets[verdict].([]interface{})
		for _, entry := range entries {
			id, _ := entry.(string)
			claim, known := result.byID[id]
			switch {
			case !known:
				findings = append(findings, fmt.Sprintf("aggregate.buckets.%s names %q, which is not a claim", verdict, id))
			case bucketed[id] != "":
				findings = append(findings, fmt.Sprintf("aggregate.buckets.%s lists %q, which aggregate.buckets.%s already lists; each claim sits in one bucket", verdict, id, bucketed[id]))
			case claim.Verdict != verdict:
				findings = append(findings, fmt.Sprintf("aggregate.buckets.%s lists %q, whose verdict is %s", verdict, id, claim.Verdict))
				bucketed[id] = verdict
			default:
				bucketed[id] = verdict
			}
		}
	}
	for _, claim := range result.Claims {
		if bucketed[claim.ID] == "" {
			findings = append(findings, fmt.Sprintf("%s is in no bucket; the three buckets must partition the claims", claim.Label()))
		}
	}
	if population, ok := readCount(coverage["evaluated_population"], "aggregate.coverage.evaluated_population", &result.nonInteger); ok && population != int64(len(result.Claims)) {
		findings = append(findings, fmt.Sprintf("aggregate.coverage.evaluated_population is %d, but the Result carries %d claims", population, len(result.Claims)))
	}
	if unknown, ok := readCount(coverage["unknown_count"], "aggregate.coverage.unknown_count", &result.nonInteger); ok && unknown != int64(result.unknown) {
		findings = append(findings, fmt.Sprintf("aggregate.coverage.unknown_count is %d, but %d claims have sufficiency UNKNOWN", unknown, result.unknown))
	}
	readCount(coverage["excluded_not_applicable"], "aggregate.coverage.excluded_not_applicable", &result.nonInteger)
	findings = append(findings, result.nonInteger...)
	if len(findings) > 0 {
		return result.CheckedResult, Hint(ErrInput, "the Result's headline values do not cross-check: "+strings.Join(findings, "; "))
	}
	return result.CheckedResult, nil
}

// CheckResult is the whole document gate: schema, then cross-checks.
func CheckResult(doc map[string]interface{}, contract string) (CheckedResult, error) {
	if err := CheckResultSchema(doc); err != nil {
		return CheckedResult{}, err
	}
	return CrossCheckResult(doc, contract)
}

// ResultStatement is the document in its canonical (JCS) form, the bytes
// `--out` writes and a seal commits to, and the SHA-256 of those bytes.
func ResultStatement(doc map[string]interface{}) ([]byte, string, error) {
	encoded, err := canonical.JCS(doc)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:]), nil
}

// WriteOutput writes data at path as a new file, or leaves alone a regular
// file already holding exactly these bytes, so a repeat of a verb with the
// same output flags is the no-op it reports. Anything else at the path is
// refused by name and nothing is overwritten. sealed says what was sealed
// regardless and where its id is ("the record is sealed: see record_id"),
// so the caller passes another path, or moves the file, and repeats.
func WriteOutput(flag, path string, data []byte, sealed string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := AtomicFile(path, data, false); errors.Is(err, os.ErrExist) {
			return Hint(ErrInput, fmt.Sprintf("%s %s was created by another process while this one ran; nothing was overwritten (%s); repeat to compare it, or pass another path", flag, path, sealed))
		} else if err != nil {
			return err
		}
		return nil
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return Hint(ErrInput, fmt.Sprintf("%s %s exists and is not a regular file; nothing was written there (%s); pass another path", flag, path, sealed))
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(existing, data) {
		return Hint(ErrInput, fmt.Sprintf("%s %s already exists and does not hold these bytes; nothing was overwritten (%s); pass another path, or move that file, and repeat", flag, path, sealed))
	}
	return nil
}

// resultBuildResult is what `result build` reports.
type resultBuildResult struct {
	CapsuleID       string   `json:"capsule_id"`
	Seq             uint64   `json:"seq"`
	AlreadyBuilt    bool     `json:"already_built"`
	ContractRef     string   `json:"contract_ref,omitempty"`
	ContractRefs    []string `json:"contract_refs"`
	Claims          int      `json:"claims"`
	Cites           []string `json:"cites"`
	StatementDigest string   `json:"statement_digest"`
	Result          string   `json:"result,omitempty"`
	Capsule         string   `json:"capsule,omitempty"`
}

// resultCites is every record the Result cites, each once, in citation
// order: the Capsule's acted_on references.
func resultCites(checked CheckedResult) []string {
	cites := []string{}
	seen := make(map[string]bool)
	for _, claim := range checked.Claims {
		for _, digest := range claim.Cited() {
			if !seen[digest] {
				seen[digest] = true
				cites = append(cites, digest)
			}
		}
	}
	return cites
}

// resultRequest is the Capsule `result build` seals: an fyi whose payload
// is the canonical Result and whose references cite, acted_on, every record
// the Result cites, so a bundle's closure from it carries them. Its time is
// the Result's generated_at, so the same document under the same key seals
// the same Capsule. digest_alg is "SHA-256", the spelling a bundle's
// closure follows (citedIDs; aac bundle.go): any other spelling would seal
// references no bundle walks.
func resultRequest(p Profile, doc map[string]interface{}, encoded []byte, cites []string) (Request, error) {
	at, _ := doc["generated_at"].(string)
	generated, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return Request{}, InputError("generated_at must be an RFC 3339 date-time: " + err.Error())
	}
	references := make([]emit.Reference, 0, len(cites))
	for _, digest := range cites {
		references = append(references, emit.Reference{Type: "agent-action-capsule", DigestAlg: "SHA-256", Digest: digest, CitationPurpose: "acted_on"})
	}
	return Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{ActionID: resultActionID, ActionType: emit.ActionTypeFYI, Operator: p.Operator, Developer: resultDeveloper, Timestamp: generated.UTC().Truncate(time.Second), References: references},
		Payload: json.RawMessage(encoded),
	}, nil
}

// inLog reports whether the log holds a record with this Capsule ID.
func inLog(c *cobra.Command, log cll.Backend, capsuleID string) (cll.Entry, bool, error) {
	id, err := hex.DecodeString(capsuleID)
	if err != nil || len(id) != cll.EntryBytes {
		return cll.Entry{}, false, nil
	}
	entry, err := log.GetEntry(c.Context(), id)
	if errors.Is(err, cll.ErrNotFound) {
		return cll.Entry{}, false, nil
	}
	return entry, err == nil, err
}

const resultBuildLong = `Seal a caller-supplied Evidence Result v0 into the profile's log as one
Capsule whose payload is the Result, after checking it. Nothing is sealed when
a check fails.

Checked: the vendored evidence-result-v0 schema, under which every claim
cites at least one record (a claim about a recorded absence cites the
absence record); every headline count
recomputes from the claims (counts must be integer literals: 7, not 7.0 or
7e0); every record a claim cites is in the profile's log. Everything else a
claim states is sealed as stated.

The Capsule cites every record the Result cites (acted_on), so a bundle rooted
on it carries them. Next: cll checkpoint create, then disclose --root with the
capsule_id, then presentation build --bundle for the Result page. disclose
carries the cited records' originals, so they must still be retained.

On a book profile this is book result build, which seals into the book.

A repeat with the same document and key returns the Capsule already in the
log. --out and --capsule-out are left alone when they already hold the same
bytes, and refused when they hold anything else; the Result is still sealed
and its Capsule reported.`

func resultBuildCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "build", Short: "Seal a checked Evidence Result v0 into the profile's log as the Capsule a Result page is built on", Long: resultBuildLong, Args: NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		if done, e := delegateLog(c, p, nil); done {
			return e
		}
		if p.ReadOnly {
			return ErrReadOnlyCLL
		}
		resultPath, _ := c.Flags().GetString("result")
		contract, _ := c.Flags().GetString("contract")
		out, _ := c.Flags().GetString("out")
		capsuleOut, _ := c.Flags().GetString("capsule-out")
		if resultPath == "" {
			return InputError("--result is required")
		}
		raw, e := ReadInput(resultPath)
		if e != nil {
			return e
		}
		doc, e := DecodeResult(raw)
		if e != nil {
			return e
		}
		checked, e := CheckResult(doc, contract)
		if e != nil {
			return e
		}
		encoded, digest, e := ResultStatement(doc)
		if e != nil {
			return Hint(ErrInput, "the Result cannot be put in canonical (JCS) form: "+e.Error())
		}
		cites := resultCites(checked)
		request, e := resultRequest(p, doc, encoded, cites)
		if e != nil {
			return e
		}
		private, e := PrivateKey(p.Signing)
		if e != nil {
			return e
		}
		if e = RequirePublisherKey(p, private); e != nil {
			return e
		}
		record, e := Seal(request, private)
		if e != nil {
			return e
		}
		t, e := OpenTarget(c.Context(), p, UsePublication)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.Close()) }()
		// Every cited record must be in the log and its Capsule in the
		// artifact store, both of which a bundle built on this one reads.
		for _, digest := range cites {
			_, ok, e := inLog(c, t.Log, digest)
			if e != nil {
				return e
			}
			if ok {
				if _, e = t.Artifacts.Get(c.Context(), digest); errors.Is(e, artifact.ErrNotFound) {
					return Hint(ErrInput, fmt.Sprintf("the Result cites %s, which this profile's log lists but whose Capsule its artifact store does not hold; a bundle could not carry it", digest))
				} else if e != nil {
					return e
				}
				continue
			}
			return Hint(ErrInput, fmt.Sprintf("the Result cites %s, which is not a record in this profile's log; a Result is sealed only over records its bundle can carry (publish them here first)", digest))
		}
		result := resultBuildResult{ContractRef: checked.ContractRef, ContractRefs: checked.Contracts, Claims: len(checked.Claims), Cites: cites, StatementDigest: digest, CapsuleID: record.CapsuleID}
		entry, already, e := inLog(c, t.Log, record.CapsuleID)
		if e != nil {
			return e
		}
		if already {
			result.AlreadyBuilt, result.Seq = true, entry.Seq
		} else {
			published, e := t.Publish(c.Context(), request, private)
			if published.CapsuleID == "" {
				return e
			}
			result.Seq = published.Sequence
			if e != nil {
				return errors.Join(e, Output(c, result))
			}
		}
		// The Capsule is in the log from here on: a failure writing either
		// file still reports its capsule_id, so the caller never loses it.
		const sealed = "the Result is sealed: see capsule_id"
		if out != "" {
			if e = WriteOutput("--out", out, encoded, sealed); e != nil {
				return errors.Join(e, Output(c, result))
			}
			result.Result = out
		}
		if capsuleOut != "" {
			// The record as `seal --output` writes it: prepared, so
			// `verify --capsule` reads it.
			prepared, e := artifact.Prepare(record)
			var stored []byte
			if e == nil {
				stored, e = json.Marshal(prepared)
			}
			if e == nil {
				e = WriteOutput("--capsule-out", capsuleOut, stored, sealed)
			}
			if e != nil {
				return errors.Join(e, Output(c, result))
			}
			result.Capsule = capsuleOut
		}
		return Output(c, result)
	}}
	cmd.Flags().String("result", "", "Evidence Result v0 document to seal (JSON)")
	cmd.Flags().String("contract", "", "The <contract_id>@<version> every claim must name; default: the one contract the claims name, if they name one")
	cmd.Flags().String("out", "", "Also write the Result in its canonical (JCS) form, the bytes the Capsule's payload digest covers (required on a profile whose log a plugin keeps)")
	cmd.Flags().String("capsule-out", "", "Also write the sealed Capsule as an artifact.Record for `verify --capsule`")
	return cmd
}

// resultCommand is `capsulectl result`: `build` is the core's.
func resultCommand() *cobra.Command {
	group := &cobra.Command{Use: "result", Short: "Seal an Evidence Result v0 as the root of a Result page"}
	group.AddCommand(resultBuildCommand())
	return group
}
