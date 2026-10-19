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
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/action-state-group/capsule-emit/go/artifact"
	artifactjsonl "github.com/action-state-group/capsule-emit/go/artifact/jsonl"
	artifactmysql "github.com/action-state-group/capsule-emit/go/artifact/mysql"
	artifactsqlite "github.com/action-state-group/capsule-emit/go/artifact/sqlite"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	clljsonl "github.com/action-state-group/checkpointed-local-log/go/store/jsonl"
	cllmysql "github.com/action-state-group/checkpointed-local-log/go/store/mysql"
	cllsqlite "github.com/action-state-group/checkpointed-local-log/go/store/sqlite"
	driver "github.com/go-sql-driver/mysql"
)

// jsonlFile names the fixed artifact and CLL files inside a jsonl profile's
// connection.database directory.
const (
	JSONLArtifactFile = "artifacts.jsonl"
	JSONLLogFile      = "cll.jsonl"
)

// jsonlDir returns the absolute storage directory for a jsonl profile.
func jsonlDir(p Profile) (string, error) {
	return filepath.Abs(p.Connection.Database)
}

var ErrConflict = errors.New("stored state conflicts with the selected target or identity")
var ErrPending = errors.New("operation durably saved; delivery pending, retry the same frozen request and target")
var ErrReadOnlyCLL = errors.New("operation requires writes; profile is read-only")
var ErrPartial = errors.New("verification incomplete or required originals unavailable")

// openLog and initLog dispatch CLL storage by profile type. MySQL and SQLite are
// peer backends. For SQLite the coordinate is a file path (opened as its own
// single-writer handle); for MySQL it is the shared DSN.
func OpenLog(ctx context.Context, p Profile, coordinate, logID string) (cll.Backend, error) {
	switch p.Type {
	case "mysql":
		return cllmysql.Open(ctx, coordinate, logID)
	case "sqlite":
		return cllsqlite.Open(coordinate, logID)
	case "jsonl":
		// The file is the log; coordinate is the profile's storage directory.
		return clljsonl.Open(filepath.Join(coordinate, JSONLLogFile))
	default:
		return nil, InputError("profile type must be mysql, sqlite or jsonl")
	}
}

func initLog(ctx context.Context, p Profile, coordinate, logID string) error {
	switch p.Type {
	case "mysql":
		return cllmysql.Init(ctx, coordinate, logID)
	case "sqlite":
		return cllsqlite.Init(coordinate, logID)
	case "jsonl":
		return clljsonl.Init(filepath.Join(coordinate, JSONLLogFile))
	default:
		return InputError("profile type must be mysql, sqlite or jsonl")
	}
}

func NewArtifactStore(p Profile, db *sql.DB, keys []ed25519.PublicKey) (ArtifactStore, error) {
	switch p.Type {
	case "mysql":
		return artifactmysql.New(db, p.Namespace, keys)
	case "sqlite":
		return artifactsqlite.New(db, p.Namespace, keys)
	case "jsonl":
		dir, e := jsonlDir(p)
		if e != nil {
			return nil, e
		}
		return artifactjsonl.New(filepath.Join(dir, JSONLArtifactFile), p.Namespace, keys)
	default:
		return nil, InputError("profile type must be mysql, sqlite or jsonl")
	}
}

// ArtifactStore is the subset of the artifact SDK the CLI drives, satisfied
// by every backend (jsonl, mysql, sqlite) so OpenTarget can pick one by
// profile type. Purge erases a record's originals; only a book's retention
// calls it.
type ArtifactStore interface {
	Init(context.Context) error
	Put(context.Context, artifact.Record) error
	Get(context.Context, string) (artifact.Record, error)
	Purge(context.Context, string) error
}

type Target struct {
	db        *sql.DB
	Artifacts ArtifactStore
	// Log is the CLL of a plain profile. A book profile has no such log:
	// its one log is the evidence book (Book), and Log stays nil.
	Log     cll.Backend
	Book    BookLog
	Profile Profile
}

// errBookProfile refuses a CLL-only operation on a book profile, whose one
// log is its evidence book, rather than reaching for a second log.
var ErrBookProfile = InputError("not available on a book profile: its log is the evidence book")

