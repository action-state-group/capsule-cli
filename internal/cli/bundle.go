package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/mmr"
	"github.com/spf13/cobra"
)

const defaultBundleURL = "https://verify.agentactioncapsule.org/bundle"

// defaultMaxFragment is the longest fragment a permalink carries: the top of
// the 2-8 KB that links survive in messengers, mail and QR codes.
const defaultMaxFragment = 8192

type bundleArtifacts interface {
	Get(context.Context, string) (artifact.Record, error)
}

// BundleOptions declares the citation traversal and disclosure treatment.
type BundleOptions struct {
	Root         string
	ClosureDepth int
	Payloads     string
	Suppress     map[string]bool
	// Withhold names records (by capsule_id) whose originals are not
	// disclosed at all: they verify as WITHHELD. Suppress withholds a member
	// from every record; Withhold withholds every member of one record.
	Withhold       map[string]bool
	WithDisclosure bool
	// ProducerKey, when set, is declared in the bundle's producer-key/v1
	// extension (see producerKeyExtension). Nil emits no extension.
	ProducerKey ed25519.PublicKey
}

// producerKeyExtensionKind is the Evidence Bundle extension the AAC viewer
// reads to learn the producer's own Ed25519 key, so that a countersignature
// made with that key renders "not independent". Its exact shape is the one the
// viewer parses (agent-action-capsule ts/src/evidence-graph-view.ts,
// producerPublicKeys): extensions["producer-key/v1"].public_key, one Ed25519
// public key as 64 lowercase hex characters. Extensions are covered by the
// bundle digest (draft-mih-zhang-agent-disclosure-bundle, "Typed
// Extensions"), so the declaration is fixed before any countersignature is
// made over that digest. It can only make a countersignature look less
// independent, never more: a viewer treats the declared key as the
// producer's own.
const producerKeyExtensionKind = "producer-key/v1"

// producerKeyExtension returns the extensions member declaring key as the
// producer's: {"producer-key/v1": {"public_key": "<64 lowercase hex>"}}.
func producerKeyExtension(key ed25519.PublicKey) (map[string]interface{}, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, inputError("producer key must be a 32-byte Ed25519 public key")
	}
	return map[string]interface{}{producerKeyExtensionKind: map[string]interface{}{"public_key": hex.EncodeToString(key)}}, nil
}

// declaredProducerKey reads a bundle's producer-key/v1 public_key, accepting
// only the form the AAC viewer accepts (64 lowercase hex). Anything else is
// treated as no declaration.
func declaredProducerKey(bundle map[string]interface{}) (ed25519.PublicKey, bool) {
	extensions, _ := bundle["extensions"].(map[string]interface{})
	block, _ := extensions[producerKeyExtensionKind].(map[string]interface{})
	value, _ := block["public_key"].(string)
	if len(value) != 2*ed25519.PublicKeySize || !isLowerHex(value) {
		return nil, false
	}
	key, err := hex.DecodeString(value)
	if err != nil {
		return nil, false
	}
	return ed25519.PublicKey(key), true
}

// bundleProducerKey chooses the key a bundle declares as the producer's:
// --producer-key when given (an operator whose countersigning key differs
// from its ledger key declares the one a self-countersignature would carry),
// otherwise the public half of the profile's signing key -- the key that
// sealed the profile's records. A profile with no signing key declares none.
func bundleProducerKey(c *cobra.Command, profile Profile) (ed25519.PublicKey, error) {
	if c.Flags().Lookup("producer-key") != nil {
		if value, _ := c.Flags().GetString("producer-key"); value != "" {
			keys, err := parseKeys([]string{value})
			if err != nil {
				return nil, inputError("--producer-key must be a 32-byte Ed25519 public key in hex")
			}
			return keys[0], nil
		}
	}
	if profile.Signing == (Secret{}) {
		return nil, nil
	}
	key, err := privateKey(profile.Signing)
	if err != nil {
		return nil, err
	}
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, inputError("the profile's signing key is not an Ed25519 key")
	}
	return public, nil
}

func integer(value uint64) json.Number { return json.Number(fmt.Sprintf("%d", value)) }

