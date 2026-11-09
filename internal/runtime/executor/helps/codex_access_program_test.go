package helps

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestCodexAccessProgramRoute(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	auth := &cliproxyauth.Auth{ID: "codex-access-program-route-test", Provider: "codex", Prefix: "team"}
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{
		{ID: "gpt-6-sol"},
		{ID: "team/gpt-6-sol-daybreak-blue", MetadataModelID: "gpt-6-sol", AccessProgram: "daybreak_blue"},
	})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	if model, program := CodexAccessProgramRoute(auth, "gpt-6-sol-daybreak-blue"); model != "gpt-6-sol" || program != "daybreak_blue" {
		t.Fatalf("variant route = %q %q, want gpt-6-sol daybreak_blue", model, program)
	}
	if model, program := CodexAccessProgramRoute(auth, "gpt-6-sol"); model != "gpt-6-sol" || program != "" {
		t.Fatalf("base route = %q %q, want unchanged without a program", model, program)
	}
	other := &cliproxyauth.Auth{ID: "codex-access-program-route-other", Provider: "codex"}
	if model, program := CodexAccessProgramRoute(other, "gpt-6-sol-daybreak-blue"); model != "gpt-6-sol-daybreak-blue" || program != "" {
		t.Fatalf("credential without the variant routed it: %q %q", model, program)
	}
}

func TestApplyCodexAccessProgramOverwritesClientValue(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-6-sol"}`,
		`{"model":"gpt-6-sol","access_programs":{"cyber":"standard"}}`,
		`{"model":"gpt-6-sol","access_programs":"standard"}`,
	} {
		got := ApplyCodexAccessProgram([]byte(body), "daybreak_blue")
		if program := gjson.GetBytes(got, "access_programs.cyber").String(); program != "daybreak_blue" {
			t.Fatalf("ApplyCodexAccessProgram(%s) = %s", body, got)
		}
	}
	if got := ApplyCodexAccessProgram([]byte(`{"access_programs":{"cyber":"standard"}}`), ""); string(got) != `{"access_programs":{"cyber":"standard"}}` {
		t.Fatalf("no program changed the body: %s", got)
	}
}

func TestCodexAccessProgramEchoWarnsOncePerInterval(t *testing.T) {
	hook := test.NewGlobal()
	t.Cleanup(hook.Reset)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	codexAccessProgramNow = func() time.Time { return now }
	t.Cleanup(func() { codexAccessProgramNow = time.Now })

	auth := &cliproxyauth.Auth{ID: "codex-access-program-echo-test"}
	echo := NewCodexAccessProgramEcho(auth, "gpt-6-sol-daybreak-blue", []byte(`{"access_programs":{"cyber":"daybreak_blue"}}`))
	downgraded := []byte(`{"type":"response.created","response":{"access_programs":{"cyber":"standard"}}}`)
	warnings := func() int {
		count := 0
		for _, entry := range hook.AllEntries() {
			if entry.Level == log.WarnLevel && entry.Message == "codex access program not honoured" && entry.Data["auth_id"] == auth.ID {
				count++
			}
		}
		return count
	}

	echo.Observe([]byte(`{"type":"response.created","response":{"access_programs":{"cyber":"daybreak_blue"}}}`))
	echo.Observe([]byte(`{"type":"response.output_text.delta","response":{"access_programs":{"cyber":"standard"}}}`))
	if got := warnings(); got != 0 {
		t.Fatalf("matching echo or non-lifecycle event warned %d times", got)
	}
	echo.Observe(downgraded)
	echo.Observe([]byte(strings.Replace(string(downgraded), "response.created", "response.completed", 1)))
	if got := warnings(); got != 1 {
		t.Fatalf("downgrade warned %d times, want 1 within the interval", got)
	}
	entry := hook.LastEntry()
	if entry.Data["access_program"] != "daybreak_blue" || entry.Data["served_access_program"] != "standard" || entry.Data["model"] != "gpt-6-sol-daybreak-blue" {
		t.Fatalf("warning fields = %v", entry.Data)
	}
	now = now.Add(codexAccessProgramWarnInterval)
	echo.Observe(downgraded)
	if got := warnings(); got != 2 {
		t.Fatalf("downgrade after the interval warned %d times in total, want 2", got)
	}

	NewCodexAccessProgramEcho(auth, "gpt-6-sol", []byte(`{"model":"gpt-6-sol"}`)).Observe(downgraded)
	if got := warnings(); got != 2 {
		t.Fatal("a request that sent no program warned")
	}
}

// Delta turns over the pooled upstream websocket trim only input and add
// previous_response_id, so they keep the access program, and a changed program
// is a property change that forces a full request.
func TestCodexWSDeltaKeepsAccessProgram(t *testing.T) {
	withProgram := func(body string, program string) string {
		return strings.Replace(body, `"store":false`, `"store":false,"access_programs":{"cyber":"`+program+`"}`, 1)
	}
	baseline := newCodexWSBaseline(newCodexWSRequestShape([]byte(withProgram(codexWSTestBaseBody, "daybreak_blue"))), codexWSTestOutput, "resp_1")
	next := codexWSTestNextBody(t, codexWSTestBaseBody, func(body string) string { return withProgram(body, "daybreak_blue") })
	items, ok := codexWSIncrementalItems(newCodexWSRequestShape(next), baseline)
	if !ok {
		t.Fatal("same-program turn was not sent as a delta")
	}
	body, errBuild := buildCodexWSIncrementalBody(next, "resp_1", items)
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	if got := gjson.GetBytes(body, "access_programs.cyber").String(); got != "daybreak_blue" || gjson.GetBytes(body, "previous_response_id").String() != "resp_1" {
		t.Fatalf("delta body lost the access program: %s", body)
	}

	changed := codexWSTestNextBody(t, codexWSTestBaseBody, func(body string) string { return withProgram(body, "standard") })
	if _, ok := codexWSIncrementalItems(newCodexWSRequestShape(changed), baseline); ok {
		t.Fatal("a changed access program must be sent in full")
	}
}
