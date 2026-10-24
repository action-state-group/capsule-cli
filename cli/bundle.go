package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/action-state-group/capsule-viewer/go/emitter"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/envelope"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/spf13/cobra"
)

const DefaultBundleURL = "https://verify.agentactioncapsule.org/bundle"

type BundleArtifacts interface {
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
	// extension (ProducerKeyExtensionKind). Nil emits no extension.
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
const ProducerKeyExtensionKind = "producer-key/v1"

// declaredProducerKey reads a bundle's producer-key/v1 public_key, accepting
// only the form the AAC viewer accepts (64 lowercase hex). Anything else is
// treated as no declaration.
func DeclaredProducerKey(bundle map[string]interface{}) (ed25519.PublicKey, bool) {
	extensions, _ := bundle["extensions"].(map[string]interface{})
	block, _ := extensions[ProducerKeyExtensionKind].(map[string]interface{})
	value, _ := block["public_key"].(string)
	if len(value) != 2*ed25519.PublicKeySize || !IsLowerHex(value) {
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
			keys, err := ParseKeys([]string{value})
			if err != nil {
				return nil, InputError("--producer-key must be a 32-byte Ed25519 public key in hex")
			}
			return keys[0], nil
		}
	}
	if profile.Signing == (Secret{}) {
		return nil, nil
	}
	key, err := PrivateKey(profile.Signing)
	if err != nil {
		return nil, err
	}
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, InputError("the profile's signing key is not an Ed25519 key")
	}
	return public, nil
}

func Integer(value uint64) json.Number { return json.Number(fmt.Sprintf("%d", value)) }

// AssembleBundle builds a self-verifying v2 Evidence Bundle with AAC's
// producer (bundle.BuildEvidenceBundle) over the log and the artifact store.
// The proof range deliberately spans the checkpointed log: the authoritative
// verifier requires a membership entry for every sequence in a claimed
// interval, not merely the graph-closure records. What stays here is the CLI's
// policy: which originals are disclosed, which records are withheld, and the
// payloads mode the command claims.
func AssembleBundle(ctx context.Context, artifacts BundleArtifacts, log cll.Backend, logID string, options BundleOptions) (map[string]interface{}, error) {
	if len(options.Root) != 64 {
		return nil, InputError("--root is required: the Capsule ID (64 hex) the bundle is built around")
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
		return nil, InputError("--payloads must be all, selected, or none")
	}
	// "none" is a digests-only bundle (no disclosures overlay at all), the
	// withheld form a countersign request submits; it is not a disclosure mode.
	// The draft's payloads_mode has no "none": such a bundle states selected.
	if options.Payloads == "none" && options.WithDisclosure {
		return nil, InputError("--payloads none cannot be combined with disclosure")
	}
	if options.Payloads == "all" && len(options.Withhold) != 0 {
		return nil, InputError("--payloads all discloses every record, so it cannot withhold any: a shared copy uses --payloads selected")
	}
	if options.ProducerKey != nil && len(options.ProducerKey) != ed25519.PublicKeySize {
		return nil, InputError("producer key must be a 32-byte Ed25519 public key")
	}

	source := &bundleSource{ProofSource: aacbundle.ProofSourceFromCLL(log, bundleRecords{artifacts})}
	leaves, err := source.Leaves(ctx)
	if err != nil {
		return nil, err
	}
	payloads := options.Payloads
	if payloads == "none" {
		payloads = "selected"
	}
	var overlay map[string]map[string]interface{}
	if options.WithDisclosure {
		if payloads, overlay, err = disclosures(ctx, artifacts, leaves, payloads, options); err != nil {
			return nil, err
		}
	}
	suppressed := make([]string, 0, len(options.Suppress))
	for member := range options.Suppress {
		suppressed = append(suppressed, member)
	}
	sort.Strings(suppressed)
	depth := options.ClosureDepth
	bundle, err := aacbundle.BuildEvidenceBundle(ctx, source, aacbundle.BuildOptions{
		Root: options.Root, IntervalFrom: 1, ClosureDepth: &depth,
		Disclosures: overlay, SuppressedFields: suppressed, PayloadsMode: payloads,
		ProducerKey:  options.ProducerKey,
		Verification: map[string]interface{}{"producer": "capsulectl", "checks": []interface{}{"graph_closure", "interval_coverage", "per_record_membership"}},
	})
	if err != nil {
		return nil, err
	}
	if certificate, _ := bundle["completeness_certificate"].(map[string]interface{}); certificate["log_id"] != logID {
		return nil, fmt.Errorf("the log's checkpoint names log %v, not %s", certificate["log_id"], logID)
	}
	if err := VerifyProducedBundle(bundle, options.WithDisclosure); err != nil {
		return nil, err
	}
	return bundle, nil
}

