package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// classify: pure functions, no filesystem
// ---------------------------------------------------------------------------

func TestIsDenyFile(t *testing.T) {
	cases := []struct {
		name string
		deny bool
	}{
		{".env", true}, {".env.production", true}, {"id_rsa", true}, {"id_ed25519.pub", true},
		{"server.pem", true}, {"tls.key", true}, {"credentials.json", true}, {"CREDENTIALS", true},
		{"my_secret_config.yaml", true}, {"api_token.json", true}, {".netrc", true},
		{"otel-collector.yaml", false}, {"mcp.json", false}, {"go.mod", false}, {"Dockerfile", false},
	}
	for _, tc := range cases {
		deny, _ := isDenyFile(tc.name, nil)
		assert.Equal(t, tc.deny, deny, "isDenyFile(%q)", tc.name)
	}
}

func TestIsDenyFileHonorsScopeExtras(t *testing.T) {
	deny, reason := isDenyFile("internal-only.yaml", []string{"internal-*.yaml"})
	assert.True(t, deny)
	assert.Contains(t, reason, "--scope deny pattern")
}

func TestIsAllowFile(t *testing.T) {
	assert.True(t, isAllowFile("otel-collector.yaml"))
	assert.True(t, isAllowFile("mcp.json"))
	assert.True(t, isAllowFile("go.mod"))
	assert.True(t, isAllowFile("Dockerfile"))
	assert.False(t, isAllowFile("README.md"))
	assert.False(t, isAllowFile("binary.exe"))
}

func TestDenyWinsOverAllowExtension(t *testing.T) {
	// A secret with a config-shaped extension is still denied -- deny
	// always wins, checked before allow in scanRoot.
	deny, _ := isDenyFile(".env.yaml", nil)
	assert.True(t, deny)
}

func TestClassifyKind(t *testing.T) {
	assert.Equal(t, "otel-config", classifyKind("config/otel-collector.yaml"))
	assert.Equal(t, "mcp-manifest", classifyKind("servers/mcp.json"))
	assert.Equal(t, "gateway-config", classifyKind("agentgateway/config.yaml"))
	assert.Equal(t, "ci-config", classifyKind(".github/workflows/python.yml"))
	assert.Equal(t, "repo-config", classifyKind("go.mod"))
	assert.Equal(t, "unknown-config", classifyKind("random.json"))
}

// ---------------------------------------------------------------------------
// scanRoot: real filesystem, tmp dirs only
// ---------------------------------------------------------------------------

func TestScanRootDigestsAnAllowedConfigFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "otel-collector.yaml"), []byte("receivers: {}\n"), 0o644))

	files, err := scanRoot(root, nil)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, dispositionScanned, files[0].Disposition)
	assert.Equal(t, "SHA-256", files[0].DigestAlg)
	assert.NotEmpty(t, files[0].Digest)
	assert.Equal(t, "otel-config", files[0].Kind)
}

func TestScanRootRecordsUnknownFilesByPathOnly(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# hello\n"), 0o644))

	files, err := scanRoot(root, nil)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, dispositionPresentNotRead, files[0].Disposition)
	assert.Empty(t, files[0].Digest)
}

func TestScanRootPrunesDenyDirsEntirely(t *testing.T) {
	root := t.TempDir()
	sshDir := filepath.Join(root, ".ssh")
	require.NoError(t, os.MkdirAll(sshDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sshDir, "config.yaml"), []byte("x: 1\n"), 0o644))

	files, err := scanRoot(root, nil)
	require.NoError(t, err)
	assert.Empty(t, files, "nothing under a denied directory should appear in the inventory at all")
}

// TestScanRootPrunesVendoredAndBuildDirs is the noise-pruning regression
// case found on the 2026-09-22 own-environment live dry run: an un-pruned
// Rust target/ directory alone produced ~1,270 rows, several mislabeled
// "mcp-manifest" purely because a Cargo .fingerprint file name embedded a
// dependency crate name (rmcp) containing the substring "mcp". These
// directories hold vendored/third-party code and build output, never our
// own OTel/MCP/gateway/CI/repo config, so they are pruned the same way
// (SkipDir, no rows) as the security-motivated entries above.
func TestScanRootPrunesVendoredAndBuildDirs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".venv", ".venv-redteam", "node_modules", "target", "__pycache__"} {
		full := filepath.Join(root, dir, "nested")
		require.NoError(t, os.MkdirAll(full, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(full, "config.json"), []byte(`{"tools":[]}`), 0o644))
	}
	// A real config file at the scope root, to prove pruning does not also
	// eat legitimate siblings.
	require.NoError(t, os.WriteFile(filepath.Join(root, "otel-collector.yaml"), []byte("receivers: {}\n"), 0o644))

	files, err := scanRoot(root, nil)
	require.NoError(t, err)
	require.Len(t, files, 1, "only the real config file should survive; every vendored/build dir is pruned")
	assert.Equal(t, "otel-collector.yaml", files[0].Path)
}

func TestScanRootExcludesSymlinkEscapingRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "otel.yaml")
	require.NoError(t, os.WriteFile(target, []byte("x: 1\n"), 0o644))
	require.NoError(t, os.Symlink(target, filepath.Join(root, "otel.yaml")))

	files, err := scanRoot(root, nil)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, dispositionExcludedSymlink, files[0].Disposition)
}

// TestScanRootNeverOpensASecretBehindABenignlyNamedSymlink is the symlink
// variant of the adversarial proof above: a real secret ("credentials.yaml",
// 0000 permissions) lives inside the scope root, and a symlink with an
// innocuous name points at it. The deny/allow check must classify by the
// RESOLVED target's basename, not the link's own name -- otherwise a link
// named e.g. "config.yaml" would sail past isDenyFile on its own name and
// digestFile would open the real secret through the link.
//
// R4 mutant: pass d.Name() instead of the resolved checkName into
// isDenyFile/isAllowFile (i.e. revert the discover.go fix) and this test
// goes red -- the link resolves cleanly (it does not escape the root), the
// deny check misses on the link's own name "config.yaml", and digestFile
// hits the 0000 target with a permission-denied error.
func TestScanRootNeverOpensASecretBehindABenignlyNamedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	root := t.TempDir()
	secret := filepath.Join(root, "credentials.yaml")
	require.NoError(t, os.WriteFile(secret, []byte("api_key: super-secret-value\n"), 0o600))
	require.NoError(t, os.Chmod(secret, 0o000))
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })
	link := filepath.Join(root, "config.yaml")
	require.NoError(t, os.Symlink(secret, link))

	files, err := scanRoot(root, nil)
	require.NoError(t, err, "a denied secret reached through a symlink must never fail the scan")
	require.Len(t, files, 2)
	byPath := map[string]discoveredFile{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	assert.Equal(t, dispositionExcludedSecret, byPath["config.yaml"].Disposition,
		"classified by the resolved target's name, not the link's own name")
	assert.Empty(t, byPath["config.yaml"].Digest)
}

// TestScanRootNeverOpensADeniedSecret is the adversarial proof QUEUE_PROTOCOL
// §7a requires: a planted fake secret is set to mode 0000 (unreadable, even
// by its owner) AND given a config-shaped ".yaml" extension so it would also
// pass isAllowFile -- isolating the deny check as the only thing standing
// between it and digestFile. If scanRoot's deny check ever let this
// candidate reach digestFile, the resulting os.Open would fail with a
// permission error and the row would come back disposition "error" -- not
// "excluded_secret" with an empty digest. Passing here is proof the file was
// excluded BEFORE any open() was attempted, not merely that reading it
// happened to fail.
//
// R4 mutant: comment out the isDenyFile check in scanRoot (fall through to
// isAllowFile/digestFile for every file) and this test goes red --
// disposition flips to "error" (digestFile's os.Open hits the 0000
// permission bits), proving the deny check is what this test bites on.
func TestScanRootNeverOpensADeniedSecret(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	root := t.TempDir()
	secret := filepath.Join(root, "credentials.yaml")
	require.NoError(t, os.WriteFile(secret, []byte("api_key: super-secret-value\n"), 0o600))
	require.NoError(t, os.Chmod(secret, 0o000))
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) }) // let TempDir cleanup remove it

	files, err := scanRoot(root, nil)
	require.NoError(t, err, "a denied secret must never cause the scan itself to fail")
	require.Len(t, files, 1)
	assert.Equal(t, dispositionExcludedSecret, files[0].Disposition)
	assert.Empty(t, files[0].Digest, "no digest means the file's bytes were never read")
	assert.Contains(t, files[0].Reason, "deny pattern")
}

func TestScanRootRefusesUnreadableAllowedFileAsError(t *testing.T) {
	// Contrast case for the adversarial test above: an ALLOW-matched file
	// that cannot be opened surfaces as disposition "error", not silently
	// dropped and not misreported as "excluded_secret". This is what
	// TestScanRootNeverOpensADeniedSecret's mutant flips into.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	root := t.TempDir()
	cfg := filepath.Join(root, "otel-collector.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("receivers: {}\n"), 0o600))
	require.NoError(t, os.Chmod(cfg, 0o000))
	t.Cleanup(func() { _ = os.Chmod(cfg, 0o600) })

	files, err := scanRoot(root, nil)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, dispositionError, files[0].Disposition)
}

func TestLoadScopeRequiresAFile(t *testing.T) {
	_, err := loadScope("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--scope is required")
}

func TestLoadScopeRequiresAtLeastOneRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scope.yaml")
	require.NoError(t, os.WriteFile(path, []byte("roots: []\n"), 0o644))
	_, err := loadScope(path)
	require.Error(t, err)
}

func TestLoadScopeParsesRootsAndDeny(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scope.yaml")
	require.NoError(t, os.WriteFile(path, []byte("roots:\n  - /a\n  - /b\ndeny:\n  - \"internal-*.yaml\"\n"), 0o644))
	scope, err := loadScope(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"/a", "/b"}, scope.Roots)
	assert.Equal(t, []string{"internal-*.yaml"}, scope.Deny)
}

