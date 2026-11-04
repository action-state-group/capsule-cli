package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// commitmentPins are the checkpoints an asker saw first, per node key and
// stream: trust on first use for a node no witness vouches for. Every later
// answer for the stream must prove consistency to the pin, so the asker holds
// its own private witness from first contact on, with no infrastructure
// (loop#319, COMMON_RULES, "Trust on first use for the unwitnessed"). The pin
// is the asker's own file; nothing the node or a carrier says writes it.
type commitmentPins struct {
	KeyID   string               `json:"key_id"`
	Streams map[string]streamPin `json:"streams"`
	path    string
	changed bool
}

// streamPin is the first checkpoint seen for a stream.
type streamPin struct {
	LogID  string `json:"log_id"`
	Size   uint64 `json:"size"`
	Root   string `json:"root"`
	SeenAt string `json:"seen_at"`
}

// DefaultPinDir is where pins live: beside the profiles, under the user's
// config directory.
func DefaultPinDir() (string, error) {
	profiles, err := ProfilesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(profiles), "commitment-pins"), nil
}

// loadPins reads the pins for a node key; none yet is an empty set.
func loadPins(dir, keyID string) (*commitmentPins, error) {
	p := &commitmentPins{KeyID: keyID, Streams: map[string]streamPin{}, path: filepath.Join(dir, keyID+".json")}
	raw, err := os.ReadFile(p.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return p, nil
	case err != nil:
		return nil, fmt.Errorf("read the pin file: %w", err)
	}
	if err := json.Unmarshal(raw, p); err != nil || p.KeyID != keyID || p.Streams == nil {
		return nil, InputError("the pin file " + p.path + " is not this node key's pins; remove it to pin again")
	}
	return p, nil
}

// add pins a stream's first checkpoint; a stream already pinned is not
// changed.
func (p *commitmentPins) add(stream string, pin streamPin) {
	if _, held := p.Streams[stream]; !held {
		p.Streams[stream] = pin
		p.changed = true
	}
}

// save writes the pins atomically; it does nothing when nothing was added.
func (p *commitmentPins) save() error {
	if !p.changed {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.path), ".pin-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p.path)
}
