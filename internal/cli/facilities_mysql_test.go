package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/action-state-group/cll-go/cll"
	"github.com/stretchr/testify/require"
)

func TestMySQLIndependentFacilitiesAndReadOnlyList(t *testing.T) {
	p, key := mysqlProfile(t)
	producerTrust := p.TrustedKeys
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	admin, _, err := connection(p)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	for _, name := range []string{"capsule_cli_artifacts_only", "capsule_cli_log_only"} {
		_, err = admin.Exec("CREATE DATABASE " + name)
		require.NoError(t, err)
		name := name
		t.Cleanup(func() { _, err := admin.Exec("DROP DATABASE " + name); require.NoError(t, err) })
	}
	p.Connection.Database, p.LogID = "capsule_cli_artifacts_only", ""
	artifacts, err := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, err)
	require.NotNil(t, artifacts.artifacts)
	require.Nil(t, artifacts.log)
	var count int
	require.NoError(t, artifacts.db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name LIKE 'cll_%'").Scan(&count))
	require.Zero(t, count)
	require.NoError(t, artifacts.close())

	p.Connection.Database, p.Namespace, p.LogID, p.TrustedKeys = "capsule_cli_log_only", "", "test-log", nil
	ledger, err := openTarget(t.Context(), p, useInitialization)
	require.NoError(t, err)
	require.Nil(t, ledger.artifacts)
	require.NoError(t, ledger.db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name LIKE 'capsule_store_%'").Scan(&count))
	require.Zero(t, count)
	_, err = ledger.log.Append(t.Context(), cll.AppendInput{Value: make([]byte, 32), AppendedAt: time.Now().UTC()})
	require.NoError(t, err)
	require.NoError(t, ledger.close())
	p.Checkpoint.Endpoint = "https://127.0.0.1:1"
	p.Checkpoint.PublicKey = p.Checkpoint.TrustedKeys[0]
	writer := p
	writer.TrustedKeys = producerTrust
	require.NoError(t, saveProfile(p, false))
	_, err = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, err)
	_, err = admin.Exec("CREATE USER 'capsule_cli_reader'@'%' IDENTIFIED BY 'isolated-reader'")
	require.NoError(t, err)
	t.Cleanup(func() { _, err := admin.Exec("DROP USER 'capsule_cli_reader'@'%'"); require.NoError(t, err) })
	_, err = admin.Exec("GRANT SELECT ON capsule_cli_log_only.* TO 'capsule_cli_reader'@'%'")
	require.NoError(t, err)
	p.Credentials.Username, p.Credentials.Password = "capsule_cli_reader", Secret{Value: "isolated-reader"}
	p.ReadOnly = true
	require.NoError(t, saveProfile(p, true))
	out, err := invoke(t, "", "cll", "list", "--profile", p.Name, "--after", "0", "--through", "1")
	require.NoError(t, err)
	require.Contains(t, out, `"sequence":1`)
	_, err = invoke(t, "", "cll", "checkpoint", "status", "--profile", p.Name, "--checkpoint", "1")
	require.NoError(t, err)
	_, err = openTarget(t.Context(), p, useInitialization)
	require.ErrorIs(t, err, ErrReadOnlyCLL)
	require.NoError(t, saveProfile(writer, true))
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	record, err := seal(request, key)
	require.NoError(t, err)
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(t, atomicFile(path, raw, false))
	_, err = invoke(t, "", "cll", "append", "--profile", writer.Name, "--capsule", path)
	require.NoError(t, err)
	require.NoError(t, admin.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='capsule_cli_log_only' AND table_name LIKE 'capsule_store_%'").Scan(&count))
	require.Zero(t, count)
	_, err = openTarget(t.Context(), p, useCLL)
	require.ErrorIs(t, err, ErrReadOnlyCLL)
}
