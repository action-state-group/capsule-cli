package cli

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
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
	DealCadenceDepth = 16
	// The public witness a new deal profile uses unless told otherwise. Its
	// key ships in this binary: a user trusts it as far as they trust the
	// release they installed.
	DealDefaultWitness    = "https://witness.agentactioncapsule.org"
	DealDefaultWitnessKey = "39bb654c9dc0afe1c0edef0deffaa69099b8518836c9ba26e0491535840f96b5"
	// The bundle extension that carries a checkpoint's witnessed chain
	// through a cadence log. Its name and fields are generic, so a neutral
	// verifier can read it without knowing what a deal is. Extension names
	// are a registered space; until this one is registered it is a private
	// kind, so it carries the x- prefix.
	DealCadenceExtension = "x-cadence-witness/v0"
)

// earlierCadenceExtensions are the names bundles carried the chain under
// before, still read: cadence-witness/v0, and x-deal-cadence-v0 (whose
// chain names its log in deal_log_id).
var earlierCadenceExtensions = []string{"cadence-witness/v0", "x-deal-cadence-v0"}

// cadenceChainOf is the cadence chain a bundle carries, under its name or
// a name bundles carried it under before.
func CadenceChainOf(b map[string]interface{}) map[string]interface{} {
	ext, _ := b["extensions"].(map[string]interface{})
	for _, name := range append([]string{DealCadenceExtension}, earlierCadenceExtensions...) {
		if chain, ok := ext[name].(map[string]interface{}); ok {
			return chain
		}
	}
	return nil
}

type DealCadenceConfig struct {
	Interval, Jitter time.Duration
	PadBucket        uint64
}

func (p Profile) DealCadence() (DealCadenceConfig, error) {
	// The checkpoint cadence by default: a tick every 5m, give or take 1m
	// (288 a day). The jitter must stay under half the interval.
	cfg := DealCadenceConfig{Interval: 5 * time.Minute, Jitter: time.Minute, PadBucket: 1}
	var err error
	if p.Cadence.Interval != "" {
		if cfg.Interval, err = time.ParseDuration(p.Cadence.Interval); err != nil {
			return cfg, InputError("cadence.interval must be a duration such as 1h")
		}
	}
	if p.Cadence.Jitter != "" {
		if cfg.Jitter, err = time.ParseDuration(p.Cadence.Jitter); err != nil {
			return cfg, InputError("cadence.jitter must be a duration such as 10m")
		}
	}
	if p.Cadence.PadBucket != 0 {
		cfg.PadBucket = p.Cadence.PadBucket
	}
	switch {
	case cfg.Interval < time.Minute:
		return cfg, InputError("cadence.interval must be at least 1m")
	case cfg.Jitter < 0:
		return cfg, InputError("cadence.jitter must be at least 0")
	case 2*cfg.Jitter >= cfg.Interval:
		// A profile that set a jitter for an earlier default and no interval
		// is told the interval it now gets.
		interval := "cadence.interval is " + ShortDuration(cfg.Interval)
		if p.Cadence.Interval == "" {
			interval = "cadence.interval is not set, so it is the default " + ShortDuration(cfg.Interval)
		}
		return cfg, InputError("cadence.jitter is " + ShortDuration(cfg.Jitter) + " but " + interval + ", and the jitter must be under half the interval: set cadence.interval (for example 1h), or a cadence.jitter under " + ShortDuration(cfg.Interval/2))
	case cfg.PadBucket > 1024:
		return cfg, InputError("cadence.pad_bucket must be at most 1024")
	}
	return cfg, nil
}

// The tree. Leaves, nodes and the empty leaf are domain-separated.
func CadenceLeaf(logID string, size uint64, checkpointSHA256, salt string) ([]byte, error) {
	jcs, err := canonical.JCS(map[string]interface{}{"checkpoint_sha256": checkpointSHA256, "log_id": logID, "salt": salt, "size": Integer(size)})
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(append([]byte{0x00}, jcs...))
	return h[:], nil
}

func CadenceNode(left, right []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(left)
	h.Write(right)
	return h.Sum(nil)
}

// cadenceFold recomputes a root from a leaf, its position and its path.
func cadenceFold(leaf []byte, index uint64, path [][]byte) []byte {
	h := leaf
	for level, sibling := range path {
		if (index>>level)&1 == 0 {
			h = CadenceNode(h, sibling)
		} else {
			h = CadenceNode(sibling, h)
		}
	}
	return h
}

// shortDuration writes 1h, 90m, 5m or 30s, never "1h0m0s".
func ShortDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}
