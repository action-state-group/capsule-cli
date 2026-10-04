package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/net/publicsuffix"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/mmr"
	"github.com/action-state-group/cll-go/witness"
	"github.com/spf13/cobra"
)

// Proof uses the library's exact checkpoint COSE bytes and structured MMR
// inclusion proof. Its byte slices encode as JSON base64. Absence of an
// inclusion proof is reported as partial.
type Proof struct {
	Checkpoint     []byte              `json:"checkpoint"`
	CapsuleID      string              `json:"capsule_id,omitempty"`
	InclusionProof *mmr.InclusionProof `json:"inclusion_proof,omitempty"`
}

func verifyCheckpoint(p Profile, raw []byte) (checkpoint.Record, error) {
	r, e := checkpoint.ParseRecord(raw)
	if e != nil {
		return r, e
	}
	if r.LogID != p.LogID {
		return r, ErrConflict
	}
	if e = r.VerifySignature(); e != nil {
		return r, e
	}
	if !checkpointSignerTrusted(p, r.KeyID) {
		return r, errors.New("checkpoint signer not trusted")
	}
	return r, nil
}
func checkpointSignerTrusted(p Profile, keyID string) bool {
	for _, key := range p.Checkpoint.TrustedKeys {
		if strings.EqualFold(key, keyID) {
			return true
		}
	}
	return false
}

// witnessKeyIssue says what is wrong with the witness public key a profile
// with a checkpoint endpoint must carry, as text to show the operator; ""
// when it is set and shaped as a 64-hex Ed25519 key.
func witnessKeyIssue(p Profile) string {
	if p.Checkpoint.Endpoint == "" {
		return ""
	}
	fix := "capsulectl profile update --profile " + p.Name + " --checkpoint-public-key <64-hex Ed25519 key>"
	if p.Checkpoint.PublicKey == "" {
		return "checkpoint.public_key is required when checkpoint.endpoint is set: " + fix
	}
	if _, e := parseKeys([]string{p.Checkpoint.PublicKey}); e != nil {
		return "checkpoint.public_key must be the witness's Ed25519 public key as 64 hex characters: " + fix
	}
	return ""
}

// checkWitnessConfig refuses a checkpoint endpoint that serviceID could
// never use, naming the field and the fix (shown verbatim, unlike a plain
// input error).
func checkWitnessConfig(p Profile) error {
	endpoint := strings.TrimRight(p.Checkpoint.Endpoint, "/")
	if endpoint == "" {
		return nil
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return hint(ErrInput, "checkpoint.endpoint must be an HTTPS URL without credentials, query or fragment")
	}
	if issue := witnessKeyIssue(p); issue != "" {
		return hint(ErrInput, issue)
	}
	return nil
}

func serviceID(p Profile) (string, error) {
	endpoint := strings.TrimRight(p.Checkpoint.Endpoint, "/")
	if endpoint == "" {
		return "", nil
	}
	if e := checkWitnessConfig(p); e != nil {
		return "", e
	}
	sum := sha256.Sum256([]byte(endpoint + "\n" + strings.ToLower(p.Checkpoint.PublicKey)))
	return "service-" + hex.EncodeToString(sum[:]), nil
}

// Bearer auth is attached only by the library's redirect-rejecting HTTP client.
type bearerTransport struct {
	token string
	base  http.RoundTripper // nil uses the platform's verified TLS transport
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	if b.token != "" {
		copy.Header.Set("Authorization", "Bearer "+b.token)
	}
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(copy)
}

type safeSubmitter struct{ client *witness.Client }

func (s safeSubmitter) Submit(ctx context.Context, b []byte) (witness.Receipt, error) {
	r, e := s.client.Submit(ctx, b)
	if e != nil {
		// A request that got no answer from the witness at all (refused,
		// unresolved, timed out, or stopped by a proxy asking for
		// authorization) is recorded as not reached, so it can be shown as
		// such: on an agent host that asks before network access, that is
		// what a missing consent looks like from here.
		var answered *witness.HTTPError
		if !errors.As(e, &answered) || answered.StatusCode == http.StatusProxyAuthRequired {
			return r, &witness.HTTPError{StatusCode: 503, Body: witnessNotReached}
		}
		status := 400
		if witness.IsRetryable(e) {
			status = 503
		}
		return r, &witness.HTTPError{StatusCode: status, Body: "checkpoint submission failed; response details suppressed"}
	}
	return r, nil
}

