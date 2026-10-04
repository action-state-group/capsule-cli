package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Which skill instructions were present when a deal opened.
//
// `deal open --skill FILE` digests the SKILL.md the agent says it is
// following and seals the digest in the deal's baseline, with how many other
// copies of the same skill sit beside it in the skills folder. Like a
// toolset digest, this is a boundary marker, not a detector: the agent
// computes it, so it catches accidents (a stale backup the host loaded, a
// partial upgrade, skill files that drifted from the binary), not a
// dishonest agent, and it records which instructions were present, never
// that the agent followed them. The paths stay on this device.

// dealSkillCaveat is said wherever the digest is shown.
const dealSkillCaveat = "This records which skill instructions were present when the deal opened, as the agent reported them: a marker for accidents such as a stale copy, not proof the agent followed them."

// dealSkillEnv names the SKILL.md when --skill is not given.
const dealSkillEnv = "CAPSULE_DEAL_SKILL"

type dealSkill struct {
	Digest      string   `json:"skill_md_digest"`
	OtherCopies int      `json:"other_copies"`
	Path        string   `json:"path,omitempty"`   // local only
	Others      []string `json:"others,omitempty"` // local only
}

func readSkillFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, errors.New("too large for a SKILL.md")
	}
	return raw, nil
}

// readDealSkill digests the SKILL.md at path and finds the other copies of
// the same skill in the folders beside its own.
func readDealSkill(path string) (*dealSkill, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := readSkillFile(abs)
	if err != nil {
		return nil, hint(ErrInput, "--skill: cannot read the SKILL.md ("+err.Error()+")")
	}
	name := skillName(abs)
	if name == "" {
		return nil, hint(ErrInput, "--skill: the file has no frontmatter name: line; pass the SKILL.md the agent is following")
	}
	sum := sha256.Sum256(raw)
	s := &dealSkill{Digest: hex.EncodeToString(sum[:]), Path: abs}
	own := filepath.Dir(abs)
	entries, _ := os.ReadDir(filepath.Dir(own))
	for _, e := range entries {
		dir := filepath.Join(filepath.Dir(own), e.Name())
		if dir == own || !e.IsDir() {
			continue
		}
		if skillName(filepath.Join(dir, "SKILL.md")) == name {
			s.Others = append(s.Others, filepath.Join(dir, "SKILL.md"))
		}
	}
	sort.Strings(s.Others)
	s.OtherCopies = len(s.Others)
	return s, nil
}

func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// previousSkill is the skill recorded by the deal opened most recently
// before this one, or nil when there is none or it recorded none.
func (s *dealSession) previousSkill(ctx context.Context, dealID, openedAt string) (*dealSkill, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT deal_id, local FROM deal_steps WHERE n = 1 AND deal_id <> ?`, dealID)
	if err != nil {
		return nil, err
	}
	var best *dealEvent
	for rows.Next() {
		var id, local string
		if err = rows.Scan(&id, &local); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var ev dealEvent
		if json.Unmarshal([]byte(local), &ev) != nil || ev.Open == nil || ev.At > openedAt {
			continue
		}
		if best == nil || ev.At > best.At || (ev.At == best.At && ev.DealID > best.DealID) {
			e := ev
			best = &e
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil || best == nil {
		return nil, err
	}
	return best.Open.Skill, nil
}

// dealReportFor is the deal's report with what its skill record shows: the
// instructions line, and anomalies for another visible copy of the skill
// and for a change since the previous deal.
func (s *dealSession) dealReportFor(ctx context.Context, events []sealedEvent) (dealReport, error) {
	report := buildDealReport(events)
	open := events[0].Event
	skill := open.Open.Skill
	if skill == nil {
		report.Instructions = "Which skill instructions were present was not recorded (the deal was opened without --skill)."
		return report, nil
	}
	report.Instructions = "Instructions present when the deal opened: deal SKILL.md sha256:" + shortDigest(skill.Digest) + ". " + dealSkillCaveat
	base := []string{events[0].CapsuleID}
	if skill.OtherCopies > 0 {
		report.Anomalies = append(report.Anomalies, dealReportItem{Side: "agent", Kind: "skill_copies_visible",
			Text: "Another copy of the deal skill was visible to the agent when this deal opened: an old copy may be the one it followed", Steps: base})
	}
	prev, err := s.previousSkill(ctx, open.DealID, open.At)
	if err != nil {
		return report, err
	}
	if prev != nil && prev.Digest != skill.Digest {
		report.Anomalies = append(report.Anomalies, dealReportItem{Side: "agent", Kind: "skill_changed",
			Text: "The skill instructions changed since the previous deal (sha256:" + shortDigest(prev.Digest) + " → sha256:" + shortDigest(skill.Digest) + ")", Steps: base})
	}
	return report, nil
}
