package cli

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
	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/action-state-group/cll-go/mmr"
	"github.com/spf13/cobra"
)

// A deal profile publishes to its witness on time alone, never on activity.
//
// Each deal is its own log, and none of those logs is ever published: a
// witness that saw them would learn how many deals there are, when each one
// starts, and how many steps each has. Instead the profile keeps one cadence
// log (its log_id). At every tick, and whether or not anything happened,
// `deal tick` cuts each deal's checkpoint locally, builds a fixed-depth
// Merkle tree whose leaves are those checkpoints (each with a fresh random
// salt, at a fresh random position) plus one random filler leaf, appends the
// tree's root as exactly one entry of the cadence log, and publishes the
// cadence log's checkpoint. The witness sees one log that grows by one entry
// per tick on the profile's clock: the volume signal is reduced to the tick
// count, and the timing signal to the cadence.
//
// A deal's receipt then carries the chain from its own checkpoint to the
// witnessed cadence checkpoint: the leaf's salt and position, the tree path
// (always dealCadenceDepth hashes, so it says nothing about other deals),
// the cadence entry's inclusion proof, the cadence checkpoint and the
// witness receipt. `capsulectl verify --bundle` checks every link.

const (
	// dealCadenceDepth fixes every path at 16 hashes: up to 65535 deals.
	dealCadenceDepth = 16
	// The public witness a new deal profile uses unless told otherwise. Its
	// key ships in this binary: a user trusts it as far as they trust the
	// release they installed.
	dealDefaultWitness    = "https://witness.agentactioncapsule.org"
	dealDefaultWitnessKey = "39bb654c9dc0afe1c0edef0deffaa69099b8518836c9ba26e0491535840f96b5"
	// The bundle extension that carries a checkpoint's witnessed chain
	// through a cadence log. Its name and fields are generic, so a neutral
	// verifier can read it without knowing what a deal is. Extension names
	// are a registered space; until this one is registered it is a private
	// kind, so it carries the x- prefix.
	dealCadenceExtension = "x-cadence-witness/v0"
)

// earlierCadenceExtensions are the names bundles carried the chain under
// before, still read: cadence-witness/v0, and x-deal-cadence-v0 (whose
// chain names its log in deal_log_id).
var earlierCadenceExtensions = []string{"cadence-witness/v0", "x-deal-cadence-v0"}

// cadenceChainOf is the cadence chain a bundle carries, under its name or
// a name bundles carried it under before.
func cadenceChainOf(b map[string]interface{}) map[string]interface{} {
	ext, _ := b["extensions"].(map[string]interface{})
	for _, name := range append([]string{dealCadenceExtension}, earlierCadenceExtensions...) {
		if chain, ok := ext[name].(map[string]interface{}); ok {
			return chain
		}
	}
	return nil
}

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

type dealCadenceConfig struct {
	interval, jitter time.Duration
	padBucket        uint64
}

func (p Profile) dealCadence() (dealCadenceConfig, error) {
	// The checkpoint cadence by default: a tick every 5m, give or take 1m
	// (288 a day). The jitter must stay under half the interval.
	cfg := dealCadenceConfig{interval: 5 * time.Minute, jitter: time.Minute, padBucket: 1}
	var err error
	if p.Cadence.Interval != "" {
		if cfg.interval, err = time.ParseDuration(p.Cadence.Interval); err != nil {
			return cfg, inputError("cadence.interval must be a duration such as 1h")
		}
	}
	if p.Cadence.Jitter != "" {
		if cfg.jitter, err = time.ParseDuration(p.Cadence.Jitter); err != nil {
			return cfg, inputError("cadence.jitter must be a duration such as 10m")
		}
	}
	if p.Cadence.PadBucket != 0 {
		cfg.padBucket = p.Cadence.PadBucket
	}
	switch {
	case cfg.interval < time.Minute:
		return cfg, inputError("cadence.interval must be at least 1m")
	case cfg.jitter < 0 || 2*cfg.jitter >= cfg.interval:
		return cfg, inputError("cadence.jitter must be at least 0 and under half of cadence.interval")
	case cfg.padBucket > 1024:
		return cfg, inputError("cadence.pad_bucket must be at most 1024")
	}
	return cfg, nil
}

// The tree. Leaves, nodes and the empty leaf are domain-separated.
func cadenceLeaf(logID string, size uint64, checkpointSHA256, salt string) ([]byte, error) {
	jcs, err := canonical.JCS(map[string]interface{}{"checkpoint_sha256": checkpointSHA256, "log_id": logID, "salt": salt, "size": integer(size)})
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(append([]byte{0x00}, jcs...))
	return h[:], nil
}

func cadenceFillerLeaf(nonce []byte) []byte {
	h := sha256.Sum256(append([]byte{0x03}, nonce...))
	return h[:]
}

func cadenceNode(left, right []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(left)
	h.Write(right)
	return h.Sum(nil)
}

