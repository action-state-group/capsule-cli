package plugin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/action-state-group/checkpointed-local-log/go/mmr"
	"github.com/spf13/cobra"
)

// dealWitnessSees is said wherever the witness is configured.
const dealWitnessSees = "The witness sees one checkpoint per tick of this profile's checkpoint cadence: hashes, a size that grows by the same amount every tick, and a time on the cadence. It never sees content, how many deals there are, or when they happen."

var dealCadenceSchema = `CREATE TABLE IF NOT EXISTS deal_cadence (
	tick INTEGER PRIMARY KEY,
	at TEXT NOT NULL,
	due_next TEXT NOT NULL,
	entry_seq INTEGER NOT NULL,
	checkpoint_size INTEGER NOT NULL,
	checkpoint BLOB NOT NULL,
	leaves TEXT NOT NULL
)`

func cadenceFillerLeaf(nonce []byte) []byte {
	h := sha256.Sum256(append([]byte{0x03}, nonce...))
	return h[:]
}

// cadenceEmpty[h] is the root of an empty subtree of height h.
var cadenceEmpty = func() [][]byte {
	levels := make([][]byte, cli.DealCadenceDepth+1)
	leaf := sha256.Sum256([]byte{0x02})
	levels[0] = leaf[:]
	for h := 1; h <= cli.DealCadenceDepth; h++ {
		levels[h] = cli.CadenceNode(levels[h-1], levels[h-1])
	}
	return levels
}()

// cadenceTree returns the root of the sparse tree over leaves and, for
// index, its path: the sibling at each height, leaf level first.
func cadenceTree(leaves map[uint64][]byte, index uint64) (root []byte, path [][]byte) {
	level := leaves
	for h := 0; h < cli.DealCadenceDepth; h++ {
		sibling, ok := level[index^1]
		if !ok {
			sibling = cadenceEmpty[h]
		}
		path = append(path, sibling)
		next := make(map[uint64][]byte, len(level)/2+1)
		for i := range level {
			parent := i >> 1
			if _, done := next[parent]; done {
				continue
			}
			left, ok := level[parent<<1]
			if !ok {
				left = cadenceEmpty[h]
			}
			right, ok := level[parent<<1|1]
			if !ok {
				right = cadenceEmpty[h]
			}
			next[parent] = cli.CadenceNode(left, right)
		}
		level, index = next, index>>1
	}
	if r, ok := level[0]; ok {
		return r, path
	}
	return cadenceEmpty[cli.DealCadenceDepth], path
}

