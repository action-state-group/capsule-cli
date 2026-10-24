package cli

import "errors"

// The deal commands' error classes. The deal commands are the
// capsulectl-deal plugin's, but their classes are the core's: SafeError
// prints them and ExitCode maps them, in the plugin's binary as in this one.
var (
	// ErrPaused is a deal step held for the user: nothing was sealed, and
	// the step must not be taken until a check names it and the user
	// approves.
	ErrPaused = errors.New("paused: this needs the user's approval first")
	// ErrEmailUnverified is a merchant email whose DKIM signature does not
	// verify against the sealed key record, or a copy that is not the sealed
	// one. The result was printed.
	ErrEmailUnverified = errors.New("merchant email did not verify against the sealed key record")
	// ErrObjectUnverified is a held object, or a copy of it or of its
	// carrier headers, that does not match what its deal step sealed. The
	// result was printed.
	ErrObjectUnverified = errors.New("held object did not match its sealed digest")
	// ErrStatementInvalid is a verifier statement whose shape or signature
	// does not check out. The result was printed.
	ErrStatementInvalid = errors.New("verifier statement did not verify")
)
