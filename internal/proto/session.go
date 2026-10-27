package proto

import (
	"encoding/json"

	"github.com/charmbracelet/crush/internal/config"
)

// Session represents a session in the proto layer.
//
// IsBusy is computed on read (it is not persisted with the session) and
// reflects whether an agent run is currently in flight for this session.
// It is populated by REST handlers in internal/server/proto.go from the
// workspace's AgentCoordinator. The Session SSE event path does not set
// it, since SSE consumers can compute presence from other agent signals.
//
// AttachedClients counts the number of clients currently viewing this
// session — i.e. entries in the workspace's clients map whose
// currentSessionID equals this session's ID and which have at least one
// live SSE stream. Hold-only clients (streams == 0) do not contribute.
// Like IsBusy, it is computed on read by REST handlers.
type Session struct {
	ID               string  `json:"id"`
	ParentSessionID  string  `json:"parent_session_id"`
	Title            string  `json:"title"`
	MessageCount     int64   `json:"message_count"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	SummaryMessageID string  `json:"summary_message_id"`
	Cost             float64 `json:"cost"`
	Todos            []Todo  `json:"todos,omitempty"`
	Channel          string  `json:"channel,omitempty"`
	CreatedAt        int64   `json:"created_at"`
	UpdatedAt        int64   `json:"updated_at"`
	IsBusy           bool    `json:"is_busy"`
	AttachedClients  int     `json:"attached_clients"`
	// PermissionSkip is the session's persisted auto-approve tri-state:
	// nil means it inherits the workspace flag, non-nil forces prompts
	// off (true) or on (false). Additive/omitempty.
	PermissionSkip *bool `json:"permission_skip,omitempty"`
	// ModelOverride pins the session's large model slot; nil means the
	// session follows the shared config selection. Additive/omitempty.
	ModelOverride *config.SelectedModel `json:"model_override,omitempty"`
}

// SelectedModelFromJSON decodes a persisted session model override —
// the domain's raw storage form, a serialized SelectedModel — into
// the typed wire value. nil/empty input yields (nil, nil).
func SelectedModelFromJSON(raw json.RawMessage) (*config.SelectedModel, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m config.SelectedModel
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// SelectedModelJSON encodes a typed selection for domain storage —
// the inverse of SelectedModelFromJSON. nil input yields nil.
func SelectedModelJSON(m *config.SelectedModel) (json.RawMessage, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// Todo represents a single plan item on a session in the proto layer.
// New PlanItem fields are additive-only (omitempty) so older clients
// keep decoding the flat shape.
type Todo struct {
	ID             string   `json:"id,omitempty"`
	Key            string   `json:"key,omitempty"`
	Content        string   `json:"content"`
	Status         string   `json:"status"`
	ActiveForm     string   `json:"active_form"`
	DependsOn      []string `json:"depends_on,omitempty"`
	EvidenceChecks []string `json:"evidence_checks,omitempty"`
	EvidencePaths  []string `json:"evidence_paths,omitempty"`
}