// AssembleBundle builds a self-verifying v2 Evidence Bundle. The proof range
// deliberately spans the checkpointed log: the authoritative verifier requires
// a membership entry for every sequence in a claimed interval, not merely the
// graph-closure records.
func AssembleBundle(ctx context.Context, artifacts bundleArtifacts, log cll.Backend, logID string, options BundleOptions) (map[string]interface{}, error) {
	if len(options.Root) != 64 {
		return nil, inputError("--root is required: the Capsule ID (64 hex) the bundle is built around")
	}
	// A negative depth means "unset" (the default); an explicit 0 is honored as a
	// root-only bundle. The bundle command's flag default is 2.
	if options.ClosureDepth < 0 {
		options.ClosureDepth = 2
	}
	if options.Payloads == "" {
		options.Payloads = "selected"
	}
	if options.Payloads != "all" && options.Payloads != "selected" && options.Payloads != "none" {
		return nil, inputError("--payloads must be all, selected, or none")
	}
	// "none" is a digests-only bundle (no disclosures overlay at all), the
	// withheld form a countersign request submits; it is not a disclosure mode.
	if options.Payloads == "none" && options.WithDisclosure {
		return nil, inputError("--payloads none cannot be combined with disclosure")
	}
	if options.Payloads == "all" && len(options.Withhold) != 0 {
		return nil, inputError("--payloads all discloses every record, so it cannot withhold any: a shared copy uses --payloads selected")
	}

	root, err := getCapsule(ctx, artifacts, options.Root)
	if err != nil {
		return nil, err
	}
	closure, missing, err := citationClosure(ctx, artifacts, root, options.ClosureDepth)
	if err != nil {
		return nil, err
	}
	state, err := log.LoadCLL(ctx)
	if err != nil {
		return nil, err
	}
	if state.Checkpoint == nil {
		return nil, inputError("the log has no checkpoint yet, so no bundle can prove its records: cut one with `capsulectl cll checkpoint create`")
	}
	checkpointSize := state.Checkpoint.Size
	checkpointSeq := state.Checkpoint.IndexedSeq
	if checkpointSeq == 0 {
		return nil, inputError("the log's checkpoint covers no entries: append a record and cut a new checkpoint with `capsulectl cll checkpoint create`")
	}
	// The emitted range must end at the checkpoint tip (leaf_count(size) ==
	// checkpointSeq); the verifier enforces this, so refuse to emit a cert that
	// would fail rather than produce one that only some verifiers accept.
	if leaves, ok := mmr.LeafCount(checkpointSize); !ok || leaves != checkpointSeq {
		return nil, inputError("covering checkpoint is not tip-aligned (leaf_count(size) != indexed seq)")
	}
	entries, err := allEntries(ctx, log, checkpointSeq)
	if err != nil {
		return nil, err
	}
	if uint64(len(entries)) != checkpointSeq {
		return nil, errors.New("log scan does not cover the checkpointed size")
	}
	for index, entry := range entries {
		if entry.Seq != uint64(index+1) || len(entry.Value) != cll.EntryBytes {
			return nil, errors.New("log scan is not a dense Capsule-ID interval")
		}
	}
	tree, err := mmr.New(state.Nodes)
	if err != nil {
		return nil, err
	}
	rangeRoot := mmr.RootFromPeaks(state.Checkpoint.Peaks)

	recordsByID := closure
	sequenceByID := make(map[string]uint64, len(entries))
	for _, entry := range entries {
		id := hex.EncodeToString(entry.Value)
		sequenceByID[id] = entry.Seq
		record, found := recordsByID[id]
		if !found {
			record, err = getCapsule(ctx, artifacts, id)
			if err != nil {
				return nil, fmt.Errorf("checkpointed record %s is unavailable: %w", id, err)
			}
			recordsByID[id] = record
		}
	}
	for id := range closure {
		if _, found := sequenceByID[id]; !found {
			return nil, fmt.Errorf("cited record %s is not in the checkpointed log", id)
		}
	}

	ids := make([]string, 0, len(recordsByID))
	for id := range recordsByID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return sequenceByID[ids[i]] < sequenceByID[ids[j]] })
	records := make([]interface{}, 0, len(ids))
	memberships := make(map[string]interface{}, len(ids))
	for _, id := range ids {
		seq := sequenceByID[id]
		proof, proofErr := tree.InclusionProof(seq-1, checkpointSize)
		if proofErr != nil {
			return nil, proofErr
		}
		records = append(records, recordsByID[id])
		memberships[id] = map[string]interface{}{
			"log_coordinates": map[string]interface{}{"log_id": logID, "seq": integer(seq), "leaf_index": integer(seq - 1)},
			"inclusion_proof": proofJSON(proof),
		}
	}
	// CLL #13 range proof over the whole checkpointed interval [1, checkpointSeq]
	// plus the ordered body digests of every leaf in it, so a verifier binds each
	// record, not just the two endpoints.
	rangeP, err := tree.RangeProof(0, checkpointSeq-1, checkpointSize)
	if err != nil {
		return nil, err
	}
	bodyDigests := make([]interface{}, len(entries))
	for i, entry := range entries {
		bodyDigests[i] = hex.EncodeToString(entry.Value)
	}
	witnessJSON := make([]interface{}, len(rangeP.Witness))
	for i, w := range rangeP.Witness {
		witnessJSON[i] = hex.EncodeToString(w)
	}
	missingIDs := make([]interface{}, len(missing))
	for i := range missing {
		missingIDs[i] = missing[i]
	}
	mode := "complete"
	if len(missing) != 0 {
		mode = "declared_incomplete"
	}
	checkpointObject, err := bundleCheckpoint(state.Checkpoint.Bytes, rangeRoot, checkpointSize)
	if err != nil {
		return nil, err
	}
	bundle := map[string]interface{}{
		"bundle_version": "2",
		"bundle_kind":    "evidence-bundle/v2",
		"root":           options.Root,
		"records":        records,
		"completeness": map[string]interface{}{
			"closure_depth": integer(uint64(options.ClosureDepth)), "records_mode": mode,
			"payloads_mode": options.Payloads, "suppressed_fields": suppressed(options.Suppress), "missing": missingIDs,
		},
		"completeness_certificate": map[string]interface{}{
			"log_id": logID, "range_root": hex.EncodeToString(rangeRoot), "first_seq": integer(1), "last_seq": integer(checkpointSeq),
			"body_digests": bodyDigests,
			"range_proof":  map[string]interface{}{"from_seq": integer(1), "to_seq": integer(checkpointSeq), "size": integer(checkpointSize), "from_index": integer(0), "to_index": integer(checkpointSeq - 1), "witness": witnessJSON},
			"memberships":  memberships,
		},
		"checkpoint":   checkpointObject,
		"verification": map[string]interface{}{"producer": "capsulectl", "checks": []interface{}{"graph_closure", "interval_coverage", "per_record_membership"}},
	}
	if options.WithDisclosure {
		overlay, disclosureErr := disclosureOverlay(ctx, artifacts, ids, options.Suppress, options.Withhold)
		if disclosureErr != nil {
			return nil, disclosureErr
		}
		// payloads=all is a claim to disclose EVERY committed eligible member that
		// is not suppressed. If any such member's original is not retained it would
		// verify as WITHHELD, making "all" a false claim -- refuse rather than emit it.
		if options.Payloads == "all" {
			for _, id := range ids {
				members, _ := overlay[id].(map[string]interface{})
				for _, member := range committedEligibleMembers(recordsByID[id]) {
					if options.Suppress[member] {
						continue
					}
					// Test presence, not nil value: a retained original that is JSON
					// null is present (DE-3 validates the value), so `== nil` would
					// wrongly reject it as not retained.
					if _, ok := members[member]; members == nil || !ok {
						return nil, inputError(fmt.Sprintf("payloads=all cannot be honored: the original for %s of %s is not retained", member, id))
					}
				}
			}
		}
		bundle["disclosures"] = overlay
	}
	if options.ProducerKey != nil {
		extensions, err := producerKeyExtension(options.ProducerKey)
		if err != nil {
			return nil, err
		}
		bundle["extensions"] = extensions
	}
	if err := verifyProducedBundle(bundle, options.WithDisclosure); err != nil {
		return nil, err
	}
	return bundle, nil
}