// targetUse names the actual dependencies instead of inferring artifact access
// from whether a command also needs a log. CLL-only commands never build SDK
// artifact handles; publication explicitly needs both.
type TargetUse uint8

const (
	UseArtifacts TargetUse = iota
	UseCLL
	UsePublication
	UseCLLRead
	UseInitialization
	// useRead is useCLLRead with the artifact store: a read of a log's
	// records as well as its entries, writing nothing.
	UseRead
)

func (t *Target) Close() error {
	var e error
	if t.Log != nil {
		e = t.Log.Close()
	}
	if t.Book != nil {
		e = errors.Join(e, t.Book.Release())
	}
	// The jsonl backend uses no *sql.DB, so t.db is nil for that profile type.
	if t.db != nil {
		e = errors.Join(e, t.db.Close())
	}
	return e
}

// connection opens the backend named by the profile type. MySQL and SQLite are
// peer backends: neither is the default body, each is a named case.
func connection(p Profile) (*sql.DB, string, error) {
	switch p.Type {
	case "mysql":
		return mysqlConnection(p)
	case "sqlite":
		return SqliteConnection(p)
	case "jsonl":
		return jsonlConnection(p)
	default:
		return nil, "", InputError("profile type must be mysql, sqlite or jsonl")
	}
}

// jsonlConnection uses no database handle. It resolves the storage directory
// and returns it as the coordinate without side effects; the directory is
// provisioned only by store init (see openTarget), so a mistyped path fails
// loudly instead of silently creating an empty tree. The artifact store and CLL
// log open their own files (artifacts.jsonl, cll.jsonl) inside it.
func jsonlConnection(p Profile) (*sql.DB, string, error) {
	dir, e := jsonlDir(p)
	if e != nil {
		return nil, "", e
	}
	return nil, dir, nil
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
func SqliteConnection(p Profile) (*sql.DB, string, error) {
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

// missingLogIDReason says why a profile without a log_id cannot serve a log
// command, and what to pass instead.
func missingLogIDReason(p Profile) string {
	if p.Type == "sqlite" && p.Namespace == "deal" {
		return "profile " + p.Name + " has no log_id: a deal profile keeps one log per deal; pass --log-id deal/<deal id> (cll list), or add a log with `capsulectl profile update --profile " + p.Name + " --log-id NAME`"
	}
	return "profile " + p.Name + " has no log_id, which this command needs: `capsulectl profile update --profile " + p.Name + " --log-id NAME`"
}

func OpenTarget(ctx context.Context, p Profile, use TargetUse) (_ *Target, err error) {
	needsArtifacts := use == UseArtifacts || use == UsePublication || use == UseRead || (use == UseInitialization && p.Namespace != "") ||
		// A book keeps its records' originals in the artifact store.
		(use == UseCLL && p.IsBook() && p.Namespace != "")
	needsCLL := use == UseCLL || use == UseCLLRead || use == UseRead || use == UsePublication || (use == UseInitialization && p.LogID != "")
	if needsArtifacts && p.Namespace == "" {
		return nil, InputError("profile " + p.Name + " has no namespace, which this command needs: `capsulectl profile update --profile " + p.Name + " --namespace NAME`")
	}
	if needsCLL && p.LogID == "" {
		return nil, InputError(missingLogIDReason(p))
	}
	if p.ReadOnly && use != UseArtifacts && use != UseCLLRead && use != UseRead {
		return nil, ErrReadOnlyCLL
	}
	keys, err := ParseKeys(p.TrustedKeys)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 && needsArtifacts {
		return nil, InputError("artifact operations require producer trusted_keys")
	}
	db, dsn, err := connection(p)
	if err != nil {
		return nil, err
	}
	t := &Target{db: db, Profile: p}
	defer func() {
		if err != nil {
			err = errors.Join(err, t.Close())
		}
	}()
	// The jsonl backend has no auto-creating handle: store init provisions the
	// directory; any other use requires it to already exist so a mistyped path
	// or an uninitialized store fails clearly rather than fabricating a tree.
	if p.Type == "jsonl" {
		if use == UseInitialization {
			if err = os.MkdirAll(dsn, 0o755); err != nil {
				return nil, err
			}
		} else if _, statErr := os.Stat(dsn); statErr != nil {
			err = InputError("jsonl storage directory does not exist; run 'store init'")
			return nil, err
		}
	}
	// CLL-only commands do not need producer policy or an artifact store. The
	// SDK constructor rightly requires trust, but that is not a CLL requirement.
	if needsArtifacts {
		t.Artifacts, err = NewArtifactStore(p, db, keys)
		if err != nil {
			return nil, err
		}
	}
	if use == UseInitialization && needsArtifacts {
		if err = t.Artifacts.Init(ctx); err != nil {
			return nil, err
		}
	}
	if needsCLL && p.IsBook() && bookBackend == nil {
		return nil, errBookNeedsPlugin(p)
	}
	if needsCLL && p.IsBook() && (use == UseCLLRead || use == UseRead) {
		// Reads (cll list, checkpoint status) never open the book: that
		// needs its signing keys and takes its writer lock.
	} else if needsCLL && p.IsBook() && use == UseInitialization {
		// store init signs nothing, so it creates the book's files without
		// opening the book: no signing or checkpoint key, no trust check.
		if err = bookBackend.Init(ctx, p); err != nil {
			return nil, err
		}
	} else if needsCLL && p.IsBook() {
		book, err := bookBackend.Open(ctx, p, t.Artifacts)
		if err != nil {
			return nil, err
		}
		t.Book = book
	} else if needsCLL {
		if use == UseInitialization {
			if err = initLog(ctx, p, dsn, p.LogID); err != nil {
				return nil, err
			}
		}
		// Assigned only on success: a failed open returns a typed nil, which
		// close would call.
		log, err := OpenLog(ctx, p, dsn, p.LogID)
		if err != nil {
			return nil, err
		}
		t.Log = log
	}
	return t, nil
}

// Publication reports Capsule identity and delivery state. A caller should
// retain the frozen request and use the same signing configuration and target.
type Publication struct {
	Namespace string `json:"namespace"`
	LogID     string `json:"log_id"`
	CapsuleID string `json:"capsule_id"`
	// SealedID is the id `seal` gives the same request, when it differs
	// from CapsuleID: on a book profile the published capsule carries its
	// record header, so its id is the record's. The book records SealedID
	// as the record's subject_ref; claims cite CapsuleID.
	SealedID string `json:"sealed_id,omitempty"`
	Sequence uint64 `json:"sequence,omitempty"`
	State    string `json:"state"`
	// Warning is set when the record committed but a later step of the
	// append failed (jsonl profiles); the position stands.
	Warning string `json:"warning,omitempty"`
}

func RequirePublisherKey(p Profile, private ed25519.PrivateKey) error {
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return InputError("the profile's signing key is not an Ed25519 key")
	}
	trusted, err := ParseKeys(p.TrustedKeys)
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
func (t *Target) preparePublication(ctx context.Context, r Request, private ed25519.PrivateKey) (Publication, error) {
	record, err := Seal(r, private)
	if err != nil {
		return Publication{}, err
	}
	if len(MissingBindings(record)) != 0 {
		return Publication{}, ErrPartial
	}
	if err = t.Artifacts.Put(ctx, record); err != nil {
		return Publication{}, err
	}
	return Publication{Namespace: t.Profile.Namespace, LogID: t.Profile.LogID, CapsuleID: record.CapsuleID, State: "pending"}, nil
}

// Publish seals r and appends it. On a plain profile the capsule is put in
// the artifact store and its id appended to the CLL. On a book profile the
// book seals it, carrying its record header, as the record itself.
func (t *Target) Publish(ctx context.Context, r Request, private ed25519.PrivateKey) (Publication, error) {
	if t.Book != nil {
		return t.Book.Publish(ctx, r, private)
	}
	result, err := t.preparePublication(ctx, r, private)
	if err != nil {
		return result, err
	}
	entry, err := appendRecord(ctx, t.Log, result.CapsuleID)
	if err != nil {
		return result, err
	}
	result.Sequence = entry.Seq
	result.State = "appended"
	return result, nil
}
func appendRecord(ctx context.Context, log cll.Backend, capsuleID string) (cll.Entry, error) {
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