// witnessNotReached marks a delivery whose request never got an answer from
// the witness.
const witnessNotReached = "witness not reached; response details suppressed"

// witnessSite is the site an agent host's per-site network grant names for
// a witness endpoint: its registrable domain (witness.example.org ->
// example.org), or the host itself when it has none (an IP address).
func witnessSite(endpoint string) (host, site string) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return endpoint, endpoint
	}
	host = u.Hostname()
	if site, err = publicsuffix.EffectiveTLDPlusOne(host); err != nil {
		site = host
	}
	return host, site
}

// witnessAllowText is the plain instruction for an agent host that asks
// before network access.
func witnessAllowText(endpoint string) string {
	host, site := witnessSite(endpoint)
	return fmt.Sprintf("If your agent host asks before network access, choose \"Always allow this site\" for %s (the witness is %s), not \"allow once\": ticks run on a schedule, when nobody is there to answer a prompt. That grant covers %s and all its subdomains.", site, host, site)
}

// witnessConsentText says what a delivery that did not reach the witness
// most likely needs, without claiming to know: this program cannot see an
// agent host's network-consent prompt.
func witnessConsentText(endpoint string) string {
	return "pending: network consent needed, or no network. The request did not reach the witness. " + witnessAllowText(endpoint)
}

// witnessConsentReached is doctor's note when its own probe got through.
func witnessConsentReached(endpoint string) string {
	return "Reached from this command. A scheduled tick runs without anyone present. " + witnessAllowText(endpoint)
}

// witnessPendingReason classifies a pending delivery from its stored error.
func witnessPendingReason(state cll.WitnessState, endpoint string) (string, string) {
	if strings.Contains(state.LastError, witnessNotReached) {
		return "network_consent_needed", witnessConsentText(endpoint)
	}
	if state.Attempts == 0 {
		return "not_attempted", "pending: not sent yet; it goes at the next tick."
	}
	return "witness_error", "pending: the witness answered with an error; it is retried at every tick."
}

// A scoped view prevents one command from delivering unrelated checkpoint rows.
type selectedWitness struct {
	cll.WitnessStateStore
	id   string
	size uint64
}

func (s selectedWitness) PendingWitnesses(ctx context.Context, now time.Time, _ int) ([]cll.WitnessState, error) {
	r, e := s.GetWitness(ctx, s.id, s.size)
	if e != nil {
		return nil, e
	}
	if r.Receipt != nil || r.Permanent || r.NextAttemptAt.After(now) {
		return nil, nil
	}
	return []cll.WitnessState{r}, nil
}
func witnessResult(state cll.WitnessState) map[string]any {
	status := "pending"
	if state.Permanent {
		status = "failed"
	}
	if state.Receipt != nil {
		status = "verified"
	}
	return map[string]any{"state": status, "attempts": state.Attempts, "next_attempt_at": state.NextAttemptAt, "receipt": state.Receipt}
}

// Status re-verifies persisted evidence rather than trusting a receipt-shaped
// database row or an earlier successful network call.
func verifyWitness(p Profile, state cll.WitnessState) error {
	if state.Receipt == nil {
		return nil
	}
	r := state.Receipt
	if r.LeafIndex == nil || r.TreeSize == nil {
		return errors.New("incomplete stored receipt")
	}
	keys, err := parseKeys([]string{p.Checkpoint.PublicKey})
	if err != nil {
		return err
	}
	v, err := witness.NewReceiptVerifier(keys[0])
	if err != nil {
		return err
	}
	return v.Verify(state.Checkpoint, witness.Receipt{Bytes: r.Bytes, EntryHash: r.EntryHash, EntryHashScheme: r.EntryHashScheme, LeafIndex: *r.LeafIndex, TreeSize: *r.TreeSize})
}

