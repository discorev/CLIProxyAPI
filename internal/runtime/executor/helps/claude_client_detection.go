package helps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

const (
	// claudeAnthropicVersion is the only Anthropic-Version Claude Code sends.
	claudeAnthropicVersion = "2023-06-01"
	// claudeDefaultStainlessTimeout is the X-Stainless-Timeout every measured
	// native helper sends. It is deliberately NOT read from
	// claude-header-defaults.timeout: applyClaudeHeaders routes a confirmed client
	// through misc.EnsureHeader, which prefers the incoming header and only falls
	// back to the configured value when the caller sent none. A confirmed helper
	// therefore always forwards its own 600, so comparing against the operator
	// value would make any non-600 configuration reject every genuine helper.
	claudeDefaultStainlessTimeout = "600"
)

var (
	claudeCodeUserAgentPattern        = regexp.MustCompile(`(?i)^claude-cli/`)
	claudeCodeUserAgentDetailsPattern = regexp.MustCompile(`(?i)^claude-cli/\S+\s+\(external,\s*([^,)]+)(?:,\s*agent-sdk/([^,)]+))?`)
	claudeCodeNativeUserAgentPattern  = regexp.MustCompile(`(?i)^claude-cli/[0-9]+\.[0-9]+\.[0-9]+\s+\(external,\s*[^,)]+(?:,\s*agent-sdk/[0-9]+\.[0-9]+\.[0-9]+)?\)$`)
)

var claudeCodeSubclientByEntrypoint = map[string]string{
	"cli":                       "claude-code-cli",
	"mcp":                       "claude-code-mcp",
	"bench":                     "claude-code-bench",
	"sdk-cli":                   "claude-code-cli-sdk",
	"sdk-ts":                    "claude-code-sdk-ts",
	"sdk-py":                    "claude-code-sdk-py",
	"claude-vscode":             "claude-code-vscode",
	"claude-code-github-action": "claude-code-gh-action",
	"local-agent":               "claude-local-agent",
	"local_agent":               "claude-local-agent",
	"claude-desktop":            "claude-desktop",
	"claude-desktop-3p":         "claude-desktop-3p",
	"remote":                    "claude-remote",
	"remote_baku":               "claude-remote-baku",
	"remote_cowork":             "claude-remote-cowork",
	"remote_trigger":            "claude-remote-trigger",
	"remote_desktop":            "claude-remote-desktop",
	"remote_mobile":             "claude-remote-mobile",
	"claude_in_slack":           "claude-in-slack",
	"claude-in-slack":           "claude-in-slack",
	"claude-in-teams":           "claude-in-teams",
	"claude-security":           "claude-security",
	"ssh-remote":                "claude-ssh-remote",
	"claude-coworker":           "claude-coworker",
	"claude-coworker-terminal":  "claude-coworker-terminal",
}

// Only product surfaces backed by the Claude Code binary are eligible for
// pass-through. The Agent SDK entrypoints (sdk-ts, sdk-py) spawn the same
// binary in stream-json mode, so their wire behavior matches the CLI. Other
// first-party-looking entrypoints are cloaked until their CPA-reachable
// request shape has been captured and reviewed. Hosts that embed the Agent SDK
// may set their own entrypoint (e.g. t3-code); see isNativeClaudeEntrypoint.
var nativeClaudeEntrypoints = map[string]bool{
	"cli":           true,
	"sdk-cli":       true,
	"sdk-ts":        true,
	"sdk-py":        true,
	"claude-vscode": true,
}

type claudeCodeHelperShape uint8

const (
	claudeCodeHelperShapeNone claudeCodeHelperShape = iota
	claudeCodeHelperShapeMinimal
	claudeCodeHelperShapeStructured
	claudeCodeHelperShapeTitle280
	claudeCodeHelperShapeTitle325

	claudeCodeHelperModel = "claude-haiku-4-5-20251001"
	// claudeCodeTitle325HelperModel is the model 2.1.325 moved its
	// session-title helper to; its quota probe stays on claudeCodeHelperModel.
	claudeCodeTitle325HelperModel = "claude-haiku-5-5"
)

