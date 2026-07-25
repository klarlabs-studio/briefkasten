package mcpserver

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestToolInputSchemasMarkRequiredFields locks in the schema contract
// agents rely on: required fields are declared, and parameters carry
// descriptions.
func TestToolInputSchemasMarkRequiredFields(t *testing.T) {
	client, _ := newClient(t)
	tools, err := client.ListTools()
	if err != nil {
		t.Fatal(err)
	}

	wantRequired := map[string][]string{
		"email.fetch":  {"id"},
		"email.search": {"query"},
	}
	// The batch-taking tools cannot mark either field required: exactly
	// one of id and ids must be given, which the schema dialect the
	// builder emits cannot express. Both must still be declared and
	// described, and the handler enforces the exclusivity.
	wantOptional := map[string][]string{
		"email.mark_seen": {"id", "ids"},
		"email.archive":   {"id", "ids"},
		"email.delete":    {"id", "ids"},
	}

	seen := map[string]bool{}
	for _, tool := range tools {
		name := tool["name"].(string)
		want, ok := wantRequired[name]
		optional, alsoOK := wantOptional[name]
		if !ok && !alsoOK {
			continue
		}
		seen[name] = true
		// The test client hands the schema through unserialized; the wire
		// shape is what matters, so round-trip through JSON.
		rawSchema, err := json.Marshal(tool["inputSchema"])
		if err != nil {
			t.Fatalf("%s: marshal schema: %v", name, err)
		}
		var schema map[string]any
		if err := json.Unmarshal(rawSchema, &schema); err != nil {
			t.Fatalf("%s: unmarshal schema: %v", name, err)
		}
		var got []string
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				got = append(got, r.(string))
			}
		}
		for _, field := range want {
			if !slices.Contains(got, field) {
				t.Errorf("%s: field %q not marked required (got %v)", name, field, got)
			}
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: no properties", name)
			continue
		}
		for _, field := range optional {
			if _, declared := props[field]; !declared {
				t.Errorf("%s: field %q not declared (got %v)", name, field, props)
			}
		}
		for fname, p := range props {
			prop, ok := p.(map[string]any)
			if !ok || prop["description"] == "" || prop["description"] == nil {
				t.Errorf("%s: parameter %q has no description", name, fname)
			}
		}
	}
	for _, want := range []map[string][]string{wantRequired, wantOptional} {
		for name := range want {
			if !seen[name] {
				t.Errorf("tool %s not listed", name)
			}
		}
	}
}
