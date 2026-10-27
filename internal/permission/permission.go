package permission

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

// hookApprovalKey is the unexported context key used to mark a tool call as
// pre-approved by a PreToolUse hook. The value is the tool call ID so an
// approval can't be reused across calls that happen to share a context.
type hookApprovalKey struct{}

// WithHookApproval returns a context that marks the given tool call ID as
// pre-approved by a hook. When the permission service sees a matching
// request it short-circuits the normal prompt and grants immediately.
func WithHookApproval(ctx context.Context, toolCallID string) context.Context {
	return context.WithValue(ctx, hookApprovalKey{}, toolCallID)
}

// hookApproved reports whether the context carries a hook approval for the
// given tool call ID.
func hookApproved(ctx context.Context, toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	v, _ := ctx.Value(hookApprovalKey{}).(string)
	return v == toolCallID
}

type CreatePermissionRequest struct {
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
}

type PermissionNotification struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Granted    bool   `json:"granted"`
	Denied     bool   `json:"denied"`
	// Cancelled marks a request whose waiter left without an answer
	// (context cancelled) — a withdrawal, not a resolution. UIs use it
	// to dismiss or purge prompts for requests that can no longer
	// proceed.
	Cancelled bool `json:"cancelled,omitempty"`
}

type PermissionRequest struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
}

type Service interface {
	pubsub.Subscriber[PermissionRequest]
	// GrantPersistent grants a permission request and remembers the grant
	// for the session. It returns true if this call actually resolved the
	// pending request; false if the request had already been resolved
	// (e.g., by another concurrent caller) or is unknown.
	GrantPersistent(permission PermissionRequest) bool
	// Grant grants a permission request. It returns true if this call
	// actually resolved the pending request; false if the request had
	// already been resolved or is unknown.
	Grant(permission PermissionRequest) bool
	// Deny denies a permission request. It returns true if this call
	// actually resolved the pending request; false if the request had
	// already been resolved or is unknown.
	Deny(permission PermissionRequest) bool
	Request(ctx context.Context, opts CreatePermissionRequest) (bool, error)
	// ActiveRequest returns the request that is currently awaiting a
	// decision, or false when no request is pending. A request is only
	// reported while it can still be resolved; a request whose waiter
	// was cancelled is not returned. When several requests are parked
	// the most recently published one is preferred; if it has already
	// been resolved, any still-pending request is returned.
	ActiveRequest() (PermissionRequest, bool)
	// PendingRequests returns every request currently parked awaiting a
	// decision. Requests are only listed while they can still be
	// resolved; a request whose waiter was cancelled is not returned.
	PendingRequests() []PermissionRequest
	AutoApproveSession(sessionID string)
	// SetSessionAutoApprove sets the session's auto-approve override.
	// Unlike GrantPersistent it takes effect without answering a
	// pending request, so remote clients can arm or disarm a session
	// directly. The flag is checked at Request() entry — arming does
	// not resolve a request that is already awaiting a decision; that
	// request still needs a Grant/Deny (same semantics as the global
	// skip). approve=false is an explicit "prompt even if the
	// workspace skips" override, not a cleared flag — use
	// ClearSessionAutoApprove to return the session to inherit mode.
	SetSessionAutoApprove(sessionID string, approve bool)
	// ClearSessionAutoApprove removes the session's override so its
	// requests follow the workspace-level skip flag again.
	ClearSessionAutoApprove(sessionID string)
	// SessionAutoApproves reports whether permission requests on the
	// given session resolve without prompting: the session's override
	// when one is set, otherwise the workspace-level skip flag.
	SessionAutoApproves(sessionID string) bool
	SetSkipRequests(skip bool)
	SkipRequests() bool
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
}

// PermissionKey is a composite key for session permission lookups.
type PermissionKey struct {
	SessionID string
	ToolName  string
	Action    string
	Path      string
}

// permissionKey builds the remembered-grant lookup key for a request.
func permissionKey(r PermissionRequest) PermissionKey {
	return PermissionKey{
		SessionID: r.SessionID,
		ToolName:  r.ToolName,
		Action:    r.Action,
		Path:      r.Path,
	}
}

// pendingRequest is a parked permission request: the full request as
// published plus the channel its waiter blocks on.
type pendingRequest struct {
	req    PermissionRequest
	respCh chan bool
}

