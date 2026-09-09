package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteSDKOnlyStorage(t *testing.T) {
	for _, facility := range []string{"artifacts", "cll", "both"} {
		t.Run(facility, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			p, key := profileFixture(t)
			p.Type = "sqlite"
			p.Connection.Database = filepath.Join(t.TempDir(), "sdk.db")
			expected := []string{}
			if facility != "cll" {
				expected = append(expected, "capsule_store_artifacts", "capsule_store_capsules")
			} else {
				p.Namespace = ""
			}
			if facility != "artifacts" {
				expected = append(expected, "cll_entries", "cll_meta", "cll_nodes", "cll_witnesses")
			} else {
				p.LogID = ""
			}
			// Provision through SDKs without any CLI initialization or metadata.
			db, coordinate, err := connection(p)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			if p.Namespace != "" {
				keys, err := parseKeys(p.TrustedKeys)
				require.NoError(t, err)
				store, err := newArtifactStore(p, db, keys)
				require.NoError(t, err)
				require.NoError(t, store.Init(t.Context()))
			}
			if p.LogID != "" {
				require.NoError(t, initLog(t.Context(), p, coordinate, p.LogID))
			}
			require.NoError(t, saveProfile(p, false))
			profilePath, err := profilePath(p.Name)
			require.NoError(t, err)
			before, err := os.ReadFile(profilePath)
			require.NoError(t, err)
			if facility == "both" {
				target, err := openTarget(t.Context(), p, usePublication)
				require.NoError(t, err)
				request, err := parseRequest(requestFixture(t))
				require.NoError(t, err)
				first, err := target.publish(t.Context(), request, key)
				require.NoError(t, err)
				require.NoError(t, target.close())
				target, err = openTarget(t.Context(), p, usePublication)
				require.NoError(t, err)
				second, err := target.publish(t.Context(), request, key)
				require.NoError(t, err)
				assert.Equal(t, first, second)
				require.NoError(t, target.close())
				_, err = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
				require.NoError(t, err)
			}
			for range 2 {
				out, err := invoke(t, "", "store", "init", "--profile", p.Name)
				require.NoError(t, err)
				assert.NotContains(t, out, "store_id")
			}
			after, err := os.ReadFile(profilePath)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			rows, err := db.QueryContext(t.Context(), "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
			require.NoError(t, err)
			var tables []string
			for rows.Next() {
				var name string
				require.NoError(t, rows.Scan(&name))
				tables = append(tables, name)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			assert.ElementsMatch(t, expected, tables)
		})
	}
}

func TestRetiredStoreIDFieldRejected(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	path, err := profilePath(p.Name)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	raw = append(raw, []byte("\nstore_id: obsolete-cli-identity\n")...)
	require.NoError(t, os.WriteFile(path, raw, 0600))
	// No backward-compatible shim: the retired field is unknown and fails closed.
	_, err = loadProfile(p.Name)
	require.ErrorIs(t, err, ErrInput)
}
