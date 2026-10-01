package cli

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The Evidence Result v0 schema is vendored, not fetched: `result build`
// seals what it validates, so the schema it validates against is pinned
// here by digest, unlike `contract validate`, which validates against
// whatever --schema the caller names. The copy is agent-action-capsule's
// schemas/evidence-result-v0.json at the commit resultSchemaSource names --
// the one carrying the optional claim `type` (requirement / reconcile /
// close) of the 2026-09-25 ruling; the pinned aac Go module does not
// ship the schema (it lives outside the module's go/ directory), which is
// why a copy is here at all. Refresh it by replacing the file and the
// digest together; TestResultSchemaIsPinned keeps them in step.
//
//go:embed schema/evidence-result-v0.json
var resultSchemaBytes []byte

const (
	resultVersion    = "evidence-result-v0"
	resultRecordType = "evidence_result"
	resultSchemaID   = "https://agentactioncapsule.org/schemas/evidence-result-v0.json"
	// resultSchemaDigest is the SHA-256 of schema/evidence-result-v0.json.
	resultSchemaDigest = "9c49bfbead577dc1fed5e3b626511253adc9a9c87b32d15d948997af42cbd033"
	// resultSchemaSource names where the vendored copy came from.
	resultSchemaSource = "agent-action-capsule main@ee7888df15f1db7eecfe9da62626d290eeabcce6 schemas/evidence-result-v0.json"

	claimTypeRequirement = "requirement"
	claimTypeReconcile   = "reconcile"
	claimTypeClose       = "close"
)

var resultVerdicts = []string{"met", "not_met", "not_evaluable"}

var resultSchemaOnce struct {
	sync.Once
	schema *jsonschema.Schema
	raw    any
	err    error
}

