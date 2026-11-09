package helps

import (
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexAccessProgramWarnInterval limits echo mismatch warnings per credential and model.
const codexAccessProgramWarnInterval = 10 * time.Minute

var (
	codexAccessProgramWarned sync.Map // auth ID + model -> time.Time of the last warning
	codexAccessProgramNow    = time.Now
)

// CodexAccessProgramRoute resolves a Daybreak variant the credential registers
// to the upstream model and the access_programs.cyber value it sends. Any other
// model is returned unchanged with no program.
func CodexAccessProgramRoute(auth *cliproxyauth.Auth, model string) (string, string) {
	if auth == nil || auth.ID == "" || model == "" {
		return model, ""
	}
	reg := registry.GetGlobalRegistry()
	info := reg.GetClientModelInfo(auth.ID, model)
	if prefix := strings.TrimSpace(auth.Prefix); info == nil && prefix != "" {
		info = reg.GetClientModelInfo(auth.ID, prefix+"/"+model)
	}
	if info == nil || info.AccessProgram == "" || info.MetadataModelID == "" {
		return model, ""
	}
	return info.MetadataModelID, info.AccessProgram
}

// RequestedCodexAccessProgram returns the non-standard access_programs.cyber a
// client body asks for, lowercased, or "" for a standard or absent program.
func RequestedCodexAccessProgram(body []byte) string {
	program := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "access_programs.cyber").String()))
	if program == "standard" {
		return ""
	}
	return program
}

// ApplyCodexAccessProgram sets access_programs.cyber to a variant's program,
// replacing any client value. It is built-in translation, so callers apply it
// before user payload rules.
func ApplyCodexAccessProgram(body []byte, program string) []byte {
	if program == "" {
		return body
	}
	if current := gjson.GetBytes(body, "access_programs"); current.Exists() && !current.IsObject() {
		body, _ = sjson.DeleteBytes(body, "access_programs")
	}
	return SetStringIfDifferent(body, "access_programs.cyber", program)
}

// CodexAccessProgramEcho compares the access program upstream echoes in
// response.created and response.completed with the one a request sent.
type CodexAccessProgramEcho struct {
	authID string
	model  string
	sent   string
}

// NewCodexAccessProgramEcho records the access program of the final upstream body.
func NewCodexAccessProgramEcho(auth *cliproxyauth.Auth, model string, body []byte) CodexAccessProgramEcho {
	echo := CodexAccessProgramEcho{model: model, sent: gjson.GetBytes(body, "access_programs.cyber").String()}
	if auth != nil {
		echo.authID = auth.ID
	}
	return echo
}

// Observe warns, at most once per interval for the credential and model, when
// upstream reports serving a different program than the one sent.
func (e CodexAccessProgramEcho) Observe(payload []byte) {
	if e.sent == "" {
		return
	}
	switch gjson.GetBytes(payload, "type").String() {
	case "response.created", "response.completed":
	default:
		return
	}
	served := gjson.GetBytes(payload, "response.access_programs.cyber")
	if !served.Exists() || served.String() == e.sent {
		return
	}
	now := codexAccessProgramNow()
	key := e.authID + "\x00" + e.model
	if last, ok := codexAccessProgramWarned.Load(key); ok && now.Sub(last.(time.Time)) < codexAccessProgramWarnInterval {
		return
	}
	codexAccessProgramWarned.Store(key, now)
	log.WithFields(log.Fields{
		"provider":              "codex",
		"model":                 e.model,
		"auth_id":               e.authID,
		"access_program":        e.sent,
		"served_access_program": served.String(),
	}).Warn("codex access program not honoured")
}
