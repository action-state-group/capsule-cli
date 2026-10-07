package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
)

// materialityPredicateType is the one predicate document type this version
// reads.
const materialityPredicateType = "materiality-predicate/v0"

// materialityPredicate decides which of the agent's own picks are material,
// so that a check pauses for the user's nod on them. It is data: a profile or
// a pack supplies it, capsulectl only evaluates it. Deal computes the facts
// (what the user asked, what the agent picked); the predicate says which
// picks matter.
//
// An attribute the agent picked is material when it matches at least one
// matcher in Material.
type materialityPredicate struct {
	Type     string               `json:"type"`
	Name     string               `json:"name"`
	Version  string               `json:"version"`
	Material []materialityMatcher `json:"material"`
	// Digest is the SHA-256 of the document's JCS bytes: which predicate
	// applied, as a record will cite it.
	Digest string `json:"-"`
}

// materialityMatcher matches an attribute by its field: exactly one of Field
// (the whole field, e.g. "quantity" or "conditions.size") or FieldPrefix
// (e.g. "conditions."), the latter optionally narrowed to names containing
// any of NameContainsAny (case-insensitive) after the prefix. ExceptValues
// are values of a matching attribute that are not material.
type materialityMatcher struct {
	Field           string   `json:"field,omitempty"`
	FieldPrefix     string   `json:"field_prefix,omitempty"`
	NameContainsAny []string `json:"name_contains_any,omitempty"`
	ExceptValues    []string `json:"except_values,omitempty"`
}

// parseMaterialityPredicate reads a predicate document strictly: an unknown
// member, another type, a matcher that names no field or both kinds, or an
// empty narrowing word is refused. A predicate that cannot be read never
// falls back to anything.
func parseMaterialityPredicate(where string, raw []byte) (*materialityPredicate, error) {
	refuse := func(why string) error {
		return inputError("materiality predicate " + where + ": " + why)
	}
	var p materialityPredicate
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return nil, refuse(err.Error())
	}
	if decoder.More() {
		return nil, refuse("more than one JSON value")
	}
	switch {
	case p.Type != materialityPredicateType:
		return nil, refuse(fmt.Sprintf("type must be %q", materialityPredicateType))
	case strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Version) == "":
		return nil, refuse("name and version are required")
	case p.Material == nil:
		return nil, refuse(`"material" is required (an empty list means nothing the agent picks is material)`)
	}
	for i, m := range p.Material {
		at := fmt.Sprintf("material[%d]: ", i)
		switch {
		case (m.Field == "") == (m.FieldPrefix == ""):
			return nil, refuse(at + "exactly one of field or field_prefix")
		case m.Field != "" && len(m.NameContainsAny) > 0:
			return nil, refuse(at + "name_contains_any goes with field_prefix")
		}
		for _, w := range append(slices.Clone(m.NameContainsAny), m.ExceptValues...) {
			if strings.TrimSpace(w) == "" {
				return nil, refuse(at + "an empty word")
			}
		}
	}
	var generic any
	if err := decodeJSONPreserveNumbers("the materiality predicate", raw, &generic); err != nil {
		return nil, refuse(err.Error())
	}
	digest, err := canonical.JSONDigest(generic)
	if err != nil {
		return nil, refuse(err.Error())
	}
	p.Digest = digest
	return &p, nil
}

// loadMaterialityPredicate reads the predicate at path, or returns nil when
// none is configured (path empty): the check then fails safe.
func loadMaterialityPredicate(path string) (*materialityPredicate, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := readInput(path)
	if err != nil {
		return nil, err
	}
	return parseMaterialityPredicate(path, raw)
}