// These are the exact beta sequences observed on markerless native Haiku
// helper requests. The first six are Claude Code 2.1.220. The seventh is the
// 2.1.280 title helper captured 2026-09-23: structured output, then
// server-side fallback, fallback credit, and cache diagnosis. The last two are
// the 2.1.325 quota probe and session-title helper captured 2026-11-16; 2.1.325
// assembles them in its main-turn phase order, so thinking-token-count moved.
// Keeping the allowlist exact avoids turning the helper exception into a
// generic no-claude-code-beta bypass.
var measuredClaudeCodeHelperBetaProfiles = map[string]claudeCodeHelperShape{
	claudeCodeHelperBetaProfile(true):  claudeCodeHelperShapeMinimal,
	claudeCodeHelperBetaProfile(false): claudeCodeHelperShapeMinimal,
	claudeCodeHelperBetaProfile(true,
		"advisor-tool-2026-03-01",
		"structured-outputs-2025-12-15",
		"cache-diagnosis-2026-04-07",
	): claudeCodeHelperShapeStructured,
	claudeCodeHelperBetaProfile(true,
		"structured-outputs-2025-12-15",
		"fallback-credit-2026-06-01",
	): claudeCodeHelperShapeStructured,
	claudeCodeHelperBetaProfile(true,
		"structured-outputs-2025-12-15",
	): claudeCodeHelperShapeStructured,
	claudeCodeHelperBetaProfile(false,
		"structured-outputs-2025-12-15",
	): claudeCodeHelperShapeStructured,
	claudeCodeHelperBetaProfile(true,
		"structured-outputs-2025-12-15",
		"server-side-fallback-2026-06-01",
		"fallback-credit-2026-06-01",
		"cache-diagnosis-2026-04-07",
	): claudeCodeHelperShapeTitle280,
	strings.Join([]string{
		"oauth-2025-04-20",
		"interleaved-thinking-2025-05-14",
		"redact-thinking-2026-02-12",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
	}, ","): claudeCodeHelperShapeMinimal,
	strings.Join([]string{
		"oauth-2025-04-20",
		"interleaved-thinking-2025-05-14",
		"redact-thinking-2026-02-12",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"per-turn-control-2026-07-01",
		"mid-conversation-tool-changes-2026-07-01",
		"tool-provenance-2026-10-26",
		"thinking-token-count-2026-05-13",
		"mid-conversation-system-clear-at-2026-08-21",
		"claude-code-side-query-2026-09-29",
		"effort-2025-11-24",
		"structured-outputs-2025-12-15",
		"cache-diagnosis-2026-04-07",
	}, ","): claudeCodeHelperShapeTitle325,
}

// ClaudeCodeRequestDetection records the strong signals and first-party
// subclient identity used to distinguish an official Claude Code request from
// a client that only copied its User-Agent.
type ClaudeCodeRequestDetection struct {
	Confirmed         bool
	NativePassthrough bool
	StrongSignals     bool
	NativeClient      bool
	XAppCLI           bool
	UserAgent         bool
	BetasPresent      bool
	MetadataUserID    bool
	HelperProfile     bool
	Entrypoint        string
	Subclient         string
	AgentSDKVersion   string
}

