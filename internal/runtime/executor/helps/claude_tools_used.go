package helps

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// Limits of Claude Code 2.1.325's x-claude-code-sandy-plover (tools used) header.
const (
	claudeToolsUsedMaxEntries   = 64
	claudeToolsUsedMaxBytes     = 7168
	claudeToolsUsedMaxNameBytes = 256
)

// claudeToolsUsedCoreTools are the built-in tools 2.1.325 lists after every
// other tool in the tools-used header.
var claudeToolsUsedCoreTools = map[string]bool{
	"Bash":          true,
	"Read":          true,
	"Edit":          true,
	"Write":         true,
	"Glob":          true,
	"Grep":          true,
	"NotebookEdit":  true,
	"TodoWrite":     true,
	"Agent":         true,
	"Task":          true,
	"ToolSearch":    true,
	"EnterPlanMode": true,
	"ExitPlanMode":  true,
}

// ClaudeToolsUsedHeaderValue rebuilds 2.1.325's x-claude-code-sandy-plover
// value from the transcript a request carries: a JSON array with one record per
// distinct tool the conversation has called (tool_use blocks), in first-use
// order, with the built-in core tools moved after the others. MCP tools are
// marked with "mcp":true, as Claude Code marks them. A conversation without
// tool calls yields "[]".
//
// Claude Code keeps this list per session and can attach connector URLs to MCP
// records; a stateless proxy only sees the transcript, so the records carry the
// name (and MCP mark) alone. The entry and byte caps match 2.1.325; its
// truncation marker for longer lists is not reproduced.
func ClaudeToolsUsedHeaderValue(body []byte) string {
	seen := make(map[string]bool)
	var names []string
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		if message.Get("role").String() != "assistant" {
			return true
		}
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "tool_use" {
				return true
			}
			name := block.Get("name").String()
			if name == "" || len(name) > claudeToolsUsedMaxNameBytes || seen[name] {
				return true
			}
			seen[name] = true
			names = append(names, name)
			return true
		})
		return true
	})

	ordered := make([]string, 0, len(names))
	for _, name := range names {
		if !claudeToolsUsedCoreTools[name] {
			ordered = append(ordered, name)
		}
	}
	for _, name := range names {
		if claudeToolsUsedCoreTools[name] {
			ordered = append(ordered, name)
		}
	}

	var b strings.Builder
	b.WriteByte('[')
	size, count := 2, 0
	for _, name := range ordered {
		if count == claudeToolsUsedMaxEntries {
			break
		}
		record := `{"name":` + claudeToolsUsedJSONString(name)
		if strings.HasPrefix(name, "mcp__") {
			record += `,"mcp":true`
		}
		record += "}"
		added := len(record)
		if count > 0 {
			added++
		}
		if size+added > claudeToolsUsedMaxBytes {
			break
		}
		if count > 0 {
			b.WriteByte(',')
		}
		b.WriteString(record)
		size += added
		count++
	}
	b.WriteByte(']')
	return b.String()
}

// claudeToolsUsedJSONString encodes s like JSON.stringify followed by Claude
// Code's escape of every UTF-16 code unit from U+007F up, which keeps the
// header value ASCII.
func claudeToolsUsedJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	writeUnit := func(unit rune) {
		hex := strconv.FormatInt(int64(unit), 16)
		b.WriteString(`\u`)
		b.WriteString(strings.Repeat("0", 4-len(hex)))
		b.WriteString(hex)
	}
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || (r >= 0x7f && r <= 0xffff):
			writeUnit(r)
		case r > 0xffff:
			r -= 0x10000
			writeUnit(0xd800 + (r >> 10))
			writeUnit(0xdc00 + (r & 0x3ff))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
