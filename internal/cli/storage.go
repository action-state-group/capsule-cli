package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/action-state-group/capsule-emit-go/artifact"
	artifactmysql "github.com/action-state-group/capsule-emit-go/artifact/mysql"
	"github.com/action-state-group/cll-go/cll"
	cllmysql "github.com/action-state-group/cll-go/store/mysql"
	driver "github.com/go-sql-driver/mysql"
)

var ErrConflict = errors.New("operation conflicts with its frozen input or target")
var ErrPending = errors.New("operation durably saved; delivery pending, retry the same request and key")
var ErrReadOnlyCLL = errors.New("operation requires writes; profile is read-only")
var ErrPartial = errors.New("verification incomplete or required originals unavailable")

// CLI-owned tables contain coordination only, never a replacement for the SDK
// artifact schema or CLL schema. MySQL DDL is individually idempotent; a log is
// marked ready only after every library's explicit initialization succeeds.
type coordinationStatement struct {
	sql       string
	artifacts bool
	cll       bool
}

var coordinationDDL = []coordinationStatement{
	{sql: `CREATE TABLE IF NOT EXISTS capsule_cli_identity (singleton TINYINT PRIMARY KEY, store_id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, CHECK(singleton=1)) ENGINE=InnoDB`, artifacts: false, cll: false},
	{sql: `CREATE TABLE IF NOT EXISTS capsule_cli_logs (log_id VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin PRIMARY KEY, namespace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL) ENGINE=InnoDB`, artifacts: false, cll: true},
	{sql: `CREATE TABLE IF NOT EXISTS capsule_cli_operations (store_id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, namespace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, log_id VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL, operation_key VARBINARY(191) NOT NULL, request_digest BINARY(32) NOT NULL, signer BINARY(32) NOT NULL, capsule_id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL, appended_seq BIGINT UNSIGNED NULL, PRIMARY KEY(store_id,namespace,log_id,operation_key)) ENGINE=InnoDB`, artifacts: true, cll: true},
	{sql: `CREATE TABLE IF NOT EXISTS capsule_cli_checkpoints (store_id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, log_id VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL, mmr_size BIGINT UNSIGNED NOT NULL, statement LONGBLOB NOT NULL, service_id VARCHAR(191) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, PRIMARY KEY(store_id,log_id,mmr_size)) ENGINE=InnoDB`, artifacts: false, cll: true},
}

type target struct {
	db        *sql.DB
	artifacts *artifactmysql.Store
	log       cll.Backend
	profile   Profile
	storeID   string
}

// targetUse names the actual dependencies instead of inferring artifact access
// from whether a command also needs a log. CLL-only commands never build SDK
// artifact handles; publication explicitly needs both.
type targetUse uint8

const (
	useArtifacts targetUse = iota
	useCLL
	usePublication
	useCLLRead
	useInitialization
)

