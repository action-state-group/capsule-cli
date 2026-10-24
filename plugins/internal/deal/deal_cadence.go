package deal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

// dealWitnessSees is said wherever the witness is configured.
const dealWitnessSees = "The witness sees one checkpoint per tick of this profile's checkpoint cadence: hashes, a size that grows by the same amount every tick, and a time on the cadence. It never sees content, how many deals there are, or when they happen."

// dealCadenceSchema is the table released builds kept their ticks in. The
// cadence now keeps its own (evidencebook's cadence_ticks); this one is read
// once, and again after a released build wrote to it, by migrate.
var dealCadenceSchema = `CREATE TABLE IF NOT EXISTS deal_cadence (
	tick INTEGER PRIMARY KEY,
	at TEXT NOT NULL,
	due_next TEXT NOT NULL,
	entry_seq INTEGER NOT NULL,
	checkpoint_size INTEGER NOT NULL,
	checkpoint BLOB NOT NULL,
	leaves TEXT NOT NULL
)`

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *dealSession) dealIDs(ctx context.Context) ([]string, error) { return s.index.dealIDs(ctx) }

// localOnly is the profile with its witness removed: a deal's own checkpoint
// is signed but never queued for, or sent to, the witness.
func localOnly(p cli.Profile) cli.Profile {
	p.Checkpoint.Endpoint = ""
	return p
}

// cadenceProfile makes sure the profile has a cadence log, giving a profile
// made before there was one its own random log id.
func (s *dealSession) cadenceProfile() (cli.Profile, error) {
	if s.p.LogID != "" {
		return s.p, nil
	}
	suffix, err := randomHex(8)
	if err != nil {
		return cli.Profile{}, err
	}
	s.p.LogID = "deal-cadence/" + suffix
	return s.p, cli.SaveProfile(s.p, true)
}

// tick publishes the cadence checkpoint when a tick is due, and retries any
// delivery still pending either way. The time a tick is due depends only on
// the previous tick and a random jitter, never on what the deals did. The
// tick itself is evidencebook's cadence over the shelf: it checkpoints the
// live logs only, with the occupancy of every log the profile ever had
// (capsule-cli#242 note §2.5).
func (s *dealSession) tick(ctx context.Context) (map[string]any, error) {
	c, p, err := s.cadenceOf(ctx)
	if err != nil {
		return nil, err
	}
	service, err := cli.ServiceID(p)
	if err != nil {
		return nil, err
	}
	if err := s.coverLiveSteps(ctx, c); err != nil {
		return nil, err
	}
	now := cli.DealClock()
	result, err := c.Tick(ctx, now)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"cadence_log": p.LogID}
	if result.Ticked {
		out["state"], out["tick"], out["checkpoint"], out["due"] = "ticked", result.Tick.N, result.Tick.CheckpointSize, result.Tick.DueNext.Format(time.RFC3339)
	} else {
		out["state"], out["due"] = "not_due", result.DueNext.Format(time.RFC3339)
	}
	if service == "" {
		out["witness"] = "not_configured"
		return out, nil
	}
	out["witness_sees"] = dealWitnessSees
	if err := s.settleEnded(ctx, c); err != nil {
		return nil, err
	}
	delivered, pending, err := s.deliverCadence(ctx, c, p, service, now)
	if err != nil {
		return nil, err
	}
	out["delivered"], out["pending"] = delivered, pending
	return out, nil
}

