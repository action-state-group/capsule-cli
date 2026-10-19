package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/checkpointed-local-log/go/checkpoint"
	"github.com/spf13/cobra"
)

// dealCheckpointCommands groups the deal log's checkpoint reads.
func dealCheckpointCommands() *cobra.Command {
	group := &cobra.Command{Use: "checkpoint", Short: "Read a deal log's checkpoints (read-only)"}
	group.AddCommand(dealCheckpointStatusCommand())
	return group
}

func dealCheckpointStatusCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "status", Short: "The deal log's latest checkpoint, the log head it covers, and its stored witness state; read-only: never seals, cuts a checkpoint or contacts the witness", Args: cli.NoArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		p, err := cli.Selected(c)
		if err != nil {
			return err
		}
		dealID, _ := c.Flags().GetString("deal")
		if dealID == "" {
			return cli.InputError("--deal is required: a deal id as printed by `deal open`")
		}
		if !cli.DealIDPattern.MatchString(dealID) {
			return cli.InputError("--deal must be a deal id as printed by `deal open`: deal- followed by 16 hex characters")
		}
		s, err := openDealSessionRead(c.Context(), p, dealID)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, s.close()) }()
		out, err := s.checkpointStatus(c.Context(), dealID)
		if err != nil {
			return err
		}
		return cli.Output(c, out)
	}}
	cmd.Flags().String("deal", "", "Deal id")
	return cmd
}

// openDealSessionRead opens the deal store and a known deal's own log for
// reading only. It writes nothing, not even what openDealSession provisions
// (the index tables, the store secret, the lock file), so it serves a
// read-only profile too: every query it makes is a SELECT. It uses the
// store's own handle (sqliteConnection), which closes last, so SQLite's -wal
// and -shm files are left as the read found them (a read-only handle closing
// last would leave them behind). When the store's lock file exists it holds a
// shared lock on it, so the read never sees a step half sealed.
func openDealSessionRead(ctx context.Context, p cli.Profile, dealID string) (_ *dealSession, err error) {
	s, err := openDealStoreRead(p)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	steps, err := s.index.stepCount(ctx, dealID)
	if err != nil && !strings.Contains(err.Error(), "no such table") {
		return nil, err
	}
	if steps == 0 {
		return nil, cli.InputError("unknown deal: " + dealID)
	}
	s.dp = p
	s.dp.LogID = cli.DealLogID(dealID)
	if s.t, err = cli.OpenTarget(ctx, s.dp, cli.UseCLLRead); err != nil {
		return nil, err
	}
	return s, nil
}

// openDealStoreRead is openDealSessionRead without a deal: the store's
// handle under the shared lock, and no log opened.
func openDealStoreRead(p cli.Profile) (_ *dealSession, err error) {
	if p.Type != "sqlite" {
		return nil, cli.InputError("deal commands need a sqlite profile; create one with `deal init`")
	}
	dbPath, err := filepath.Abs(p.Connection.Database)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(dbPath); err != nil {
		return nil, cli.InputError("deal store does not exist; run `deal init`")
	}
	unlock, err := lockDealStoreShared(dbPath)
	if err != nil {
		return nil, err
	}
	s := &dealSession{p: p, unlock: unlock}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	if s.db, _, err = cli.SqliteConnection(p); err != nil {
		return nil, err
	}
	s.index = sqliteDealIndex{s.db}
	return s, nil
}

// checkpointStatus is the deal log's latest checkpoint and where it stands
// with the witness, from what the store holds. A deal with no checkpoint yet
// has checkpoint and witness null.
func (s *dealSession) checkpointStatus(ctx context.Context, dealID string) (map[string]any, error) {
	state, err := s.t.Log.LoadCLL(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := s.logEntries(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"deal_id": dealID, "log_id": s.dp.LogID, "log_entries": len(entries), "checkpoint": nil, "witness": nil}
	if state.Checkpoint == nil {
		return out, nil
	}
	statement := state.Checkpoint.Bytes
	record, err := checkpoint.ParseRecord(statement)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(statement)
	out["checkpoint"] = map[string]any{
		"id": hex.EncodeToString(sum[:]), "at": record.Timestamp.UTC().Format(time.RFC3339),
		"mmr_size": record.MMRSize, "entries": mmrLeafCount(record.MMRSize), "root": record.Root,
	}
	cadence, err := s.dealWitnessState(ctx, dealID, statement)
	if err != nil {
		return nil, err
	}
	out["witness"] = witnessStatus(cadence)
	return out, nil
}

// witnessStatus is the summary of a deal checkpoint's witness state
// (dealWitnessState) without the proofs: the state, and for a witnessed one
// how much of the log the witness covers and the tick that carried it.
func witnessStatus(cadence map[string]any) map[string]any {
	out := map[string]any{"state": cadence["state"]}
	for _, k := range []string{"reason", "text", "due"} {
		if v, ok := cadence[k]; ok {
			out[k] = v
		}
	}
	if v, ok := cadence["tick"]; ok {
		out["tick"] = v
	}
	if cadence["state"] == "witnessed" {
		out["extent"] = cadence["extent"]
		out["witnessed_mmr_size"] = cadence["size"]
		out["tick_at"] = cadence["checkpoint_at"]
		if rest, ok := cadence["rest"].(map[string]any); ok {
			out["current"] = witnessStatus(rest)
		}
	}
	return out
}