// pinMateriality sets p's materiality predicate to the one at path, pinned by
// its digest, or removes it when path is empty. Only the user does this (deal
// init, profile update): which picks pause is policy.
func pinMateriality(p *Profile, path string) error {
	if path == "" {
		p.Materiality.Predicate, p.Materiality.Digest = "", ""
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	predicate, err := loadMaterialityPredicate(abs)
	if err != nil {
		return err
	}
	p.Materiality.Predicate, p.Materiality.Digest = abs, predicate.Digest
	return nil
}

// pinnedMateriality is the predicate a check of p evaluates: the one p
// pins, refused unless it still has the pinned digest; nil when p pins none
// (the check fails safe). Nothing about the check run can choose another.
func pinnedMateriality(p Profile) (*materialityPredicate, error) {
	m := p.Materiality
	switch {
	case m.Predicate == "" && m.Digest == "":
		return nil, nil
	case m.Predicate == "" || m.Digest == "":
		return nil, inputError("the profile's materiality predicate is not pinned: set it with `capsulectl --profile " + p.Name + " profile update --materiality FILE` (the user's to run)")
	}
	if _, err := os.Stat(m.Predicate); errors.Is(err, fs.ErrNotExist) {
		return nil, inputError("materiality predicate file missing (" + m.Predicate + "): re-pin it with `capsulectl --profile " + p.Name + " profile update --materiality FILE`, which is the user's to run")
	}
	predicate, err := loadMaterialityPredicate(m.Predicate)
	if err != nil {
		return nil, err
	}
	if predicate.Digest != m.Digest {
		return nil, inputError("materiality predicate changed since it was pinned (" + m.Predicate + "): re-pin it with `capsulectl --profile " + p.Name + " profile update --materiality FILE`, which is the user's to run")
	}
	return predicate, nil
}

// dealMateriality is which predicate decided a check's pauses on the agent's
// picks: its name, version and digest, or digest "none" when no predicate
// was configured and every pick paused (fail safe).
type dealMateriality struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest"`
}

// materialityLabel names a predicate for the card: its name and version, or
// that none was configured.
func materialityLabel(m dealMateriality) string {
	if m.Digest == "none" || m.Name == "" {
		return "none: every pick asked about"
	}
	return m.Name + " " + m.Version
}

func (p *materialityPredicate) ref() dealMateriality {
	if p == nil {
		return dealMateriality{Digest: "none"}
	}
	return dealMateriality{Name: p.Name, Version: p.Version, Digest: p.Digest}
}

// material reports whether an attribute the agent picked is material. With
// no predicate configured (p nil) every pick is: the check fails safe, and
// the user is asked about everything the agent chose on its own.
func (p *materialityPredicate) material(a dealAttribute) bool {
	if p == nil {
		return true
	}
	for _, m := range p.Material {
		if m.matches(a) {
			return true
		}
	}
	return false
}

func (m materialityMatcher) matches(a dealAttribute) bool {
	switch {
	case m.Field != "":
		if a.Field != m.Field {
			return false
		}
	default:
		name, ok := strings.CutPrefix(a.Field, m.FieldPrefix)
		if !ok {
			return false
		}
		if len(m.NameContainsAny) > 0 {
			hit := false
			for _, w := range m.NameContainsAny {
				hit = hit || strings.Contains(strings.ToLower(name), strings.ToLower(w))
			}
			if !hit {
				return false
			}
		}
	}
	for _, v := range m.ExceptValues {
		if a.Value == v {
			return false
		}
	}
	return true
}

// materialityOpenings are, for each step that committed a materiality
// predicate's name and version, the step and the opening of its
// label_commitment: the nonce, name and version. Only the user's own copy
// carries them.
func materialityOpenings(events []sealedEvent) []interface{} {
	out := []interface{}{}
	for _, se := range events {
		nonce := se.Event.Nonces["materiality"]
		if nonce == "" {
			continue
		}
		var m dealMateriality
		switch {
		case se.Event.Open != nil:
			m = se.Event.Open.Materiality
		case se.Event.Check != nil:
			m = se.Event.Check.Materiality
		default:
			continue
		}
		out = append(out, map[string]interface{}{"step": se.CapsuleID, "nonce": nonce, "name": m.Name, "version": m.Version})
	}
	return out
}