// ---------------------------------------------------------------------------
// End-to-end: discover command through the CLI, including the sealed capsule
// ---------------------------------------------------------------------------

func TestDiscoverCommandSealsAScanCapsule(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Name = "discover-test"
	require.NoError(t, saveProfile(p, false))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "otel-collector.yaml"), []byte("receivers: {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=x\n"), 0o600))

	scopePath := filepath.Join(t.TempDir(), "scope.yaml")
	require.NoError(t, os.WriteFile(scopePath, []byte("roots:\n  - "+root+"\n"), 0o644))
	sealOutput := filepath.Join(t.TempDir(), "scan.json")

	out, err := invoke(t, "", "discover", "--profile", "discover-test", "--scope", scopePath,
		"--seal-output", sealOutput, "--format", "json")
	require.NoError(t, err)

	var result struct {
		Files  []discoveredFile `json:"files"`
		Sealed struct {
			CapsuleID string `json:"capsule_id"`
			Artifact  string `json:"artifact"`
		} `json:"sealed"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Len(t, result.Files, 2)
	assert.NotEmpty(t, result.Sealed.CapsuleID)
	assert.Equal(t, sealOutput, result.Sealed.Artifact)

	sealedBytes, err := os.ReadFile(sealOutput)
	require.NoError(t, err)
	assert.Contains(t, string(sealedBytes), result.Sealed.CapsuleID)

	// Cryptographic round-trip, not just "a capsule_id string is present":
	// the sealed artifact must actually verify against the profile's own
	// trusted key, through the CLI's own verify command.
	verifyOut, err := invoke(t, "", "verify", "--profile", "discover-test", "--capsule", sealOutput)
	require.NoError(t, err)
	assert.Contains(t, verifyOut, "passed")

	var byDisposition = map[string]int{}
	for _, f := range result.Files {
		byDisposition[f.Disposition]++
	}
	assert.Equal(t, 1, byDisposition[dispositionScanned])
	assert.Equal(t, 1, byDisposition[dispositionExcludedSecret])
}

func TestDiscoverCommandTableFormat(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Name = "discover-table-test"
	require.NoError(t, saveProfile(p, false))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "otel-collector.yaml"), []byte("receivers: {}\n"), 0o644))
	scopePath := filepath.Join(t.TempDir(), "scope.yaml")
	require.NoError(t, os.WriteFile(scopePath, []byte("roots:\n  - "+root+"\n"), 0o644))
	sealOutput := filepath.Join(t.TempDir(), "scan.json")

	out, err := invoke(t, "", "discover", "--profile", "discover-table-test", "--scope", scopePath,
		"--seal-output", sealOutput, "--format", "table")
	require.NoError(t, err)
	assert.Contains(t, out, "ROOT")
	assert.Contains(t, out, "DISPOSITION")
	assert.Contains(t, out, "otel-collector.yaml")
	assert.Contains(t, out, dispositionScanned)
	assert.Contains(t, out, "sealed: capsule_id=")
	// The table view must not degrade into the JSON envelope's field names.
	assert.NotContains(t, out, `"spec_version"`)
}

func TestDiscoverCommandRequiresScope(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Name = "discover-test2"
	require.NoError(t, saveProfile(p, false))

	_, err := invoke(t, "", "discover", "--profile", "discover-test2", "--seal-output", filepath.Join(t.TempDir(), "x.json"))
	require.Error(t, err)
}

func TestDiscoverEffectsBuildsBoundaryMapFromMCPManifest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Name = "discover-effects-test"
	require.NoError(t, saveProfile(p, false))

	root := t.TempDir()
	manifest := `{"tools":[
		{"name":"get_weather","annotations":{"readOnlyHint":true}},
		{"name":"book_flight","annotations":{"readOnlyHint":false}},
		{"name":"mystery_tool"}
	]}`
	require.NoError(t, os.WriteFile(filepath.Join(root, "mcp.json"), []byte(manifest), 0o644))

	scopePath := filepath.Join(t.TempDir(), "scope.yaml")
	require.NoError(t, os.WriteFile(scopePath, []byte("roots:\n  - "+root+"\n"), 0o644))
	sealOutput := filepath.Join(t.TempDir(), "effects.json")

	out, err := invoke(t, "", "discover", "--profile", "discover-effects-test", "--scope", scopePath,
		"--effects", "--seal-output", sealOutput, "--format", "json")
	require.NoError(t, err)

	var result struct {
		Effects []effectBoundary `json:"effects"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Len(t, result.Effects, 3)
	byName := map[string]effectBoundary{}
	for _, e := range result.Effects {
		byName[e.Surface] = e
	}
	assert.Equal(t, classificationObservation, byName["get_weather"].Classification)
	assert.Equal(t, classificationEffect, byName["book_flight"].Classification)
	assert.Equal(t, classificationEffect, byName["mystery_tool"].Classification, "no annotation present -- fail-safe default")
}