type permissionService struct {
	*pubsub.Broker[PermissionRequest]

	notificationBroker    *pubsub.Broker[PermissionNotification]
	workingDir            string
	sessionPermissions    *csync.Map[PermissionKey, bool]
	pendingRequests       *csync.Map[string, pendingRequest]
	autoApproveSessions   map[string]bool
	autoApproveSessionsMu sync.RWMutex
	// sessionOverrideMisses tombstones sessions whose persisted
	// override lookup came back nil — without it every Request on an
	// unflagged session would re-read the session row. Guarded by
	// autoApproveSessionsMu alongside the map it complements.
	sessionOverrideMisses map[string]struct{}
	skip                  atomic.Bool
	allowedTools          []string

	// requestMu serializes request registration and publishing so
	// concurrent calls emit events in a deterministic order. It is
	// released before the caller parks, so several tool calls can
	// await decisions concurrently.
	requestMu       sync.Mutex
	activeRequest   *PermissionRequest
	activeRequestMu sync.Mutex

	// sessionOverride resolves a session's persisted auto-approve
	// override on a map miss — restart rehydration without a startup
	// scan. Nil disables lookup; a nil return means the row carries
	// no override (or the session is unknown).
	sessionOverride func(sessionID string) *bool
}

// resolve atomically removes the pending request entry for the given
// permission and, if it was still pending, publishes exactly one
// PermissionNotification and forwards the outcome to the waiter on
// respCh. It returns the stored entry — the canonical request, not the
// caller-supplied copy — and true if this call resolved the request,
// or a zero entry and false if it had already been resolved (e.g., by
// another concurrent caller) or the request ID is unknown.
//
// All three public resolution methods (Grant, GrantPersistent, Deny)
// route through this helper so multi-subscriber UIs can race safely:
// the first caller wins, the rest become no-ops.
func (s *permissionService) resolve(permission PermissionRequest, granted, denied bool) (pendingRequest, bool) {
	entry, ok := s.pendingRequests.Take(permission.ID)
	if !ok {
		return pendingRequest{}, false
	}

	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		SessionID:  entry.req.SessionID,
		ToolCallID: entry.req.ToolCallID,
		Granted:    granted,
		Denied:     denied,
	})

	// respCh is buffered (cap 1) and only ever has at most one sender
	// per request because Take removes the entry under the map lock,
	// so this send never blocks.
	entry.respCh <- granted

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
	return entry, true
}

func (s *permissionService) GrantPersistent(permission PermissionRequest) bool {
	// Record the persistent grant only if this call wins the
	// pending-request race, and key it off the stored request rather
	// than the caller-supplied one — a stale caller payload must not
	// skew which calls get auto-approved. Otherwise a losing
	// GrantPersistent that lost to a Deny would still leave an
	// auto-approve entry behind, silently flipping later denied calls
	// to allowed.
	entry, ok := s.resolve(permission, true, false)
	if !ok {
		return false
	}
	s.sessionPermissions.Set(permissionKey(entry.req), true)

	// A persistent grant also covers requests already parked with the
	// same key: with concurrent parking, a second same-key call can
	// register before the grant lands. Resolve them here rather than
	// re-prompting for an action class just approved for the session.
	// Entries resolved concurrently lose their Take inside resolve and
	// are skipped.
	//
	// requestMu serializes the sweep against registerRequest's
	// check-and-insert, so a same-key request cannot slip between the
	// grant being recorded and the sweep reading the map: it either
	// registered first (the sweep resolves it) or registers after (its
	// sessionPermissions check hits and it is approved inline).
	key := permissionKey(entry.req)
	s.requestMu.Lock()
	for _, queued := range s.pendingRequests.Seq2() {
		if permissionKey(queued.req) == key {
			s.resolve(queued.req, true, false)
		}
	}
	s.requestMu.Unlock()
	return true
}

func (s *permissionService) Grant(permission PermissionRequest) bool {
	_, ok := s.resolve(permission, true, false)
	return ok
}

func (s *permissionService) Deny(permission PermissionRequest) bool {
	_, ok := s.resolve(permission, false, true)
	return ok
}

