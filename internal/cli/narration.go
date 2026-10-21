package cli

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/spf13/cobra"
)

// The narration contract (capsule-cli#345): every result object an agent
// reads says whether to speak and exactly what. voice is one of these; say
// is the text to speak verbatim, lines joined by "\n", with its values filled
// here from sealed fields, never by the agent. Words come from one wording
// pack (aac.wording-pack/v0): the built-in neutral pack, or the complete pack
// the profile pins (profile update --wording).
const (
	VoiceSilent = "silent"
	VoiceAsk    = "ask"
	VoiceLine   = "line"
	VoiceReport = "report"
	VoiceError  = "error"
)

// Narration is the part of a result an agent speaks from. Empty fields are
// left out of the output.
type Narration struct {
	Voice  string  `json:"voice"`
	Say    string  `json:"say,omitempty"`
	Ask    *Ask    `json:"ask,omitempty"`
	Attach *Attach `json:"attach,omitempty"`
	// Hold is true with voice error only: the agent stops, and goes on with
	// nothing that pays or shares.
	Hold bool `json:"hold,omitempty"`
	// Wording is "built-in" when the profile's pinned pack could not be
	// used and the built-in pack spoke instead.
	Wording string `json:"wording,omitempty"`
}

// Ask is a card the agent shows the user (voice ask). On a check it is the
// sealed card's parts; Options are the check's sealed options. Reply
// "words" says the answer is the user's own words rather than a button.
type Ask struct {
	Tag        string      `json:"tag"`
	Question   string      `json:"question"`
	Facts      string      `json:"facts,omitempty"`
	Why        []string    `json:"why,omitempty"`
	GoodToKnow string      `json:"good_to_know,omitempty"`
	Next       string      `json:"next,omitempty"`
	Options    []AskOption `json:"options"`
	Reply      string      `json:"reply,omitempty"`
	// HoldSay is what to say when the user holds and no command runs (a
	// card no answer is sealed for).
	HoldSay string `json:"hold_say,omitempty"`
	// WhyKey is the pack key that says the reasons, when not the default
	// "Why I'm asking" (a deny says why it stopped).
	WhyKey string `json:"-"`
}

// AskOption is one button: its label and the choice `deal note --kind
// approval --choice` takes for it.
type AskOption struct {
	Label  string `json:"label"`
	Choice string `json:"choice"`
}

// Attach is the page a report sends after its lines.
type Attach struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Label     string `json:"label"`
}

// Narrate adds n's fields to a result map before Output.
func Narrate(out map[string]any, n Narration) map[string]any {
	out["voice"] = n.Voice
	if n.Say != "" {
		out["say"] = n.Say
	}
	if n.Ask != nil {
		out["ask"] = n.Ask
	}
	if n.Attach != nil {
		out["attach"] = n.Attach
	}
	if n.Hold {
		out["hold"] = true
	}
	if n.Wording != "" {
		out["wording"] = n.Wording
	}
	return out
}

// Lines joins the non-empty lines of a say.
func Lines(lines ...string) string {
	return strings.Join(slices.DeleteFunc(lines, func(s string) bool { return s == "" }), "\n")
}

// WordingPackID names the built-in neutral pack.
const WordingPackID = "capsulectl.wording/v0"

//go:embed assets/wording/capsulectl-en.json
var builtinWordingBytes []byte

// Wording is a loaded wording pack: its entries, and where it came from
// ("built-in" or "pinned").
type Wording struct {
	ID      string
	Locale  string
	Entries map[string]string
	Source  string
}

// BuiltinWording is the neutral pack that ships with this binary.
func BuiltinWording() Wording { return builtinWording() }

var builtinWording = sync.OnceValue(func() Wording {
	w, err := parseWording(builtinWordingBytes)
	if err != nil {
		// The pack is embedded and tested: a broken one is a build defect.
		panic("built-in wording pack: " + err.Error())
	}
	w.Source = "built-in"
	return w
})

// BuiltinWordingBytes is the built-in pack as distributed.
func BuiltinWordingBytes() []byte { return slices.Clone(builtinWordingBytes) }

