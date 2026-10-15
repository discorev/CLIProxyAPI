package cliproxy

import (
	"testing"

	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Usage eligibility alone must not enroll xAI in restricted-model fetching.
// Adding a lister requires choosing an identity stable across OAuth refreshes.
func TestXAIExecutorsDoNotListRestrictedModels(t *testing.T) {
	for _, executor := range []coreauth.ProviderExecutor{
		&runtimeexecutor.XAIExecutor{},
		&runtimeexecutor.XAIAutoExecutor{},
	} {
		if _, ok := executor.(coreauth.UpstreamModelLister); ok {
			t.Fatalf("%T implements UpstreamModelLister; define a stable xAI restricted-model identity before enabling listing", executor)
		}
	}
}