func addCheckpointCommands(logs *cobra.Command) {
	verify := &cobra.Command{Use: "verify", Short: "Verify a signed checkpoint and optional Capsule inclusion proof offline", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := selected(c)
		if e != nil {
			return e
		}
		path, _ := c.Flags().GetString("proof")
		raw, e := readInput(path)
		if e != nil {
			return e
		}
		var proof Proof
		if e = decodeJSON(raw, &proof); e != nil {
			return e
		}
		r, e := verifyCheckpoint(p, proof.Checkpoint)
		if e != nil {
			return e
		}
		checks := map[string]string{"checkpoint_signature_and_trust": "passed", "log_id": "passed", "embedded_consistency": "passed", "inclusion": "not_performed", "witness_receipt": "not_performed", "producer_signature": "not_performed"}
		if proof.CapsuleID != "" {
			if proof.InclusionProof == nil {
				return inputError("proof is missing inclusion_proof; a proof minted before the structured-proof format (leaf_index + path) is no longer accepted — re-mint it")
			}
			root, e := hex.DecodeString(r.Root)
			if e != nil {
				return e
			}
			if !mmr.VerifyHexInclusion(root, r.MMRSize, proof.InclusionProof.LeafIndex, proof.CapsuleID, *proof.InclusionProof) {
				return errors.New("invalid inclusion proof")
			}
			checks["inclusion"] = "passed"
		}
		if e = output(c, checks); e != nil {
			return e
		}
		if proof.CapsuleID == "" {
			return ErrPartial
		}
		return nil
	}}
	verify.Flags().String("proof", "", "Proof JSON with a base64 checkpoint and structured inclusion_proof")
	logs.AddCommand(verify)
	group := &cobra.Command{Use: "checkpoint", Short: "Create signed checkpoints over the log"}
	logs.AddCommand(group)
	create := &cobra.Command{Use: "create", Short: "Cut a signed checkpoint at the current log tip", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := selected(c)
		if e != nil {
			return e
		}
		key, e := privateKey(p.Checkpoint.Signing)
		if e != nil {
			return e
		}
		signer, e := checkpoint.NewEd25519Signer(key)
		if e != nil {
			return e
		}
		if !checkpointSignerTrusted(p, signer.KeyID()) {
			return inputError("checkpoint signer must be explicitly trusted")
		}
		service, e := serviceID(p)
		if e != nil {
			return e
		}
		t, e := openTarget(c.Context(), p, useCLL)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.close()) }()
		if t.book != nil {
			result, e := checkpointBook(c.Context(), p, t.book.book, service)
			if e != nil {
				return e
			}
			return output(c, result)
		}
		cp, e := cutCheckpoint(c.Context(), p, t.log)
		if e != nil {
			return e
		}
		return output(c, map[string]any{"checkpoint": cp.Size, "indexed_sequence": cp.IndexedSeq, "statement": cp.Bytes, "log_id": p.LogID})
	}}
	group.AddCommand(create)
	for _, publish := range []bool{false, true} {
		verb := "status"
		if publish {
			verb = "publish"
		}
		cmd := &cobra.Command{Use: verb, Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
			p, e := selected(c)
			if e != nil {
				return e
			}
			size, _ := c.Flags().GetUint64("checkpoint")
			if size == 0 {
				return inputError("--checkpoint MMR size is required")
			}
			service, e := serviceID(p)
			if e != nil {
				return e
			}
			if service == "" {
				return inputError("checkpoint service not configured")
			}
			use := useCLLRead
			if publish {
				use = useCLL
			}
			t, e := openTarget(c.Context(), p, use)
			if e != nil {
				return e
			}
			defer func() { err = errors.Join(err, t.close()) }()
			var store cll.WitnessStateStore = t.log
			if p.Type == "jsonl" {
				store = newBookWitnessStore(p)
			}
			state, e := store.GetWitness(c.Context(), service, size)
			if e != nil {
				return e
			}
			record, e := verifyCheckpoint(p, state.Checkpoint)
			if e != nil {
				return e
			}
			if record.MMRSize != size {
				return ErrConflict
			}
			if publish {
				if state, e = deliverWitness(c.Context(), p, store, service, size); e != nil {
					return e
				}
			}
			if e = verifyWitness(p, state); e != nil {
				return e
			}
			if e = output(c, witnessResult(state)); e != nil {
				return e
			}
			if publish && state.Permanent {
				return errors.New("checkpoint delivery permanently failed")
			}
			if publish && state.Receipt == nil {
				return ErrPending
			}
			return nil
		}}
		cmd.Flags().Uint64("checkpoint", 0, "CLL checkpoint MMR size (not entry count)")
		group.AddCommand(cmd)
	}
}

