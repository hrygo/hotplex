package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/execution"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/security"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/internal/skills"
	"github.com/hrygo/hotplex/internal/worker"
	"github.com/hrygo/hotplex/internal/worker/base"
	"github.com/hrygo/hotplex/pkg/aep"
	"github.com/hrygo/hotplex/pkg/events"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// LevelTrace is one step below slog.LevelDebug, for high-volume protocol
// chatter (ping/pong) that should not appear even at debug level.
const (
	LevelTrace                 = slog.Level(-8)
	stopRuntimeFinalizeTimeout = 500 * time.Millisecond
)

// ─── Message Handler ─────────────────────────────────────────────────────────

// Handler processes incoming messages from a client connection.
// It coordinates between the hub, session manager, and pool.
type Handler struct {
	log             *slog.Logger
	hub             *Hub
	sm              SessionManager
	auth            *security.Authenticator
	bridge          *Bridge
	skillsLocator   SkillsLocator
	auditCollector  *audit.Collector
	executionStore  execution.Store
	repairer        *execution.Repairer
	ownerInstanceID string
	stopFence       turnStopFence
	dispatchGate    sessionDispatchGate
	// configProvider reads the LIVE configuration so a hot-reload of the input
	// queue is visible to the next enqueue instead of being frozen at startup.
	// Nil (tests, no config store) means the queue is disabled.
	configProvider func() *config.Config

	// catalogStore is the session-scoped merged command catalog (spec §5.2).
	// catalogGen tracks the per-session generation bumped on /reset, /cd, and
	// every Worker attach; catalogGenMu guards the map.
	catalogStore *sessionCatalogStore
	catalogGenMu sync.Mutex
	catalogGen   map[string]uint64

	homeDirOnce sync.Once
	homeDir     string
	homeDirErr  error
}

// SetConfigProvider late-injects the live config reader. The Bridge uses the
// same hook for runtime-plan resolution (#946 D2).
func (h *Handler) SetConfigProvider(fn func() *config.Config) { h.configProvider = fn }

// SkillsLocator discovers skills from the filesystem.
type SkillsLocator interface {
	List(ctx context.Context, homeDir, workDir string) ([]skills.Skill, error)
	Close()
}

// NewHandler creates a new message handler.
func NewHandler(deps HandlerDeps) *Handler {
	h := &Handler{
		log:             deps.Log.With("component", "handler"),
		hub:             deps.Hub,
		sm:              deps.SM,
		auth:            deps.Auth,
		bridge:          deps.Bridge,
		skillsLocator:   deps.SkillsLocator,
		executionStore:  deps.ExecutionStore,
		repairer:        deps.Repairer,
		ownerInstanceID: deps.OwnerInstanceID,
		catalogStore:    newSessionCatalogStore(deps.Log, deps.SkillsLocator),
		catalogGen:      make(map[string]uint64),
		// stopFence stays zero-valued: the turn stop fence is ready to use.
	}
	if deps.Bridge != nil {
		deps.Bridge.SetReplayValidator(h.ValidateNativeReplay)
	}
	return h
}

// InvalidateCatalog drops the session's cached command catalog and bumps its
// generation so the next catalog assembly is forced (spec §5.2, §8.7). Called
// by the Handler on /reset and /cd, and by the Bridge on every Worker attach;
// the worker-instance comparison in sessionCatalogStore.Lookup is the
// belt-and-suspenders guarantee (spec §8.7). Safe to call on a zero-value
// Handler (nil store is a no-op).
func (h *Handler) InvalidateCatalog(sessionID string) {
	if h.catalogStore == nil {
		return
	}
	h.catalogGenMu.Lock()
	h.catalogGen[sessionID]++
	h.catalogGenMu.Unlock()
	h.catalogStore.Invalidate(sessionID)
}

// catalogGeneration returns the current catalog generation for a session.
func (h *Handler) catalogGeneration(sessionID string) uint64 {
	h.catalogGenMu.Lock()
	defer h.catalogGenMu.Unlock()
	return h.catalogGen[sessionID]
}

// ReleaseSession drops the session's per-session catalog state (generation
// counter and cached merged catalog) when the session is released at runtime.
// Mirrors the hub's ReleaseSeq/ForgetSeq cleanup so long-lived gateways with
// session churn do not accumulate one entry per deleted session. Safe to call
// on a zero-value Handler (nil store is a no-op).
func (h *Handler) ReleaseSession(sessionID string) {
	if h.catalogStore == nil {
		return
	}
	h.catalogGenMu.Lock()
	delete(h.catalogGen, sessionID)
	h.catalogGenMu.Unlock()
	h.catalogStore.Invalidate(sessionID)
}

// SetAuditCollector injects the audit collector for message event recording.
func (h *Handler) SetAuditCollector(ac *audit.Collector) {
	h.auditCollector = ac
}

// emitAudit enqueues a non-blocking message.inbound audit event. No-op when collector is nil.
func (h *Handler) emitAudit(outcome, userID, platform, sessionID, content string) {
	if h.auditCollector == nil {
		return
	}
	if userID == "" {
		userID = audit.AnonymousUserID
	}
	detailJSON := "{}"
	if content != "" {
		detail := map[string]any{"content": content}
		if bytes, err := json.Marshal(detail); err == nil {
			detailJSON = string(bytes)
		}
	}
	_ = h.auditCollector.Enqueue(context.Background(), &audit.UserActivity{
		Ts:         time.Now().UnixMilli(),
		UserID:     userID,
		UserIDType: audit.UserIDTypePlatform,
		Platform:   platform,
		SessionID:  sessionID,
		Action:     audit.ActionMessageInbound,
		Outcome:    outcome,
		DetailJSON: detailJSON,
	})
}

// emitInteractionAudit enqueues a non-blocking interaction response audit event
// (permission.response / question.response / elicitation.response).
func (h *Handler) emitInteractionAudit(userID, platform, sessionID string, eventType events.Kind, data any, content string) {
	if h.auditCollector == nil {
		return
	}
	if userID == "" {
		userID = audit.AnonymousUserID
	}

	var action string
	var resourceType string
	var resourceID string
	var outcome string
	detailMap := make(map[string]any)

	switch eventType {
	case events.PermissionRequest:
		action = audit.ActionPermissionRequest
		resourceType = "permission"
		var reqID string
		var toolName string
		var description string
		var args []string

		if prd, ok := data.(events.PermissionRequestData); ok {
			reqID = prd.ID
			toolName = prd.ToolName
			description = prd.Description
			args = prd.Args
		} else if prdPtr, ok := data.(*events.PermissionRequestData); ok && prdPtr != nil {
			reqID = prdPtr.ID
			toolName = prdPtr.ToolName
			description = prdPtr.Description
			args = prdPtr.Args
		} else if m, ok := data.(map[string]any); ok {
			reqID, _ = m["id"].(string)
			if reqID == "" {
				reqID, _ = m["request_id"].(string)
			}
			toolName, _ = m["tool_name"].(string)
			if toolName == "" {
				toolName, _ = m["tool"].(string)
			}
			description, _ = m["description"].(string)
			if rawArgs, ok := m["args"].([]string); ok {
				args = rawArgs
			} else if rawArgsAny, ok := m["args"].([]any); ok {
				for _, a := range rawArgsAny {
					if s, ok := a.(string); ok {
						args = append(args, s)
					}
				}
			}
		}

		resourceID = reqID
		if reqID != "" {
			detailMap["id"] = reqID
		}
		if toolName != "" {
			detailMap["tool_name"] = toolName
		}
		if description != "" {
			detailMap["description"] = description
		}
		if len(args) > 0 {
			detailMap["args"] = args
		}
		outcome = audit.OutcomeSuccess

	case events.PermissionResponse:
		action = audit.ActionPermissionResponse
		resourceType = "permission"
		var allowed = true
		var reason string
		var reqID string

		if prd, ok := data.(events.PermissionResponseData); ok {
			reqID = prd.ID
			allowed = prd.Allowed
			reason = prd.Reason
		} else if m, ok := data.(map[string]any); ok {
			reqID, _ = m["id"].(string)
			if reqID == "" {
				reqID, _ = m["request_id"].(string)
			}
			if a, ok := m["allowed"].(bool); ok {
				allowed = a
			}
			reason, _ = m["reason"].(string)
		}

		resourceID = reqID
		if reqID != "" {
			detailMap["id"] = reqID
		}
		detailMap["allowed"] = allowed
		if reason != "" {
			detailMap["reason"] = reason
		}
		if content != "" {
			detailMap["content"] = content
		}
		if allowed {
			outcome = audit.OutcomeSuccess
		} else {
			outcome = audit.OutcomeDenied
		}

	case events.QuestionResponse:
		action = audit.ActionQuestionResponse
		resourceType = "question"
		var reqID string
		var answers any

		if qrd, ok := data.(events.QuestionResponseData); ok {
			reqID = qrd.ID
			answers = qrd.Answers
		} else if m, ok := data.(map[string]any); ok {
			reqID, _ = m["id"].(string)
			answers = m["answers"]
		}

		resourceID = reqID
		if reqID != "" {
			detailMap["id"] = reqID
		}
		if answers != nil {
			detailMap["answers"] = answers
		}
		if content != "" {
			detailMap["content"] = content
		}
		outcome = audit.OutcomeSuccess

	case events.ElicitationResponse:
		action = audit.ActionElicitationResponse
		resourceType = "elicitation"
		var reqID string
		var act string
		var elicitContent any

		if erd, ok := data.(events.ElicitationResponseData); ok {
			reqID = erd.ID
			act = erd.Action
			elicitContent = erd.Content
		} else if m, ok := data.(map[string]any); ok {
			reqID, _ = m["id"].(string)
			act, _ = m["action"].(string)
			elicitContent = m["content"]
		}

		resourceID = reqID
		if reqID != "" {
			detailMap["id"] = reqID
		}
		if act != "" {
			detailMap["action"] = act
		}
		if elicitContent != nil {
			detailMap["content"] = elicitContent
		} else if content != "" {
			detailMap["content"] = content
		}
		if act == "decline" || act == "cancel" {
			outcome = audit.OutcomeDenied
		} else {
			outcome = audit.OutcomeSuccess
		}

	default:
		return
	}

	detailJSON := "{}"
	if len(detailMap) > 0 {
		if bytes, err := json.Marshal(detailMap); err == nil {
			detailJSON = string(bytes)
		}
	}

	_ = h.auditCollector.Enqueue(context.Background(), &audit.UserActivity{
		Ts:           time.Now().UnixMilli(),
		UserID:       userID,
		UserIDType:   audit.UserIDTypePlatform,
		Platform:     platform,
		SessionID:    sessionID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Outcome:      outcome,
		DetailJSON:   detailJSON,
	})
}

