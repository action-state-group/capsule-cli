package cli

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/emitter"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/mmr"
	"github.com/spf13/cobra"
)

const defaultBundleURL = "https://verify.agentactioncapsule.org/bundle"

type bundleArtifacts interface {
	Get(context.Context, string) (artifact.Record, error)
}

//go:embed assets/evidence-graph.iife.js
var evidenceGraphIIFE []byte

// BundleOptions declares the citation traversal and disclosure treatment.
type BundleOptions struct {
	Root           string
	ClosureDepth   int
	Payloads       string
	Suppress       map[string]bool
	WithDisclosure bool
}

func integer(value uint64) json.Number { return json.Number(fmt.Sprintf("%d", value)) }

// AssembleBundle builds a self-verifying v2 Evidence Bundle. The proof range
// deliberately spans the checkpointed log: the authoritative verifier requires
// a membership entry for every sequence in a claimed interval, not merely the
// graph-closure records.
func AssembleBundle(ctx context.Context, artifacts bundleArtifacts, log cll.Backend, logID string, options BundleOptions) (map[string]interface{}, error) {
	if len(options.Root) != 64 {
		return nil, inputError("--root is required")
	}
	// A negative depth means "unset" (the default); an explicit 0 is honored as a
	// root-only bundle. The bundle command's flag default is 2.
	if options.ClosureDepth < 0 {
		options.ClosureDepth = 2
	}
	if options.Payloads == "" {
		options.Payloads = "selected"
	}
	if options.Payloads != "all" && options.Payloads != "selected" {
		return nil, inputError("--payloads must be all or selected")
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
		return nil, inputError("log has no covering checkpoint")
	}
	checkpointSize := state.Checkpoint.Size
	checkpointSeq := state.Checkpoint.IndexedSeq
	if checkpointSeq == 0 {
		return nil, inputError("covering checkpoint has no entries")
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
		"checkpoint":   map[string]interface{}{"root": hex.EncodeToString(rangeRoot), "mmr_size": integer(checkpointSize), "statement": base64.StdEncoding.EncodeToString(state.Checkpoint.Bytes)},
		"verification": map[string]interface{}{"producer": "capsulectl", "checks": []interface{}{"graph_closure", "interval_coverage", "per_record_membership"}},
	}
	if options.WithDisclosure {
		overlay, disclosureErr := disclosureOverlay(ctx, artifacts, ids, options.Suppress)
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
	return capsule, nil
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

func disclosureOverlay(ctx context.Context, artifacts bundleArtifacts, ids []string, suppress map[string]bool) (map[string]interface{}, error) {
	// Originals are looked up by the same names seal() persists. A missing or
	// purged original is intentionally absent, which the verifier reports as WITHHELD.
	result := make(map[string]interface{})
	for _, id := range ids {
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

func validateViewRoot(bundle map[string]interface{}, root string) error {
	reject := func(found string) error {
		return inputError("view requires the root to be a disclosed evaluation-summary/v1 aggregate; found " + found)
	}
	records, _ := bundle["records"].([]interface{})
	disclosures, _ := bundle["disclosures"].(map[string]interface{})
	for _, value := range records {
		record, _ := value.(map[string]interface{})
		if record["capsule_id"] != root {
			continue
		}
		attestation, _ := record["model_attestation"].(map[string]interface{})
		compute, _ := attestation["compute_attestation"].(map[string]interface{})
		digest, _ := compute["agent_input_digest"].(string)
		members, ok := disclosures[digest].(map[string]interface{})
		if !ok {
			// Match the renderer's Capsule-ID fallback for assembled disclosures.
			members, _ = disclosures[root].(map[string]interface{})
		}
		input, present := members["agent_input"]
		if !present {
			return reject("undisclosed agent_input")
		}
		payload, ok := input.(map[string]interface{})
		if !ok {
			return reject("non-object agent_input")
		}
		if payload["spec_version"] != "evaluation-summary/v1" {
			return reject(fmt.Sprintf("agent_input spec_version=%v", payload["spec_version"]))
		}
		return nil
	}
	return reject("missing root record")
}

func bundleCommands() []*cobra.Command {
	shortFor := map[string]string{
		"bundle":    "Assemble a self-verifying Evidence Bundle from the root's citation closure",
		"disclose":  "Assemble an Evidence Bundle with disclosed agent_input/agent_output originals",
		"permalink": "Mint a viewer permalink over a disclosed Evidence Bundle",
		"view":      "Write a disclosed Evidence Bundle evidence-graph drill-down HTML file",
	}
	makeCommand := func(use string, disclosure bool, permalink bool, view bool) *cobra.Command {
		command := &cobra.Command{Use: use, Short: shortFor[use], Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
			out, _ := c.Flags().GetString("out")
			if view && out == "" {
				return inputError("--out is required")
			}
			profile, err := selected(c)
			if err != nil {
				return err
			}
			root, _ := c.Flags().GetString("root")
			closureDepth, _ := c.Flags().GetInt("closure-depth")
			payloads, _ := c.Flags().GetString("payloads")
			suppressNames, _ := c.Flags().GetStringSlice("suppress")
			suppressSet := make(map[string]bool, len(suppressNames))
			for _, name := range suppressNames {
				if name != "agent_input" && name != "agent_output" {
					return inputError("--suppress must name agent_input or agent_output")
				}
				suppressSet[name] = true
			}
			if view && suppressSet["agent_input"] {
				return inputError("view cannot suppress agent_input: the root aggregate must be disclosed")
			}
			target, err := openTarget(c.Context(), profile, usePublication)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, target.close()) }()
			value, err := AssembleBundle(c.Context(), target.artifacts, target.log, profile.LogID, BundleOptions{Root: root, ClosureDepth: closureDepth, Payloads: payloads, Suppress: suppressSet, WithDisclosure: disclosure || permalink || view})
			if err != nil {
				return err
			}
			if view {
				if err := validateViewRoot(value, root); err != nil {
					return err
				}
				html, err := emitter.EmitEvidenceGraphHTML(value, evidenceGraphIIFE)
				if err != nil {
					return err
				}
				if err := atomicFile(out, []byte(html), false); err != nil {
					return err
				}
				_, err = fmt.Fprintf(c.OutOrStdout(), "wrote Evidence Bundle view to %s\n", out)
				return err
			}
			if permalink {
				fragment, err := aacbundle.EncodeFragment(value)
				if err != nil {
					return err
				}
				decoded, err := aacbundle.DecodeFragment(fragment)
				if err != nil {
					return err
				}
				if _, ok := decoded.(map[string]interface{}); !ok {
					return errors.New("permalink fragment did not round-trip")
				}
				base, _ := c.Flags().GetString("base-url")
				if base == "" {
					base = defaultBundleURL
				}
				_, err = fmt.Fprintln(c.OutOrStdout(), strings.TrimRight(base, "#")+"#"+fragment)
				return err
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if out != "" {
				return atomicFile(out, encoded, false)
			}
			_, err = c.OutOrStdout().Write(append(encoded, '\n'))
			return err
		}}
		command.Flags().String("root", "", "Root Capsule ID")
		command.Flags().Int("closure-depth", 2, "Citation closure traversal depth from the root")
		if disclosure || permalink || view {
			command.Flags().String("payloads", "all", "Disclosure mode: all or selected")
			command.Flags().StringSlice("suppress", nil, "Disclosed member to withhold (agent_input or agent_output)")
		}
		if !permalink {
			outUsage := "Write the Evidence Bundle JSON to a new file"
			if view {
				outUsage = "Write the evidence-graph HTML to a new file"
			}
			command.Flags().String("out", "", outUsage)
		} else {
			command.Flags().String("base-url", defaultBundleURL, "Bundle viewer base URL")
		}
		return command
	}
	return []*cobra.Command{makeCommand("bundle", false, false, false), makeCommand("disclose", true, false, false), makeCommand("permalink", true, true, false), makeCommand("view", true, false, true)}
}
