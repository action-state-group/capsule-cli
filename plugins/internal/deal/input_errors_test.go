package deal

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCLLListOnDealProfile: a deal profile keeps one log per deal. One made
// before deals shared a cadence log has no log_id at all; `cll list` on it
// says so and names --log-id, and --log-id deal/<id> reads that deal's log.
func TestCLLListOnDealProfile(t *testing.T) {
	dealFixture(t)
	dealID := openJetSki(t)

	// Today's deal profile lists its cadence log.
	_, err := invoke(t, "", "--profile", "deal", "cll", "list")
	require.NoError(t, err)

	// A deal profile as earlier releases wrote it: no log_id.
	p, err := cli.LoadProfile("deal")
	require.NoError(t, err)
	p.LogID = ""
	require.NoError(t, cli.SaveProfile(p, true))
	_, err = invoke(t, "", "--profile", "deal", "cll", "list")
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, cli.SafeError(err), "profile deal has no log_id: a deal profile keeps one log per deal; pass --log-id deal/<deal id>")

	out, err := invoke(t, "", "--profile", "deal", "cll", "list", "--log-id", cli.DealLogID(dealID))
	require.NoError(t, err)
	var listed struct {
		Entries []map[string]any `json:"entries"`
		LogID   string           `json:"log_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	assert.Equal(t, cli.DealLogID(dealID), listed.LogID)
	assert.Len(t, listed.Entries, 5, "open plus four notes")

	_, err = invoke(t, "", "--profile", "deal", "cll", "list", "--log-id", "Not A Log")
	require.ErrorIs(t, err, cli.ErrInput)
	assert.Contains(t, cli.SafeError(err), "--log-id must be")
}

// TestMalformedEmailRefusalNeverQuotesAHeader: the mail parser's own error
// can quote a malformed header line; the shown refusal never does.
func TestMalformedEmailRefusalNeverQuotesAHeader(t *testing.T) {
	raw := []byte("Subject: order\r\nship-to jane.doe@example.com 12 Main Street\r\n\r\nbody\r\n")
	_, err := captureEmail(raw, nil)
	require.ErrorIs(t, err, cli.ErrInput)
	msg := cli.SafeError(err)
	assert.Contains(t, msg, "the email is not a raw RFC 822 message with its headers intact")
	assert.NotContains(t, msg, "jane.doe@example.com")
	assert.NotContains(t, msg, "Main Street")
}

// joinAllowed lists the functions that join ErrInput with an error that is
// already a shown reason (an inputError, a hint, or an error type SafeError
// prints), so the join only fixes the exit code.
var joinAllowed = map[string]string{
	"Selected":        "LoadProfile returns InputError reasons",
	"profileCommands": "profile show: LoadProfile returns InputError reasons",
}

// classifyAllowed lists the functions that use ErrInput only as an errors.Is
// target in a list, to keep another command's error class while showing that
// command's own reason.
var classifyAllowed = map[string]string{
	"canaryStep": "keeps a deal verb's error class; the shown reason is the verb's own SafeError text",
	"Unwrap":     "PluginExit reads a plugin's exit code as its error class; the plugin printed the reason itself",
}

// shownTypes are the error values SafeError prints in their own words.
var shownTypes = map[string]bool{"InputFileError": true, "schemaLoadError": true, "artifact.ErrUntrustedSigner": true}

// TestNoGenericInputErrors holds every ErrInput path in the plugins'
// package, as the core's test of the same name holds the core's, to a shown
// reason. It parses the package's own source: ErrInput may appear only
// as errors.Is's target, hint's class, SafeError's own text, or joined in a
// function listed in joinAllowed; and every inputError reason must be a real
// sentence, not a bare word.
func TestNoGenericInputErrors(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			fnName := ""
			if fn != nil {
				fnName = fn.Name.Name
			}
			ok := map[ast.Node]bool{}
			// mark accepts an ErrInput use, written bare or as cli.ErrInput.
			mark := func(n ast.Node) {
				ok[n] = true
				if sel, isSel := n.(*ast.SelectorExpr); isSel {
					ok[sel.Sel] = true
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				callee := exprName(call.Fun)
				switch callee {
				case "errors.Is":
					for _, a := range call.Args {
						mark(a)
					}
				case "errors.Join":
					if len(call.Args) > 0 && exprName(call.Args[0]) == "ErrInput" {
						if _, allowed := joinAllowed[fnName]; allowed {
							mark(call.Args[0])
						} else if len(call.Args) > 1 && startsWithReason(call.Args[1]) {
							mark(call.Args[0])
						}
					}
				case "inputError", "hint":
					reasonArg := call.Args[0]
					if callee == "hint" {
						for _, a := range call.Args {
							mark(a)
						}
						if exprName(call.Args[0]) != "ErrInput" || len(call.Args) < 2 {
							return true
						}
						reasonArg = call.Args[1]
					}
					if lit, isLit := reasonArg.(*ast.BasicLit); isLit {
						reason, _ := strconv.Unquote(lit.Value)
						if !namesWhatIsWrong(reason) {
							t.Errorf("%s: %s(%q) names no flag, field or file, and is too short to say what is wrong", fset.Position(call.Pos()), callee, reason)
						}
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, isSel := n.(*ast.SelectorExpr)
				if isSel && exprName(sel) == "ErrInput.Error" {
					mark(sel.X)
				}
				id, isID := n.(*ast.Ident)
				if !isID || id.Name != "ErrInput" || ok[n] {
					return true
				}
				if gen, isGen := decl.(*ast.GenDecl); isGen && gen.Tok == token.VAR {
					return true // its declaration
				}
				if _, allowed := classifyAllowed[fnName]; allowed {
					return true
				}
				t.Errorf("%s: ErrInput used bare in %s: return inputError(reason) or hint(ErrInput, reason) so the operator sees what is wrong", fset.Position(id.Pos()), fnName)
				return true
			})
		}
	}
}

// reasonNames matches a flag (--name), a field (snake_case, dotted, an
// UPPER_CASE variable) or a quoted command in a reason.
var reasonNames = regexp.MustCompile("--[a-z]|[a-z0-9]+_[a-z0-9_]+|[a-z]+\\.[a-z_]+|[A-Z]+_[A-Z_]+|`")

// namesWhatIsWrong is the bar every literal reason meets: it names the flag,
// field or file at fault, or says in at least six words what is wrong and
// what is expected. "emit rejected sealing input" and "invalid --log-id"
// would not have passed in that form.
func namesWhatIsWrong(reason string) bool {
	words := len(strings.Fields(reason))
	return words >= 6 || words >= 3 && reasonNames.MatchString(reason)
}

// startsWithReason reports whether e is an inputError(...) or hint(...)
// call, or an error SafeError prints in its own words.
func startsWithReason(e ast.Expr) bool {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		if lit, ok := u.X.(*ast.CompositeLit); ok {
			return shownTypes[exprName(lit.Type)]
		}
	}
	if shownTypes[exprName(e)] {
		return true
	}
	call, ok := e.(*ast.CallExpr)
	return ok && (exprName(call.Fun) == "inputError" || exprName(call.Fun) == "hint")
}

// exprName names an expression the way the checks above spell it. The
// reason helpers and the error class are the core's: inputError, hint and
// ErrInput, also written as InputError, Hint, or cli.X from a plugin.
func exprName(e ast.Expr) string {
	var name string
	switch v := e.(type) {
	case *ast.Ident:
		name = v.Name
	case *ast.SelectorExpr:
		name = exprName(v.X) + "." + v.Sel.Name
	}
	name = strings.TrimPrefix(name, "cli.")
	switch name {
	case "InputError":
		return "inputError"
	case "Hint":
		return "hint"
	}
	return name
}
