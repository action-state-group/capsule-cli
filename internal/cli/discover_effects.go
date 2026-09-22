package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// effectBoundary is one row of the `discover --effects` map: one named
// surface (an MCP tool, an OpenAPI verb, a bus topic) and its Signal-1
// classification per docs/whats-consequential.md in capsule-emit (the
// two-signal rule) -- see capsule_emit.connector.classify_signal_1 for the
// Python implementation this mirrors. Built ONLY from config files that
// already passed classifyFile's allow/deny gate (discover.go) -- this file
// never opens anything itself.
type effectBoundary struct {
	Source         string `json:"source"`         // file path the surface was declared in
	Surface        string `json:"surface"`        // tool name / "METHOD path" / topic name
	SurfaceType    string `json:"surface_type"`   // "mcp_tool" | "openapi_verb" | "bus_topic"
	Classification string `json:"classification"` // "effect" | "observation"
	Signal         string `json:"signal"`         // which signal decided it, for transparency
}

const (
	classificationEffect      = "effect"
	classificationObservation = "observation"
)

var safeHTTPMethods = map[string]bool{"GET": true, "HEAD": true, "OPTIONS": true}

// mcpToolManifest is the minimal shape discover reads from an MCP tool
// manifest / server config -- name plus the two annotations
// docs/whats-consequential.md's Signal 1 table already names. Any other
// field in the file is ignored, never retained.
type mcpToolManifest struct {
	Tools []struct {
		Name        string `json:"name" yaml:"name"`
		Annotations *struct {
			ReadOnlyHint    *bool `json:"readOnlyHint" yaml:"readOnlyHint"`
			DestructiveHint *bool `json:"destructiveHint" yaml:"destructiveHint"`
		} `json:"annotations" yaml:"annotations"`
	} `json:"tools" yaml:"tools"`
}

// classifyMCPHints applies the same priority order as
// capsule_emit.connector.classify_signal_1: destructiveHint=true always
// wins; otherwise readOnlyHint decides when present; absent both, the
// fail-safe default is EFFECT.
func classifyMCPHints(readOnly, destructive *bool) (classification, signal string) {
	if destructive != nil && *destructive {
		return classificationEffect, "mcp_destructive_hint=true"
	}
	if readOnly != nil {
		if *readOnly {
			return classificationObservation, "mcp_read_only_hint=true"
		}
		return classificationEffect, "mcp_read_only_hint=false"
	}
	return classificationEffect, "no signal present (fail-safe default)"
}

func extractMCPEffects(source string, raw []byte) ([]effectBoundary, error) {
	var manifest mcpToolManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		if err := yaml.Unmarshal(raw, &manifest); err != nil {
			return nil, fmt.Errorf("not a recognized MCP tool manifest: %w", err)
		}
	}
	if len(manifest.Tools) == 0 {
		return nil, fmt.Errorf("no tools[] array found")
	}
	boundaries := make([]effectBoundary, 0, len(manifest.Tools))
	for _, tool := range manifest.Tools {
		if tool.Name == "" {
			continue
		}
		var readOnly, destructive *bool
		if tool.Annotations != nil {
			readOnly, destructive = tool.Annotations.ReadOnlyHint, tool.Annotations.DestructiveHint
		}
		classification, signal := classifyMCPHints(readOnly, destructive)
		boundaries = append(boundaries, effectBoundary{
			Source: source, Surface: tool.Name, SurfaceType: "mcp_tool",
			Classification: classification, Signal: signal,
		})
	}
	return boundaries, nil
}

// openAPIDocument is the minimal shape discover reads from an OpenAPI
// document -- paths and their declared verbs only. Request/response bodies,
// examples, and every other field are never even unmarshaled into this
// struct, let alone retained.
type openAPIDocument struct {
	OpenAPI string                    `json:"openapi" yaml:"openapi"`
	Paths   map[string]map[string]any `json:"paths" yaml:"paths"`
}

var httpVerbs = []string{"get", "head", "options", "post", "put", "patch", "delete", "trace"}

func extractOpenAPIEffects(source string, raw []byte) ([]effectBoundary, error) {
	var doc openAPIDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("not a recognized OpenAPI document: %w", err)
		}
	}
	if doc.OpenAPI == "" || len(doc.Paths) == 0 {
		return nil, fmt.Errorf("no openapi version or paths found")
	}
	var boundaries []effectBoundary
	for path, methods := range doc.Paths {
		for _, verb := range httpVerbs {
			if _, ok := methods[verb]; !ok {
				continue
			}
			method := strings.ToUpper(verb)
			classification, signal := classificationEffect, "http_method="+method
			if safeHTTPMethods[method] {
				classification, signal = classificationObservation, "http_method="+method
			}
			boundaries = append(boundaries, effectBoundary{
				Source: source, Surface: method + " " + path, SurfaceType: "openapi_verb",
				Classification: classification, Signal: signal,
			})
		}
	}
	return boundaries, nil
}

// busTopicConfig is the minimal shape discover reads for a message-bus
// topic declaration: a name and, when the config states it, a direction
// this node takes. "operation" absent -- the direction is unknown to a
// static config read -- is the same fail-safe EFFECT default as an MCP
// tool with no annotations: a topic this scan cannot prove is read-only is
// never assumed to be one.
type busTopicConfig struct {
	Topics []struct {
		Name      string `json:"name" yaml:"name"`
		Operation string `json:"operation" yaml:"operation"` // "publish" | "consume"
	} `json:"topics" yaml:"topics"`
}

func extractBusEffects(source string, raw []byte) ([]effectBoundary, error) {
	var config busTopicConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		if err := yaml.Unmarshal(raw, &config); err != nil {
			return nil, fmt.Errorf("not a recognized bus topic config: %w", err)
		}
	}
	if len(config.Topics) == 0 {
		return nil, fmt.Errorf("no topics[] array found")
	}
	boundaries := make([]effectBoundary, 0, len(config.Topics))
	for _, topic := range config.Topics {
		if topic.Name == "" {
			continue
		}
		classification, signal := classificationEffect, "no operation declared (fail-safe default)"
		switch strings.ToLower(topic.Operation) {
		case "publish":
			classification, signal = classificationEffect, "operation=publish"
		case "consume":
			classification, signal = classificationObservation, "operation=consume"
		}
		boundaries = append(boundaries, effectBoundary{
			Source: source, Surface: topic.Name, SurfaceType: "bus_topic",
			Classification: classification, Signal: signal,
		})
	}
	return boundaries, nil
}

// extractEffects tries each recognized config shape in turn against
// already-read, already-allow-listed bytes (raw never came from a
// deny-listed or unknown file -- see scanRoot). The first shape that parses
// wins; a file matching none is simply not a source of effect-boundary
// rows -- not an error, since most allow-listed config is not one of these
// three shapes.
func extractEffects(source string, raw []byte) []effectBoundary {
	if boundaries, err := extractMCPEffects(source, raw); err == nil {
		return boundaries
	}
	if boundaries, err := extractOpenAPIEffects(source, raw); err == nil {
		return boundaries
	}
	if boundaries, err := extractBusEffects(source, raw); err == nil {
		return boundaries
	}
	return nil
}
