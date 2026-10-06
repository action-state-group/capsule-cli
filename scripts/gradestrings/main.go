// Command gradestrings enumerates every string literal capsulectl ships that
// uses the evidence grade ladder's vocabulary, so the strings a receipt can
// show are checked against the ladder by count, not by sampling. Run it from
// the repository root:
//
//	go run ./scripts/gradestrings > grade-strings.tsv
//
// It reads the non-test Go files under internal/cli and cmd (git ls-files)
// and the page script internal/cli/assets/deal-view.js, and writes one
// tab-separated row per matching literal: file:line, class, the literal
// (newlines, carriage returns and tabs escaped). On stderr it prints the
// counts, the SHA-256 of the rows, and a cross-check: a second, independent
// tokenizer over the same source text must find the same number of
// literals and of matches, or it exits non-zero.
//
// Classes, by the literal and its source line: ladder (internal/cli/
// grade_ladder.go); rung-value (the value of a rung, time_rung or grade
// field); identifier (no space: JSON keys, states, codes); receipt-copy
// (words the receipt, email or page shows); operator (help and errors).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// vocabulary is the ladder's words and the off-ladder ones a grade could
// drift to. A letter may not touch either end; an underscore may, so
// snake_case values (witnessed_in_part) are matched.
var vocabulary = regexp.MustCompile(`(?i)(?:^|[^a-z])(?:self[- _]?attested|attest(?:s|ed|ation)?|witness(?:ed)?|countersign(?:ed|ature)?|self[- _]countersigned|unresolved[- _]signer|anchored|notari[sz]ed|tamper[- ]evident|non-repudiation|unsigned)(?:[^a-z]|$)`)

var (
	rungLine    = regexp.MustCompile(`view\.Rung = "|"rung": "|\["rung"\] = "|assuranceLadder = |timeRung|time_rung|\bRung:\s*"`)
	helpLine    = regexp.MustCompile(`Short:|Use:|Flags\(\)|Errorf|inputError|errors\.New`)
	timeRung    = regexp.MustCompile(`timeRung|time_rung`)
	receiptFile = map[string]bool{"internal/cli/deal_email.go": true, "internal/cli/deal_countersign.go": true, "internal/cli/deal_report.go": true, "internal/cli/deal_obligation.go": true}
	escape      = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`)
	// goToken and jsLiteral are the independent tokenizer and the page
	// script's literals: comments are consumed so their quotes do not count.
	goToken   = regexp.MustCompile("(?s)//[^\\n]*|/\\*.*?\\*/|'(?:[^'\\\\\\n]|\\\\.)*'|\"(?:[^\"\\\\\\n]|\\\\.)*\"|`[^`]*`")
	jsLiteral = regexp.MustCompile("(?s)'(?:[^'\\\\\\n]|\\\\.)*'|\"(?:[^\"\\\\\\n]|\\\\.)*\"|`(?:[^`\\\\]|\\\\.)*`")
)

func main() {
	out, err := exec.Command("git", "ls-files", "internal/cli/*.go", "cmd/*.go").Output()
	if err != nil {
		fail(err)
	}
	var files []string
	for _, f := range strings.Fields(string(out)) {
		if !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	var rows []string
	literals, independentLiterals, independentMatches := 0, 0, 0
	fset := token.NewFileSet()
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		lines := strings.Split(string(src), "\n")
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			fail(err)
		}
		var found []*ast.BasicLit
		ast.Inspect(f, func(n ast.Node) bool {
			if b, ok := n.(*ast.BasicLit); ok && b.Kind == token.STRING {
				found = append(found, b)
			}
			return true
		})
		sort.SliceStable(found, func(i, j int) bool { return found[i].Pos() < found[j].Pos() })
		for _, b := range found {
			literals++
			v, err := strconv.Unquote(b.Value)
			if err != nil {
				v = b.Value
			}
			v = escape.Replace(v)
			if !vocabulary.MatchString(v) {
				continue
			}
			line := fset.Position(b.Pos()).Line
			rows = append(rows, fmt.Sprintf("%s:%d\t%s\t%s", path, line, goClass(path, lines[line-1], v), v))
		}
		for _, t := range goToken.FindAllString(string(src), -1) {
			if t[0] == '"' || t[0] == '`' {
				independentLiterals++
				if vocabulary.MatchString(t[1 : len(t)-1]) {
					independentMatches++
				}
			}
		}
	}
	goRows := len(rows)
	const page = "internal/cli/assets/deal-view.js"
	js, err := os.ReadFile(page)
	if err != nil {
		fail(err)
	}
	jsLines := strings.Split(string(js), "\n")
	for _, loc := range jsLiteral.FindAllStringIndex(string(js), -1) {
		v := string(js[loc[0]+1 : loc[1]-1])
		if !vocabulary.MatchString(v) {
			continue
		}
		line := strings.Count(string(js[:loc[0]]), "\n") + 1
		class := "receipt-copy"
		switch {
		case regexp.MustCompile(`rung\s*(?::|===|!==)\s*"` + regexp.QuoteMeta(v) + `"`).MatchString(jsLines[line-1]):
			class = "rung-value"
		case !strings.Contains(strings.TrimSpace(v), " "):
			class = "identifier"
		}
		rows = append(rows, fmt.Sprintf("%s:%d\t%s\t%s", page, line, class, strings.ReplaceAll(v, "\n", `\n`)))
	}

	body := strings.Join(rows, "\n") + "\n"
	fmt.Print(body)
	sum := sha256.Sum256([]byte(body))
	classes := map[string]int{}
	for _, r := range rows {
		classes[strings.Split(r, "\t")[1]]++
	}
	fmt.Fprintf(os.Stderr, "Go literals %d (independent %d); matching %d (independent %d); page script %d; rows %d\n",
		literals, independentLiterals, goRows, independentMatches, len(rows)-goRows, len(rows))
	fmt.Fprintf(os.Stderr, "classes %v\nsha256 %s\n", classes, hex.EncodeToString(sum[:]))
	if literals != independentLiterals || goRows != independentMatches {
		fail(fmt.Errorf("the two tokenizers disagree: a literal was dropped"))
	}
}

// goClass classifies one Go literal by its value and source line.
func goClass(path, line, v string) string {
	switch {
	case path == "internal/cli/grade_ladder.go":
		return "ladder"
	case !strings.Contains(v, " ") && rungLine.MatchString(line) &&
		(regexp.MustCompile(`(?:rung"\]?\s*[:=]\s*|Rung\s*[:=]\s*)"`+regexp.QuoteMeta(v)+`"`).MatchString(line) ||
			strings.Contains(line, "assuranceLadder") || timeRung.MatchString(line)):
		return "rung-value"
	case !strings.Contains(strings.TrimSpace(v), " "):
		return "identifier"
	case receiptFile[path] && !helpLine.MatchString(line),
		path == "internal/cli/deal.go" && strings.HasPrefix(v, "tamper-evident against ourselves"):
		return "receipt-copy"
	}
	return "operator"
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gradestrings:", err)
	os.Exit(1)
}
