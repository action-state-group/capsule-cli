package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/mmr"
	"github.com/action-state-group/checkpointed-local-log/go/witness"
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

func VerifyCheckpoint(p Profile, raw []byte) (checkpoint.Record, error) {
	r, e := checkpoint.ParseRecord(raw)
	if e != nil {
		return r, e
	}
	if r.LogID != p.LogID && !backendOwnsLogID(p, r.LogID) {
		if p.HasPluginLog() {
			return r, fmt.Errorf("%w: checkpoint log id %q is neither the profile's log_id %q nor a log under it (\"%s/<suffix>\") that the profile's backend plugin declares", ErrConflict, r.LogID, p.LogID, p.LogID)
		}
		return r, ErrConflict
	}
	if e = r.VerifySignature(); e != nil {
		return r, e
	}
	if !CheckpointSignerTrusted(p, r.KeyID) {
		return r, errors.New("checkpoint signer not trusted")
	}
	return r, nil
}
func CheckpointSignerTrusted(p Profile, keyID string) bool {
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
	if _, e := ParseKeys([]string{p.Checkpoint.PublicKey}); e != nil {
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
	u, e := url.Parse(witnessHTTPS(endpoint))
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Hint(ErrInput, "checkpoint.endpoint must be an HTTPS URL, or scrapi+https:// for a SCITT SCRAPI witness, without credentials, query or fragment")
	}
	if issue := witnessKeyIssue(p); issue != "" {
		return Hint(ErrInput, issue)
	}
	return nil
}

func ServiceID(p Profile) (string, error) {
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
	ctx, connected := traceConnection(ctx)
	r, e := s.client.Submit(ctx, b)
	return r, classifySubmitError(e, connected.Load())
}

// traceConnection notes on ctx whether a request made with it got a
// connection to the witness: after any proxy tunnel and the verified TLS
// handshake, or a kept-alive connection reused. A timeout before that point
// (DNS, dial, a proxy holding CONNECT, a handshake that never finishes) is
// what a host waiting on network consent looks like from here; a timeout
// after it is a witness, or the network to it, slow to answer. The flag
// stays set once any request of the submission connected (a SCRAPI POST
// before its polls, say): the host let the site through, so consent is not
// what is missing. The witness client builds its requests from ctx, so the
// trace reaches them.
func traceConnection(ctx context.Context) (context.Context, *atomic.Bool) {
	connected := new(atomic.Bool)
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	return httptrace.WithClientTrace(ctx, trace), connected
}

// timedOut reports a request ended by its deadline: the HTTP client's
// timeout or a context deadline.
func timedOut(e error) bool {
	var network net.Error
	return errors.Is(e, context.DeadlineExceeded) || errors.As(e, &network) && network.Timeout()
}

// classifySubmitError is how a delivery records a witness client's error;
// connected says whether the request got a connection to the witness
// (traceConnection).
func classifySubmitError(e error, connected bool) error {
	if e != nil {
		var answered *witness.HTTPError
		// A request that reached the witness and then ran out of time is a
		// slow witness, not a network gate: it is recorded as timed out.
		if !errors.As(e, &answered) && connected && timedOut(e) {
			return &witness.HTTPError{StatusCode: 503, Body: WitnessTimedOut}
		}
		// A request that got no answer and no connection to the witness
		// (refused, unresolved, timed out before connecting, or stopped by
		// a proxy asking for authorization) is recorded as not reached, so
		// it can be shown as such: on an agent host that asks before
		// network access, that is what a missing consent looks like from
		// here.
		if answered == nil || answered.StatusCode == http.StatusProxyAuthRequired {
			return &witness.HTTPError{StatusCode: 503, Body: WitnessNotReached}
		}
		// A 403 may come from a network gate on this host rather than the
		// witness (capsule-cli#335), and such a gate opens once the host
		// grants the site, so it is retried like an outage.
		status := 400
		if witness.IsRetryable(e) || answered.StatusCode == http.StatusForbidden {
			status = 503
		}
		// The answered status (never the body) is kept, so a pending
		// delivery can say what came back. A continuity refusal also keeps
		// the witness's own last-accepted size and root, parsed strictly,
		// which a re-proof from the witness's tip needs (capsule-cli#647).
		body := WitnessFailureBody(answered.StatusCode)
		if answered.StatusCode == http.StatusConflict {
			if size, root, ok := lastAccepted(answered.Body); ok {
				body += fmt.Sprintf("; the witness last accepted mmr_size %d root %s", size, root)
			}
		}
		return &witness.HTTPError{StatusCode: status, Body: body}
	}
	return nil
}

