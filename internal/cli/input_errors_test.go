package cli

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
	t.Run("JSON input", func(t *testing.T) { jsonInputErrorCases(t, p) })
}

// joinAllowed lists the functions that join ErrInput with an error that is
// already a shown reason (an inputError, a hint, or an error type SafeError
// prints), so the join only fixes the exit code.
var joinAllowed = map[string]string{
	"selected":        "loadProfile returns inputError reasons",
	"profileCommands": "profile show: loadProfile returns inputError reasons",
}

// classifyAllowed lists the functions that use ErrInput only as an errors.Is
// target in a list, to keep another command's error class while showing that
// command's own reason.
var classifyAllowed = map[string]string{
	"canaryStep": "keeps a deal verb's error class; the shown reason is the verb's own SafeError text",
}

// shownTypes are the error values SafeError prints in their own words.
var shownTypes = map[string]bool{"inputFileError": true, "schemaLoadError": true, "artifact.ErrUntrustedSigner": true}

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
				case "errors.Is":
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
				case "inputError", "hint":
					reasonArg := call.Args[0]
					if callee == "hint" {
						for _, a := range call.Args {
							ok[a] = true
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
					ok[sel.X] = true
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

func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprName(v.X) + "." + v.Sel.Name
	}
	return ""
}

// jsonInputErrorCases: a JSON input that does not fit names the flag or
// file, then the unknown field, the mistyped field and the type it takes, or
// the byte where the syntax breaks; it never repeats a value.
func jsonInputErrorCases(t *testing.T, p Profile) {
	const secret = "SECRET-VALUE-0123"
	file := func(body string) string {
		path := filepath.Join(t.TempDir(), "in.json")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown field", []string{"publish", "--profile", p.Name, "--request", file(`{"spec_version":"capsule-seal-request/v1","capsule":{},"bogus_field":"` + secret + `"}`)},
			`--request has a field this command does not accept: "bogus_field"`},
		{"wrong type", []string{"publish", "--profile", p.Name, "--request", file(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":["` + secret + `"]}}`)},
			"--request: field capsule.ActionID must be a string, not array"},
		{"syntax", []string{"publish", "--profile", p.Name, "--request", file(`{"spec_version":"` + secret + `",,}`)},
			"--request is not valid JSON: the syntax breaks at byte 37"},
		{"card-like number into a string field", []string{"publish", "--profile", p.Name, "--request", file(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":4111111111111111.5}}`)},
			"--request: field capsule.ActionID must be a string, not number"},
		{"card-like number into a string field of a judge file", []string{"judge", "pin", file(`{"model_id":"m","model_version":4111111111111111}`)},
			"field model_version must be a string, not number"},
		{"judge file wrong type", []string{"judge", "pin", file(`{"model_id":{"x":"` + secret + `"}}`)},
			"field model_id must be a string, not object"},
		{"empty", []string{"publish", "--profile", p.Name, "--request", file(``)},
			"--request is empty or ends before its JSON value is complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := invoke(t, "", tc.args...)
			require.ErrorIs(t, err, ErrInput)
			msg := SafeError(err)
			assert.Contains(t, msg, tc.want)
			assert.NotContains(t, msg+out, secret, "a value from the input is never repeated")
			assert.NotContains(t, msg+out, "4111111111111111", "a number from the input is never repeated")
		})
	}
}

// TestJSONTypeErrorNeverEchoesTheNumber: Go reports a number that does not
// fit an integer field with the number itself ("number 4111111111111111.5");
// the refusal keeps only the kind.
func TestJSONTypeErrorNeverEchoesTheNumber(t *testing.T) {
	var into struct {
		AmountMinor int64 `json:"amount_minor"`
	}
	for _, raw := range []string{`{"amount_minor":4111111111111111.5}`, `{"amount_minor":41111111111111111111111}`} {
		err := decodeJSONAs("--input", []byte(raw), &into)
		require.ErrorIs(t, err, ErrInput)
		msg := SafeError(err)
		assert.Contains(t, msg, "--input: field amount_minor must be an integer, not number")
		assert.NotContains(t, msg, "4111111111111111")
	}
}

// TestMalformedEmailRefusalNeverQuotesAHeader: the mail parser's own error
// can quote a malformed header line; the shown refusal never does.
func TestMalformedEmailRefusalNeverQuotesAHeader(t *testing.T) {
	raw := []byte("Subject: order\r\nship-to jane.doe@example.com 12 Main Street\r\n\r\nbody\r\n")
	_, err := captureEmail(raw, nil)
	require.ErrorIs(t, err, ErrInput)
	msg := SafeError(err)
	assert.Contains(t, msg, "the email is not a raw RFC 822 message with its headers intact")
	assert.NotContains(t, msg, "jane.doe@example.com")
	assert.NotContains(t, msg, "Main Street")
}

// The bar TestNoGenericInputErrors holds literal reasons to.
func TestNamesWhatIsWrong(t *testing.T) {
	for _, terse := range []string{"emit rejected sealing input", "invalid --log-id", "invalid profile fields", "unsupported profile type"} {
		assert.False(t, namesWhatIsWrong(terse), terse)
	}
	for _, named := range []string{"--log-id must be lowercase letters, digits and ._:/-", "--peer is required: a held Evidence Bundle file", "model_id is required in the judge pin input", "XDG_CONFIG_HOME must be an absolute path when it is set"} {
		assert.True(t, namesWhatIsWrong(named), named)
	}
}

// A capsule the producer refuses names the field and the rule it breaks.
func TestSealRefusalNamesTheField(t *testing.T) {
	r, err := parseRequest([]byte(`{"spec_version":"capsule-seal-request/v1","capsule":{"ActionID":"a","ActionType":"decide","Operator":"o","Developer":"d","Timestamp":"2026-09-08T00:00:00Z"}}`))
	require.NoError(t, err)
	_, key := profileFixture(t)
	_, err = seal(r, key)
	require.ErrorIs(t, err, ErrInput)
	msg := SafeError(err)
	assert.Contains(t, msg, "emit rejected sealing input: decide action requires a disposition")
	assert.Contains(t, msg, "fix that field in the capsule-seal-request/v1 request (--request)")
}
