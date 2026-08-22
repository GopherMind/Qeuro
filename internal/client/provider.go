package client

import "context"

// Provider is a source of streaming model responses. Client sends requests to
// the Qeuro backend; LocalProvider sends them directly to a model on this host.
type Provider interface {
	Chat(context.Context, ChatRequest) (<-chan Event, error)
}