// lastAccepted reads the witness's last-accepted checkpoint from a
// continuity refusal's body (capsule-anchor router.py, the
// ContinuityMismatchError detail): an unsigned size and a 32-byte root as
// hex. Nothing else in the body is read, and anything not of that shape is
// not kept.
func lastAccepted(body string) (uint64, string, bool) {
	var wire struct {
		Detail struct {
			Size *uint64 `json:"last_accepted_mmr_size"`
			Root string  `json:"last_accepted_root"`
		} `json:"detail"`
	}
	if json.Unmarshal([]byte(body), &wire) != nil || wire.Detail.Size == nil || !hexRoot.MatchString(wire.Detail.Root) {
		return 0, "", false
	}
	return *wire.Detail.Size, strings.ToLower(wire.Detail.Root), true
}

// hexRoot is an MMR root as the witness writes it: 32 bytes as hex.
var hexRoot = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// witnessFailureBody is what a delivery answered with an error records: its
// HTTP status, never its body. Rows stored before capsule-cli#335 read "the
// witness answered HTTP N" and are still read the same way.
func WitnessFailureBody(status int) string {
	return fmt.Sprintf("checkpoint submission failed (answered HTTP %d); response details suppressed", status)
}

// witnessAnswered reads the answered HTTP status from a delivery's recorded
// error (safeSubmitter keeps it, never the response body).
var witnessAnswered = regexp.MustCompile(`answered HTTP (\d{3})`)

// answeredStatus is the HTTP status a delivery's recorded error says came
// back, or "" when it records none.
func answeredStatus(state cll.WitnessState) string {
	if m := witnessAnswered.FindStringSubmatch(state.LastError); m != nil {
		return m[1]
	}
	return ""
}

// witnessRearmed marks a delivery re-armed after a continuity refusal
// (HTTP 409). selectedWitness.CommitWitness keeps it through every later
// failure of the row, so the row is re-armed once, never again.
const witnessRearmed = "; re-armed once after a continuity refusal"

// rearmable reports a delivery stored as failed for good that RearmRefused
// makes due again: a 403 a build before capsule-cli#335 stored, or a
// continuity refusal (409) not yet re-armed. A witness may refuse a valid
// checkpoint once, before its own continuity tip has moved on
// (capsule-cli#647), so a 409 is never final on its first answer.
func rearmable(state cll.WitnessState) bool {
	if !state.Permanent || state.Receipt != nil {
		return false
	}
	switch answeredStatus(state) {
	case "403":
		return true
	case "409":
		return !strings.Contains(state.LastError, witnessRearmed)
	}
	return false
}

// withRearmed is a delivery error carrying the witnessRearmed mark, within
// the stored error's bound.
func withRearmed(lastError string) string {
	if strings.Contains(lastError, witnessRearmed) {
		return lastError
	}
	if len(lastError)+len(witnessRearmed) > cll.MaxReasonBytes {
		lastError = WitnessFailureBody(http.StatusConflict)
	}
	return lastError + witnessRearmed
}

// continuityRetryGap is the least time between a continuity refusal and the
// one retry RearmRefused gives it. The refusal a witness gives before its
// own tip has moved on clears within moments; a delivery call made in the
// same tick (a plugin's cadence delivers each pending row in turn, each
// call re-arming first) must not spend the retry on a witness that has not
// caught up yet.
const continuityRetryGap = time.Minute

// RearmRefused makes a rearmable delivery due again. A 403 is re-armed once:
// it is now stored as retryable, so no build writes another. A 409 is
// re-armed once and marked, so a second refusal stays; it is due no sooner
// than continuityRetryGap after the next-attempt time the refusal stored,
// which is at or after the refusal (selectedWitness.CommitWitness), and
// later rows wait behind it meanwhile.
func RearmRefused(ctx context.Context, store cll.WitnessStateStore, state cll.WitnessState, now time.Time) (cll.WitnessState, error) {
	if !rearmable(state) {
		return state, nil
	}
	next := state
	if answeredStatus(state) == "409" {
		next.LastError = withRearmed(state.LastError)
	}
	next.Permanent = false
	// Whole seconds: a store that rounds the time it keeps must not move
	// it past now, or a delivery run at now would skip the row.
	next.NextAttemptAt = now.Truncate(time.Second)
	if due := state.NextAttemptAt.Add(continuityRetryGap); answeredStatus(state) == "409" && due.After(now) {
		next.NextAttemptAt = due.Truncate(time.Second)
	}
	if e := store.CommitWitness(ctx, state.Attempts, next); e != nil {
		return state, e
	}
	return next, nil
}