// deliverCadence makes one delivery attempt of every cadence checkpoint
// still pending for the witness, and reports each verified receipt to the
// cadence (Witnessed), which records its leaves as witnessed and lets ended
// deals leave the live set. A receipt stored but not reported (an error in
// between) leaves the session unmigrated, so close does not mark the store
// seen and the next open's repair reports it (witnessNativeTicks); a
// receipt a released build stored is found the same way.
func (s *dealSession) deliverCadence(ctx context.Context, c *evidencebook.Cadence, p cli.Profile, service string, now time.Time) (delivered []uint64, pending []map[string]any, err error) {
	t, err := cli.OpenDealTarget(ctx, p, cli.UseCLL, s)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	if err := cli.RearmRefusedLog(ctx, t.Log, service, now); err != nil {
		return nil, nil, err
	}
	waiting, err := t.Log.PendingWitnesses(ctx, now.Add(24*time.Hour), cll.MaxWitnesses)
	if err != nil {
		return nil, nil, err
	}
	delivered, pending = []uint64{}, []map[string]any{}
	for _, w := range waiting {
		if w.WitnessID != service {
			continue
		}
		// A delivery can store a receipt and still fail (a cancelled read
		// after the commit), so the session is unmigrated from before the
		// attempt until the receipt, if one landed, is reported.
		migrated := s.migrated
		s.migrated = false
		// A delivery error is pending, read from state as before; whether a
		// receipt landed anyway is read back from the log below.
		state, _ := dealDeliverWitness(ctx, p, t.Log, service, w.CheckpointSize)
		receipt, ok, err := verifiedReceipt(ctx, p, t.Log, service, w.CheckpointSize)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			reason, text := cli.WitnessPendingReason(state, p.Checkpoint.Endpoint)
			pending = append(pending, map[string]any{"checkpoint": w.CheckpointSize, "reason": reason, "text": text})
			s.migrated = migrated
			continue
		}
		n, found, err := tickAtSize(ctx, c, w.CheckpointSize)
		if err != nil {
			return nil, nil, err
		}
		// A cadence checkpoint no tick holds (a released build's core write,
		// note §3.3) is witnessed on the log but not part of the cadence.
		if found {
			if err := c.Witnessed(ctx, n, *receipt); err != nil {
				return nil, nil, err
			}
			delivered = append(delivered, w.CheckpointSize)
		}
		s.migrated = migrated
	}
	return delivered, pending, nil
}

// dealDeliverWitness is cli.DeliverWitness, a test hook for a delivery
// that stores its receipt and then fails.
var dealDeliverWitness = cli.DeliverWitness

// tickAtSize is the cadence's tick whose checkpoint has size, walking back
// from the newest: a pending delivery is for a recent tick.
func tickAtSize(ctx context.Context, c *evidencebook.Cadence, size uint64) (uint64, bool, error) {
	last, ok, err := c.Last(ctx)
	if err != nil || !ok {
		return 0, false, err
	}
	for n := last.N; n >= 1; n-- {
		tick, err := c.Get(ctx, n)
		if err != nil {
			return 0, false, err
		}
		if tick.CheckpointSize == size {
			return n, true, nil
		}
		if tick.CheckpointSize != 0 && tick.CheckpointSize < size {
			break
		}
	}
	return 0, false, nil
}

