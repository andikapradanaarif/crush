package permission

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"

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

// PermissionMode represents the current permission mode.
type PermissionMode int

const (
	// PermissionModeNormal prompts for all non-safe commands.
	PermissionModeNormal PermissionMode = iota
	// PermissionModeYolo auto-approves every request, including dangerous
	// commands, but keeps the exec-time block list armed so a dangerous
	// command that only surfaces once the shell runs it is still caught.
	PermissionModeYolo
	// PermissionModeSysadmin auto-approves every request and drops the
	// exec-time block list as well, so nothing is checked at any point.
	PermissionModeSysadmin
)

// SkipsPrompts reports whether the mode answers requests itself instead of
// putting them in front of the user. Yolo and sysadmin both do; they part
// ways only later, at exec time, over the block list.
func (m PermissionMode) SkipsPrompts() bool {
	return m == PermissionModeYolo || m == PermissionModeSysadmin
}

type CreatePermissionRequest struct {
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
	Danger      string `json:"danger,omitempty"`
	// GrantKey narrows what "allow for this session" covers. Leave it empty
	// when Path already says what is being acted on; set it when it does
	// not, as for a shell command, where Path is only the directory.
	GrantKey string `json:"grant_key,omitempty"`
}

type PermissionNotification struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Granted    bool   `json:"granted"`
	Denied     bool   `json:"denied"`
	// Cancelled is set when the request was abandoned before a decision
	// (the turn was cancelled while the prompt was open).
	Cancelled bool `json:"cancelled"`
}

// Outcome is how a permission prompt the user saw was resolved.
type Outcome string

const (
	OutcomeGranted   Outcome = "granted"
	OutcomeDenied    Outcome = "denied"
	OutcomeCancelled Outcome = "cancelled"
)

// PromptObserver hears about every prompt the user is shown and how it
// was resolved, in that order. Auto-approved calls show no prompt and
// are not reported. Calls run off the requesting goroutine, so a slow
// observer never delays the prompt or the tool.
type PromptObserver interface {
	PromptShown(ctx context.Context, req PermissionRequest)
	PromptResolved(ctx context.Context, req PermissionRequest, outcome Outcome)
}

// ModeChangedEvent is published whenever the permission mode changes.
type ModeChangedEvent struct {
	Mode PermissionMode `json:"mode"`
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
	Danger      string `json:"danger,omitempty"`
	// GrantKey narrows what a session grant covers. See
	// [CreatePermissionRequest.GrantKey].
	GrantKey string `json:"grant_key,omitempty"`
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
	// request still needs a Grant/Deny (same semantics as the
	// workspace mode). approve=false is an explicit "prompt even if the
	// workspace mode skips" override, not a cleared flag — use
	// ClearSessionAutoApprove to return the session to inherit mode.
	SetSessionAutoApprove(sessionID string, approve bool)
	// ClearSessionAutoApprove removes the session's override so its
	// requests follow the workspace permission mode again.
	ClearSessionAutoApprove(sessionID string)
	// SessionAutoApproves reports whether permission requests on the
	// given session resolve without prompting: the session's override
	// when one is set, otherwise the workspace permission mode.
	SessionAutoApproves(sessionID string) bool
	SetPermissionMode(mode PermissionMode)
	PermissionMode() PermissionMode
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
	SubscribeModeChanges(ctx context.Context) <-chan pubsub.Event[ModeChangedEvent]
}

// PermissionKey is a composite key for session permission lookups.
type PermissionKey struct {
	SessionID string
	ToolName  string
	Action    string
	Path      string
	// Grant narrows a session grant to one specific operation. For tools
	// whose Path already identifies what is being touched, such as reading
	// or editing a file, it is empty and the path carries the meaning. For
	// bash it holds the command, because there the path is only the working
	// directory: without this, approving `ls` for the session would approve
	// every later command run in the same directory, `sudo` included.
	Grant string
}

// pendingRequest couples a request with the channel its caller waits on.
//
// Keeping the request rather than only the channel means a resolution is
// recorded against what the server actually asked, not against whatever the
// answering client says it was answering. Those are the same thing when the
// client is honest and the difference matters when it is not.
type pendingRequest struct {
	req    PermissionRequest
	respCh chan bool
}

type permissionService struct {
	*pubsub.Broker[PermissionRequest]

	notificationBroker    *pubsub.Broker[PermissionNotification]
	modeBroker            *pubsub.Broker[ModeChangedEvent]
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
	// sessionOverrideGen counts Set/Clear mutations per session. A
	// lazy seed captures it before the store read and re-verifies
	// under the write lock — a bump means a writer interleaved, so the
	// stale store read must not be seeded (the resurrect-race: read
	// old row → writer persists+clears → seed resurrects it).
	sessionOverrideGen map[string]uint64
	allowedTools       []string
	observer           PromptObserver
	mode               PermissionMode
	modeMu             sync.RWMutex

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
	sessionOverride func(sessionID string) (*bool, error)
}

