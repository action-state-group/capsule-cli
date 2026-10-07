package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// retireClock dates a retired profile's name; tests set it.
var retireClock = func() time.Time { return time.Now().UTC() }

// `profile retire NAME` frees a profile's name without touching what it
// recorded: the profile is renamed NAME-retired-YYYYMMDD (NAME-retired-
// YYYYMMDD-2, -3, ... when that is taken), with its log id, keys and data
// directory unchanged, so every record, checkpoint and witness receipt stays
// in its own log and the retired profile still bundles and verifies it.
// Nothing is deleted or re-logged. It is not made read-only: a jsonl
// profile's bundle puts itself on record, which a read-only profile refuses.
//
// --move-data DIR also renames a jsonl profile's data directory to DIR (a
// path that does not exist yet, on the same filesystem), for an install that
// wants the old path for a new profile.
func profileRetireCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retire NAME",
		Short: "Rename a profile to NAME-retired-YYYYMMDD, freeing its name; nothing it recorded is moved, changed or deleted",
		Args:  oneArg,
		RunE: func(c *cobra.Command, args []string) error {
			moveTo, _ := c.Flags().GetString("move-data")
			result, err := retireProfile(args[0], moveTo)
			if err != nil {
				return err
			}
			return output(c, result)
		},
	}
	cmd.Flags().String("move-data", "", "Also rename a jsonl profile's data directory to this new path (same filesystem), for a new profile to use the old one")
	return cmd
}

func retireProfile(name, moveTo string) (map[string]any, error) {
	p, err := loadProfile(name) // its errors already say what is wrong
	if err != nil {
		return nil, err
	}
	retired, err := retiredName(name)
	if err != nil {
		return nil, err
	}
	data := p.Connection.Database
	if moveTo != "" {
		if moveTo, err = checkDataMove(p, moveTo); err != nil {
			return nil, err
		}
	}
	oldPath, err := profilePath(name)
	if err != nil {
		return nil, err
	}
	p.Name = retired
	if moveTo != "" {
		p.Connection.Database = moveTo
	}
	// The retired profile is written first, create-only; the data moves next;
	// the old file goes last. A failure before the last step leaves NAME as
	// it was, and removes the retired file this run wrote.
	if err = saveProfile(p, false); err != nil {
		return nil, err
	}
	undo := func(cause error) (map[string]any, error) {
		if path, e := profilePath(retired); e == nil {
			cause = errors.Join(cause, os.Remove(path))
		}
		return nil, cause
	}
	if moveTo != "" {
		if err = os.Rename(data, moveTo); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				err = inputError("--move-data must stay on the data directory's filesystem; nothing was changed")
			}
			return undo(err)
		}
	}
	if err = os.Remove(oldPath); err != nil {
		if moveTo != "" {
			err = errors.Join(err, os.Rename(moveTo, data))
		}
		return undo(err)
	}
	result := map[string]any{
		"retired": name, "now": retired, "log_id": p.LogID, "data": p.Connection.Database,
		"note": "nothing it recorded changed; `capsulectl --profile " + retired + "` still bundles and verifies it. " +
			"Anything that ran with --profile " + name + " (a scheduled checkpoint cadence, a plugin) now refers to a free name: point it at the new profile or at " + retired,
	}
	return result, nil
}

// retiredName is the first free NAME-retired-YYYYMMDD[-N] for name.
func retiredName(name string) (string, error) {
	base := name + "-retired-" + retireClock().Format("20060102")
	for n := 1; n < 100; n++ {
		candidate := base
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d", base, n)
		}
		if !profileName.MatchString(candidate) {
			return "", inputError("profile " + name + " is too long a name to retire as " + candidate + " (at most 64 characters)")
		}
		path, err := profilePath(candidate)
		if err != nil {
			return "", err
		}
		if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", inputError("profile " + name + " has been retired 99 times today")
}

// checkDataMove refuses a --move-data the retire cannot do safely: only a
// jsonl profile's data directory moves (a sqlite store has sibling files), to
// a new path, and never one holding a key file the profile names by path.
func checkDataMove(p Profile, moveTo string) (string, error) {
	if p.Type != "jsonl" {
		return "", inputError("--move-data moves a jsonl profile's data directory; a " + p.Type + " profile keeps its store where it is")
	}
	target, err := filepath.Abs(moveTo)
	if err != nil {
		return "", err
	}
	if _, err = os.Lstat(target); err == nil {
		return "", inputError("--move-data " + moveTo + " already exists: name a new path")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	data, err := filepath.Abs(p.Connection.Database)
	if err != nil {
		return "", err
	}
	for _, s := range []Secret{p.Signing, p.Checkpoint.Signing, p.Checkpoint.Token, p.Credentials.Password} {
		if s.File == "" {
			continue
		}
		file, err := filepath.Abs(s.File)
		if err != nil {
			return "", err
		}
		if rel, err := filepath.Rel(data, file); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", inputError("--move-data: the profile names a key file inside its data directory (" + s.File + "), which a move would break; retire without --move-data")
		}
	}
	return target, nil
}