// dealWitnessState says, for the deal checkpoint a report carries, where it
// stands with the witness, truthfully: not_configured, scheduled (not yet in
// a tick), pending (in a tick whose delivery has not completed) or witnessed
// (with the whole chain a verifier needs).
//
// Witnessed comes in two extents (evidencebook's Cadence.Chain, which is
// capsule-cli's cadenceChain member for member). "all": a witnessed tick
// holds this very checkpoint. "part": a witnessed tick holds an EARLIER
// checkpoint of the deal; the chain then carries that checkpoint and an MMR
// consistency proof from it to the current one, so a verifier sees exactly
// which steps the witness covers, and the deal adds its step counts.
func (s *dealSession) dealWitnessState(ctx context.Context, dealID string, statement []byte) (_ map[string]interface{}, err error) {
	service, err := cli.ServiceID(s.p)
	if err != nil || service == "" || s.p.LogID == "" {
		return map[string]interface{}{"state": "not_configured"}, err
	}
	current, err := checkpoint.ParseRecord(statement)
	if err != nil {
		return nil, err
	}
	c, p, err := s.cadenceOf(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := c.Chain(ctx, statement)
	if errors.Is(err, evidencebook.ErrNotFound) {
		return s.notYetWitnessed(ctx, c, p, service, dealID, current)
	}
	if err != nil {
		return nil, err
	}
	chain, err := bundleValue(raw)
	if err != nil {
		return nil, err
	}
	// The cadence recorded the receipt when it verified; the chain counts
	// only if it still verifies for the witness this profile names now (a
	// changed endpoint or key leaves the deal unwitnessed, as before).
	if ok, err := chainVerifiesNow(ctx, p, service, chain); err != nil {
		return nil, err
	} else if !ok {
		return s.notYetWitnessed(ctx, c, p, service, dealID, current)
	}
	if chain["extent"] != "part" {
		return chain, nil
	}
	sizeNumber, _ := chain["size"].(json.Number)
	size, err := strconv.ParseUint(string(sizeNumber), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: the cadence chain's size: %v", cli.ErrConflict, err)
	}
	witnessedSteps, err := s.stepsIn(ctx, dealID, mmrLeafCount(size))
	if err != nil {
		return nil, err
	}
	steps, err := s.stepsIn(ctx, dealID, mmrLeafCount(current.MMRSize))
	if err != nil {
		return nil, err
	}
	chain["steps_witnessed"], chain["steps"] = cli.Integer(witnessedSteps), cli.Integer(steps)
	if chain["rest"], err = s.notYetWitnessed(ctx, c, p, service, dealID, current); err != nil {
		return nil, err
	}
	return chain, nil
}

// chainVerifiesNow reports whether the cadence checkpoint a chain names has
// a receipt, on the cadence log, that verifies for the profile's current
// witness service and key.
func chainVerifiesNow(ctx context.Context, p cli.Profile, service string, chain map[string]interface{}) (ok bool, err error) {
	cadence, _ := chain["cadence"].(map[string]interface{})
	cp, _ := cadence["checkpoint"].(map[string]interface{})
	cose, _ := cp["cose"].(string)
	statement, err := base64.RawURLEncoding.DecodeString(cose)
	if err != nil {
		return false, nil
	}
	record, err := checkpoint.ParseRecord(statement)
	if err != nil {
		return false, nil
	}
	t, err := cli.OpenTarget(ctx, p, cli.UseCLLRead)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	state, err := t.Log.GetWitness(ctx, service, record.MMRSize)
	if errors.Is(err, cll.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state.Receipt != nil && cli.VerifyWitness(p, state) == nil, nil
}

// bundleValue is v as a bundle's members hold JSON: decoded objects, with
// every number a json.Number, as cli.Integer writes them, so the bundle's
// canonical form (JCS) takes it.
func bundleValue(v map[string]any) (map[string]interface{}, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var out map[string]interface{}
	if err := d.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// notYetWitnessed is where a deal checkpoint no witnessed tick holds
// stands: pending when the newest tick holding the deal's log holds this
// checkpoint, or an earlier one with the same steps (only the deal's sealed
// reports came after it), and its delivery has not completed; scheduled
// otherwise, with the next tick's due time. Only that one tick is read
// (note §2.5): a live log is in every tick.
func (s *dealSession) notYetWitnessed(ctx context.Context, c *evidencebook.Cadence, p cli.Profile, service, dealID string, current checkpoint.Record) (_ map[string]interface{}, err error) {
	rest := map[string]interface{}{"state": "scheduled", "cadence": s.cadenceWords()}
	last, ok, err := c.Last(ctx)
	if err != nil || !ok {
		return rest, err
	}
	rest["due"] = last.DueNext.Format(time.RFC3339)
	leaf, in := last.Leaves.Deals[cli.DealLogID(dealID)]
	if !in || leaf.Size > current.MMRSize {
		return rest, nil
	}
	held, err := s.stepsIn(ctx, dealID, mmrLeafCount(leaf.Size))
	if err != nil {
		return nil, err
	}
	steps, err := s.stepsIn(ctx, dealID, mmrLeafCount(current.MMRSize))
	if err != nil {
		return nil, err
	}
	if held != steps {
		return rest, nil
	}
	t, err := cli.OpenTarget(ctx, p, cli.UseCLLRead)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	state, err := t.Log.GetWitness(ctx, service, last.CheckpointSize)
	if err != nil && !errors.Is(err, cll.ErrNotFound) {
		return nil, err
	}
	// A tick whose receipt verifies is not pending: what is not yet
	// witnessed (only sealed reports since) waits for the next tick.
	if err == nil && state.Receipt != nil && cli.VerifyWitness(p, state) == nil {
		return rest, nil
	}
	reason, text := cli.WitnessPendingReason(state, p.Checkpoint.Endpoint)
	return map[string]interface{}{"state": "pending", "tick": cli.Integer(last.N), "reason": reason, "text": text, "cadence": s.cadenceWords()}, nil
}

func trimEndpoint(endpoint string) string {
	for len(endpoint) > 0 && endpoint[len(endpoint)-1] == '/' {
		endpoint = endpoint[:len(endpoint)-1]
	}
	return endpoint
}

// dealSleep waits d, or until ctx ends; tests replace it.
var dealSleep = func(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The checkpoint cadence's poll (`cll checkpoint cadence`, also `deal
// tick`). A tick is published when the poll runs at or after its due time,
// so the jitter in the due time only shows if the poll runs near it: a
// scheduler that runs it every N minutes passes --wait-up-to N (5m by the
// documented schedule), and the run then waits for each due time inside
// that window and publishes on time. A run with no tick due publishes
// nothing. The waiting happens with the store unlocked, and every publish
// re-checks the due time under the lock.
func dealTickCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "tick", Short: "The checkpoint cadence's poll: when a tick is due, publish the profile's cadence checkpoint to its witness; a run that is not due cuts no tick and only retries any delivery still pending (schedule it every 5 minutes with --wait-up-to 5m; deal events never publish)", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		wait, _ := c.Flags().GetDuration("wait-up-to")
		if wait < 0 {
			return cli.InputError("--wait-up-to must not be negative")
		}
		until := cli.DealClock().Add(wait)
		published := 0
		// Merchant emails deferred for want of DNS are sealed first, so
		// their key records are in the checkpoint this tick publishes.
		// A failure there is reported, and never stops the tick.
		emails, err := sealDeferredEmails(c)
		if err != nil {
			emails = map[string]any{"error": err.Error()}
		}
		for {
			var out map[string]any
			err := runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
				var err error
				out, err = s.tick(ctx)
				return err
			})
			if err != nil {
				return err
			}
			if out["state"] == "ticked" {
				published++
			}
			due, err := time.Parse(time.RFC3339, fmt.Sprint(out["due"]))
			now := cli.DealClock()
			if wait == 0 || err != nil || due.After(until) {
				out["published_this_run"] = published
				if emails != nil {
					out["merchant_emails"] = emails
				}
				return cli.Output(c, out)
			}
			if d := due.Sub(now); d > 0 {
				if err := dealSleep(c.Context(), d); err != nil {
					return err
				}
			}
		}
	}}
	cmd.Flags().Duration("wait-up-to", 0, "The scheduler's period (for example 5m): wait for each tick due within this long and publish it on time, so the cadence keeps its jitter; the run stays alive up to this long")
	return cmd
}

// cadenceWords is this profile's cadence as a receipt says it, for example
// "every 5m, give or take 2m": the profile's own values, not the default.
func (s *dealSession) cadenceWords() string {
	cfg, err := s.p.DealCadence()
	if err != nil {
		return ""
	}
	words := "every " + cli.ShortDuration(cfg.Interval)
	if cfg.Jitter > 0 {
		words += ", give or take " + cli.ShortDuration(cfg.Jitter)
	}
	return words
}

// aboutDuration writes a wait for a reader: 1h10m, 1h, 7m or 30s.
func aboutDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Hour && d%time.Minute == 0:
		return fmt.Sprintf("%dh%dm", d/time.Hour, d%time.Hour/time.Minute)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

// mmrLeafCount is the number of leaves (deal steps) in an MMR of this size.
func mmrLeafCount(size uint64) uint64 {
	var leaves uint64
	for peak := uint64(1) << 62; peak > 0; peak >>= 1 {
		nodes := 2*peak - 1
		if size >= nodes {
			size -= nodes
			leaves += peak
		}
	}
	return leaves
}
