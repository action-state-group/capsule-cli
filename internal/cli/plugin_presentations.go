package cli

import (
	"bytes"
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

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A plugin may contribute presentations: modules a page builder can put in a
// page (agent-action-capsule's presentation contract,
// aac.presentation-manifest/v0). The cli-plugin/v1 handshake carries them as
// an optional member that an older capsulectl ignores:
//
//	"presentations": [{"manifest": {…}, "files": [{"role": "script", "path": "…", "sha256": "…"}]}]
//
// The manifest is the module's identity: its id is the module id, its
// trust_class says whether it is code (trusted-executable) or a manifest and
// wording only (declarative), and its presentation_api and runtime_min say
// which presentation runtime it needs, independently of cli-plugin/v1.
//
// capsulectl reads every file itself: a path relative to the launcher's own
// directory, which must stay inside it, held to the same ownership and
// permission rules as the launcher, read up to presentationFileLimit bytes and
// hashed. Each digest must equal the one the file entry states and the one the
// manifest pins for that role. Any failure refuses all of that plugin's
// presentations, with the reason; its subcommands are unaffected.
//
// Presentations load only from the two built-in trusted roots, never from
// roots CAPSULECTL_PLUGIN_ROOTS names: a presentation's code runs in every
// page it is put in, so no environment variable may widen where it comes from.

//go:embed assets/presentation/presentation-manifest-v0.json
var presentationManifestSchema []byte

// presentationFileLimit caps one presentation file.
const presentationFileLimit = 1 << 20

// presentationRoots are where presentations may come from: the built-in
// trusted roots. A test replaces it; nothing else does.
var presentationRoots = builtinPluginRoots

func builtinPluginRoots() []string {
	roots := []string{"/usr/local/lib/capsulectl/plugins"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".local", "lib", "capsulectl", "plugins"))
	}
	return roots
}

// presentationRefusal is why a plugin's presentations were refused: a
// machine-readable reason, the file it concerns (when one), and the detail.
// The reasons: untrusted_root, invalid_manifest, invalid_files,
// unsupported_carrier, duplicate_id, path_escape, not_regular_file, writable,
// missing_file, oversize, digest_mismatch, invalid_wording, unreadable.
type presentationRefusal struct {
	Reason string `json:"reason"`
	File   string `json:"file,omitempty"`
	Detail string `json:"detail"`
}

func (r *presentationRefusal) Error() string { return r.Detail }

func refuse(reason, file, format string, args ...interface{}) error {
	return &presentationRefusal{Reason: reason, File: file, Detail: fmt.Sprintf(format, args...)}
}

// asRefusal is err as a refusal; an error loadPresentations did not classify
// (the launcher could not be resolved, the schema could not be read) is
// "unreadable".
func asRefusal(err error) *presentationRefusal {
	if r, ok := err.(*presentationRefusal); ok {
		return r
	}
	return &presentationRefusal{Reason: "unreadable", Detail: err.Error()}
}

type pluginPresentation struct {
	Manifest json.RawMessage          `json:"manifest"`
	Files    []pluginPresentationFile `json:"files"`
}