// committedEligibleMembers returns the disclosure-eligible members whose digest
// the capsule actually commits (under model_attestation.compute_attestation), so
// payloads=all can require each of them to be disclosed.
func committedEligibleMembers(capsule map[string]interface{}) []string {
	attestation, _ := capsule["model_attestation"].(map[string]interface{})
	compute, _ := attestation["compute_attestation"].(map[string]interface{})
	var members []string
	for member, field := range map[string]string{"agent_input": "agent_input_digest", "agent_output": "agent_output_digest"} {
		if _, ok := compute[field].(string); ok {
			members = append(members, member)
		}
	}
	sort.Strings(members)
	return members
}

func getCapsule(ctx context.Context, artifacts bundleArtifacts, id string) (map[string]interface{}, error) {
	record, err := artifacts.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(record.Capsule)))
	decoder.UseNumber()
	var capsule map[string]interface{}
	if err := decoder.Decode(&capsule); err != nil {
		return nil, err
	}
	if capsule["capsule_id"] != id {
		return nil, errors.New("artifact capsule identity does not match requested id")
	}
	// The record carries its producer signature inline (signature: the hex
	// COSE_Sign1 producer envelope; key_id: the signer's raw public key), the
	// form bundle verifiers check. Both sit outside the capsule_id preimage.
	if len(record.ProducerEnvelope) != 0 {
		signed := envelope.Verify(id, record.ProducerEnvelope)
		if !signed.OK {
			return nil, fmt.Errorf("stored producer envelope for %s does not verify", id)
		}
		capsule["signature"] = hex.EncodeToString(record.ProducerEnvelope)
		capsule["key_id"] = hex.EncodeToString(signed.PublicKey)
	}
	return capsule, nil
}