func (s *permissionService) Request(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	// Resolution order: the session's persisted override wins over the
	// workspace flag — an explicit off forces prompts even under a
	// workspace skip, an explicit on approves even without one, and an
	// absent override inherits the flag. Resolved once: the override
	// lookup can hit the session store on a cold miss.
	override, hasOverride := s.sessionAutoApproveOverride(opts.SessionID)
	if hasOverride && override {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			SessionID:  opts.SessionID,
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}
	// The workspace skip stays silent — it predates the notification
	// surface, and publishing granted without a matching request-start
	// would leave subscribers pairing half a flow. The session-on path
	// above publishes because it shares the auto-approve machinery
	// that always notified. (Asymmetric, inherited, intentional.)
	if !hasOverride && s.skip.Load() {
		return true, nil
	}

	// Check if the tool/action combination is in the allowlist
	commandKey := opts.ToolName + ":" + opts.Action
	if slices.Contains(s.allowedTools, commandKey) || slices.Contains(s.allowedTools, opts.ToolName) {
		return true, nil
	}

	// A PreToolUse hook that returned decision=allow stamps the context
	// with the tool call ID. Treat that as a pre-approval and skip the
	// prompt entirely. We still publish a granted notification so the UI
	// and audit subscribers see the outcome.
	if hookApproved(ctx, opts.ToolCallID) {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			SessionID:  opts.SessionID,
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	fileInfo, err := os.Stat(opts.Path)
	dir := opts.Path
	if err == nil {
		if fileInfo.IsDir() {
			dir = opts.Path
		} else {
			dir = filepath.Dir(opts.Path)
		}
	}

	if dir == "." {
		dir = s.workingDir
	}

	entry, approved := s.registerRequest(opts, dir)
	if approved {
		return true, nil
	}
	defer s.pendingRequests.Del(entry.req.ID)

	// registerRequest returns with requestMu already released, so the
	// caller parks without serializing other requests: several tool
	// calls may await decisions concurrently, and every parked request
	// stays listed in pendingRequests for replay and enumeration.
	select {
	case <-ctx.Done():
		// The waiter is leaving without an answer, but cancellation
		// races resolution: Take arbitrates. If the entry is already
		// gone, a resolver won and its buffered send on respCh is
		// guaranteed — consume the real outcome rather than publish a
		// spurious withdrawal after a Granted/Denied notification.
		if _, ok := s.pendingRequests.Take(entry.req.ID); !ok {
			return <-entry.respCh, nil
		}
		// We won the withdrawal: drop the pending entry and publish so
		// UIs can dismiss the prompt (or purge a queued copy) instead
		// of surfacing a dead request.
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			SessionID:  entry.req.SessionID,
			ToolCallID: entry.req.ToolCallID,
			Cancelled:  true,
		})
		return false, ctx.Err()
	case granted := <-entry.respCh:
		return granted, nil
	}
}

// registerRequest performs the locked section of Request: it publishes
// the request-start notification, applies the inline-approval checks
// (auto-approve and remembered session grant), and — when approval is
// not short-circuited — builds, registers, and publishes the pending
// request. requestMu is held for the whole section so notification,
// registration, and request events stay in deterministic order across
// concurrent callers. It returns the parked entry, or approved=true
// when the request was granted inline and nothing parked.
func (s *permissionService) registerRequest(opts CreatePermissionRequest, dir string) (pendingRequest, bool) {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	// tell the UI that a permission was requested
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		SessionID:  opts.SessionID,
		ToolCallID: opts.ToolCallID,
	})

	s.autoApproveSessionsMu.RLock()
	autoApprove := s.autoApproveSessions[opts.SessionID]
	s.autoApproveSessionsMu.RUnlock()

	if autoApprove {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			SessionID:  opts.SessionID,
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return pendingRequest{}, true
	}

	permission := PermissionRequest{
		ID:          uuid.New().String(),
		Path:        dir,
		SessionID:   opts.SessionID,
		ToolCallID:  opts.ToolCallID,
		ToolName:    opts.ToolName,
		Description: opts.Description,
		Action:      opts.Action,
		Params:      opts.Params,
	}

	if _, ok := s.sessionPermissions.Get(permissionKey(permission)); ok {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			SessionID:  opts.SessionID,
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return pendingRequest{}, true
	}

	s.activeRequestMu.Lock()
	s.activeRequest = &permission
	s.activeRequestMu.Unlock()

	respCh := make(chan bool, 1)
	entry := pendingRequest{req: permission, respCh: respCh}
	s.pendingRequests.Set(permission.ID, entry)

	s.Publish(pubsub.CreatedEvent, permission)
	return entry, false
}

// ActiveRequest returns the request that is currently awaiting a
// decision. The active pointer can outlive the waiter that published
// it (Request leaves it behind when its context is cancelled), so a
// request is only reported while its ID is still registered in
// pendingRequests, i.e. while Grant/Deny can actually resolve it. When
// the recorded request is gone but others remain parked — possible now
// that requests park concurrently — a still-pending one is returned.
func (s *permissionService) ActiveRequest() (PermissionRequest, bool) {
	s.activeRequestMu.Lock()
	active := s.activeRequest
	s.activeRequestMu.Unlock()
	if active != nil {
		if _, ok := s.pendingRequests.Get(active.ID); ok {
			return *active, true
		}
	}
	for _, entry := range s.pendingRequests.Seq2() {
		return entry.req, true
	}
	return PermissionRequest{}, false
}