func parseWording(raw []byte) (Wording, error) {
	schema, err := PresentationSchema("wording")
	if err != nil {
		return Wording{}, err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Wording{}, errors.New("not JSON")
	}
	if err := schema.Validate(doc); err != nil {
		return Wording{}, errors.New("not a valid aac.wording-pack/v0: " + FirstSchemaError(err))
	}
	var pack struct {
		ID      string            `json:"id"`
		Locale  string            `json:"locale"`
		Entries map[string]string `json:"entries"`
	}
	if err := json.Unmarshal(raw, &pack); err != nil {
		return Wording{}, errors.New("not JSON")
	}
	return Wording{ID: pack.ID, Locale: pack.Locale, Entries: pack.Entries}, nil
}

// missingKeys are the built-in pack's keys w lacks: a pack must be complete,
// as one pack is never mixed with another.
func (w Wording) missingKeys() []string {
	var missing []string
	for k := range BuiltinWording().Entries {
		if _, ok := w.Entries[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

func packDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ProfileWording is a profile's pinned wording pack.
type ProfileWording struct {
	Path   string `yaml:"path,omitempty" mapstructure:"path"`
	SHA256 string `yaml:"sha256,omitempty" mapstructure:"sha256"`
}

// pinWording pins the pack at path on p, or removes the pin for "". A pack
// that is not valid or not complete is refused.
func pinWording(p *Profile, path string) error {
	if path == "" {
		p.Wording = ProfileWording{}
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return InputError("--wording needs a readable file path")
	}
	raw, err := ReadInput(abs)
	if err != nil {
		return err
	}
	w, err := parseWording(raw)
	if err != nil {
		return InputError("--wording " + path + " is " + err.Error())
	}
	if missing := w.missingKeys(); len(missing) > 0 {
		return InputError(fmt.Sprintf("--wording %s is not complete: it lacks %d of the built-in pack's keys, first %s", path, len(missing), missing[0]))
	}
	p.Wording = ProfileWording{Path: abs, SHA256: packDigest(raw)}
	return nil
}

// ActiveWording is the pack p speaks with, and why the pinned one is not
// used when it is not: missing, changed since it was pinned, or lacking a
// key a later release added. Then the whole built-in pack is used, never a
// mix of the two.
func ActiveWording(p Profile) (Wording, string) {
	if p.Wording.Path == "" {
		return BuiltinWording(), ""
	}
	raw, err := os.ReadFile(p.Wording.Path)
	switch {
	case err != nil:
		return BuiltinWording(), "the pinned wording pack cannot be read: " + p.Wording.Path
	case packDigest(raw) != p.Wording.SHA256:
		return BuiltinWording(), "the pinned wording pack changed since it was pinned: pin it again with profile update --wording"
	}
	w, err := parseWording(raw)
	if err != nil {
		return BuiltinWording(), "the pinned wording pack is " + err.Error()
	}
	if missing := w.missingKeys(); len(missing) > 0 {
		return BuiltinWording(), "the pinned wording pack lacks keys this release uses: " + strings.Join(missing, ", ")
	}
	w.Source = "pinned"
	return w, ""
}

var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

// Line is the entry at key with its {placeholders} filled from vals. A key
// the pack lacks, or a placeholder with no value, is a defect the tests
// catch; at run time the line is left out rather than spoken half-filled.
func (w Wording) Line(key string, vals map[string]string) string {
	text, ok := w.Entries[key]
	if !ok {
		return ""
	}
	missing := false
	filled := placeholder.ReplaceAllStringFunc(text, func(m string) string {
		v, ok := vals[m[1:len(m)-1]]
		if !ok {
			missing = true
		}
		return v
	})
	if missing {
		return ""
	}
	return filled
}

// V is shorthand for a Line's values.
type V = map[string]string

// outputWritten counts what this process wrote to its stdout, so a failure
// that already printed something gets no error object after it.
var outputWritten atomic.Int64

// Stdout is the process's stdout, counted. Mains hand it to the root
// command; a plugin is given the file itself.
type Stdout struct{ File *os.File }

func (s Stdout) Write(b []byte) (int, error) {
	n, err := s.File.Write(b)
	outputWritten.Add(int64(n))
	return n, err
}

// stdoutFile is w's file when w is the counted stdout, so a plugin writes to
// the real stdout and not behind a pipe.
func stdoutFile(w io.Writer) io.Writer {
	if s, ok := w.(Stdout); ok {
		return s.File
	}
	return w
}

// errPluginNotStarted marks a plugin that never ran: it wrote nothing, so
// the core speaks its error.
var errPluginNotStarted = errors.New("plugin not started")

// NarrationExempt, on a command's Annotations, marks an operator command no
// agent runs (canary): its failure keeps its own plain line and no object.
const NarrationExempt = "narration-exempt"

// ErrorPhase tells, for a failed command, whether the user's step had
// happened yet: "before" (nothing was done), "after" (it happened outside,
// and the record may be incomplete) or "other". A plugin sets it for its
// own commands.
var ErrorPhase = func(*cobra.Command) string { return "other" }

// Exit ends a main: the error line on stderr as before, and, when the
// command wrote nothing to stdout, one error object an agent speaks from.
// It returns the exit code.
func Exit(cmd *cobra.Command, err error, name string) int {
	return exitTo(Stdout{File: os.Stdout}, os.Stderr, cmd, err, name)
}

func exitTo(stdout, stderr io.Writer, cmd *cobra.Command, err error, name string) int {
	if msg := SafeError(err); msg != "" {
		fmt.Fprintln(stderr, name+": "+msg)
	}
	if obj := errorObject(cmd, err); obj != nil && outputWritten.Load() == 0 {
		_ = json.NewEncoder(stdout).Encode(obj)
	}
	return ExitCode(err)
}

// errorObject is the error result for a failed command, or nil when none is
// written: a plugin that ran (it speaks for itself) or an exempt command.
func errorObject(cmd *cobra.Command, err error) map[string]any {
	var exit *PluginExit
	switch {
	case errors.As(err, &exit):
		return nil
	case errors.Is(err, errPluginStopped) && !errors.Is(err, errPluginNotStarted):
		return nil
	}
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations[NarrationExempt] != "" {
			return nil
		}
	}
	phase := "other"
	build := func(w Wording) Narration {
		return Narration{Voice: VoiceError, Hold: true, Say: Lines(w.Line("say.error."+phase, nil), w.Line("say.error.cause."+errorCause(err), nil))}
	}
	n := speakWith(Profile{}, false, build)
	if cmd != nil {
		phase = ErrorPhase(cmd)
		n = NarrateWith(cmd, build)
	}
	out := map[string]any{"spec_version": "capsule-cli-result/v1", "error": SafeError(err)}
	return Narrate(out, n)
}

// errorCause is the cause key for an error's class.
func errorCause(err error) string {
	switch {
	case errors.Is(err, ErrInput):
		return "input"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrPending):
		return "pending"
	case errors.Is(err, ErrPartial):
		return "partial"
	case errors.Is(err, ErrPaused):
		return "paused"
	case errors.Is(err, ErrNoAnswer):
		return "no_answer"
	default:
		return "other"
	}
}

// Run executes a main's root command with the counted stdout and returns
// the exit code, writing the error line and, when nothing was printed, the
// error object.
func Run(ctx context.Context, root *cobra.Command, name string) int {
	return runTo(ctx, root, name, os.Stdout, os.Stderr)
}

func runTo(ctx context.Context, root *cobra.Command, name string, stdout *os.File, stderr io.Writer) int {
	outputWritten.Store(0)
	out := Stdout{File: stdout}
	root.SetOut(out)
	root.SetErr(stderr)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return 0
	}
	return exitTo(out, stderr, cmd, err, name)
}

