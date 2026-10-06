package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	emit "github.com/action-state-group/capsule-emit-go"
	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/action-state-group/cll-go/checkpoint"
	"github.com/action-state-group/cll-go/cll"
	"github.com/spf13/cobra"
)

// `capsulectl deal` is the check an agent's host calls before the agent pays,
// books, signs or shares on a user's behalf. It is advisory: it holds an
// action only where the host runs it from a pre-action hook; otherwise a
// skipped check shows in the report. Every step is sealed as a
// Capsule in the profile's SQLite store and appended to its checkpointed log
// before anything is compared or shown:
//
//	snapshot -> seal -> diff -> show -> approval -> seal approval -> act
//
// The seed that signs these Capsules lives on the same machine as the agent,
// so the trail is tamper-evident (a later edit or deletion is detectable), not
// non-repudiation (it does not prove the user, rather than the machine, said
// something).

// dealClock is the time source for sealed timestamps; tests replace it.
var dealClock = func() time.Time { return time.Now().UTC() }

// The local store, in the profile's SQLite file. It never leaves the device:
// deal_steps.local holds each step's raw values and commitment nonces,
// deal_disclosures each shared copy's disclosure record,
// deal_keys each deal's key, and deal_store the store secret the keys derive
// from. What is sealed is only the x-deal-v0 record derived from them.
var dealIndexSchema = []string{
	`CREATE TABLE IF NOT EXISTS deal_steps (
	deal_id TEXT NOT NULL,
	n INTEGER NOT NULL,
	kind TEXT NOT NULL,
	capsule_id TEXT NOT NULL,
	cll_sequence INTEGER NOT NULL,
	record_digest TEXT NOT NULL,
	local TEXT NOT NULL,
	PRIMARY KEY (deal_id, n)
)`,
	`CREATE TABLE IF NOT EXISTS deal_disclosures (
	deal_id TEXT NOT NULL,
	n INTEGER NOT NULL,
	record_digest TEXT NOT NULL,
	cll_sequence INTEGER NOT NULL,
	record TEXT NOT NULL,
	PRIMARY KEY (deal_id, n)
)`,
	`CREATE TABLE IF NOT EXISTS deal_keys (deal_id TEXT PRIMARY KEY, deal_key BLOB NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS deal_store (id INTEGER PRIMARY KEY CHECK (id = 1), secret BLOB NOT NULL)`,
	dealCadenceSchema,
}

// dealSession holds the write lock, the index handle and the signing key for
// one deal command, and the store opened for the one deal it acts on.
type dealSession struct {
	p      Profile
	dp     Profile // p with the deal's own log selected
	db     *sql.DB
	t      *target
	key    ed25519.PrivateKey
	keys   []ed25519.PublicKey
	secret []byte // the store secret
	dkey   []byte // the current deal's key
	unlock func() error
}

var dealIDPattern = regexp.MustCompile(`^deal-[0-9a-f]{16}$`)

// dealLogID names a deal's own checkpointed log. Each deal is its own log in
// the profile's SQLite file, so a report proves exactly one deal's steps and
// discloses nothing about any other deal.
func dealLogID(dealID string) string { return "deal/" + dealID }

func openDealSession(ctx context.Context, p Profile) (_ *dealSession, err error) {
	if p.Type != "sqlite" {
		return nil, inputError("deal commands need a sqlite profile; create one with `deal init`")
	}
	if p.ReadOnly {
		return nil, ErrReadOnlyCLL
	}
	key, err := privateKey(p.Signing)
	if err != nil {
		return nil, err
	}
	if err = requirePublisherKey(p, key); err != nil {
		return nil, err
	}
	keys, err := parseKeys(p.TrustedKeys)
	if err != nil {
		return nil, err
	}
	dbPath, err := filepath.Abs(p.Connection.Database)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(dbPath); err != nil {
		return nil, inputError("deal store does not exist; run `deal init`")
	}
	unlock, err := lockDealStore(dbPath)
	if err != nil {
		return nil, err
	}
	s := &dealSession{p: p, key: key, keys: keys, unlock: unlock}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	if s.db, _, err = sqliteConnection(p); err != nil {
		return nil, err
	}
	for _, stmt := range dealIndexSchema {
		if _, err = s.db.ExecContext(ctx, stmt); err != nil {
			return nil, err
		}
	}
	// The store secret: 32 CSPRNG bytes, made once, never written to a record.
	err = s.db.QueryRowContext(ctx, `SELECT secret FROM deal_store WHERE id=1`).Scan(&s.secret)
	if errors.Is(err, sql.ErrNoRows) {
		s.secret = make([]byte, 32)
		if _, err = rand.Read(s.secret); err != nil {
			return nil, err
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO deal_store (id, secret) VALUES (1, ?)`, s.secret)
	}
	if err != nil {
		return nil, err
	}
	if len(s.secret) != 32 {
		return nil, ErrConflict
	}
	return s, nil
}

// useDeal opens the store with the deal's own log, creating the log for a new
// deal. An existing deal must already have indexed steps.
func (s *dealSession) useDeal(ctx context.Context, dealID string, create bool) error {
	if !dealIDPattern.MatchString(dealID) {
		return inputError("--deal must be a deal id as printed by `deal open`: deal- followed by 16 hex characters")
	}
	if create {
		// The deal key is stored at open, so rotating the store secret later
		// cannot break a deal in progress.
		s.dkey = dealKeyFor(s.secret, dealID)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO deal_keys (deal_id, deal_key) VALUES (?, ?)`, dealID, s.dkey); err != nil {
			return err
		}
	} else {
		err := s.db.QueryRowContext(ctx, `SELECT deal_key FROM deal_keys WHERE deal_id=?`, dealID).Scan(&s.dkey)
		if errors.Is(err, sql.ErrNoRows) {
			return inputError("unknown deal: " + dealID)
		}
		if err != nil {
			return err
		}
	}
	s.dp = s.p
	s.dp.LogID = dealLogID(dealID)
	use := usePublication
	if create {
		use = useInitialization
	}
	t, err := openTarget(ctx, s.dp, use)
	if err != nil {
		return err
	}
	s.t = t
	return nil
}

func (s *dealSession) close() error {
	var e error
	if s.t != nil {
		e = s.t.close()
	}
	if s.db != nil {
		e = errors.Join(e, s.db.Close())
	}
	return errors.Join(e, s.unlock())
}

// logEntries reads the deal's whole log, in order.
func (s *dealSession) logEntries(ctx context.Context) ([]cll.Entry, error) {
	var out []cll.Entry
	for after := uint64(0); ; {
		batch, err := s.t.log.ScanEntries(ctx, after, cll.MaxScanLimit)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		out = append(out, batch...)
		after = batch[len(batch)-1].Seq
	}
}