// DetectClaudeCodeRequest first mirrors CCH's strong-signal contract, then
// applies CPA's native-client policy. Standard Messages requests require all
// four strong signals; count_tokens omits metadata.user_id. A separate narrow
// profile recognizes measured native Haiku helper requests that intentionally
// omit claude-code-20250219. Generic sdk-ts/sdk-py Agent SDK entrypoints remain
// unconfirmed and receive CLI cloaking.
func DetectClaudeCodeRequest(headers http.Header, payload []byte, countTokens bool, configs ...*config.Config) ClaudeCodeRequestDetection {
	var cfg *config.Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	userAgent := headerValue(headers, "User-Agent")
	entrypoint, agentSDKVersion := parseClaudeCodeUserAgentDetails(userAgent)
	detection := ClaudeCodeRequestDetection{
		XAppCLI:         headerValue(headers, "X-App") == "cli",
		UserAgent:       plausibleClaudeCodeUserAgent(userAgent, cfg),
		BetasPresent:    headerContainsClaudeCodeBeta(headers),
		Entrypoint:      entrypoint,
		Subclient:       claudeCodeSubclientByEntrypoint[entrypoint],
		AgentSDKVersion: agentSDKVersion,
	}

	metadataUserID := gjson.GetBytes(payload, "metadata.user_id")
	detection.MetadataUserID = metadataUserID.Exists() && metadataUserID.Type == gjson.String && isValidUserID(metadataUserID.String())
	detection.NativeClient = isNativeClaudeEntrypoint(entrypoint, agentSDKVersion)
	detection.NativePassthrough = nativePassthroughEligible(userAgent, entrypoint, agentSDKVersion, detection.XAppCLI, detection.MetadataUserID, countTokens)
	standardSignals := detection.XAppCLI && detection.UserAgent && detection.BetasPresent && (countTokens || detection.MetadataUserID)
	detection.HelperProfile = detection.NativeClient && matchesMeasuredClaudeCodeHelperProfile(headers, payload, countTokens, detection, cfg)
	detection.StrongSignals = standardSignals || detection.HelperProfile
	detection.Confirmed = detection.StrongSignals && detection.NativeClient
	return detection
}

// nativePassthroughEligible accepts native Claude Code versions at or above
// minimumNativeClaudeCodeVersion. The floor is forward-only: newer patch, minor,
// and major versions remain eligible without a ceiling.
func nativePassthroughEligible(userAgent, entrypoint, agentSDKVersion string, xAppCLI, metadataUserID, countTokens bool) bool {
	if !xAppCLI || (!countTokens && !metadataUserID) || !isNativeClaudeEntrypoint(entrypoint, agentSDKVersion) {
		return false
	}
	candidate, okCandidate := parseClaudeCLIVersion(userAgent)
	return okCandidate && candidate.Compare(minimumNativeClaudeCodeVersion) >= 0
}

func claudeCodeHelperBetaProfile(redactThinking bool, trailing ...string) string {
	betas := []string{"oauth-2025-04-20", "interleaved-thinking-2025-05-14"}
	if redactThinking {
		betas = append(betas, "redact-thinking-2026-02-12")
	}
	betas = append(betas,
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
	)
	betas = append(betas, trailing...)
	return strings.Join(betas, ",")
}

func matchesMeasuredClaudeCodeHelperProfile(
	headers http.Header,
	payload []byte,
	countTokens bool,
	detection ClaudeCodeRequestDetection,
	cfg *config.Config,
) bool {
	if countTokens ||
		detection.Entrypoint != "cli" ||
		detection.BetasPresent ||
		!detection.XAppCLI ||
		!detection.UserAgent ||
		!detection.MetadataUserID {
		return false
	}

	shape := measuredClaudeCodeHelperBetaProfiles[normalizedClaudeBetaHeader(headers)]
	if shape == claudeCodeHelperShapeNone || measuredClaudeCodeHelperBodyShape(payload) != shape {
		return false
	}
	if !measuredClaudeCodeHelperHeadersMatch(headers, cfg, shape) {
		return false
	}
	return measuredClaudeCodeHelperSessionMatches(headers, payload)
}

