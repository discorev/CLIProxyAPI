package config

import "testing"

func TestResetCreditsConfigDefaultsAndOptIn(t *testing.T) {
	for _, tt := range []struct {
		body            string
		enabled, dryRun bool
	}{
		{"config-version: 8\nserver:\n  port: 8317\n", false, false},
		{"config-version: 8\nreset-credits:\n  auto-apply: false\n  dry-run: false\n", false, false},
		{"config-version: 8\nreset-credits:\n  auto-apply: true\n", true, false},
		{"config-version: 8\nreset-credits:\n  dry-run: true\n", false, true},
		{"config-version: 8\nreset-credits:\n  auto-apply: true\n  dry-run: true\n", true, true},
	} {
		cfg, err := ParseConfigBytes([]byte(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ResetCredits.AutoApply != tt.enabled || cfg.ResetCredits.DryRun != tt.dryRun {
			t.Fatalf("reset-credits=%+v want auto-apply=%v dry-run=%v", cfg.ResetCredits, tt.enabled, tt.dryRun)
		}
		if err := ValidateV8Config([]byte(tt.body)); err != nil {
			t.Fatal(err)
		}
	}
}
