package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/action-state-group/capsule-emit-go/artifact"
	artifactmysql "github.com/action-state-group/capsule-emit-go/artifact/mysql"
	artifactsqlite "github.com/action-state-group/capsule-emit-go/artifact/sqlite"
	"github.com/action-state-group/cll-go/cll"
	cllmysql "github.com/action-state-group/cll-go/store/mysql"
	cllsqlite "github.com/action-state-group/cll-go/store/sqlite"
	driver "github.com/go-sql-driver/mysql"
)

var ErrConflict = errors.New("stored state conflicts with the selected target or identity")
var ErrPending = errors.New("operation durably saved; delivery pending, retry the same frozen request and target")
var ErrReadOnlyCLL = errors.New("operation requires writes; profile is read-only")
var ErrPartial = errors.New("verification incomplete or required originals unavailable")

// openLog and initLog dispatch CLL storage by profile type. MySQL and SQLite are
// peer backends. For SQLite the coordinate is a file path (opened as its own
// single-writer handle); for MySQL it is the shared DSN.
func openLog(ctx context.Context, p Profile, coordinate, logID string) (cll.Backend, error) {
	switch p.Type {
	case "mysql":
		return cllmysql.Open(ctx, coordinate, logID)
	case "sqlite":
		return cllsqlite.Open(coordinate, logID)
	default:
		return nil, inputError("unsupported profile type")
	}
}

func initLog(ctx context.Context, p Profile, coordinate, logID string) error {
	switch p.Type {
	case "mysql":
		return cllmysql.Init(ctx, coordinate, logID)
	case "sqlite":
		return cllsqlite.Init(coordinate, logID)
	default:
		return inputError("unsupported profile type")
	}
}

func newArtifactStore(p Profile, db *sql.DB, keys []ed25519.PublicKey) (artifactStore, error) {
	switch p.Type {
	case "mysql":
		return artifactmysql.New(db, p.Namespace, keys)
	case "sqlite":
		return artifactsqlite.New(db, p.Namespace, keys)
	default:
		return nil, inputError("unsupported profile type")
	}
}

// artifactStore is the subset of the artifact SDK the CLI drives, satisfied by
// both the MySQL and SQLite backends so openTarget can pick one by profile type.
type artifactStore interface {
	Init(context.Context) error
	Put(context.Context, artifact.Record) error
	Get(context.Context, string) (artifact.Record, error)
}

type target struct {
	db        *sql.DB
	artifacts artifactStore
	log       cll.Backend
	profile   Profile
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

// connection opens the backend named by the profile type. MySQL and SQLite are
// peer backends: neither is the default body, each is a named case.
func connection(p Profile) (*sql.DB, string, error) {
	switch p.Type {
	case "mysql":
		return mysqlConnection(p)
	case "sqlite":
		return sqliteConnection(p)
	default:
		return nil, "", inputError("unsupported profile type")
	}
}

func mysqlConnection(p Profile) (*sql.DB, string, error) {
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

// sqliteConnection opens the local artifact database handle and returns the
// absolute file path as the CLL coordinate. A single open connection serializes
// writers; foreign_keys and WAL match the cll-go SQLite store so both sets of
// tables share one file safely.
func sqliteConnection(p Profile) (*sql.DB, string, error) {
	absolute, e := filepath.Abs(p.Connection.Database)
	if e != nil {
		return nil, "", e
	}
	uri := (&url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=rwc"}).String()
	db, e := sql.Open("sqlite", uri)
	if e != nil {
		return nil, "", e
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL"} {
		if _, e = db.ExecContext(context.Background(), pragma); e != nil {
			return nil, "", errors.Join(e, db.Close())
		}
	}
	return db, absolute, nil
}
func openTarget(ctx context.Context, p Profile, use targetUse) (_ *target, err error) {
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
		t.artifacts, err = newArtifactStore(p, db, keys)
		if err != nil {
			return nil, err
		}
	}
	if use == useInitialization && needsArtifacts {
		if err = t.artifacts.Init(ctx); err != nil {
			return nil, err
		}
	}
	if needsCLL {
		if use == useInitialization {
			if err = initLog(ctx, p, dsn, p.LogID); err != nil {
				return nil, err
			}
		}
		if t.log, err = openLog(ctx, p, dsn, p.LogID); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// Publication reports Capsule identity and delivery state. A caller should
// retain the frozen request and use the same signing configuration and target.
type Publication struct {
	Namespace string `json:"namespace"`
	LogID     string `json:"log_id"`
	CapsuleID string `json:"capsule_id"`
	Sequence  uint64 `json:"sequence,omitempty"`
	State     string `json:"state"`
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
	return Publication{Namespace: t.profile.Namespace, LogID: t.profile.LogID, CapsuleID: record.CapsuleID, State: "pending"}, nil
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