// Handle processes an incoming envelope from a client.
func (h *Handler) Handle(ctx context.Context, env *events.Envelope) (err error) {
	defer func() {
		if r := recover(); r != nil {
			sid := ""
			if env != nil {
				sid = env.SessionID
			}
			h.log.Error("gateway: panic in handler", "session_id", sid, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	if env.Event.Type != events.Ping {
		dataSize, dataSHA256 := eventDataLogSummary(env.Event.Data)
		h.log.Info("gateway: Handle received event",
			"type", env.Event.Type,
			"session_id", env.SessionID,
			"seq", env.Seq,
			"data_size", dataSize,
			"data_sha256", dataSHA256,
		)
	}
	switch env.Event.Type {
	case events.Input:
		return h.handleInput(ctx, env)
	case events.Ping:
		return h.handlePing(ctx, env)
	case events.Control:
		return h.handleControl(ctx, env)
	case events.WorkerCmd:
		return h.handleWorkerCommand(ctx, env)
	case events.PermissionResponse, events.QuestionResponse, events.ElicitationResponse:
		return h.handleInteractionResponseEvent(ctx, env)
	// AEP-011 / AEP-012: pass-through events from worker to all session clients.
	case events.Reasoning, events.Step, events.PermissionRequest,
		events.QuestionRequest,
		events.ElicitationRequest,
		events.Message, events.MessageStart, events.MessageEnd:
		return h.passthroughToSession(ctx, env)
	default:
		return h.sendErrorf(ctx, env, events.ErrCodeProtocolViolation, "unknown event type: %s", env.Event.Type)
	}
}

// eventDataLogSummary returns an operationally useful, non-plaintext summary
// of event data. JSON encoding gives maps a stable key order; unsupported
// values use their type name, which is likewise stable and reveals no body.
func eventDataLogSummary(data any) (int, string) {
	encoded, err := json.Marshal(data)
	if err != nil {
		encoded = []byte(fmt.Sprintf("unsupported:%T", data))
	}
	sum := sha256.Sum256(encoded)
	return len(encoded), hex.EncodeToString(sum[:])[:16]
}

func (h *Handler) handleInput(ctx context.Context, env *events.Envelope) error {
	data, ok := env.Event.Data.(map[string]any)
	if !ok {
		h.log.Warn("gateway: handleInput malformed data", "session_id", env.SessionID)
		return h.sendErrorf(ctx, env, events.ErrCodeInvalidMessage, "malformed input data")
	}

	if handled, err := h.tryInteractionResponse(ctx, env, data); handled {
		return err
	}

	content, _ := data["content"].(string)
	if strings.TrimSpace(content) == "" {
		return nil
	}
	if handled, err := h.tryCommandDispatch(ctx, env, content); handled {
		// Control/help/worker commands bypass the durable execution path, so
		// no InputAck or Done is ever sent. The webchat client locks
		// pendingInput on every sendInput and only releases on a terminal
		// InputAck/Done/Error — without this synthetic "delivered" ack the UI
		// stays frozen for up to the 5-minute settle timeout after commands
		// like /reset that only emit a State event.
		if err == nil {
			h.ackControlCommand(ctx, env)
		}
		return err
	}
	// Explicit /worker <name> [args] entry (spec §5.3): runs after the
	// fixed-command branch so /reset-style safety commands always win, and
	// before Skill resolution so a filesystem Skill named "worker" cannot
	// shadow the reserved explicit entry.
	if handled, err := h.tryExplicitNativeCommand(ctx, env, content); handled {
		return err
	}
	if invocation, matched, err := h.resolveSkillForSession(ctx, env.SessionID, content); err != nil {
		if errors.Is(err, skills.ErrAmbiguousInvocation) {
			return h.sendErrorf(ctx, env, events.ErrCodeInvalidMessage, "ambiguous Skill invocation")
		}
		if errors.Is(err, worker.ErrSkillNotSupported) {
			return h.sendErrorf(ctx, env, events.ErrCodeNotSupported,
				"native Skill is discoverable but not callable by the current Worker; inspect /skills for callable entries")
		}
		return h.sendErrorf(ctx, env, events.ErrCodeInternalError, "skill resolution failed: %v", err)
	} else if matched {
		return h.deliverSkillToWorker(ctx, env, content, invocation)
	}

	return h.deliverToWorker(ctx, env, content)
}

// workerCommandInputRe matches the explicit "/worker <name> [args]" entry
// (spec §5.3). The "/worker" prefix is case-sensitive; the regexp enforces
// that a name (or a bare prefix) is present — "/workername" without a space is
// NOT a /worker input.
var workerCommandInputRe = regexp.MustCompile(`^/worker\s+(\S+)(?:\s+(.*))?$`)

// tryExplicitNativeCommand implements the reserved /worker <name> [args]
// entry (spec §5.3, §5.4). It runs after tryCommandDispatch so Gateway fixed
// commands always win, and before Skill resolution so the reserved prefix is
// never shadowed. A matched input is handled here and never falls through to
// the ordinary text path: unknown, unavailable, ambiguous, and stale-catalog
// names all resolve to NOT_SUPPORTED.
func (h *Handler) tryExplicitNativeCommand(ctx context.Context, env *events.Envelope, content string) (handled bool, err error) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "/worker") {
		return false, nil
	}
	// "/worker" is only a /worker input when the reserved prefix is followed
	// by whitespace (or ends the input). "/workername" falls through to the
	// Skill/ordinary path so a Skill literally named "workername" keeps its
	// short syntax.
	if trimmed != "/worker" && !strings.HasPrefix(trimmed, "/worker ") && !strings.HasPrefix(trimmed, "/worker\t") {
		return false, nil
	}
	startedAt := time.Now()

	matches := workerCommandInputRe.FindStringSubmatch(trimmed)
	if matches == nil {
		return true, h.sendErrorf(ctx, env, events.ErrCodeNotSupported,
			"native command requires a name: /worker <name> [args]")
	}
	name, args := matches[1], matches[2]

	status := "ok"
	errorClass := ""
	workerType := ""
	defer func() {
		h.log.Info("gateway: native command dispatched",
			"session_id", env.SessionID,
			"worker_type", workerType,
			"command", name,
			"status", status,
			"duration_ms", time.Since(startedAt).Milliseconds(),
			"error_class", errorClass,
		)
	}()

	fail := func(format string, args ...any) (bool, error) {
		status = "error"
		errorClass = string(events.ErrCodeNotSupported)
		return true, h.sendErrorf(ctx, env, events.ErrCodeNotSupported, format, args...)
	}

	if h.catalogStore == nil {
		return fail("native command catalog unavailable")
	}
	si, err := h.sm.Get(ctx, env.SessionID)
	if err != nil {
		return fail("native command catalog unavailable")
	}
	w := h.sm.GetWorker(env.SessionID)
	if w == nil {
		return fail("native command catalog unavailable")
	}
	workerType = string(w.Type())

	descriptors, lookupErr := h.catalogStore.Lookup(ctx, env.SessionID, si.WorkDir, w, h.catalogGeneration(env.SessionID))
	if lookupErr != nil {
		return fail("native command catalog unavailable: %v", lookupErr)
	}

	var descriptor *worker.NativeCommandDescriptor
	for i := range descriptors {
		if descriptors[i].Name == name {
			descriptor = &descriptors[i]
			break
		}
	}
	if descriptor == nil {
		ambiguous := 0
		for _, d := range descriptors {
			if strings.EqualFold(d.Name, name) {
				ambiguous++
			}
		}
		if ambiguous > 0 {
			return fail("native command %q is ambiguous in the worker catalog", name)
		}
		return fail("worker %s does not support native command %q", w.Type(), name)
	}
	if descriptor.Kind == worker.NativeCommandKindSkill {
		fsSkills, fsErr := h.listSessionSkills(ctx, si)
		if fsErr != nil {
			return fail("native command catalog unavailable: %v", fsErr)
		}
		fs, hasFS := findFilesystemSkill(fsSkills, descriptor.Name)
		if status := classifyNativeSkillCallability(*descriptor, fs, hasFS, w, true); status != events.SkillStatusCallable {
			return fail("worker %s does not advertise callable Skill %q", w.Type(), name)
		}
	}

	// Gateway fixed commands are gateway-handled (spec §5.2) — the merged
	// catalog's fixed tier always shadows same-named Worker commands, so an
	// explicit /worker <fixed-name> must never be dispatched through the
	// Worker's native invoker. Dispatching it would inject the slash text into
	// a running turn (claudecode), surface an internal error from the empty
	// fixed Path (codexcli), or race the single-prompt invariant (ACP) —
	// while bypassing the busy gate, cancelRetry and the execution record.
	if _, fixed := fixedCommandNamesFor(w)[name]; fixed {
		return fail("native command %q is a Gateway fixed command; use /%s instead", name, name)
	}

	invoker, ok := worker.AsNativeInvoker(w)
	if !ok {
		return fail("worker %s cannot invoke native command %q", w.Type(), name)
	}

	invocation := worker.NativeCommandInvocation{
		Name: name,
		Args: args,
		Path: descriptor.Path,
		Mode: descriptor.Mode,
	}

	if descriptor.StartsTurn {
		stashInvocation(env, invocation, content)
		dispatchErr := h.deliverToWorkerWithBusyHandling(ctx, env, content, &invocation, true)
		if dispatchErr != nil {
			status = "error"
			errorClass = string(classifyWorkerError(dispatchErr))
		}
		return true, dispatchErr
	}

	// StartsTurn=false: bounded control request through the native invoker,
	// settled with a synthetic ACK and no execution record (spec §5.4).
	if invokeErr := invoker.InvokeNativeCommand(ctx, invocation); invokeErr != nil {
		status = "error"
		errorClass = string(classifyWorkerError(invokeErr))
		h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, name)
		return true, h.sendErrorf(ctx, env, classifyWorkerError(invokeErr), "native command %q failed: %v", name, invokeErr)
	}
	h.ackControlCommand(ctx, env)
	return true, nil
}

func (h *Handler) resolveSkillForSession(ctx context.Context, sessionID, content string) (worker.NativeCommandInvocation, bool, error) {
	// Only slash-prefixed input can be a Skill invocation. Gating here keeps
	// the ordinary-message hot path free of session lookups and filesystem
	// catalog scans.
	if !strings.HasPrefix(strings.TrimSpace(content), "/") {
		return worker.NativeCommandInvocation{}, false, nil
	}
	si, err := h.sm.Get(ctx, sessionID)
	if err != nil {
		// Fall through to the normal delivery path, which re-checks the
		// session and reports the canonical session-not-found error. Skill
		// resolution must not mask a concurrent delete/GC with a generic
		// internal error here.
		return worker.NativeCommandInvocation{}, false, nil
	}
	if h.catalogStore == nil {
		return worker.NativeCommandInvocation{}, false, nil
	}
	w := h.sm.GetWorker(sessionID)
	if w == nil {
		return worker.NativeCommandInvocation{}, false, nil
	}
	descriptors, fsSkills, authoritativeOK, err := h.loadNativeSkillEvidence(ctx, sessionID, si, w)
	if err != nil {
		return worker.NativeCommandInvocation{}, false, err
	}
	invocation, matched, err := resolveNativeSkillInvocation(content, descriptors)
	if err != nil {
		return worker.NativeCommandInvocation{}, matched, err
	}
	if !matched {
		if nativeSkillSurfaceAvailable(h, w) {
			return worker.NativeCommandInvocation{}, true,
				nativeSkillNotSupportedError(nativeSkillNameFromInput(content))
		}
		return worker.NativeCommandInvocation{}, false, nil
	}
	fs, hasFS := findFilesystemSkill(fsSkills, invocation.Name)
	descriptor := findNativeSkillDescriptor(descriptors, invocation.Name)
	if descriptor == nil {
		return worker.NativeCommandInvocation{}, false, nil
	}
	if status := classifyNativeSkillCallability(*descriptor, fs, hasFS, w, authoritativeOK); status != events.SkillStatusCallable {
		return worker.NativeCommandInvocation{}, true, nativeSkillNotSupportedError(invocation.Name)
	}
	invocation.Path = descriptor.Path
	invocation.Mode = descriptor.Mode
	return invocation, true, nil
}

