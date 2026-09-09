package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

var ErrConflict = errors.New("stored state conflicts with the selected target or identity")
var ErrPending = errors.New("operation durably saved; delivery pending, retry the same frozen request and target")
var ErrReadOnlyCLL = errors.New("operation requires writes; profile is read-only")
var ErrPartial = errors.New("verification incomplete or required originals unavailable")

// CLI-owned tables contain coordination only, never a replacement for the SDK
// artifact schema or CLL schema. MySQL DDL is individually idempotent; a log is
// marked ready only after every library's explicit initialization succeeds.
type coordinationStatement struct {
	sql string
	cll bool
}

var coordinationDDL = []coordinationStatement{
	{sql: `CREATE TABLE IF NOT EXISTS capsule_cli_identity (singleton TINYINT PRIMARY KEY, store_id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, CHECK(singleton=1)) ENGINE=InnoDB`, cll: false},
	{sql: `CREATE TABLE IF NOT EXISTS capsule_cli_logs (log_id VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin PRIMARY KEY, namespace VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL) ENGINE=InnoDB`, cll: true},
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
			if stmt.cll && !needsCLL {
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

// Publication reports Capsule identity and delivery state. A caller should
// retain the frozen request and use the same signing configuration and target.
type Publication struct {
	StoreID   string `json:"store_id"`
	Namespace string `json:"namespace"`
	LogID     string `json:"log_id"`
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

// preparePublication persists the deterministic record through the artifact SDK.
// Retries reuse the frozen request and signer.
func (t *target) preparePublication(ctx context.Context, r Request, private ed25519.PrivateKey) (Publication, error) {
	if err := requirePublisherKey(t.profile, private); err != nil {
		return Publication{}, err
	}
	record, err := seal(r, private)
	if err != nil {
		return Publication{}, err
	}
	if len(missingBindings(record)) != 0 {
		return Publication{}, ErrPartial
	}
	if err = t.artifacts.Put(ctx, record); err != nil {
		return Publication{}, err
	}
	return Publication{StoreID: t.storeID, Namespace: t.profile.Namespace, LogID: t.profile.LogID, CapsuleID: record.CapsuleID, State: "pending"}, nil
}
func (t *target) publish(ctx context.Context, r Request, private ed25519.PrivateKey) (Publication, error) {
	result, err := t.preparePublication(ctx, r, private)
	if err != nil {
		return result, err
	}
	entry, err := appendRecord(ctx, t.log, result.CapsuleID)
	if err != nil {
		return result, err
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
		return cll.Entry{}, ErrPending
	}
	return result.Entry, nil
}
