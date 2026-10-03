package helps

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestResetParseClaudeFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/claude_usage_cedar_ember.json")
	if err != nil {
		t.Fatal(err)
	}
	parsed := ParseClaudeResetGrants(body)
	if parsed == nil || parsed.ClaudeResetStatus == nil || !parsed.Eligible || len(parsed.Grants) != 1 {
		t.Fatalf("parsed = %+v", parsed)
	}
	grant := parsed.Grants[0]
	wantEnd := time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC)
	if grant.EndsAt == nil || !grant.EndsAt.Equal(wantEnd) || grant.UseRequiresLimit || !grant.UsableNow ||
		grant.ResetsLeft != 1 || len(grant.Clears) != 3 || grant.PercentUsed["seven_day_overage_included"] != 42 {
		t.Fatalf("grant = %+v", grant)
	}
	if parsed.NextGrantID == nil || *parsed.NextGrantID != grant.ID || parsed.AtLimit || len(parsed.Exhausted) != 0 {
		t.Fatalf("block = %+v", parsed.ClaudeResetStatus)
	}
}

func TestResetParseClaudeStrictValidation(t *testing.T) {
	valid := `{"eligible":true,"grants":[{"id":"launch-1","resets_total":1,"resets_left":1}]}`
	for name, block := range map[string]string{
		"missing eligibility": strings.Replace(valid, `"eligible":true,`, "", 1),
		"wrong eligibility":   strings.Replace(valid, `"eligible":true`, `"eligible":"true"`, 1),
		"invalid ID":          strings.Replace(valid, "launch-1", "BAD!", 1),
		"negative count":      strings.Replace(valid, `"resets_left":1`, `"resets_left":-1`, 1),
		"excess count":        strings.Replace(valid, `"resets_left":1`, `"resets_left":2`, 1),
		"fractional count":    strings.Replace(valid, `"resets_left":1`, `"resets_left":0.5`, 1),
		"missing count":       strings.Replace(valid, `,"resets_left":1`, "", 1),
		"string count":        strings.Replace(valid, `"resets_left":1`, `"resets_left":"1"`, 1),
		"bad grant timestamp": strings.Replace(valid, `"id":"launch-1"`, `"id":"launch-1","ends_at":"nonsense"`, 1),
		"bad block timestamp": strings.Replace(valid, `"eligible":true`, `"eligible":true,"cooldown_until":17`, 1),
		"bad boolean":         strings.Replace(valid, `"id":"launch-1"`, `"id":"launch-1","usable_now":1`, 1),
		"bad clears":          strings.Replace(valid, `"id":"launch-1"`, `"id":"launch-1","clears":"five_hour"`, 1),
		"duplicate":           `{"eligible":true,"grants":[{"id":"x","resets_total":1,"resets_left":1},{"id":"x","resets_total":1,"resets_left":1}]}`,
		"null grant":          `{"eligible":true,"grants":[null]}`,
		"malformed grants":    `{"eligible":true,"grants":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := ParseClaudeResetGrants([]byte(`{"cedar_ember":` + block + `}`)); got != nil {
				t.Fatalf("accepted malformed block: %+v", got)
			}
		})
	}
	parsed := ParseClaudeResetGrants([]byte(`{"cedar_ember":` + valid + `}`))
	if parsed == nil || parsed.Grants[0].UsableNow || !parsed.Grants[0].UseRequiresLimit {
		t.Fatalf("unsafe defaults: %+v", parsed)
	}
	for _, body := range []string{`{}`, `{"cedar_ember":null}`, `[]`, `null`, `{`} {
		if ParseClaudeResetGrants([]byte(body)) != nil {
			t.Fatalf("accepted missing/malformed block: %s", body)
		}
	}
}

func TestResetParseClaudeWindowsAndDisplaySanitizing(t *testing.T) {
	body := `{"cedar_ember":{"eligible":true,"ineligible_reason":"new_reason","next_grant_id":"missing","exhausted":["five_hour","future_window"],"grants":[{"id":"x","label":" a\n b\u0080c ","resets_total":1,"resets_left":1,"clears":["five_hour","five_hour",12,"seven_day_overage_included","fable"],"percent_used":{"five_hour":12,"seven_day":101,"seven_day_overage_included":null,"other":20}}]}}`
	parsed := ParseClaudeResetGrants([]byte(body))
	if parsed == nil || *parsed.IneligibleReason != "unknown" || parsed.NextGrantID != nil || len(parsed.Exhausted) != 1 {
		t.Fatalf("block = %+v", parsed)
	}
	grant := parsed.Grants[0]
	if grant.Label != "a b c" || len(grant.Clears) != 2 || len(grant.PercentUsed) != 1 || grant.PercentUsed["five_hour"] != 12 {
		t.Fatalf("grant = %+v", grant)
	}
	encoded, err := json.Marshal(parsed)
	if err != nil || !strings.Contains(string(encoded), `"grants"`) || strings.Contains(string(encoded), `"ClaudeResetStatus"`) {
		t.Fatalf("unflattened API shape: %s, %v", encoded, err)
	}
}

func TestResetParseCodexCredits(t *testing.T) {
	body := `{"available_count":10,"applicable_available_count":10,"credits":[
		{"id":"valid","reset_type":"codex_rate_limits","status":"available","expires_at":"2026-10-22T16:00:00Z"},
		{"id":42,"resetType":"codex_rate_limits","status":"available","expiresAt":"2026-10-23T16:00:00Z"},
		{"id":"spent","reset_type":"codex_rate_limits","status":"consumed","expires_at":"2026-10-22T16:00:00Z"},
		{"reset_type":"other","status":"available","expires_at":"2026-10-22T16:00:00Z"},
		{"reset_type":"codex_rate_limits","status":"available","expires_at":"bad"},null,12]}`
	parsed := ParseCodexResetCredits([]byte(body))
	if parsed == nil || len(parsed.Credits) != 2 || parsed.Credits[0].ID != "valid" || parsed.Credits[1].ID != "42" {
		t.Fatalf("parsed = %+v", parsed)
	}
	for _, body := range []string{`{}`, `{"available_count":100}`, `{"credits":{}}`, `null`, `"bad"`} {
		if got := ParseCodexResetCredits([]byte(body)); got != nil {
			t.Fatalf("accepted missing credit list: %+v", got)
		}
	}
	if got := ParseCodexResetCredits([]byte(`{"credits":[]}`)); got == nil || len(got.Credits) != 0 {
		t.Fatalf("empty inventory = %+v", got)
	}
}