// resultSchema compiles the vendored schema once, after checking that the
// embedded bytes are the ones the digest pins. Formats are asserted
// (generated_at and a period's start/end are RFC 3339 date-times), which
// `contract validate` does not do for a caller's schema; a Result is sealed
// from what passes here, so it is checked as tightly as the schema allows.
func resultSchema() (*jsonschema.Schema, any, error) {
	resultSchemaOnce.Do(func() {
		sum := sha256.Sum256(resultSchemaBytes)
		if hex.EncodeToString(sum[:]) != resultSchemaDigest {
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

// checkResultSchema validates one decoded Result document against the
// vendored schema and reports every violation in one actionable line.
func checkResultSchema(doc map[string]interface{}) error {
	schema, raw, err := resultSchema()
	if err != nil {
		return err
	}
	report := validateContract(schema, raw, doc)
	if report.valid {
		return nil
	}
	parts := make([]string, len(report.issues))
	for i, issue := range report.issues {
		parts[i] = "at " + issue.Path + ": " + issue.Message
	}
	return hint(ErrInput, fmt.Sprintf("the Result does not satisfy %s: %s", resultVersion, strings.Join(parts, "; ")))
}

// resultClaim is one claim as `result build` reads it, after the schema has
// accepted the document: the members the cross-checks and the citation walk
// need, nothing more. The document itself is sealed verbatim.
type resultClaim struct {
	index       int
	id          string
	typ         string
	contractRef string
	sufficiency string
	verdict     string
	// evidence is claims[].evidence[].digest in order; carrierEvidence is
	// presentation.evidence[].digest when the carrier is a disclosure.
	evidence        []string
	carrierEvidence []string
	// close members, when typ is close.
	closeState   string
	closeRef     string
	closePeer    string
	closePeerRef string
	// reconcile members, when typ is reconcile.
	reconcilePeer    string
	reconcileTallies map[string]int64
}

// resultDocument is what the cross-checks establish about a schema-valid
// Result: the contracts its claims name and its claims. contractRef is the
// contract --contract named, or the one contract every claim names; a
// Result whose claims span contracts (a requirement claim beside a close
// claim, as the upstream vectors do) has none.
type resultDocument struct {
	contracts   []string
	contractRef string
	claims      []resultClaim
	byID        map[string]*resultClaim
	unknown     int
}

func (c resultClaim) label() string {
	return fmt.Sprintf("claim %q (claims[%d])", c.id, c.index)
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

// readResult turns a schema-valid document into resultClaims. It is
// deliberately tolerant of shape: the schema already refused anything
// malformed, so a missing member here is read as its zero value.
func readResult(doc map[string]interface{}) resultDocument {
	rawClaims, _ := doc["claims"].([]interface{})
	out := resultDocument{claims: make([]resultClaim, 0, len(rawClaims)), byID: make(map[string]*resultClaim, len(rawClaims))}
	for i, raw := range rawClaims {
		object, _ := raw.(map[string]interface{})
		claim := resultClaim{index: i, typ: claimTypeRequirement}
		claim.id, _ = object["id"].(string)
		if typ, ok := object["type"].(string); ok {
			claim.typ = typ
		}
		claim.contractRef, _ = object["contract_ref"].(string)
		claim.sufficiency, _ = object["sufficiency"].(string)
		claim.verdict, _ = object["verdict"].(string)
		claim.evidence = readDigests(object["evidence"])
		if presentation, ok := object["presentation"].(map[string]interface{}); ok && presentation["kind"] == "disclosure" {
			claim.carrierEvidence = readDigests(presentation["evidence"])
		}
		if closeBody, ok := object["close"].(map[string]interface{}); ok {
			claim.closeState, _ = closeBody["close_state"].(string)
			claim.closePeer, _ = closeBody["peer"].(string)
			if ref, ok := closeBody["close_ref"].(map[string]interface{}); ok {
				claim.closeRef, _ = ref["digest"].(string)
			}
			if ref, ok := closeBody["peer_close_ref"].(map[string]interface{}); ok {
				claim.closePeerRef, _ = ref["digest"].(string)
			}
		}
		if reconcile, ok := object["reconcile"].(map[string]interface{}); ok {
			claim.reconcilePeer, _ = reconcile["peer"].(string)
			if tallies, ok := reconcile["tallies"].(map[string]interface{}); ok {
				claim.reconcileTallies = make(map[string]int64, len(tallies))
				for state, value := range tallies {
					claim.reconcileTallies[state], _ = readInteger(value)
				}
			}
		}
		if claim.sufficiency == "UNKNOWN" {
			out.unknown++
		}
		out.claims = append(out.claims, claim)
	}
	for i := range out.claims {
		if _, dup := out.byID[out.claims[i].id]; !dup {
			out.byID[out.claims[i].id] = &out.claims[i]
		}
	}
	return out
}

// crossCheckResult enforces what the schema cannot express and an
// adversarial reader would recompute: every claim names the contract
// --contract names, when given; claim ids are unique; the three buckets
// partition the claims, each entry under its own verdict; and the coverage
// counts equal what the claims add up to. Every finding is reported, in one
// line, so a producer fixes the document once.
func crossCheckResult(doc map[string]interface{}, contract string) (resultDocument, error) {
	result := readResult(doc)
	var findings []string
	seen := make(map[string]int, len(result.claims))
	for _, claim := range result.claims {
		if first, dup := seen[claim.id]; dup {
			findings = append(findings, fmt.Sprintf("claims[%d].id %q duplicates claims[%d]; claim ids must be unique", claim.index, claim.id, first))
		} else {
			seen[claim.id] = claim.index
		}
	}
	contracts := make(map[string]bool)
	for _, claim := range result.claims {
		contracts[claim.contractRef] = true
		if contract != "" && claim.contractRef != contract {
			findings = append(findings, fmt.Sprintf("%s names contract %s, not --contract %s", claim.label(), claim.contractRef, contract))
		}
	}
	for name := range contracts {
		result.contracts = append(result.contracts, name)
	}
	sort.Strings(result.contracts)
	result.contractRef = contract
	if contract == "" && len(result.contracts) == 1 {
		result.contractRef = result.contracts[0]
	}

	aggregate, _ := doc["aggregate"].(map[string]interface{})
	buckets, _ := aggregate["buckets"].(map[string]interface{})
	coverage, _ := aggregate["coverage"].(map[string]interface{})
	bucketed := make(map[string]string, len(result.claims))
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
			case claim.verdict != verdict:
				findings = append(findings, fmt.Sprintf("aggregate.buckets.%s lists %q, whose verdict is %s", verdict, id, claim.verdict))
				bucketed[id] = verdict
			default:
				bucketed[id] = verdict
			}
		}
	}
	for _, claim := range result.claims {
		if bucketed[claim.id] == "" {
			findings = append(findings, fmt.Sprintf("%s is in no bucket; the three buckets must partition the claims", claim.label()))
		}
	}
	if population, ok := readInteger(coverage["evaluated_population"]); ok && population != int64(len(result.claims)) {
		findings = append(findings, fmt.Sprintf("aggregate.coverage.evaluated_population is %d, but the Result carries %d claims", population, len(result.claims)))
	}
	if unknown, ok := readInteger(coverage["unknown_count"]); ok && unknown != int64(result.unknown) {
		findings = append(findings, fmt.Sprintf("aggregate.coverage.unknown_count is %d, but %d claims have sufficiency UNKNOWN", unknown, result.unknown))
	}
	if len(findings) > 0 {
		return result, hint(ErrInput, "the Result's headline values do not cross-check: "+strings.Join(findings, "; "))
	}
	return result, nil
}

// checkResult is the whole document gate: schema, then cross-checks.
func checkResult(doc map[string]interface{}, contract string) (resultDocument, error) {
	if err := checkResultSchema(doc); err != nil {
		return resultDocument{}, err
	}
	return crossCheckResult(doc, contract)
}
