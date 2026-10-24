package book

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each plugin binary answers the handshake with the name its launcher's
// filename carries, and names its commands as an operator reaches them,
// `capsulectl deal history`, never by the launcher's own name.
func TestThePluginBinariesNameThemselves(t *testing.T) {
	for _, tc := range []struct {
		meta    cli.PluginMetadata
		cmds    []string
		command string
	}{
		{BookMetadata, []string{"close"}, "capsulectl book close"},
	} {
		var root = cli.NewPluginCommand(tc.meta, BookCommands()...)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"cli-plugin-metadata"})
		require.NoError(t, root.Execute())
		var meta map[string]any
		require.NoError(t, json.Unmarshal(out.Bytes(), &meta))
		assert.Equal(t, tc.meta.Name, meta["name"], "the launcher is capsulectl-%s", tc.meta.Name)
		assert.Equal(t, "cli-plugin/v1", meta["plugin_api"])

		found, _, err := root.Find(tc.cmds)
		require.NoError(t, err)
		assert.Equal(t, tc.command, found.CommandPath())
	}
}
