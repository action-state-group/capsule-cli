package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retireOn fixes the date a retired profile is named with.
func retireOn(t *testing.T, day string) {
	t.Helper()
	at, err := time.Parse("2006-01-02", day)
	require.NoError(t, err)
	previous := retireClock
	retireClock = func() time.Time { return at }
	t.Cleanup(func() { retireClock = previous })
}

// jsonlWithARecord is a jsonl profile (the fixture's name) holding one
// published capsule, and that capsule's id.
func jsonlWithARecord(t *testing.T) (Profile, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	p.Type = "jsonl"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, saveProfile(p, false))
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	target, err := openTarget(t.Context(), p, usePublication)
	require.NoError(t, err)
	published, err := target.publish(t.Context(), request, key)
	require.NoError(t, err)
	require.NoError(t, target.close())
	return p, published.CapsuleID
}

// treeDigests is every regular file under dir, by relative path, with its
// SHA-256.
func treeDigests(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		sum := sha256.Sum256(raw)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	}))
	return out
}

// bundleVerdictOf is verify --bundle's verdict on a profile's bundle of root.
func bundleVerdictOf(t *testing.T, profile, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.json")
	_, err := invoke(t, "", "bundle", "--profile", profile, "--root", root, "--out", path)
	require.NoError(t, err)
	out, _ := invoke(t, "", "verify", "--bundle", path) // INCOMPLETE exits 3 and still prints
	var result struct {
		Verdict string `json:"verdict"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	require.NotEmpty(t, result.Verdict)
	return result.Verdict
}

func profileNames(t *testing.T) []string {
	t.Helper()
	out, err := invoke(t, "", "profile", "list")
	require.NoError(t, err)
	var listed struct {
		Profiles []string `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	return listed.Profiles
}

// Retiring frees the name and changes nothing the profile recorded: the
// same log id and data, every file byte for byte, and the retired profile
// bundles the same record with the same verdict. The name can be used again.
func TestProfileRetireFreesTheNameAndKeepsEverything(t *testing.T) {
	retireOn(t, "2026-10-07")
	p, root := jsonlWithARecord(t)
	verdict := bundleVerdictOf(t, p.Name, root)
	files := treeDigests(t, p.Connection.Database)

	out, err := invoke(t, "", "profile", "retire", p.Name)
	require.NoError(t, err)
	retired := p.Name + "-retired-20261007"
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, retired, result["now"])
	assert.Contains(t, result["note"], "refers to a free name")

	assert.Equal(t, []string{retired}, profileNames(t), "the old name is free")
	r, err := loadProfile(retired)
	require.NoError(t, err)
	assert.Equal(t, p.LogID, r.LogID)
	assert.Equal(t, p.Connection.Database, r.Connection.Database)
	assert.False(t, r.ReadOnly, "a jsonl bundle puts itself on record, which read-only would refuse")
	assert.Equal(t, files, treeDigests(t, p.Connection.Database), "nothing in the data directory changed")
	assert.Equal(t, verdict, bundleVerdictOf(t, retired, root), "the retired profile bundles its record as before")

	_, err = invoke(t, "", "profile", "create", "--name", p.Name, "--type", "sqlite", "--sqlite-path", filepath.Join(t.TempDir(), "new.db"),
		"--operator", "example-operator", "--log-id", "new-log")
	require.NoError(t, err, "the name can be used again")

	_, err = invoke(t, "", "profile", "retire", p.Name)
	require.NoError(t, err)
	assert.Contains(t, profileNames(t), retired+"-2", "a second retire the same day")
}

func TestProfileRetireRefusesWhatItCannotDo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := invoke(t, "", "profile", "retire", "absent")
	require.ErrorIs(t, err, ErrInput)

	p, _ := profileFixture(t)
	p.Name = strings.Repeat("a", 60)
	require.NoError(t, saveProfile(p, false))
	_, err = invoke(t, "", "profile", "retire", p.Name)
	require.ErrorIs(t, err, ErrInput, "too long a name to retire")
	assert.Contains(t, profileNames(t), p.Name, "and it is left as it was")
}

// --move-data renames a jsonl profile's data directory, and the retired
// profile reads it there; it refuses a sqlite profile, a target that exists,
// and a key file inside the directory, leaving the profile as it was.
func TestProfileRetireMoveData(t *testing.T) {
	retireOn(t, "2026-10-07")
	p, root := jsonlWithARecord(t)
	verdict := bundleVerdictOf(t, p.Name, root)
	files := treeDigests(t, p.Connection.Database)

	exists := t.TempDir()
	_, err := invoke(t, "", "profile", "retire", p.Name, "--move-data", exists)
	require.ErrorIs(t, err, ErrInput, "the target exists")
	assert.Contains(t, profileNames(t), p.Name)

	moved := filepath.Join(t.TempDir(), "retired-store")
	_, err = invoke(t, "", "profile", "retire", p.Name, "--move-data", moved)
	require.NoError(t, err)
	assert.NoDirExists(t, p.Connection.Database)
	r, err := loadProfile(p.Name + "-retired-20261007")
	require.NoError(t, err)
	assert.Equal(t, moved, r.Connection.Database)
	assert.Equal(t, files, treeDigests(t, moved), "moved byte for byte")
	assert.Equal(t, verdict, bundleVerdictOf(t, r.Name, root))
}

func TestProfileRetireMoveDataRefusals(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type = "sqlite"
	p.Connection.Database = filepath.Join(t.TempDir(), "store.db")
	require.NoError(t, saveProfile(p, false))
	_, err := invoke(t, "", "profile", "retire", p.Name, "--move-data", filepath.Join(t.TempDir(), "x"))
	require.ErrorIs(t, err, ErrInput, "a sqlite store is not moved")
	assert.Contains(t, profileNames(t), p.Name)

	q, _ := jsonlWithARecord(t)
	key := filepath.Join(q.Connection.Database, "signing.key")
	require.NoError(t, os.WriteFile(key, []byte("not read"), 0o600))
	q.Signing = Secret{File: key}
	require.NoError(t, saveProfile(q, true))
	_, err = invoke(t, "", "profile", "retire", q.Name, "--move-data", filepath.Join(t.TempDir(), "y"))
	require.ErrorIs(t, err, ErrInput, "a key file inside the data directory")
	assert.Contains(t, profileNames(t), q.Name)
	assert.DirExists(t, q.Connection.Database)
}