// load reads a deal's steps back and re-verifies each one against the deal's
// own log: the log must hold exactly one entry per indexed step, in order,
// so a lost or deleted step is a conflict, never a silently shorter deal.
// Then, per step: the Capsule signature and trust, the payload binding, the
// local step re-derived into exactly the sealed record bytes, and the prev
// chain of record digests. A last step that was indexed but never reached
// the log (a crash between the two writes) is recovered by publishing it
// from the stored step; its Capsule must come out identical.
func (s *dealSession) load(ctx context.Context, dealID string) (_ []sealedEvent, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n, kind, capsule_id, cll_sequence, record_digest, local FROM deal_steps WHERE deal_id=? ORDER BY n`, dealID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	type row struct {
		n                    int64
		kind, id, digest, lo string
		seq                  uint64
	}
	var index []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.n, &r.kind, &r.id, &r.seq, &r.digest, &r.lo); err != nil {
			return nil, err
		}
		index = append(index, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	entries, err := s.logEntries(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case len(entries) > len(index), len(index) > len(entries)+1:
		return nil, ErrConflict
	case len(index) == 0:
		return nil, inputError("unknown deal: " + dealID)
	}
	events := make([]sealedEvent, 0, len(index))
	for i, r := range index {
		var ev dealEvent
		if err = decodeJSON([]byte(r.lo), &ev); err != nil {
			return nil, ErrConflict
		}
		prev := ""
		if i > 0 {
			prev = events[i-1].Digest
		}
		if ev.DealID != dealID || ev.N != int64(i+1) || ev.N != r.n || ev.Kind != r.kind || ev.Prev != prev {
			return nil, ErrConflict
		}
		if i == len(entries) {
			se, err := s.recoverStep(ctx, events, ev, r.id)
			if err != nil {
				return nil, err
			}
			events = append(events, se)
			continue
		}
		if hex.EncodeToString(entries[i].Value) != r.id {
			return nil, ErrConflict
		}
		record, err := s.t.artifacts.Get(ctx, r.id)
		if err != nil {
			return nil, err
		}
		checks, err := artifact.Verify(record, s.keys)
		if err != nil {
			return nil, err
		}
		var content []byte
		for _, a := range record.Artifacts {
			if a.Binding == artifact.PayloadDigest && a.State == artifact.Present {
				content = a.Content
			}
		}
		if content == nil || !checks["payload"].Verified {
			return nil, ErrConflict
		}
		payload, digest, err := encodeDealRecord(ev, events, s.dkey)
		if err != nil || !bytes.Equal(payload, content) || digest != r.digest {
			return nil, ErrConflict
		}
		events = append(events, sealedEvent{CapsuleID: r.id, Digest: digest, Sequence: entries[i].Seq, Event: ev})
	}
	return events, nil
}

// stepRequest is the frozen seal request for a step: rebuilt from the stored
// step it gives the same Capsule, which is what makes recovery possible.
func (s *dealSession) stepRequest(events []sealedEvent, ev dealEvent) (Request, string, error) {
	payload, digest, err := encodeDealRecord(ev, events, s.dkey)
	if err != nil {
		return Request{}, "", err
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", ev.At)
	if err != nil {
		return Request{}, "", ErrConflict
	}
	request := Request{
		Version: "capsule-seal-request/v1",
		Capsule: emit.Input{ActionID: fmt.Sprintf("%s/%d", ev.DealID, ev.N), ActionType: emit.ActionTypeFYI, Operator: s.p.Name, Developer: "capsulectl-deal", Timestamp: at.UTC()},
		Payload: payload,
	}
	// Each step's Capsule follows the one before it (ordering only), so the
	// deal is a citation chain any Evidence Bundle verifier can close. A
	// record sealed after the close instead CONFIRMS the close (registered
	// chain.relation): `follows` is ordering only, and verifiers must not
	// read it as confirming anything.
	switch {
	case ev.Confirms != "":
		request.Capsule.Chain = &emit.Chain{ParentCapsuleID: ev.Confirms, Relation: "confirms"}
	case len(events) > 0:
		request.Capsule.Chain = &emit.Chain{ParentCapsuleID: events[len(events)-1].CapsuleID, Relation: "follows"}
	}
	return request, digest, nil
}

// prepareStep fixes a step's time, chain and nonces, derives its record
// (refusing one that fails the profile schema or would carry a raw
// identifier), and writes its index row with the local step BEFORE anything
// reaches the log, so a crash can never leave a logged step with no local
// record of it.
func (s *dealSession) prepareStep(ctx context.Context, dealID string, events []sealedEvent, ev dealEvent) (Request, sealedEvent, error) {
	now := dealClock().UTC().Truncate(time.Second)
	ev.DealID = dealID
	ev.N = int64(len(events) + 1)
	ev.At = now.Format("2006-01-02T15:04:05Z")
	ev.Prev = ""
	if len(events) > 0 {
		ev.Prev = events[len(events)-1].Digest
	}
	ev.Producer = currentProducer()
	ev.Nonces = map[string]string{}
	for name := range dealTexts(ev) {
		nonce := make([]byte, 32)
		if _, err := rand.Read(nonce); err != nil {
			return Request{}, sealedEvent{}, err
		}
		ev.Nonces[name] = hex.EncodeToString(nonce)
	}
	request, digest, err := s.stepRequest(events, ev)
	if err != nil {
		return Request{}, sealedEvent{}, err
	}
	record, err := seal(request, s.key)
	if err != nil {
		return Request{}, sealedEvent{}, err
	}
	local, err := json.Marshal(ev)
	if err != nil {
		return Request{}, sealedEvent{}, err
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO deal_steps (deal_id, n, kind, capsule_id, cll_sequence, record_digest, local) VALUES (?,?,?,?,0,?,?)`,
		dealID, ev.N, ev.Kind, record.CapsuleID, digest, string(local)); err != nil {
		return Request{}, sealedEvent{}, err
	}
	return request, sealedEvent{CapsuleID: record.CapsuleID, Digest: digest, Event: ev}, nil
}

