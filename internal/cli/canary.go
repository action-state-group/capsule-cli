package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The canary is a scripted DEMO deal run on a schedule, and a watcher that
// reads only the public witness.
//
// A deal profile's cadence log grows by one entry per tick whether or not
// anything happened (see deal_cadence.go), so on a profile with its own tick
// timer the public log advances even when every deal fails. `canary run`
// therefore runs on a profile of its own whose only ticker it is: it ticks
// only after the whole flow (open, note, check with its approval, report)
// has succeeded. A run that stops anywhere publishes nothing, the public log
// stops advancing, and `canary watch`, run anywhere, raises the alarm from
// the witness alone. Absence is the alarm.

// ErrAlarm is `canary watch` finding the log stale or its history rewritten.
var ErrAlarm = errors.New("canary alarm")

// errWitnessUnread is `canary watch` unable to read the witness at all: it
// cannot tell whether the log advanced, so it says so instead of alarming.
var errWitnessUnread = errors.New("witness not readable")

func canaryCommands() *cobra.Command {
	canary := &cobra.Command{Use: "canary", Short: "A scheduled DEMO deal, and a watcher that alarms from the public witness when it stops"}
	canary.AddCommand(canaryRunCommand(), canaryWatchCommand())
	return canary
}

// The scripted deal: a synthetic merchant, a demo purchase, never real money.
const (
	canaryOpen = `{"type":"purchase","demo":true,"channel":"web",
"intent":{"verbatim":"Canary: order the cat sticker in my cart at Example Stickers (demo)","asked":{"item":"cat sticker","quantity":1},"allowed":["pay"]},
"who":{"name":"Example Stickers","domain":"stickers.example"},
"terms":{"item":"cat sticker","quantity":1,"price_minor":627,"currency":"USD"},
"recourse":{"rail":"card","refundable":true}}`
	canaryCheck = `{"action":"pay","description":"Place order: 1 cat sticker, $6.27 total, saved card","amount_minor":627,"authorized_max_minor":627,
"who":{"name":"Example Stickers","domain":"stickers.example"},
"terms":{"item":"cat sticker","quantity":1,"price_minor":627,"currency":"USD"},
"recourse":{"rail":"card","refundable":true}}`
)

func canaryRunCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "run", Short: "Run the scripted DEMO deal on this canary profile and tick its cadence log only if every step succeeded", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		skill, _ := c.Flags().GetString("skill")
		wantVersion, _ := c.Flags().GetString("expect-version")
		wantSkill, _ := c.Flags().GetString("expect-skill-sha256")
		p, err := selected(c)
		if err != nil {
			return err
		}
		if p.Checkpoint.Endpoint == "" {
			return hint(ErrInput, "canary: this profile has no witness, so nothing the canary does can be seen from outside; nothing was sealed")
		}
		if wantVersion != "" && wantVersion != cliVersion {
			return hint(ErrConflict, fmt.Sprintf("canary: this binary is %s, expected %s; nothing was sealed", cliVersion, wantVersion))
		}
		skillSum := ""
		if skill != "" {
			data, err := os.ReadFile(skill)
			if err != nil {
				return hint(ErrInput, fmt.Sprintf("canary: the skill file %s cannot be read (%s); nothing was sealed", skill, describeFileError(err)))
			}
			sum := sha256.Sum256(data)
			skillSum = hex.EncodeToString(sum[:])
		}
		if wantSkill != "" {
			if skill == "" {
				return inputError("--expect-skill-sha256 needs --skill")
			}
			if !strings.EqualFold(wantSkill, skillSum) {
				return hint(ErrConflict, fmt.Sprintf("canary: %s has sha256 %s, expected %s; nothing was sealed", skill, skillSum, strings.ToLower(wantSkill)))
			}
		}

		work, err := os.MkdirTemp("", "capsulectl-canary-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		file := func(name, body string) (string, error) {
			path := filepath.Join(work, name)
			return path, os.WriteFile(path, []byte(body), 0o600)
		}
		step := func(args ...string) (map[string]any, error) {
			return canaryStep(c.Context(), p.Name, args...)
		}

		openPath, err := file("open.json", canaryOpen)
		if err != nil {
			return err
		}
		opened, err := step("open", "--input", openPath)
		if err != nil {
			return err
		}
		dealID, _ := opened["deal_id"].(string)
		if dealID == "" {
			return hint(ErrPartial, "canary: `deal open` gave no deal_id; stopped before the tick")
		}
		skillDetail := "no skill file given"
		if skillSum != "" {
			skillDetail = "skill sha256 " + skillSum
		}
		noteBody, _ := json.Marshal(map[string]string{
			"about":  "canary self-check",
			"source": "capsulectl canary run",
			"detail": fmt.Sprintf("binary %s (commit %s); %s", cliVersion, cliCommit, skillDetail),
		})
		notePath, err := file("note.json", string(noteBody))
		if err != nil {
			return err
		}
		if _, err = step("note", "--deal", dealID, "--kind", "evidence", "--input", notePath); err != nil {
			return err
		}
		checkPath, err := file("check.json", canaryCheck)
		if err != nil {
			return err
		}
		checked, err := step("check", "--deal", dealID, "--input", checkPath)
		if err != nil {
			return err
		}
		approvalID, _ := checked["approval_id"].(string)
		if checked["verdict"] != "pass" || approvalID == "" {
			return hint(ErrPartial, fmt.Sprintf("canary: the demo check did not pass (verdict %v), so no approval was sealed; stopped before the tick", checked["verdict"]))
		}
		if _, err = step("report", "--deal", dealID); err != nil {
			return err
		}
		ticked, err := step("tick")
		if err != nil {
			return err
		}
		out := map[string]any{
			"canary":         "ok",
			"deal_id":        dealID,
			"check_id":       checked["check_id"],
			"approval_id":    approvalID,
			"binary_version": cliVersion,
			"binary_commit":  cliCommit,
			"cadence_log":    ticked["cadence_log"],
			"tick":           ticked["state"],
		}
		if skillSum != "" {
			out["skill_sha256"] = skillSum
		}
		for _, k := range []string{"checkpoint", "due", "delivered", "pending", "witness"} {
			if v, ok := ticked[k]; ok {
				out[k] = v
			}
		}
		return output(c, out)
	}}
	cmd.Flags().String("skill", "", "The SKILL.md the agent loads; its sha256 is sealed in the run, and a missing file stops the run")
	cmd.Flags().String("expect-version", "", "Stop before sealing unless this binary reports exactly this version")
	cmd.Flags().String("expect-skill-sha256", "", "Stop before sealing unless --skill has exactly this sha256")
	return cmd
}

// canaryStep runs one `deal` verb on the profile exactly as an agent would,
// and returns its JSON result. A failing verb stops the run before the tick
// and names the verb, with the verb's own (already non-secret) message.
func canaryStep(ctx context.Context, profile string, args ...string) (map[string]any, error) {
	root := NewCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(append([]string{"--profile", profile, "deal"}, args...))
	if err := root.ExecuteContext(ctx); err != nil {
		class := ErrPartial
		for _, known := range []error{ErrInput, ErrConflict, ErrPending, ErrPartial} {
			if errors.Is(err, known) {
				class = known
				break
			}
		}
		return nil, hint(class, fmt.Sprintf("canary: `deal %s` failed: %s; stopped before the tick", args[0], SafeError(err)))
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return nil, hint(ErrPartial, fmt.Sprintf("canary: `deal %s` printed no result object; stopped before the tick", args[0]))
	}
	return result, nil
}

func describeFileError(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "no such file"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	default:
		return "unreadable"
	}
}

// witnessCheckpoint is the witness's read-only answer for one log:
// GET {witness}/checkpoints/{log_id}, the last checkpoint it accepted.
type witnessCheckpoint struct {
	LogID           string            `json:"log_id"`
	MMRSize         uint64            `json:"mmr_size"`
	Root            string            `json:"root"`
	PrevSize        uint64            `json:"prev_size"`
	PrevRoot        string            `json:"prev_root"`
	KeyID           string            `json:"key_id"`
	Timestamp       string            `json:"timestamp"`
	Equivocations   []json.RawMessage `json:"equivocations"`
	ContinuityGrade string            `json:"continuity_grade"`
}

