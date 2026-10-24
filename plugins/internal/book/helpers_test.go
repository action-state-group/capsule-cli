package book

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// The book package's test helpers. Until the plugins split (capsule-cli#326)
// they were shared with the deal tests; each package now keeps the ones it
// uses.

func profileFixture(t *testing.T) (cli.Profile, ed25519.PrivateKey) {
	t.Helper()
	public, private, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	p := cli.Profile{Name: "test", Type: "mysql", LogID: "test-log", Namespace: "capsule", Operator: "test-operator"}
	p.Connection.Host = "127.0.0.1"
	p.Connection.Port = 3306
	p.Connection.Database = "capsule_cli_test"
	p.Connection.TLS = "false"
	p.Credentials.Username = "root"
	p.Signing.Value = hex.EncodeToString(private.Seed())
	p.TrustedKeys = []string{hex.EncodeToString(public)}
	p.Checkpoint.Signing = p.Signing
	p.Checkpoint.TrustedKeys = p.TrustedKeys
	return p, private
}

func requestFixture(t *testing.T) []byte {
	t.Helper()
	return []byte(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"test-action","ActionType":"fyi","Operator":"test-operator","Developer":"test-developer","Timestamp":"2026-09-08T00:00:00Z"},"payload":{"a": 1},"agent_output":{"ok":true}}`)
}

func invoke(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	c := testCommand()
	var out bytes.Buffer
	c.SetIn(strings.NewReader(input))
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	e := c.ExecuteContext(t.Context())
	return out.String(), e
}

// testCommand is the capsulectl tree as a user reaches it with the book
// plugin installed, in one process: the core commands; the book plugin's
// own commands at the top level, where its binary has them (replacing the
// transitional aliases); and `book ...` as the core dispatches it.
func testCommand() *cobra.Command {
	book := &cobra.Command{Use: cli.BookPluginName}
	book.AddCommand(append(BookCommands(), BookAliases()...)...)
	return cli.NewCommandWith(append(BookCommands(), book)...)
}

func writeDirectory(t *testing.T, rows ...map[string]interface{}) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "witnesses.json")
	data, err := json.Marshal(map[string]interface{}{"directory_version": "1", "witnesses": rows})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func verifyBundleOutput(t *testing.T, path string) (map[string]interface{}, error) {
	t.Helper()
	out, err := invoke(t, "", "verify", "--bundle", path)
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	return result, err
}

func status(result map[string]interface{}, claim string) string {
	return result[claim].(map[string]interface{})["status"].(string)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

func writeJSON(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// coreAssetsDir holds the core's assets the report page is built from.
const coreAssetsDir = "../../../cli/assets"

// installed is capsulectl with the book plugin installed, as an operator has
// it: the core binary in dir, and capsulectl-book in dir/plugins, a trusted
// plugin root. They are built once per test binary.
var installed struct {
	once sync.Once
	dir  string
	err  error
	out  string
}

// installedCapsulectl builds the binaries once and returns the directory
// holding capsulectl. It points CAPSULECTL_PLUGIN_ROOTS at the plugins, so a
// capsulectl the test runs finds them.
func installedCapsulectl(t *testing.T) string {
	t.Helper()
	installed.once.Do(func() {
		installed.dir, installed.err = os.MkdirTemp("", "capsulectl-bin-")
		if installed.err != nil {
			return
		}
		plugins := filepath.Join(installed.dir, "plugins")
		if installed.err = os.Mkdir(plugins, 0o700); installed.err != nil {
			return
		}
		for path, pkg := range map[string]string{
			filepath.Join(installed.dir, "capsulectl"): "github.com/action-state-group/capsule-cli/cmd/capsulectl",
			filepath.Join(plugins, "capsulectl-book"):  "github.com/action-state-group/capsule-cli/plugins/cmd/capsulectl-book",
		} {
			out, err := exec.Command("go", "build", "-o", path, pkg).CombinedOutput()
			if err != nil {
				installed.err, installed.out = err, string(out)
				return
			}
		}
	})
	require.NoError(t, installed.err, installed.out)
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", filepath.Join(installed.dir, "plugins"))
	return installed.dir
}

// TestMain runs the tests as capsulectl-book runs: the book backend
// installed in the core tree. It gives the package a private HOME, so a test
// that sets no HOME of its own never touches the real one, and removes the
// built binaries afterwards.
func TestMain(m *testing.M) {
	keepGoCaches()
	home, err := os.MkdirTemp("", "capsulectl-home-")
	if err == nil {
		err = os.Chmod(home, 0o755)
	}
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	cli.SetBookBackend(BookBackend{})
	code := m.Run()
	if installed.dir != "" {
		_ = os.RemoveAll(installed.dir)
	}
	_ = os.RemoveAll(home)
	os.Exit(code)
}

// keepGoCaches pins the go env file and the Go build and module caches, as
// go resolves them under the real HOME, before a test HOME replaces it.
func keepGoCaches() {
	out, err := exec.Command("go", "env", "-json", "GOENV", "GOPATH", "GOCACHE", "GOMODCACHE").Output()
	var env map[string]string
	if err != nil || json.Unmarshal(out, &env) != nil {
		return
	}
	for name, value := range env {
		if value != "" && os.Getenv(name) == "" {
			os.Setenv(name, value)
		}
	}
}