// resolve atomically removes the pending request entry for the given
// permission and, if it was still pending, publishes exactly one
// PermissionNotification and forwards the outcome to the waiter on
// respCh. It returns true if this call resolved the request, false if
// it had already been resolved (e.g., by another concurrent caller) or
// the request ID is unknown.
//
// If onResolve is non-nil it runs after the pending entry has been
// taken but before the notification is published or the waiter is
// unblocked. This lets GrantPersistent record the session permission
// only when it actually wins the race, so a losing GrantPersistent
// that lost to a Deny does not leak an auto-approve entry.
//
// All three public resolution methods (Grant, GrantPersistent, Deny)
// route through this helper so multi-subscriber UIs can race safely:
// the first caller wins, the rest become no-ops.
func (s *permissionService) resolve(permission PermissionRequest, granted, denied bool, onResolve func(PermissionRequest)) bool {
	pending, ok := s.pendingRequests.Take(permission.ID)
	if !ok {
		return false
	}

	// Everything below describes the request the server issued, not the copy
	// the answer arrived with. Only the ID is taken from the caller, and it
	// is unguessable and single-use. A client that echoed back a different
	// session, tool, action or path would otherwise have that echo recorded
	// as the thing being approved.
	req := pending.req

	if onResolve != nil {
		onResolve(req)
	}

	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		SessionID:  req.SessionID,
		ToolCallID: req.ToolCallID,
		Granted:    granted,
		Denied:     denied,
	})

	// respCh is buffered (cap 1) and only ever has at most one sender
	// per request because Take removes the entry under the map lock,
	// so this send never blocks.
	pending.respCh <- granted

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == req.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
	return true
}

// permissionKeyFor builds the session-grant key for a request.
func permissionKeyFor(req PermissionRequest) PermissionKey {
	return PermissionKey{
		SessionID: req.SessionID,
		ToolName:  req.ToolName,
		Action:    req.Action,
		Path:      req.Path,
		Grant:     req.GrantKey,
	}
}

func (s *permissionService) GrantPersistent(permission PermissionRequest) bool {
	// Record the persistent grant only if this call wins the
	// pending-request race. Otherwise a losing GrantPersistent that
	// lost to a Deny would still leave an auto-approve entry behind,
	// silently flipping later denied calls to allowed.
	var req PermissionRequest
	if !s.resolve(permission, true, false, func(r PermissionRequest) {
		req = r
		s.sessionPermissions.Set(permissionKeyFor(r), true)
	}) {
		return false
	}

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
	key := permissionKeyFor(req)
	s.requestMu.Lock()
	for _, queued := range s.pendingRequests.Seq2() {
		if permissionKeyFor(queued.req) == key {
			s.resolve(queued.req, true, false, nil)
		}
	}
	s.requestMu.Unlock()
	return true
}

func (s *permissionService) Grant(permission PermissionRequest) bool {
	return s.resolve(permission, true, false, nil)
}

func (s *permissionService) Deny(permission PermissionRequest) bool {
	return s.resolve(permission, false, true, nil)
}

