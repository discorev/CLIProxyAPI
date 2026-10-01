package config

import "testing"

func TestClaudeNativePassthroughDefaultsTrueAndRespectsFalse(t *testing.T) {
	defaultCfg, errDefault := ParseConfigBytes([]byte(`{}`))
	if errDefault != nil {
		t.Fatalf("ParseConfigBytes(default) error = %v", errDefault)
	}
	if !defaultCfg.ClaudeNativePassthrough {
		t.Fatal("ClaudeNativePassthrough = false, want default true")
	}

	disabledCfg, errDisabled := ParseConfigBytes([]byte("claude-native-passthrough: false\n"))
	if errDisabled != nil {
		t.Fatalf("ParseConfigBytes(false) error = %v", errDisabled)
	}
	if disabledCfg.ClaudeNativePassthrough {
		t.Fatal("ClaudeNativePassthrough = true, want explicit false")
	}
}

func TestClaudeNativePassthroughSurvivesV8MigrationAndValidation(t *testing.T) {
	migrated, _, errMigrate := NormalizeConfigLayout([]byte("debug: true\nclaude-native-passthrough: false\n"), true)
	if errMigrate != nil {
		t.Fatalf("NormalizeConfigLayout() error = %v", errMigrate)
	}
	if errValidate := ValidateV8Config(migrated); errValidate != nil {
		t.Fatalf("ValidateV8Config() error = %v\n%s", errValidate, migrated)
	}
	cfg, errParse := ParseConfigBytes(migrated)
	if errParse != nil {
		t.Fatalf("ParseConfigBytes(migrated) error = %v", errParse)
	}
	if cfg.ClaudeNativePassthrough {
		t.Fatalf("ClaudeNativePassthrough = true after v8 migration, want false\n%s", migrated)
	}
}
