package cli

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/mmr"
	"github.com/spf13/cobra"
)

const defaultBundleURL = "https://verify.agentactioncapsule.org/bundle"

type bundleArtifacts interface {
	Get(context.Context, string) (artifact.Record, error)
}

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
	if options.ClosureDepth == 0 {
		options.ClosureDepth = 2
	}
	if options.ClosureDepth < 0 {
		return nil, inputError("closure depth must not be negative")
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
	firstProof, err := tree.InclusionProof(0, checkpointSize)
	if err != nil {
		return nil, err
	}
	lastProof, err := tree.InclusionProof(checkpointSeq-1, checkpointSize)
	if err != nil {
		return nil, err
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
			"first_digest": hex.EncodeToString(entries[0].Value), "last_digest": hex.EncodeToString(entries[len(entries)-1].Value),
			"range_proof": map[string]interface{}{"from_seq": integer(1), "to_seq": integer(checkpointSeq), "size": integer(checkpointSize), "inclusion_from": proofJSON(firstProof), "inclusion_to": proofJSON(lastProof)},
			"memberships": memberships,
		},
		"checkpoint":   map[string]interface{}{"root": hex.EncodeToString(rangeRoot), "mmr_size": integer(checkpointSize), "statement": base64.StdEncoding.EncodeToString(state.Checkpoint.Bytes)},
		"verification": map[string]interface{}{"producer": "capsulectl", "checks": []interface{}{"graph_closure", "interval_coverage", "per_record_membership"}},
	}
	if options.WithDisclosure {
		overlay, disclosureErr := disclosureOverlay(ctx, artifacts, ids, options.Suppress)
		if disclosureErr != nil {
			return nil, disclosureErr
		}
		bundle["disclosures"] = overlay
	}
	if err := verifyProducedBundle(bundle, options.WithDisclosure); err != nil {
		return nil, err
	}
	return bundle, nil
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
	records := map[string]map[string]interface{}{root["capsule_id"].(string): root}
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
					missingSet[id] = true
					continue
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
		batch, err := log.ScanEntries(ctx, after, cll.MaxScanLimit)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		if batch[len(batch)-1].Seq <= after {
			return nil, errors.New("log scan did not advance")
		}
		entries = append(entries, batch...)
		after = batch[len(batch)-1].Seq
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

func bundleCommands() []*cobra.Command {
	makeCommand := func(use string, disclosure bool, permalink bool) *cobra.Command {
		command := &cobra.Command{Use: use, Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
			profile, err := selected(c)
			if err != nil {
				return err
			}
			root, _ := c.Flags().GetString("root")
			payloads, _ := c.Flags().GetString("payloads")
			suppressNames, _ := c.Flags().GetStringSlice("suppress")
			suppressSet := make(map[string]bool, len(suppressNames))
			for _, name := range suppressNames {
				if name != "agent_input" && name != "agent_output" {
					return inputError("--suppress must name agent_input or agent_output")
				}
				suppressSet[name] = true
			}
			target, err := openTarget(c.Context(), profile, usePublication)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, target.close()) }()
			value, err := AssembleBundle(c.Context(), target.artifacts, target.log, profile.LogID, BundleOptions{Root: root, Payloads: payloads, Suppress: suppressSet, WithDisclosure: disclosure || permalink})
			if err != nil {
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
			out, _ := c.Flags().GetString("out")
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
		if disclosure || permalink {
			command.Flags().String("payloads", "all", "Disclosure mode: all or selected")
			command.Flags().StringSlice("suppress", nil, "Disclosed member to withhold (agent_input or agent_output)")
		}
		if !permalink {
			command.Flags().String("out", "", "Write the Evidence Bundle JSON to a new file")
		} else {
			command.Flags().String("base-url", defaultBundleURL, "Bundle viewer base URL")
		}
		return command
	}
	return []*cobra.Command{makeCommand("bundle", false, false), makeCommand("disclose", true, false), makeCommand("permalink", true, true)}
}
