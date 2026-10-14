package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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

	id, payload := sealPRCapsuleRequest(t, raw)
	assert.NotEmpty(t, id)
	assert.NotContains(t, payload, "model_attestation", "a PR with no capsule-declare block must not carry a model")
	assurance, _ := payload["assurance"].(map[string]any)
	assert.Equal(t, "self_attested", assurance["attestation_mode"])
	refs, _ := payload["references"].([]any)
	assert.Len(t, refs, 2, "the diff and CI result, no prompt without a declared prompt")
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

	id, payload := sealPRCapsuleRequest(t, raw)
	assert.NotEmpty(t, id)
	attestation, ok := payload["model_attestation"].(map[string]any)
	require.True(t, ok, "declared agent_provider/agent_model must surface as model_attestation")
	assert.Equal(t, "anthropic", attestation["provider"])
	assert.Equal(t, "claude-sonnet-5", attestation["model_id"])
	assurance, _ := payload["assurance"].(map[string]any)
	assert.Equal(t, "self_attested", assurance["attestation_mode"], "a declared model with no compute attestation is still self-attested, not a stronger grade")
	refs, _ := payload["references"].([]any)
	assert.Len(t, refs, 3, "the diff, CI result and declared prompt")
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

	_, payload := sealPRCapsuleRequest(t, raw)
	refs, _ := payload["references"].([]any)
	assert.Len(t, refs, 2, "the malformed prompt digest must be dropped, leaving only the diff and CI result")
}

// prCapsuleBaseEnv is the head-capsule environment every profile test starts
// from; each test adds the variables its case is about.
func prCapsuleBaseEnv(dir, base, head string) map[string]string {
	return map[string]string{
		"CAPSULE_PR_REPO":      "octo-org/example-repo",
		"CAPSULE_PR_NUMBER":    "45",
		"CAPSULE_PR_HEAD_SHA":  head,
		"CAPSULE_PR_BASE_SHA":  base,
		"CAPSULE_PR_AUTHOR":    "octocat",
		"CAPSULE_CI_JOBS_JSON": `{"quality":"success","test":"success"}`,
		"CAPSULE_PR_REPO_DIR":  dir,
	}
}

// conformanceOutEnv names a directory that sealPRCapsuleRequest writes each
// sealed payload into, as <capsule_id>.json, for the registry-conformance CI
// job to check. Unset, nothing is written.
const conformanceOutEnv = "CAPSULE_CONFORMANCE_OUT"

// sealPRCapsuleRequest decodes and seals a request with capsule-cli's own
// parseRequest/seal and returns the sealed payload.
func sealPRCapsuleRequest(t *testing.T, raw []byte) (string, map[string]any) {
	t.Helper()
	r, e := parseRequest(raw)
	require.NoError(t, e, "capsule-cli's own decoder rejected the script's request shape")
	_, private, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	record, e := seal(r, private)
	require.NoError(t, e)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(record.Capsule, &payload))
	refs, _ := payload["references"].([]any)
	for i, ref := range refs {
		m, _ := ref.(map[string]any)
		assert.Equal(t, "acted_on", m["citation_purpose"], "references[%d]", i)
	}
	referenceRoles(t, payload)
	if dir := os.Getenv(conformanceOutEnv); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, record.CapsuleID+".json"), record.Capsule, 0o600))
	}
	return record.CapsuleID, payload
}

// profileRoleOrder is the order docs/PR-CAPSULE-PROFILE.md fixes for the
// references, absent entries left out.
var profileRoleOrder = []string{"diff_digest", "ci_result_digest", "review_digest", "prompt_digest"}

// referenceRoles reads each reference's role the way docs/PR-CAPSULE-PROFILE.md
// defines it, by type and position: the first application/json entry is the
// CI result and a second one is the review digest. It fails the test unless
// the roles follow profileRoleOrder with no role twice, and returns them in
// payload order with each role's digest.
func referenceRoles(t *testing.T, payload map[string]any) ([]string, map[string]string) {
	t.Helper()
	refs, _ := payload["references"].([]any)
	var order []string
	digests := map[string]string{}
	next := 0
	for i, ref := range refs {
		m, _ := ref.(map[string]any)
		var role string
		switch m["type"] {
		case "text/x-diff":
			role = "diff_digest"
		case "text/plain":
			role = "prompt_digest"
		case "application/json":
			role = "ci_result_digest"
			if _, seen := digests[role]; seen {
				role = "review_digest"
			}
		default:
			t.Fatalf("reference type %v is not in the PR profile", m["type"])
		}
		for next < len(profileRoleOrder) && profileRoleOrder[next] != role {
			next++
		}
		if next == len(profileRoleOrder) {
			t.Fatalf("references[%d] is %s, out of the profile's order %v", i, role, profileRoleOrder)
		}
		next++
		order = append(order, role)
		digests[role], _ = m["digest"].(string)
	}
	return order, digests
}

func referenceDigest(t *testing.T, payload map[string]any, role string) string {
	t.Helper()
	_, digests := referenceRoles(t, payload)
	return digests[role]
}