// narratedByOutcome marks a command whose voice follows its outcome (the
// error it returns), known only after it printed: Output then adds no
// default voice, and NarrateByOutcome adds one after the run.
const narratedByOutcome = "narrated-by-outcome"

func byOutcome(c *cobra.Command) bool {
	for ; c != nil; c = c.Parent() {
		if c.Annotations[narratedByOutcome] != "" {
			return true
		}
	}
	return false
}

// NarrateByOutcome wraps cmd's RunE: what it prints is held, and when it is
// one result object, narrate's fields for the run's outcome are added to it
// before it is written. Members keep their order (verify --history's report
// is read in order).
func NarrateByOutcome(cmd *cobra.Command, narrate func(c *cobra.Command, result map[string]any, err error) Narration) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[narratedByOutcome] = "true"
	run := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		out := c.OutOrStdout()
		var held strings.Builder
		c.SetOut(&held)
		err := run(c, args)
		c.SetOut(out)
		text := held.String()
		var result map[string]any
		trimmed := strings.TrimRight(text, "\n")
		if json.Unmarshal([]byte(trimmed), &result) != nil || !strings.HasSuffix(trimmed, "}") {
			_, werr := io.WriteString(out, text)
			return errors.Join(err, werr)
		}
		fields, merr := json.Marshal(narrate(c, result, err))
		if merr != nil {
			return errors.Join(err, merr)
		}
		_, werr := io.WriteString(out, strings.TrimSuffix(trimmed, "}")+","+string(fields[1:])+"\n")
		return errors.Join(err, werr)
	}
	return cmd
}

