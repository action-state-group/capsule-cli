package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoRoot resolves the capsule-cli checkout root from this test file's own
// path, so the test works regardless of the caller's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// gitRepoWithTwoCommits creates a throwaway repo with a base and a head
// commit, so pr-capsule-request.sh's diff/timestamp derivation has real git
// state to work against without depending on this checkout's own history.
func gitRepoWithTwoCommits(t *testing.T) (dir, baseSHA, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, e := cmd.CombinedOutput()
		require.NoError(t, e, "git %v: %s", args, out)
		return string(out)
	}
	run("init", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o600))
	run("add", "f.txt")
	run("commit", "-m", "base")
	baseSHA = trimSHA(run("rev-parse", "HEAD"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\nhead\n"), 0o600))
	run("add", "f.txt")
	run("commit", "-m", "head")
	headSHA = trimSHA(run("rev-parse", "HEAD"))
	return dir, baseSHA, headSHA
}

func trimSHA(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// runPRCapsuleRequest executes the real scripts/pr-capsule-request.sh (not a
// reimplementation) and returns the request JSON it wrote. This is the
// tripwire: if the script's field names or the capsule.Model/top-level model
// split ever drift from what parseRequest/seal below actually accept, this
// test fails — a bash script has no compiler to catch that drift on its own.
func runPRCapsuleRequest(t *testing.T, env map[string]string) []byte {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "pr-capsule-request.sh")
	output := filepath.Join(t.TempDir(), "request.json")
	env["CAPSULE_REQUEST_OUTPUT"] = output
	cmd := exec.Command("bash", script)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, e := cmd.CombinedOutput()
	require.NoError(t, e, "pr-capsule-request.sh: %s", out)
	b, e := os.ReadFile(output)
	require.NoError(t, e)
	return b
}

func TestPRCapsuleRequestShapeHuman(t *testing.T) {
	dir, base, head := gitRepoWithTwoCommits(t)
	raw := runPRCapsuleRequest(t, map[string]string{
		"CAPSULE_PR_REPO":      "octo-org/example-repo",
		"CAPSULE_PR_NUMBER":    "42",
		"CAPSULE_PR_HEAD_SHA":  head,
		"CAPSULE_PR_BASE_SHA":  base,
		"CAPSULE_PR_AUTHOR":    "octocat",
		"CAPSULE_CI_JOBS_JSON": `{"quality":"success","test":"success"}`,
		"CAPSULE_PR_REPO_DIR":  dir,
	})

	r, e := parseRequest(raw)
	require.NoError(t, e, "capsule-cli's own decoder rejected the script's request shape")

	_, private, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	record, e := seal(r, private)
	require.NoError(t, e)
	assert.NotEmpty(t, record.CapsuleID)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(record.Capsule, &payload))
	assert.NotContains(t, payload, "model_attestation", "a PR with no capsule-declare block must not carry a model")
	assurance, _ := payload["assurance"].(map[string]any)
	assert.Equal(t, "self_attested", assurance["attestation_mode"])
	refs, _ := payload["references"].([]any)
	assert.Len(t, refs, 2, "diff_digest and ci_result_digest, no prompt_digest without a declared prompt")
}

func TestPRCapsuleRequestShapeAgentDeclared(t *testing.T) {
	dir, base, head := gitRepoWithTwoCommits(t)
	promptDigest := hex.EncodeToString(make([]byte, 32)) // 64 zero-hex chars; format is all this script checks
	body := "Adds a thing.\n\n<!-- capsule-declare\nagent_provider: anthropic\nagent_model: claude-sonnet-5\nprompt_digest_sha256: " + promptDigest + "\n-->\n"
	raw := runPRCapsuleRequest(t, map[string]string{
		"CAPSULE_PR_REPO":      "octo-org/example-repo",
		"CAPSULE_PR_NUMBER":    "43",
		"CAPSULE_PR_HEAD_SHA":  head,
		"CAPSULE_PR_BASE_SHA":  base,
		"CAPSULE_PR_AUTHOR":    "claude-bot",
		"CAPSULE_PR_BODY":      body,
		"CAPSULE_CI_JOBS_JSON": `{"quality":"success","test":"success"}`,
		"CAPSULE_PR_REPO_DIR":  dir,
	})

	// The request must place the declared model at the top-level "model" key,
	// never nested under "capsule": capsule-emit-go's Seal() rejects a
	// request whose capsule.Model is set directly (sign.go), so a script that
	// nested it there would fail this line, not silently ship a wrong shape.
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Contains(t, decoded, "model")
	capsuleField, _ := decoded["capsule"].(map[string]any)
	assert.NotContains(t, capsuleField, "Model")

	r, e := parseRequest(raw)
	require.NoError(t, e)

	_, private, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	record, e := seal(r, private)
	require.NoError(t, e)
	assert.NotEmpty(t, record.CapsuleID)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(record.Capsule, &payload))
	attestation, ok := payload["model_attestation"].(map[string]any)
	require.True(t, ok, "declared agent_provider/agent_model must surface as model_attestation")
	assert.Equal(t, "anthropic", attestation["provider"])
	assert.Equal(t, "claude-sonnet-5", attestation["model_id"])
	assurance, _ := payload["assurance"].(map[string]any)
	assert.Equal(t, "self_attested", assurance["attestation_mode"], "a declared model with no compute attestation is still self-attested, not a stronger grade")
	refs, _ := payload["references"].([]any)
	assert.Len(t, refs, 3, "diff_digest, ci_result_digest, and prompt_digest")
}

// TestPRCapsuleRequestRejectsMalformedPromptDigest locks the script's own
// validation: a prompt_digest_sha256 that is not 64 lowercase hex chars must
// be dropped, not passed through to a Reference the emitter would reject or
// silently accept as free text.
func TestPRCapsuleRequestRejectsMalformedPromptDigest(t *testing.T) {
	dir, base, head := gitRepoWithTwoCommits(t)
	body := "<!-- capsule-declare\nagent_provider: anthropic\nagent_model: claude-sonnet-5\nprompt_digest_sha256: not-a-digest\n-->\n"
	raw := runPRCapsuleRequest(t, map[string]string{
		"CAPSULE_PR_REPO":      "octo-org/example-repo",
		"CAPSULE_PR_NUMBER":    "44",
		"CAPSULE_PR_HEAD_SHA":  head,
		"CAPSULE_PR_BASE_SHA":  base,
		"CAPSULE_PR_AUTHOR":    "claude-bot",
		"CAPSULE_PR_BODY":      body,
		"CAPSULE_CI_JOBS_JSON": `{"quality":"success"}`,
		"CAPSULE_PR_REPO_DIR":  dir,
	})

	r, e := parseRequest(raw)
	require.NoError(t, e)
	_, private, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	record, e := seal(r, private)
	require.NoError(t, e)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(record.Capsule, &payload))
	refs, _ := payload["references"].([]any)
	assert.Len(t, refs, 2, "the malformed prompt digest must be dropped, leaving only diff_digest and ci_result_digest")
}