func nativeSkillSurfaceAvailable(h *Handler, w worker.Worker) bool {
	if h.skillsLocator != nil || (h.catalogStore != nil && h.catalogStore.skillsLocator != nil) {
		return true
	}
	_, ok := worker.AsNativeCatalogProvider(w)
	return ok
}

func nativeSkillNameFromInput(content string) string {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "/") {
		return ""
	}
	fields := strings.Fields(strings.TrimPrefix(trimmed, "/"))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// listSessionSkills returns the current filesystem discovery snapshot used for
// source metadata and evidence-origin classification. It is never used as
// invocation authority by itself.
func (h *Handler) listSessionSkills(ctx context.Context, si *session.SessionInfo) ([]skills.Skill, error) {
	if si == nil {
		return nil, nil
	}
	locator := h.skillsLocator
	if locator == nil && h.catalogStore != nil {
		locator = h.catalogStore.skillsLocator
	}
	if locator == nil {
		return nil, nil
	}
	homeDir, err := h.userHomeDir()
	if err != nil {
		return nil, err
	}
	return locator.List(ctx, homeDir, si.WorkDir)
}

func (h *Handler) loadNativeSkillEvidence(
	ctx context.Context,
	sessionID string,
	si *session.SessionInfo,
	w worker.Worker,
) ([]worker.NativeCommandDescriptor, []skills.Skill, bool, error) {
	if h.catalogStore == nil {
		return nil, nil, false, nil
	}
	descriptors, lookupErr := h.catalogStore.Lookup(ctx, sessionID, si.WorkDir, w, h.catalogGeneration(sessionID))
	fsSkills, err := h.listSessionSkills(ctx, si)
	if err != nil {
		return nil, nil, false, err
	}
	// Keep the resolver aligned with the filesystem snapshot even when a test
	// or a custom Handler constructed its catalog store without a locator. The
	// synthetic entries carry discovery evidence only; authoritativeOK still
	// controls whether the shared classifier can make them callable.
	seen := make(map[string]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		seen[descriptor.Name] = struct{}{}
	}
	mode := worker.NativeModeForType(w.Type())
	for _, skill := range fsSkills {
		if _, exists := seen[skill.Name]; exists {
			continue
		}
		descriptors = append(descriptors, worker.NativeCommandDescriptor{
			Name:        skill.Name,
			Description: skill.Description,
			Kind:        worker.NativeCommandKindSkill,
			Mode:        mode,
			StartsTurn:  true,
			AcceptsArgs: true,
			Path:        skill.FilePath,
		})
		seen[skill.Name] = struct{}{}
	}
	return descriptors, fsSkills, lookupErr == nil, nil
}

func findFilesystemSkill(fsSkills []skills.Skill, name string) (skills.Skill, bool) {
	for _, skill := range fsSkills {
		if skill.Name == name {
			return skill, true
		}
	}
	return skills.Skill{}, false
}

func findNativeSkillDescriptor(descriptors []worker.NativeCommandDescriptor, name string) *worker.NativeCommandDescriptor {
	for i := range descriptors {
		if descriptors[i].Kind == worker.NativeCommandKindSkill && descriptors[i].Name == name {
			return &descriptors[i]
		}
	}
	return nil
}

// userHomeDir caches os.UserHomeDir for the process lifetime: it is consulted
// on every slash-prefixed input and /skills listing, and the home directory
// does not change mid-process.
func (h *Handler) userHomeDir() (string, error) {
	h.homeDirOnce.Do(func() {
		h.homeDir, h.homeDirErr = os.UserHomeDir()
	})
	return h.homeDir, h.homeDirErr
}

func (h *Handler) cancelRetryIfNeeded(sessionID string) {
	if h.bridge != nil {
		h.bridge.CancelRetry(sessionID)
	}
}

// tryInteractionResponse routes permission/question/elicitation responses directly
// to the worker, bypassing command detection and state transitions.
func (h *Handler) tryInteractionResponse(ctx context.Context, env *events.Envelope, data map[string]any) (bool, error) {
	md, ok := data["metadata"].(map[string]any)
	if !ok {
		return false, nil
	}
	if md["permission_response"] == nil &&
		md["question_response"] == nil &&
		md["elicitation_response"] == nil {
		return false, nil
	}
	h.cancelRetryIfNeeded(env.SessionID)

	respType := "unknown"
	switch {
	case md["permission_response"] != nil:
		respType = "permission"
	case md["question_response"] != nil:
		respType = "question"
	case md["elicitation_response"] != nil:
		respType = "elicitation"
	}

	content, _ := data["content"].(string)
	w := h.sm.GetWorker(env.SessionID)
	if w != nil {
		h.log.Info("gateway: routing interaction response",
			"type", respType,
			"session_id", env.SessionID)
		if err := w.Input(ctx, content, md); err != nil {
			h.log.Warn("gateway: worker interaction response failed",
				"err", err,
				"type", respType,
				"session_id", env.SessionID)
			return true, fmt.Errorf("gateway: %s interaction response failed: %w", respType, err)
		} else if h.bridge != nil {
			h.bridge.CaptureInboundEvent(env.SessionID, env.Seq, events.Input, env.Event.Data)
			h.recordPermissionDenial(respType, md, env)
		}
		if h.auditCollector != nil {
			siPlatform := ""
			if h.sm != nil {
				if si, err := h.sm.Get(ctx, env.SessionID); err == nil {
					siPlatform = si.Platform
				}
			}
			switch respType {
			case "permission":
				h.emitInteractionAudit(env.OwnerID, siPlatform, env.SessionID, events.PermissionResponse, md["permission_response"], content)
			case "question":
				h.emitInteractionAudit(env.OwnerID, siPlatform, env.SessionID, events.QuestionResponse, md["question_response"], content)
			case "elicitation":
				h.emitInteractionAudit(env.OwnerID, siPlatform, env.SessionID, events.ElicitationResponse, md["elicitation_response"], content)
			}
		}
	} else {
		h.log.Warn("gateway: interaction response dropped — no worker",
			"type", respType,
			"session_id", env.SessionID)
		return true, fmt.Errorf("gateway: %s interaction response dropped: no worker for session %s", respType, env.SessionID)
	}
	return true, nil
}

// recordPermissionDenial registers a user's tool denial in the bridge dedup
// cache so a same-fingerprint retry within the window is auto-suppressed.
// Watchdog auto-denials (reason "interaction timed out") are excluded — a
// timeout is not a user decision, and the user deserves a fresh card on retry.
func (h *Handler) recordPermissionDenial(respType string, md map[string]any, env *events.Envelope) {
	if respType != "permission" {
		return
	}
	pr, ok := md["permission_response"].(map[string]any)
	if !ok {
		return
	}
	if allowed, _ := pr["allowed"].(bool); allowed {
		return
	}
	if reason, _ := pr["reason"].(string); reason == "interaction timed out" {
		return
	}
	h.bridge.RecordPermissionDeny(env.SessionID, env.ID, env.OwnerID)
}

// tryCommandDispatch detects help/control/worker commands and dispatches them.
// Returns (true, err) if a command was handled, (false, nil) to fall through.
func (h *Handler) tryCommandDispatch(ctx context.Context, env *events.Envelope, content string) (handled bool, err error) {
	if messaging.IsHelpCommand(content) {
		h.cancelRetryIfNeeded(env.SessionID)
		helpEnv := events.NewEnvelope(
			aep.NewID(), env.SessionID,
			0,
			events.Message, events.MessageData{Content: messaging.HelpText()},
		)
		return true, h.hub.SendToSession(ctx, helpEnv)
	}

	if result := messaging.ParseControlCommand(content); result != nil {
		h.cancelRetryIfNeeded(env.SessionID)
		data := events.ControlData{Action: result.Action}
		if result.Arg != "" {
			data.Details = map[string]any{"path": result.Arg}
		}
		ctrlEnv := &events.Envelope{
			Version:   events.Version,
			ID:        aep.NewID(),
			SessionID: env.SessionID,
			Seq:       0,
			Event: events.Event{
				Type: events.Control,
				Data: data,
			},
			OwnerID: env.OwnerID,
		}
		return true, h.handleControl(ctx, ctrlEnv)
	}

	if cmdResult := messaging.ParseWorkerCommand(content); cmdResult != nil {
		h.cancelRetryIfNeeded(env.SessionID)
		wcmdEnv := &events.Envelope{
			Version:   events.Version,
			ID:        aep.NewID(),
			SessionID: env.SessionID,
			Seq:       0,
			Event: events.Event{
				Type: events.WorkerCmd,
				Data: events.WorkerCommandData{
					Command: cmdResult.Command,
					Args:    cmdResult.Args,
					Extra:   cmdResult.Extra,
				},
			},
			OwnerID: env.OwnerID,
		}
		return true, h.handleWorkerCommand(ctx, wcmdEnv)
	}

	return false, nil
}