// TestPRCapsuleReferencesCiteActedOn seals the fullest capsule the profile
// defines, a decision capsule with reviews and a declared prompt, and pins
// all four roles in order. sealPRCapsuleRequest checks acted_on and the role
// order on every capsule the PR tests seal.
func TestPRCapsuleReferencesCiteActedOn(t *testing.T) {
	dir, base, head := gitRepoWithTwoCommits(t)
	promptDigest := hex.EncodeToString(make([]byte, 32))
	env := prDecisionEnv(dir, base, head)
	env["CAPSULE_PR_REVIEWS_JSON"] = `[{"reviewer":"reviewer-a","state":"APPROVED","commit_id":"` + head + `","submitted_at":"2026-10-02T10:00:00Z"}]`
	env["CAPSULE_PR_BODY"] = "<!-- capsule-declare\nprompt_digest_sha256: " + promptDigest + "\n-->\n"
	_, payload := sealPRCapsuleRequest(t, runPRCapsuleRequest(t, env))

	order, digests := referenceRoles(t, payload)
	assert.Equal(t, profileRoleOrder, order)
	assert.Equal(t, promptDigest, digests["prompt_digest"])
	ciSum := sha256.Sum256([]byte(`{"quality":"success","test":"success"}` + "\n"))
	assert.Equal(t, hex.EncodeToString(ciSum[:]), digests["ci_result_digest"], "the second entry must be the CI result, not the reviews")
}

func runPRCapsuleRequestFails(t *testing.T, env map[string]string) string {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "pr-capsule-request.sh"))
	cmd.Env = os.Environ()
	env["CAPSULE_REQUEST_OUTPUT"] = filepath.Join(t.TempDir(), "request.json")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, e := cmd.CombinedOutput()
	require.Error(t, e, "pr-capsule-request.sh accepted an invalid request: %s", out)
	return string(out)
}

// prDecisionEnv is the closed-event environment: a merge decided by a User.
func prDecisionEnv(dir, base, head string) map[string]string {
	env := prCapsuleBaseEnv(dir, base, head)
	env["CAPSULE_PR_MERGE_DECISION"] = "merged"
	env["CAPSULE_PR_DECIDED_AT"] = "2026-10-02T11:00:00Z"
	env["CAPSULE_PR_DECIDED_BY_TYPE"] = "User"
	return env
}

// TestPRCapsuleReviewDigest pins the profile's review_digest on the decision
// capsule: the logins are committed only as a digest, the API's ordering
// does not change it, only reviews submitted by the decision count (a later
// review and a pending one do not), and the recompute documented in
// docs/PR-CAPSULE-PROFILE.md reproduces it.
func TestPRCapsuleReviewDigest(t *testing.T) {
	dir, base, head := gitRepoWithTwoCommits(t)
	approved := `{"reviewer":"reviewer-b","state":"APPROVED","commit_id":"` + head + `","submitted_at":"2026-10-02T10:05:00Z"}`
	commented := `{"reviewer":"reviewer-a","state":"COMMENTED","commit_id":"` + head + `","submitted_at":"2026-10-02T10:00:00Z"}`
	reordered := `{"submitted_at":"2026-10-02T10:00:00Z","commit_id":"` + head + `","state":"COMMENTED","reviewer":"reviewer-a"}`
	afterMerge := `{"reviewer":"reviewer-c","state":"COMMENTED","commit_id":"` + head + `","submitted_at":"2026-10-02T12:00:00Z"}`
	pending := `{"reviewer":"reviewer-d","state":"PENDING","commit_id":"` + head + `","submitted_at":null}`

	sealWith := func(reviews string) (string, map[string]any, []byte) {
		env := prDecisionEnv(dir, base, head)
		env["CAPSULE_PR_REVIEWS_JSON"] = reviews
		raw := runPRCapsuleRequest(t, env)
		id, payload := sealPRCapsuleRequest(t, raw)
		return id, payload, raw
	}
	id, payload, raw := sealWith("[" + approved + "," + commented + "]")
	assert.NotContains(t, string(raw), "reviewer-a", "a reviewer's login must enter the request only as a digest")
	digest := referenceDigest(t, payload, "review_digest")
	require.Regexp(t, `^[0-9a-f]{64}$`, digest)

	reorderedID, _, _ := sealWith("[" + reordered + "," + approved + "]")
	assert.Equal(t, id, reorderedID, "the order reviews arrive in must not change the capsule")
	laterID, _, _ := sealWith("[" + approved + "," + afterMerge + "," + pending + "," + commented + "]")
	assert.Equal(t, id, laterID, "a review after the decision, or a pending one, must not change the capsule")

	canonical := `[{"commit_id":"` + head + `","reviewer":"reviewer-a","state":"COMMENTED","submitted_at":"2026-10-02T10:00:00Z"},` +
		`{"commit_id":"` + head + `","reviewer":"reviewer-b","state":"APPROVED","submitted_at":"2026-10-02T10:05:00Z"}]` + "\n"
	sum := sha256.Sum256([]byte(canonical))
	assert.Equal(t, hex.EncodeToString(sum[:]), digest, "the documented canonical form must reproduce review_digest")
}

