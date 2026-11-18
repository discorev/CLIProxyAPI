package config

import "testing"

func TestClaudeCloakRefusalFallbackDefaultsFalseAndParsesBothLayouts(t *testing.T) {
	defaultCfg, errDefault := ParseConfigBytes([]byte(`{}`))
	if errDefault != nil {
		t.Fatalf("ParseConfigBytes(default) error = %v", errDefault)
	}
	if defaultCfg.ClaudeCloakRefusalFallback {
		t.Fatal("ClaudeCloakRefusalFallback = true, want default false")
	}
	for name, raw := range map[string]string{
		"upstream": "upstream:\n  claude:\n    claude-cloak-refusal-fallback: true\n",
		"legacy":   "claude-cloak-refusal-fallback: true\n",
	} {
		cfg, errParse := ParseConfigBytes([]byte(raw))
		if errParse != nil {
			t.Fatalf("%s: ParseConfigBytes() error = %v", name, errParse)
		}
		if !cfg.ClaudeCloakRefusalFallback {
			t.Fatalf("%s: ClaudeCloakRefusalFallback = false, want true", name)
		}
	}
	migrated, _, errMigrate := NormalizeConfigLayout([]byte("claude-cloak-refusal-fallback: true\n"), true)
	if errMigrate != nil {
		t.Fatalf("NormalizeConfigLayout() error = %v", errMigrate)
	}
	if errValidate := ValidateV8Config(migrated); errValidate != nil {
		t.Fatalf("ValidateV8Config() error = %v\n%s", errValidate, migrated)
	}
	cfg, errParse := ParseConfigBytes(migrated)
	if errParse != nil || !cfg.ClaudeCloakRefusalFallback {
		t.Fatalf("migrated ClaudeCloakRefusalFallback lost (err %v):\n%s", errParse, migrated)
	}
}
