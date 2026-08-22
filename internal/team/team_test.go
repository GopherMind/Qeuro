package team

import (
	"encoding/json"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	cases := []struct{ in, want string }{
		{`prefix {"a":1} suffix`, `{"a":1}`},
		{"```json\n{\"a\":{\"b\":2}}\n```", `{"a":{"b":2}}`},
		{`no json here`, ``},
		{`{"s":"a } not end","x":1}`, `{"s":"a } not end","x":1}`},
	}
	for _, c := range cases {
		if got := extractJSON(c.in); got != c.want {
			t.Errorf("extractJSON(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParsePlan(t *testing.T) {
	text := `Sure, here is the plan:
{"summary":"build api","test_command":"go test ./...","subtasks":[
  {"role":"backend","skill_query":"go rest api","task":"write handlers"},
  {"role":"database","skill_query":"postgres schema","task":"design tables"}
]}`
	p := parsePlan(text)
	if p.Summary != "build api" || p.TestCommand != "go test ./..." {
		t.Fatalf("plan header parsed wrong: %+v", p)
	}
	if len(p.Subtasks) != 2 || p.Subtasks[0].Role != "backend" || p.Subtasks[1].Role != "database" {
		t.Fatalf("subtasks parsed wrong: %+v", p.Subtasks)
	}
}

func TestProfileForTier(t *testing.T) {
	cases := map[string]string{
		"free":    Econom.Name,
		"":        Econom.Name,
		"pro":     Balanced.Name,
		"mid":     Quality.Name,
		"ultra":   Quality.Name,
		"ULTRA":   Quality.Name,
		"unknown": Econom.Name,
	}
	for tier, want := range cases {
		if got := ProfileForTier(tier).Name; got != want {
			t.Errorf("ProfileForTier(%q).Name = %q, want %q", tier, got, want)
		}
	}
}

func TestIsVisualRole(t *testing.T) {
	visual := []string{"designer", "дизайнер", "frontend", "front-end", "UI", "ux", "web-дизайнер", "landing"}
	for _, r := range visual {
		if !isVisualRole(r) {
			t.Errorf("isVisualRole(%q) = false, want true", r)
		}
	}
	notVisual := []string{"backend", "database", "tester", "devops", "marketing", "architect"}
	for _, r := range notVisual {
		if isVisualRole(r) {
			t.Errorf("isVisualRole(%q) = true, want false", r)
		}
	}
}

func TestToolDefsFilter(t *testing.T) {
	raw := toolDefs(readOnlyTools...)
	var defs []map[string]any
	if err := json.Unmarshal(raw, &defs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(defs) != len(readOnlyTools) {
		t.Fatalf("expected %d read-only tools, got %d", len(readOnlyTools), len(defs))
	}
	// Ensure no mutating tool leaked into the read-only set.
	for _, d := range defs {
		fn, _ := d["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "write_file" || name == "patch_file" || name == "run_command" {
			t.Errorf("mutating tool %q leaked into read-only set", name)
		}
	}
}
