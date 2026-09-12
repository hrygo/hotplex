package messaging

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/hrygo/hotplex/pkg/events"
)

// NewSendResponseFunc creates a SendResponse closure for registering pending interactions.
func NewSendResponseFunc(log *slog.Logger, bridge *Bridge, requestID, sessionID, ownerID string, conn PlatformConn) func(map[string]any) {
	return func(metadata map[string]any) {
		respCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := SendInteractionResponse(respCtx, bridge, requestID, sessionID, ownerID, conn, metadata); err != nil {
			log.Error("interaction: failed to send response",
				"request_id", requestID,
				"session_id", sessionID,
				"err", err)
		}
	}
}

// NewSendResponseSyncFunc creates a response sender for interactive clients
// that must render an accurate success or failure state before acknowledging a
// button click. A nil error means the current worker accepted the response.
func NewSendResponseSyncFunc(bridge *Bridge, requestID, sessionID, ownerID string, conn PlatformConn) func(context.Context, map[string]any) error {
	return func(ctx context.Context, metadata map[string]any) error {
		return SendInteractionResponse(ctx, bridge, requestID, sessionID, ownerID, conn, metadata)
	}
}

// SendInteractionResponse delivers a standard interaction response through the
// gateway to the active worker. It is shared by asynchronous timeout handling
// and synchronous card actions so only successful worker delivery is reported
// as a submitted response.
func SendInteractionResponse(ctx context.Context, bridge *Bridge, requestID, sessionID, ownerID string, conn PlatformConn, metadata map[string]any) error {
	if bridge == nil {
		return fmt.Errorf("interaction: bridge not available")
	}
	env := &events.Envelope{
		Version:   events.Version,
		ID:        requestID,
		SessionID: sessionID,
		Event: events.Event{
			Type: events.Input,
			Data: map[string]any{
				"content":  "",
				"metadata": metadata,
			},
		},
		OwnerID: ownerID,
	}
	if err := bridge.Handle(ctx, env, conn); err != nil {
		return fmt.Errorf("interaction: send response: %w", err)
	}
	return nil
}

// BuildPermissionResponse creates a permission_response metadata map.
func BuildPermissionResponse(requestID string, allowed bool, reason string) map[string]any {
	return map[string]any{
		"permission_response": map[string]any{
			"request_id": requestID,
			"allowed":    allowed,
			"reason":     reason,
		},
	}
}

// BuildQuestionResponse creates a question_response metadata map.
func BuildQuestionResponse(requestID, answer string) map[string]any {
	answers := map[string]string{}
	if answer != "" {
		answers["_"] = answer
	}
	return BuildQuestionResponseAnswers(requestID, answers)
}

// BuildQuestionResponseAnswers creates a question_response with one or more
// named answers. Form-based cards use this for multi-select and multi-question
// requests, while the single-answer helper retains the legacy "_" key.
func BuildQuestionResponseAnswers(requestID string, answers map[string]string) map[string]any {
	return BuildQuestionResponseAnswersWithOrder(requestID, answers, nil)
}

// BuildQuestionResponseAnswersWithOrder includes the original question order
// for workers whose native protocol accepts positional answer arrays.
func BuildQuestionResponseAnswersWithOrder(requestID string, answers map[string]string, questionOrder []string) map[string]any {
	options := make(map[string][]string, len(answers))
	for question, answer := range answers {
		options[question] = []string{answer}
	}
	return BuildQuestionResponseOptionsWithOrder(requestID, options, questionOrder)
}

// BuildQuestionResponseOptionsWithOrder preserves all selected values for
// multi-select questions while remaining compatible with single answers.
func BuildQuestionResponseOptionsWithOrder(requestID string, answers map[string][]string, questionOrder []string) map[string]any {
	response := map[string]any{
		"id":      requestID,
		"answers": answers,
	}
	if len(questionOrder) > 0 {
		response["question_order"] = questionOrder
	}
	return map[string]any{
		"question_response": response,
	}
}

// BuildElicitationResponse creates an elicitation_response metadata map.
func BuildElicitationResponse(requestID, action string) map[string]any {
	return map[string]any{
		"elicitation_response": map[string]any{
			"id":     requestID,
			"action": action,
		},
	}
}

const (
	// DefaultInteractionTimeout is the default timeout for user interactions.
	DefaultInteractionTimeout = 5 * time.Minute
)

