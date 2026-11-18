package helps

import (
	"strings"
	"testing"
)

func TestClaudeToolsUsedHeaderValue(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{name: "no tool calls", body: `{"messages":[{"role":"user","content":"hi"}]}`, want: `[]`},
		{
			// Captured 2.1.325 tool loop: the turn after a Bash call.
			name: "first tool call",
			body: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`,
			want: `[{"name":"Bash"}]`,
		},
		{
			name: "first-use order, duplicates once, core tools last, mcp marked",
			body: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"Read"},{"type":"tool_use","name":"Skill"}]},` +
				`{"role":"assistant","content":[{"type":"tool_use","name":"mcp__docs__search"},{"type":"tool_use","name":"Read"},{"type":"server_tool_use","name":"web_search"}]},` +
				`{"role":"user","content":[{"type":"tool_use","name":"NotAssistant"}]}]}`,
			want: `[{"name":"Skill"},{"name":"mcp__docs__search","mcp":true},{"name":"Read"}]`,
		},
		{
			name: "non-ASCII names are escaped like JSON.stringify plus the u007f filter",
			body: `{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"té\"😀"}]}]}`,
			want: `[{"name":"t\u00e9\"\ud83d\ude00"}]`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClaudeToolsUsedHeaderValue([]byte(tt.body)); got != tt.want {
				t.Fatalf("ClaudeToolsUsedHeaderValue() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestClaudeToolsUsedHeaderValueCapsEntries(t *testing.T) {
	t.Parallel()

	var blocks []string
	for i := 0; i < claudeToolsUsedMaxEntries+5; i++ {
		blocks = append(blocks, `{"type":"tool_use","name":"tool_`+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26))+`"}`)
	}
	body := `{"messages":[{"role":"assistant","content":[` + strings.Join(blocks, ",") + `]}]}`
	got := ClaudeToolsUsedHeaderValue([]byte(body))
	if count := strings.Count(got, `{"name":`); count != claudeToolsUsedMaxEntries {
		t.Fatalf("records = %d, want %d: %s", count, claudeToolsUsedMaxEntries, got)
	}
}