// canaryObservation is what `canary watch` keeps between runs.
type canaryObservation struct {
	LogID     string `json:"log_id"`
	MMRSize   uint64 `json:"mmr_size"`
	Root      string `json:"root"`
	KeyID     string `json:"key_id"`
	Timestamp string `json:"timestamp"`
	// SizeSince is when this watcher first saw the log at MMRSize.
	SizeSince string `json:"size_since"`
}

var canaryHTTP = canaryClient(nil)

// canaryClient reads the witness with transport (nil: the default), and
// never follows a redirect off HTTPS: an answer about a public log must come
// over the same kind of channel it was asked on.
func canaryClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !witnessURLAllowed(req.URL) {
				return errors.New("redirected to a non-HTTPS URL")
			}
			return nil
		},
	}
}

// witnessURLAllowed: https, or plain http to a loopback address (a witness
// run on this machine, as the tests do).
func witnessURLAllowed(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

func canaryWatchCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "watch", Short: "Read a log's last checkpoint from the public witness and alarm when it is stale or its history was rewritten", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		logID, _ := c.Flags().GetString("log-id")
		witness, _ := c.Flags().GetString("witness")
		every, _ := c.Flags().GetDuration("expect-every")
		statePath, _ := c.Flags().GetString("state")
		if logID == "" || every <= 0 {
			return inputError("--log-id and a positive --expect-every are required")
		}
		if u, err := url.Parse(witness); err != nil || u.Host == "" || !witnessURLAllowed(u) {
			return hint(ErrInput, "--witness must be an https URL (plain http only for a witness on this machine)")
		}
		if statePath == "" {
			dir, err := profilesDir()
			if err != nil {
				return err
			}
			statePath = filepath.Join(filepath.Dir(dir), "canary", canaryStateName(logID)+".json")
		}
		now := dealClock().UTC()
		cp, found, err := readWitnessCheckpoint(c.Context(), witness, logID)
		if err != nil {
			return hint(errWitnessUnread, fmt.Sprintf("could not read %s for %s (%s); cannot tell whether it advanced", witness, logID, err))
		}
		if !found {
			return hint(ErrAlarm, fmt.Sprintf("%s has never been witnessed at %s", logID, witness))
		}
		prev, hadPrev, err := readCanaryState(statePath)
		if err != nil {
			return err
		}
		if why := canaryRewritten(cp, prev, hadPrev); why != "" {
			// The last good observation is kept, so every later run alarms too.
			return hint(ErrAlarm, fmt.Sprintf("%s history was rewritten: %s", logID, why))
		}
		next := canaryObservation{LogID: logID, MMRSize: cp.MMRSize, Root: cp.Root, KeyID: cp.KeyID, Timestamp: cp.Timestamp, SizeSince: now.Format(time.RFC3339)}
		if hadPrev && prev.MMRSize == cp.MMRSize && prev.SizeSince != "" {
			next.SizeSince = prev.SizeSince
		}
		if err := writeCanaryState(statePath, next); err != nil {
			return err
		}
		stamped, err := time.Parse(time.RFC3339, cp.Timestamp)
		if err != nil {
			return hint(ErrAlarm, fmt.Sprintf("%s: the witness's last checkpoint has no readable time (%q)", logID, cp.Timestamp))
		}
		since, _ := time.Parse(time.RFC3339, next.SizeSince)
		if age := now.Sub(stamped); age > every {
			return hint(ErrAlarm, fmt.Sprintf("%s has not advanced since %s (%s ago; expected every %s)", logID, cp.Timestamp, age.Truncate(time.Minute), every))
		}
		if held := now.Sub(since); held > every {
			return hint(ErrAlarm, fmt.Sprintf("%s has stayed at size %d since %s (%s; expected every %s)", logID, cp.MMRSize, next.SizeSince, held.Truncate(time.Minute), every))
		}
		return output(c, map[string]any{
			"state":            "advancing",
			"log_id":           logID,
			"mmr_size":         cp.MMRSize,
			"root":             cp.Root,
			"timestamp":        cp.Timestamp,
			"age_seconds":      int64(now.Sub(stamped) / time.Second),
			"continuity_grade": cp.ContinuityGrade,
		})
	}}
	cmd.Flags().String("log-id", "", "The canary profile's cadence log id (deal-cadence/...)")
	cmd.Flags().String("witness", dealDefaultWitness, "The public witness to read")
	cmd.Flags().Duration("expect-every", 0, "Alarm when the log has not advanced for longer than this: the canary's schedule plus the tick interval and jitter")
	cmd.Flags().String("state", "", "Where this watcher keeps its last observation (default: under the config directory)")
	return cmd
}

