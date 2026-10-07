package helps

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sort"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexWSDigest is the hash of one canonicalized JSON value.
type codexWSDigest [sha256.Size]byte

// codexWSRequestShape is the comparable form of one full upstream Responses
// request: a digest over every property except the conversation input, and a
// digest per input item. It is computed from the final upstream body, after
// every translation and proxy-side rewrite, so two requests compare equal only
// when the upstream would see the same thing.
type codexWSRequestShape struct {
	props    codexWSDigest
	items    []codexWSDigest
	rawItems []string
	valid    bool
}

// codexWSBaseline is what one pooled socket remembers about its last
// successfully completed response.
type codexWSBaseline struct {
	props      codexWSDigest
	items      []codexWSDigest // previous input followed by the response's output items
	responseID string
}

// codexWSRequestPropertyIgnored lists top-level fields that do not take part in
// the "same request properties" check. They either describe the transport
// (type, stream_options), carry per-turn client telemetry (client_metadata), or
// are the fields the delta rewrites (input, previous_response_id). This mirrors
// codex-rs responses_request_properties_match; every other field, including
// instructions, must be identical.
var codexWSRequestPropertyIgnored = map[string]struct{}{
	"input":                {},
	"previous_response_id": {},
	"type":                 {},
	"stream_options":       {},
	"client_metadata":      {},
	"generate":             {},
}

// newCodexWSRequestShape computes the comparable shape of a full upstream body.
// It returns an invalid shape when the body is not an object with an input array;
// such requests are always sent in full.
func newCodexWSRequestShape(body []byte) codexWSRequestShape {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return codexWSRequestShape{}
	}
	input := root.Get("input")
	if !input.IsArray() {
		return codexWSRequestShape{}
	}

	type property struct {
		key   string
		value []byte
	}
	var props []property
	ok := true
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if _, ignored := codexWSRequestPropertyIgnored[name]; ignored {
			return true
		}
		canonical, errCanon := canonicalCodexWSJSON([]byte(value.Raw), false)
		if errCanon != nil {
			ok = false
			return false
		}
		props = append(props, property{key: name, value: canonical})
		return true
	})
	if !ok {
		return codexWSRequestShape{}
	}
	sort.Slice(props, func(i, j int) bool { return props[i].key < props[j].key })
	hasher := sha256.New()
	for _, prop := range props {
		hasher.Write([]byte(prop.key))
		hasher.Write([]byte{0})
		hasher.Write(prop.value)
		hasher.Write([]byte{0})
	}
	shape := codexWSRequestShape{valid: true}
	copy(shape.props[:], hasher.Sum(nil))

	for _, item := range input.Array() {
		digest, errDigest := codexWSItemDigest([]byte(item.Raw))
		if errDigest != nil {
			return codexWSRequestShape{}
		}
		shape.items = append(shape.items, digest)
		shape.rawItems = append(shape.rawItems, item.Raw)
	}
	return shape
}

// codexWSItemDigest hashes one conversation item after removing the fields a
// client legitimately drops or rewrites when it echoes a server output item back
// as input: the item's own id and status, null values and empty arrays (for
// example annotations: [] and logprobs: [] on output_text parts). Anything that
// carries content - text, arguments, call ids, encrypted reasoning - must match
// exactly, so a client that alters history gets a full request.
func codexWSItemDigest(raw []byte) (codexWSDigest, error) {
	canonical, err := canonicalCodexWSJSON(raw, true)
	if err != nil {
		return codexWSDigest{}, err
	}
	return sha256.Sum256(canonical), nil
}

// canonicalCodexWSJSON re-encodes raw JSON with sorted object keys and literal
// numbers. When item is true the top-level id/status fields are removed and
// null values and empty arrays are pruned recursively.
func canonicalCodexWSJSON(raw []byte, item bool) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if item {
		if object, ok := value.(map[string]any); ok {
			delete(object, "id")
			delete(object, "status")
		}
		value = pruneCodexWSJSON(value)
	}
	return json.Marshal(value)
}

func pruneCodexWSJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if child == nil {
				delete(typed, key)
				continue
			}
			if array, ok := child.([]any); ok && len(array) == 0 {
				delete(typed, key)
				continue
			}
			typed[key] = pruneCodexWSJSON(child)
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = pruneCodexWSJSON(typed[i])
		}
		return typed
	default:
		return value
	}
}

// codexWSIncrementalItems returns the raw input items to send with
// previous_response_id when shape strictly extends the baseline: identical
// request properties, and an input that starts with the previous input plus the
// previous response's output items and adds at least one new item. Any doubt
// returns ok=false, and the caller sends the full request.
func codexWSIncrementalItems(shape codexWSRequestShape, baseline *codexWSBaseline) ([]string, bool) {
	if !shape.valid || baseline == nil || baseline.responseID == "" {
		return nil, false
	}
	if shape.props != baseline.props {
		return nil, false
	}
	if len(shape.items) <= len(baseline.items) {
		return nil, false
	}
	for i := range baseline.items {
		if shape.items[i] != baseline.items[i] {
			return nil, false
		}
	}
	return shape.rawItems[len(baseline.items):], true
}

// codexWSMatchedPrefix reports how many baseline items shape extends, or -1
// when shape is not a strict extension of baseline.
func codexWSMatchedPrefix(shape codexWSRequestShape, baseline *codexWSBaseline) int {
	if _, ok := codexWSIncrementalItems(shape, baseline); !ok {
		return -1
	}
	return len(baseline.items)
}

// codexWSSharedPrefix returns how many leading input items shape shares with
// baseline when the request properties match, and 0 otherwise. A positive
// value means the request continues the same conversation chain even though it
// is not a strict extension (for example a client that drops reasoning items
// from history), so the socket is the natural one to reuse for a full request.
func codexWSSharedPrefix(shape codexWSRequestShape, baseline *codexWSBaseline) int {
	if !shape.valid || baseline == nil || shape.props != baseline.props {
		return 0
	}
	n := 0
	for n < len(shape.items) && n < len(baseline.items) && shape.items[n] == baseline.items[n] {
		n++
	}
	return n
}

// newCodexWSBaseline records the state a socket holds after a completed
// response: the full request input followed by the response's output items.
func newCodexWSBaseline(shape codexWSRequestShape, outputItems [][]byte, responseID string) *codexWSBaseline {
	if !shape.valid || responseID == "" {
		return nil
	}
	items := make([]codexWSDigest, 0, len(shape.items)+len(outputItems))
	items = append(items, shape.items...)
	for _, raw := range outputItems {
		digest, err := codexWSItemDigest(raw)
		if err != nil {
			return nil
		}
		items = append(items, digest)
	}
	return &codexWSBaseline{props: shape.props, items: items, responseID: responseID}
}

// buildCodexWSIncrementalBody rewrites a full upstream body into a
// previous_response_id continuation carrying only the new items.
func buildCodexWSIncrementalBody(body []byte, responseID string, items []string) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(2 + len(items)*64)
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(item)
	}
	buf.WriteByte(']')
	out, err := sjson.SetRawBytes(body, "input", buf.Bytes())
	if err != nil {
		return nil, err
	}
	return sjson.SetBytes(out, "previous_response_id", responseID)
}