// disclosures is the overlay a disclosing bundle carries, read from the
// artifact store for every record on the log (the bundle carries them all),
// and the payloads mode it can honestly claim.
func disclosures(ctx context.Context, artifacts BundleArtifacts, leaves [][]byte, payloads string, options BundleOptions) (string, map[string]map[string]interface{}, error) {
	ids := make([]string, len(leaves))
	records := make(map[string]map[string]interface{}, len(leaves))
	for i, leaf := range leaves {
		ids[i] = hex.EncodeToString(leaf)
		record, err := GetCapsule(ctx, artifacts, ids[i])
		if errors.Is(err, artifact.ErrNotFound) {
			return "", nil, unheldEntry(uint64(i + 1))
		}
		if err != nil {
			return "", nil, fmt.Errorf("checkpointed record %s is unavailable: %w", ids[i], err)
		}
		records[ids[i]] = record
	}
	// A disclosure capsule's input says what was disclosed to someone. A
	// later bundle proves its place in the log but withholds its input, so a
	// copy made for one party never shows what was disclosed to another; the
	// bundle then states payloads selected, which is what it is.
	withhold := make(map[string]bool, len(options.Withhold))
	for id, v := range options.Withhold {
		withhold[id] = v
	}
	for id, record := range records {
		if record["action_id"] == disclosureActionID && record["action_type"] == "fyi" {
			withhold[id] = true
			if payloads == "all" {
				payloads = "selected"
			}
		}
	}
	overlay, err := DisclosureOverlay(ctx, artifacts, ids, options.Suppress, withhold)
	if err != nil {
		return "", nil, err
	}
	// payloads=all is a claim to disclose EVERY committed eligible member that
	// is not suppressed. If any such member's original is not retained it would
	// verify as WITHHELD, making "all" a false claim -- refuse rather than emit it.
	if payloads == "all" {
		for _, id := range ids {
			members, _ := overlay[id].(map[string]interface{})
			for _, member := range committedEligibleMembers(records[id]) {
				if options.Suppress[member] {
					continue
				}
				// Test presence, not nil value: a retained original that is JSON
				// null is present (DE-3 validates the value), so `== nil` would
				// wrongly reject it as not retained.
				if _, ok := members[member]; members == nil || !ok {
					return "", nil, InputError(fmt.Sprintf("payloads=all cannot be honored: the original for %s of %s is not retained", member, id))
				}
			}
		}
	}
	byID := make(map[string]map[string]interface{}, len(overlay))
	for id, members := range overlay {
		byID[id], _ = members.(map[string]interface{})
	}
	return payloads, byID, nil
}

// unheldEntry is the refusal for a checkpointed entry the store does not
// hold: before disclosures were sealed as capsules, disclose appended a
// disclosure record's bare digest, kept nowhere else; or a record was lost.
// The two cannot be told apart, and either way no bundle can supply it, since
// the verifier requires every entry.
func unheldEntry(seq uint64) error {
	return InputError(fmt.Sprintf("log entry %d is checkpointed but not in this profile's store: either a disclosure an earlier release appended as a bare digest, or a lost record; no bundle over this log can include it; start a new profile (or log id) for new reports", seq))
}

// bundleRecords is the artifact store as AAC's RecordSource.
type bundleRecords struct{ artifacts BundleArtifacts }

func (r bundleRecords) GetRecord(ctx context.Context, id string) ([]byte, []byte, error) {
	record, err := r.artifacts.Get(ctx, id)
	if errors.Is(err, artifact.ErrNotFound) {
		return nil, nil, fmt.Errorf("%w: %s", aacbundle.ErrRecordNotFound, id)
	}
	if err != nil {
		return nil, nil, err
	}
	return record.Capsule, record.ProducerEnvelope, nil
}

