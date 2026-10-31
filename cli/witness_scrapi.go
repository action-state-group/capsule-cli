package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	aacwitness "github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
)

// A checkpoint.endpoint of scrapi+https://host names a SCITT Transparency
// Service (SCRAPI POST /entries) as the profile's witness. Its Receipt's leaf
// is SHA-256 of the checkpoint statement, the anchor digest interop profile
// v1 §6.2.2 checks (checkpointed-local-log#117). Registration sends the
// statement to that service, so this CLI registers only with the witness URL
// the holder configured and ships no SCRAPI witness URL of its own.

// scrapiPrefix is the binding prefix a SCRAPI witness URL carries.
const scrapiPrefix = aacwitness.BindingSCRAPI + "+"

// witnessIsSCRAPI reports whether endpoint names the scrapi binding.
func witnessIsSCRAPI(endpoint string) bool {
	return aacwitness.BindingOf(endpoint) == aacwitness.BindingSCRAPI
}

// witnessHTTPS is the HTTPS URL a witness endpoint dials: the endpoint
// without its scrapi+ prefix.
func witnessHTTPS(endpoint string) string {
	if witnessIsSCRAPI(endpoint) {
		return endpoint[len(scrapiPrefix):]
	}
	return endpoint
}

// witnessBinding is the profile's witness client and the verifier its
// receipts must pass before they are stored.
func witnessBinding(p Profile, token string) (witness.Submitter, witness.Verifier, error) {
	verifier, e := witnessVerifier(p)
	if e != nil {
		return nil, nil, e
	}
	httpClient := &http.Client{Transport: bearerTransport{token: token}, Timeout: 30 * time.Second}
	if witnessIsSCRAPI(p.Checkpoint.Endpoint) {
		client, e := witness.NewSCRAPIClient(witnessHTTPS(p.Checkpoint.Endpoint), httpClient, 0)
		if e != nil {
			return nil, nil, e
		}
		return scrapiSubmitter{client}, verifier, nil
	}
	client, e := witness.NewClient(p.Checkpoint.Endpoint, httpClient, 0)
	if e != nil {
		return nil, nil, e
	}
	return safeSubmitter{client}, verifier, nil
}

// witnessVerifier is the receipt check for the profile's witness binding
// under its pinned key.
func witnessVerifier(p Profile) (witness.Verifier, error) {
	keys, e := ParseKeys([]string{p.Checkpoint.PublicKey})
	if e != nil {
		return nil, e
	}
	if witnessIsSCRAPI(p.Checkpoint.Endpoint) {
		return newAnchorVerifier(p.Checkpoint.Endpoint, keys[0])
	}
	return witness.NewReceiptVerifier(keys[0])
}

// scrapiSubmitter is safeSubmitter for a SCRAPI client: the same failure
// classes, recorded without the witness's response body. A registration
// still pending after its poll budget is retried like an outage.
type scrapiSubmitter struct{ client *witness.SCRAPIClient }

func (s scrapiSubmitter) Submit(ctx context.Context, b []byte) (witness.Receipt, error) {
	r, e := s.client.Submit(ctx, b)
	return r, classifySCRAPI(e)
}

func classifySCRAPI(e error) error {
	if errors.Is(e, witness.ErrSCRAPIPending) {
		return &witness.HTTPError{StatusCode: http.StatusServiceUnavailable, Body: WitnessFailureBody(http.StatusAccepted)}
	}
	return classifySubmitError(e)
}

// anchorVerifier checks a SCRAPI Receipt with agent-action-capsule's
// anchor_witnessed check (interop profile v1 §6.2.2) under the profile's
// pinned witness key.
type anchorVerifier struct {
	policy aacwitness.AnchorWitnessPolicy
}

func newAnchorVerifier(endpoint string, key ed25519.PublicKey) (anchorVerifier, error) {
	spki, e := x509.MarshalPKIXPublicKey(key)
	if e != nil {
		return anchorVerifier{}, e
	}
	return anchorVerifier{policy: aacwitness.AnchorWitnessPolicy{Witnesses: []aacwitness.AnchorWitness{{Name: endpoint, Keys: []string{hex.EncodeToString(spki)}}}}}, nil
}

func (v anchorVerifier) Verify(statement []byte, r witness.Receipt) error {
	record, e := checkpoint.ParseRecord(statement)
	if e != nil {
		return e
	}
	if e = record.VerifySignature(); e != nil {
		return e
	}
	anchor := sha256.Sum256(statement)
	if r.EntryHashScheme != "" || r.EntryHash != hex.EncodeToString(anchor[:]) {
		return errors.New("SCRAPI receipt entry does not name this checkpoint's anchor")
	}
	report := aacwitness.CheckAnchorReceipt(hex.EncodeToString(anchor[:]), r.Bytes, v.policy)
	if report.Status != aacwitness.AnchorPass {
		return fmt.Errorf("SCRAPI receipt does not witness this checkpoint's anchor: %s", strings.ToLower(report.Status))
	}
	return nil
}