// PendingInteraction represents an interaction request awaiting a user response.
type PendingInteraction struct {
	ID        string            // request ID from the worker
	SessionID string            // session ID
	OwnerID   string            // user ID of the interaction owner (for auth verification)
	Type      events.Kind       // PermissionRequest, QuestionRequest, ElicitationRequest
	CreatedAt time.Time         // when the request was created
	Timeout   time.Duration     // timeout duration
	Questions []events.Question // original question schema for platform form decoding
	// SendResponse sends the user's response back through the bridge.
	// The metadata map contains the response data specific to the interaction type.
	SendResponse func(metadata map[string]any)
	// SendResponseSync delivers the response and reports whether the active
	// worker accepted it. Interactive card actions use it to avoid presenting a
	// successful state before the worker can continue.
	SendResponseSync func(context.Context, map[string]any) error
	// cancelCh identifies one registration and is closed on completion or cancellation.
	cancelCh  chan struct{}
	resolving bool
}

// InteractionManager manages pending user interactions (permission requests,
// question requests, MCP elicitation requests) with timeout support.
type InteractionManager struct {
	mu      sync.RWMutex
	pending map[string]*PendingInteraction // keyed by request ID
	log     *slog.Logger
}

// NewInteractionManager creates a new InteractionManager.
func NewInteractionManager(log *slog.Logger) *InteractionManager {
	return &InteractionManager{
		pending: make(map[string]*PendingInteraction),
		log:     log,
	}
}

// Register adds a new pending interaction and starts its timeout timer.
// If an interaction with the same ID already exists, this is a no-op.
func (m *InteractionManager) Register(pi *PendingInteraction) {
	m.mu.Lock()

	// Dedup: avoid spawning multiple timeout goroutines for the same request ID.
	if _, exists := m.pending[pi.ID]; exists {
		m.mu.Unlock()
		m.log.Debug("interaction: duplicate register, ignoring", "request_id", pi.ID)
		return
	}

	pi.cancelCh = make(chan struct{})
	pi.resolving = false
	cancelCh, timeout := pi.cancelCh, pi.Timeout
	m.pending[pi.ID] = pi
	m.mu.Unlock()

	// Capture the registration signal before another caller can complete and
	// re-register the same object. A watcher never follows a replacement.
	go m.watchRegistrationTimeout(pi, cancelCh, timeout)
}

// Get retrieves a pending interaction by its request ID.
func (m *InteractionManager) Get(requestID string) (*PendingInteraction, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	pi, ok := m.pending[requestID]
	return pi, ok
}

// Complete removes a pending interaction after a response is received.
func (m *InteractionManager) Complete(requestID string) (*PendingInteraction, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pi, ok := m.pending[requestID]
	if ok {
		delete(m.pending, requestID)
		if pi.cancelCh != nil {
			close(pi.cancelCh)
		}
	}
	return pi, ok
}

// Claim marks a pending interaction as being delivered. Only one button or
// text response can claim a request; callers must follow with CompleteClaimed
// on success or Release on failure.
func (m *InteractionManager) Claim(requestID string) (*PendingInteraction, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pi, ok := m.pending[requestID]
	if !ok || pi.resolving {
		return nil, false
	}
	pi.resolving = true
	return pi, true
}

// CompleteClaimed removes an interaction after its response was successfully
// accepted by the worker. It rejects unclaimed requests to preserve the
// claim/finish/release state machine.
func (m *InteractionManager) CompleteClaimed(requestID string) (*PendingInteraction, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pi, ok := m.pending[requestID]
	if !ok || !pi.resolving {
		return nil, false
	}
	delete(m.pending, requestID)
	if pi.cancelCh != nil {
		close(pi.cancelCh)
	}
	return pi, true
}

// Release makes a claimed interaction available for retry after a delivery
// failure. It is safe to call after cancellation; in that case it is a no-op.
func (m *InteractionManager) Release(requestID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	pi, ok := m.pending[requestID]
	if !ok || !pi.resolving {
		return false
	}
	pi.resolving = false
	return true
}

// Len returns the number of pending interactions.
func (m *InteractionManager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.pending)
}

// GetAll returns a snapshot of all pending interactions.
// The returned slice is ordered by creation time (most recent first).
func (m *InteractionManager) GetAll() []*PendingInteraction {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*PendingInteraction, 0, len(m.pending))
	for _, pi := range m.pending {
		result = append(result, pi)
	}
	slices.SortFunc(result, func(a, b *PendingInteraction) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return result
}