// bundleSource is AAC's CLL proof source with capsulectl's refusals, which
// name the command that fixes them. It keeps the leaves it read, so the
// producer and the disclosure overlay see the same scan.
type bundleSource struct {
	aacbundle.ProofSource
	leaves [][]byte
	seqOf  map[string]uint64
}

func (s *bundleSource) Leaves(ctx context.Context) ([][]byte, error) {
	if s.seqOf != nil {
		return s.leaves, nil
	}
	leaves, err := s.ProofSource.Leaves(ctx)
	var refused *aacbundle.BuildError
	if errors.As(err, &refused) {
		// The CLL source's only refusal here is a log with no checkpoint.
		return nil, InputError("the log has no checkpoint yet, so no bundle can prove its records: cut one with `capsulectl cll checkpoint create`")
	}
	if err != nil {
		return nil, err
	}
	if len(leaves) == 0 {
		return nil, InputError("the log's checkpoint covers no entries: append a record and cut a new checkpoint with `capsulectl cll checkpoint create`")
	}
	s.leaves, s.seqOf = leaves, make(map[string]uint64, len(leaves))
	for i, leaf := range leaves {
		if id := hex.EncodeToString(leaf); s.seqOf[id] == 0 {
			s.seqOf[id] = uint64(i + 1)
		}
	}
	return leaves, nil
}