func (s *permissionService) Request(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	// Resolution order: the session's persisted override wins over the
	// workspace mode — an explicit off forces prompts even under a
	// prompt-skipping mode, and an absent override inherits the mode.
	// An explicit on falls through to registerRequest's auto-approve
	// path (the lookup already seeded the map) so it still emits
	// request-start + granted like any live approval.
	_, hasOverride := s.sessionAutoApproveOverride(opts.SessionID)
	if !hasOverride {
		mode := s.PermissionMode()
		// Both yolo and sysadmin mean "stop asking me", so neither
		// prompts, not even for a dangerous command. They part ways
		// after this, at exec time: yolo keeps the block list armed
		// for commands the static check did not flag, and sysadmin
		// disarms it entirely. Normal mode is the only one that still
		// puts a dangerous command in front of the user.
		if mode.SkipsPrompts() {
			return true, nil
		}
	}

	// The explicit-off override governs only this blanket-skip layer.
	// Narrower, deliberate approvals still bypass prompts under it:
	// the allowlist below, remembered per-session grants, and hook
	// approvals. "Force prompts" means no blanket approval, not "no
	// approval mechanism may fire."

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

	resolved := s.observe(ctx, entry.req)

	// registerRequest returns with requestMu already released, so the
	// caller parks without serializing other requests: several tool
	// calls may await decisions concurrently, and every parked request
	// stays listed in pendingRequests for replay and enumeration.
	select {
	case <-ctx.Done():
		// The prompt is abandoned without a decision; tell subscribers
		// so the request they saw is not left open.
		if _, ok := s.pendingRequests.Take(entry.req.ID); ok {
			s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
				SessionID:  opts.SessionID,
				ToolCallID: opts.ToolCallID,
				Cancelled:  true,
			})
			resolved(OutcomeCancelled)
			return false, ctx.Err()
		}
		// A decision won the race; resolve always delivers it.
		resolved(outcomeOf(<-entry.respCh))
		return false, ctx.Err()
	case granted := <-entry.respCh:
		resolved(outcomeOf(granted))
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
		Danger:      opts.Danger,
		GrantKey:    opts.GrantKey,
	}

	if _, ok := s.sessionPermissions.Get(permissionKeyFor(permission)); ok {
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

// observe reports the prompt to the observer and returns the function
// that reports its outcome. Both run on one goroutine so the observer
// always hears them in order, and outlive ctx so a cancelled turn still
// reports the cancellation.
func (s *permissionService) observe(ctx context.Context, req PermissionRequest) func(Outcome) {
	if s.observer == nil {
		return func(Outcome) {}
	}
	ctx = context.WithoutCancel(ctx)
	outcome := make(chan Outcome, 1)
	go func() {
		s.observer.PromptShown(ctx, req)
		s.observer.PromptResolved(ctx, req, <-outcome)
	}()
	return func(o Outcome) { outcome <- o }
}

func outcomeOf(granted bool) Outcome {
	if granted {
		return OutcomeGranted
	}
	return OutcomeDenied
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
	s.sessionOverrideGen[sessionID]++
	s.autoApproveSessionsMu.Unlock()
}

func (s *permissionService) ClearSessionAutoApprove(sessionID string) {
	s.autoApproveSessionsMu.Lock()
	delete(s.autoApproveSessions, sessionID)
	delete(s.sessionOverrideMisses, sessionID)
	s.sessionOverrideGen[sessionID]++
	s.autoApproveSessionsMu.Unlock()
}

// sessionAutoApproveOverride returns the session's explicit override
// and whether one is set; absent means inherit the workspace mode. On
// a map miss the persisted row is consulted (when a lookup is wired)
// and the result seeded, so a restart honors overrides set before the
// process died without a startup scan.
func (s *permissionService) sessionAutoApproveOverride(sessionID string) (bool, bool) {
	s.autoApproveSessionsMu.RLock()
	v, ok := s.autoApproveSessions[sessionID]
	_, missed := s.sessionOverrideMisses[sessionID]
	gen := s.sessionOverrideGen[sessionID]
	s.autoApproveSessionsMu.RUnlock()
	if ok || missed || s.sessionOverride == nil {
		return v, ok
	}
	persisted, err := s.sessionOverride(sessionID)
	if err != nil {
		// Transient failure — retry next request rather than
		// tombstone a suppression until restart. Fail closed while
		// it's unreadable: reporting an explicit-off prompts, where
		// reporting "no override" would let a workspace mode
		// auto-approve a session that may be pinned restrictive.
		return false, true
	}
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
	if s.sessionOverrideGen[sessionID] != gen {
		// A Set/Clear interleaved with our store read — `persisted`
		// may describe the row before it. The map is the final writer's
		// state and it's empty, so the writer was a Clear: tombstone
		// is the fresh verdict (the row is NULL).
		s.sessionOverrideMisses[sessionID] = struct{}{}
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
	return s.PermissionMode().SkipsPrompts()
}

func (s *permissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification] {
	return s.notificationBroker.Subscribe(ctx)
}

func (s *permissionService) SetPermissionMode(mode PermissionMode) {
	s.modeMu.Lock()
	s.mode = mode
	s.modeMu.Unlock()
	s.modeBroker.Publish(pubsub.UpdatedEvent, ModeChangedEvent{Mode: mode})
}

func (s *permissionService) SubscribeModeChanges(ctx context.Context) <-chan pubsub.Event[ModeChangedEvent] {
	return s.modeBroker.Subscribe(ctx)
}

func (s *permissionService) PermissionMode() PermissionMode {
	s.modeMu.RLock()
	defer s.modeMu.RUnlock()
	return s.mode
}

// Option configures a permissionService at construction.
type Option func(*permissionService)

// WithSessionOverride wires a lookup that resolves a session's
// persisted auto-approve override on a map miss — the restart
// rehydration path. The lookup returns the row's tri-state: nil for
// no override, non-nil for an explicit on/off.
func WithSessionOverride(lookup func(sessionID string) (*bool, error)) Option {
	return func(s *permissionService) {
		s.sessionOverride = lookup
	}
}

func NewPermissionService(workingDir string, allowedTools []string, observer PromptObserver, opts ...Option) Service {
	svc := &permissionService{
		Broker:                pubsub.NewBroker[PermissionRequest](),
		notificationBroker:    pubsub.NewBroker[PermissionNotification](),
		modeBroker:            pubsub.NewBroker[ModeChangedEvent](),
		workingDir:            workingDir,
		sessionPermissions:    csync.NewMap[PermissionKey, bool](),
		autoApproveSessions:   make(map[string]bool),
		sessionOverrideMisses: make(map[string]struct{}),
		sessionOverrideGen:    make(map[string]uint64),
		mode:                  PermissionModeNormal,
		allowedTools:          allowedTools,
		pendingRequests:       csync.NewMap[string, pendingRequest](),
		observer:              observer,
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}