// verifyNarration is verify's report line: unchanged when every check
// passed, unable when something could not be checked here (exit 3), and
// changed when a check failed.
func verifyNarration(c *cobra.Command, _ map[string]any, err error) Narration {
	key := "say.verify.unchanged"
	switch {
	case err == nil:
	case errors.Is(err, ErrPartial):
		key = "say.verify.unable"
	default:
		key = "say.verify.changed"
	}
	return NarrateWith(c, func(w Wording) Narration { return Narration{Voice: VoiceReport, Say: w.Line(key, nil)} })
}

// doctorNarration is silent when nothing needs fixing, and one line with
// the number of problems when something does.
func doctorNarration(c *cobra.Command, result map[string]any, _ error) Narration {
	n := countIssues(result)
	if n == 0 {
		return Narration{Voice: VoiceSilent}
	}
	return NarrateWith(c, func(w Wording) Narration {
		if n == 1 {
			return Narration{Voice: VoiceLine, Say: w.Line("say.doctor.issues.one", nil)}
		}
		return Narration{Voice: VoiceLine, Say: w.Line("say.doctor.issues.other", V{"count": fmt.Sprint(n)})}
	})
}

// countIssues counts the "issue" members anywhere in a doctor report.
func countIssues(v any) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k == "issue" {
				if s, _ := x.(string); s != "" {
					n++
				}
				continue
			}
			if list, ok := x.([]any); ok && k == "issues" {
				n += len(list)
				continue
			}
			n += countIssues(x)
		}
	case []any:
		for _, x := range t {
			n += countIssues(x)
		}
	}
	return n
}

// Speak adds the narration build makes with p's wording to out. When the
// pinned pack cannot be used, the result says so ("wording": "built-in").
func Speak(out map[string]any, p Profile, build func(Wording) Narration) map[string]any {
	return Narrate(out, speakWith(p, true, build))
}

// speakWith builds a narration with p's wording (or the built-in pack when
// there is no profile), marked when a pinned pack could not be used.
func speakWith(p Profile, hasProfile bool, build func(Wording) Narration) Narration {
	w, problem := BuiltinWording(), ""
	if hasProfile {
		w, problem = ActiveWording(p)
	}
	n := build(w)
	if problem != "" {
		n.Wording = w.Source
	}
	return n
}

// NarrateWith builds a narration with the selected profile's wording, or
// the built-in pack when no profile is selected.
func NarrateWith(c *cobra.Command, build func(Wording) Narration) Narration {
	p, err := Selected(c)
	return speakWith(p, err == nil, build)
}

// AskText is a card as the lines the user reads: the tag, the question,
// the facts, why it is asked, what is good to know and what comes next.
// Button labels are not part of it.
func AskText(w Wording, a Ask) string {
	lines := []string{a.Tag, a.Question, a.Facts}
	if len(a.Why) > 0 {
		key := a.WhyKey
		if key == "" {
			key = "card.why.label"
		}
		lines = append(lines, w.Line(key, V{"why": strings.Join(a.Why, "; ")}))
	}
	if a.GoodToKnow != "" {
		lines = append(lines, w.Line("card.good_to_know.label", V{"text": a.GoodToKnow}))
	}
	if a.Next != "" {
		lines = append(lines, w.Line("card.next.label", V{"text": a.Next}))
	}
	if a.Reply == "words" {
		lines = append(lines, w.Line("card.reply.words", nil))
	}
	return Lines(lines...)
}