// cadenceEmpty[h] is the root of an empty subtree of height h.
var cadenceEmpty = func() [][]byte {
	levels := make([][]byte, dealCadenceDepth+1)
	leaf := sha256.Sum256([]byte{0x02})
	levels[0] = leaf[:]
	for h := 1; h <= dealCadenceDepth; h++ {
		levels[h] = cadenceNode(levels[h-1], levels[h-1])
	}
	return levels
}()

// cadenceTree returns the root of the sparse tree over leaves and, for
// index, its path: the sibling at each height, leaf level first.
func cadenceTree(leaves map[uint64][]byte, index uint64) (root []byte, path [][]byte) {
	level := leaves
	for h := 0; h < dealCadenceDepth; h++ {
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
			next[parent] = cadenceNode(left, right)
		}
		level, index = next, index>>1
	}
	if r, ok := level[0]; ok {
		return r, path
	}
	return cadenceEmpty[dealCadenceDepth], path
}

// cadenceFold recomputes a root from a leaf, its position and its path.
func cadenceFold(leaf []byte, index uint64, path [][]byte) []byte {
	h := leaf
	for level, sibling := range path {
		if (index>>level)&1 == 0 {
			h = cadenceNode(h, sibling)
		} else {
			h = cadenceNode(sibling, h)
		}
	}
	return h
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
		value, err := cadenceLeaf(leaf.LogID, leaf.Size, leaf.CheckpointSHA256, leaf.Salt)
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
func localOnly(p Profile) Profile {
	p.Checkpoint.Endpoint = ""
	return p
}

// cadenceProfile makes sure the profile has a cadence log, giving a profile
// made before there was one its own random log id.
func (s *dealSession) cadenceProfile() (Profile, error) {
	if s.p.LogID != "" {
		return s.p, nil
	}
	suffix, err := randomHex(8)
	if err != nil {
		return Profile{}, err
	}
	s.p.LogID = "deal-cadence/" + suffix
	return s.p, saveProfile(s.p, true)
}

// tick publishes the cadence checkpoint when a tick is due, and retries any
// delivery still pending either way. The time a tick is due depends only on
// the previous tick and a random jitter, never on what the deals did.
func (s *dealSession) tick(ctx context.Context) (map[string]any, error) {
	cfg, err := s.p.dealCadence()
	if err != nil {
		return nil, err
	}
	if s.p.Checkpoint.Signing == (Secret{}) {
		return nil, inputError("the deal profile has no checkpoint key (see `deal init`)")
	}
	p, err := s.cadenceProfile()
	if err != nil {
		return nil, err
	}
	service, err := serviceID(p)
	if err != nil {
		return nil, err
	}
	last, err := s.ticks(ctx, 1)
	if err != nil {
		return nil, err
	}
	now := dealClock()
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

func (s *dealSession) cutTick(ctx context.Context, p Profile, cfg dealCadenceConfig, n int64, now time.Time) (dealTick, error) {
	// Times leaving this device are coarsened to the minute.
	at := now.UTC().Truncate(time.Minute)
	ids, err := s.dealIDs(ctx)
	if err != nil {
		return dealTick{}, err
	}
	if len(ids) >= 1<<dealCadenceDepth {
		return dealTick{}, inputError("too many deals for one cadence tree")
	}
	used := map[uint64]bool{}
	position := func() (uint64, error) {
		for {
			var b [8]byte
			if _, err := rand.Read(b[:]); err != nil {
				return 0, err
			}
			i := binary.BigEndian.Uint64(b[:]) % (1 << dealCadenceDepth)
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
		cp, err := cutCheckpointAt(ctx, localOnly(s.dp), s.t.log, at)
		err = errors.Join(err, s.t.close())
		s.t = nil
		if err != nil {
			return dealTick{}, err
		}
		sum := sha256.Sum256(cp.Bytes)
		leaf := dealTickLeaf{LogID: dealLogID(id), Size: cp.Size, CheckpointSHA256: hex.EncodeToString(sum[:]), Statement: base64.RawURLEncoding.EncodeToString(cp.Bytes)}
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
	t, err := openTarget(ctx, p, useInitialization)
	if err != nil {
		return dealTick{}, err
	}
	defer func() { err = errors.Join(err, t.close()) }()
	appended, err := t.log.Append(ctx, cll.AppendInput{Value: root, AppendedAt: at})
	if err != nil {
		return dealTick{}, err
	}
	// Padding (Evidence Layer padding records) brings the leaf count to a
	// multiple of the bucket. With the default bucket of 1 there is none:
	// each tick adds exactly one entry.
	for count := appended.Entry.Seq; count%cfg.padBucket != 0; count++ {
		id, err := dealPaddingRecordID()
		if err != nil {
			return dealTick{}, err
		}
		if _, err = t.log.Append(ctx, cll.AppendInput{Value: id, AppendedAt: at}); err != nil {
			return dealTick{}, err
		}
	}
	cp, err := cutCheckpointAt(ctx, p, t.log, at)
	if err != nil {
		return dealTick{}, err
	}
	jitter := time.Duration(0)
	if cfg.jitter > 0 {
		r, err := rand.Int(rand.Reader, big.NewInt(int64(2*cfg.jitter)+1))
		if err != nil {
			return dealTick{}, err
		}
		jitter = time.Duration(r.Int64()) - cfg.jitter
	}
	tick := dealTick{n: n, at: at, dueNext: now.UTC().Add(cfg.interval + jitter).Truncate(time.Second), entrySeq: appended.Entry.Seq, size: cp.Size, checkpoint: cp.Bytes, leaves: leaves}
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
func (s *dealSession) deliverCadence(ctx context.Context, p Profile, service string, now time.Time) (delivered []uint64, pending []map[string]any, err error) {
	t, err := openTarget(ctx, p, useCLL)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, t.close()) }()
	waiting, err := t.log.PendingWitnesses(ctx, now.Add(24*time.Hour), cll.MaxWitnesses)
	if err != nil {
		return nil, nil, err
	}
	delivered, pending = []uint64{}, []map[string]any{}
	for _, w := range waiting {
		if w.WitnessID != service {
			continue
		}
		state, err := deliverWitness(ctx, p, t.log, service, w.CheckpointSize)
		if err != nil || state.Receipt == nil || verifyWitness(p, state) != nil {
			reason, text := witnessPendingReason(state, p.Checkpoint.Endpoint)
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
	service, err := serviceID(s.p)
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
	t, err := openTarget(ctx, s.p, useCLLRead)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, t.close()) }()
	witnessed := func(tick dealTick) (cll.WitnessState, bool, error) {
		state, err := t.log.GetWitness(ctx, service, tick.size)
		if errors.Is(err, cll.ErrNotFound) {
			return state, false, nil
		}
		if err != nil {
			return state, false, err
		}
		return state, state.Receipt != nil && verifyWitness(s.p, state) == nil, nil
	}
	// This very checkpoint, in the newest tick whose receipt verifies.
	var held *dealTick
	var heldState cll.WitnessState
	for i := range ticks {
		leaf, ok := ticks[i].leaves.Deals[dealID]
		if !ok || leaf.CheckpointSHA256 != want {
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
		reason, text := witnessPendingReason(heldState, s.p.Checkpoint.Endpoint)
		rest = map[string]interface{}{"state": "pending", "tick": integer(uint64(held.n)), "reason": reason, "text": text, "cadence": s.cadenceWords()}
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
func (s *dealSession) cadenceChain(ctx context.Context, t *target, tick dealTick, dealID string, state cll.WitnessState, current *checkpoint.Record) (map[string]interface{}, error) {
	leaf := tick.leaves.Deals[dealID]
	_, path, err := tick.leaves.tree(leaf.Index)
	if err != nil {
		return nil, err
	}
	logState, err := t.log.LoadCLL(ctx)
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
		"leaf_index":  integer(uint64(*state.Receipt.LeafIndex)), "tree_size": integer(uint64(*state.Receipt.TreeSize)),
	}
	out := map[string]interface{}{
		"state": "witnessed", "extent": "all", "checkpoint_at": tick.at.UTC().Format(time.RFC3339),
		"log_id": leaf.LogID, "size": integer(leaf.Size), "salt": leaf.Salt,
		"index": integer(leaf.Index), "path": hexPath,
		"cadence": map[string]interface{}{
			"log_id": s.p.LogID, "entry_index": integer(tick.entrySeq - 1),
			"checkpoint":      map[string]interface{}{"cose": base64.RawURLEncoding.EncodeToString(tick.checkpoint)},
			"inclusion_proof": proofJSON(proof),
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
	dealState, err := s.t.log.LoadCLL(ctx)
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
	out["extent"] = "part"
	out["steps_witnessed"] = integer(mmrLeafCount(record.MMRSize))
	out["steps"] = integer(mmrLeafCount(current.MMRSize))
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
		"v": integer(p.V), "kind": p.Kind, "old_size": integer(p.OldSize), "new_size": integer(p.NewSize),
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

// A tick is published when `deal tick` runs at or after its due time, so the
// jitter in the due time only shows if something runs `deal tick` near it.
// Run it every minute (a run that is not due exits at once); a scheduler
// that can run it only every N minutes passes --wait-up-to N, and the run
// then waits for each due time inside that window and publishes on time.
// The waiting happens with the store unlocked, and every publish re-checks
// the due time under the lock.
func dealTickCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "tick", Short: "The checkpoint cadence's poll: when a tick is due, publish the profile's cadence checkpoint to its witness; a run that is not due publishes nothing (schedule it every 5 minutes with --wait-up-to 5m; deal events never publish)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		wait, _ := c.Flags().GetDuration("wait-up-to")
		if wait < 0 {
			return inputError("--wait-up-to must not be negative")
		}
		until := dealClock().Add(wait)
		published := 0
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
			now := dealClock()
			if wait == 0 || err != nil || due.After(until) {
				out["published_this_run"] = published
				return output(c, out)
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
	cfg, err := s.p.dealCadence()
	if err != nil {
		return ""
	}
	words := "every " + shortDuration(cfg.interval)
	if cfg.jitter > 0 {
		words += ", give or take " + shortDuration(cfg.jitter)
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

// shortDuration writes 1h, 90m, 5m or 30s, never "1h0m0s".
func shortDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
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