// dealPaddingRecordID is the id of an Evidence Layer padding record:
// {"record_type": "padding", "epistemic_type": "producer_claim",
// "store_nonce": <fresh 256-bit value>}, identified by its JCS digest. Its
// only content is the nonce; it is never an action and never counted.
func dealPaddingRecordID() ([]byte, error) {
	nonce, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	id, err := canonical.JSONDigest(map[string]interface{}{"record_type": "padding", "epistemic_type": "producer_claim", "store_nonce": nonce})
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(id)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// The stored state of one tick: every leaf, so a receipt can carry its path.
type dealTickLeaf struct {
	LogID            string `json:"log_id"`
	Size             uint64 `json:"size"`
	CheckpointSHA256 string `json:"checkpoint_sha256"`
	Salt             string `json:"salt"`
	Index            uint64 `json:"index"`
	// Statement is the deal checkpoint itself (base64url COSE), kept on the
	// device so a later report can show that an earlier, witnessed
	// checkpoint of the deal is a prefix of its current one. It is not part
	// of the leaf: the leaf commits to its SHA-256.
	Statement string `json:"statement,omitempty"`
}

type dealTickLeaves struct {
	Deals       map[string]dealTickLeaf `json:"deals"`
	FillerIndex uint64                  `json:"filler_index"`
	FillerNonce string                  `json:"filler_nonce"`
}

func (l dealTickLeaves) tree(index uint64) ([]byte, [][]byte, error) {
	leaves := map[uint64][]byte{}
	nonce, err := hex.DecodeString(l.FillerNonce)
	if err != nil {
		return nil, nil, err
	}
	leaves[l.FillerIndex] = cadenceFillerLeaf(nonce)
	for _, leaf := range l.Deals {
		value, err := cli.CadenceLeaf(leaf.LogID, leaf.Size, leaf.CheckpointSHA256, leaf.Salt)
		if err != nil {
			return nil, nil, err
		}
		leaves[leaf.Index] = value
	}
	root, path := cadenceTree(leaves, index)
	return root, path, nil
}

type dealTick struct {
	n              int64
	at, dueNext    time.Time
	entrySeq, size uint64
	checkpoint     []byte
	leaves         dealTickLeaves
}

func (s *dealSession) ticks(ctx context.Context, limit int) ([]dealTick, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tick, at, due_next, entry_seq, checkpoint_size, checkpoint, leaves FROM deal_cadence ORDER BY tick DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []dealTick
	for rows.Next() {
		var t dealTick
		var at, due, leaves string
		if err = rows.Scan(&t.n, &at, &due, &t.entrySeq, &t.size, &t.checkpoint, &leaves); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if t.at, err = time.Parse(time.RFC3339, at); err == nil {
			t.dueNext, err = time.Parse(time.RFC3339, due)
		}
		if err == nil {
			err = json.Unmarshal([]byte(leaves), &t.leaves)
		}
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, t)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func (s *dealSession) dealIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT deal_id FROM deal_steps ORDER BY deal_id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}

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
// the previous tick and a random jitter, never on what the deals did.
func (s *dealSession) tick(ctx context.Context) (map[string]any, error) {
	cfg, err := s.p.DealCadence()
	if err != nil {
		return nil, err
	}
	if s.p.Checkpoint.Signing == (cli.Secret{}) {
		return nil, cli.InputError("the deal profile has no checkpoint key (see `deal init`)")
	}
	p, err := s.cadenceProfile()
	if err != nil {
		return nil, err
	}
	service, err := cli.ServiceID(p)
	if err != nil {
		return nil, err
	}
	last, err := s.ticks(ctx, 1)
	if err != nil {
		return nil, err
	}
	now := cli.DealClock()
	out := map[string]any{"cadence_log": p.LogID}
	if len(last) == 1 && now.Before(last[0].dueNext) {
		out["state"], out["due"] = "not_due", last[0].dueNext.Format(time.RFC3339)
	} else {
		var n int64 = 1
		if len(last) == 1 {
			n = last[0].n + 1
		}
		t, err := s.cutTick(ctx, p, cfg, n, now)
		if err != nil {
			return nil, err
		}
		out["state"], out["tick"], out["checkpoint"], out["due"] = "ticked", t.n, t.size, t.dueNext.Format(time.RFC3339)
	}
	if service == "" {
		out["witness"] = "not_configured"
		return out, nil
	}
	out["witness_sees"] = dealWitnessSees
	delivered, pending, err := s.deliverCadence(ctx, p, service, now)
	if err != nil {
		return nil, err
	}
	out["delivered"], out["pending"] = delivered, pending
	return out, nil
}

func (s *dealSession) cutTick(ctx context.Context, p cli.Profile, cfg cli.DealCadenceConfig, n int64, now time.Time) (dealTick, error) {
	// Times leaving this device are coarsened to the minute.
	at := now.UTC().Truncate(time.Minute)
	ids, err := s.dealIDs(ctx)
	if err != nil {
		return dealTick{}, err
	}
	if len(ids) >= 1<<cli.DealCadenceDepth {
		return dealTick{}, cli.InputError("too many deals for one cadence tree")
	}
	used := map[uint64]bool{}
	position := func() (uint64, error) {
		for {
			var b [8]byte
			if _, err := rand.Read(b[:]); err != nil {
				return 0, err
			}
			i := binary.BigEndian.Uint64(b[:]) % (1 << cli.DealCadenceDepth)
			if !used[i] {
				used[i] = true
				return i, nil
			}
		}
	}
	leaves := dealTickLeaves{Deals: map[string]dealTickLeaf{}}
	if leaves.FillerIndex, err = position(); err != nil {
		return dealTick{}, err
	}
	if leaves.FillerNonce, err = randomHex(32); err != nil {
		return dealTick{}, err
	}
	for _, id := range ids {
		if err = s.useDeal(ctx, id, false); err != nil {
			return dealTick{}, err
		}
		cp, err := cli.CutCheckpointAt(ctx, localOnly(s.dp), s.t.Log, at)
		err = errors.Join(err, s.t.Close())
		s.t = nil
		if err != nil {
			return dealTick{}, err
		}
		sum := sha256.Sum256(cp.Bytes)
		leaf := dealTickLeaf{LogID: cli.DealLogID(id), Size: cp.Size, CheckpointSHA256: hex.EncodeToString(sum[:]), Statement: base64.RawURLEncoding.EncodeToString(cp.Bytes)}
		if leaf.Salt, err = randomHex(32); err != nil {
			return dealTick{}, err
		}
		if leaf.Index, err = position(); err != nil {
			return dealTick{}, err
		}
		leaves.Deals[id] = leaf
	}
	root, _, err := leaves.tree(0)
	if err != nil {
		return dealTick{}, err
	}
	t, err := cli.OpenTarget(ctx, p, cli.UseInitialization)
	if err != nil {
		return dealTick{}, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	appended, err := t.Log.Append(ctx, cll.AppendInput{Value: root, AppendedAt: at})
	if err != nil {
		return dealTick{}, err
	}
	// Padding (Evidence Layer padding records) brings the leaf count to a
	// multiple of the bucket. With the default bucket of 1 there is none:
	// each tick adds exactly one entry.
	for count := appended.Entry.Seq; count%cfg.PadBucket != 0; count++ {
		id, err := dealPaddingRecordID()
		if err != nil {
			return dealTick{}, err
		}
		if _, err = t.Log.Append(ctx, cll.AppendInput{Value: id, AppendedAt: at}); err != nil {
			return dealTick{}, err
		}
	}
	cp, err := cli.CutCheckpointAt(ctx, p, t.Log, at)
	if err != nil {
		return dealTick{}, err
	}
	jitter := time.Duration(0)
	if cfg.Jitter > 0 {
		r, err := rand.Int(rand.Reader, big.NewInt(int64(2*cfg.Jitter)+1))
		if err != nil {
			return dealTick{}, err
		}
		jitter = time.Duration(r.Int64()) - cfg.Jitter
	}
	tick := dealTick{n: n, at: at, dueNext: now.UTC().Add(cfg.Interval + jitter).Truncate(time.Second), entrySeq: appended.Entry.Seq, size: cp.Size, checkpoint: cp.Bytes, leaves: leaves}
	encoded, err := json.Marshal(leaves)
	if err != nil {
		return dealTick{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO deal_cadence (tick, at, due_next, entry_seq, checkpoint_size, checkpoint, leaves) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tick.n, tick.at.Format(time.RFC3339), tick.dueNext.Format(time.RFC3339), tick.entrySeq, tick.size, tick.checkpoint, string(encoded))
	return tick, err
}

// deliverCadence offers every cadence checkpoint still waiting for the
// witness. A witness that is slow or down leaves it pending; deals go on.
func (s *dealSession) deliverCadence(ctx context.Context, p cli.Profile, service string, now time.Time) (delivered []uint64, pending []map[string]any, err error) {
	t, err := cli.OpenTarget(ctx, p, cli.UseCLL)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	waiting, err := t.Log.PendingWitnesses(ctx, now.Add(24*time.Hour), cll.MaxWitnesses)
	if err != nil {
		return nil, nil, err
	}
	delivered, pending = []uint64{}, []map[string]any{}
	for _, w := range waiting {
		if w.WitnessID != service {
			continue
		}
		state, err := cli.DeliverWitness(ctx, p, t.Log, service, w.CheckpointSize)
		if err != nil || state.Receipt == nil || cli.VerifyWitness(p, state) != nil {
			reason, text := cli.WitnessPendingReason(state, p.Checkpoint.Endpoint)
			pending = append(pending, map[string]any{"checkpoint": w.CheckpointSize, "reason": reason, "text": text})
			continue
		}
		delivered = append(delivered, w.CheckpointSize)
	}
	return delivered, pending, nil
}

// dealWitnessState says, for the deal checkpoint a report carries, where it
// stands with the witness, truthfully: not_configured, scheduled (not yet in
// a tick), pending (in a tick whose delivery has not completed) or witnessed
// (with the whole chain a verifier needs).
//
// Witnessed comes in two extents. "all": a tick whose receipt verifies holds
// this very checkpoint. "part": no such tick yet, but a verified tick holds
// an EARLIER checkpoint of the deal; the chain then carries that checkpoint
// and an MMR consistency proof from it to the current one, so a verifier
// sees exactly which steps the witness covers. Of the ticks that hold a
// checkpoint, the newest whose receipt verifies is used: a later tick still
// pending never hides an earlier verified one.
func (s *dealSession) dealWitnessState(ctx context.Context, dealID string, statement []byte) (_ map[string]interface{}, err error) {
	service, err := cli.ServiceID(s.p)
	if err != nil || service == "" || s.p.LogID == "" {
		return map[string]interface{}{"state": "not_configured"}, err
	}
	current, err := checkpoint.ParseRecord(statement)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(statement)
	want := hex.EncodeToString(sum[:])
	ticks, err := s.ticks(ctx, 1<<20)
	if err != nil {
		return nil, err
	}
	t, err := cli.OpenTarget(ctx, s.p, cli.UseCLLRead)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	witnessed := func(tick dealTick) (cll.WitnessState, bool, error) {
		state, err := t.Log.GetWitness(ctx, service, tick.size)
		if errors.Is(err, cll.ErrNotFound) {
			return state, false, nil
		}
		if err != nil {
			return state, false, err
		}
		return state, state.Receipt != nil && cli.VerifyWitness(s.p, state) == nil, nil
	}
	// This very checkpoint, in the newest tick whose receipt verifies. A tick
	// that holds an earlier checkpoint with the same steps (only the deal's
	// sealed reports came after it) is where those steps wait for the witness:
	// its pending state is this deal's.
	steps, err := s.stepsIn(ctx, dealID, mmrLeafCount(current.MMRSize))
	if err != nil {
		return nil, err
	}
	var held *dealTick
	var heldState cll.WitnessState
	for i := range ticks {
		leaf, ok := ticks[i].leaves.Deals[dealID]
		if !ok {
			continue
		}
		if leaf.CheckpointSHA256 != want {
			if held == nil && leaf.Size < current.MMRSize {
				same, err := s.stepsIn(ctx, dealID, mmrLeafCount(leaf.Size))
				if err != nil {
					return nil, err
				}
				if state, err := t.Log.GetWitness(ctx, service, ticks[i].size); same == steps && (err == nil || errors.Is(err, cll.ErrNotFound)) && (state.Receipt == nil || cli.VerifyWitness(s.p, state) != nil) {
					held, heldState = &ticks[i], state
				}
			}
			continue
		}
		state, ok, err := witnessed(ticks[i])
		if err != nil {
			return nil, err
		}
		if ok {
			return s.cadenceChain(ctx, t, ticks[i], dealID, state, nil)
		}
		if held == nil {
			held, heldState = &ticks[i], state
		}
	}
	rest := map[string]interface{}{"state": "scheduled", "cadence": s.cadenceWords()}
	switch {
	case held != nil:
		reason, text := cli.WitnessPendingReason(heldState, s.p.Checkpoint.Endpoint)
		rest = map[string]interface{}{"state": "pending", "tick": cli.Integer(uint64(held.n)), "reason": reason, "text": text, "cadence": s.cadenceWords()}
	case len(ticks) > 0:
		rest["due"] = ticks[0].dueNext.Format(time.RFC3339)
	}
	// An earlier checkpoint of this deal, in the newest tick whose receipt
	// verifies.
	for i := range ticks {
		leaf, ok := ticks[i].leaves.Deals[dealID]
		if !ok || leaf.Statement == "" || leaf.Size >= current.MMRSize {
			continue
		}
		state, ok, err := witnessed(ticks[i])
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out, err := s.cadenceChain(ctx, t, ticks[i], dealID, state, &current)
		if err != nil {
			return nil, err
		}
		if out != nil {
			out["rest"] = rest
			return out, nil
		}
	}
	return rest, nil
}

// cadenceChain is the witnessed chain for the deal checkpoint a tick holds.
// With current set, that checkpoint is an earlier one, and the chain also
// carries it and its consistency proof to current; nil when the stored
// checkpoint does not match its leaf or is not a prefix of current.
func (s *dealSession) cadenceChain(ctx context.Context, t *cli.Target, tick dealTick, dealID string, state cll.WitnessState, current *checkpoint.Record) (map[string]interface{}, error) {
	leaf := tick.leaves.Deals[dealID]
	_, path, err := tick.leaves.tree(leaf.Index)
	if err != nil {
		return nil, err
	}
	logState, err := t.Log.LoadCLL(ctx)
	if err != nil {
		return nil, err
	}
	tree, err := mmr.New(logState.Nodes)
	if err != nil {
		return nil, err
	}
	proof, err := tree.InclusionProof(tick.entrySeq-1, tick.size)
	if err != nil {
		return nil, err
	}
	hexPath := make([]interface{}, len(path))
	for i := range path {
		hexPath[i] = hex.EncodeToString(path[i])
	}
	receipt := map[string]interface{}{
		"ts_url": trimEndpoint(s.p.Checkpoint.Endpoint), "entry_hash": state.Receipt.EntryHash,
		"receipt_b64": base64.StdEncoding.EncodeToString(state.Receipt.Bytes),
		"leaf_index":  cli.Integer(uint64(*state.Receipt.LeafIndex)), "tree_size": cli.Integer(uint64(*state.Receipt.TreeSize)),
	}
	out := map[string]interface{}{
		"state": "witnessed", "extent": "all", "checkpoint_at": tick.at.UTC().Format(time.RFC3339),
		"log_id": leaf.LogID, "size": cli.Integer(leaf.Size), "salt": leaf.Salt,
		"index": cli.Integer(leaf.Index), "path": hexPath,
		"cadence": map[string]interface{}{
			"log_id": s.p.LogID, "entry_index": cli.Integer(tick.entrySeq - 1),
			"checkpoint":      map[string]interface{}{"cose": base64.RawURLEncoding.EncodeToString(tick.checkpoint)},
			"inclusion_proof": cli.ProofJSON(proof),
			"witnesses":       []interface{}{receipt},
		},
	}
	if current == nil {
		return out, nil
	}
	earlier, err := base64.RawURLEncoding.DecodeString(leaf.Statement)
	if err != nil {
		return nil, nil
	}
	sum := sha256.Sum256(earlier)
	record, err := checkpoint.ParseRecord(earlier)
	if err != nil || hex.EncodeToString(sum[:]) != leaf.CheckpointSHA256 || record.LogID != current.LogID || record.MMRSize != leaf.Size {
		return nil, nil
	}
	dealState, err := s.t.Log.LoadCLL(ctx)
	if err != nil {
		return nil, err
	}
	dealTree, err := mmr.New(dealState.Nodes)
	if err != nil {
		return nil, err
	}
	consistency, err := dealTree.ConsistencyProof(record.MMRSize, current.MMRSize)
	if err != nil {
		return nil, nil
	}
	// Counted in steps: the deal's log also holds its sealed reports, which
	// are not steps.
	witnessedSteps, err := s.stepsIn(ctx, dealID, mmrLeafCount(record.MMRSize))
	if err != nil {
		return nil, err
	}
	steps, err := s.stepsIn(ctx, dealID, mmrLeafCount(current.MMRSize))
	if err != nil {
		return nil, err
	}
	out["extent"] = "part"
	out["steps_witnessed"] = cli.Integer(witnessedSteps)
	out["steps"] = cli.Integer(steps)
	out["earlier"] = map[string]interface{}{
		"checkpoint":        map[string]interface{}{"cose": leaf.Statement},
		"consistency_proof": consistencyJSON(consistency),
	}
	return out, nil
}

func consistencyJSON(p mmr.ConsistencyProof) map[string]interface{} {
	hashes := func(values [][]byte) []interface{} {
		out := make([]interface{}, len(values))
		for i := range values {
			out[i] = hex.EncodeToString(values[i])
		}
		return out
	}
	witness := make([]interface{}, len(p.Witness))
	for i := range p.Witness {
		witness[i] = hashes(p.Witness[i])
	}
	return map[string]interface{}{
		"v": cli.Integer(p.V), "kind": p.Kind, "old_size": cli.Integer(p.OldSize), "new_size": cli.Integer(p.NewSize),
		"old_peaks": hashes(p.OldPeaks), "witness": witness, "new_peaks": hashes(p.NewPeaks),
	}
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
	cmd := &cobra.Command{Use: "tick", Short: "The checkpoint cadence's poll: when a tick is due, publish the profile's cadence checkpoint to its witness; a run that is not due publishes nothing (schedule it every 5 minutes with --wait-up-to 5m; deal events never publish)", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
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
