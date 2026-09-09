package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-emit-go/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLLOnlyAppendRejectsUntrustedProducer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, key := profileFixture(t)
	p.Namespace, p.TrustedKeys = "", nil
	require.NoError(t, saveProfile(p, false))
	request, err := parseRequest(requestFixture(t))
	require.NoError(t, err)
	record, err := seal(request, key)
	require.NoError(t, err)
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(t, atomicFile(path, raw, false))
	_, err = invoke(t, "", "cll", "append", "--profile", p.Name, "--capsule", path)
	require.ErrorIs(t, err, artifact.ErrUntrustedSigner)
}

func TestOptionalProfileFacilities(t *testing.T) {
	p, _ := profileFixture(t)
	p.LogID = ""
	require.NoError(t, p.validate())
	_, err := openTarget(t.Context(), p, useCLLRead)
	require.ErrorIs(t, err, ErrInput)
	_, err = openTarget(t.Context(), p, usePublication)
	require.ErrorIs(t, err, ErrInput)
	p.Namespace, p.LogID = "", "log-only"
	require.NoError(t, p.validate())
	_, err = openTarget(t.Context(), p, useArtifacts)
	require.ErrorIs(t, err, ErrInput)
	_, err = openTarget(t.Context(), p, usePublication)
	require.ErrorIs(t, err, ErrInput)
	p.ReadOnly = true
	_, err = openTarget(t.Context(), p, useCLL)
	require.ErrorIs(t, err, ErrReadOnlyCLL)
	p.LogID = ""
	require.ErrorIs(t, p.validate(), ErrInput)
}

func TestCreateSingleFacilityProfiles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tt := range []struct{ name, namespace, log string }{
		{"artifacts", "artifacts", ""}, {"ledger", "", "ledger"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := invoke(t, "", "profile", "create", "--name", tt.name,
				"--mysql-host", "127.0.0.1", "--mysql-database", "test",
				"--namespace", tt.namespace, "--log-id", tt.log)
			require.NoError(t, err)
			p, err := loadProfile(tt.name)
			require.NoError(t, err)
			assert.Equal(t, tt.namespace, p.Namespace)
			assert.Equal(t, tt.log, p.LogID)
		})
	}
}