// TestPRCapsuleMergeDecision seals the closed-event capsule: decide, the
// disposition mapped from the decision and the deciding account's type, and
// a confirms chain to the head capsule.
func TestPRCapsuleMergeDecision(t *testing.T) {
	dir, base, head := gitRepoWithTwoCommits(t)
	parentID, _ := sealPRCapsuleRequest(t, runPRCapsuleRequest(t, prCapsuleBaseEnv(dir, base, head)))

	cases := []struct {
		decision, byType, wantDecision, wantVerdict, wantApprover string
		wantHuman                                                 bool
	}{
		{"merged", "User", "accept", "executed", "human", true},
		{"merged", "Bot", "accept", "executed", "policy", false},
		{"closed", "User", "reject", "denied", "human", true},
		{"closed", "Bot", "reject", "denied", "policy", false},
	}
	for _, c := range cases {
		t.Run(c.decision+"-"+c.byType, func(t *testing.T) {
			env := prCapsuleBaseEnv(dir, base, head)
			env["CAPSULE_PR_MERGE_DECISION"] = c.decision
			env["CAPSULE_PR_DECIDED_AT"] = "2026-10-02T11:00:00Z"
			env["CAPSULE_PR_DECIDED_BY_TYPE"] = c.byType
			env["CAPSULE_PR_PARENT_CAPSULE_ID"] = parentID
			id, payload := sealPRCapsuleRequest(t, runPRCapsuleRequest(t, env))
			assert.NotEqual(t, parentID, id)
			assert.Equal(t, "decide", payload["action_type"])
			disposition, _ := payload["disposition"].(map[string]any)
			assert.Equal(t, c.wantDecision, disposition["decision"])
			assert.Equal(t, c.wantVerdict, disposition["verdict_class"])
			assert.Equal(t, c.wantApprover, disposition["approver"])
			assert.Equal(t, c.wantHuman, disposition["human_disposed"])
			chain, _ := payload["chain"].(map[string]any)
			assert.Equal(t, parentID, chain["parent_capsule_id"])
			assert.Equal(t, "confirms", chain["relation"])
			assurance, _ := payload["assurance"].(map[string]any)
			assert.Equal(t, "chained", assurance["ledger_mode"])
		})
	}
}

// TestPRCapsuleMergeDecisionRejectsBadInput keeps the script's own checks: a
// decision outside the profile, a missing decided-at, an unknown account type,
// a malformed parent id, and a parent id or reviews on a head capsule all
// fail before a request is written.
func TestPRCapsuleMergeDecisionRejectsBadInput(t *testing.T) {
	dir, base, head := gitRepoWithTwoCommits(t)
	parent := hex.EncodeToString(make([]byte, 32))
	decided := func(extra map[string]string) map[string]string {
		env := prCapsuleBaseEnv(dir, base, head)
		env["CAPSULE_PR_MERGE_DECISION"] = "merged"
		env["CAPSULE_PR_DECIDED_AT"] = "2026-10-02T11:00:00Z"
		env["CAPSULE_PR_DECIDED_BY_TYPE"] = "User"
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	assert.Contains(t, runPRCapsuleRequestFails(t, decided(map[string]string{"CAPSULE_PR_MERGE_DECISION": "approved"})), "merged or closed")
	assert.Contains(t, runPRCapsuleRequestFails(t, decided(map[string]string{"CAPSULE_PR_DECIDED_AT": ""})), "CAPSULE_PR_DECIDED_AT")
	assert.Contains(t, runPRCapsuleRequestFails(t, decided(map[string]string{"CAPSULE_PR_DECIDED_BY_TYPE": "Organization"})), "User or Bot")
	assert.Contains(t, runPRCapsuleRequestFails(t, decided(map[string]string{"CAPSULE_PR_PARENT_CAPSULE_ID": "not-hex"})), "64 lowercase hex")
	env := prCapsuleBaseEnv(dir, base, head)
	env["CAPSULE_PR_PARENT_CAPSULE_ID"] = parent
	assert.Contains(t, runPRCapsuleRequestFails(t, env), "only with CAPSULE_PR_MERGE_DECISION")
	env = prCapsuleBaseEnv(dir, base, head)
	env["CAPSULE_PR_REVIEWS_JSON"] = "[]"
	assert.Contains(t, runPRCapsuleRequestFails(t, env), "CAPSULE_PR_REVIEWS_JSON applies only with CAPSULE_PR_MERGE_DECISION")
}

// TestPRCapsuleIDIsSignerIndependent pins what lets a PR capsule sealed in CI
// be cited from another book: the capsule_id commits the payload, not the
// signing key, so publishing the same request under the book's own key gives
// the id the PR comment named.
func TestPRCapsuleIDIsSignerIndependent(t *testing.T) {
	dir, base, head := gitRepoWithTwoCommits(t)
	raw := runPRCapsuleRequest(t, prCapsuleBaseEnv(dir, base, head))
	first, _ := sealPRCapsuleRequest(t, raw)
	second, _ := sealPRCapsuleRequest(t, raw)
	assert.Equal(t, first, second)
}