// GetBySession returns pending interactions for a specific session, ordered by
// creation time (most recent first). Returns nil if sessionID is empty or no
// interactions match.
func (m *InteractionManager) GetBySession(sessionID string) []*PendingInteraction {
	if sessionID == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*PendingInteraction
	for _, pi := range m.pending {
		if pi.SessionID == sessionID && !pi.resolving {
			result = append(result, pi)
		}
	}
	slices.SortFunc(result, func(a, b *PendingInteraction) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return result
}

// watchTimeout waits for the interaction timeout and auto-denies.
func (m *InteractionManager) watchRegistrationTimeout(pi *PendingInteraction, cancelCh chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-cancelCh:
		// This exact registration was completed or cancelled.
		return
	case <-timer.C:
	}

	// Claim before auto-denying. A card click may already be submitting the
	// response; in that case it owns the resolution and the timeout must not
	// race it with a stale denial.
	claimed, ok := m.completeTimedOutRegistration(pi, cancelCh)
	if !ok {
		return
	}

	m.log.Info("interaction: timeout, auto-denying",
		"request_id", claimed.ID,
		"type", claimed.Type,
		"session_id", claimed.SessionID)

	// Recover from panics in SendResponse — platform connection may be closed.
	func() {
		defer func() {
			if r := recover(); r != nil {
				m.log.Error("interaction: panic in SendResponse during timeout",
					"request_id", claimed.ID,
					"type", claimed.Type,
					"session_id", claimed.SessionID,
					"panic", r)
			}
		}()

		// Send auto-deny/reject response based on type
		switch claimed.Type {
		case events.PermissionRequest:
			claimed.SendResponse(BuildPermissionResponse(claimed.ID, false, "interaction timed out"))
		case events.QuestionRequest:
			claimed.SendResponse(BuildQuestionResponse(claimed.ID, ""))
		case events.ElicitationRequest:
			claimed.SendResponse(BuildElicitationResponse(claimed.ID, "cancel"))
		}
	}()
}

// completeTimedOutRegistration performs identity comparison, claim and removal
// in one critical section. A timer already firing during Complete/CancelAll
// cannot claim a newer request that happens to reuse the worker request ID.
func (m *InteractionManager) completeTimedOutRegistration(pi *PendingInteraction, cancelCh chan struct{}) (*PendingInteraction, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.pending[pi.ID]
	if !ok || current != pi || current.cancelCh != cancelCh || current.resolving {
		return nil, false
	}
	current.resolving = true
	delete(m.pending, pi.ID)
	if cancelCh != nil {
		close(cancelCh)
	}
	// The response runs outside the lock. Copy the registration fields so a
	// later registration of the same input object cannot change its kind.
	snapshot := *current
	return &snapshot, true
}

// CancelAll removes all pending interactions for a given session.
// Called when a session ends (GC/Reset/Close). Nil-safe: no-op if m is nil.
func (m *InteractionManager) CancelAll(sessionID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, pi := range m.pending {
		if pi.SessionID == sessionID {
			delete(m.pending, id)
			close(pi.cancelCh)
			m.log.Debug("interaction: cancelled", "request_id", id, "session_id", sessionID)
		}
	}
}

// ExtractPermissionData extracts PermissionRequestData from an AEP envelope.
func ExtractPermissionData(env *events.Envelope) (*events.PermissionRequestData, error) {
	switch d := env.Event.Data.(type) {
	case events.PermissionRequestData:
		return &d, nil
	case map[string]any:
		id, _ := d["id"].(string)
		toolName, _ := d["tool_name"].(string)
		desc, _ := d["description"].(string)
		var args []string
		if a, ok := d["args"].([]any); ok {
			for _, v := range a {
				if s, ok := v.(string); ok {
					args = append(args, s)
				}
			}
		}
		return &events.PermissionRequestData{
			ID:          id,
			ToolName:    toolName,
			Description: desc,
			Args:        args,
		}, nil
	default:
		return nil, fmt.Errorf("unexpected permission data type: %T", env.Event.Data)
	}
}

// ExtractQuestionData extracts QuestionRequestData from an AEP envelope.
func ExtractQuestionData(env *events.Envelope) (*events.QuestionRequestData, error) {
	d, ok := events.DecodeAs[events.QuestionRequestData](env.Event.Data)
	if !ok {
		return nil, fmt.Errorf("unexpected question data type: %T", env.Event.Data)
	}
	return &d, nil
}

// ExtractElicitationData extracts ElicitationRequestData from an AEP envelope.
func ExtractElicitationData(env *events.Envelope) (*events.ElicitationRequestData, error) {
	d, ok := events.DecodeAs[events.ElicitationRequestData](env.Event.Data)
	if !ok {
		return nil, fmt.Errorf("unexpected elicitation data type: %T", env.Event.Data)
	}
	return &d, nil
}
