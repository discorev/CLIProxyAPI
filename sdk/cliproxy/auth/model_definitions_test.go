package auth

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseModelDefinitions(t *testing.T) {
	raw := json.RawMessage(`[
		{"id":" claude-secret-1 ","display_name":"Secret","context_length":200000,"thinking":{"levels":["low"]}},
		{"id":"claude-opus-5","thinking":null},
		{"id":"mine","upstream_id":" claude-fable-5 "},
		{"id":"same","upstream_id":"SAME"},
		{"display_name":"no id"},
		"not an object"
	]`)
	definitions := ParseModelDefinitions(raw)
	got := make([][2]string, 0, len(definitions))
	for _, definition := range definitions {
		got = append(got, [2]string{definition.ID, definition.UpstreamID})
	}
	want := [][2]string{{"claude-secret-1", ""}, {"claude-opus-5", ""}, {"mine", "claude-fable-5"}, {"same", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("definitions = %v, want %v", got, want)
	}
	// Raw keeps absent and null fields distinguishable.
	var fields map[string]json.RawMessage
	if errDecode := json.Unmarshal(definitions[1].Raw, &fields); errDecode != nil {
		t.Fatal(errDecode)
	}
	if value, ok := fields["thinking"]; !ok || string(value) != "null" {
		t.Fatalf("null thinking lost: %s", definitions[1].Raw)
	}
	if _, ok := fields["display_name"]; ok {
		t.Fatalf("absent field invented: %s", definitions[1].Raw)
	}

	if got := ParseModelDefinitions(map[string]any{"id": "x"}); got != nil {
		t.Fatalf("non-array value parsed: %v", got)
	}

	auth := &Auth{Provider: "claude"}
	SetModelDefinitionsAttribute(auth, definitions)
	if roundTrip := ModelDefinitionsFromAttributes(auth.Attributes); len(roundTrip) != len(definitions) || roundTrip[2].UpstreamID != "claude-fable-5" {
		t.Fatalf("attribute round trip = %+v", roundTrip)
	}
}

func TestModelDefinitionUpstreamIDRoutesThroughAlias(t *testing.T) {
	definitions := ParseModelDefinitions(json.RawMessage(`[{"id":"my-sol","upstream_id":"gpt-6-sol"},{"id":"gpt-secret"}]`))
	auth := &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}
	SetOAuthModelAliasesAttribute(auth, ModelDefinitionAliases(definitions))
	mgr := NewManager(nil, nil, nil)
	if got := mgr.applyOAuthModelAlias(auth, "my-sol"); got != "gpt-6-sol" {
		t.Fatalf("my-sol routes to %q, want gpt-6-sol", got)
	}
	if got := mgr.applyOAuthModelAlias(auth, "gpt-secret"); got != "gpt-secret" {
		t.Fatalf("gpt-secret routes to %q", got)
	}
}