// Record refuses a checkpointed entry the store does not hold, which no
// bundle over the whole log can carry. A citation outside the log stays
// missing, as the producer states it.
func (s *bundleSource) Record(ctx context.Context, id string) (aacbundle.StoredRecord, bool, error) {
	record, found, err := s.ProofSource.Record(ctx, id)
	if err == nil && !found && s.seqOf[id] != 0 {
		return record, false, unheldEntry(s.seqOf[id])
	}
	return record, found, err
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

func GetCapsule(ctx context.Context, artifacts BundleArtifacts, id string) (map[string]interface{}, error) {
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

func DisclosureOverlay(ctx context.Context, artifacts BundleArtifacts, ids []string, suppress, withhold map[string]bool) (map[string]interface{}, error) {
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

func VerifyProducedBundle(value map[string]interface{}, disclosuresRequired bool) error {
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
// command-layer helper (and sealDisclosure below) knows about the log.
func DisclosureRecord(bundle map[string]interface{}) (map[string]interface{}, error) {
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
	// A standalone report (capsule-cli#294) is disclosed beside the records:
	// the copy reveals its input too.
	exts, _ := bundle["extensions"].(map[string]interface{})
	if x, ok := exts[DealReportExtension].(map[string]interface{}); ok {
		record, _ := x["record"].(map[string]interface{})
		if id, _ := record["capsule_id"].(string); id != "" {
			digest, err := canonical.JSONDigest(x["agent_input"])
			if err != nil {
				return nil, fmt.Errorf("digest the standalone report %s: %w", id, err)
			}
			revealed[id] = map[string]interface{}{"agent_input": digest}
		}
	}
	return map[string]interface{}{
		"type":              "disclosure_record",
		"root":              bundle["root"],
		"payloads_mode":     completeness["payloads_mode"],
		"suppressed_fields": completeness["suppressed_fields"],
		"revealed":          revealed,
	}, nil
}

// sealDisclosure puts a disclose act on the log: the disclosure record (ids,
// digests and labels only) sealed as an fyi capsule with the profile's
// signing key, stored, and appended like any record. A later bundle supplies
// it as a record, so a checkpoint over it never stops another report.
func sealDisclosure(ctx context.Context, t *Target, p Profile, bundle map[string]interface{}) (Publication, error) {
	record, err := DisclosureRecord(bundle)
	if err != nil {
		return Publication{}, err
	}
	// The capsule commits to the record by an unsalted digest, which a later
	// bundle shows. The record's other fields have few possible values, so
	// without a nonce a party could confirm a guess at what was disclosed to
	// another; with one, two identical disclosures seal different digests.
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return Publication{}, err
	}
	record["nonce"] = hex.EncodeToString(nonce)
	operator := p.Operator
	if operator == "" {
		operator = "capsulectl"
	}
	raw, err := json.Marshal(map[string]interface{}{
		"spec_version": "capsule-seal-request/v1",
		"capsule": map[string]interface{}{
			"ActionID": disclosureActionID, "ActionType": "fyi", "Operator": operator, "Developer": "capsulectl",
			"Timestamp": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		},
		"payload": record,
	})
	if err != nil {
		return Publication{}, err
	}
	request, err := ParseRequest(raw)
	if err != nil {
		return Publication{}, err
	}
	private, err := PrivateKey(p.Signing)
	if err != nil {
		return Publication{}, err
	}
	if err = RequirePublisherKey(p, private); err != nil {
		return Publication{}, err
	}
	return t.Publish(ctx, request, private)
}

// appendRecordDigest appends a disclosure record's own digest as a log entry.
func AppendRecordDigest(ctx context.Context, log cll.Backend, record map[string]interface{}) (cll.Entry, error) {
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

// disclosureActionID is the action_id of the capsule a disclose seals to put
// the disclosure act on the log: an fyi capsule whose input is the
// disclosure record (ids, digests and labels, never a disclosed byte).
const disclosureActionID = "capsulectl-disclosure"

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
		if !LogName.MatchString(logID) {
			return p, InputError("--log-id must be lowercase letters, digits and ._:/-, such as a profile's log_id")
		}
	case p.LogID == "":
		return p, InputError("the profile has no log_id: name the log with --log-id, or a deal with --deal")
	case p.Namespace == "deal" && strings.HasPrefix(p.LogID, "deal-cadence/"):
		return p, InputError("this is a deal profile: each deal is its own log; name the deal with --deal DEAL_ID")
	default:
		return p, nil
	}
	if strings.HasPrefix(logID, "deal/") {
		return p, InputError("a deal's logs are read with --deal DEAL_ID, which applies the deal's share rules; --log-id does not")
	}
	if root == "" {
		return p, InputError("--root is required: the Capsule ID (64 hex) the bundle is built around")
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
		command := &cobra.Command{Use: use, Short: shortFor[use], Args: NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
			htmlPath, _ := c.Flags().GetString("html")
			if htmlPath != "" && permalink {
				return InputError("--html writes a page holding the bundle; a permalink carries the bundle in its link. Use `bundle --html FILE` or `disclose --html FILE` for a page")
			}
			if dealID, _ := c.Flags().GetString("deal"); dealID != "" {
				if htmlPath != "" {
					return InputError("a deal's page is its receipt: `deal report --deal ID --html FILE` (or `--share ... --html FILE` for someone else)")
				}
				if DealBundleRun == nil {
					return ForwardToPlugin(c, DealPluginName, CommandLine(c, nil))
				}
				return DealBundleRun(c, use)
			}
			if c.Flags().Changed("share") || c.Flags().Changed("to") {
				return InputError("--share and --to go with --deal: they choose what a deal's shared copy withholds")
			}
			profile, err := Selected(c)
			if err != nil {
				return err
			}
			if done, err := delegateBook(c, profile, nil); done {
				return err
			}
			if htmlPath != "" && profile.IsBook() && use == "disclose" {
				return errJSONLPage // before anything is opened or put on record
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
					return InputError("--suppress must name agent_input or agent_output")
				}
				suppressSet[name] = true
			}
			producerKey, err := bundleProducerKey(c, profile)
			if err != nil {
				return err
			}
			target, err := OpenTarget(c.Context(), profile, UsePublication)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, target.Close()) }()
			var value map[string]interface{}
			var encoded []byte
			if target.Book != nil {
				// A jsonl profile's bundle comes from its book, which puts every
				// bundle it builds on record as a disclosure record.
				attach := false
				if disclosure || permalink {
					attach, _ = c.Flags().GetBool("attach-input-originals")
				}
				bundle, err := target.Book.Bundle(c.Context(), BookBundleRequest{Root: root, ClosureDepth: closureDepth, Payloads: payloads, Suppress: suppressSet, Disclose: disclosure || permalink, AttachOriginals: attach, ProducerKey: producerKey})
				if err != nil {
					return err
				}
				if value, err = DecodeBundleJSON(bundle); err != nil {
					return err
				}
				encoded = bundle
			} else if value, err = AssembleBundle(c.Context(), target.Artifacts, target.Log, profile.LogID, BundleOptions{Root: root, ClosureDepth: closureDepth, Payloads: payloads, Suppress: suppressSet, WithDisclosure: disclosure || permalink, ProducerKey: producerKey}); err != nil {
				return err
			}
			// The page's verdict, before anything is put on record. A refused
			// page is never written. disclose then stops, so no disclosure
			// goes on record without its page; bundle still writes the JSON
			// (verify --bundle states its verdict) and exits with the refusal.
			var pageErr error
			if htmlPath != "" {
				if profile.IsBook() {
					pageErr = errJSONLPage
				} else {
					pageErr = PageGate(value)
				}
				if pageErr != nil && use == "disclose" {
					return pageErr
				}
			}
			if use == "disclose" && target.Book == nil {
				// Every disclose act goes on record: sealed as a capsule and
				// appended before the bundle is emitted, not merely alongside it.
				if _, err := sealDisclosure(c.Context(), target, profile, value); err != nil {
					return err
				}
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
					base = DefaultBundleURL
				}
				_, err = fmt.Fprintln(c.OutOrStdout(), strings.TrimRight(base, "#")+"#"+fragment)
				return err
			}
			if htmlPath != "" && pageErr == nil {
				// One self-contained page: the bundle embedded and
				// agent-action-capsule's own evidence-graph verifier, vendored
				// (assets/evidence-graph.iife.js), which checks it with no
				// network when the page is opened.
				page, err := emitter.EmitEvidenceGraphHTML(value, EvidenceGraphIIFE)
				if err != nil {
					return err
				}
				if err = AtomicFile(htmlPath, []byte(page), false); err != nil {
					return err
				}
			}
			out, _ := c.Flags().GetString("out")
			if encoded == nil {
				if encoded, err = json.Marshal(value); err != nil {
					return err
				}
			}
			if out != "" {
				return errors.Join(AtomicFile(out, encoded, false), pageErr)
			}
			_, err = c.OutOrStdout().Write(append(encoded, '\n'))
			return errors.Join(err, pageErr)
		}}
		command.Flags().String("root", "", "Root Capsule ID")
		command.Flags().String("deal", "", "A deal on a deal profile: the whole deal from its own log, built as `deal report` builds it (disclose also needs --share and --to; a deal is not shared as a link)")
		command.Flags().String("share", DealAudienceKeep, "With --deal: who the copy is for: keep (bundle only: your own copy), counterparty or adjudicator (disclose only: a shared copy, put on record first)")
		command.Flags().String("to", "", "With --deal and --share: who the shared copy is for, as sealed in the disclosure record")
		command.Flags().String("log-id", "", "Read this log instead of the profile's log_id")
		command.Flags().Int("closure-depth", 2, "Citation closure traversal depth from the root")
		command.Flags().String("producer-key", "", producerKeyFlagUsage)
		if !permalink {
			command.Flags().String("html", "", "Also write the bundle as one self-contained page that checks itself offline (the bundle and the vendored verifier) to this new file")
		} else {
			command.Flags().String("html", "", "Not for permalink: a link carries the bundle; use bundle or disclose --html")
		}
		if disclosure || permalink {
			command.Flags().String("payloads", "all", "Disclosure mode: all or selected")
			command.Flags().StringSlice("suppress", nil, "Disclosed member to withhold (agent_input or agent_output)")
			command.Flags().Bool("attach-input-originals", false, "Book profiles: also carry each published capsule's retained agent_input original in the bundle (extension "+InputOriginalsExtension+"), read from the artifact store and checked against its agent_input_digest")
		}
		if !permalink {
			command.Flags().String("out", "", "Write the Evidence Bundle JSON to a new file")
		} else {
			command.Flags().String("base-url", DefaultBundleURL, "Bundle viewer base URL")
		}
		return command
	}
	return []*cobra.Command{makeCommand("bundle", false, false), makeCommand("disclose", true, false), makeCommand("permalink", true, true)}
}

// InputOriginalsExtension is the bundle extension that carries, on opt-in at
// disclose time (--attach-input-originals), the agent_input original of each
// published capsule in a book profile's bundle: a JSON object from
// capsule_id to the original's exact bytes, base64url without padding, read
// from the artifact store. It is part of the bundle the disclosure record
// commits to. A reader checks each original's JSON-DIGEST against the
// agent_input_digest of the capsule it names: the bundle record itself (a
// book profile's own capsule is its record), or, for a capsule an older
// capsulectl wrapped, the capsule the record's first payload carries.
const InputOriginalsExtension = "capsulectl/agent-input-originals/v1"
