package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	digestA = strings.Repeat("a", 64)
	digestB = strings.Repeat("b", 64)
	digestC = strings.Repeat("c", 64)
)

// pinFile writes a --rules-checker pin file for a checker under the plugin root.
func pinFile(t *testing.T, root, body string) string {
	t.Helper()
	checker := writeLauncher(t, root, "checker", "#!/bin/sh\n", 0o755)
	path := filepath.Join(t.TempDir(), "pin.json")
	require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(body, "CHECKER", checker)), 0o600))
	return path
}

func TestRulesCheckerPinsAPackPerRole(t *testing.T) {
	root := pluginRoot(t)
	var p Profile
	require.NoError(t, pinRulesChecker(&p, pinFile(t, root, `{"command":["CHECKER"],"definition_digest":"`+digestC+`","roles":{
		"buyer":{"pack":"asg/everyday","definition_digest":"`+digestA+`"},
		"seller":{"pack":"asg/seller","definition_digests":["`+digestB+`","`+digestC+`"]}}}`)))

	buyer, seller := p.RulesChecker.ForRole("buyer"), p.RulesChecker.ForRole("seller")
	assert.Equal(t, "asg/everyday", buyer.Pack)
	assert.Equal(t, []string{digestA}, buyer.PinnedDigests())
	assert.Equal(t, "asg/seller", seller.Pack)
	assert.Equal(t, []string{digestB, digestC}, seller.PinnedDigests())
	assert.Empty(t, p.RulesChecker.MissingRoles())
}

// Swapping one role's pin is a profile update of the pin file alone; the other
// role's pin is untouched and no code changes.
func TestSwappingOneRolesPinNeedsOnlyAProfileUpdate(t *testing.T) {
	root := pluginRoot(t)
	before := `{"command":["CHECKER"],"definition_digest":"` + digestC + `","roles":{"buyer":{"pack":"asg/everyday","definition_digest":"` + digestA + `"},"seller":{"pack":"asg/seller","definition_digest":"` + digestB + `"}}}`
	after := strings.Replace(before, `"asg/seller","definition_digest":"`+digestB, `"asg/seller-next","definition_digest":"`+digestC, 1)

	var p Profile
	require.NoError(t, pinRulesChecker(&p, pinFile(t, root, before)))
	require.NoError(t, pinRulesChecker(&p, pinFile(t, root, after)))

	assert.Equal(t, "asg/seller-next", p.RulesChecker.ForRole("seller").Pack)
	assert.Equal(t, []string{digestC}, p.RulesChecker.ForRole("seller").PinnedDigests())
	assert.Equal(t, "asg/everyday", p.RulesChecker.ForRole("buyer").Pack)
	assert.Equal(t, []string{digestA}, p.RulesChecker.ForRole("buyer").PinnedDigests())
}

// A profile without roles checks as before: every role falls back to the flat pin.
func TestRolelessRulesCheckerPinIsEveryRolesDefault(t *testing.T) {
	root := pluginRoot(t)
	var p Profile
	require.NoError(t, pinRulesChecker(&p, pinFile(t, root, `{"command":["CHECKER"],"definition_digest":"`+digestA+`"}`)))
	for _, role := range RulesRoles {
		assert.Equal(t, []string{digestA}, p.RulesChecker.ForRole(role).PinnedDigests(), role)
	}
	assert.Empty(t, p.RulesChecker.MissingRoles())

	// A role not listed falls back to the flat default.
	require.NoError(t, pinRulesChecker(&p, pinFile(t, root, `{"command":["CHECKER"],"definition_digest":"`+digestA+`","roles":{"seller":{"definition_digest":"`+digestB+`"}}}`)))
	assert.Equal(t, []string{digestA}, p.RulesChecker.ForRole("buyer").PinnedDigests())
	assert.Equal(t, []string{digestB}, p.RulesChecker.ForRole("seller").PinnedDigests())
}