// handleSupplementOnBusy routes a SESSION_BUSY input to mid-turn passthrough
// (when the worker implements MidTurnInjector and the turn is still live) or
// the fallback pending buffer. Falls back to the legacy SESSION_BUSY error
// only when bridge buffering is unavailable.
//
// invocation carries the resolved native Skill invocation when the busy input
// is a Skill; nil for ordinary text. Skill inputs never take the mid-turn
// text-injection path — injecting the raw slash text would silently degrade
// the native invocation. They are delivered as a fresh turn when the gate is
// already released, or buffered otherwise; both paths preserve Skill semantics
// (the buffer path stashes the invocation for DeliverReplay).
//
// Decision order:
//  1. invocation != nil → deliver as a new turn if the gate is released,
//     otherwise fall through to the buffer.
//  2. Worker implements MidTurnInjector AND !IsStopped → InjectMidTurn.
//     Success → metric + capture + "injected" notify + return nil.
//     Failure → re-check the gate; deliver as a new turn if released, otherwise
//     fall through to the buffer.
//  3. bridge != nil → BufferPending + metric + "buffered" notify + return nil.
//  4. Otherwise → legacy sendErrorf SESSION_BUSY.
func (h *Handler) handleSupplementOnBusy(ctx context.Context, env *events.Envelope, content string, invocation *worker.NativeCommandInvocation) error {
	if h.bridge == nil {
		return h.sendErrorf(ctx, env, events.ErrCodeSessionBusy, "session has an active execution")
	}
	unlockSession, ok := h.bridge.lockSupplementSession(env.SessionID)
	if !ok {
		return h.sendErrorf(ctx, env, events.ErrCodeSessionTerminated, "gateway is shutting down")
	}
	sessionLocked := true
	defer func() {
		if sessionLocked {
			unlockSession()
		}
	}()

	clientID := clientMessageID(env)
	payloadHash, err := inputPayloadHash(env)
	if err != nil {
		return h.sendErrorf(ctx, env, events.ErrCodeInvalidMessage, "cannot hash supplement payload")
	}
	lease, disposition := h.bridge.BeginSupplement(env.SessionID, clientID, payloadHash)
	switch disposition {
	case supplementConflict:
		return h.sendErrorf(ctx, env, events.ErrCodeInvalidMessage,
			"client message id %q was already used with different input", clientID)
	case supplementCapacity:
		return h.sendErrorf(ctx, env, events.ErrCodeSessionBusy, "too many concurrent supplement attempts")
	case supplementInjected, supplementBuffered:
		h.ackSupplement(ctx, env, supplementReceiptForDisposition(disposition), true,
			h.activeExecutionID(ctx, env.SessionID))
		return nil
	case supplementQueued:
		// A retry of an already-queued input. The durable record is the answer;
		// re-answering with a synthetic supplement ID would tell the client
		// something weaker than what we actually know.
		if h.executionStore != nil {
			if record, err := h.queuedRecordFor(ctx, env.SessionID, clientID); err == nil && record != nil {
				h.ackQueuedInput(ctx, env, record, true)
				return nil
			}
		}
		return h.sendErrorf(ctx, env, events.ErrCodeInternalError, "queued input record unavailable")
	case supplementNormal:
		unlockSession()
		sessionLocked = false
		return h.deliverToWorkerWithBusyHandling(ctx, env, content, invocation, true)
	}
	committed := false
	defer func() {
		if !committed {
			lease.Abort()
		}
	}()
	deliverAsNewTurn := func() (bool, error) {
		err := h.deliverToWorkerWithBusyHandling(ctx, env, content, invocation, false)
		if errors.Is(err, execution.ErrSessionBusy) {
			return false, nil
		}
		if err == nil {
			lease.Commit(supplementNormal)
			committed = true
		}
		return true, err
	}
	if invocation != nil {
		// A resolved Skill must keep its native semantics: mid-turn text
		// injection would degrade it to an ordinary prompt. Deliver it as a
		// fresh turn when the gate was released between busy-detection and
		// now; otherwise fall through to the pending buffer, whose replay
		// re-dispatches through the Skill path (stashInvocation below).
		if handled, deliverErr := deliverAsNewTurn(); handled {
			return deliverErr
		}
	} else if w := h.sm.GetWorker(env.SessionID); w != nil {
		parentExecutionID := ""
		if inj, ok := w.(worker.MidTurnInjector); ok && !w.IsStopped() {
			// Re-check the active gate immediately before injecting. A race
			// window exists between the SESSION_BUSY detection (in
			// acceptInputExecutionWithRetry above) and this inject: if the
			// running turn's Done arrived in between and released the gate,
			// injecting now would write the supplement as the PRIMARY input of
			// a new unsolicited CC turn — CC headless stays alive reading stdin
			// after Done — producing a ghost turn with no execution record. If
			// the gate is already released, route to the normal delivery path so
			// the supplement becomes a proper new turn. Codex avoids this via
			// SteerTurn's expectedTurnID; Claude Code uses a worker-local active-turn
			// fence so InjectMidTurn rejects writes after Done even if the gate
			// changes between this check and the stdin write.
			if h.executionStore != nil {
				if active, aerr := h.executionStore.ActiveBySession(ctx, env.SessionID); errors.Is(aerr, execution.ErrNotFound) {
					if handled, deliverErr := deliverAsNewTurn(); handled {
						return deliverErr
					}
				} else if aerr == nil && active != nil {
					parentExecutionID = active.ExecutionID
				}
			}
			if err := inj.InjectMidTurn(ctx, content, nil); err == nil {
				observability.MidTurnInjected().Add(ctx, 1)
				if h.bridge != nil {
					h.bridge.CaptureInboundEvent(env.SessionID, env.Seq, events.Input, env.Event.Data)
				}
				lease.Commit(supplementInjected)
				committed = true
				h.ackSupplement(ctx, env, supplementInjectedReceipt, false, parentExecutionID)
				h.notifySupplement(ctx, env.SessionID, "injected")
				return nil
			} else {
				h.log.Warn("gateway: mid-turn inject failed, falling back to buffer",
					"session_id", env.SessionID, "err", err)
				if h.executionStore != nil {
					if _, aerr := h.executionStore.ActiveBySession(ctx, env.SessionID); errors.Is(aerr, execution.ErrNotFound) {
						if handled, deliverErr := deliverAsNewTurn(); handled {
							return deliverErr
						}
					}
				}
			}
		}
	}
	if invocation != nil {
		stashInvocation(env, *invocation, content)
	}
	// Durable queue first. A volatile buffer is a strictly weaker promise: it
	// is lost on restart and cannot be inspected by an operator. Taking it only
	// when the durable path is disabled or refuses keeps the existing fallback
	// while making the queue the default answer when it can answer.
	if h.queueEnabled() {
		record, queued, qerr := h.EnqueueBusyInput(ctx, env, content, invocation)
		switch {
		case qerr == nil && queued && record != nil:
			lease.Commit(supplementQueued)
			committed = true
			h.ackQueuedInput(ctx, env, record, false)
			h.notifySupplement(ctx, env.SessionID, "queued")
			return nil
		case qerr != nil && errors.Is(qerr, execution.ErrPayloadConflict):
			return h.sendErrorf(ctx, env, events.ErrCodeInvalidMessage,
				"client message id %q was already used with different input", clientID)
		}
		// Full, oversized or otherwise unusable: fall through to the volatile
		// buffer, which is still better than refusing the input outright.
	}
	if !h.bridge.BufferPending(env.SessionID, env, content) {
		return h.sendErrorf(ctx, env, events.ErrCodeSessionBusy, "supplement buffer is full")
	}
	lease.Commit(supplementBuffered)
	committed = true
	observability.SupplementBuffered().Add(ctx, 1)
	h.ackSupplement(ctx, env, supplementBufferedReceipt, false,
		h.activeExecutionID(ctx, env.SessionID))
	h.notifySupplement(ctx, env.SessionID, "buffered")
	return nil
}

type supplementReceipt struct {
	status     events.ExecutionStatus
	inputMode  events.InputMode
	durability events.InputDurability
}

var (
	supplementInjectedReceipt = supplementReceipt{
		status:     events.ExecutionStatusDelivered,
		inputMode:  events.InputModeInjected,
		durability: events.InputDurabilityVolatile,
	}
	supplementBufferedReceipt = supplementReceipt{
		status:     events.ExecutionStatusAccepted,
		inputMode:  events.InputModeBuffered,
		durability: events.InputDurabilityVolatile,
	}
)

func supplementReceiptForDisposition(disposition supplementDisposition) supplementReceipt {
	if disposition == supplementBuffered {
		return supplementBufferedReceipt
	}
	return supplementInjectedReceipt
}

func (h *Handler) activeExecutionID(ctx context.Context, sessionID string) string {
	if h.executionStore == nil {
		return ""
	}
	active, err := h.executionStore.ActiveBySession(ctx, sessionID)
	if err != nil || active == nil {
		return ""
	}
	return active.ExecutionID
}

// ackSupplement settles AEP clients for a busy input that was accepted outside
// the execution ledger. The stable synthetic execution ID mirrors command ACKs;
// client_message_id remains the authoritative correlation and dedup key.
func (h *Handler) ackSupplement(
	ctx context.Context,
	source *events.Envelope,
	receipt supplementReceipt,
	duplicate bool,
	parentExecutionID string,
) {
	if h.hub == nil {
		return
	}
	clientID := clientMessageID(source)
	ack := events.NewEnvelope(aep.NewID(), source.SessionID, 0, events.InputAck, events.InputAckData{
		ClientMessageID:   clientID,
		ExecutionID:       "supplement-" + clientID,
		Status:            receipt.status,
		Duplicate:         duplicate,
		InputMode:         receipt.inputMode,
		Durability:        receipt.durability,
		ParentExecutionID: parentExecutionID,
	})
	ack.Priority = events.PriorityControl
	ack.OwnerID = source.OwnerID
	ack.Metadata = map[string]any{"client_message_id": clientID}
	ack.Metadata["supplement_mode"] = string(receipt.inputMode)
	if err := h.hub.SendToSession(context.WithoutCancel(ctx), ack); err != nil {
		h.log.Warn("gateway: supplement ack delivery failed", "err", err,
			"session_id", source.SessionID, "client_message_id", clientID)
	}
}

// notifySupplement broadcasts a marker `message` envelope so platform conns
// (Slack/Feishu/Webchat) can render their own i18n "supplement accepted"
// text. Metadata["supplement_mode"] carries the mode ("injected"|"buffered");
// Content is empty — conns substitute their own localized text (Task 10).
// Best-effort: delivery failures are not surfaced to the user.
func (h *Handler) notifySupplement(ctx context.Context, sessionID, mode string) {
	env := events.NewEnvelope(aep.NewID(), sessionID, 0,
		events.Message, events.MessageData{Content: ""})
	env.Metadata = map[string]any{"supplement_mode": mode}
	_ = h.hub.SendToSession(ctx, env)
}

// DeliverReplay replays a buffered supplement as a fresh input turn.
// Implements bridge.PendingReplayer; the active gate is already released by
// the prior done, so deliverToWorker's accept path will succeed. The content
// is extracted from the envelope's Data map (preserved by cloneForReplay).
func (h *Handler) DeliverReplay(ctx context.Context, env *events.Envelope) error {
	data, _ := env.Event.Data.(map[string]any)
	content, _ := data["content"].(string)

	// The stash records the resolution made at buffer time against the merged
	// catalog — including explicit /worker <name> entries, which the filesystem
	// re-resolution below must never hijack (a Skill literally named "worker"
	// would compact-match). invocationFromMetadata is content-keyed, so the
	// stash only applies while the replayed content is unchanged and a merged
	// multi-entry replay never replays only its Skill half. The delivery path
	// still re-validates against the Worker's authoritative catalog and fails
	// loudly when the Skill is no longer supported.
	if stashed, ok := invocationFromMetadata(env.Metadata, content); ok {
		validated, err := h.revalidateStashedNativeInvocation(ctx, env.SessionID, stashed)
		if err != nil {
			if errors.Is(err, worker.ErrSkillNotSupported) {
				return h.sendErrorf(ctx, env, events.ErrCodeNotSupported,
					"native Skill replay is no longer callable by the current Worker")
			}
			return err
		}
		return h.deliverToWorkerWithBusyHandling(ctx, env, content, &validated, false)
	}

	invocation, matched, err := h.resolveSkillForSession(ctx, env.SessionID, content)
	if err != nil {
		if errors.Is(err, skills.ErrAmbiguousInvocation) {
			return h.sendErrorf(ctx, env, events.ErrCodeInvalidMessage, "ambiguous Skill invocation")
		}
		if errors.Is(err, worker.ErrSkillNotSupported) {
			return h.sendErrorf(ctx, env, events.ErrCodeNotSupported,
				"native Skill replay is discoverable but not callable by the current Worker")
		}
		return err
	}
	if matched {
		return h.deliverToWorkerWithBusyHandling(ctx, env, content, &invocation, false)
	}
	// The Bridge already holds the session replay read fence. Returning a raw
	// busy result lets replayPending requeue without recursively entering the
	// supplement handler and acquiring the same RWMutex behind a queued writer.
	return h.deliverToWorkerWithBusyHandling(ctx, env, content, nil, false)
}

