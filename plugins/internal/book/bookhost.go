package book

import (
	"context"
	"crypto/ed25519"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
)

// BookBackend is the evidence book behind every book profile in the
// capsulectl-book plugin: the core's publish, cll and bundle commands reach
// a book profile's log through it (cli.SetBookBackend).
type BookBackend struct{}

var _ cli.BookBackend = BookBackend{}

// Init is `store init` on a book profile: the book's files, no signing.
func (BookBackend) Init(ctx context.Context, p cli.Profile) error { return initBookFiles(ctx, p) }

// Open opens the profile's book over the artifact store the core opened.
func (BookBackend) Open(ctx context.Context, p cli.Profile, artifacts cli.ArtifactStore) (cli.BookLog, error) {
	opened, err := openBookWith(ctx, p, false, artifacts)
	if err != nil {
		return nil, err
	}
	return &bookLog{opened: opened, profile: p}, nil
}

// List is `cll list` on a book profile.
func (BookBackend) List(ctx context.Context, p cli.Profile, after, through uint64, limit int, all bool) (any, uint64, error) {
	return listBookFiles(ctx, p, after, through, limit, all)
}

// WitnessStore keeps a book's checkpoint deliveries in book/witness.json.
func (BookBackend) WitnessStore(p cli.Profile) cll.WitnessStateStore { return newBookWitnessStore(p) }

// bookLog is one open book, as the core's commands use it.
type bookLog struct {
	opened  openedBook
	profile cli.Profile
}

func (b *bookLog) Publish(ctx context.Context, r cli.Request, key ed25519.PrivateKey) (cli.Publication, error) {
	return publishOwn(ctx, b.opened.book, b.opened.originals, b.profile, r, key)
}

func (b *bookLog) Append(ctx context.Context, record artifact.Record) (uint64, string, error) {
	// The core put the capsule in the artifact store before this. Putting it
	// again through the book's durable store (a byte-identical retry, which
	// the store admits) syncs it, so the wrapper never outlives the capsule
	// it names.
	if err := b.opened.originals.Put(ctx, record); err != nil {
		return 0, "", err
	}
	return appendWrapped(ctx, b.opened.book, record)
}

func (b *bookLog) Checkpoint(ctx context.Context, service string) (map[string]any, error) {
	return checkpointBook(ctx, b.profile, b.opened.book, service)
}

func (b *bookLog) Bundle(ctx context.Context, req cli.BookBundleRequest) ([]byte, error) {
	bundle, err := bookBundle(ctx, b.opened.book, b.opened.originals, req.Root, req.ClosureDepth, req.Payloads, req.Suppress, req.Disclose, req.AttachOriginals, req.ProducerKey)
	return bundle.JSON, err
}

func (b *bookLog) Release() error { return b.opened.release() }

// bookOf is the evidence book behind a target the core opened on a book
// profile in this plugin.
func bookOf(t *cli.Target) *bookLog {
	if b, ok := t.Book.(*bookLog); ok {
		return b
	}
	return nil
}