// normalizedClaudeBetaHeader joins every Anthropic-Beta value in wire order.
// Values() is tried first so canonical headers keep a deterministic order; the
// case-insensitive fallback only exists for hand-built header maps that store a
// non-canonical key, where ranging the map alone would be order-dependent.
func normalizedClaudeBetaHeader(headers http.Header) string {
	if headers == nil {
		return ""
	}
	values := headers.Values("Anthropic-Beta")
	if len(values) == 0 {
		keys := make([]string, 0, 2)
		for key := range headers {
			if strings.EqualFold(key, "Anthropic-Beta") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			values = append(values, headers[key]...)
		}
	}
	betas := make([]string, 0, 12)
	for _, value := range values {
		for _, beta := range strings.Split(value, ",") {
			if beta = strings.TrimSpace(beta); beta != "" {
				betas = append(betas, beta)
			}
		}
	}
	return strings.Join(betas, ",")
}

// measuredClaudeCodeHelperHeadersMatch validates the helper transport envelope.
//
// Platform and software-version headers are deliberately NOT compared for
// equality. The device-profile pipeline this detector feeds already pins OS/Arch
// to the configured baseline and replaces a non-baseline software tuple instead
// of rejecting it, so demanding equality here would classify a genuine Claude
// Code helper from Windows/Linux, or from a different Node or SDK build, as a
// foreign client and cloak it. The Claude Code version follows the same native
// floor as passthrough (a patch release at or above 2.1.280 in its release
// line, or in the configured baseline's line), not the impersonated baseline,
// and the SDK tuple only has to be well formed. Values that carry real
// discriminating power - the exact beta allowlist, the body shape, the billing
// CCH and the session binding - stay strict.
func measuredClaudeCodeHelperHeadersMatch(headers http.Header, cfg *config.Config, shape claudeCodeHelperShape) bool {
	expected := map[string]string{
		"Accept":                  "application/json",
		"Content-Type":            "application/json",
		"X-Stainless-Lang":        "js",
		"X-Stainless-Runtime":     "node",
		"X-Stainless-Retry-Count": "0",
		"X-Stainless-Timeout":     claudeDefaultStainlessTimeout,
		"Anthropic-Version":       claudeAnthropicVersion,
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
	}
	for name, want := range expected {
		if headerValue(headers, name) != want {
			return false
		}
	}
	// Presence is still required: the native SDK always sends these.
	for _, name := range []string{
		"X-Stainless-Package-Version",
		"X-Stainless-Runtime-Version",
		"X-Stainless-OS",
		"X-Stainless-Arch",
	} {
		if headerValue(headers, name) == "" {
			return false
		}
	}
	if !plausibleClaudeCodeUserAgent(headerValue(headers, "User-Agent"), cfg) ||
		!claudePackageVersionPattern.MatchString(headerValue(headers, "X-Stainless-Package-Version")) ||
		!claudeRuntimeVersionPattern.MatchString(headerValue(headers, "X-Stainless-Runtime-Version")) {
		return false
	}
	// Claude Code 2.1.258 (@anthropic-ai/sdk 0.112.1), measured 2026-09-02 on the
	// quota probe and the session-title helper: X-Stainless-Async is never sent
	// and both shapes offer the full compression set. x-client-request-id is
	// attached only when the client's base URL is api.anthropic.com, so a client
	// pointed at CPA through ANTHROPIC_BASE_URL sends none while one that reaches
	// CPA through a transparent proxy still carries the UUID; both are accepted.
	if headerValue(headers, "X-Stainless-Async") != "" {
		return false
	}
	if headerValue(headers, "Accept-Encoding") != "gzip, deflate, br, zstd" {
		return false
	}
	if requestID := headerValue(headers, "X-Client-Request-Id"); requestID != "" {
		if _, errRequestID := uuid.Parse(requestID); errRequestID != nil {
			return false
		}
	}
	return true
}

