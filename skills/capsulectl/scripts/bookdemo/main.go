// Command bookdemo is a fixture helper for run-scripted-demo.sh, never part
// of capsulectl. `close` refuses a period that has not ended, so a demo that
// runs in one sitting has nothing to close unless the book already holds
// records from an earlier day. `seed` writes such records, at a commit time
// the caller names, into the book directory `capsulectl` opens for a jsonl
// profile (<store>/book). `aac-verify` checks an Evidence Bundle with the
// neutral agent-action-capsule bundle verifier alone, so the demo's bundle
// check does not rest on capsulectl's own.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/evidencebook"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bookdemo:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bookdemo seed|aac-verify ...")
	}
	switch args[0] {
	case "seed":
		return seed(args[1:])
	case "aac-verify":
		return aacVerify(args[1:])
	}
	return fmt.Errorf("unknown mode %q", args[0])
}

func seedKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: not a 32-byte hex seed", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// seed appends one "exchange" record per EXCHANGE:REQUEST:PAYLOAD argument.
func seed(args []string) (err error) {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	store := fs.String("store", "", "the jsonl profile's store directory")
	logID := fs.String("log-id", "", "the profile's log_id (the book id)")
	operator := fs.String("operator", "", "the profile's operator")
	signing := fs.String("signing-key-file", "", "the profile's record signing seed")
	checkpoint := fs.String("checkpoint-key-file", "", "the profile's checkpoint signing seed")
	at := fs.String("at", "", "RFC 3339 commit time for every seeded record")
	if err = fs.Parse(args); err != nil {
		return err
	}
	when, err := time.Parse(time.RFC3339, *at)
	if err != nil {
		return fmt.Errorf("--at: %w", err)
	}
	recordKey, err := seedKey(*signing)
	if err != nil {
		return err
	}
	checkpointKey, err := seedKey(*checkpoint)
	if err != nil {
		return err
	}
	signer, err := evidencebook.NewEd25519Signer(recordKey)
	if err != nil {
		return err
	}
	// Same layout, log id and open order as capsulectl: the log (and its
	// lock) first.
	dir := filepath.Join(*store, "book")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	substrate, err := evidencebook.OpenCLL(filepath.Join(dir, "log.jsonl"), *logID, checkpointKey)
	if err != nil {
		return err
	}
	records, err := evidencebook.OpenFileStore(filepath.Join(dir, "records"))
	if err != nil {
		return errors.Join(err, substrate.Release())
	}
	payloads, err := evidencebook.OpenPayloadDir(filepath.Join(dir, "payloads"))
	if err != nil {
		return errors.Join(err, records.Release(), substrate.Release())
	}
	ctx := context.Background()
	book, err := evidencebook.Open(ctx, evidencebook.Config{
		BookID: *logID, Operator: *operator, Store: records, Substrate: substrate, Payloads: payloads, Signer: signer,
		Now: func() time.Time { return when },
	})
	if err != nil {
		return errors.Join(err, records.Release(), substrate.Release())
	}
	defer func() { err = errors.Join(err, book.Release()) }()
	for _, spec := range fs.Args() {
		parts := strings.SplitN(spec, ":", 3)
		if len(parts) != 3 {
			return fmt.Errorf("%q: want EXCHANGE:REQUEST:PAYLOAD", spec)
		}
		if _, err = book.Append(ctx, evidencebook.Entry{
			RecordType: "exchange", EpistemicType: evidencebook.ObservedEvent,
			Correlation: evidencebook.Correlation{ExchangeID: parts[0], RequestDigest: parts[1]},
			Payloads:    [][]byte{[]byte(parts[2])},
		}); err != nil {
			return err
		}
	}
	return nil
}

// aacVerify prints the bundle verifier's three claim statuses and fails
// unless all three pass.
func aacVerify(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: bookdemo aac-verify BUNDLE")
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var decoded any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	result := aacbundle.VerifyBundle(decoded)
	claims := map[string]aacbundle.ClaimResult{
		"graph_closure": result.GraphClosure, "interval_coverage": result.IntervalCoverage, "per_record_membership": result.PerRecordMembership,
	}
	statuses := map[string]string{}
	for name, claim := range claims {
		statuses[name] = claim.Status
	}
	if err = json.NewEncoder(os.Stdout).Encode(statuses); err != nil {
		return err
	}
	for name, claim := range claims {
		if claim.Status != "pass" {
			return fmt.Errorf("%s: %s %v", name, claim.Status, claim.Findings)
		}
	}
	return nil
}
