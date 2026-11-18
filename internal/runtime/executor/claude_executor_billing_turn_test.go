package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestClaudeBillingTurnIndexCountsInputTurnsNotToolLoops(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		body string
		want int
	}{
		{name: "no messages", body: `{}`, want: 0},
		{name: "first prompt", body: `{"messages":[{"role":"user","content":"hi"}]}`, want: 1},
		{
			name: "role=system turns are not input",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"system","content":[{"type":"text","text":"x"}]}]}`,
			want: 1,
		},
		{
			name: "consecutive leading user messages are one turn",
			body: `{"messages":[{"role":"user","content":"context"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			want: 1,
		},
		{
			name: "consecutive user messages after a reply are one turn",
			body: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"a"},{"role":"system","content":"x"},{"role":"user","content":"b"}]}`,
			want: 2,
		},
		{
			name: "tool loop keeps the position",
			body: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Bash","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"},{"type":"text","text":"<system-reminder>x</system-reminder>"}]}]}`,
			want: 1,
		},
		{
			name: "second prompt advances",
			body: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Bash","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]},{"role":"assistant","content":"done"},{"role":"user","content":"again"}]}`,
			want: 2,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeBillingTurnIndex([]byte(tt.body)); got != tt.want {
				t.Fatalf("claudeBillingTurnIndex() = %d, want %d", got, tt.want)
			}
		})
	}
}

// 2.1.325 field order: cc_version; cc_entrypoint; cch; cc_workload;
// cc_is_subagent; cc_prev_req; cc_prompt_id; cc_turn_origin; cc_prompt_index;
// cc_turn_index.
func TestGenerateBillingHeaderMatches2_1_325FieldOrder(t *testing.T) {
	t.Parallel()

	const promptID = "00000000-0000-4000-8000-000000000001"
	got := generateBillingHeader(true, "2.1.325", "hello world", "cli", "", false, "req_011CAAAAAAAAAAAAAAAAAAAA", promptID, 2, "human")
	want := "x-anthropic-billing-header: cc_version=2.1.325." + computeFingerprint("hello world", "2.1.325") +
		"; cc_entrypoint=cli; cch=00000; cc_prev_req=req_011CAAAAAAAAAAAAAAAAAAAA; cc_prompt_id=" + promptID +
		"; cc_turn_origin=human; cc_prompt_index=2; cc_turn_index=2;"
	if got != want {
		t.Fatalf("billing header =\n%s\nwant\n%s", got, want)
	}

	for _, tt := range []struct {
		name   string
		header string
	}{
		{name: "subagent has no turn origin", header: generateBillingHeader(true, "2.1.325", "x", "cli", "", true, "", promptID, 1)},
		{name: "unsigned path omits attribution", header: generateBillingHeader(false, "2.1.325", "x", "cli", "", false, "", promptID, 1, "human")},
		{name: "no counted turn", header: generateBillingHeader(true, "2.1.325", "x", "cli", "", false, "", promptID, 0, "human")},
	} {
		if strings.Contains(tt.header, "cc_prompt_index") || strings.Contains(tt.header, "cc_turn_index") {
			t.Errorf("%s: unexpected turn position in %q", tt.name, tt.header)
		}
	}
}

// cc_prompt_index and cc_turn_index share the turn count, so two leading user
// messages put both at 1, not 2.
func TestClaudeBillingHeaderCountsLeadingUserRunOnce(t *testing.T) {
	t.Parallel()

	const promptID = "00000000-0000-4000-8000-000000000001"
	body := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"context"},{"role":"user","content":"hi"}]}`)
	out := checkSystemInstructionsWithSigningModeAt(body, false, true, "2.1.325", "cli", "", "2026-01-01", false, "", promptID, false, "human")
	billing := gjson.GetBytes(out, "system.0.text").String()
	if !strings.HasSuffix(billing, "; cc_turn_origin=human; cc_prompt_index=1; cc_turn_index=1;") {
		t.Fatalf("billing header = %q, want prompt and turn index 1", billing)
	}
}

// On the execution-metadata path the prompt id must rotate only when the
// prompt index advances. A 2.1.325 tool iteration appends a tool_result message
// and a separate user text message; across two such iterations the turn index
// stays put, so the prompt id must too.
func TestClaudeContinuityPromptIDStableAcrossToolIterationsWithTrailingUserText(t *testing.T) {
	helps.ResetClaudeDiagnosticsForTest()
	t.Cleanup(helps.ResetClaudeDiagnosticsForTest)

	auth := &cliproxyauth.Auth{ID: "prompt-id-tool-iterations"}
	ctx := helps.WithClaudeExecutionMetadata(helps.WithClaudeSessionID(context.Background(), "sess-tool-iterations"), true)
	const (
		prompt    = `{"role":"user","content":"list the files"}`
		toolUse1  = `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}`
		result1   = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}`
		reminder  = `{"role":"user","content":[{"type":"text","text":"<system-reminder>placeholder</system-reminder>"}]}`
		toolUse2  = `{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{}}]}`
		result2   = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}`
		nextUse   = `{"role":"assistant","content":"done"}`
		nextInput = `{"role":"user","content":"thanks"}`
	)
	bodies := []string{
		`{"model":"claude-opus-5-5","messages":[` + prompt + `]}`,
		`{"model":"claude-opus-5-5","messages":[` + strings.Join([]string{prompt, toolUse1, result1, reminder}, ",") + `]}`,
		`{"model":"claude-opus-5-5","messages":[` + strings.Join([]string{prompt, toolUse1, result1, reminder, toolUse2, result2, reminder}, ",") + `]}`,
	}
	var firstPromptID string
	for i, body := range bodies {
		if got := claudeBillingTurnIndex([]byte(body)); got != 1 {
			t.Fatalf("request %d turn index = %d, want 1", i, got)
		}
		_, promptID, cCtx, ok := resolveClaudeContinuityTags(ctx, nil, auth, http.Header{}, []byte(body), true, "", "")
		if !ok || promptID == "" {
			t.Fatalf("request %d: continuity not resolved (ok=%t, prompt id %q)", i, ok, promptID)
		}
		// The executor records each completed response before the next request.
		helps.CommitClaudeContinuity(cCtx.Key, cCtx.Sequence, fmt.Sprintf("msg_%d", i), fmt.Sprintf("req_%d", i), promptID)
		if i == 0 {
			firstPromptID = promptID
		} else if promptID != firstPromptID {
			t.Fatalf("request %d prompt id = %q, want %q kept across tool iterations", i, promptID, firstPromptID)
		}
	}

	next := `{"model":"claude-opus-5-5","messages":[` + strings.Join([]string{prompt, toolUse1, result1, reminder, toolUse2, result2, reminder, nextUse, nextInput}, ",") + `]}`
	if got := claudeBillingTurnIndex([]byte(next)); got != 2 {
		t.Fatalf("next prompt turn index = %d, want 2", got)
	}
	if _, promptID, _, _ := resolveClaudeContinuityTags(ctx, nil, auth, http.Header{}, []byte(next), true, "", ""); promptID == firstPromptID {
		t.Fatalf("next prompt kept prompt id %q, want a new one when the prompt index advances", promptID)
	}
}