// bundleCheckpoint is the bundle's checkpoint object: the signed checkpoint's
// own fields (log_id, mmr_size, root, prev_size, prev_root, key_id,
// timestamp, as the COSE statement signs them), plus the statement itself as
// cose (the draft's portable authenticator, unpadded base64url) and as
// statement (the same bytes, for readers of the earlier shape).
func bundleCheckpoint(statement, rangeRoot []byte, checkpointSize uint64) (map[string]interface{}, error) {
	object, err := signedCheckpoint(statement)
	if err != nil {
		return nil, fmt.Errorf("parse checkpoint statement: %w", err)
	}
	if object["root"] != hex.EncodeToString(rangeRoot) || fmt.Sprint(object["mmr_size"]) != fmt.Sprint(checkpointSize) {
		return nil, errors.New("checkpoint statement does not sign the bundle's range root and size")
	}
	object["cose"] = base64.RawURLEncoding.EncodeToString(statement)
	object["statement"] = base64.StdEncoding.EncodeToString(statement)
	return object, nil
}

func citationClosure(ctx context.Context, artifacts bundleArtifacts, root map[string]interface{}, depth int) (map[string]map[string]interface{}, []string, error) {
	rootID, _ := root["capsule_id"].(string)
	records := map[string]map[string]interface{}{rootID: root}
	missingSet := make(map[string]bool)
	frontier := []map[string]interface{}{root}
	for i := 0; i < depth; i++ {
		var next []map[string]interface{}
		for _, record := range frontier {
			for _, id := range citations(record) {
				if _, found := records[id]; found || missingSet[id] {
					continue
				}
				cited, err := getCapsule(ctx, artifacts, id)
				if err != nil {
					// Only a genuinely absent original is a declared-missing citation.
					// A decode/identity/authorization/backend error is an execution
					// failure and must not masquerade as an incomplete graph.
					if errors.Is(err, artifact.ErrNotFound) {
						missingSet[id] = true
						continue
					}
					return nil, nil, fmt.Errorf("resolving cited record %s: %w", id, err)
				}
				records[id] = cited
				next = append(next, cited)
			}
		}
		frontier = next
	}
	missing := make([]string, 0, len(missingSet))
	for id := range missingSet {
		missing = append(missing, id)
	}
	sort.Strings(missing)
	return records, missing, nil
}

