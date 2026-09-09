package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMySQLPublishWithoutJournalOrOperationKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := mysqlProfile(t)
	target, err := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.close()) })
	p.StoreID = target.storeID
	require.NoError(t, saveProfile(p, false))
	var tables int
	require.NoError(t, target.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='capsule_cli_operations'").Scan(&tables))
	assert.Zero(t, tables)
	path := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(path, requestFixture(t), 0600))
	first, err := invoke(t, "", "publish", "--profile", p.Name, "--request", path)
	require.NoError(t, err)
	second, err := invoke(t, "", "publish", "--profile", p.Name, "--request", path)
	require.NoError(t, err)
	assert.JSONEq(t, first, second)
	assert.NotContains(t, first, "idempotency_key")
	entries, err := target.log.ScanEntries(t.Context(), 0, 10)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}
