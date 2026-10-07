package auth

import "context"

// UpstreamModelLister is optional; only OAuth executors with a model-list API implement it.
type UpstreamModelLister interface {
	ListUpstreamModels(context.Context, *Auth) ([]string, error)
}
