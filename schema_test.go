package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every tool schema must be valid JSON Schema as the API judges it: "parameters"
// an object, "properties" an object, and "required" an array - never null.
//
// This caught a real 400 in production shape: the screenshot tool has no
// mandatory argument, and passing a nil slice through the schema helper emitted
// `"required": null`, which the API rejects with "Invalid schema for function
// 'screenshot': null is not of type \"array\"" before it even looks at the
// messages.
func TestToolSchemasAreValidJSONSchema(t *testing.T) {
	for _, mode := range []string{ModeAgent, ModePlan, ModeAsk} {
		tools := toolSchemas(mode)
		if len(tools) == 0 {
			t.Fatalf("%s: no tools", mode)
		}
		raw, err := json.Marshal(tools)
		if err != nil {
			t.Fatalf("%s: marshal: %v", mode, err)
		}
		if strings.Contains(string(raw), `"required":null`) {
			t.Fatalf("%s: a schema sends required as null: %s", mode, raw)
		}

		var decoded []struct {
			Function struct {
				Name       string `json:"name"`
				Parameters struct {
					Type       string          `json:"type"`
					Properties json.RawMessage `json:"properties"`
					Required   json.RawMessage `json:"required"`
				} `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s: unmarshal: %v", mode, err)
		}
		for _, tl := range decoded {
			name := tl.Function.Name
			if tl.Function.Parameters.Type != "object" {
				t.Errorf("%s/%s: parameters.type = %q, want object", mode, name, tl.Function.Parameters.Type)
			}
			var required []string
			if err := json.Unmarshal(tl.Function.Parameters.Required, &required); err != nil {
				t.Errorf("%s/%s: required is not a string array: %v (%s)", mode, name, err, tl.Function.Parameters.Required)
			}
			var props map[string]any
			if err := json.Unmarshal(tl.Function.Parameters.Properties, &props); err != nil {
				t.Errorf("%s/%s: properties is not an object: %v", mode, name, err)
			}
			// Everything listed as required must actually be described.
			for _, r := range required {
				if _, ok := props[r]; !ok {
					t.Errorf("%s/%s: required %q is not in properties", mode, name, r)
				}
			}
		}
	}
}

// The screenshot tool is offered in every mode - looking at the screen is how a
// read-only session answers "what does this error say?" - and it is never
// silently auto-approved outside agent mode.
func TestScreenshotToolAvailabilityAndApproval(t *testing.T) {
	for _, mode := range []string{ModeAgent, ModePlan, ModeAsk} {
		found := false
		for _, tl := range toolSchemas(mode) {
			if tl["function"].(map[string]any)["name"] == "screenshot" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the screenshot tool is missing", mode)
		}
	}
	if !needsApproval("screenshot") {
		t.Fatal("a screen capture must always be shown to the user first: it uploads their desktop")
	}
	risk := classifyTool("screenshot", `{"width":640}`)
	if risk.Level != riskElevated {
		t.Fatalf("risk level %q, want elevated", risk.Level)
	}
	if len(risk.Reasons) == 0 || !strings.Contains(strings.Join(risk.Reasons, " "), "screen") {
		t.Fatalf("the approval card does not say what it does: %v", risk.Reasons)
	}
}

func TestParseShotArgs(t *testing.T) {
	a, err := parseShotArgs(`{"x":10,"y":20,"width":200,"height":100,"scale":0.5}`)
	if err != nil {
		t.Fatalf("valid args rejected: %v", err)
	}
	if a.X != 10 || a.Y != 20 || a.W != 200 || a.H != 100 || a.Scale != 0.5 {
		t.Fatalf("got %+v", a)
	}

	// Absent region means the whole desktop, at native resolution.
	a, err = parseShotArgs(`{}`)
	if err != nil || a.Scale != 1 || a.W != 0 || a.H != 0 {
		t.Fatalf("defaults: %+v, %v", a, err)
	}

	// One side of the rectangle is a typo, not a request.
	if _, err := parseShotArgs(`{"width":100}`); err == nil {
		t.Fatal("width without height accepted")
	}
	if _, err := parseShotArgs(`{"width":-5,"height":-5}`); err == nil {
		t.Fatal("negative rectangle accepted")
	}
	// Out-of-range zoom is clamped, not refused: a slightly wrong number should
	// still produce a picture.
	a, _ = parseShotArgs(`{"scale":50}`)
	if a.Scale != shotMaxScale {
		t.Fatalf("scale not clamped to %v: %v", shotMaxScale, a.Scale)
	}
	a, _ = parseShotArgs(`{"scale":0.0001}`)
	if a.Scale != shotMinScale {
		t.Fatalf("scale not clamped to %v: %v", shotMinScale, a.Scale)
	}
}