// RearmRefusedLog re-arms every refused delivery to service on log
// (RearmRefused).
func RearmRefusedLog(ctx context.Context, log cll.Backend, service string, now time.Time) error {
	state, e := log.LoadCLL(ctx)
	if e != nil {
		return e
	}
	for _, w := range state.Witnesses {
		if w.WitnessID != service {
			continue
		}
		if _, e = RearmRefused(ctx, log, w, now); e != nil {
			return e
		}
	}
	return nil
}

// witnessNotReached marks a delivery whose request got neither an answer
// nor a connection to the witness.
const WitnessNotReached = "witness not reached; response details suppressed"

// WitnessTimedOut marks a delivery whose request got a connection to the
// witness but no complete answer before its deadline. It must not contain
// WitnessNotReached, which WitnessPendingReason matches as a substring.
const WitnessTimedOut = "witness reached but did not answer in time; response details suppressed"

// WitnessTimedOutText is the pending text of a delivery that timed out after
// reaching the witness. It names no network consent: the connection was
// made. "Reached" means a connection to the witness's host with its TLS
// certificate verified (traceConnection's GotConn), never a bare TCP
// connect. Which side was slow, the witness or the network after the
// connection, cannot be told from here. A host proxy that terminated TLS
// with its own trusted certificate and then held the request for consent
// would also read as reached: this split cannot tell that case apart.
func WitnessTimedOutText() string {
	return "pending: the witness was reached but its answer did not come in time, so the witness or the network to it is slow; " + WitnessRetryText()
}

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

// witnessGrantText states the network grant a witness needs. It is a
// statement, not an instruction: an agent reads this output, and an agent
// host may treat instructions found in tool output as prompt injection and
// refuse to go on. The agent's own skill tells the person what to choose.
func WitnessGrantText(endpoint string) string {
	host, site := witnessSite(endpoint)
	return fmt.Sprintf("Ticks run on a schedule, with nobody present to answer a network prompt, so they need a standing grant (\"Always allow this site\") for %s, not a one-time one (\"allow once\"). The witness is %s; a grant for %s covers it and all its subdomains.", site, host, site)
}

// witnessConsentText says what a delivery that did not reach the witness
// most likely needs, without claiming to know: this program cannot see an
// agent host's network-consent prompt.
func WitnessConsentText(endpoint string) string {
	return "pending: network consent needed, or no network. The request did not reach the witness. " + WitnessGrantText(endpoint)
}

// witnessConsentReached is doctor's note when its own probe got through.
func WitnessConsentReached(endpoint string) string {
	return "Reached from this command. " + WitnessGrantText(endpoint)
}

// WitnessNotDelivered starts the text of a delivery failed for good: it is
// not pending, since it is never retried.
const WitnessNotDelivered = "not delivered: "

// WitnessRetryText is the retry sentence of the pending texts. A tick only
// attempts a delivery whose next-attempt time has passed, and the library
// backs that time off up to its MaxBackoff, so a failing witness is not tried
// at every tick. The cap is read from the delivery configuration the CLI runs
// with, so the text cannot drift from it.
func WitnessRetryText() string {
	max := witness.DefaultDeliveryConfig().MaxBackoff
	span := max.String()
	if max == time.Hour {
		span = "an hour"
	}
	return "it is tried again on a later capsulectl run, after a wait that grows up to " + span + "."
}

// witnessPendingReason classifies a pending delivery from its stored error.
func WitnessPendingReason(state cll.WitnessState, endpoint string) (string, string) {
	if strings.Contains(state.LastError, WitnessNotReached) {
		return "network_consent_needed", WitnessConsentText(endpoint)
	}
	if strings.Contains(state.LastError, WitnessTimedOut) {
		return "witness_timeout", WitnessTimedOutText()
	}
	if state.Attempts == 0 {
		return "not_attempted", "pending: not sent yet; it goes at the next tick."
	}
	status := answeredStatus(state)
	if status == "403" {
		// A refused delivery stored as failed for good is re-armed at the
		// next tick (RearmRefused), so it is retried either way.
		return "refused", "pending: the request was refused (HTTP 403), by the witness or by a network gate on this host; " + WitnessRetryText() + " " + WitnessGrantText(endpoint)
	}
	if status == "409" && (rearmable(state) || !state.Permanent && strings.Contains(state.LastError, witnessRearmed)) {
		return "witness_error", "pending: the witness refused it as not continuing the checkpoint it last accepted (HTTP 409); it is sent once more at a later capsulectl run, a minute or more after the refusal."
	}
	if status == "409" && reanchorDue(state) {
		return "witness_error", "pending: the witness refused it again after a retry, as not continuing the checkpoint it last accepted (HTTP 409); capsulectl proves it again from that checkpoint at a later capsulectl run, and later checkpoints wait for it."
	}
	if status == "409" && reanchored(state) {
		size, _, _ := witnessTipOf(state)
		return "witness_error", WitnessNotDelivered + fmt.Sprintf("the witness refused it again after a retry, as not continuing the checkpoint it last accepted (HTTP 409); capsulectl re-anchored it, proving it again from that checkpoint (mmr_size %d), so later checkpoints are delivered; the receipt for the re-anchor is not kept.", size)
	}
	if status == "409" && state.Permanent {
		text := WitnessNotDelivered + "the witness refused it again after a retry, as not continuing the checkpoint it last accepted (HTTP 409); "
		if i := strings.Index(state.LastError, witnessNotReanchored); i >= 0 {
			text += "it was not re-anchored: " + strings.TrimSuffix(state.LastError[i+len(witnessNotReanchored):], ".") + "; "
		}
		return "witness_error", text + "this delivery is not retried."
	}
	if state.Permanent {
		if status != "" {
			return "witness_error", WitnessNotDelivered + "the witness answered with an error (HTTP " + status + "); this delivery is not retried."
		}
		return "witness_error", WitnessNotDelivered + "the delivery failed; it is not retried."
	}
	if status != "" {
		return "witness_error", "pending: the witness answered with an error (HTTP " + status + "); " + WitnessRetryText()
	}
	return "witness_error", "pending: the witness answered with an error; " + WitnessRetryText()
}