func measuredClaudeCodeHelperSessionMatches(headers http.Header, payload []byte) bool {
	metadata := gjson.GetBytes(payload, "metadata")
	if !metadata.IsObject() || !claudeJSONObjectHasKeys([]byte(metadata.Raw), []string{"user_id"}) {
		return false
	}
	userID := metadata.Get("user_id")
	if userID.Type != gjson.String || !isValidUserID(userID.String()) {
		return false
	}
	// The native metadata builder is
	//	{...extraMetadata, device_id, account_uuid, session_id, ...parentSessionId && {parent_session_id}}
	// in 2.1.220, 2.1.221 and 2.1.227 alike, so parent_session_id is a legitimate
	// optional trailing key for sub-agent and forked sessions. Rejecting it would
	// cloak the helper requests those sessions issue.
	identityRaw := []byte(userID.String())
	if !claudeJSONObjectHasKeys(identityRaw, []string{"device_id", "account_uuid", "session_id"}) &&
		!claudeJSONObjectHasKeys(identityRaw, []string{"device_id", "account_uuid", "session_id", "parent_session_id"}) {
		return false
	}
	return headerValue(headers, ClaudeCodeSessionHeader) == gjson.GetBytes(identityRaw, "session_id").String()
}

func measuredClaudeCodeHelperBodyShape(payload []byte) claudeCodeHelperShape {
	minimalKeys := []string{"model", "max_tokens", "messages", "metadata"}
	structuredKeys := []string{"model", "messages", "system", "tools", "metadata", "max_tokens", "thinking", "temperature", "output_config", "stream"}
	structuredTitle280Keys := []string{"model", "max_tokens", "messages", "metadata", "output_config"}
	structuredTitle325Keys := []string{"model", "messages", "system", "tools", "metadata", "max_tokens", "output_config", "stream"}
	shape := claudeCodeHelperShapeNone
	isTitle280 := false
	helperModel := claudeCodeHelperModel
	switch {
	case claudeJSONObjectHasKeys(payload, minimalKeys):
		shape = claudeCodeHelperShapeMinimal
	case claudeJSONObjectHasKeys(payload, structuredKeys):
		shape = claudeCodeHelperShapeStructured
	case claudeJSONObjectHasKeys(payload, structuredTitle280Keys):
		shape = claudeCodeHelperShapeTitle280
		isTitle280 = true
	case claudeJSONObjectHasKeys(payload, structuredTitle325Keys):
		shape = claudeCodeHelperShapeTitle325
		helperModel = claudeCodeTitle325HelperModel
	default:
		return claudeCodeHelperShapeNone
	}

	maxTokens := gjson.GetBytes(payload, "max_tokens")
	if gjson.GetBytes(payload, "model").String() != helperModel ||
		maxTokens.Type != gjson.Number {
		return claudeCodeHelperShapeNone
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() || len(messages.Array()) != 1 {
		return claudeCodeHelperShapeNone
	}
	message := messages.Get("0")
	if !claudeJSONObjectHasKeys([]byte(message.Raw), []string{"role", "content"}) ||
		message.Get("role").String() != "user" {
		return claudeCodeHelperShapeNone
	}

	if shape == claudeCodeHelperShapeMinimal {
		if maxTokens.Raw != "1" || message.Get("content").Type != gjson.String {
			return claudeCodeHelperShapeNone
		}
		return shape
	}

	if isTitle280 {
		outputConfig := gjson.GetBytes(payload, "output_config")
		format := outputConfig.Get("format")
		schema := format.Get("schema")
		if maxTokens.Raw != "80" ||
			message.Get("content").Type != gjson.String ||
			!claudeJSONObjectHasKeys([]byte(outputConfig.Raw), []string{"format"}) ||
			!claudeJSONObjectHasKeys([]byte(format.Raw), []string{"type", "schema"}) ||
			format.Get("type").String() != "json_schema" ||
			!claudeJSONObjectHasKeys([]byte(schema.Raw), []string{"type"}) ||
			schema.Get("type").String() != "object" {
			return claudeCodeHelperShapeNone
		}
		return shape
	}

	content := message.Get("content")
	if !content.IsArray() || len(content.Array()) != 1 {
		return claudeCodeHelperShapeNone
	}
	contentBlock := content.Get("0")
	if !claudeJSONObjectHasKeys([]byte(contentBlock.Raw), []string{"type", "text"}) ||
		contentBlock.Get("type").String() != "text" {
		return claudeCodeHelperShapeNone
	}
	if !measuredClaudeCodeHelperSystemMatches(gjson.GetBytes(payload, "system")) {
		return claudeCodeHelperShapeNone
	}
	if tools := gjson.GetBytes(payload, "tools"); !tools.IsArray() || len(tools.Array()) != 0 {
		return claudeCodeHelperShapeNone
	}
	outputConfig := gjson.GetBytes(payload, "output_config")
	outputConfigKeys := []string{"format"}
	if shape == claudeCodeHelperShapeTitle325 {
		// 2.1.325 drops thinking and temperature, keeps the session effort in
		// output_config, and raises max_tokens to the model's 128000.
		outputConfigKeys = []string{"effort", "format"}
		if maxTokens.Raw != "128000" ||
			outputConfig.Get("effort").Type != gjson.String || outputConfig.Get("effort").String() == "" {
			return claudeCodeHelperShapeNone
		}
	} else {
		thinking := gjson.GetBytes(payload, "thinking")
		if !claudeJSONObjectHasKeys([]byte(thinking.Raw), []string{"type"}) ||
			thinking.Get("type").String() != "disabled" {
			return claudeCodeHelperShapeNone
		}
	}
	format := outputConfig.Get("format")
	schema := format.Get("schema")
	properties := schema.Get("properties")
	titleProperty := properties.Get("title")
	required := schema.Get("required")
	additionalProperties := schema.Get("additionalProperties")
	if !claudeJSONObjectHasKeys([]byte(outputConfig.Raw), outputConfigKeys) ||
		!claudeJSONObjectHasKeys([]byte(format.Raw), []string{"type", "schema"}) ||
		format.Get("type").String() != "json_schema" ||
		!claudeJSONObjectHasKeys([]byte(schema.Raw), []string{"type", "properties", "required", "additionalProperties"}) ||
		schema.Get("type").String() != "object" ||
		!claudeJSONObjectHasKeys([]byte(properties.Raw), []string{"title"}) ||
		!claudeJSONObjectHasKeys([]byte(titleProperty.Raw), []string{"type"}) ||
		titleProperty.Get("type").String() != "string" ||
		!required.IsArray() || len(required.Array()) != 1 || required.Get("0").String() != "title" ||
		additionalProperties.Type != gjson.False {
		return claudeCodeHelperShapeNone
	}
	if gjson.GetBytes(payload, "stream").Type != gjson.True {
		return claudeCodeHelperShapeNone
	}
	if shape == claudeCodeHelperShapeTitle325 {
		return shape
	}
	temperature := gjson.GetBytes(payload, "temperature")
	if maxTokens.Raw != "32000" || temperature.Raw != "1" {
		return claudeCodeHelperShapeNone
	}
	return shape
}

func measuredClaudeCodeHelperSystemMatches(system gjson.Result) bool {
	if !system.IsArray() || len(system.Array()) != 3 {
		return false
	}
	for _, block := range system.Array() {
		if !claudeJSONObjectHasKeys([]byte(block.Raw), []string{"type", "text"}) || block.Get("type").String() != "text" {
			return false
		}
	}
	billing := system.Get("0.text").String()
	identity := system.Get("1.text").String()
	return strings.HasPrefix(billing, "x-anthropic-billing-header:") && measuredClaudeBillingCCH(billing) && strings.HasPrefix(identity, "You are Claude Code")
}

// measuredClaudeBillingCCH validates the five lowercase hexadecimal characters the
// native billing header carries. It duplicates isLowerHex in
// internal/runtime/executor/claude_signing.go because the signing side lives in the
// package that imports this one; keep the two definitions in step.
func measuredClaudeBillingCCH(billing string) bool {
	marker := strings.Index(billing, " cch=")
	if marker < 0 {
		return false
	}
	valueStart := marker + len(" cch=")
	valueEnd := valueStart + 5
	if valueEnd >= len(billing) || billing[valueEnd] != ';' {
		return false
	}
	for _, character := range billing[valueStart:valueEnd] {
		decimal := character >= '0' && character <= '9'
		lowerHex := character >= 'a' && character <= 'f'
		if !decimal && !lowerHex {
			return false
		}
	}
	return true
}

func claudeJSONObjectHasKeys(raw []byte, want []string) bool {
	if !json.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, errOpening := decoder.Token()
	if errOpening != nil || opening != json.Delim('{') {
		return false
	}
	keyIndex := 0
	for decoder.More() {
		token, errToken := decoder.Token()
		if errToken != nil {
			return false
		}
		key, okKey := token.(string)
		if !okKey || keyIndex >= len(want) || key != want[keyIndex] {
			return false
		}
		keyIndex++
		var value json.RawMessage
		if errValue := decoder.Decode(&value); errValue != nil {
			return false
		}
	}
	closing, errClosing := decoder.Token()
	return errClosing == nil && closing == json.Delim('}') && keyIndex == len(want)
}

// isNativeClaudeEntrypoint reports whether a Claude Code User-Agent comes from
// the Claude Code binary. A versioned agent-sdk suffix is only added when the
// Agent SDK spawns the binary, so any host-chosen entrypoint qualifies with it.
// Without the suffix (interactive CLI, VS Code) the entrypoint must be listed.
func isNativeClaudeEntrypoint(entrypoint, agentSDKVersion string) bool {
	if nativeClaudeEntrypoints[entrypoint] {
		return true
	}
	return entrypoint != "" && claudePackageVersionPattern.MatchString(agentSDKVersion)
}

func plausibleClaudeCodeUserAgent(userAgent string, cfg *config.Config) bool {
	userAgent = strings.TrimSpace(userAgent)
	if !claudeCodeUserAgentPattern.MatchString(userAgent) || !claudeCodeNativeUserAgentPattern.MatchString(userAgent) {
		return false
	}
	candidate, okCandidate := parseClaudeCLIVersion(userAgent)
	if !okCandidate {
		return false
	}
	// Patch releases at or above the native floor within its release line stay
	// plausible, independent of the newer version the cloak impersonates. An
	// operator-configured baseline in a later release line is accepted as well.
	if plausibleClaudeCLIVersion(candidate, minimumNativeClaudeCodeVersion) {
		return true
	}
	baseline, okBaseline := parseClaudeCLIVersion(defaultClaudeDeviceProfile(cfg).UserAgent)
	return okBaseline && plausibleClaudeCLIVersion(candidate, baseline)
}

func parseClaudeCodeUserAgentDetails(userAgent string) (entrypoint, agentSDKVersion string) {
	matches := claudeCodeUserAgentDetailsPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(matches) < 2 {
		return "", ""
	}
	entrypoint = strings.ToLower(strings.TrimSpace(matches[1]))
	if len(matches) >= 3 {
		agentSDKVersion = strings.TrimSpace(matches[2])
	}
	return entrypoint, agentSDKVersion
}

func headerValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) || len(values) == 0 {
			continue
		}
		return values[0]
	}
	return ""
}

func headerContainsClaudeCodeBeta(headers http.Header) bool {
	if headers == nil {
		return false
	}
	for key, values := range headers {
		if !strings.EqualFold(key, "Anthropic-Beta") {
			continue
		}
		for _, value := range values {
			for _, beta := range strings.Split(value, ",") {
				if strings.TrimSpace(beta) == "claude-code-20250219" {
					return true
				}
			}
		}
	}
	return false
}