// revalidateStashedNativeInvocation treats replay metadata as correlation
// only. The current session catalog supplies the descriptor path/mode and the
// shared classifier decides whether the Worker can still invoke the Skill.
func (h *Handler) revalidateStashedNativeInvocation(
	ctx context.Context,
	sessionID string,
	invocation worker.NativeCommandInvocation,
) (worker.NativeCommandInvocation, error) {
	if h.catalogStore == nil {
		return worker.NativeCommandInvocation{}, nativeSkillNotSupportedError(invocation.Name)
	}
	si, err := h.sm.Get(ctx, sessionID)
	if err != nil {
		return worker.NativeCommandInvocation{}, err
	}
	w := h.sm.GetWorker(sessionID)
	if w == nil {
		return worker.NativeCommandInvocation{}, nativeSkillNotSupportedError(invocation.Name)
	}
	descriptors, fsSkills, authoritativeOK, err := h.loadNativeSkillEvidence(ctx, sessionID, si, w)
	if err != nil {
		return worker.NativeCommandInvocation{}, err
	}
	return revalidatedNativeInvocation(invocation, descriptors, fsSkills, w, authoritativeOK)
}

// ValidateNativeReplay validates a Worker-produced structured replay against
// the current session catalog. It is injected into Bridge after construction;
// the replay's stored path and mode are correlation data only.
func (h *Handler) ValidateNativeReplay(
	ctx context.Context,
	sessionID string,
	w worker.Worker,
	replay worker.InputReplay,
) (worker.InputReplay, error) {
	if replay.Skill == nil {
		return replay, nil
	}
	if h.catalogStore == nil || h.sm == nil || w == nil {
		return worker.InputReplay{}, nativeSkillNotSupportedError(replay.Skill.Name)
	}
	si, err := h.sm.Get(ctx, sessionID)
	if err != nil {
		return worker.InputReplay{}, err
	}
	descriptors, fsSkills, authoritativeOK, err := h.loadNativeSkillEvidence(ctx, sessionID, si, w)
	if err != nil {
		return worker.InputReplay{}, err
	}
	validated, err := revalidatedNativeInvocation(*replay.Skill, descriptors, fsSkills, w, authoritativeOK)
	if err != nil {
		return worker.InputReplay{}, err
	}
	replay.Skill = &validated
	return replay, nil
}

// deliverToWorker validates session state, handles IDLE→RUNNING transition,
// and delivers user input to the worker process.
func (h *Handler) deliverToWorker(ctx context.Context, env *events.Envelope, content string) error {
	return h.deliverToWorkerWithBusyHandling(ctx, env, content, nil, true)
}

func (h *Handler) deliverSkillToWorker(ctx context.Context, env *events.Envelope, content string, invocation worker.NativeCommandInvocation) error {
	return h.deliverToWorkerWithBusyHandling(ctx, env, content, &invocation, true)
}

