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
	require.NoError(t, pinRulesChecker(&p, pinFile(t, root, `{"command":["CHECKER"],"roles":{
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
	before := `{"command":["CHECKER"],"roles":{"buyer":{"pack":"asg/everyday","definition_digest":"` + digestA + `"},"seller":{"pack":"asg/seller","definition_digest":"` + digestB + `"}}}`
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

// doctor --install-check names each role with no pin at all.
func TestInstallCheckReportsAMissingPinPerRole(t *testing.T) {
	root := pluginRoot(t)
	owned := ownedProfile(t, "jsonl", "ledger")
	pin := pinFile(t, root, `{"command":["CHECKER"],"roles":{"buyer":{"pack":"asg/everyday","definition_digest":"`+digestA+`"}}}`)
	out, err := invoke(t, "", "profile", "update", "--profile", owned.Name, "--rules-checker", pin)
	require.NoError(t, err, out)

	check, err := installCheckOf(t, owned.Name, t.TempDir())
	require.ErrorIs(t, err, ErrPartial)
	assert.Contains(t, check["issues"], "profile "+owned.Name+" pins no rules pack for the seller role: capsulectl profile update --profile "+owned.Name+" --rules-checker FILE with roles.seller")
	for _, issue := range check["issues"].([]any) {
		assert.NotContains(t, issue, "buyer role")
	}
}
