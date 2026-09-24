package agent

import "errors"

var (
	ErrRequestCancelled = errors.New("request canceled by user")
	ErrSessionBusy      = errors.New("session is currently processing another request")
	ErrEmptyPrompt      = errors.New("prompt is empty")
	ErrSessionMissing   = errors.New("session id is missing")
	// ErrContextWindowExceeded is the harness-side stand-in for a
	// provider overflow rejection, raised only when the
	// enforce_context_window option is on: a manifest-pinned
	// context_window smaller than the endpoint's real one would
	// otherwise never trip the provider, so the agent rejects the
	// rendered request locally instead.
	ErrContextWindowExceeded = errors.New("rendered request exceeds the declared context window")
)
