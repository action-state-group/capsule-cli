package cli

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/disclosure"
	"github.com/action-state-group/agent-action-capsule/go/emitter"
	"github.com/action-state-group/evidencebook"
	"github.com/spf13/cobra"
)

// `report build` renders a held, disclosed Evidence Bundle whose root is a
// sealed Evidence Result v0 into an offline report.html (2026-09-26:
// one bundle plus the presentation profile; Go twin, no Node at render
// time; the card selects the renderer, presentation feeds the header only).
// It verifies before it renders and refuses a bundle that does not verify
// or whose root is not a Result; it touches no store and seals nothing --
// the disclose act that produced the bundle is already on record.
//
// The page is aac's go/emitter shell around the bundle and the browser
// runtime (the IIFE). The pinned aac Go module ships the shell but not the
// IIFE (dist/ is gitignored upstream and no release carries it), so the
// built IIFE is vendored here with its digest pinned in code; TestIIFEIsPinned
// keeps the two in step and scripts/iife-sync.sh rebuilds it from an aac
// checkout and diffs. The report is only as current as that file.
//
//go:embed assets/evidence-graph.iife.js
var evidenceGraphIIFE []byte

const (
	// evidenceGraphIIFEDigest is the SHA-256 of assets/evidence-graph.iife.js.
	evidenceGraphIIFEDigest = "12cb62afd6c3bcdea2cfdec80190d6bd2600982e682fab561f2941642848c336"
	// evidenceGraphIIFESource names the aac commit and build the vendored
	// IIFE came from (aac main, after #140 and #141 merged).
	evidenceGraphIIFESource = "agent-action-capsule main@ee7888df15f1db7eecfe9da62626d290eeabcce6 ts: npm run emitter:iife (esbuild 0.28.2)"

	// cardExtension records which card the report was built for. There is
	// no card registry yet: every card renders the one Result page today,
	// and the viewer keys on this block once cards exist.
	cardExtension = "report-card/v1"
	// presentationExtension is the bundle-level header block the viewer
	// reads (producer display name, logo, title), and nothing else.
	presentationExtension = "presentation/v1"
)

// reportCards is the closed profile set of the Evidence Contract schema
// (its `profile`-discriminated oneOf; TestReportCardsAreTheContractProfiles
// keeps this list equal to what the schema fixture declares), sorted.
var reportCards = []string{"attribution", "human_role", "obligation", "outcome", "process", "quality", "settlement"}

// presentationBlock is exactly what the viewer's readPresentationBlock
// extracts; a member outside these three never reaches a renderer.
type presentationBlock struct {
	ProducerDisplayName string `json:"producer_display_name,omitempty"`
	LogoDataURL         string `json:"logo_data_url,omitempty"`
	Title               string `json:"title,omitempty"`
}