func TestRulesCheckerRolesAreValidated(t *testing.T) {
	root := pluginRoot(t)
	for name, body := range map[string]string{
		"unknown role": `{"command":["CHECKER"],"roles":{"broker":{"pack":"x"}}}`,
		"empty role":   `{"command":["CHECKER"],"roles":{"buyer":{}}}`,
		"bad digest":   `{"command":["CHECKER"],"roles":{"buyer":{"definition_digest":"nope"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var p Profile
			assert.Error(t, pinRulesChecker(&p, pinFile(t, root, body)))
		})
	}
}

// A role that names a pack but no digest keeps the flat digests, so naming a
// pack never loosens the digest check.
func TestRolePackWithoutDigestKeepsTheFlatDigests(t *testing.T) {
	root := pluginRoot(t)
	var p Profile
	require.NoError(t, pinRulesChecker(&p, pinFile(t, root, `{"command":["CHECKER"],"definition_digest":"`+digestA+`","roles":{"buyer":{"pack":"asg/x"}}}`)))
	buyer := p.RulesChecker.ForRole("buyer")
	assert.Equal(t, "asg/x", buyer.Pack)
	assert.Equal(t, []string{digestA}, buyer.PinnedDigests())
}

func TestProfileValidationRefusesBadRoleEntries(t *testing.T) {
	good := ProfileRulesRole{Pack: "asg/x", DefinitionDigest: digestA}
	for name, roles := range map[string]map[string]ProfileRulesRole{
		"unknown role":    {"broker": good},
		"empty entry":     {"buyer": {}},
		"both digests":    {"buyer": {DefinitionDigest: digestA, DefinitionDigests: []string{digestB, digestC}}},
		"bad digest":      {"buyer": {DefinitionDigest: "nope"}},
		"bad digest list": {"buyer": {DefinitionDigests: []string{digestA, "nope"}}},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := profileFixture(t)
			p.RulesChecker = ProfileRulesChecker{Command: []string{"/x"}, DefinitionDigest: digestA, Roles: roles}
			assert.Error(t, p.validate())
		})
	}
	p, _ := profileFixture(t)
	p.RulesChecker = ProfileRulesChecker{Command: []string{"/x"}, DefinitionDigest: digestB, Roles: map[string]ProfileRulesRole{"buyer": good}}
	assert.NoError(t, p.validate())
}

// Plain doctor reports each role's pin; a pack-only role shows the flat digests it keeps.
func TestDoctorReportsPerRolePins(t *testing.T) {
	root := pluginRoot(t)
	owned := ownedProfile(t, "jsonl", "ledger")
	pin := pinFile(t, root, `{"command":["CHECKER"],"definition_digest":"`+digestC+`","roles":{"buyer":{"pack":"asg/everyday","definition_digest":"`+digestA+`"},"seller":{"pack":"asg/seller"}}}`)
	out, err := invoke(t, "", "profile", "update", "--profile", owned.Name, "--rules-checker", pin)
	require.NoError(t, err, out)

	checker := doctorReport(t, "--profile", owned.Name)["profile"].(map[string]any)["rules_checker"].(map[string]any)
	assert.NotContains(t, checker, "notice")
	roles := checker["roles"].(map[string]any)
	assert.Equal(t, []any{digestA}, roles["buyer"].(map[string]any)["definition_digests"])
	assert.Equal(t, "asg/seller", roles["seller"].(map[string]any)["pack"])
	assert.Equal(t, []any{digestC}, roles["seller"].(map[string]any)["definition_digests"])
	assert.NotContains(t, roles["seller"], "notice")
	assert.NotContains(t, checker, "missing_roles")
}

// MissingRoles names a role with no digest to check against.
func TestMissingRolesNamesARoleWithoutADigest(t *testing.T) {
	r := ProfileRulesChecker{Roles: map[string]ProfileRulesRole{"buyer": {Pack: "asg/x", DefinitionDigest: digestA}, "seller": {Pack: "asg/y"}}}
	assert.Equal(t, []string{"seller"}, r.MissingRoles())
	assert.Empty(t, ProfileRulesChecker{DefinitionDigest: digestA}.MissingRoles())
}

// Until the deal plugin checks per role, a profile holding roles without a
// flat default would run unpinned, so it does not validate (remove with
// evidencebook-deal#66).
func TestProfileWithRolesNeedsAFlatDefaultUntilTheDealPluginChecksPerRole(t *testing.T) {
	p, _ := profileFixture(t)
	p.RulesChecker = ProfileRulesChecker{Command: []string{"/x"}, Roles: map[string]ProfileRulesRole{"buyer": {Pack: "asg/x", DefinitionDigest: digestA}}}
	err := p.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evidencebook-deal#66")
	p.RulesChecker.DefinitionDigest = digestB
	assert.NoError(t, p.validate())
}

// Install-check says nothing about roles for a checker pinned without them.
func TestInstallCheckIgnoresRolesWhenNoneArePinned(t *testing.T) {
	root := pluginRoot(t)
	owned := ownedProfile(t, "jsonl", "ledger")
	out, err := invoke(t, "", "profile", "update", "--profile", owned.Name, "--rules-checker", pinFile(t, root, `{"command":["CHECKER"]}`))
	require.NoError(t, err, out)
	check, _ := installCheckOf(t, owned.Name, t.TempDir())
	for _, issue := range check["issues"].([]any) {
		assert.NotContains(t, issue, "rules pack")
	}
}

// Until the deal plugin checks per role, roles without a flat default would
// run unpinned, so the pin file is refused (remove with evidencebook-deal#66).
func TestRolesNeedAFlatDefaultUntilTheDealPluginChecksPerRole(t *testing.T) {
	root := pluginRoot(t)
	var p Profile
	err := pinRulesChecker(&p, pinFile(t, root, `{"command":["CHECKER"],"roles":{"buyer":{"pack":"asg/x","definition_digest":"`+digestA+`"}}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "evidencebook-deal#66")
}