func citations(record map[string]interface{}) []string {
	var ids []string
	if chain, ok := record["chain"].(map[string]interface{}); ok {
		if id, ok := chain["parent_capsule_id"].(string); ok {
			ids = append(ids, id)
		}
	}
	if references, ok := record["references"].([]interface{}); ok {
		for _, raw := range references {
			if ref, ok := raw.(map[string]interface{}); ok && ref["type"] == "agent-action-capsule" && ref["digest_alg"] == "SHA-256" {
				if id, ok := ref["digest"].(string); ok {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

func allEntries(ctx context.Context, log cll.EntrySource, size uint64) ([]cll.Entry, error) {
	entries := make([]cll.Entry, 0, size)
	for after := uint64(0); after < size; {
		// Bound each page to the checkpointed range so a log that has grown past
		// the checkpoint (newer, uncheckpointed entries) does not overshoot size
		// and fail the dense-interval check.
		limit := cll.MaxScanLimit
		if remaining := int(size - after); remaining < limit {
			limit = remaining
		}
		batch, err := log.ScanEntries(ctx, after, limit)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		if batch[len(batch)-1].Seq <= after {
			return nil, errors.New("log scan did not advance")
		}
		before := after
		for _, entry := range batch {
			if entry.Seq > size {
				break
			}
			entries = append(entries, entry)
			after = entry.Seq
		}
		if after == before {
			// The batch contained only entries beyond the checkpointed size (a
			// backend that ignored the limit); advancing is impossible, so stop
			// rather than spin.
			return nil, errors.New("log scan returned only entries beyond the checkpointed size")
		}
	}
	return entries, nil
}

func proofJSON(proof mmr.InclusionProof) map[string]interface{} {
	hashes := func(values [][]byte) []interface{} {
		result := make([]interface{}, len(values))
		for i := range values {
			result[i] = hex.EncodeToString(values[i])
		}
		return result
	}
	return map[string]interface{}{"v": integer(proof.V), "kind": proof.Kind, "size": integer(proof.Size), "leaf_index": integer(proof.LeafIndex), "witness": hashes(proof.Witness), "peaks_left": hashes(proof.PeaksLeft), "peaks_right": hashes(proof.PeaksRight)}
}

func suppressed(values map[string]bool) []interface{} {
	result := make([]interface{}, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].(string) < result[j].(string) })
	return result
}

func disclosureOverlay(ctx context.Context, artifacts bundleArtifacts, ids []string, suppress, withhold map[string]bool) (map[string]interface{}, error) {
	// Originals are looked up by the same names seal() persists. A missing or
	// purged original is intentionally absent, which the verifier reports as WITHHELD.
	result := make(map[string]interface{})
	for _, id := range ids {
		if withhold[id] {
			continue
		}
		record, err := artifacts.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		members := make(map[string]interface{})
		for _, original := range record.Artifacts {
			member := ""
			switch original.Binding {
			case artifact.PayloadDigest:
				member = "agent_input"
			case artifact.AgentOutputDigest:
				member = "agent_output"
			}
			if member == "" || suppress[member] || original.State != artifact.Present {
				continue
			}
			decoder := json.NewDecoder(strings.NewReader(string(original.Content)))
			decoder.UseNumber()
			var value interface{}
			if err := decoder.Decode(&value); err != nil {
				return nil, fmt.Errorf("decode stored %s original for %s: %w", member, id, err)
			}
			members[member] = value
		}
		if len(members) != 0 {
			result[id] = members
		}
	}
	return result, nil
}

func verifyProducedBundle(value map[string]interface{}, disclosuresRequired bool) error {
	result := aacbundle.VerifyBundle(value)
	if result.GraphClosure.Status == "fail" || result.IntervalCoverage.Status != "pass" || result.PerRecordMembership.Status != "pass" {
		return fmt.Errorf("authoritative bundle verification rejected produced evidence: graph=%s interval=%s membership=%s", result.GraphClosure.Status, result.IntervalCoverage.Status, result.PerRecordMembership.Status)
	}
	if disclosuresRequired {
		for _, result := range result.Disclosures {
			if result.Status == disclosure.Mismatch || result.Status == disclosure.Ineligible || result.Status == disclosure.NoCommittedDigest {
				return errors.New("authoritative bundle verification rejected disclosures")
			}
		}
	}
	return nil
}

// disclosureRecord is the CLI-layer commitment for a disclose act: what root
// was disclosed, in what payloads mode, which fields were suppressed, and —
// for every member actually revealed — the digest DE-3 already binds it to,
// not the content itself. AssembleBundle stays a pure builder; only this
// command-layer helper (and appendDisclosureRecord below) knows about the log.
func disclosureRecord(bundle map[string]interface{}) (map[string]interface{}, error) {
	completeness, _ := bundle["completeness"].(map[string]interface{})
	overlay, _ := bundle["disclosures"].(map[string]interface{})
	ids := make([]string, 0, len(overlay))
	for id := range overlay {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	revealed := make(map[string]interface{}, len(ids))
	for _, id := range ids {
		members, _ := overlay[id].(map[string]interface{})
		names := make([]string, 0, len(members))
		for member := range members {
			names = append(names, member)
		}
		sort.Strings(names)
		digestByMember := make(map[string]interface{}, len(names))
		for _, member := range names {
			digest, err := canonical.JSONDigest(members[member])
			if err != nil {
				return nil, fmt.Errorf("digest disclosed %s of %s: %w", member, id, err)
			}
			digestByMember[member] = digest
		}
		revealed[id] = digestByMember
	}
	return map[string]interface{}{
		"type":              "disclosure_record",
		"root":              bundle["root"],
		"payloads_mode":     completeness["payloads_mode"],
		"suppressed_fields": completeness["suppressed_fields"],
		"revealed":          revealed,
	}, nil
}

// appendDisclosureRecord seals the disclose act onto the CLL: it appends the
// disclosure_record's own digest as a new log entry, the same way a Capsule's
// ID (itself a content digest) is appended by publish/append. Emitting the
// bundle file is not enough on its own -- every disclose act must be on record.
func appendDisclosureRecord(ctx context.Context, log cll.Backend, bundle map[string]interface{}) (cll.Entry, error) {
	record, err := disclosureRecord(bundle)
	if err != nil {
		return cll.Entry{}, err
	}
	return appendRecordDigest(ctx, log, record)
}

// appendRecordDigest appends a disclosure record's own digest as a log entry.
func appendRecordDigest(ctx context.Context, log cll.Backend, record map[string]interface{}) (cll.Entry, error) {
	digest, err := canonical.JSONDigest(record)
	if err != nil {
		return cll.Entry{}, err
	}
	value, err := hex.DecodeString(digest)
	if err != nil || len(value) != cll.EntryBytes {
		return cll.Entry{}, errors.New("disclosure record digest is not a valid log identity")
	}
	result, err := log.Append(ctx, cll.AppendInput{Value: value, AppendedAt: time.Now().UTC()})
	if err != nil {
		return cll.Entry{}, ErrPending
	}
	return result.Entry, nil
}

// htmlEmitterStubMessage names the unmet dependency explicitly rather than
// silently ignoring --html: agent-action-capsule PR #102 ("evidence-graph
// emitter", branch evidence-graph-emitter) is not yet merged to go/emitter on
// main, so there is no library to render report.html from. TODO: once #102
// merges, wire its Go emitter here to write the offline report.html carrier
// (the bundle embedded + inline verifier); the permalink carrier (codec B,
// aacbundle.EncodeFragment/DecodeFragment) is already wired below.
const htmlEmitterStubMessage = "--html requires the agent-action-capsule #102 evidence-graph emitter (branch evidence-graph-emitter), not yet merged to go/emitter on main; not wired"

const producerKeyFlagUsage = "Ed25519 public key (hex) to declare in the producer-key/v1 extension (default: the profile's signing key)"

// bundleLog picks the log a bundle is read from: --log-id names any log,
// else the profile's log_id. A deal profile's log_id is its cadence log,
// which holds no records, and each deal is its own log: those go through
// --deal (dealBundleRun). disclose appends a disclosure record to the log it
// read, so it refuses a deal's log named by --log-id.
func bundleLog(c *cobra.Command, p Profile, use, root string) (Profile, error) {
	logID, _ := c.Flags().GetString("log-id")
	switch {
	case logID != "":
		if !logName.MatchString(logID) {
			return p, inputError("--log-id must be lowercase letters, digits and ._:/-, such as a profile's log_id")
		}
	case p.LogID == "":
		return p, inputError("the profile has no log_id: name the log with --log-id, or a deal with --deal")
	case p.Namespace == "deal" && strings.HasPrefix(p.LogID, "deal-cadence/"):
		return p, inputError("this is a deal profile: each deal is its own log; name the deal with --deal DEAL_ID")
	default:
		return p, nil
	}
	if strings.HasPrefix(logID, "deal/") {
		return p, inputError("a deal's logs are read with --deal DEAL_ID, which applies the deal's share rules; --log-id does not")
	}
	if root == "" {
		return p, inputError("--root is required: the Capsule ID (64 hex) the bundle is built around")
	}
	p.LogID = logID
	return p, nil
}

func bundleCommands() []*cobra.Command {
	shortFor := map[string]string{
		"bundle":    "Assemble a self-verifying Evidence Bundle from the root's citation closure",
		"disclose":  "Assemble an Evidence Bundle with disclosed agent_input/agent_output originals",
		"permalink": "Mint a viewer permalink over a disclosed Evidence Bundle",
	}
	makeCommand := func(use string, disclosure bool, permalink bool) *cobra.Command {
		command := &cobra.Command{Use: use, Short: shortFor[use], Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
			if html, _ := c.Flags().GetBool("html"); html {
				return inputError(htmlEmitterStubMessage)
			}
			if dealID, _ := c.Flags().GetString("deal"); dealID != "" {
				return dealBundleRun(c, use)
			}
			if c.Flags().Changed("share") || c.Flags().Changed("to") {
				return inputError("--share and --to go with --deal: they choose what a deal's shared copy withholds")
			}
			profile, err := selected(c)
			if err != nil {
				return err
			}
			root, _ := c.Flags().GetString("root")
			closureDepth, _ := c.Flags().GetInt("closure-depth")
			if profile, err = bundleLog(c, profile, use, root); err != nil {
				return err
			}
			payloads, _ := c.Flags().GetString("payloads")
			suppressNames, _ := c.Flags().GetStringSlice("suppress")
			suppressSet := make(map[string]bool, len(suppressNames))
			for _, name := range suppressNames {
				if name != "agent_input" && name != "agent_output" {
					return inputError("--suppress must name agent_input or agent_output")
				}
				suppressSet[name] = true
			}
			producerKey, err := bundleProducerKey(c, profile)
			if err != nil {
				return err
			}
			target, err := openTarget(c.Context(), profile, usePublication)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, target.close()) }()
			var value map[string]interface{}
			var encoded []byte
			if target.book != nil {
				// A jsonl profile's bundle comes from its book, which puts every
				// bundle it builds on record as a disclosure record.
				bundle, err := bookBundle(c.Context(), target.book.book, root, closureDepth, payloads, suppressSet, disclosure || permalink, producerKey)
				if err != nil {
					return err
				}
				if value, err = decodeBundleJSON(bundle.JSON); err != nil {
					return err
				}
				encoded = bundle.JSON
			} else if value, err = AssembleBundle(c.Context(), target.artifacts, target.log, profile.LogID, BundleOptions{Root: root, ClosureDepth: closureDepth, Payloads: payloads, Suppress: suppressSet, WithDisclosure: disclosure || permalink, ProducerKey: producerKey}); err != nil {
				return err
			}
			if use == "disclose" && target.book == nil {
				// Every disclose act goes on record: the CLL append must succeed
				// before the bundle is emitted, not merely alongside it.
				if _, err := appendDisclosureRecord(c.Context(), target.log, value); err != nil {
					return err
				}
			}
			if permalink {
				link, err := mintPermalink(c, value)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(c.OutOrStdout(), link)
				return err
			}
			out, _ := c.Flags().GetString("out")
			if encoded == nil {
				if encoded, err = json.Marshal(value); err != nil {
					return err
				}
			}
			if out != "" {
				return atomicFile(out, encoded, false)
			}
			_, err = c.OutOrStdout().Write(append(encoded, '\n'))
			return err
		}}
		command.Flags().String("root", "", "Root Capsule ID")
		command.Flags().String("deal", "", "A deal on a deal profile: the whole deal from its own log, built as `deal report` builds it (disclose and permalink also need --share and --to)")
		command.Flags().String("share", dealAudienceKeep, "With --deal: who the copy is for: keep (bundle only: your own copy), counterparty or adjudicator (a shared copy, put on record first)")
		command.Flags().String("to", "", "With --deal and --share: who the shared copy is for, as sealed in the disclosure record")
		command.Flags().String("log-id", "", "Read this log instead of the profile's log_id")
		command.Flags().Int("closure-depth", 2, "Citation closure traversal depth from the root")
		command.Flags().String("producer-key", "", producerKeyFlagUsage)
		command.Flags().Bool("html", false, "Also render an offline report.html carrier (not yet wired; see docs)")
		if disclosure || permalink {
			command.Flags().String("payloads", "all", "Disclosure mode: all or selected")
			command.Flags().StringSlice("suppress", nil, "Disclosed member to withhold (agent_input or agent_output)")
		}
		if !permalink {
			command.Flags().String("out", "", "Write the Evidence Bundle JSON to a new file")
		} else {
			command.Flags().String("base-url", defaultBundleURL, "Bundle viewer base URL")
			command.Flags().Int("max-fragment", defaultMaxFragment, "Refuse a link whose fragment is longer than this many characters (0: no limit, to measure)")
		}
		return command
	}
	return []*cobra.Command{makeCommand("bundle", false, false), makeCommand("disclose", true, false), makeCommand("permalink", true, true)}
}
