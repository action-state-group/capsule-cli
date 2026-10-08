package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A page that checks itself (`bundle --html`, `disclose --html`, `deal report
// --html`) carries a verdict its reader takes at face value. The vendored
// viewer's banner says the bundle passed whenever its own checks pass, which
// is more than `verify --bundle` may say about the same bundle, and it shows
// no rows, without failing, when the report the bundle carries cannot be
// built. So capsulectl writes a page only when it can stand behind what the
// page shows: the bundle is VALID by `verify --bundle`'s own verdict
// (bundleVerdict), and a report/v1 root shows every one of its rows.

// pageGate refuses a page for a bundle that is not VALID, or whose
// report/v1 root would show fewer rows than it has. The bundle is judged as
// `verify --bundle` reads it from a file (its JSON, decoded the same way), so
// the page and the file can never get different verdicts.
func pageGate(value map[string]interface{}) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if value, err = decodeBundleJSON(raw); err != nil {
		return err
	}
	verdict, result := bundleVerdict(value, nil)
	if verdict != "VALID" {
		reason := "verification incomplete"
		class := ErrPartial
		if verdict == "INVALID" {
			reason, class = "verification failed", ErrBundleInvalid
		}
		return hint(class, fmt.Sprintf("no page written: `verify --bundle` calls this bundle %s (%s: %s), and a page must never claim more than that; write the bundle with --out and check it with `capsulectl verify --bundle`",
			verdict, reason, strings.Join(unmetClaims(result), ", ")))
	}
	if shown, total, ok := reportRowsShown(value); ok && (total == 0 || shown < total) {
		return hint(ErrPartial, fmt.Sprintf("no page written: its report would show %d of %d rows, because the records its rows cite are not in this bundle", shown, total))
	}
	return nil
}

// unmetClaims names each claim of a bundleVerdict result that did not pass,
// in a stable order, for the refusal message.
func unmetClaims(result map[string]any) []string {
	var unmet []string
	for name, v := range result {
		c, ok := v.(claimOutput)
		if !ok || c.Status == "pass" {
			continue
		}
		unmet = append(unmet, name+" "+c.Status)
	}
	sort.Strings(unmet)
	if len(unmet) == 0 {
		unmet = []string{"a record or disclosure did not check out"}
	}
	return unmet
}

// reportRowsShown counts, for a bundle whose root discloses a report/v1
// payload, its rows and those whose every citation is a record in the
// bundle: what the viewer's report page can show. ok is false when the root
// is not a report/v1 document (another root family, which has no rows).
func reportRowsShown(value map[string]interface{}) (shown, total int, ok bool) {
	root, _ := value["root"].(string)
	disclosures, _ := value["disclosures"].(map[string]interface{})
	members, _ := disclosures[root].(map[string]interface{})
	report, _ := members["agent_input"].(map[string]interface{})
	if report["spec_version"] != "report/v1" {
		return 0, 0, false
	}
	inBundle := map[string]bool{}
	records, _ := value["records"].([]interface{})
	for _, r := range records {
		if m, isMap := r.(map[string]interface{}); isMap {
			if id, isString := m["capsule_id"].(string); isString {
				inBundle[id] = true
			}
		}
	}
	rows, _ := report["rows"].([]interface{})
	for _, r := range rows {
		row, _ := r.(map[string]interface{})
		refs, _ := row["references"].([]interface{})
		resolved := true
		for _, x := range refs {
			ref, _ := x.(map[string]interface{})
			if digest, _ := ref["digest"].(string); !inBundle[digest] {
				resolved = false
			}
		}
		if resolved {
			shown++
		}
	}
	return shown, len(rows), true
}

// errJSONLPage refuses a page on a jsonl profile: its bundles carry the
// evidence book's records about the capsules it sealed, not the capsules
// themselves, so a page can never show what was sealed.
var errJSONLPage = hint(ErrInput, "no page written: a jsonl profile's bundle carries its evidence book's records about what you sealed, not the records themselves, so a page cannot show them; publish on a sqlite profile for a page (the README's self-checking report example), or write the bundle with --out")
