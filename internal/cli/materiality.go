package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
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