// cutCheckpoint signs a checkpoint at the current log tip and returns it after
// re-verifying the stored statement. The checkpoint signer must be explicitly
// trusted by the profile.
func cutCheckpoint(ctx context.Context, p Profile, log cll.Backend) (*cll.CheckpointState, error) {
	return cutCheckpointAt(ctx, p, log, time.Now().UTC())
}

// cutCheckpointAt is cutCheckpoint with the time the checkpoint states.
func cutCheckpointAt(ctx context.Context, p Profile, log cll.Backend, at time.Time) (*cll.CheckpointState, error) {
	key, e := privateKey(p.Checkpoint.Signing)
	if e != nil {
		return nil, e
	}
	signer, e := checkpoint.NewEd25519Signer(key)
	if e != nil {
		return nil, e
	}
	if !checkpointSignerTrusted(p, signer.KeyID()) {
		return nil, inputError("checkpoint signer must be explicitly trusted")
	}
	service, e := serviceID(p)
	if e != nil {
		return nil, e
	}
	config := checkpoint.DefaultRunnerConfig(p.LogID)
	config.Cadence.CadenceEntries = 1
	if service != "" {
		config.WitnessIDs = []string{service}
	}
	runner, e := checkpoint.NewRunner(config, log, signer)
	if e != nil {
		return nil, e
	}
	// RunOnce indexes up to one cll ScanLimit batch and, with CadenceEntries=1,
	// cuts a checkpoint at that tip; it reports no change once the log is fully
	// caught up. Loop to drain a backlog, but bound the work: a log under
	// continuous concurrent append never reaches "no change", so an unbounded
	// loop could spin forever (the CLI context carries no deadline). The cap is
	// a per-invocation work budget; reaching it returns ErrPending so the
	// operator re-runs to continue. A single-writer log converges in the first
	// couple of batches.
	const maxCheckpointBatches = 10000
	for batch := 0; batch < maxCheckpointBatches; batch++ {
		changed, e := runner.RunOnce(ctx, at)
		if e != nil {
			return nil, e
		}
		if changed {
			continue
		}
		state, e := log.LoadCLL(ctx)
		if e != nil {
			return nil, e
		}
		if state.Checkpoint == nil {
			return nil, inputError("log has no entries")
		}
		record, e := verifyCheckpoint(p, state.Checkpoint.Bytes)
		if e != nil {
			return nil, e
		}
		if record.MMRSize != state.Checkpoint.Size {
			return nil, ErrConflict
		}
		return state.Checkpoint, nil
	}
	return nil, ErrPending
}

// deliverWitness makes one delivery attempt of the checkpoint at size to the
// profile's witness service and returns the resulting persisted state.
func deliverWitness(ctx context.Context, p Profile, store cll.WitnessStateStore, service string, size uint64) (cll.WitnessState, error) {
	token, e := p.Checkpoint.Token.resolve()
	if e != nil {
		return cll.WitnessState{}, e
	}
	client, e := witness.NewClient(p.Checkpoint.Endpoint, &http.Client{Transport: bearerTransport{token: token}, Timeout: 30 * time.Second}, 0)
	if e != nil {
		return cll.WitnessState{}, e
	}
	keys, e := parseKeys([]string{p.Checkpoint.PublicKey})
	if e != nil {
		return cll.WitnessState{}, e
	}
	verifier, e := witness.NewReceiptVerifier(keys[0])
	if e != nil {
		return cll.WitnessState{}, e
	}
	runner, e := witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), selectedWitness{WitnessStateStore: store, id: service, size: size}, map[string]witness.Submitter{service: safeSubmitter{client}}, map[string]witness.Verifier{service: verifier})
	if e != nil {
		return cll.WitnessState{}, e
	}
	if _, e = runner.RunOnce(ctx, time.Now().UTC(), 1); e != nil {
		return cll.WitnessState{}, e
	}
	return store.GetWitness(ctx, service, size)
}