func readPresentation(path string) (map[string]interface{}, error) {
	raw, err := readInput(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var block presentationBlock
	if err := decoder.Decode(&block); err != nil {
		return nil, hint(ErrInput, "--presentation must be a JSON object with only producer_display_name, logo_data_url and title (all optional strings)")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, inputError("input must contain one JSON value")
	}
	if block.LogoDataURL != "" && !strings.HasPrefix(block.LogoDataURL, "data:image/") {
		return nil, hint(ErrInput, "--presentation logo_data_url must be an inline data:image/ URL; the report loads nothing from the network")
	}
	if block == (presentationBlock{}) {
		return nil, hint(ErrInput, "--presentation names none of producer_display_name, logo_data_url or title")
	}
	out := make(map[string]interface{})
	if block.ProducerDisplayName != "" {
		out["producer_display_name"] = block.ProducerDisplayName
	}
	if block.LogoDataURL != "" {
		out["logo_data_url"] = block.LogoDataURL
	}
	if block.Title != "" {
		out["title"] = block.Title
	}
	return out, nil
}

// verifyHeldBundle runs the neutral verifier with the acceptance the
// bundle verbs apply to what they produce, in one sentence when it fails.
func verifyHeldBundle(value map[string]interface{}) (aacbundle.VerificationResult, error) {
	result := aacbundle.VerifyBundle(value)
	if result.GraphClosure.Status == "fail" || result.IntervalCoverage.Status != "pass" || result.PerRecordMembership.Status != "pass" {
		return result, hint(ErrInput, fmt.Sprintf("the bundle does not verify (graph_closure=%s, interval_coverage=%s, per_record_membership=%s); nothing was rendered", result.GraphClosure.Status, result.IntervalCoverage.Status, result.PerRecordMembership.Status))
	}
	for _, d := range result.Disclosures {
		if d.Status == disclosure.Mismatch || d.Status == disclosure.Ineligible || d.Status == disclosure.NoCommittedDigest {
			return result, hint(ErrInput, fmt.Sprintf("the bundle does not verify: disclosed %s of %s is %s; nothing was rendered", d.Member, d.CapsuleID, d.Status))
		}
	}
	return result, nil
}

// resultRoot is what the root gate establishes: which disclosed member of
// the root carries the Result, in which form, and the document itself.
type resultRoot struct {
	id       string
	member   string
	form     string // payload | book
	document map[string]interface{}
}

// resultCandidates is the viewer's dispatch test (aac ts/src/result-root.ts
// resultDocument) over one record's disclosed members, kept to every member
// that qualifies: agent_output first, then agent_input; a member IS the
// Result when it names itself one (result_version), and agent_input is the
// Result in book form when it is an evidence_result record header whose
// statement is the document (document is nil when the header carries none).
func resultCandidates(id string, members map[string]interface{}) []resultRoot {
	var found []resultRoot
	for _, member := range []string{"agent_output", "agent_input"} {
		payload, ok := members[member].(map[string]interface{})
		if !ok {
			continue
		}
		if payload["result_version"] == resultVersion {
			found = append(found, resultRoot{id: id, member: member, form: "payload", document: payload})
		} else if member == "agent_input" && payload["record_type"] == resultRecordType {
			statement, _ := payload["statement"].(map[string]interface{})
			found = append(found, resultRoot{id: id, member: member, form: "book", document: statement})
		}
	}
	return found
}

// resultRootOf is the Go mirror of the viewer's root gate: the root carries
// the one Result the page is built on, and no other disclosed member of the
// bundle -- the root's other member, or any other record -- carries one, so
// what the report renders is never a matter of which Result was found
// first. It names what it found when the root is not a Result.
func resultRootOf(value map[string]interface{}) (resultRoot, error) {
	id, _ := value["root"].(string)
	found := false
	for _, raw := range asSlice(value["records"]) {
		if record, ok := raw.(map[string]interface{}); ok && record["capsule_id"] == id {
			found = true
		}
	}
	if !found {
		return resultRoot{}, hint(ErrInput, fmt.Sprintf("root %s is not among the bundle's records; nothing was rendered", id))
	}
	disclosures, _ := value["disclosures"].(map[string]interface{})
	members, _ := disclosures[id].(map[string]interface{})
	if len(members) == 0 {
		return resultRoot{}, hint(ErrInput, fmt.Sprintf("root %s is not a sealed Evidence Result v0: the bundle discloses nothing for it (build the bundle with disclose, not bundle)", id))
	}
	if candidates := resultCandidates(id, members); len(candidates) > 0 {
		if len(candidates) > 1 {
			return resultRoot{}, hint(ErrInput, fmt.Sprintf("root %s carries a Result in both its agent_output and its agent_input; a report is rooted on exactly one Result document; nothing was rendered", id))
		}
		root := candidates[0]
		if root.document == nil {
			return resultRoot{}, hint(ErrInput, fmt.Sprintf("root %s is an evidence_result record whose header carries no statement object; nothing was rendered", id))
		}
		others := make([]string, 0, len(disclosures))
		for other := range disclosures {
			if other != id {
				others = append(others, other)
			}
		}
		sort.Strings(others)
		for _, other := range others {
			otherMembers, _ := disclosures[other].(map[string]interface{})
			if second := resultCandidates(other, otherMembers); len(second) > 0 {
				return resultRoot{}, hint(ErrInput, fmt.Sprintf("the bundle carries two Result documents: root %s and record %s (its %s); a report is rooted on exactly one Result, so build the bundle on the one it reports, with a closure that reaches no other; nothing was rendered", id, other, second[0].member))
			}
		}
		return root, nil
	}
	header, _ := members["agent_input"].(map[string]interface{})
	if recordType, ok := header["record_type"].(string); ok {
		return resultRoot{}, hint(ErrInput, fmt.Sprintf("root %s is not a sealed Evidence Result v0: its agent_input is a book record header of record_type %q, not %q", id, recordType, resultRecordType))
	}
	for _, member := range []string{"agent_output", "agent_input"} {
		payload, _ := members[member].(map[string]interface{})
		if family, ok := payload["spec_version"].(string); ok {
			return resultRoot{}, hint(ErrInput, fmt.Sprintf("root %s is not a sealed Evidence Result v0: its %s is a %s document", id, member, family))
		}
	}
	return resultRoot{}, hint(ErrInput, fmt.Sprintf("root %s is not a sealed Evidence Result v0: no disclosed member carries an %s document or an %s record header", id, resultVersion, resultRecordType))
}

func asSlice(value interface{}) []interface{} {
	items, _ := value.([]interface{})
	return items
}

// unsupportedClaims counts the claims whose cited evidence does not all
// resolve in the bundle -- by a record's capsule_id, or by the published
// capsule a disclosed book record header names as its subject. The viewer
// renders such a claim `unsupported`, never met; this only reports how many.
func unsupportedClaims(value map[string]interface{}, checked resultDocument) int {
	held := make(map[string]bool)
	for _, raw := range asSlice(value["records"]) {
		if record, ok := raw.(map[string]interface{}); ok {
			if id, ok := record["capsule_id"].(string); ok {
				held[id] = true
			}
		}
	}
	disclosures, _ := value["disclosures"].(map[string]interface{})
	for _, raw := range disclosures {
		members, _ := raw.(map[string]interface{})
		if header, ok := members["agent_input"].(map[string]interface{}); ok {
			subject, _ := header["subject_ref"].(string)
			recordType, _ := header["record_type"].(string)
			if subject != "" && (recordType == recordTypePublished || (recordType == recordTypeBackfilled && len(asSlice(header["payload_commitments"])) > 0)) {
				held[subject] = true
			}
		}
	}
	unsupported := 0
	for _, claim := range checked.claims {
		supported := len(claim.evidence) > 0
		for _, digest := range claim.evidence {
			supported = supported && held[digest]
		}
		if !supported {
			unsupported++
		}
	}
	return unsupported
}

// setExtension records block under key, refusing to overwrite a different
// block already there: the embedded copy states one card and one header.
func setExtension(value map[string]interface{}, key string, block map[string]interface{}) error {
	extensions, _ := value["extensions"].(map[string]interface{})
	if extensions == nil {
		extensions = make(map[string]interface{})
	}
	if existing, present := extensions[key]; present {
		have, _ := json.Marshal(existing)
		want, _ := json.Marshal(block)
		if !bytes.Equal(have, want) {
			return hint(ErrInput, fmt.Sprintf("the bundle already carries a different %s block (%s); build the report from a bundle without one, or pass what it states", key, have))
		}
	}
	extensions[key] = block
	value["extensions"] = extensions
	return nil
}

type reportBuildResult struct {
	Root              string   `json:"root"`
	Form              string   `json:"form"`
	Member            string   `json:"member"`
	Card              string   `json:"card"`
	Claims            int      `json:"claims"`
	UnsupportedClaims int      `json:"unsupported_claims"`
	ContractRefs      []string `json:"contract_refs"`
	Report            string   `json:"report"`
	Permalink         string   `json:"permalink,omitempty"`
	BundleDigest      string   `json:"bundle_digest"`
	Verification      string   `json:"verification"`
	Draft             bool     `json:"draft"`
}

func reportCommands() *cobra.Command {
	group := &cobra.Command{Use: "report", Short: "Render a verified Result-root Evidence Bundle into an offline report"}
	build := &cobra.Command{Use: "build", Short: "Verify a held Evidence Bundle rooted at a sealed Result v0 and write report.html (and a permalink); touches no store", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		bundlePath, _ := c.Flags().GetString("bundle")
		card, _ := c.Flags().GetString("card")
		out, _ := c.Flags().GetString("out")
		presentationPath, _ := c.Flags().GetString("presentation")
		permalink, _ := c.Flags().GetBool("permalink")
		dryRun, _ := c.Flags().GetBool("dry-run")
		if bundlePath == "" || out == "" {
			return inputError("--bundle and --out are required")
		}
		if i := sort.SearchStrings(reportCards, card); card == "" || i >= len(reportCards) || reportCards[i] != card {
			return hint(ErrInput, fmt.Sprintf("--card must be one of %s (the contract's profile set); got %q", strings.Join(reportCards, ", "), card))
		}
		var presentation map[string]interface{}
		if presentationPath != "" {
			var err error
			if presentation, err = readPresentation(presentationPath); err != nil {
				return err
			}
		}
		raw, err := readInput(bundlePath)
		if err != nil {
			return err
		}
		value, err := decodeBundleJSON(raw)
		if err != nil {
			return err
		}
		// Verify first, then gate the root, then decorate, then render:
		// nothing is written until everything before it has passed.
		if _, err = verifyHeldBundle(value); err != nil {
			return err
		}
		root, err := resultRootOf(value)
		if err != nil {
			return err
		}
		checked, err := checkResult(root.document, "")
		if err != nil {
			return hint(ErrInput, fmt.Sprintf("root %s carries a Result that cannot be rendered: %s", root.id, SafeError(err)))
		}
		if root.form == "book" {
			// The book's own verifier checks the header the record commits
			// and the checkpoint under which it sits.
			verified, err := evidencebook.VerifyBundle(raw)
			if err != nil {
				return hint(ErrInput, "the bundle does not verify as an evidence-book bundle: "+err.Error())
			}
			verifiedRoot := false
			for _, r := range verified.Records {
				verifiedRoot = verifiedRoot || (r.RecordID == root.id && r.HeaderVerified && r.CapsuleOK && verified.Covers(r.Seq))
			}
			if !verifiedRoot {
				return hint(ErrInput, fmt.Sprintf("root %s's disclosed header does not verify against its record, or the record is not under the bundle's checkpoint; nothing was rendered", root.id))
			}
		}
		cardBlock := map[string]interface{}{"card": card}
		if dryRun {
			cardBlock["draft"] = true
		}
		if err = setExtension(value, cardExtension, cardBlock); err != nil {
			return err
		}
		if presentation != nil {
			if err = setExtension(value, presentationExtension, presentation); err != nil {
				return err
			}
		}
		html, err := emitter.EmitEvidenceGraphHTML(value, evidenceGraphIIFE)
		if err != nil {
			return err
		}
		if err = atomicFile(out, []byte(html), false); err != nil {
			return err
		}
		digest, err := aacbundle.BundleDigest(value)
		if err != nil {
			return err
		}
		result := reportBuildResult{Root: root.id, Form: root.form, Member: root.member, Card: card, Claims: len(checked.claims), UnsupportedClaims: unsupportedClaims(value, checked), ContractRefs: checked.contracts, Report: out, BundleDigest: digest, Verification: "pass", Draft: dryRun}
		if permalink && !dryRun {
			// Over the same map the page embeds, so both carriers hold
			// identical bytes.
			fragment, err := aacbundle.EncodeFragment(value)
			if err != nil {
				return err
			}
			decoded, err := aacbundle.DecodeFragment(fragment)
			if err != nil {
				return err
			}
			if _, ok := decoded.(map[string]interface{}); !ok {
				return errors.New("permalink fragment did not round-trip")
			}
			base, _ := c.Flags().GetString("base-url")
			if base == "" {
				base = defaultBundleURL
			}
			result.Permalink = strings.TrimRight(base, "#") + "#" + fragment
		}
		return output(c, result)
	}}
	build.Flags().String("bundle", "", "A held, disclosed Evidence Bundle whose root is a sealed Result v0")
	build.Flags().String("card", "", "Renderer card: "+strings.Join(reportCards, ", ")+" (recorded in the report's report-card/v1 block)")
	build.Flags().String("presentation", "", "presentation/v1 header block JSON: producer_display_name, logo_data_url (data:image/ only), title")
	build.Flags().String("out", "", "Write the offline report.html here (new file)")
	build.Flags().Bool("permalink", false, "Also mint a viewer permalink over the same embedded bundle")
	build.Flags().String("base-url", defaultBundleURL, "Bundle viewer base URL for --permalink")
	build.Flags().Bool("dry-run", false, "Write the page marked draft in its report-card/v1 block and mint no permalink")
	group.AddCommand(build)
	return group
}