func (h *Handler) deliverToWorkerWithBusyHandling(ctx context.Context, env *events.Envelope, content string, invocation *worker.NativeCommandInvocation, handleBusy bool) error {
	unlockDispatch := h.dispatchGate.Lock(env.SessionID)
	dispatchLocked := true
	defer func() {
		if dispatchLocked {
			unlockDispatch()
		}
	}()

	inputReceivedAt := time.Now()
	si, err := h.sm.Get(ctx, env.SessionID)
	if err != nil {
		h.log.Warn("gateway: handleInput session not found", "session_id", env.SessionID, "err", err)
		h.emitAudit(audit.OutcomeFailure, env.OwnerID, "", env.SessionID, content)
		h.cancelRetryIfNeeded(env.SessionID)
		return h.sendErrorf(ctx, env, events.ErrCodeSessionNotFound, "session not found")
	}

	execRecord, duplicate, err := h.acceptInputExecutionWithRetry(ctx, env)
	if err != nil {
		if errors.Is(err, execution.ErrPayloadConflict) {
			observability.ExecutionConflict().Add(ctx, 1)
			return h.sendErrorf(ctx, env, events.ErrCodeInvalidMessage,
				"client message id %q was already used with different input", clientMessageID(env))
		}
		if errors.Is(err, execution.ErrSessionBusy) {
			observability.ExecutionSessionBusy().Add(ctx, 1)
			h.cancelRetryIfNeeded(env.SessionID)
			if !handleBusy {
				return execution.ErrSessionBusy
			}
			// Supplement handling can re-enter normal primary delivery after
			// re-checking the active execution. Release this non-reentrant gate
			// before that path to avoid self-deadlock.
			unlockDispatch()
			dispatchLocked = false
			return h.handleSupplementOnBusy(ctx, env, content, invocation)
		}
		h.log.Error("gateway: persist input acceptance failed", "err", err, "session_id", env.SessionID)
		// A genuine new input failed to durably register; cancel any in-flight
		// LLM retry so it cannot fire and answer the previous failed turn.
		h.cancelRetryIfNeeded(env.SessionID)
		return h.sendErrorf(ctx, env, events.ErrCodeInternalError, "input acceptance failed")
	}
	if duplicate {
		observability.ExecutionDuplicate().Add(ctx, 1)
		h.log.Info("gateway: duplicate input suppressed",
			"session_id", env.SessionID,
			"client_message_id", execRecord.ClientMessageID,
			observability.KeyExecutionID, execRecord.ExecutionID,
			"status", execRecord.Status)
		h.sendInputAck(ctx, env, execRecord, true)
		return nil
	}
	finalized := execRecord == nil
	if !finalized {
		observability.ExecutionAccept().Add(ctx, 1)
		if h.bridge != nil {
			h.bridge.beginTurnTTFT(env.SessionID, execRecord.ExecutionID, worker.WorkerType("unknown"), inputReceivedAt)
			h.bridge.markTurnDurablyAccepted(env.SessionID, execRecord.ExecutionID)
		}
	}
	defer func() {
		if finalized {
			return
		}
		if err := h.finishInputExecution(ctx, execRecord, execution.StatusUnknown, events.ErrCodeInternalError); err != nil {
			h.log.Error("gateway: persist abandoned input status failed", "err", err,
				"session_id", env.SessionID, observability.KeyExecutionID, execRecord.ExecutionID)
		}
	}()
	// The first acknowledgement means the input is durably recorded. A second
	// acknowledgement below reports the worker-delivery outcome.
	h.sendInputAck(ctx, env, execRecord, false)
	capturedInbound := false
	captureInbound := func() {
		if capturedInbound || h.bridge == nil {
			return
		}
		capturedInbound = true
		h.bridge.CaptureInbound(context.WithoutCancel(ctx), env.SessionID, env.Seq,
			clientMessageID(env), events.Input, env.Event.Data, si.Platform, si.OwnerID)
	}
	h.cancelRetryIfNeeded(env.SessionID)

	finishOutcome := func(status execution.Status, code events.ErrorCode) {
		if statusErr := h.finishInputExecution(ctx, execRecord, status, code); statusErr != nil {
			h.log.Error("gateway: persist input outcome failed", "err", statusErr,
				"session_id", env.SessionID, observability.KeyExecutionID, execRecord.ExecutionID, "status", status)
		}
		// The outcome is recorded on the in-memory record (and reflected in the
		// ack) regardless of durable-write success; gateway-restart recovery
		// reconciles the DB. finalized=true ensures the defer safety-net does
		// not overwrite the intended outcome with a generic unknown.
		finalized = true
		h.sendInputAck(ctx, env, execRecord, false)
		if h.bridge != nil && status == execution.StatusFailed {
			h.bridge.finishTurnTTFT(env.SessionID, string(status))
		}
	}
	// Fence recovery may have replaced the Worker and transitioned a TERMINATED
	// session back to RUNNING while accepting this input. Re-read state so the
	// fresh Worker is not immediately replaced by the normal resume path below.
	si, err = h.sm.Get(ctx, env.SessionID)
	if err != nil {
		h.emitAudit(audit.OutcomeFailure, env.OwnerID, "", env.SessionID, content)
		finishOutcome(execution.StatusFailed, events.ErrCodeSessionNotFound)
		return h.sendErrorf(ctx, env, events.ErrCodeSessionNotFound, "session not found after input acceptance")
	}

	if !si.State.IsActive() {
		// Auto-resume TERMINATED sessions so the user can continue on the
		// same WebSocket connection after clicking stop (which sends terminate).
		if si.State == events.StateTerminated && h.bridge != nil {
			h.log.Info("gateway: auto-resuming terminated session", "session_id", env.SessionID)
			resumeCtx, resumeCancel := context.WithTimeout(ctx, 30*time.Second)
			resumeErr := h.bridge.ResumeSession(resumeCtx, env.SessionID, si.WorkDir)
			resumeCancel()
			if resumeErr != nil {
				h.log.Warn("gateway: auto-resume failed", "session_id", env.SessionID, "err", resumeErr)
				h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
				finishOutcome(execution.StatusFailed, events.ErrCodeInternalError)
				return h.sendErrorf(ctx, env, events.ErrCodeInternalError, "session resume failed: %v", resumeErr)
			}
			prevPlatform := si.Platform
			si, err = h.sm.Get(ctx, env.SessionID)
			if err != nil {
				h.emitAudit(audit.OutcomeFailure, env.OwnerID, prevPlatform, env.SessionID, content)
				finishOutcome(execution.StatusFailed, events.ErrCodeSessionNotFound)
				return h.sendErrorf(ctx, env, events.ErrCodeSessionNotFound, "session not found after resume")
			}
			if !si.State.IsActive() {
				h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
				finishOutcome(execution.StatusFailed, events.ErrCodeSessionBusy)
				return h.sendErrorf(ctx, env, events.ErrCodeSessionBusy, "session not active after resume: %s", si.State)
			}
		} else {
			h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
			finishOutcome(execution.StatusFailed, events.ErrCodeSessionBusy)
			return h.sendErrorf(ctx, env, events.ErrCodeSessionBusy, "session not active: %s", si.State)
		}
	}

	var w worker.Worker
	workerRunID := ""
	workerRunBound := false
	if h.bridge != nil {
		var ok bool
		w, workerRunID, ok = h.bridge.CurrentWorkerBinding(env.SessionID)
		workerRunBound = ok
		if !ok && si.State.IsActive() && h.bridge.sm != nil {
			h.log.Info("gateway: auto-resuming active session lacking worker binding", "session_id", env.SessionID, "state", si.State)
			resumeCtx, resumeCancel := context.WithTimeout(ctx, 30*time.Second)
			resumeErr := h.bridge.ResumeSession(resumeCtx, env.SessionID, si.WorkDir)
			resumeCancel()
			if resumeErr != nil {
				h.log.Warn("gateway: auto-resume failed for active session lacking worker binding", "session_id", env.SessionID, "err", resumeErr)
				h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
				finishOutcome(execution.StatusFailed, events.ErrCodeInternalError)
				return h.sendErrorf(ctx, env, events.ErrCodeInternalError, "session resume failed: %v", resumeErr)
			}
			// Refresh session info after successful resume
			var getErr error
			si, getErr = h.sm.Get(ctx, env.SessionID)
			if getErr != nil {
				h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
				finishOutcome(execution.StatusFailed, events.ErrCodeSessionNotFound)
				return h.sendErrorf(ctx, env, events.ErrCodeSessionNotFound, "session not found after resume")
			}
			w, workerRunID, ok = h.bridge.CurrentWorkerBinding(env.SessionID)
			workerRunBound = ok
		}
		if !ok {
			if h.executionStore != nil {
				finishOutcome(execution.StatusFailed, events.ErrCodeInternalError)
				return h.sendErrorf(ctx, env, events.ErrCodeInternalError, "worker run identity unavailable")
			}
			w = h.sm.GetWorker(env.SessionID)
		}
	} else {
		w = h.sm.GetWorker(env.SessionID)
	}
	if w == nil {
		h.log.Warn("gateway: handleInput no worker found", "session_id", env.SessionID)
		h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
		finishOutcome(execution.StatusFailed, events.ErrCodeSessionNotFound)
		return h.sendErrorf(ctx, env, events.ErrCodeSessionNotFound, "no worker attached to session")
	}
	if h.bridge != nil && execRecord != nil {
		h.bridge.setTurnTTFTWorkerType(env.SessionID, execRecord.ExecutionID, w.Type())
	}

	if si.State == events.StateIdle {
		if err := h.sm.TransitionWithInput(ctx, env.SessionID, events.StateRunning, content, nil); err != nil {
			h.log.Warn("gateway: handleInput transition failed", "session_id", env.SessionID, "err", err)
			h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
			finishOutcome(execution.StatusFailed, events.ErrCodeSessionBusy)
			return h.sendErrorf(ctx, env, events.ErrCodeSessionBusy, "session busy: %v", err)
		}
	}

	if execRecord != nil && h.executionStore != nil {
		persistedRunID := execRecord.WorkerRunID
		if workerRunID == "" {
			workerRunID = persistedRunID
		}
		if mrErr := h.executionStore.MarkRunning(ctx, execRecord.ExecutionID, h.ownerInstanceID, workerRunID); mrErr != nil {
			h.log.Warn("gateway: mark execution running failed", "err", mrErr,
				"session_id", env.SessionID, observability.KeyExecutionID, execRecord.ExecutionID)
			h.finishRuntimeWithoutDispatch(ctx, execRecord, persistedRunID, events.ErrCodeInternalError)
			finishOutcome(execution.StatusFailed, events.ErrCodeInternalError)
			return h.sendErrorf(ctx, env, events.ErrCodeInternalError, "execution dispatch registration failed")
		}
		execRecord.WorkerRunID = workerRunID
	}

	if h.log.Enabled(ctx, slog.LevelDebug) {
		runes := []rune(content)
		preview := string(runes)
		if len(runes) > 32 {
			preview = string(runes[:32]) + "..."
		}
		h.log.Debug("gateway: delivering input to worker", "session_id", env.SessionID, "content_len", len(content), "preview", preview)
	}
	if h.bridge != nil && workerRunBound {
		if err := h.bridge.beginWorkerRunTurn(ctx, env.SessionID, workerRunID); err != nil {
			h.finishRuntimeWithoutDispatch(ctx, execRecord, workerRunID, events.ErrCodeInternalError)
			finishOutcome(execution.StatusFailed, events.ErrCodeInternalError)
			return h.sendErrorf(ctx, env, events.ErrCodeInternalError, "worker turn admission failed")
		}
	}

	// Stamp the turn start immediately before delivery so the Done-bound timer
	// measures only this turn's processing, excluding inter-turn idle
	// (Turn-Integrity spec RC-4 / Fix D).
	if h.bridge != nil {
		h.bridge.RecordTurnStart(env.SessionID)
	}

	// A new primary turn begins: clear the previous turn's stop claim so this
	// turn can be stopped again (per-turn single-stop contract, C04/C05). The
	// fence is same-session/same-run/same-execution scoped; a new turn carries
	// a NEW execution ID, so this clear only matches when the execution ledger
	// is disabled (empty execID) — the exec-scoped key keeps the in-flight
	// stop's claim intact when the input path races the stop path. A replaced
	// worker run or execution is cleared by its own Claim overwriting the
	// stale entry. Metadata responses and mid-turn injections do NOT reach
	// this point and keep the claim.
	execID := ""
	if execRecord != nil {
		execID = execRecord.ExecutionID
	}
	h.stopFence.BeginTurn(env.SessionID, workerRunID, execID)

	var (
		dispatchAccepted bool
		inputErr         error
	)
	releaseDispatch := func() {
		if dispatchLocked {
			unlockDispatch()
			dispatchLocked = false
		}
	}
	var acceptanceErr error
	finalizeAcceptance := func() {
		captureInbound()
		if h.bridge != nil && execRecord != nil {
			h.bridge.markTurnWorkerAccepted(env.SessionID, execRecord.ExecutionID)
		}
		if err := h.finishInputExecution(ctx, execRecord, execution.StatusDelivered, ""); err != nil {
			h.log.Error("gateway: persist delivered input status failed", "err", err,
				"session_id", env.SessionID, observability.KeyExecutionID, execRecord.ExecutionID)
			// The worker accepted the input but the durable status write failed —
			// treat the outcome as ambiguous for the client.
			execRecord.Status = execution.StatusUnknown
			execRecord.ErrorCode = string(events.ErrCodeInternalError)
			finalized = true
			h.sendInputAck(ctx, env, execRecord, false)
			acceptanceErr = h.sendErrorf(ctx, env, events.ErrCodeInternalError, "input delivery status is unknown")
			return
		}
		finalized = true
		h.sendInputAck(ctx, env, execRecord, false)
		h.log.Debug("gateway: input delivered to worker", "session_id", env.SessionID)
		h.emitAudit(audit.OutcomeSuccess, env.OwnerID, si.Platform, env.SessionID, content)
	}
	if invocation != nil {
		resolvedInvocation := *invocation
		if resolvedInvocation.Mode == "" {
			resolvedInvocation.Mode = worker.NativeModeForType(w.Type())
		}
		invocation = &resolvedInvocation
		if provider, ok := worker.AsNativeCatalogProvider(w); ok {
			// Bound the authoritative re-validation with the same "cannot
			// confirm" timeout as catalog assembly (spec §8.2, §8.5): the
			// turn is already accepted and marked running, and a hung
			// catalog endpoint must not stall it beyond the bound.
			queryTimeout := nativeCatalogQueryTimeout
			if h.catalogStore != nil {
				queryTimeout = h.catalogStore.queryTimeout
			}
			catalogCtx, cancel := context.WithTimeout(ctx, queryTimeout)
			descriptors, catalogErr := provider.ListNativeCommands(catalogCtx, si.WorkDir)
			cancel()
			if catalogErr != nil {
				if h.bridge != nil {
					h.bridge.ClearTurnStart(env.SessionID)
				}
				finishOutcome(execution.StatusFailed, events.ErrCodeInternalError)
				h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
				return h.sendErrorf(ctx, env, events.ErrCodeInternalError,
					"worker %s Skill catalog failed: %v", w.Type(), catalogErr)
			}
			advertised := false
			for _, descriptor := range descriptors {
				if descriptor.Name == invocation.Name {
					advertised = true
					// The Worker's catalog is authoritative for resolution.
					// Prefer its path over the HotPlex filesystem scan, which
					// may diverge (symlink resolution, /private/var aliases,
					// different scan roots) and leave the native Skill item
					// pointing at a path the Worker cannot resolve.
					if descriptor.Path != "" {
						resolvedInvocation.Path = descriptor.Path
					}
					if descriptor.Mode != "" {
						resolvedInvocation.Mode = descriptor.Mode
					}
					break
				}
			}
			if !advertised {
				if h.bridge != nil {
					h.bridge.ClearTurnStart(env.SessionID)
				}
				finishOutcome(execution.StatusFailed, events.ErrCodeNotSupported)
				h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
				return h.sendErrorf(ctx, env, events.ErrCodeNotSupported,
					"worker %s does not advertise Skill %q", w.Type(), invocation.Name)
			}
		}
		invoker, ok := worker.AsNativeInvoker(w)
		if !ok {
			if h.bridge != nil {
				h.bridge.ClearTurnStart(env.SessionID)
			}
			finishOutcome(execution.StatusFailed, events.ErrCodeNotSupported)
			h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
			return h.sendErrorf(ctx, env, events.ErrCodeNotSupported,
				"worker %s does not support native Skill invocation", w.Type())
		}
		dispatchAccepted, inputErr = runAcceptedDispatch(func(accepted func()) error {
			return worker.DispatchNativeCommand(ctx, w, invoker, *invocation, accepted)
		}, finalizeAcceptance, releaseDispatch)
	} else {
		dispatchAccepted, inputErr = runAcceptedDispatch(func(accepted func()) error {
			return worker.DispatchInput(ctx, w, content, nil, accepted)
		}, finalizeAcceptance, releaseDispatch)
	}
	if acceptanceErr != nil {
		return acceptanceErr
	}
	if inputErr != nil && dispatchAccepted {
		// A stop may make a blocking adapter's already-accepted RPC return a
		// cancellation error. Re-enter the session gate so an in-flight stop
		// reaches a stable success/failure decision before classifying it. A
		// successful stop keeps the old Worker's marker set; a failed stop clears
		// it and the real input error remains visible.
		unlockStopDecision := h.dispatchGate.Lock(env.SessionID)
		stopped := w.IsStopped()
		unlockStopDecision()
		if stopped {
			h.log.Debug("gateway: accepted input ended by successful stop", "session_id", env.SessionID)
			return nil
		}
		var we *worker.WorkerError
		if errors.As(inputErr, &we) && we.Kind == worker.ErrKindTimeout {
			h.log.Info("gateway: accepted worker input timed out (worker still processing)", "session_id", env.SessionID)
			return nil
		}
		h.log.Warn("gateway: accepted worker input failed", "err", inputErr, "session_id", env.SessionID)
		code := classifyWorkerError(inputErr)
		if code == events.ErrCodeSessionTerminated && h.bridge != nil {
			h.bridge.cleanupCrashedWorker(env.SessionID, w)
		}
		// Provider acceptance already committed and audited the delivery as
		// successful. Surface the later turn error without emitting a conflicting
		// second audit outcome for the same input action.
		return h.sendErrorf(ctx, env, code, "worker input failed: %v", inputErr)
	}
	if inputErr != nil {
		var we *worker.WorkerError
		if errors.As(inputErr, &we) && we.Kind == worker.ErrKindTimeout {
			h.log.Info("gateway: worker input delivery timed out (worker still processing)", "session_id", env.SessionID)
			// The worker may still complete this input after the request timeout;
			// retain the user turn so late assistant output remains pairable.
			captureInbound()
			finishOutcome(execution.StatusUnknown, events.ErrCodeExecutionTimeout)
			return nil
		}
		h.log.Warn("gateway: worker input", "err", inputErr, "session_id", env.SessionID)
		// Input never reached the worker: clear the turn start so a later Done
		// (or crash-cleanup) cannot bill idle time to a turn that never ran.
		if h.bridge != nil {
			h.bridge.ClearTurnStart(env.SessionID)
		}
		code := classifyWorkerError(inputErr)
		finishOutcome(execution.StatusFailed, code)
		// ErrKindUnavailable (e.g. ACP session lost) means the worker's
		// internal session is dead but the process may still be alive.
		// Send SESSION_TERMINATED so the client can reconnect, and trigger
		// crash cleanup so forwardEvents exits and the worker is replaced.
		if code == events.ErrCodeSessionTerminated && h.bridge != nil {
			h.bridge.cleanupCrashedWorker(env.SessionID, w)
		}
		h.emitAudit(audit.OutcomeFailure, env.OwnerID, si.Platform, env.SessionID, content)
		return h.sendErrorf(ctx, env, code, "worker input failed: %v", inputErr)
	}
	return nil
}

