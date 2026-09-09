package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// Proof uses the library's exact checkpoint COSE bytes and MMR inclusion path.
// Byte slices are JSON base64. Absence of inclusion is reported as partial.
type Proof struct {
	Checkpoint []byte   `json:"checkpoint"`
	CapsuleID  string   `json:"capsule_id,omitempty"`
	LeafIndex  uint64   `json:"leaf_index"`
	Path       [][]byte `json:"path,omitempty"`
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
func serviceID(p Profile) (string, error) {
	endpoint := strings.TrimRight(p.Checkpoint.Endpoint, "/")
	if endpoint == "" {
		return "", nil
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", inputError("checkpoint service requires an HTTPS endpoint without credentials or query")
	}
	if _, e = parseKeys([]string{p.Checkpoint.PublicKey}); e != nil {
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
		status := 400
		if witness.IsRetryable(e) {
			status = 503
		}
		return r, &witness.HTTPError{StatusCode: status, Body: "checkpoint submission failed; response details suppressed"}
	}
	return r, nil
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
			root, e := hex.DecodeString(r.Root)
			if e != nil {
				return e
			}
			if !mmr.VerifyHexInclusion(root, r.MMRSize, proof.LeafIndex, proof.CapsuleID, proof.Path) {
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
	verify.Flags().String("proof", "", "Proof JSON with base64 checkpoint/path")
	logs.AddCommand(verify)
	group := &cobra.Command{Use: "checkpoint"}
	logs.AddCommand(group)
	create := &cobra.Command{Use: "create", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
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
		config := checkpoint.DefaultRunnerConfig(p.LogID)
		config.Cadence.CadenceEntries = 1
		if service != "" {
			config.WitnessIDs = []string{service}
		}
		runner, e := checkpoint.NewRunner(config, t.log, signer)
		if e != nil {
			return e
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
			changed, e := runner.RunOnce(c.Context(), time.Now().UTC())
			if e != nil {
				return e
			}
			if changed {
				continue
			}
			state, e := t.log.LoadCLL(c.Context())
			if e != nil {
				return e
			}
			if state.Checkpoint == nil {
				return inputError("log has no entries")
			}
			record, e := verifyCheckpoint(p, state.Checkpoint.Bytes)
			if e != nil {
				return e
			}
			if record.MMRSize != state.Checkpoint.Size {
				return ErrConflict
			}
			return output(c, map[string]any{"checkpoint": state.Checkpoint.Size, "indexed_sequence": state.Checkpoint.IndexedSeq, "statement": state.Checkpoint.Bytes, "log_id": p.LogID})
		}
		return ErrPending
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
			state, e := t.log.GetWitness(c.Context(), service, size)
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
				token, e := p.Checkpoint.Token.resolve()
				if e != nil {
					return e
				}
				client, e := witness.NewClient(p.Checkpoint.Endpoint, &http.Client{Transport: bearerTransport{token: token}, Timeout: 30 * time.Second}, 0)
				if e != nil {
					return e
				}
				keys, e := parseKeys([]string{p.Checkpoint.PublicKey})
				if e != nil {
					return e
				}
				verifier, e := witness.NewReceiptVerifier(keys[0])
				if e != nil {
					return e
				}
				runner, e := witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), selectedWitness{WitnessStateStore: t.log, id: service, size: size}, map[string]witness.Submitter{service: safeSubmitter{client}}, map[string]witness.Verifier{service: verifier})
				if e != nil {
					return e
				}
				if _, e = runner.RunOnce(c.Context(), time.Now().UTC(), 1); e != nil {
					return e
				}
				state, e = t.log.GetWitness(c.Context(), service, size)
				if e != nil {
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