type pluginPresentationFile struct {
	Role   string `json:"role"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// presentationModule is a presentation a plugin contributed, checked: only
// loadPresentations makes one. Its files are the bytes capsulectl read and
// hashed, by role.
type presentationModule struct {
	ID         string
	TrustClass string
	Manifest   map[string]interface{}
	Script     *presentationFile
	Styles     []presentationFile
	Wording    *presentationFile
	plugin     string
}

type presentationFile struct {
	SHA256 string
	Bytes  []byte
}

var (
	presentationSchemasOnce sync.Once
	presentationSchemas     map[string]*jsonschema.Schema
	presentationSchemasErr  error
	lowerHexDigest          = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func presentationSchema(def string) (*jsonschema.Schema, error) {
	presentationSchemasOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(presentationManifestSchema))
		if err != nil {
			presentationSchemasErr = err
			return
		}
		const url = "https://agentactioncapsule.org/schemas/presentation-manifest-v0.json"
		c := jsonschema.NewCompiler()
		if err = c.AddResource(url, doc); err != nil {
			presentationSchemasErr = err
			return
		}
		presentationSchemas = map[string]*jsonschema.Schema{}
		for name, ref := range map[string]string{"manifest": url, "wording": url + "#/$defs/WordingPack"} {
			if presentationSchemas[name], err = c.Compile(ref); err != nil {
				presentationSchemasErr = err
				return
			}
		}
	})
	return presentationSchemas[def], presentationSchemasErr
}

// loadPresentations checks a plugin's presentations against its launcher's
// directory. It returns them all, or none and the reason.
func loadPresentations(info pluginInfo) ([]presentationModule, error) {
	if len(info.Presentations) == 0 {
		return nil, nil
	}
	launcher, err := filepath.EvalSymlinks(info.path)
	if err != nil {
		return nil, err
	}
	if root := rootOf(launcher, presentationRoots()); root == "" {
		return nil, refuse("untrusted_root", "", "presentations load only from the built-in plugin roots (%s), and %s is not under one", strings.Join(presentationRoots(), ", "), launcher)
	}
	dir := filepath.Dir(launcher)
	seen := map[string]bool{}
	var out []presentationModule
	for i, p := range info.Presentations {
		m, err := loadPresentation(dir, p)
		if err != nil {
			if r, ok := err.(*presentationRefusal); ok {
				r.Detail = fmt.Sprintf("presentation %d: %s", i, r.Detail)
			}
			return nil, err
		}
		if seen[m.ID] {
			return nil, refuse("duplicate_id", "", "presentation %d: the module id %s is given twice", i, m.ID)
		}
		seen[m.ID] = true
		m.plugin = info.Name
		out = append(out, m)
	}
	return out, nil
}

func rootOf(path string, roots []string) string {
	for _, root := range roots {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if path == rr || strings.HasPrefix(path, rr+string(os.PathSeparator)) {
			return rr
		}
	}
	return ""
}

func loadPresentation(dir string, p pluginPresentation) (presentationModule, error) {
	var m presentationModule
	schema, err := presentationSchema("manifest")
	if err != nil {
		return m, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(p.Manifest))
	if err != nil {
		return m, refuse("invalid_manifest", "", "the manifest is not JSON")
	}
	if err = schema.Validate(doc); err != nil {
		return m, refuse("invalid_manifest", "", "the manifest is not an aac.presentation-manifest/v0: %s", firstSchemaError(err))
	}
	if err = json.Unmarshal(p.Manifest, &m.Manifest); err != nil {
		return m, err
	}
	m.ID, _ = m.Manifest["id"].(string)
	m.TrustClass, _ = m.Manifest["trust_class"].(string)
	var script string
	var styles []string
	var wording string
	switch m.TrustClass {
	case "trusted-executable":
		exec, _ := m.Manifest["executable"].(map[string]interface{})
		if exec["carrier"] != "module-slot" {
			return m, refuse("unsupported_carrier", "", "%s: a plugin's module is carried in the module slot; core-runtime modules ship inside the runtime", m.ID)
		}
		script, _ = exec["script_sha256"].(string)
		for _, s := range asList(exec["style_sha256"]) {
			styles = append(styles, s.(string))
		}
		wording, _ = exec["wording_sha256"].(string)
	case "declarative":
		decl, _ := m.Manifest["declarative"].(map[string]interface{})
		wording, _ = decl["wording_sha256"].(string)
	}
	byRole := map[string][]pluginPresentationFile{}
	for _, f := range p.Files {
		byRole[f.Role] = append(byRole[f.Role], f)
	}
	for role := range byRole {
		if role != "script" && role != "style" && role != "wording" {
			return m, refuse("invalid_files", "", "%s: unknown file role %q", m.ID, role)
		}
	}
	read := func(f pluginPresentationFile) (presentationFile, error) {
		if !lowerHexDigest.MatchString(f.SHA256) {
			return presentationFile{}, refuse("invalid_files", f.Path, "%s: %s's sha256 is not 64 lowercase hex", m.ID, f.Path)
		}
		bytes, err := readPresentationFile(dir, f.Path)
		if err != nil {
			if r, ok := err.(*presentationRefusal); ok {
				r.Detail = m.ID + ": " + r.Detail
			}
			return presentationFile{}, err
		}
		sum := sha256.Sum256(bytes)
		if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
			return presentationFile{}, refuse("digest_mismatch", f.Path, "%s: %s hashes to %s, not the %s it states", m.ID, f.Path, got, f.SHA256)
		}
		return presentationFile{SHA256: f.SHA256, Bytes: bytes}, nil
	}

	// The script: exactly one for a module-slot module, none otherwise.
	switch scripts := byRole["script"]; {
	case script == "" && len(scripts) > 0:
		return m, refuse("invalid_files", "", "%s: a %s module carries no script", m.ID, m.TrustClass)
	case script != "" && len(scripts) != 1:
		return m, refuse("invalid_files", "", "%s: the manifest pins one script; the plugin lists %d", m.ID, len(scripts))
	case script != "":
		f, err := read(scripts[0])
		if err != nil {
			return m, err
		}
		if f.SHA256 != script {
			return m, refuse("digest_mismatch", scripts[0].Path, "%s: the script is %s, not the script_sha256 the manifest pins", m.ID, f.SHA256)
		}
		m.Script = &f
	}
	// The stylesheets: exactly the set the manifest pins.
	var gotStyles []string
	for _, s := range byRole["style"] {
		f, err := read(s)
		if err != nil {
			return m, err
		}
		m.Styles = append(m.Styles, f)
		gotStyles = append(gotStyles, f.SHA256)
	}
	sort.Strings(gotStyles)
	sort.Strings(styles)
	if !slices.Equal(gotStyles, styles) {
		return m, refuse("digest_mismatch", "", "%s: the stylesheets listed are not the style_sha256 the manifest pins", m.ID)
	}
	// The wording pack: the one the manifest pins, a valid wording pack.
	switch words := byRole["wording"]; {
	case wording == "" && len(words) > 0:
		return m, refuse("invalid_files", "", "%s: the manifest pins no wording pack", m.ID)
	case wording != "" && len(words) != 1:
		return m, refuse("invalid_files", "", "%s: the manifest pins one wording pack; the plugin lists %d", m.ID, len(words))
	case wording != "":
		f, err := read(words[0])
		if err != nil {
			return m, err
		}
		if f.SHA256 != wording {
			return m, refuse("digest_mismatch", words[0].Path, "%s: the wording pack is %s, not the wording_sha256 the manifest pins", m.ID, f.SHA256)
		}
		pack, err := jsonschema.UnmarshalJSON(bytes.NewReader(f.Bytes))
		if err != nil {
			return m, refuse("invalid_wording", words[0].Path, "%s: the wording pack is not JSON", m.ID)
		}
		schema, err := presentationSchema("wording")
		if err != nil {
			return m, err
		}
		if err = schema.Validate(pack); err != nil {
			return m, refuse("invalid_wording", words[0].Path, "%s: the wording pack is not valid: %s", m.ID, firstSchemaError(err))
		}
		m.Wording = &f
	}
	return m, nil
}

func asList(v interface{}) []interface{} {
	list, _ := v.([]interface{})
	return list
}

// readPresentationFile reads a file a plugin names, relative to its
// launcher's directory: it must stay inside that directory once symlinks are
// resolved, be a regular file under the launcher's own trust rules, and be at
// most presentationFileLimit bytes.
func readPresentationFile(dir, rel string) ([]byte, error) {
	if rel == "" || filepath.IsAbs(rel) || rel != filepath.Clean(rel) || strings.HasPrefix(rel, "..") {
		return nil, refuse("path_escape", rel, "%q is not a plain path relative to the plugin's directory", rel)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(dir, rel))
	if err != nil {
		return nil, refuse("missing_file", rel, "cannot resolve %s: %v", rel, err)
	}
	if !strings.HasPrefix(resolved, dir+string(os.PathSeparator)) {
		return nil, refuse("path_escape", rel, "%s resolves to %s, outside the plugin's directory", rel, resolved)
	}
	// The trust walk stops at the built-in root the plugin is under, never at
	// a root CAPSULECTL_PLUGIN_ROOTS names.
	if err = verifyTrustedPathUnder(resolved, presentationRoots()); err != nil {
		reason := "path_escape"
		switch {
		case errors.Is(err, errNotRegularFile):
			reason = "not_regular_file"
		case errors.Is(err, errWritable), errors.Is(err, errNotTrustedOwner):
			reason = "writable"
		}
		return nil, refuse(reason, rel, "%v", err)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, presentationFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > presentationFileLimit {
		return nil, refuse("oversize", rel, "%s is larger than %d bytes", rel, presentationFileLimit)
	}
	return data, nil
}
