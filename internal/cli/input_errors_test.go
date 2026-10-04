package cli

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
	p, err := loadProfile("deal")
	require.NoError(t, err)
	p.LogID = ""
	require.NoError(t, saveProfile(p, true))
	_, err = invoke(t, "", "--profile", "deal", "cll", "list")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "profile deal has no log_id: a deal profile keeps one log per deal; pass --log-id deal/<deal id>")

	out, err := invoke(t, "", "--profile", "deal", "cll", "list", "--log-id", dealLogID(dealID))
	require.NoError(t, err)
	var listed struct {
		Entries []map[string]any `json:"entries"`
		LogID   string           `json:"log_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	assert.Equal(t, dealLogID(dealID), listed.LogID)
	assert.Len(t, listed.Entries, 5, "open plus four notes")

	_, err = invoke(t, "", "--profile", "deal", "cll", "list", "--log-id", "Not A Log")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--log-id must be")
}

// TestInputErrorsNameTheProblem runs commands the wrong way and requires each
// refusal to say more than the generic class text.
func TestInputErrorsNameTheProblem(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.LogID, p.Namespace = "", "capsule"
	require.NoError(t, saveProfile(p, false))
	for _, args := range [][]string{
		{"get"},
		{"get", "extra-arg"},
		{"get", "--no-such-flag"},
		{"get", "--profile"},
		{"get", "--profile", "missing-profile"},
		{"get", "--profile", "bad name!"},
		{"cll", "list", "--profile", p.Name},
		{"cll", "list", "--profile", p.Name, "--limit", "0"},
		{"cll", "list", "--profile", p.Name, "--after", "5", "--through", "2"},
		{"cll", "checkpoint", "publish", "--profile", p.Name, "--checkpoint", "1"},
		{"profile", "show"},
		{"contract", "diff", "only-one.json"},
		{"judge", "drift", "pin", "only-one.json"},
		{"publish", "--profile", p.Name},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := invoke(t, "", args...)
			require.Error(t, err)
			assert.Equal(t, 2, ExitCode(err))
			msg := SafeError(err)
			assert.NotEqual(t, ErrInput.Error(), msg, "the refusal must name the field, flag or file at fault")
			assert.True(t, strings.HasPrefix(msg, ErrInput.Error()+": "), msg)
		})
	}
}

// joinAllowed lists the functions that join ErrInput with an error that is
// already a shown reason (an inputError, a hint, or an error type SafeError
// prints), so the join only fixes the exit code.
var joinAllowed = map[string]string{
	"decodeJSON":                "joins the inputError reasons it returns",
	"decodeJSONPreserveNumbers": "joins the inputError reasons it returns",
	"selected":                  "loadProfile returns inputError reasons",
	"profileCommands":           "profile show: loadProfile returns inputError reasons",
}

// shownTypes are the error values SafeError prints in their own words.
var shownTypes = map[string]bool{"inputFileError": true, "schemaLoadError": true, "pluginRequiredError": true, "artifact.ErrUntrustedSigner": true}

// TestNoGenericInputErrors holds every ErrInput path in the package to a
// shown reason. It parses the package's own source: ErrInput may appear only
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
			ast.Inspect(decl, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				callee := exprName(call.Fun)
				switch callee {
				case "errors.Is", "hint":
					for _, a := range call.Args {
						ok[a] = true
					}
				case "errors.Join":
					if len(call.Args) > 0 && exprName(call.Args[0]) == "ErrInput" {
						if _, allowed := joinAllowed[fnName]; allowed {
							ok[call.Args[0]] = true
						} else if len(call.Args) > 1 && startsWithReason(call.Args[1]) {
							ok[call.Args[0]] = true
						}
					}
				case "inputError":
					if lit, isLit := call.Args[0].(*ast.BasicLit); isLit {
						reason, _ := strconv.Unquote(lit.Value)
						if len(strings.Fields(reason)) < 4 {
							t.Errorf("%s: inputError(%q) is too terse to name the field and the expected value", fset.Position(call.Pos()), reason)
						}
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, isSel := n.(*ast.SelectorExpr)
				if isSel && exprName(sel) == "ErrInput.Error" {
					ok[sel.X] = true
				}
				id, isID := n.(*ast.Ident)
				if !isID || id.Name != "ErrInput" || ok[n] {
					return true
				}
				if gen, isGen := decl.(*ast.GenDecl); isGen && gen.Tok == token.VAR {
					return true // its declaration
				}
				t.Errorf("%s: ErrInput used bare in %s: return inputError(reason) or hint(ErrInput, reason) so the operator sees what is wrong", fset.Position(id.Pos()), fnName)
				return true
			})
		}
	}
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

func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprName(v.X) + "." + v.Sel.Name
	}
	return ""
}
