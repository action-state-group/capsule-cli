package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyMCPHints(t *testing.T) {
	truth, lie := true, false
	classification, _ := classifyMCPHints(&truth, nil)
	assert.Equal(t, classificationObservation, classification)

	classification, _ = classifyMCPHints(&lie, nil)
	assert.Equal(t, classificationEffect, classification)

	classification, _ = classifyMCPHints(&truth, &truth)
	assert.Equal(t, classificationEffect, classification, "destructiveHint=true always wins over readOnlyHint")

	classification, _ = classifyMCPHints(nil, nil)
	assert.Equal(t, classificationEffect, classification, "fail-safe default when no signal is present")
}

func TestExtractMCPEffectsSkipsUnnamedTools(t *testing.T) {
	raw := []byte(`{"tools":[{"annotations":{"readOnlyHint":true}}]}`)
	boundaries, err := extractMCPEffects("mcp.json", raw)
	require.NoError(t, err)
	assert.Empty(t, boundaries)
}

func TestExtractMCPEffectsRejectsNonManifest(t *testing.T) {
	_, err := extractMCPEffects("plain.json", []byte(`{"receivers": {}}`))
	require.Error(t, err)
}

func TestExtractOpenAPIEffects(t *testing.T) {
	raw := []byte(`{
		"openapi": "3.0.0",
		"paths": {
			"/orders": {"get": {}, "post": {}},
			"/orders/{id}": {"delete": {}}
		}
	}`)
	boundaries, err := extractOpenAPIEffects("openapi.json", raw)
	require.NoError(t, err)
	require.Len(t, boundaries, 3)
	byMethod := map[string]effectBoundary{}
	for _, b := range boundaries {
		byMethod[b.Surface] = b
	}
	assert.Equal(t, classificationObservation, byMethod["GET /orders"].Classification)
	assert.Equal(t, classificationEffect, byMethod["POST /orders"].Classification)
	assert.Equal(t, classificationEffect, byMethod["DELETE /orders/{id}"].Classification)
}

func TestExtractOpenAPIEffectsRejectsNonOpenAPI(t *testing.T) {
	_, err := extractOpenAPIEffects("plain.json", []byte(`{"tools": []}`))
	require.Error(t, err)
}

func TestExtractBusEffects(t *testing.T) {
	raw := []byte(`{"topics":[
		{"name":"orders.created","operation":"publish"},
		{"name":"orders.audit_log","operation":"consume"},
		{"name":"orders.unknown_direction"}
	]}`)
	boundaries, err := extractBusEffects("bus.yaml", raw)
	require.NoError(t, err)
	require.Len(t, boundaries, 3)
	byName := map[string]effectBoundary{}
	for _, b := range boundaries {
		byName[b.Surface] = b
	}
	assert.Equal(t, classificationEffect, byName["orders.created"].Classification)
	assert.Equal(t, classificationObservation, byName["orders.audit_log"].Classification)
	assert.Equal(t, classificationEffect, byName["orders.unknown_direction"].Classification, "fail-safe default")
}

func TestExtractBusEffectsRejectsNonBusConfig(t *testing.T) {
	_, err := extractBusEffects("plain.json", []byte(`{"openapi": "3.0.0"}`))
	require.Error(t, err)
}

func TestExtractEffectsTriesEachShapeInTurn(t *testing.T) {
	mcp := extractEffects("mcp.json", []byte(`{"tools":[{"name":"a","annotations":{"readOnlyHint":true}}]}`))
	require.Len(t, mcp, 1)
	assert.Equal(t, "mcp_tool", mcp[0].SurfaceType)

	openapi := extractEffects("api.json", []byte(`{"openapi":"3.0.0","paths":{"/x":{"get":{}}}}`))
	require.Len(t, openapi, 1)
	assert.Equal(t, "openapi_verb", openapi[0].SurfaceType)

	bus := extractEffects("bus.json", []byte(`{"topics":[{"name":"t","operation":"publish"}]}`))
	require.Len(t, bus, 1)
	assert.Equal(t, "bus_topic", bus[0].SurfaceType)

	assert.Nil(t, extractEffects("unrelated.json", []byte(`{"go_version": "1.27"}`)))
}
