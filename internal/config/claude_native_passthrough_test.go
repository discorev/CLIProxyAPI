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
