package config

import "testing"

func TestResetCreditsConfigDefaultsAndOptIn(t *testing.T) {
	for _, tt := range []struct {
		body    string
		enabled bool
	}{
		{"config-version: 8\nserver:\n  port: 8317\n", false},
		{"config-version: 8\nreset-credits:\n  auto-apply: false\n", false},
		{"config-version: 8\nreset-credits:\n  auto-apply: true\n", true},
	} {
		cfg, err := ParseConfigBytes([]byte(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ResetCredits.AutoApply != tt.enabled {
			t.Fatalf("auto-apply=%v want=%v", cfg.ResetCredits.AutoApply, tt.enabled)
		}
		if err := ValidateV8Config([]byte(tt.body)); err != nil {
			t.Fatal(err)
		}
	}
}