// publishStep persists and appends a prepared step and records its log
// position.
func (s *dealSession) publishStep(ctx context.Context, request Request, se sealedEvent) (sealedEvent, error) {
	pub, err := s.t.publish(ctx, request, s.key)
	if err != nil {
		return sealedEvent{}, err
	}
	if pub.CapsuleID != se.CapsuleID {
		return sealedEvent{}, ErrConflict
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE deal_steps SET cll_sequence=? WHERE deal_id=? AND n=?`, pub.Sequence, se.Event.DealID, se.Event.N); err != nil {
		return sealedEvent{}, err
	}
	se.Sequence = pub.Sequence
	return se, nil
}

func (s *dealSession) recoverStep(ctx context.Context, events []sealedEvent, ev dealEvent, capsuleID string) (sealedEvent, error) {
	request, digest, err := s.stepRequest(events, ev)
	if err != nil {
		return sealedEvent{}, ErrConflict
	}
	return s.publishStep(ctx, request, sealedEvent{CapsuleID: capsuleID, Digest: digest, Event: ev})
}

// seal prepares and publishes one step. Any failure is returned: a step
// that is not sealed was not checked. If the step never reached
// the log, its index row is withdrawn; if it did, the next read recovers it.
func (s *dealSession) seal(ctx context.Context, dealID string, events []sealedEvent, ev dealEvent) (sealedEvent, error) {
	request, se, err := s.prepareStep(ctx, dealID, events, ev)
	if err != nil {
		return sealedEvent{}, err
	}
	published, err := s.publishStep(ctx, request, se)
	if err != nil {
		if entries, scanErr := s.logEntries(ctx); scanErr == nil && int64(len(entries)) < se.Event.N {
			_, delErr := s.db.ExecContext(ctx, `DELETE FROM deal_steps WHERE deal_id=? AND n=?`, dealID, se.Event.N)
			err = errors.Join(err, delErr)
		}
		return sealedEvent{}, err
	}
	return published, nil
}

// milestone cuts a signed checkpoint of the deal's own log at a deal
// milestone (baseline, approval, close) when the profile has a checkpoint
// key. It never contacts the witness: a deal event publishing would tell the
// witness when deals happen. The deal's checkpoint reaches the witness inside
// the next cadence tick (`deal tick`), so its witness state is "scheduled".
func (s *dealSession) milestone(ctx context.Context) (map[string]any, error) {
	if s.p.Checkpoint.Signing == (Secret{}) {
		return map[string]any{"state": "not_configured"}, nil
	}
	cp, err := cutCheckpoint(ctx, localOnly(s.dp), s.t.log)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"state": "signed", "checkpoint": cp.Size, "witness": "not_configured"}
	service, err := serviceID(s.dp)
	if err != nil {
		return nil, err
	}
	if service != "" {
		out["witness"] = "scheduled"
	}
	return out, nil
}

func dealCommands() *cobra.Command {
	deal := &cobra.Command{Use: "deal", Short: "Seal a deal's baseline and check every point of no return against it"}
	deal.AddCommand(dealInitCommand(), dealOpenCommand(), dealNoteCommand(), dealCheckCommand(), dealCloseCommand(), dealReportCommand(), dealCountersignCommand(), dealExportCommand(), dealReconcileCommand(), dealTickCommand(), dealVerifyEmailCommand(), dealDeadlinesCommand())
	return deal
}

// runDeal selects the profile, opens a locked session, loads the named deal
// (when dealID is set) and refuses further steps on a finally closed deal.
func runDeal(c *cobra.Command, needDeal bool, fn func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error) (err error) {
	p, err := selected(c)
	if err != nil {
		return err
	}
	dealID := ""
	if needDeal {
		dealID, _ = c.Flags().GetString("deal")
		if dealID == "" {
			return inputError("--deal is required: a deal id as printed by `deal open`")
		}
	}
	ctx := c.Context()
	s, err := openDealSession(ctx, p)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	var events []sealedEvent
	if needDeal {
		if err = s.useDeal(ctx, dealID, false); err != nil {
			return err
		}
		if events, err = s.load(ctx, dealID); err != nil {
			return err
		}
	}
	return fn(ctx, s, dealID, events)
}

func stepOutput(dealID string, se sealedEvent) map[string]any {
	return map[string]any{"deal_id": dealID, "step": se.Event.N, "kind": se.Event.Kind, "capsule_id": se.CapsuleID, "sequence": se.Sequence}
}

func writeSeed(path string) (ed25519.PrivateKey, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, inputError("cannot create key file (it must not already exist): " + path)
	}
	_, werr := f.WriteString(hex.EncodeToString(private.Seed()))
	if err = errors.Join(werr, f.Sync(), f.Close()); err != nil {
		return nil, err
	}
	return private, nil
}

func dealInitCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "init", Short: "Create a local deal profile: SQLite store, 0600 signing and checkpoint seeds", Args: noArgs, RunE: func(c *cobra.Command, _ []string) (err error) {
		name, _ := c.Flags().GetString("profile")
		dir, _ := c.Flags().GetString("dir")
		if name == "" || dir == "" {
			return inputError("--profile and --dir are required")
		}
		if dir, err = filepath.Abs(dir); err != nil {
			return err
		}
		if path, e := profilePath(name); e != nil {
			return e
		} else if _, e = os.Lstat(path); e == nil {
			return inputError("profile already exists: " + name)
		}
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		signing, err := writeSeed(filepath.Join(dir, "signing.seed"))
		if err != nil {
			return err
		}
		cpKey, err := writeSeed(filepath.Join(dir, "checkpoint.seed"))
		if err != nil {
			return err
		}
		signer, err := checkpoint.NewEd25519Signer(cpKey)
		if err != nil {
			return err
		}
		public := hex.EncodeToString(signing.Public().(ed25519.PublicKey))
		// Each deal gets its own log (see dealLogID), never published. The
		// profile's log is the cadence log: the only one the witness sees.
		suffix, err := randomHex(8)
		if err != nil {
			return err
		}
		p := Profile{Name: name, Type: "sqlite", Namespace: "deal", LogID: "deal-cadence/" + suffix}
		p.Connection.Database = filepath.Join(dir, "deal.db")
		p.Signing.File = filepath.Join(dir, "signing.seed")
		p.TrustedKeys = []string{public}
		p.Checkpoint.Signing.File = filepath.Join(dir, "checkpoint.seed")
		p.Checkpoint.TrustedKeys = []string{signer.KeyID()}
		witness := "not_configured"
		if noWitness, _ := c.Flags().GetBool("no-witness"); !noWitness {
			p.Checkpoint.Endpoint, p.Checkpoint.PublicKey = dealDefaultWitness, dealDefaultWitnessKey
			witness = "scheduled"
		}
		if err = saveProfile(p, false); err != nil {
			return err
		}
		t, err := openTarget(c.Context(), p, useInitialization)
		if err != nil {
			return err
		}
		if err = t.close(); err != nil {
			return err
		}
		if err = os.Chmod(p.Connection.Database, 0o600); err != nil {
			return err
		}
		out := map[string]any{
			"profile": name, "store": p.Connection.Database, "public_key": public, "witness": witness, "cadence_log": p.LogID,
			"guarantee": "tamper-evident against ourselves and the agent, not non-repudiation: the signing seed is on this machine",
		}
		if witness == "scheduled" {
			out["witness_endpoint"], out["witness_sees"] = dealDefaultWitness, dealWitnessSees
			out["next"] = "run `capsulectl --profile " + name + " deal tick` every minute from a timer (a run that is not due exits at once; every minute keeps the cadence's jitter: see `deal tick --help` for schedulers that cannot run every minute)"
		}
		return output(c, out)
	}}
	cmd.Flags().String("dir", "", "Directory for the store and seeds (created 0700)")
	cmd.Flags().Bool("no-witness", false, "Do not configure the default public witness; deals are sealed on this device only")
	return cmd
}

func dealOpenCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "open", Short: "Seal the baseline: the user's verbatim words, who, terms, claims and recourse", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := readInput(path)
		if err != nil {
			return err
		}
		var o dealOpen
		if err = decodeJSONAs("--input", raw, &o); err != nil {
			return err
		}
		if err = o.validate(); err != nil {
			return err
		}
		if err = normalizeOpen(&o); err != nil {
			return err
		}
		o.Skill = nil
		skillPath, _ := c.Flags().GetString("skill")
		if skillPath == "" {
			skillPath = os.Getenv(dealSkillEnv)
		}
		if skillPath != "" {
			if o.Skill, err = readDealSkill(skillPath); err != nil {
				return err
			}
		}
		switch {
		case o.ExpectCloseBy == "":
			o.ExpectCloseBy = defaultExpectClose(o.Type, dealClock())
		case !validDate(o.ExpectCloseBy):
			return inputError("expect_close_by must be a date, YYYY-MM-DD")
		}
		return runDeal(c, false, func(ctx context.Context, s *dealSession, _ string, _ []sealedEvent) error {
			id := make([]byte, 8)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			dealID := "deal-" + hex.EncodeToString(id)
			if err := s.useDeal(ctx, dealID, true); err != nil {
				return err
			}
			se, err := s.seal(ctx, dealID, nil, dealEvent{Kind: "open", Open: &o})
			if err != nil {
				return err
			}
			cp, err := s.milestone(ctx)
			if err != nil {
				return err
			}
			out := stepOutput(dealID, se)
			out["demo"] = o.Demo
			out["points_of_no_return"] = dealPointsOfNoReturn[o.Type]
			out["checkpoint"] = cp
			out["remote_warm"] = dealRemoteWarm(ctx)
			if o.Skill != nil {
				out["skill"] = map[string]any{"skill_md_digest": o.Skill.Digest, "other_copies": o.Skill.OtherCopies, "others": o.Skill.Others}
			}
			return output(c, out)
		})
	}}
	cmd.Flags().String("input", "", "Baseline JSON: type, intent, who, terms, claims, recourse")
	cmd.Flags().String("skill", "", "The SKILL.md the agent is following (or $"+dealSkillEnv+"): its digest is sealed in the baseline")
	return cmd
}

// dealActInput is what an agent reports after acting. Authorization fields are
// computed by the wrapper and cannot be supplied.
type dealActInput struct {
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
	AmountMinor *int64 `json:"amount_minor,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Payee       string `json:"payee,omitempty"`
	Rail        string `json:"rail,omitempty"`
	Reference   string `json:"reference,omitempty"`
}

func dealNoteCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "note", Short: "Seal a message, claim, evidence, detail change, the user's answer to a check, an action taken, or something the agent told someone (disclosure)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		kind, _ := c.Flags().GetString("kind")
		emailPath, _ := c.Flags().GetString("email")
		keyPath, _ := c.Flags().GetString("key-record")
		dmarcPath, _ := c.Flags().GetString("dmarc-record")
		ev := dealEvent{Kind: kind}
		var act dealActInput
		if (emailPath != "" || keyPath != "" || dmarcPath != "") && kind != "evidence" {
			return inputError("--email, --key-record and --dmarc-record go with --kind evidence")
		}
		if keyPath != "" && emailPath == "" {
			return inputError("--key-record goes with --email: the key record is read for that email's DKIM signature")
		}
		path, _ := c.Flags().GetString("input")
		switch {
		case kind == "evidence" && emailPath != "" && path == "":
			ev.Evidence = &dealEvidence{About: "merchant confirmation email", Source: "merchant_email"}
		case slices.Contains([]string{"message", "claim", "evidence", "change", "intent", "act", "disclosure"}, kind):
			raw, err := readInput(path)
			if err != nil {
				return err
			}
			var target any
			switch kind {
			case "message":
				ev.Message = &dealMessage{}
				target = ev.Message
			case "claim":
				ev.Claim = &dealClaim{}
				target = ev.Claim
			case "evidence":
				ev.Evidence = &dealEvidence{}
				target = ev.Evidence
			case "change":
				ev.Change = &dealChange{}
				target = ev.Change
			case "intent":
				ev.Intent = &dealIntent{}
				target = ev.Intent
			case "act":
				target = &act
			case "disclosure":
				ev.Disclosure = &dealDisclosure{}
				target = ev.Disclosure
			}
			if err = decodeJSONAs("--input", raw, target); err != nil {
				return err
			}
		case kind == "approval":
			check, _ := c.Flags().GetString("check")
			choice, _ := c.Flags().GetString("choice")
			said, _ := c.Flags().GetString("said")
			if check == "" || choice == "" {
				return inputError("approval needs --check and --choice")
			}
			ev.Approval = &dealApproval{Check: check, Choice: choice, Approver: "user", Said: said}
		default:
			return inputError("--kind must be message, claim, evidence, change, intent, approval, act or disclosure")
		}
		if emailPath != "" {
			// Captured before the deal is locked: the key records are read
			// from DNS now, as close to receipt as the agent can make it.
			if err := attachEmail(ev.Evidence, emailPath, keyPath, dmarcPath); err != nil {
				return err
			}
		}
		if err := normalizeNote(&ev); err != nil {
			return err
		}
		switch {
		case ev.Message != nil && (!slices.Contains([]string{"counterparty", "user", "agent"}, ev.Message.From) || strings.TrimSpace(ev.Message.Text) == ""):
			return inputError("message needs from (counterparty, user or agent) and text")
		case ev.Claim != nil:
			if err := ev.Claim.validate(); err != nil {
				return err
			}
		case ev.Evidence != nil && (ev.Evidence.About == "" || ev.Evidence.Source == ""):
			return inputError("evidence needs what it is about and its source")
		case ev.Change != nil && (ev.Change.Source == "" || (ev.Change.Who == nil && ev.Change.Terms == nil && ev.Change.Recourse == nil)):
			return inputError("change needs its source and at least one of who, terms or recourse")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			if i := finalClose(events); i >= 0 {
				// A closed deal takes later evidence only, linked to the close
				// (it confirms the close); every other step needs a new deal.
				if kind != "evidence" {
					return inputError("this deal is closed; it takes later evidence only (`deal note --kind evidence`, linked to the close), and any other step needs a new deal (`deal open`)")
				}
				if ev.Evidence.Obligation != nil {
					return inputError("this deal is closed; a new cancel-by date belongs on a new deal (`deal open`)")
				}
				ev.Confirms = events[i].CapsuleID
			}
			if ev.Evidence != nil && (ev.Evidence.ResolvesStep != 0 || ev.Evidence.Resolves != "") {
				target := ev.Evidence.Resolves
				for _, se := range events {
					if se.Event.N == ev.Evidence.ResolvesStep || (target != "" && se.CapsuleID == target) {
						target = se.CapsuleID
						if se.Event.Evidence == nil || se.Event.Evidence.Obligation == nil {
							return inputError("resolves names a step that holds no cancel-by date")
						}
					}
				}
				if target == "" {
					return inputError("resolves names no step of this deal")
				}
				ev.Evidence.Resolves, ev.Evidence.ResolvesStep = target, 0
			}
			open := events[0].Event.Open
			switch kind {
			case "message":
				if ev.Message.Channel == "" {
					ev.Message.Channel = open.Channel
				}
			case "intent":
				for _, a := range ev.Intent.Allowed {
					if !slices.Contains(dealPointsOfNoReturn[open.Type], a) {
						return inputError("intent.allowed names an action that is not a point of no return for this deal type: " + a)
					}
				}
			case "approval":
				if err := judgeApproval(events, ev.Approval); err != nil {
					return err
				}
			case "act":
				if !slices.Contains(dealPointsOfNoReturn[events[0].Event.Open.Type], act.Action) {
					return inputError("act.action is not a point of no return for this deal type")
				}
				ev.Act = &dealAct{Action: act.Action, Description: act.Description, AmountMinor: act.AmountMinor, Currency: act.Currency, Payee: act.Payee, Rail: act.Rail, Reference: act.Reference}
				ev.Act.AuthorizedBy, ev.Act.Reason, ev.Act.Rule = authorizeAct(events, *ev.Act)
				ev.Act.Unchecked = ev.Act.AuthorizedBy == ""
				ev.Act.Direction, ev.Act.Reverses = actDirection(events, *ev.Act, events[0].Event.Open.Terms.Currency)
			case "disclosure":
				// Covered by the same rules as an action: a check of
				// share_contact (or share_credentials) answered with proceed,
				// and no approval covers two steps.
				d := ev.Disclosure
				if !slices.Contains(dealPointsOfNoReturn[open.Type], d.action()) {
					return inputError("this deal type has no " + d.action() + " point of no return")
				}
				d.AuthorizedBy, d.Reason, d.Rule = authorizeAct(events, dealAct{Action: d.action()})
				// An approval covers a telling only to the party its check was
				// about: approving the address for the seller is not approving
				// it for a courier.
				if d.AuthorizedBy != "" && !sameRecipient(events, d.AuthorizedBy, *d, *open) {
					d.AuthorizedBy, d.Reason, d.Rule = "", "that approval was for a telling to someone else", "approval_for_another_party"
				}
				// Noting runs when the form is filled, before it is sent: a
				// class going to this recipient for the first time that no
				// approved check named is held, and nothing is sealed. Leaving
				// a class out of the check is no way around its first telling.
				memory, err := s.counterpartyMemory(ctx, dealID)
				if err != nil {
					return err
				}
				keys := disclosureRecipientKeys(*d, *open)
				if first := firstTellings(events, d.AuthorizedBy, d.Fields, memory, keys); len(first) > 0 {
					name := recipientName(open.Who)
					if d.Who != nil && d.To == "other" {
						name = recipientName(*d.Who)
					}
					words := make([]string, len(first))
					for i, c := range first {
						words[i] = classWord(c)
					}
					if err := output(c, map[string]any{
						"deal_id": dealID, "proceed": false, "verdict": "pause", "held": first,
						"reason": "First time telling " + name + " your " + strings.Join(words, ", ") + ", and no check the user approved named it",
						"next":   `do not send it; run deal check with "disclosing" naming ` + strings.Join(first, ", ") + `, show its card, and note this again only once the user approves`,
					}); err != nil {
						return err
					}
					return ErrPaused
				}
				if class := uncheckedClass(events, d.AuthorizedBy, d.Fields); d.AuthorizedBy != "" && class != "" {
					d.AuthorizedBy, d.Reason, d.Rule = "", "the check did not name your "+classWord(class), "class_not_checked"
				}
			}
			se, err := s.seal(ctx, dealID, events, ev)
			if err != nil {
				return err
			}
			out := stepOutput(dealID, se)
			switch kind {
			case "intent":
				// A note that asks for more than the limits in force is a
				// proposal: sealed as written, applied only on the user's
				// confirmation.
				state, err := foldDeal(events)
				if err != nil {
					return err
				}
				if proposed, more := proposedLimits(state.intent.limits(), *ev.Intent); more {
					out["proposed"] = proposed
					out["in_force"] = state.intent.limits()
					out["next"] = "this note asks for a higher limit or a new action: it is recorded as a proposal and not applied. " +
						"Ask the user; only on their explicit yes, seal deal note --kind approval --check " + se.CapsuleID +
						" --choice confirm_limits --said \"<their words>\""
				}
			case "approval":
				out["proceed"] = ev.Approval.Proceed && ev.Approval.Reason == ""
				out["reason"] = ev.Approval.Reason
				if ev.Approval.Limits != nil {
					out["limits"] = ev.Approval.Limits
				}
				cp, err := s.milestone(ctx)
				if err != nil {
					return err
				}
				out["checkpoint"] = cp
			case "act":
				out["unchecked"] = ev.Act.Unchecked
				out["authorized_by"] = ev.Act.AuthorizedBy
				out["reason"] = ev.Act.Reason
			case "disclosure":
				d := ev.Disclosure
				out["approved"] = d.AuthorizedBy != ""
				out["authorized_by"] = d.AuthorizedBy
				out["reason"] = d.Reason
				classes := make([]string, len(d.Fields))
				for i, f := range d.Fields {
					classes[i] = f.Class
				}
				out["classes"] = classes
			case "evidence":
				if m := ev.Evidence.Email; m != nil {
					// Keys rotate and are revoked: checkpoint the sealed key
					// record at once (the next cadence tick witnesses it).
					cp, err := s.milestone(ctx)
					if err != nil {
						return err
					}
					out["checkpoint"] = cp
					out["dkim"] = m.DKIM.Result
					out["merchant_signed"] = m.DKIM.Merchant
					out["key_source"] = m.KeySource
					out["dmarc_policy"] = m.DKIM.Policy
					out["scope"] = emailScopeLine
					out["merchant_says"] = emailVerdictWords(m.DKIM)
					out["we_say"] = ourSealWords
					out["parsed"] = m.Parsed
					if hint := obligationHint(m.Parsed); hint != nil && ev.Evidence.Obligation == nil {
						// Proposed, never sealed by itself: the agent checks it
						// against the email and seals it as evidence.
						out["obligation_hint"] = hint
					}
				}
				if o := ev.Evidence.Obligation; o != nil {
					if ev.Evidence.Email == nil {
						cp, err := s.milestone(ctx)
						if err != nil {
							return err
						}
						out["checkpoint"] = cp
					}
					out["deadline"] = map[string]any{"cancel_by": o.CancelBy, "text": o.sentence(events[0].Event.Open.Terms.Currency), "note": deadlineNotEnforced}
				}
			}
			return output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("kind", "", "message, claim, evidence, change, intent, approval, act or disclosure")
	cmd.Flags().String("input", "", "JSON body for message, claim, evidence, change, intent or act (optional for evidence with --email)")
	cmd.Flags().String("email", "", "evidence: a merchant's email as a raw RFC 822 file (.eml), headers intact; sealed with its DKIM key records")
	cmd.Flags().String("key-record", "", "evidence: the DKIM key record to check --email against instead of DNS (marked supplied)")
	cmd.Flags().String("dmarc-record", "", "evidence: the sender's DMARC record, with --key-record (marked supplied)")
	cmd.Flags().String("check", "", "approval: the check_id being answered, or the capsule_id of an intent note whose proposed limits the user confirms")
	cmd.Flags().String("choice", "", "approval: the option id the user chose, or confirm_limits for an intent note")
	cmd.Flags().String("said", "", "approval: the user's own words")
	return cmd
}

// judgeApproval binds an answer to one sealed check. It proceeds only when the
// user chose proceed, the check has no earlier answer (the first answer is
// the one an action is held to), and nothing was sealed since that check that
// could change what they were shown.
func judgeApproval(events []sealedEvent, a *dealApproval) error {
	for i, se := range events {
		if se.CapsuleID != a.Check {
			continue
		}
		if se.Event.Kind == "intent" {
			return judgeLimitsConfirmation(events, i, a)
		}
		if se.Event.Kind != "check" {
			return inputError("--check does not name a check of this deal")
		}
		if a.Choice == "confirm_limits" {
			return inputError("confirm_limits answers an intent note that proposed new limits: --check names that note")
		}
		check := se.Event.Check
		if check.Verdict == "pass" {
			return inputError("this check passed; no answer is needed")
		}
		if !slices.ContainsFunc(check.Options, func(o dealOption) bool { return o.ID == a.Choice }) {
			return inputError("--choice is not one of the options the check offered")
		}
		var answered, changed bool
		for _, later := range events[i+1:] {
			switch k := later.Event.Kind; {
			case k == "approval" && later.Event.Approval.Check == a.Check:
				answered = true
			case changesDetails(later.Event), k == "snapshot", k == "check", k == "intent":
				changed = true
			}
		}
		switch {
		case answered:
			a.Reason = "this check was already answered; check again"
		case changed:
			a.Reason = "details changed after this check; check again"
		}
		// The record says what the user chose. A stale answer cannot authorize
		// anything: the action rule requires no change after the check.
		a.Proceed = a.Choice == "proceed"
		return nil
	}
	return inputError("--check does not name a step of this deal")
}

// judgeLimitsConfirmation judges the user's answer to an intent note
// (events[i]) that proposed a higher limit or more actions. Only
// confirm_limits answers one; the answer seals the limits in force and the
// new version, which then apply. An intent note sealed after the proposal
// replaces it, and a proposal is confirmed once.
func judgeLimitsConfirmation(events []sealedEvent, i int, a *dealApproval) error {
	if a.Choice != "confirm_limits" {
		return inputError("an intent note is answered only with --choice confirm_limits")
	}
	state, err := foldDeal(events)
	if err != nil {
		return err
	}
	cur := state.intent.limits()
	proposed, more := proposedLimits(cur, *events[i].Event.Intent)
	for _, later := range events[i+1:] {
		switch {
		case later.Event.Kind == "intent":
			a.Reason = "a later intent note replaced this proposal; confirm that one"
		case later.Event.Approval != nil && later.Event.Approval.Check == a.Check && later.Event.Approval.Limits != nil:
			a.Reason = "this proposal was already confirmed"
		}
	}
	if a.Reason == "" && !more {
		return inputError("this intent note asks for no higher limit and no new action: there is nothing to confirm")
	}
	a.Limits = &dealLimits{Previous: cur, New: proposed}
	a.Proceed = a.Reason == ""
	return nil
}

func dealCheckCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "check", Short: "Seal what is about to happen, compare it with the baseline, and return the difference card", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := readInput(path)
		if err != nil {
			return err
		}
		var snap dealSnapshot
		if err = decodeJSONAs("--input", raw, &snap); err != nil {
			return err
		}
		staleAfter, _ := c.Flags().GetDuration("stale-after")
		if staleAfter < time.Minute {
			return inputError("--stale-after must be at least one minute")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			if dealFinallyClosed(events) {
				return inputError("this deal is closed and takes no more steps; start a new one with `deal open`")
			}
			open := events[0].Event.Open
			if !slices.Contains(dealPointsOfNoReturn[open.Type], snap.Action) {
				return inputError("action is not a point of no return for a " + open.Type + " deal; use one of " + strings.Join(dealPointsOfNoReturn[open.Type], ", "))
			}
			if err := normalizeTerms(snap.Terms); err != nil {
				return err
			}
			if snap.Who != nil && snap.Who.DomainAgeDays != nil {
				return inputError("record the website's age as evidence, not in the check")
			}
			if snap.Action == "share_contact" && len(snap.Disclosing) == 0 {
				return inputError(`a share_contact check needs "disclosing": the class of every field about to be given, for example ["name","email","address"]`)
			}
			if len(snap.Disclosing) > 0 {
				for i := range snap.Disclosing {
					snap.Disclosing[i] = strings.ToLower(strings.TrimSpace(snap.Disclosing[i]))
				}
				action, err := disclosureAction(snap.Disclosing)
				if err != nil {
					return err
				}
				snap.Disclosing = sortedClasses(snap.Disclosing)
				if action != snap.Action {
					return inputError("disclosing " + strings.Join(snap.Disclosing, ", ") + " goes with action " + action)
				}
			}
			// Every share check says who receives it; nothing defaults to the
			// counterparty, so an approval always names the party it is for.
			if strings.HasPrefix(snap.Action, "share_") {
				switch snap.DisclosingTo {
				case "":
					return inputError(`a share check needs "disclosing_to": "counterparty" (the deal's counterparty) or "other" (with a recipient)`)
				case "counterparty":
					if snap.Recipient != nil {
						return inputError(`recipient goes with "disclosing_to": "other"; a telling to the counterparty is about the deal's own counterparty`)
					}
				case "other":
					if snap.Recipient == nil || len(counterpartyKeys(*snap.Recipient, "")) == 0 {
						return inputError(`"disclosing_to": "other" needs a recipient with a phone, email, profile id, reply address or website, so the approval names who it is for`)
					}
				default:
					return inputError(`disclosing_to must be counterparty or other`)
				}
			} else if snap.DisclosingTo != "" || snap.Recipient != nil {
				return inputError(`disclosing_to and recipient go with a share check`)
			}
			if snap.Action == "pay" {
				// The check names the payee it is about, so an action can be
				// held to the same payee.
				state, err := foldDeal(events)
				if err != nil {
					return err
				}
				payee := state.who.Payee
				if payee == "" {
					payee = state.who.Name
				}
				if snap.Who == nil {
					snap.Who = &dealWho{}
				}
				if snap.Who.Payee == "" {
					snap.Who.Payee = payee
				}
			}
			// snapshot -> seal
			snapped, err := s.seal(ctx, dealID, events, dealEvent{Kind: "snapshot", Snapshot: &snap})
			if err != nil {
				return err
			}
			events = append(events, snapped)
			// -> diff
			state, err := foldDeal(events)
			if err != nil {
				return err
			}
			if state.memory, err = s.counterpartyMemory(ctx, dealID); err != nil {
				return err
			}
			result := evaluateDeal(state, snap)
			if result.Remote, err = dealRemoteCheck(ctx, state, snap, result); err != nil {
				return err
			}
			result.Differences = append(result.Differences, result.Remote.Differences...)
			settleCheck(&result, state)
			result.Snapshot = snapped.CapsuleID
			result.Card = renderCard(result, open.Demo)
			// -> seal the result before it is shown
			checked, err := s.seal(ctx, dealID, events, dealEvent{Kind: "check", Check: &result})
			if err != nil {
				return err
			}
			events = append(events, checked)
			out := stepOutput(dealID, checked)
			// A passing check of an action the user already allowed is
			// approved by that standing intent, sealed as its own step: every
			// action cites a sealed approval.
			if result.Verdict == "pass" {
				approval := &dealApproval{Check: checked.CapsuleID, Choice: "proceed", Approver: "standing_intent", Proceed: true}
				approved, err := s.seal(ctx, dealID, events, dealEvent{Kind: "approval", Approval: approval})
				if err != nil {
					return err
				}
				out["approval_id"] = approved.CapsuleID
			}
			out["check_id"] = checked.CapsuleID
			out["snapshot_id"] = snapped.CapsuleID
			out["verdict"] = result.Verdict
			out["proceed"] = result.Verdict == "pass"
			out["card"] = result.Card
			out["options"] = result.Options
			out["differences"] = result.Differences
			out["unverified"] = result.Unverified
			out["asked_attributes"] = result.Asked
			out["picked_by_agent"] = result.Picked
			if rc := result.Recipient; rc != nil {
				out["recipient"] = rc
				if len(rc.Repeat) > 0 {
					words := make([]string, len(rc.Repeat))
					for i, c := range rc.Repeat {
						words[i] = classWord(c)
					}
					out["note"] = "Told " + rc.Name + " your " + strings.Join(words, ", ") + " before: noted, no pause for that."
				}
			}
			out["remote"] = result.Remote.Status
			out["demo"] = open.Demo
			// A date passing is a point of no return too: every check lists
			// the deal's open cancel-by dates.
			out["open_deadlines"] = openDeadlines(events, dealClock())
			out["checked_at"] = checked.Event.At
			out["stale_after_minutes"] = int(staleAfter / time.Minute)
			out["approval_text"] = dealApprovalText(state, snap, result, checked.Event.At, staleAfter)
			return output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("input", "", "Snapshot JSON: the action and exactly what is about to happen")
	cmd.Flags().Duration("stale-after", 15*time.Minute, "How long the check stays current; the approval text says when it goes stale")
	return cmd
}

func dealCloseCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "close", Short: "Compare what was delivered with what was agreed: completed, mismatch or open", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("input")
		raw, err := readInput(path)
		if err != nil {
			return err
		}
		var in dealCloseInput
		if err = decodeJSONAs("--input", raw, &in); err != nil {
			return err
		}
		if !slices.Contains([]string{"received", "pending", "not_received"}, in.Status) {
			return inputError("status must be received, pending or not_received")
		}
		if err = normalizeTerms(in.Delivered); err != nil {
			return err
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			if dealFinallyClosed(events) {
				return inputError("this deal is closed and takes no more steps; start a new one with `deal open`")
			}
			state, err := foldDeal(events)
			if err != nil {
				return err
			}
			carry, _ := c.Flags().GetBool("carry-open-obligations")
			open := openDeadlines(events, dealClock())
			if len(open) > 0 && in.Status != "pending" && !carry {
				return inputError("this deal has an open cancel-by date (" + open[0].CancelBy + ": " + open[0].Text + "); closing would end its record. Close with status pending, close after the cancel is sealed or the date has passed, or close with --carry-open-obligations to keep the date open after the close")
			}
			result := closeDeal(state, in)
			if in.Status != "pending" {
				for _, d := range open {
					result.Carried = append(result.Carried, dealCarried{Step: d.Step, CapsuleID: d.CapsuleID, CancelBy: d.CancelBy})
				}
			}
			for _, se := range events {
				if se.Event.Kind == "act" && se.Event.Act.Unchecked {
					result.UncheckedActions++
				}
			}
			observed, err := s.seal(ctx, dealID, events, dealEvent{Kind: "outcome", Outcome: &result})
			if err != nil {
				return err
			}
			events = append(events, observed)
			closing := result
			closing.Note = ""
			se, err := s.seal(ctx, dealID, events, dealEvent{Kind: "close", Close: &closing})
			if err != nil {
				return err
			}
			cp, err := s.milestone(ctx)
			if err != nil {
				return err
			}
			out := stepOutput(dealID, se)
			out["outcome"] = result.Outcome
			out["differences"] = result.Differences
			out["unchecked_actions"] = result.UncheckedActions
			out["checkpoint"] = cp
			if len(result.Carried) > 0 {
				out["carried_open_obligations"] = result.Carried
				out["note"] = "The deal is closed; the carried cancel-by dates stay open, and `deal deadlines` lists them as CARRIED AT CLOSE until a later record resolves them or the date passes."
			}
			return output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("input", "", "Close JSON: status and what was delivered")
	cmd.Flags().Bool("carry-open-obligations", false, "Close although a cancel-by date is open: the close seals it, and it stays open after the close")
	return cmd
}

func dealReportCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "report", Short: "Report the deal in three parts (what you asked, what the agent did, anomalies); --html writes one local page", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		htmlPath, _ := c.Flags().GetString("html")
		emailPath, _ := c.Flags().GetString("email")
		bundlePath, _ := c.Flags().GetString("bundle")
		audience, _ := c.Flags().GetString("share")
		recipient, _ := c.Flags().GetString("to")
		recipient = strings.TrimSpace(recipient)
		fromBundle, _ := c.Flags().GetString("from-bundle")
		directory, _ := c.Flags().GetString("directory")
		// A countersignature covers one exact bundle: your own copy. A shared
		// copy withholds records, so it is a different bundle.
		if fromBundle != "" && audience != dealAudienceKeep {
			return inputError("--from-bundle renders your own copy, the bundle a countersignature covers; a shared copy (--share) withholds records, so it is a different bundle and is not countersigned")
		}
		switch audience {
		case dealAudienceKeep:
			if recipient != "" {
				return inputError("--to names who a shared copy is for; use it with --share counterparty or adjudicator")
			}
		case dealAudienceCounterparty, dealAudienceAdjudicator:
			if htmlPath == "" || recipient == "" {
				return inputError("--share writes a copy for someone else: it needs --html FILE and --to (who it is for)")
			}
			// The email and the bundle file are the user's own copy, with
			// nothing withheld; a shared copy is the page alone.
			if emailPath != "" || bundlePath != "" {
				return inputError("--share writes only the shared page; --email and --bundle are the user's own copy")
			}
		default:
			return inputError("--share must be keep, counterparty or adjudicator")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			report, reportErr := s.dealReportFor(ctx, events)
			if reportErr != nil {
				return reportErr
			}
			lines := make([]string, 0, len(events))
			outcome := "open"
			restated := restatedIntents(events)
			for i, se := range events {
				if se.Event.Kind == "close" {
					outcome = se.Event.Close.Outcome
				}
				lines = append(lines, fmt.Sprintf("%d. %s %s", se.Event.N, se.Event.At, trailLineIn(events, restated, i)))
			}
			out := map[string]any{
				"deal_id": dealID, "scope": dealScopeLine, "did_line": dealDidLine(dealDidSources(events)), "demo": events[0].Event.Open.Demo, "outcome": outcome,
				"asked": report.Asked, "did": report.Did, "told": report.Told, "anomalies": report.Anomalies, "merchant": report.Merchant,
				"instructions": report.Instructions,
				"produced_by":  dealProducers(events),
				"deadlines":    dealDeadlines(events, dealClock(), 2), "cancellations": dealCancellations(events), "lifecycle": buildDealLifecycle(events, dealClock()), "trail": strings.Join(lines, "\n"),
				"countersign": dealNotCountersigned(),
			}
			if report.Money != nil {
				out["money"] = report.Money
			}
			if htmlPath == "" && emailPath == "" && bundlePath == "" && fromBundle == "" {
				// Every report states its rung, read from the same bundle the
				// page and the email would carry.
				if s.dp.Checkpoint.Signing == (Secret{}) {
					out["assurance"] = dealAssurance(map[string]interface{}{})
					return output(c, out)
				}
				b, err := s.dealReportBundle(ctx, events, report, dealAudienceKeep, "")
				if err != nil {
					return err
				}
				out["assurance"] = dealAssurance(b)
				return output(c, out)
			}
			// --from-bundle renders the receipt from a bundle file this deal
			// wrote earlier (and someone may have countersigned since), so a
			// countersignature covers exactly what the receipt shows.
			var b map[string]interface{}
			var err error
			if fromBundle != "" {
				b, err = s.dealBundleOf(fromBundle, dealID, events)
			} else {
				b, err = s.dealReportBundle(ctx, events, report, audience, dealVerifyCommand(htmlPath))
			}
			if err != nil {
				return err
			}
			countersign := dealNotCountersigned()
			if fromBundle != "" {
				if countersign, err = dealCountersignVerify(ctx, b, directory, s.p); err != nil {
					return err
				}
			}
			out["countersign"] = countersign
			page, err := dealReportHTML(b, countersign)
			if err != nil {
				return err
			}
			assurance := dealAssurance(b)
			if audience != dealAudienceKeep {
				// The final bytes are checked before anything is on record.
				if err = dealShareGate([]byte(page), events, audience); err != nil {
					return err
				}
				// Sharing is a disclose act: it is on record before the file
				// exists, and the output carries none of the local report's
				// raw text.
				share, err := s.recordShare(ctx, dealID, b, audience, recipient, []byte(page))
				if err != nil {
					return err
				}
				if err = atomicFile(htmlPath, []byte(page), false); err != nil {
					return err
				}
				return output(c, map[string]any{"deal_id": dealID, "scope": dealScopeLine, "html": htmlPath, "assurance": assurance, "countersign": countersign, "share": share})
			}
			bundle, err := json.Marshal(b)
			if err != nil {
				return err
			}
			out["assurance"] = assurance
			if htmlPath != "" {
				if err = atomicFile(htmlPath, []byte(page), false); err != nil {
					return err
				}
				out["html"] = htmlPath
			}
			if bundlePath != "" {
				if err = atomicFile(bundlePath, bundle, false); err != nil {
					return err
				}
				out["bundle"] = bundlePath
			}
			if emailPath != "" {
				view := dealEmailView{Demo: events[0].Event.Open.Demo, Asked: report.Asked, Outcome: outcome, Assurance: assurance["text"].(string), Countersign: countersign.Text, Did: report.Did, Anomalies: report.Anomalies, Merchant: report.Merchant, Deadlines: dealDeadlines(events, dealClock(), 2), Cancellations: dealCancellations(events), Lifecycle: buildDealLifecycle(events, dealClock()), Steps: len(events)}
				eml, subject, text, htmlBody, err := dealEmail(view, []byte(page), bundle, dealClock())
				if err != nil {
					return err
				}
				if err = atomicFile(emailPath, eml, false); err != nil {
					return err
				}
				out["email"] = map[string]any{"eml": emailPath, "subject": subject, "text": text, "html": htmlBody, "attachments": []string{"receipt.html", "bundle.json"}}
			}
			return output(c, out)
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("html", "", "Write the report as one local, self-contained page to this new file")
	cmd.Flags().String("email", "", "Write the receipt as a ready-to-send email (.eml, no sender or recipient) to this new file, for the agent host's own email tool to send")
	cmd.Flags().String("bundle", "", "Write the deal's Evidence Bundle (for `capsulectl verify --bundle`) to this new file")
	cmd.Flags().String("share", dealAudienceKeep, "Who the page is for: keep (your own copy, nothing withheld), counterparty or adjudicator (a shared copy, put on record first)")
	cmd.Flags().String("to", "", "With --share: who the shared copy is for, as sealed in the disclosure record")
	cmd.Flags().String("from-bundle", "", "Render from this deal's earlier bundle file instead of a new one, with any countersignatures it carries (see `deal countersign`)")
	cmd.Flags().String("directory", "", "With --from-bundle: the countersigner directory (HTTPS URL or file) that resolves who countersigned")
	return cmd
}

func dealExportCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "export", Short: "Write the deal's sealed x-deal-v0 records as one JSON array, in order (no raw values)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		path, _ := c.Flags().GetString("output")
		if path == "" {
			return inputError("--output is required: the file to write the result to")
		}
		return runDeal(c, true, func(ctx context.Context, s *dealSession, dealID string, events []sealedEvent) error {
			records := make([]json.RawMessage, 0, len(events))
			var before []sealedEvent
			for _, se := range events {
				payload, _, err := encodeDealRecord(se.Event, before, s.dkey)
				if err != nil {
					return err
				}
				records = append(records, payload)
				before = append(before, se)
			}
			b, err := json.Marshal(records)
			if err != nil {
				return err
			}
			if err = atomicFile(path, b, false); err != nil {
				return err
			}
			return output(c, map[string]any{"deal_id": dealID, "records": len(records), "output": path})
		})
	}}
	cmd.Flags().String("deal", "", "Deal ID from `deal open`")
	cmd.Flags().String("output", "", "New file for the JSON array of records")
	return cmd
}