// A scoped view prevents one command from delivering unrelated checkpoint rows.
// It still shows the selected row's older, due, never-receipted rows of the
// same witness: the witness refuses a checkpoint whose predecessor it has not
// registered as a log mutation, never to be retried, and the runner holds a
// row until its predecessor has a receipt. Rows newer than the selection are
// left alone; a permanently failed older row is skipped (GetWitness).
type selectedWitness struct {
	cll.WitnessStateStore
	id   string
	size uint64
	// reanchors is set when this delivery re-anchors a refused row
	// (reanchorRefused), so a row due for it holds later rows.
	reanchors bool
}

// PendingWitnessesFor is the method the delivery runner calls; the embedded
// store's unscoped one would hand the runner every due row of the witness.
func (s selectedWitness) PendingWitnessesFor(ctx context.Context, id string, now time.Time, limit int) ([]cll.WitnessState, error) {
	if limit < 1 || limit > cll.MaxWitnesses {
		return nil, fmt.Errorf("%w: invalid pending witness query", cll.ErrInvalid)
	}
	if id != s.id {
		return nil, nil
	}
	rows, e := s.WitnessStateStore.PendingWitnessesFor(ctx, id, now, cll.MaxWitnesses)
	if e != nil {
		return nil, e
	}
	out := make([]cll.WitnessState, 0, len(rows))
	for _, r := range rows {
		if r.CheckpointSize <= s.size {
			out = append(out, r)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CommitWitness is how the runner records a delivery. A row re-armed after a
// continuity refusal keeps its witnessRearmed mark through every later
// failure, a timeout or an unreached witness included, so it is re-armed
// once however its retries end. A first continuity refusal is stored with a
// next-attempt time no earlier than the moment it is recorded: the runner
// counts its backoff from the start of its pass, which may be long before
// the answer, and RearmRefused counts continuityRetryGap from that time.
func (s selectedWitness) CommitWitness(ctx context.Context, expectedAttempts uint32, next cll.WitnessState) error {
	if rearmable(next) && answeredStatus(next) == "409" {
		if at := ceilSecond(witnessClock()); at.After(next.NextAttemptAt) {
			next.NextAttemptAt = at
		}
	}
	if next.Receipt == nil {
		current, e := s.WitnessStateStore.GetWitness(ctx, next.WitnessID, next.CheckpointSize)
		if e != nil && !errors.Is(e, cll.ErrNotFound) {
			return e
		}
		if e == nil && strings.Contains(current.LastError, witnessRearmed) {
			next.LastError = withRearmed(next.LastError)
		}
		if e == nil && strings.Contains(current.LastError, witnessResent) {
			next.LastError = withMark(next.LastError, witnessResent)
		}
	}
	return s.WitnessStateStore.CommitWitness(ctx, expectedAttempts, next)
}

// witnessClock is the time a delivery's outcome is recorded; a variable so
// tests can move it.
var witnessClock = time.Now

// ceilSecond is t rounded up to a whole second, so a store that keeps whole
// seconds never moves it earlier.
func ceilSecond(t time.Time) time.Time {
	if r := t.Truncate(time.Second); !r.Equal(t) {
		return r.Add(time.Second)
	}
	return t
}

// GetWitness is what the runner's predecessor gate reads. A predecessor
// stored as failed for good but due again at the next call (rearmable) holds
// the selection, so one continuity refusal is not followed by refusals of
// every later row in the same pass. A predecessor that failed for good is
// never delivered again, so holding the selection for it
// would leave the selection pending for ever; it is shown as absent and the
// witness decides, as the spec lets it: a witness holding state for the log
// refuses a checkpoint whose predecessor it did not register (a permanent
// failure of the selection, reported as such), and one holding none MAY
// register it as the start of the log's history there
// (draft-mih-scitt-checkpointed-local-log-02, lines 388-393 at checkpointed-local-log 89a80fd).
func (s selectedWitness) GetWitness(ctx context.Context, id string, size uint64) (cll.WitnessState, error) {
	r, e := s.WitnessStateStore.GetWitness(ctx, id, size)
	if e == nil && r.Permanent && r.Receipt == nil && !rearmable(r) && !(s.reanchors && reanchorDue(r)) && !(id == s.id && size == s.size) {
		return cll.WitnessState{}, cll.ErrNotFound
	}
	return r, e
}

// witnessResult is a delivery's state. A verified one also carries its
// receipt as a checkpoint.witnesses entry, whose ts_url is the profile's
// witness endpoint and so names its binding.
func witnessResult(endpoint string, state cll.WitnessState) map[string]any {
	status := "pending"
	switch {
	case reanchored(state):
		status = "re-anchored"
	case state.Permanent && !rearmable(state) && !reanchorDue(state):
		status = "failed"
	}
	if state.Receipt != nil {
		status = "verified"
	}
	out := map[string]any{"state": status, "attempts": state.Attempts, "next_attempt_at": state.NextAttemptAt, "receipt": state.Receipt}
	if r := state.Receipt; r != nil && r.LeafIndex != nil && r.TreeSize != nil {
		out["witness"] = map[string]any{
			"ts_url": strings.TrimRight(endpoint, "/"), "entry_hash": r.EntryHash,
			"receipt_b64": base64.StdEncoding.EncodeToString(r.Bytes),
			"leaf_index":  *r.LeafIndex, "tree_size": *r.TreeSize,
		}
	}
	return out
}

// Status re-verifies persisted evidence rather than trusting a receipt-shaped
// database row or an earlier successful network call.
func VerifyWitness(p Profile, state cll.WitnessState) error {
	if state.Receipt == nil {
		return nil
	}
	r := state.Receipt
	if r.LeafIndex == nil || r.TreeSize == nil {
		return errors.New("incomplete stored receipt")
	}
	v, err := witnessVerifier(p)
	if err != nil {
		return err
	}
	return v.Verify(state.Checkpoint, witness.Receipt{Bytes: r.Bytes, EntryHash: r.EntryHash, EntryHashScheme: r.EntryHashScheme, LeafIndex: *r.LeafIndex, TreeSize: *r.TreeSize})
}

func addCheckpointCommands(logs *cobra.Command) {
	verify := &cobra.Command{Use: "verify", Short: "Verify a signed checkpoint and optional Capsule inclusion proof offline", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		path, _ := c.Flags().GetString("proof")
		raw, e := ReadInput(path)
		if e != nil {
			return e
		}
		var proof Proof
		if e = DecodeJSONAs("--proof "+path, raw, &proof); e != nil {
			return e
		}
		r, e := VerifyCheckpoint(p, proof.Checkpoint)
		if e != nil {
			return e
		}
		checks := map[string]string{"checkpoint_signature_and_trust": "passed", "log_id": "passed", "embedded_consistency": "passed", "inclusion": "not_performed", "witness_receipt": "not_performed", "producer_signature": "not_performed", "checkpoint_log_id": r.LogID}
		if r.LogID != p.LogID {
			checks["log_id"] = "passed (declared by the backend)"
		}
		if proof.CapsuleID != "" {
			if proof.InclusionProof == nil {
				return InputError("proof is missing inclusion_proof; a proof minted before the structured-proof format (leaf_index + path) is no longer accepted — re-mint it")
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
		if e = Output(c, checks); e != nil {
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
	// A plugin's checkpoint cadence, under the CLI's checkpoint verbs: the
	// command runs in the plugin that keeps the selected profile's log, which
	// grafts its own over this one (GraftCommand) in its process.
	group.AddCommand(&cobra.Command{Use: "cadence", Short: "A plugin's checkpoint cadence on a profile whose log it keeps: runs in that plugin", DisableFlagParsing: true, RunE: func(c *cobra.Command, args []string) error {
		return forwardCadence(c, args)
	}})
	create := &cobra.Command{Use: "create", Short: "Cut a signed checkpoint at the current log tip", Args: NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, e := Selected(c)
		if e != nil {
			return e
		}
		if done, e := delegateLog(c, p, nil); done {
			return e
		}
		key, e := PrivateKey(p.Checkpoint.Signing)
		if e != nil {
			return e
		}
		signer, e := checkpoint.NewEd25519Signer(key)
		if e != nil {
			return e
		}
		if !CheckpointSignerTrusted(p, signer.KeyID()) {
			return InputError("the checkpoint signing key is not in checkpoint.trusted_keys: add its public key with `capsulectl profile update --profile " + p.Name + " --checkpoint-trusted-key <64-hex key>`")
		}
		service, e := ServiceID(p)
		if e != nil {
			return e
		}
		t, e := OpenTarget(c.Context(), p, UseCLL)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, t.Close()) }()
		if t.Owned != nil {
			result, e := t.Owned.Checkpoint(c.Context(), service)
			if e != nil {
				return e
			}
			return Output(c, result)
		}
		cp, e := CutCheckpoint(c.Context(), p, t.Log)
		if e != nil {
			return e
		}
		return Output(c, map[string]any{"checkpoint": cp.Size, "indexed_sequence": cp.IndexedSeq, "statement": cp.Bytes, "log_id": p.LogID})
	}}
	group.AddCommand(create)
	for _, publish := range []bool{false, true} {
		verb := "status"
		if publish {
			verb = "publish"
		}
		cmd := &cobra.Command{Use: verb, Args: NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
			p, e := Selected(c)
			if e != nil {
				return e
			}
			if done, e := delegateLog(c, p, nil); done {
				return e
			}
			size, _ := c.Flags().GetUint64("checkpoint")
			if size == 0 {
				return InputError("--checkpoint MMR size is required")
			}
			service, e := ServiceID(p)
			if e != nil {
				return e
			}
			if service == "" {
				return InputError("checkpoint.endpoint is not set, so there is no witness to publish to: `capsulectl profile update --profile " + p.Name + " --checkpoint-endpoint URL --checkpoint-public-key <64-hex Ed25519 key>`")
			}
			use := UseCLLRead
			if publish {
				use = UseCLL
			}
			t, e := OpenTarget(c.Context(), p, use)
			if e != nil {
				return e
			}
			defer func() { err = errors.Join(err, t.Close()) }()
			var store cll.WitnessStateStore = t.Log
			if p.HasPluginLog() {
				store = backendFor(p).WitnessStore(p)
			}
			state, e := store.GetWitness(c.Context(), service, size)
			if e != nil {
				return e
			}
			record, e := VerifyCheckpoint(p, state.Checkpoint)
			if e != nil {
				return e
			}
			if record.MMRSize != size {
				return ErrConflict
			}
			if publish {
				if state, e = DeliverWitness(c.Context(), p, store, service, size); e != nil {
					return e
				}
			}
			if e = VerifyWitness(p, state); e != nil {
				return e
			}
			result := witnessResult(p.Checkpoint.Endpoint, state)
			result["log_id"] = record.LogID
			if e = Output(c, result); e != nil {
				return e
			}
			if publish {
				return deliveryOutcome(state)
			}
			return nil
		}}
		cmd.Flags().Uint64("checkpoint", 0, "CLL checkpoint MMR size (not entry count)")
		group.AddCommand(cmd)
	}
}

// deliveryOutcome is how cll checkpoint publish ends for a delivery's state:
// done with a receipt or re-anchored (the witness has moved on to it),
// pending while it is retried (a continuity refusal due for its re-arm or
// its re-anchor included), failed when it never will be.
func deliveryOutcome(state cll.WitnessState) error {
	switch {
	case state.Receipt != nil, reanchored(state):
		return nil
	case state.Permanent && !rearmable(state) && !reanchorDue(state):
		return errors.New("checkpoint delivery permanently failed")
	}
	return ErrPending
}

// cutCheckpoint signs a checkpoint at the current log tip and returns it after
// re-verifying the stored statement. The checkpoint signer must be explicitly
// trusted by the profile.
func CutCheckpoint(ctx context.Context, p Profile, log cll.Backend) (*cll.CheckpointState, error) {
	return CutCheckpointAt(ctx, p, log, time.Now().UTC())
}

// cutCheckpointAt is cutCheckpoint with the time the checkpoint states.
func CutCheckpointAt(ctx context.Context, p Profile, log cll.Backend, at time.Time) (*cll.CheckpointState, error) {
	key, e := PrivateKey(p.Checkpoint.Signing)
	if e != nil {
		return nil, e
	}
	signer, e := checkpoint.NewEd25519Signer(key)
	if e != nil {
		return nil, e
	}
	if !CheckpointSignerTrusted(p, signer.KeyID()) {
		return nil, InputError("the checkpoint signing key is not in checkpoint.trusted_keys: add its public key with `capsulectl profile update --profile " + p.Name + " --checkpoint-trusted-key <64-hex key>`")
	}
	service, e := ServiceID(p)
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
			return nil, InputError("the log has no checkpoint yet, so there is nothing to publish: cut one with `capsulectl cll checkpoint create`")
		}
		record, e := VerifyCheckpoint(p, state.Checkpoint.Bytes)
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

// DeliverWitness makes one delivery attempt of the checkpoint at size to the
// profile's witness service and returns the resulting persisted state. Each
// witness request has the client's own 30 s bound; DeliverWitnessBy lets a
// caller end the delivery sooner.
func DeliverWitness(ctx context.Context, p Profile, store cll.WitnessStateStore, service string, size uint64) (cll.WitnessState, error) {
	return DeliverWitnessBy(ctx, p, store, service, size, time.Time{})
}

// DeliverWitnessBy is DeliverWitness with a deadline for the witness
// requests of this call; the zero time is no deadline beyond each request's
// 30 s. A request still running at the deadline is ended and recorded like
// any timed-out delivery: witness_timeout when it had reached the witness
// (capsule-cli#644). A row whose request would start with less than
// witnessBudgetFloor left is not sent and no attempt is recorded for it, so a
// nearly spent budget is never recorded as a failed attempt or as a network
// consent wait; it goes at a later call. Refused rows are still re-armed
// first (RearmRefused), as every call does. ctx stays the context of the
// store reads and writes, so cancelling ctx ends the call without recording
// the attempt (the CLL runner returns ctx.Err() before it commits), while
// the deadline ends only the witness requests and their attempts are kept.
func DeliverWitnessBy(ctx context.Context, p Profile, store cll.WitnessStateStore, service string, size uint64, deadline time.Time) (cll.WitnessState, error) {
	token, e := p.Checkpoint.Token.resolve()
	if e != nil {
		return cll.WitnessState{}, e
	}
	client, verifier, e := witnessBinding(p, token)
	if e != nil {
		return cll.WitnessState{}, e
	}
	return deliverSelected(ctx, store, service, size, client, verifier, newReprover(p, store), time.Now().UTC(), deadline)
}

// witnessBudgetFloor is the least time left before a DeliverWitnessBy
// deadline at which a witness request is still started: enough for DNS, TCP
// and TLS to a witness on a working network, so a request ended before it
// connects still says what #644 reads it as. A variable so tests can shorten
// it.
var witnessBudgetFloor = 2 * time.Second

// errBudgetSpent is the cause deliverSelected's runner context is cancelled
// with when a row would start after the budget's floor (budgetSubmitter).
var errBudgetSpent = errors.New("witness delivery budget spent")

// budgetSubmitter bounds a submitter's requests by one deadline shared by
// every row of a deliverSelected call. A request it does not start cancels
// the runner's context first, so the runner returns before it records
// anything (it checks its context right after Submit) and no store write is
// cut short: the cancel happens while the runner waits on Submit.
type budgetSubmitter struct {
	inner    witness.Submitter
	deadline time.Time
	stop     context.CancelCauseFunc
}

func (b budgetSubmitter) Submit(ctx context.Context, statement []byte) (witness.Receipt, error) {
	if time.Until(b.deadline) < witnessBudgetFloor {
		b.stop(errBudgetSpent)
		return witness.Receipt{}, errBudgetSpent
	}
	ctx, cancel := context.WithDeadline(ctx, b.deadline)
	defer cancel()
	return b.inner.Submit(ctx, statement)
}

// deliverSelected delivers the checkpoint at size and the older rows the
// witness needs first (selectedWitness). Older refused rows are re-armed so
// they are not skipped out of order, and the runner is repeated while it
// makes progress, since one pass handles at most cll.MaxWitnesses rows. A
// row refused again after its retry is then re-anchored (reanchorRefused),
// and after a re-anchor the rows it held are delivered in the same call. A
// non-zero deadline bounds the witness requests (DeliverWitnessBy).
func deliverSelected(ctx context.Context, store cll.WitnessStateStore, service string, size uint64, submitter witness.Submitter, verifier witness.Verifier, prove reprover, now time.Time, deadline time.Time) (cll.WitnessState, error) {
	// A store that cannot list its rows (a plugin's) re-arms the selection
	// and the predecessors it chains on (witnessChain).
	rows, e := witnessRows(ctx, store, service, size)
	if e != nil {
		return cll.WitnessState{}, e
	}
	for _, w := range rows {
		if w.WitnessID == service && w.CheckpointSize <= size {
			if _, e := RearmRefused(ctx, store, w, now); e != nil {
				return cll.WitnessState{}, e
			}
		}
	}
	runCtx := ctx
	if !deadline.IsZero() {
		var stop context.CancelCauseFunc
		runCtx, stop = context.WithCancelCause(ctx)
		defer stop(nil)
		submitter = budgetSubmitter{inner: submitter, deadline: deadline, stop: stop}
	}
	view := selectedWitness{WitnessStateStore: store, id: service, size: size, reanchors: prove != nil}
	runner, e := witness.NewDeliveryRunner(witness.DefaultDeliveryConfig(), view, map[string]witness.Submitter{service: submitter}, map[string]witness.Verifier{service: verifier})
	if e != nil {
		return cll.WitnessState{}, e
	}
	// Each re-anchor step changes a row for good, so the steps end; the bound
	// is a safety net only.
	for step := 0; ; step++ {
		selected, e := runSelected(ctx, runCtx, runner, store, view, service, size, now)
		if e != nil || step == maxDeliveryPasses {
			return selected, e
		}
		moved, e := reanchorRefused(ctx, store, service, size, prove, submitter, verifier, now)
		if e != nil {
			return selected, e
		}
		if !moved {
			return store.GetWitness(ctx, service, size)
		}
	}
}

// runSelected runs the delivery runner for deliverSelected and returns the
// selection's state. The runner handles one page per pass and counts only
// receipts, so the loop does not use its count. It runs until the selection
// has been attempted (a receipt, a permanent failure, or one more attempt
// than at the start) or until a whole pass leaves the due rows exactly as
// they were, which is a held or finished pass. At most one pass per row of
// the witness's backlog is possible, so the bound is a safety net only.
func runSelected(ctx, runCtx context.Context, runner *witness.DeliveryRunner, store cll.WitnessStateStore, view selectedWitness, service string, size uint64, now time.Time) (cll.WitnessState, error) {
	start, e := store.GetWitness(ctx, service, size)
	if e != nil {
		return cll.WitnessState{}, e
	}
	for pass := 0; pass < maxDeliveryPasses; pass++ {
		before, e := dueState(ctx, view, service, now)
		if e != nil {
			return cll.WitnessState{}, e
		}
		if _, e = runner.RunOnce(runCtx, now, cll.MaxWitnesses); e != nil {
			if ctx.Err() == nil && errors.Is(context.Cause(runCtx), errBudgetSpent) {
				return store.GetWitness(ctx, service, size)
			}
			return cll.WitnessState{}, e
		}
		selected, e := store.GetWitness(ctx, service, size)
		if e != nil || selected.Receipt != nil || selected.Permanent || selected.Attempts > start.Attempts {
			return selected, e
		}
		after, e := dueState(ctx, view, service, now)
		if e != nil || after == before {
			return selected, e
		}
	}
	return cll.WitnessState{}, fmt.Errorf("witness delivery to %q made no end after %d passes", service, maxDeliveryPasses)
}

// witnessChain is the row at size and the rows it chains on through each
// checkpoint's signed prev size, newest first, down to the first row with a
// receipt or with no row: the rows the runner's predecessor gate reads for
// the selection. It reads them one by one, for a store that cannot list its
// rows.
func witnessChain(ctx context.Context, store cll.WitnessStateStore, service string, size uint64) ([]cll.WitnessState, error) {
	var rows []cll.WitnessState
	for size > 0 {
		w, e := store.GetWitness(ctx, service, size)
		if errors.Is(e, cll.ErrNotFound) {
			break
		}
		if e != nil {
			return nil, e
		}
		rows = append(rows, w)
		record, e := checkpoint.ParseRecord(w.Checkpoint)
		// Each step goes to a smaller size, so the walk ends.
		if w.Receipt != nil || e != nil || record.PrevSize >= size {
			break
		}
		size = record.PrevSize
	}
	return rows, nil
}

// maxDeliveryPasses bounds deliverSelected's loop.
const maxDeliveryPasses = 4096

// dueState is the state of the rows the runner would be given: each row's
// size, attempts, next attempt and flags, as one comparable string.
func dueState(ctx context.Context, view selectedWitness, service string, now time.Time) (string, error) {
	rows, e := view.PendingWitnessesFor(ctx, service, now, cll.MaxWitnesses)
	if e != nil {
		return "", e
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%d/%d/%d/%t/%t;", r.CheckpointSize, r.Attempts, r.NextAttemptAt.UnixNano(), r.Permanent, r.Receipt != nil)
	}
	return b.String(), nil
}