// PendingRequests returns every request currently parked awaiting a
// decision.
func (s *permissionService) PendingRequests() []PermissionRequest {
	var out []PermissionRequest
	for _, entry := range s.pendingRequests.Seq2() {
		out = append(out, entry.req)
	}
	return out
}

func (s *permissionService) AutoApproveSession(sessionID string) {
	s.SetSessionAutoApprove(sessionID, true)
}

func (s *permissionService) SetSessionAutoApprove(sessionID string, approve bool) {
	s.autoApproveSessionsMu.Lock()
	s.autoApproveSessions[sessionID] = approve
	delete(s.sessionOverrideMisses, sessionID)
	s.autoApproveSessionsMu.Unlock()
}

func (s *permissionService) ClearSessionAutoApprove(sessionID string) {
	s.autoApproveSessionsMu.Lock()
	delete(s.autoApproveSessions, sessionID)
	delete(s.sessionOverrideMisses, sessionID)
	s.autoApproveSessionsMu.Unlock()
}

// sessionAutoApproveOverride returns the session's explicit override
// and whether one is set; absent means inherit the workspace flag. On
// a map miss the persisted row is consulted (when a lookup is wired)
// and the result seeded, so a restart honors overrides set before the
// process died without a startup scan.
func (s *permissionService) sessionAutoApproveOverride(sessionID string) (bool, bool) {
	s.autoApproveSessionsMu.RLock()
	v, ok := s.autoApproveSessions[sessionID]
	_, missed := s.sessionOverrideMisses[sessionID]
	s.autoApproveSessionsMu.RUnlock()
	if ok || missed || s.sessionOverride == nil {
		return v, ok
	}
	persisted := s.sessionOverride(sessionID)
	// Seed under the write lock so the store can't interleave with a
	// concurrent Set/Clear — both hold the same mutex. If a writer
	// landed first the seeded value is stale: re-check and keep the
	// fresh one. A nil lookup seeds a tombstone instead so later
	// requests skip the store read entirely.
	s.autoApproveSessionsMu.Lock()
	defer s.autoApproveSessionsMu.Unlock()
	if v, ok := s.autoApproveSessions[sessionID]; ok {
		return v, true
	}
	if _, ok := s.sessionOverrideMisses[sessionID]; ok {
		return false, false
	}
	if persisted == nil {
		s.sessionOverrideMisses[sessionID] = struct{}{}
		return false, false
	}
	s.autoApproveSessions[sessionID] = *persisted
	return *persisted, true
}

func (s *permissionService) SessionAutoApproves(sessionID string) bool {
	if override, ok := s.sessionAutoApproveOverride(sessionID); ok {
		return override
	}
	return s.skip.Load()
}

func (s *permissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification] {
	return s.notificationBroker.Subscribe(ctx)
}

func (s *permissionService) SetSkipRequests(skip bool) {
	s.skip.Store(skip)
}

func (s *permissionService) SkipRequests() bool {
	return s.skip.Load()
}

// Option configures a permissionService at construction.
type Option func(*permissionService)

// WithSessionOverride wires a lookup that resolves a session's
// persisted auto-approve override on a map miss — the restart
// rehydration path. The lookup returns the row's tri-state: nil for
// no override, non-nil for an explicit on/off.
func WithSessionOverride(lookup func(sessionID string) *bool) Option {
	return func(s *permissionService) {
		s.sessionOverride = lookup
	}
}

func NewPermissionService(workingDir string, skip bool, allowedTools []string, opts ...Option) Service {
	svc := &permissionService{
		Broker:                pubsub.NewBroker[PermissionRequest](),
		notificationBroker:    pubsub.NewBroker[PermissionNotification](),
		workingDir:            workingDir,
		sessionPermissions:    csync.NewMap[PermissionKey, bool](),
		autoApproveSessions:   make(map[string]bool),
		sessionOverrideMisses: make(map[string]struct{}),
		allowedTools:          allowedTools,
		pendingRequests:       csync.NewMap[string, pendingRequest](),
	}
	for _, opt := range opts {
		opt(svc)
	}
	svc.skip.Store(skip)
	return svc
}