func (h *Handler) acceptInputExecution(ctx context.Context, env *events.Envelope) (*execution.Record, bool, error) {
	if h.executionStore == nil {
		return nil, false, nil
	}
	// A queued input has already been durably accepted by the queue claim,
	// which promoted it to pending with an owner lease. Accepting it again
	// would recompute a payload hash from a reconstructed envelope that cannot
	// reproduce the stored one, and report a conflict against the item's own
	// row. The claim IS the acceptance.
	if record := preacceptedFrom(ctx); record != nil {
		return record, false, nil
	}
	payloadHash, err := inputPayloadHash(env)
	if err != nil {
		return nil, false, err
	}
	workerRunID := ""
	if h.bridge != nil {
		workerRunID, _ = h.bridge.CurrentWorkerRunID(env.SessionID)
	}
	if workerRunID == "" {
		// Duplicate lookup must remain possible even when the session currently has
		// no Worker. A newly accepted record is rebound to the actual attach run by
		// MarkRunning after resume/fresh-start readiness.
		workerRunID = "run_" + uuid.NewString()
	}
	record, duplicate, err := h.executionStore.Accept(ctx, execution.AcceptRequest{
		SessionID:       env.SessionID,
		ClientMessageID: clientMessageID(env),
		PayloadHash:     payloadHash,
		OwnerInstanceID: h.ownerInstanceID,
		WorkerRunID:     workerRunID,
	})
	if err != nil {
		return nil, duplicate, err
	}
	return record, duplicate, nil
}

func inputPayloadHash(env *events.Envelope) (string, error) {
	// Hash only the user payload (content + metadata). Transport metadata such
	// as platform_msg_id is also the idempotency key, so including it would
	// couple the lookup key to the hashed payload — strip it before hashing.
	hashData := env.Event.Data
	if data, ok := hashData.(map[string]any); ok {
		if _, has := data["platform_msg_id"]; has {
			cleaned := make(map[string]any, len(data))
			for k, v := range data {
				if k == "platform_msg_id" {
					continue
				}
				cleaned[k] = v
			}
			hashData = cleaned
		}
	}
	payload, err := json.Marshal(hashData)
	if err != nil {
		return "", fmt.Errorf("marshal input for idempotency: %w", err)
	}
	return sha256Hex(string(payload)), nil
}

// acceptInputExecutionWithRetry accepts the input, and if the session is fenced
// (previous runtime outcome unknown, which blocks Accept via the active gate's
// partial unique index), clears the fence and retries once. Without this a
// fenced session stays permanently blocked. Worker health is left to the
// existing session/crash-recovery machinery, so clearing the fence only
// re-opens Accept; the old record stays runtime_status=unknown in history.
func (h *Handler) acceptInputExecutionWithRetry(ctx context.Context, env *events.Envelope) (*execution.Record, bool, error) {
	rec, duplicate, err := h.acceptInputExecution(ctx, env)
	if err == nil || !errors.Is(err, execution.ErrSessionBusy) || h.executionStore == nil {
		return rec, duplicate, err
	}
	fenced, ferr := h.executionStore.FenceBySession(ctx, env.SessionID)
	if ferr != nil || fenced == nil {
		return rec, duplicate, err
	}
	if h.bridge == nil {
		return rec, duplicate, err
	}
	freshRunID, startErr := h.bridge.StartFreshWorker(ctx, env.SessionID)
	if startErr != nil {
		h.log.Warn("gateway: fresh worker start for fenced execution failed",
			"err", startErr, "session_id", env.SessionID, observability.KeyExecutionID, fenced.ExecutionID)
		return rec, duplicate, err
	}
	if cerr := h.executionStore.ClearFenceAfterFreshStart(ctx, fenced.ExecutionID, fenced.FenceReason, freshRunID); cerr != nil {
		h.log.Warn("gateway: clear stale fence failed",
			"err", cerr, "session_id", env.SessionID, observability.KeyExecutionID, fenced.ExecutionID)
		return rec, duplicate, err
	}
	h.log.Info("gateway: cleared stale fence, retrying accept",
		"session_id", env.SessionID, observability.KeyExecutionID, fenced.ExecutionID,
		"fence_reason", fenced.FenceReason)
	return h.acceptInputExecution(ctx, env)
}

func clientMessageID(env *events.Envelope) string {
	if data, ok := env.Event.Data.(map[string]any); ok {
		if id, _ := data["client_message_id"].(string); id != "" {
			return id
		}
		if id, _ := data["platform_msg_id"].(string); id != "" {
			return id
		}
	}
	if env.ID != "" {
		return env.ID
	}
	return aep.NewID()
}

// ackControlCommand sends a synthetic "delivered" InputAck for an input that
// was intercepted as a control/help/worker command instead of being routed to
// the durable execution pipeline. The webchat client arms a pending-input lock
// on every sendInput that only clears on a terminal InputAck/Done/Error; without
// this ack the UI locks for the full settle timeout (5 min) after commands like
// /reset that emit only a State event.
func (h *Handler) ackControlCommand(ctx context.Context, env *events.Envelope) {
	if h.hub == nil {
		return
	}
	ack := events.NewEnvelope(aep.NewID(), env.SessionID, 0, events.InputAck, events.InputAckData{
		ClientMessageID: clientMessageID(env),
		ExecutionID:     "cmd-" + env.ID,
		Status:          events.ExecutionStatusDelivered,
	})
	ack.Priority = events.PriorityControl
	ack.OwnerID = env.OwnerID
	if err := h.hub.SendToSession(context.WithoutCancel(ctx), ack); err != nil {
		h.log.Warn("gateway: control command ack failed", "err", err, "session_id", env.SessionID)
	}
}

func (h *Handler) finishInputExecution(ctx context.Context, record *execution.Record, status execution.Status, code events.ErrorCode) error {
	if h.executionStore == nil || record == nil {
		return nil
	}
	// Reflect the intended terminal status on the in-memory record before the
	// durable write, so every subsequent input.ack carries the outcome the
	// client should act on even when SetDelivery fails. The defer safety-net and
	// gateway-restart recovery reconcile the durable record afterwards.
	record.Status = status
	record.ErrorCode = string(code)
	if err := h.executionStore.SetDelivery(context.WithoutCancel(ctx), record.ExecutionID, h.ownerInstanceID, status, string(code)); err != nil {
		if h.repairer != nil {
			h.repairer.Enqueue(execution.RepairIntent{
				ExecutionID: record.ExecutionID,
				OwnerID:     h.ownerInstanceID,
				Kind:        execution.RepairDelivery,
				Status:      string(status),
				ErrorCode:   string(code),
			})
		}
		return err
	}
	observability.ExecutionDeliveryOutcome().Add(ctx, 1,
		metric.WithAttributes(attribute.String("delivery_status", string(status))))
	if record.CreatedAt > 0 {
		observability.ExecutionDeliveryLatency().Record(ctx, time.Since(time.UnixMilli(record.CreatedAt)).Seconds())
	}
	return nil
}

func (h *Handler) finishRuntimeWithoutDispatch(ctx context.Context, record *execution.Record, workerRunID string, code events.ErrorCode) {
	if h.executionStore == nil || record == nil || workerRunID == "" {
		return
	}
	err := h.executionStore.FinishRuntime(
		context.WithoutCancel(ctx), record.ExecutionID, workerRunID, execution.RuntimeFailed, string(code),
	)
	if err == nil || h.repairer == nil {
		return
	}
	h.repairer.Enqueue(execution.RepairIntent{
		ExecutionID: record.ExecutionID,
		WorkerRunID: workerRunID,
		Kind:        execution.RepairRuntime,
		Status:      string(execution.RuntimeFailed),
		ErrorCode:   string(code),
	})
}

func (h *Handler) finishRuntimeOnStop(ctx context.Context, sessionID, workerRunID, ownerID string) {
	if h.executionStore == nil {
		return
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), stopRuntimeFinalizeTimeout)
	defer finishCancel()
	rec, err := h.executionStore.OpenBySession(finishCtx, sessionID)
	if err != nil {
		return
	}

	rtStatus := execution.RuntimeFailed
	eventKind := events.RuntimeExecutionFailed
	errorCode := string(events.ErrCodeSessionTerminated)

	err = h.executionStore.FinishRuntime(
		finishCtx, rec.ExecutionID, workerRunID, rtStatus, errorCode,
	)
	if err != nil {
		h.log.Warn("gateway: finish runtime on stop failed, enqueuing repair", "err", err, "session_id", sessionID, observability.KeyExecutionID, rec.ExecutionID)
		if h.repairer != nil {
			h.repairer.Enqueue(execution.RepairIntent{
				ExecutionID: rec.ExecutionID,
				WorkerRunID: workerRunID,
				Kind:        execution.RepairRuntime,
				Status:      string(rtStatus),
				ErrorCode:   errorCode,
			})
		}
	}

	rtEnv := events.NewEnvelope(aep.NewID(), sessionID, 0, eventKind, events.RuntimeExecutionData{
		ExecutionID: rec.ExecutionID,
		Status:      string(rtStatus),
		ErrorCode:   events.ErrorCode(errorCode),
	})
	rtEnv.OwnerID = ownerID
	_ = h.hub.SendToSession(finishCtx, rtEnv)
}