// canaryRewritten names how cp contradicts the last observation, or "".
func canaryRewritten(cp witnessCheckpoint, prev canaryObservation, hadPrev bool) string {
	if n := len(cp.Equivocations); n > 0 {
		return fmt.Sprintf("the witness has flagged %d conflicting checkpoint(s) for this log", n)
	}
	if !hadPrev {
		return ""
	}
	switch {
	case cp.KeyID != prev.KeyID:
		return fmt.Sprintf("its checkpoints are now signed by key %s, not %s", cp.KeyID, prev.KeyID)
	case cp.MMRSize < prev.MMRSize:
		return fmt.Sprintf("it went back from size %d to %d", prev.MMRSize, cp.MMRSize)
	case cp.MMRSize == prev.MMRSize && cp.Root != prev.Root:
		return fmt.Sprintf("size %d now has root %s, not %s", cp.MMRSize, cp.Root, prev.Root)
	case cp.PrevSize == prev.MMRSize && cp.PrevRoot != prev.Root:
		return fmt.Sprintf("the checkpoint after size %d says that size had root %s, not %s", prev.MMRSize, cp.PrevRoot, prev.Root)
	}
	return ""
}

func readWitnessCheckpoint(ctx context.Context, witness, logID string) (witnessCheckpoint, bool, error) {
	base, err := url.Parse(witness)
	if err != nil || base.Host == "" || !witnessURLAllowed(base) {
		return witnessCheckpoint{}, false, errors.New("not an https URL")
	}
	segments := strings.Split(logID, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	target := strings.TrimRight(witness, "/") + "/checkpoints/" + strings.Join(segments, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return witnessCheckpoint{}, false, err
	}
	resp, err := canaryHTTP.Do(req)
	if err != nil {
		var redirect *url.Error
		if errors.As(err, &redirect) && strings.Contains(redirect.Err.Error(), "redirect") {
			return witnessCheckpoint{}, false, redirect.Err
		}
		return witnessCheckpoint{}, false, errors.New("no answer")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return witnessCheckpoint{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return witnessCheckpoint{}, false, fmt.Errorf("status %d", resp.StatusCode)
	}
	var cp witnessCheckpoint
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cp); err != nil {
		return witnessCheckpoint{}, false, errors.New("not a checkpoint")
	}
	if cp.LogID != logID {
		return witnessCheckpoint{}, false, fmt.Errorf("answered for log %q", cp.LogID)
	}
	return cp, true, nil
}

var canaryUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func canaryStateName(logID string) string { return canaryUnsafe.ReplaceAllString(logID, "_") }

func readCanaryState(path string) (canaryObservation, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return canaryObservation{}, false, nil
	}
	if err != nil {
		return canaryObservation{}, false, hint(ErrInput, fmt.Sprintf("canary state %s is unreadable", path))
	}
	var o canaryObservation
	if err := json.Unmarshal(data, &o); err != nil {
		return canaryObservation{}, false, hint(ErrInput, fmt.Sprintf("canary state %s is not a canary observation; move it aside to start over", path))
	}
	return o, true, nil
}

func writeCanaryState(path string, o canaryObservation) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
