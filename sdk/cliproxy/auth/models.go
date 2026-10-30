package auth

import (
	"context"
	"encoding/json"
)

// UpstreamModel is one entry of an upstream model list: its ID and the raw
// upstream object it was read from.
type UpstreamModel struct {
	ID  string          `json:"id"`
	Raw json.RawMessage `json:"raw,omitempty"`
}

// UpstreamModelLister is optional; only OAuth executors with a model-list API implement it.
type UpstreamModelLister interface {
	ListUpstreamModels(context.Context, *Auth) ([]UpstreamModel, error)
}