func (t *target) close() error {
	var e error
	if t.log != nil {
		e = t.log.Close()
	}
	return errors.Join(e, t.db.Close())
}
func connection(p Profile) (*sql.DB, string, error) {
	password, e := p.Credentials.Password.resolve()
	if e != nil {
		return nil, "", e
	}
	cfg := driver.NewConfig()
	cfg.User = p.Credentials.Username
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(p.Connection.Host, strconv.Itoa(p.Connection.Port))
	cfg.DBName = p.Connection.Database
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.TLSConfig = p.Connection.TLS
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	dsn := cfg.FormatDSN()
	db, e := sql.Open("mysql", dsn)
	if e != nil {
		return nil, "", e
	}
	db.SetMaxOpenConns(4)
	return db, dsn, nil
}
func openTarget(ctx context.Context, p Profile, use targetUse) (_ *target, err error) {
	if use > useInitialization {
		return nil, inputError("unsupported target facilities")
	}
	needsArtifacts := use == useArtifacts || use == usePublication || (use == useInitialization && p.Namespace != "")
	needsCLL := use == useCLL || use == useCLLRead || use == usePublication || (use == useInitialization && p.LogID != "")
	if needsArtifacts && p.Namespace == "" {
		return nil, inputError("this command requires an artifact namespace")
	}
	if needsCLL && p.LogID == "" {
		return nil, inputError("this command requires a log_id")
	}
	if p.ReadOnly && use != useArtifacts && use != useCLLRead {
		return nil, ErrReadOnlyCLL
	}
	keys, err := parseKeys(p.TrustedKeys)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 && needsArtifacts {
		return nil, inputError("artifact operations require producer trusted_keys")
	}
	db, dsn, err := connection(p)
	if err != nil {
		return nil, err
	}
	t := &target{db: db, profile: p}
	defer func() {
		if err != nil {
			err = errors.Join(err, t.close())
		}
	}()
	// CLL-only commands do not need producer policy or an artifact store. The
	// SDK constructor rightly requires trust, but that is not a CLL requirement.
	if needsArtifacts {
		t.artifacts, err = artifactmysql.New(db, p.Namespace, keys)
		if err != nil {
			return nil, err
		}
	}
	if use == useArtifacts && p.StoreID == "" {
		return t, nil
	}
	if use == useCLLRead && p.StoreID == "" {
		if t.log, err = cllmysql.Open(ctx, dsn, p.LogID); err != nil {
			return nil, err
		}
		return t, nil
	}
	if use == useInitialization {
		for _, stmt := range coordinationDDL {
			if stmt.artifacts && !needsArtifacts || stmt.cll && !needsCLL {
				continue
			}
			if _, err = db.ExecContext(ctx, stmt.sql); err != nil {
				return nil, err
			}
		}
		id := make([]byte, 32)
		if _, err = rand.Read(id); err != nil {
			return nil, err
		}
		if _, err = db.ExecContext(ctx, `INSERT IGNORE INTO capsule_cli_identity(singleton,store_id) VALUES(1,?)`, hex.EncodeToString(id)); err != nil {
			return nil, err
		}
	}
	if err = t.loadIdentity(ctx); err != nil {
		return nil, err
	}
	if p.StoreID == "" && use != useInitialization && use != useCLLRead {
		return nil, inputError("write operations require store init to pin the store identity")
	}

	if use == useArtifacts {
		return t, nil
	}
	if use == useInitialization {
		if needsArtifacts {
			if err = t.artifacts.Init(ctx); err != nil {
				return nil, err
			}
		}
		if !needsCLL {
			return t, nil
		}
		if err = cllmysql.Init(ctx, dsn, p.LogID); err != nil {
			return nil, err
		}
		if t.log, err = cllmysql.Open(ctx, dsn, p.LogID); err != nil {
			return nil, err
		}
		if _, err = db.ExecContext(ctx, `INSERT IGNORE INTO capsule_cli_logs(log_id,namespace) VALUES(?,?)`, p.LogID, p.Namespace); err != nil {
			return nil, err
		}
		if needsArtifacts {
			if _, err = db.ExecContext(ctx, `UPDATE capsule_cli_logs SET namespace=? WHERE log_id=? AND namespace=''`, p.Namespace, p.LogID); err != nil {
				return nil, err
			}
		}
	}
	if use == useCLLRead {
		if t.log, err = cllmysql.Open(ctx, dsn, p.LogID); err != nil {
			return nil, err
		}
		return t, nil
	}
	var namespace string
	if err = db.QueryRowContext(ctx, `SELECT namespace FROM capsule_cli_logs WHERE log_id=?`, p.LogID).Scan(&namespace); err != nil {
		return nil, err
	}
	if needsArtifacts && namespace != p.Namespace {
		return nil, ErrConflict
	}
	if use != useInitialization {
		if t.log, err = cllmysql.Open(ctx, dsn, p.LogID); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// Publication identifies the persisted exact-byte operation. A caller should
// retain this result and use the same named target/key when resuming.
type Publication struct {
	StoreID   string `json:"store_id"`
	Namespace string `json:"namespace"`
	LogID     string `json:"log_id"`
	Key       string `json:"idempotency_key"`
	CapsuleID string `json:"capsule_id"`
	Sequence  uint64 `json:"sequence,omitempty"`
	State     string `json:"state"`
}

// loadIdentity reads CLI coordination identity without provisioning it. An
// explicit profile pin is always checked, including on read-only paths.
func (t *target) loadIdentity(ctx context.Context) error {
	err := t.db.QueryRowContext(ctx, `SELECT store_id FROM capsule_cli_identity WHERE singleton=1`).Scan(&t.storeID)
	if errors.Is(err, sql.ErrNoRows) {
		return inputError("CLI store identity is not initialized")
	}
	if err != nil {
		return err
	}
	if len(t.storeID) != 64 || (t.profile.StoreID != "" && t.profile.StoreID != t.storeID) {
		return ErrConflict
	}
	return nil
}

func rollback(tx *sql.Tx, err *error) {
	e := tx.Rollback()
	if !errors.Is(e, sql.ErrTxDone) {
		*err = errors.Join(*err, e)
	}
}

func requirePublisherKey(p Profile, private ed25519.PrivateKey) error {
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return inputError("invalid signer")
	}
	trusted, err := parseKeys(p.TrustedKeys)
	if err != nil {
		return err
	}
	for _, key := range trusted {
		if bytes.Equal(key, public) {
			return nil
		}
	}
	return errors.Join(ErrInput, artifact.ErrUntrustedSigner)
}

// preparePublication serializes aliases on an authoritative transactional claim.
// The claim, exact bytes via SDK PutTx, and Capsule ID commit together. A crash
// before that commit cannot append; a crash after it resumes without sealing.
func (t *target) preparePublication(ctx context.Context, raw []byte, r Request, key string, private ed25519.PrivateKey) (_ Publication, err error) {
	if key == "" || len(key) > 191 {
		return Publication{}, inputError("idempotency key must have 1..191 bytes")
	}
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return Publication{}, errors.New("invalid signer")
	}
	if err := requirePublisherKey(t.profile, private); err != nil {
		return Publication{}, err
	}
	digest := sha256.Sum256(raw)
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return Publication{}, err
	}
	defer rollback(tx, &err)
	args := []any{t.storeID, t.profile.Namespace, t.profile.LogID, key}
	insert := append(append([]any{}, args...), digest[:], []byte(public))
	if _, err = tx.ExecContext(ctx, `INSERT INTO capsule_cli_operations(store_id,namespace,log_id,operation_key,request_digest,signer) VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE operation_key=operation_key`, insert...); err != nil {
		return Publication{}, err
	}
	var storedDigest, storedSigner []byte
	var id sql.NullString
	var seq sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT request_digest,signer,capsule_id,appended_seq FROM capsule_cli_operations WHERE store_id=? AND namespace=? AND log_id=? AND operation_key=? FOR UPDATE`, args...).Scan(&storedDigest, &storedSigner, &id, &seq); err != nil {
		return Publication{}, err
	}
	if !bytes.Equal(storedDigest, digest[:]) || !bytes.Equal(storedSigner, public) {
		return Publication{}, ErrConflict
	}
	if !id.Valid {
		record, e := seal(r, private)
		if e != nil {
			return Publication{}, e
		}
		if len(missingBindings(record)) != 0 {
			return Publication{}, ErrPartial
		}
		if err = t.artifacts.PutTx(ctx, tx, record); err != nil {
			return Publication{}, err
		}
		id = sql.NullString{String: record.CapsuleID, Valid: true}
		if _, err = tx.ExecContext(ctx, `UPDATE capsule_cli_operations SET capsule_id=? WHERE store_id=? AND namespace=? AND log_id=? AND operation_key=?`, append([]any{id.String}, args...)...); err != nil {
			return Publication{}, err
		}
	} else {
		if _, err = t.artifacts.GetTx(ctx, tx, id.String, false); err != nil {
			return Publication{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Publication{}, err
	}
	result := Publication{StoreID: t.storeID, Namespace: t.profile.Namespace, LogID: t.profile.LogID, Key: key, CapsuleID: id.String, State: "pending"}
	if seq.Valid {
		result.Sequence = uint64(seq.Int64)
		result.State = "appended"
	}
	return result, nil
}
func (t *target) publish(ctx context.Context, raw []byte, r Request, key string, private ed25519.PrivateKey) (Publication, error) {
	result, e := t.preparePublication(ctx, raw, r, key, private)
	if e != nil {
		return result, e
	}
	// Reconcile actual presence first. Completed retries, including a lost ACK,
	// must not append again or become pending merely because originals were
	// legitimately purged after the original successful append.
	record, e := t.artifacts.Get(ctx, result.CapsuleID)
	if e != nil {
		return result, e
	}
	id, e := hex.DecodeString(record.CapsuleID)
	if e != nil {
		return result, ErrConflict
	}
	entry, e := t.log.GetEntry(ctx, id)
	switch {
	case e == nil:
		if !bytes.Equal(entry.Value, id) || (result.Sequence != 0 && result.Sequence != entry.Seq) {
			return result, ErrConflict
		}
	case errors.Is(e, cll.ErrNotFound):
		if result.Sequence != 0 {
			return result, ErrConflict
		}
		// Only an actually new append requires currently retained originals.
		if len(missingBindings(record)) != 0 {
			return result, ErrPartial
		}
		entry, e = appendRecord(ctx, t.log, record.CapsuleID)
		if e != nil {
			return result, e
		}
	default:
		return result, appendError(e)
	}
	if result.Sequence != 0 {
		return result, nil
	}
	_, e = t.db.ExecContext(ctx, `UPDATE capsule_cli_operations SET appended_seq=? WHERE store_id=? AND namespace=? AND log_id=? AND operation_key=? AND capsule_id=?`, entry.Seq, t.storeID, t.profile.Namespace, t.profile.LogID, key, result.CapsuleID)
	if e != nil {
		return result, ErrPending
	}
	result.Sequence = entry.Seq
	result.State = "appended"
	return result, nil
}
func appendRecord(ctx context.Context, log cll.EntryStore, capsuleID string) (cll.Entry, error) {
	id, e := hex.DecodeString(capsuleID)
	if e != nil || len(id) != cll.EntryBytes {
		return cll.Entry{}, errors.New("invalid capsule identity")
	}
	result, e := log.Append(ctx, cll.AppendInput{Value: id, AppendedAt: time.Now().UTC()})
	if e != nil {
		return cll.Entry{}, appendError(e)
	}
	observed, e := log.GetEntry(ctx, id)
	if e != nil {
		return cll.Entry{}, appendError(e)
	}
	if !bytes.Equal(observed.Value, id) || observed.Seq != result.Entry.Seq {
		return cll.Entry{}, ErrConflict
	}
	return observed, nil
}

func appendError(err error) error {
	if errors.Is(err, ErrConflict) {
		return ErrConflict
	}
	return ErrPending
}