func (h *Handler) sendInputAck(ctx context.Context, source *events.Envelope, record *execution.Record, duplicate bool) {
	if h.hub == nil || record == nil {
		return
	}
	ack := events.NewEnvelope(aep.NewID(), source.SessionID, 0, events.InputAck, events.InputAckData{
		ClientMessageID: record.ClientMessageID,
		ExecutionID:     record.ExecutionID,
		Status:          events.ExecutionStatus(record.Status),
		Duplicate:       duplicate,
		ErrorCode:       events.ErrorCode(record.ErrorCode),
		InputMode:       events.InputModePrimary,
		Durability:      events.InputDurabilityDurable,
	})
	ack.Priority = events.PriorityControl
	ack.OwnerID = source.OwnerID
	ack.Metadata = map[string]any{
		observability.KeyExecutionID: record.ExecutionID,
		"client_message_id":          record.ClientMessageID,
	}
	if err := h.hub.SendToSession(context.WithoutCancel(ctx), ack); err != nil {
		h.log.Warn("gateway: input ack delivery failed", "err", err,
			"session_id", source.SessionID, observability.KeyExecutionID, record.ExecutionID)
	}
}

func (h *Handler) handlePing(ctx context.Context, env *events.Envelope) error {
	// Include current session state in pong (per AEP spec §11.4).
	si, err := h.sm.Get(ctx, env.SessionID)
	state := "unknown"
	if err == nil {
		state = string(si.State)
	}

	reply := events.NewEnvelope(
		aep.NewID(),
		env.SessionID,
		0, // P2: pong should not consume seq
		events.Pong,
		map[string]any{"state": state},
	)
	if h.log.Enabled(ctx, LevelTrace) {
		h.log.Log(ctx, LevelTrace, "gateway: ping received, sending pong", "session_id", env.SessionID, "state", state)
	}
	err = h.hub.SendToSession(ctx, reply)
	if err != nil {
		h.log.Warn("gateway: pong send failed", "session_id", env.SessionID, "err", err)
	}
	return err
}

var passthroughMetricLabel = map[events.Kind]string{
	events.Reasoning:           "reasoning",
	events.Step:                "step",
	events.PermissionRequest:   "permission_request",
	events.PermissionResponse:  "permission_response",
	events.QuestionRequest:     "question_request",
	events.QuestionResponse:    "question_response",
	events.ElicitationRequest:  "elicitation_request",
	events.ElicitationResponse: "elicitation_response",
	events.Message:             "message",
	events.MessageStart:        "message.start",
	events.MessageEnd:          "message.end",
}

func (h *Handler) passthroughToSession(ctx context.Context, env *events.Envelope) error {
	if label, ok := passthroughMetricLabel[env.Event.Type]; ok {
		observability.GatewayEvents().Add(ctx, 1, metric.WithAttributes(attribute.String("event_type", label), attribute.String("direction", "s2c")))
	}
	return h.hub.SendToSession(ctx, env)
}

// validateOwner checks ownership and returns the session in one call.
// This avoids the double-fetch that calling ValidateOwnership then Get separately incurses.
func (h *Handler) validateOwner(ctx context.Context, env *events.Envelope) (*session.SessionInfo, error) {
	si, err := h.sm.Get(ctx, env.SessionID)
	if err != nil {
		return nil, err
	}
	if si.UserID != env.OwnerID {
		return nil, fmt.Errorf("%w: owner mismatch", session.ErrOwnershipMismatch)
	}
	return si, nil
}

// requireActiveOwner validates session ownership and returns the session info.
// On error it sends an appropriate error to the client and returns the error
// so the caller can simply do: si, err := h.requireActiveOwner(ctx, env); if err != nil { return err }
func (h *Handler) requireActiveOwner(ctx context.Context, env *events.Envelope) (*session.SessionInfo, error) {
	si, err := h.validateOwner(ctx, env)
	if err != nil {
		if errors.Is(err, session.ErrSessionCleanupPending) {
			return nil, h.sendErrorf(ctx, env, events.ErrCodeSessionBusy, "session cleanup in progress; retry later")
		}
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil, h.sendErrorf(ctx, env, events.ErrCodeSessionNotFound, "session not found")
		}
		return nil, h.sendErrorf(ctx, env, events.ErrCodeUnauthorized, "ownership required")
	}
	return si, nil
}

// ─── Bridge ─────────────────────────────────────────────────────────────────

// SessionReader provides read-only session access.
type SessionReader interface {
	Get(ctx context.Context, id string) (*session.SessionInfo, error)
	GetWorker(id string) worker.Worker
}

// SessionLifecycle provides session creation and deletion.
type SessionLifecycle interface {
	CreateWithBot(ctx context.Context, id, userID, botID, botName string, wt worker.WorkerType, allowedTools []string, platform string, platformKey map[string]string, workspaceID, workDir, title, clientKey string) (*session.SessionInfo, error)
	Delete(ctx context.Context, id string) error
	DeletePhysical(ctx context.Context, id string) error
}

// SessionTransitioner provides state transition operations.
type SessionTransitioner interface {
	Transition(ctx context.Context, id string, to events.SessionState) error
	TransitionWithInput(ctx context.Context, id string, to events.SessionState, content string, metadata map[string]any) error
	TransitionWithReason(ctx context.Context, id string, to events.SessionState, termReason string) error
}

// SessionWorkerManager provides worker attachment and detachment.
type SessionWorkerManager interface {
	AttachWorker(ctx context.Context, id string, w worker.Worker) error
	DetachWorker(id string)
	DetachWorkerIf(id string, expected worker.Worker) bool
	UpdateWorkerSessionID(ctx context.Context, id, workerSessionID string) error
	EnsureWorkerSessionID(ctx context.Context, id, workerSessionID string) error
	SetPermissionCeilingIfEmpty(ctx context.Context, id, ceiling string) (string, error)
}

// SessionAdmin provides listing, ownership validation, and metadata mutations.
type SessionAdmin interface {
	SessionExpirer
	List(ctx context.Context, userID, platform, workspaceID string, limit, offset int) ([]*session.SessionInfo, error)
	ValidateOwnership(ctx context.Context, sessionID, userID, adminUserID string) error
	UpdateWorkDir(ctx context.Context, id, workDir string) error
}

// SessionExpirer resets session expiry timers. Extracted as a single-method
// interface so bridgeSM can compose it alongside reader/lifecycle/transition
// sub-interfaces without pulling in the full SessionAdmin.
type SessionExpirer interface {
	ResetExpiry(ctx context.Context, id string) error
}

// SessionManager composes all session sub-interfaces for full management.
type SessionManager interface {
	SessionReader
	SessionLifecycle
	SessionTransitioner
	SessionWorkerManager
	SessionAdmin
}

// WorkerFactory creates worker instances. Production code uses defaultWorkerFactory.
type WorkerFactory interface {
	NewWorker(t worker.WorkerType) (worker.Worker, error)
}

type defaultWorkerFactory struct{}

func (defaultWorkerFactory) NewWorker(t worker.WorkerType) (worker.Worker, error) {
	return worker.NewWorker(t)
}

func (h *Handler) handleInteractionResponseEvent(ctx context.Context, env *events.Envelope) error {
	h.cancelRetryIfNeeded(env.SessionID)

	si, err := h.sm.Get(ctx, env.SessionID)
	if err != nil {
		h.log.Warn("gateway: interaction response session not found", "session_id", env.SessionID, "err", err)
		return h.sendErrorf(ctx, env, events.ErrCodeSessionNotFound, "session not found")
	}

	w := h.sm.GetWorker(env.SessionID)
	if w == nil {
		h.log.Warn("gateway: interaction response no worker attached", "session_id", env.SessionID)
		return h.sendErrorf(ctx, env, events.ErrCodeSessionNotFound, "no worker attached to session")
	}

	metadata, err := interactionResponseMetadata(env.Event.Type, env.Event.Data)
	if err != nil {
		h.log.Warn("gateway: normalize interaction response failed", "err", err, "session_id", env.SessionID)
		return h.sendErrorf(ctx, env, events.ErrCodeInvalidMessage, "invalid response data: %v", err)
	}

	if err := w.Input(ctx, "", metadata); err != nil {
		h.log.Warn("gateway: worker interaction response delivery failed", "err", err, "session_id", env.SessionID)
		code := classifyWorkerError(err)
		if errors.Is(err, base.ErrInvalidSchema) {
			code = events.ErrCodeInvalidMessage
		}
		// Send error envelope but include request_id in Metadata to allow UI correlation
		var reqID string
		if dataMap, ok := env.Event.Data.(map[string]any); ok {
			reqID, _ = dataMap["id"].(string)
			if reqID == "" {
				reqID, _ = dataMap["request_id"].(string)
			}
		}
		errEnv := events.NewEnvelope(aep.NewID(), env.SessionID, 0, events.Error, events.ErrorData{
			Code:    code,
			Message: fmt.Sprintf("worker response failed: %v", err),
		})
		if reqID != "" {
			errEnv.Metadata = map[string]any{
				"interaction_error": map[string]any{
					"request_id": reqID,
				},
			}
		}
		_ = h.hub.SendToSession(ctx, errEnv)
		return fmt.Errorf("%s: worker response failed: %w", code, err)
	}

	h.emitInteractionAudit(env.OwnerID, si.Platform, env.SessionID, env.Event.Type, env.Event.Data, "")
	if h.bridge != nil {
		h.bridge.CaptureInboundEvent(env.SessionID, env.Seq, env.Event.Type, env.Event.Data)
	}
	// Explicit AEP interaction responses (used by WebChat) are acknowledged
	// only after the Worker native response endpoint accepts them. WebSocket
	// send success alone is not delivery success, so the browser treats this
	// correlated echo as the authoritative resolved/rejected transition.
	if h.hub != nil {
		ack := events.NewEnvelope(
			aep.NewID(),
			env.SessionID,
			0,
			env.Event.Type,
			env.Event.Data,
		)
		ack.OwnerID = env.OwnerID
		if err := h.hub.SendToSession(ctx, ack); err != nil {
			h.log.Warn("gateway: interaction response ack delivery failed",
				"err", err,
				"type", env.Event.Type,
				"session_id", env.SessionID)
		}
	}
	return nil
}

func interactionResponseMetadata(kind events.Kind, data any) (map[string]any, error) {
	dataMap, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("response data must be a map")
	}

	metadata := make(map[string]any)
	switch kind {
	case events.PermissionResponse:
		id, _ := dataMap["id"].(string)
		if id == "" {
			id, _ = dataMap["request_id"].(string)
		}
		allowed, _ := dataMap["allowed"].(bool)
		reason, _ := dataMap["reason"].(string)

		metadata["permission_response"] = map[string]any{
			"id":         id,
			"request_id": id,
			"allowed":    allowed,
			"reason":     reason,
		}

	case events.QuestionResponse:
		id, _ := dataMap["id"].(string)
		answers := dataMap["answers"]
		metadata["question_response"] = map[string]any{
			"id":      id,
			"answers": answers,
		}

	case events.ElicitationResponse:
		id, _ := dataMap["id"].(string)
		action, _ := dataMap["action"].(string)
		content := dataMap["content"]
		metadata["elicitation_response"] = map[string]any{
			"id":      id,
			"action":  action,
			"content": content,
		}

	default:
		return nil, fmt.Errorf("unsupported interaction response kind: %s", kind)
	}

	return metadata, nil
}

// Compile-time assertion: *Handler satisfies bridge.PendingReplayer so the
// bridge can late-inject it via SetPendingReplayer for done-time replay.
var _ PendingReplayer = (*Handler)(nil)
