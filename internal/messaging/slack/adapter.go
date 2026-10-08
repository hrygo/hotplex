// Package slack provides a Slack Socket Mode platform adapter.
package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/messaging/phrases"
	"github.com/hrygo/hotplex/internal/messaging/stt"
	"github.com/hrygo/hotplex/internal/messaging/textutil"
	"github.com/hrygo/hotplex/internal/observability"
	"github.com/hrygo/hotplex/internal/session"
	"github.com/hrygo/hotplex/pkg/events"

	"runtime/debug"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

const (
	messageExpiry    = 30 * time.Minute
	dedupMaxEntries  = 5000
	dedupTTL         = 30 * time.Minute
	mediaCleanupInt  = 6 * time.Hour
	defaultMediaTTL  = 24 * time.Hour
	maxMessageLength = 3800            // Slack limit is ~4000
	errPrefix        = "\u26a0\ufe0f " // ⚠️
	// handlerMsgTimeout controls the context timeout for HandleTextMessage.
	// Covers Bridge.Handle → session start → HTTP request (createSession, initSessionConn).
	// Does NOT cover acquireServer's process fork (proc.Start is not context-aware).
	// Set longer than cmd timeout because LLM agentic turns (multi-tool) can take minutes.
	handlerMsgTimeout = 300 * time.Second
	// handlerCmdTimeout controls the context timeout for CmdControl/CmdWorker paths.
	// These are lightweight control-plane operations that should complete quickly.
	handlerCmdTimeout = 60 * time.Second
)

// Subtypes that should never be processed.
var blockedSubtypes = map[string]bool{
	"message_changed": true, "message_deleted": true,
	"channel_join": true, "channel_leave": true,
	"group_join": true, "group_leave": true,
	"channel_topic": true, "channel_purpose": true,
}

func init() {
	messaging.Register(messaging.PlatformSlack, func(log *slog.Logger) messaging.PlatformAdapterInterface {
		return &Adapter{
			BaseAdapter: messaging.BaseAdapter[*SlackConn]{
				PlatformAdapter: messaging.PlatformAdapter{Log: log.With("channel", string(messaging.PlatformSlack))},
			},
		}
	})
}

// SendProactiveMessage delivers a system-originated message to a Slack channel.
func (a *Adapter) SendProactiveMessage(ctx context.Context, text string, platformKey map[string]string) error {
	channelID := platformKey["channel_id"]
	if channelID == "" {
		return fmt.Errorf("slack: missing channel_id in platform_key")
	}
	text = messaging.SanitizeText(text)
	_, _, err := a.client.PostMessageContext(ctx, channelID, slack.MsgOptionText(text, false))
	if err != nil {
		return fmt.Errorf("slack: send proactive message: %w", err)
	}
	return nil
}

// SendCronResult delivers a cron job result to a Slack channel.
func (a *Adapter) SendCronResult(ctx context.Context, text string, platformKey map[string]string) error {
	return a.SendProactiveMessage(ctx, text, platformKey)
}

// Adapter implements messaging.PlatformAdapterInterface for Slack Socket Mode.
type Adapter struct {
	messaging.BaseAdapter[*SlackConn]

	mu sync.RWMutex

	botToken           string
	appToken           string
	client             SlackAPI
	socketMode         *socketmode.Client
	botID              string
	teamID             string
	injectExclude      []string
	userCache          *UserCache
	statusMgr          *StatusManager
	isAssistantCapable atomic.Bool
	assistantEnabled   *bool
	transcriber        stt.Transcriber
	turnSummaryEnabled bool
	mediaRetention     time.Duration
	ttsPipeline        *TTSPipeline
	phrases            *phrases.Phrases
	Extras             map[string]any
	botName            string
	displayName        string
	iconEmoji          string

	rateLimiter   *ChannelRateLimiter
	slashLimiter  *SlashRateLimiter
	activeStreams map[string]*NativeStreamingWriter // messageTS -> writer
}

func (a *Adapter) Platform() messaging.PlatformType { return messaging.PlatformSlack }

var _ messaging.PlatformAdapterInterface = (*Adapter)(nil)
var _ messaging.ProactiveMessageSender = (*Adapter)(nil)

func (a *Adapter) GetBotID() string           { return a.botID }
func (a *Adapter) GetBotName() string         { return a.botName }
func (a *Adapter) GetInjectExclude() []string { return a.injectExclude }

func (a *Adapter) SetPhrases(p *phrases.Phrases) {
	if p != nil {
		a.phrases = p
	}
}

func (a *Adapter) ConfigureWith(config messaging.AdapterConfig) error {
	// Call base to set hub/sm/handler/bridge.
	_ = a.PlatformAdapter.ConfigureWith(config)

	// Slack-specific: tokens.
	a.botToken = config.ExtrasString("bot_token")
	a.appToken = config.ExtrasString("app_token")

	// Bridge reference and workdir.
	if config.Bridge != nil {
		if a.statusMgr != nil {
			a.statusMgr.SetWorkDir(config.Bridge.WorkDir())
		}
	}

	// Shared: gate, backoff delays.
	a.ConfigureShared(config)
	a.mediaRetention = config.ExtrasDuration("media_retention")

	// Platform-specific extras.
	if v := config.ExtrasBoolPtr("assistant_enabled"); v != nil {
		a.assistantEnabled = v
	}
	if t, ok := config.Extras["transcriber"].(stt.Transcriber); ok && t != nil {
		a.transcriber = t
	}
	if v, ok := config.Extras["turn_summary_enabled"].(bool); ok {
		a.turnSummaryEnabled = v
	}
	if p, ok := config.Extras["phrases"].(*phrases.Phrases); ok && p != nil {
		a.phrases = p
	} else {
		a.phrases = phrases.Defaults()
	}
	if p, ok := config.Extras["tts_pipeline"].(*TTSPipeline); ok && p != nil {
		a.ttsPipeline = p
	}

	a.Extras = config.Extras

	if v, ok := config.Extras["inject_exclude"].([]string); ok {
		a.injectExclude = v
	}

	if config.BotName != "" {
		a.botName = config.BotName
	}
	if v := config.ExtrasString("display_name"); v != "" {
		a.displayName = v
	}
	if v := config.ExtrasString("icon_emoji"); v != "" {
		a.iconEmoji = v
	}

	return nil
}

func (a *Adapter) Start(ctx context.Context) error {
	if !a.StartGuard() {
		a.Log.Debug("slack: adapter already started, skipping")
		return nil
	}
	if a.botToken == "" || a.appToken == "" {
		return fmt.Errorf("slack: botToken and appToken required")
	}

	client := slack.New(a.botToken, slack.OptionAppLevelToken(a.appToken))
	a.client = client
	a.socketMode = socketmode.New(client)

	// Fetch bot identity
	authTest, err := a.client.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack: auth test: %w", err)
	}
	a.botID = authTest.UserID
	a.teamID = authTest.TeamID

	a.rateLimiter = NewChannelRateLimiter(ctx)
	a.slashLimiter = NewSlashRateLimiter()
	a.Dedup = messaging.NewDedup(dedupMaxEntries, dedupTTL) // Slack-specific params
	a.Dedup.StartCleanup()
	a.userCache = NewUserCache(a.client)
	a.statusMgr = NewStatusManager(a, a.Log)
	a.Interactions = messaging.NewInteractionManager(a.Log)
	a.activeStreams = make(map[string]*NativeStreamingWriter)
	a.InitConnPool(func(key string) *SlackConn {
		parts := strings.SplitN(key, "#", 2)
		threadTS := ""
		if len(parts) > 1 {
			threadTS = parts[1]
		}
		return NewSlackConn(a, parts[0], threadTS, a.Bridge().WorkDir())
	})

	a.Log.Info("slack: starting Socket Mode", "bot_id", a.botID)

	// Async probe for Assistant API capability
	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.Log.Error("slack: panic in assistant probe", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		capable := a.ProbeAssistantCapability(probeCtx)
		a.isAssistantCapable.Store(capable)
		if capable {
			a.Log.Info("slack: Assistant API capability confirmed (paid workspace)")
		} else {
			a.Log.Info("slack: Assistant API not available, using emoji reaction fallback")
			a.statusMgr.SetEmojiOnly(true)
		}
	}()

	go a.runSocketMode(ctx)
	go a.cleanupMedia(ctx)
	return nil
}

func (a *Adapter) runSocketMode(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			a.Log.Error("slack: panic in runSocketMode", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	baseDelay := a.BackoffBaseDelay
	if baseDelay <= 0 {
		baseDelay = 1 * time.Second
	}
	maxDelay := a.BackoffMaxDelay
	if maxDelay <= 0 {
		maxDelay = 60 * time.Second
	}
	backoff := messaging.NewReconnectBackoff(baseDelay, maxDelay)

	var (
		attempt      atomic.Int64 // consecutive failed attempts (reset on clean close)
		attemptStart atomic.Int64 // unix nano when the current Run() attempt began
		established  atomic.Int64 // unix nano of the last Connected event
	)
	attempt.Store(1)
	// connUptime reports connection (or in-flight attempt) age so reconnect
	// flapping is visible without diffing timestamps.
	connUptime := func() string {
		if est := established.Load(); est > 0 {
			return time.Since(time.Unix(0, est)).Round(time.Millisecond).String()
		}
		if t := attemptStart.Load(); t > 0 {
			return time.Since(time.Unix(0, t)).Round(time.Millisecond).String()
		}
		return ""
	}

	// Run() blocks until the WebSocket closes. Wrap it in a loop so that
	// connection errors trigger automatic reconnect instead of silently exiting.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.Log.Error("slack: panic in socketMode reconnect loop", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			attemptStart.Store(time.Now().UnixNano())
			a.Log.Info("slack: starting socket mode", "attempt", attempt.Load())
			err := a.socketMode.Run()
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff.Next()):
				a.Log.Warn("slack: socket mode error, will retry",
					"err", err, "attempt", attempt.Load(), "conn_uptime", connUptime())
				established.Store(0)
				attempt.Add(1)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-a.socketMode.Events:
			if !ok {
				// Channel closed — Run() exited. The reconnect goroutine above will
				// detect this and restart the connection.
				a.Log.Warn("slack: events channel closed, waiting for reconnect")
				time.Sleep(500 * time.Millisecond)
				continue
			}
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				a.socketMode.Ack(*evt.Request) //nolint:errcheck // Ack must not block event processing
				go func() {
					defer func() {
						if r := recover(); r != nil {
							a.Log.Error("slack: panic in event handler", "panic", r, "stack", string(debug.Stack()))
						}
					}()
					a.handleEventsAPI(ctx, eventsAPI)
				}()

			case socketmode.EventTypeConnecting:
				a.Log.Debug("slack: websocket handshake in progress", "attempt", attempt.Load())
			case socketmode.EventTypeConnected:
				established.Store(time.Now().UnixNano())
				a.Log.Info("slack: websocket established, ready to receive events", "attempt", attempt.Load())
				backoff.Reset()

			case socketmode.EventTypeDisconnect:
				a.Log.Info("slack: disconnected by Slack server, reconnecting",
					"attempt", attempt.Load(), "conn_uptime", connUptime())
				established.Store(0)

			case socketmode.EventTypeConnectionError:
				a.Log.Warn("slack: websocket connection error, retrying...",
					"err", evt.Data, "attempt", attempt.Load(), "conn_uptime", connUptime())

			case socketmode.EventTypeInteractive:
				go func() {
					defer func() {
						if r := recover(); r != nil {
							a.Log.Error("slack: panic in interaction handler", "panic", r, "stack", string(debug.Stack()))
						}
					}()
					a.handleInteractionEvent(ctx, evt)
				}()

			case socketmode.EventTypeSlashCommand:
				go func() {
					defer func() {
						if r := recover(); r != nil {
							a.Log.Error("slack: panic in slash command handler", "panic", r, "stack", string(debug.Stack()))
						}
					}()
					a.handleSlashCommandEvent(ctx, evt)
				}()
			}
		}
	}
}

func (a *Adapter) handleEventsAPI(ctx context.Context, event slackevents.EventsAPIEvent) {
	switch e := event.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		a.handleMessageEvent(ctx, e, event.TeamID)
	case *slackevents.AppHomeOpenedEvent:
		a.handleAppHomeOpened(ctx, e)
	}
}

func (a *Adapter) handleMessageEvent(ctx context.Context, msgEvent *slackevents.MessageEvent, teamID string) {
	if msgEvent.BotID != "" {
		a.Log.Debug("slack: skipping bot message", "bot_id", msgEvent.BotID)
		return
	}

	if blockedSubtypes[msgEvent.SubType] {
		return
	}

	if msgEvent.TimeStamp != "" {
		if ts, err := parseSlackTS(msgEvent.TimeStamp); err == nil {
			if time.Since(ts) > messageExpiry {
				a.Log.Debug("slack: skipping expired message", "ts", msgEvent.TimeStamp)
				return
			}
		}
	}

	channelID := msgEvent.Channel
	threadTS := extractThreadTS(*msgEvent)
	userID := msgEvent.User
	text, ok, media := a.ConvertMessage(*msgEvent)
	if !ok {
		return
	}
	text = messaging.SanitizeText(text)

	channelType := ChannelGroup
	if channelID != "" && channelID[0] == 'D' {
		channelType = ChannelIM
	}

	// Access control gate (must run before ResolveMentions which strips <@BOTID>)
	if a.Gate != nil {
		botMentioned := strings.Contains(text, "<@"+a.botID+">")
		if allowed, reason := a.Gate.Check(channelType == ChannelIM, userID, botMentioned); !allowed {
			a.Log.Debug("slack: gate rejected", "reason", reason, "user", userID)
			return
		}
	}

	// Dedup with conditional commit: the entry is only kept when the message
	// was actually consumed (delivered to the worker or handled as a command /
	// interaction response). Rejected deliveries roll back so the platform's
	// retry of the same ClientMsgID is not silently deduplicated.
	platformMsgID := msgEvent.ClientMsgID
	if platformMsgID == "" {
		platformMsgID = msgEvent.TimeStamp
	}
	dedupHandle, recorded := a.Dedup.TryRecordWithHandle(platformMsgID)
	if !recorded {
		return
	}
	accepted := false
	defer func() {
		if !accepted {
			a.Dedup.Rollback(dedupHandle)
		}
	}()

	// Resolve user mentions: <@UID> → @DisplayName, remove bot self-mentions
	text = a.userCache.ResolveMentions(ctx, text, a.botID)
	text = strings.TrimSpace(text)

	if text == "" && len(media) == 0 {
		return
	}

	var hasVoice bool

	// Download media files and append paths to text
	if len(media) > 0 {
		for _, m := range media {
			if m.DownloadURL == "" {
				continue
			}
			// Audio + STT: transcribe voice messages to text.
			if m.Type == mediaTypeAudio && a.transcriber != nil {
				hasVoice = true
				if audioText, audioErr := a.handleAudioMessage(ctx, m); audioErr != nil {
					// STT failed: save audio to disk for worker fallback.
					if path, saveErr := a.downloadMedia(ctx, m); saveErr == nil {
						text += fmt.Sprintf("\n[audio STT failed, saved to: %s — please use stt_once.py to transcribe]", path)
					} else {
						text += fmt.Sprintf("\n[audio: %s (STT and download both failed)]", m.Name)
					}
				} else {
					text += audioText
				}
				continue
			}
			// Audio without transcriber: save to disk for worker fallback.
			if m.Type == mediaTypeAudio {
				hasVoice = true
				if path, err := a.downloadMedia(ctx, m); err == nil {
					text += fmt.Sprintf("\n[audio saved to: %s — please use stt_once.py to transcribe]", path)
				} else {
					text += fmt.Sprintf("\n[audio: %s (download failed)]", m.Name)
				}
				continue
			}
			path, err := a.downloadMedia(ctx, m)
			if err == nil {
				text += "\n" + path
			} else {
				a.Log.Warn("slack: download media failed", "file", m.Name, "err", err)
				text += fmt.Sprintf("\n[%s: %s]", m.Type, m.Name)
			}
		}
	}

	a.Log.Debug("slack: handling message",
		"channel", channelID,
		"thread", threadTS,
		"user", userID,
		"team", teamID,
		"text_len", len(text),
	)

	cmd := messaging.DetectCommand(text)
	switch cmd.Action {
	case messaging.CmdHelp:
		_ = a.SetStatus(ctx, channelID, threadTS, StatusThinking, "Loading help...")
		opts := []slack.MsgOption{
			slack.MsgOptionText(messaging.HelpText(), false),
		}
		if threadTS != "" {
			opts = append(opts, slack.MsgOptionTS(threadTS))
		}
		_, _, _ = a.client.PostMessageContext(ctx, channelID, opts...)
		_ = a.ClearStatus(ctx, channelID, threadTS)
		accepted = true
		return
	case messaging.CmdControl:
		conn := a.GetOrCreateConn(channelID, threadTS)
		if conn != nil {
			defer conn.lockHandlerMu()()
		}
		cmdCtx, cmdCancel := context.WithTimeout(ctx, handlerCmdTimeout)
		defer cmdCancel()
		a.handleTextControlCommand(cmdCtx, teamID, channelID, threadTS, userID, cmd.Control)
		accepted = true
		return
	case messaging.CmdWorker:
		conn := a.GetOrCreateConn(channelID, threadTS)
		if conn != nil {
			conn.messageTS = msgEvent.TimeStamp
			defer conn.lockHandlerMu()()
		}
		if a.isAssistantCapable.Load() && threadTS != "" {
			_ = a.SetAssistantStatus(ctx, channelID, threadTS, "Processing "+cmd.Worker.Label+"...")
		}
		cmdCtx, cmdCancel := context.WithTimeout(ctx, handlerCmdTimeout)
		defer cmdCancel()
		a.handleTextWorkerCommand(cmdCtx, teamID, channelID, threadTS, userID, cmd.Worker)
		accepted = true
		return
	}

	// Check if text is a response to a pending interaction (text fallback for Block Kit failures).
	if a.checkPendingInteraction(ctx, text, channelID, threadTS, userID) {
		accepted = true
		return
	}

	// Set initial assistant status (native API for paid workspaces)
	if a.isAssistantCapable.Load() && threadTS != "" {
		_ = a.SetAssistantStatus(ctx, channelID, threadTS, a.phrases.Random("status"))
	} else if msgEvent.TimeStamp != "" {
		// DM first message — no thread context for assistant status API.
		// Fall back to emoji reaction on the user message for visual feedback.
		_ = a.SetStatus(ctx, channelID, msgEvent.TimeStamp, StatusThinking, a.phrases.Random("status"))
	}

	if hasVoice {
		if conn := a.GetOrCreateConn(channelID, threadTS); conn != nil {
			conn.voiceTriggered.Store(true)
		}
	}

	if err := a.HandleTextMessage(ctx, platformMsgID, channelID, teamID, threadTS, userID, text); err != nil {
		a.Log.Warn("slack: handle message failed", "err", err, "channel", channelID, "thread", threadTS, "user", userID)

		// Clear the "Thinking..." status indicator so the user doesn't see
		// a permanent spinner. Safe to call even if status was never set.
		if a.isAssistantCapable.Load() && threadTS != "" {
			_ = a.ClearStatus(ctx, channelID, threadTS)
		}

		// Always notify the user of the failure, not just on timeout.
		// Without this, non-timeout errors (session start failure, worker
		// errors, config issues) are silently swallowed.
		if errors.Is(err, context.DeadlineExceeded) {
			a.sendEphemeralOrPost(ctx, channelID, threadTS, userID,
				"⚠️ Request timed out. The operation took too long and was cancelled. Please try again.")
		} else {
			a.sendEphemeralOrPost(ctx, channelID, threadTS, userID,
				"⚠️ Failed to process your message. Please try again or use /reset to restart the session.")
		}
		// Delivery failed: keep accepted=false so the dedup entry is rolled
		// back and a platform retry of the same ClientMsgID can be delivered.
	} else {
		accepted = true
	}
}

// GetOrCreateConn returns an existing SlackConn for the channel/thread pair,
// or creates and registers a new one. This ensures the same conn is reused
// across multiple messages in the same thread, so Hub.Shutdown can close it.
func (a *Adapter) GetOrCreateConn(channelID, threadTS string) *SlackConn {
	return a.BaseAdapter.GetOrCreateConn(channelID, threadTS)
}

func (a *Adapter) HandleTextMessage(ctx context.Context, platformMsgID, channelID, teamID, threadTS, userID, text string) error {
	if a.Bridge() == nil {
		// Must error (not return nil) so handleMessageEvent rolls back the
		// dedup entry: a message dropped without a bridge was never delivered.
		return fmt.Errorf("slack: bridge not configured")
	}

	conn := a.GetOrCreateConn(channelID, threadTS)
	if conn == nil {
		return fmt.Errorf("slack: adapter closed, dropping message for channel %s", channelID)
	}

	envelope := a.makeEnvelope(teamID, channelID, threadTS, userID, text, conn.WorkDir())
	if envelope == nil {
		return fmt.Errorf("slack: failed to build envelope")
	}
	if data, ok := envelope.Event.Data.(map[string]any); ok && platformMsgID != "" {
		data["platform_msg_id"] = platformMsgID
	}

	defer conn.lockHandlerMu()()
	msgCtx, cancel := context.WithTimeout(ctx, handlerMsgTimeout)
	defer cancel()
	return a.Bridge().Handle(msgCtx, envelope, conn)
}

// makeEnvelope builds an AEP input envelope for a Slack message.
func (a *Adapter) makeEnvelope(teamID, channelID, threadTS, userID, text, workDir string) *events.Envelope {
	if workDir == "" {
		workDir = a.Bridge().WorkDir()
	}
	return a.Bridge().MakeEnvelope(userID, text, session.PlatformContext{
		Platform:  string(messaging.PlatformSlack),
		BotID:     a.botID,
		TeamID:    teamID,
		ChannelID: channelID,
		ThreadTS:  threadTS,
		UserID:    userID,
		WorkDir:   workDir,
	})
}

// NewStreamingWriter creates a streaming writer for the given channel/thread.
func (a *Adapter) NewStreamingWriter(ctx context.Context, channelID, threadTS string, onComplete func(string)) *NativeStreamingWriter {
	if a.IsClosed() {
		return nil
	}
	a.mu.RLock()
	if a.IsClosed() || a.rateLimiter == nil {
		a.mu.RUnlock()
		return nil
	}
	client, teamID, rateLimiter, log := a.client, a.teamID, a.rateLimiter, a.Log
	a.mu.RUnlock()

	w := NewNativeStreamingWriter(ctx, client, channelID, threadTS, teamID, rateLimiter, log, func(ts string) {
		if !a.IsClosed() {
			a.mu.Lock()
			delete(a.activeStreams, ts)
			a.mu.Unlock()
		}
		if onComplete != nil {
			onComplete(ts)
		}
	}, func(w *NativeStreamingWriter) {
		if !a.IsClosed() {
			a.mu.Lock()
			if w.messageTS != "" {
				a.activeStreams[w.messageTS] = w
			}
			a.mu.Unlock()
		}
	})
	return w
}

// Close gracefully terminates the platform connection. Safe to call multiple times.
func (a *Adapter) Close(ctx context.Context) error {
	a.Log.Info("slack: adapter closing")
	a.MarkClosed()

	a.mu.Lock()
	streams := make([]*NativeStreamingWriter, 0, len(a.activeStreams))
	for _, w := range a.activeStreams {
		streams = append(streams, w)
	}
	a.activeStreams = nil
	a.mu.Unlock()
	for _, w := range streams {
		_ = w.Close()
	}

	conns := a.DrainConns()
	for _, c := range conns {
		_ = c.Close()
	}

	a.mu.Lock()
	if a.rateLimiter != nil {
		a.rateLimiter.Stop()
		a.rateLimiter = nil
	}
	if a.slashLimiter != nil {
		a.slashLimiter.Stop()
		a.slashLimiter = nil
	}
	a.mu.Unlock()
	a.CloseSharedState()
	if a.userCache != nil {
		a.userCache.Close()
	}

	if a.transcriber != nil {
		if closer, ok := a.transcriber.(stt.Closer); ok {
			_ = closer.Close(ctx)
		}
	}

	return nil
}

// handleAudioMessage downloads and transcribes a voice message, returning formatted text.
func (a *Adapter) handleAudioMessage(ctx context.Context, m *MediaInfo) (string, error) {
	audioData, err := a.downloadMediaBytes(ctx, m)
	if err != nil {
		a.Log.Warn("slack: download audio failed", "file", m.Name, "err", err)
		return "", err
	}

	transcript, err := a.transcriber.Transcribe(ctx, audioData)
	if err != nil {
		a.Log.Warn("slack: stt failed", "file", m.Name, "err", err)
		return "", err
	}

	var text string
	if transcript != "" {
		text = fmt.Sprintf("\n[voice message transcription]: %s", transcript)
	} else {
		text = fmt.Sprintf("\n[voice message: %s (empty transcription)]", m.Name)
	}

	if a.transcriber.RequiresDisk() {
		if path, err := a.saveMediaBytes(m, audioData); err == nil {
			text += "\n" + path
		}
	}
	return text, nil
}

// handleTextControlCommand sends a control event derived from a text message
// through the bridge, then sends ephemeral feedback to the user.
func (a *Adapter) handleTextControlCommand(ctx context.Context, teamID, channelID, threadTS, userID string, result *messaging.ControlCommandResult) {
	if a.Bridge() == nil {
		a.Log.Warn("slack: text control command dropped without bridge", "action", result.Label)
		return
	}

	conn := a.GetOrCreateConn(channelID, threadTS)
	if conn == nil {
		a.Log.Warn("slack: adapter closed, dropping control command", "action", result.Label)
		return
	}

	env := a.makeEnvelope(teamID, channelID, threadTS, userID, "", conn.WorkDir())
	if env == nil {
		a.Log.Warn("slack: text control command failed to derive session", "action", result.Label)
		return
	}

	ctrlEnv := messaging.BuildControlEnvelope(result, env.SessionID, userID)

	// CD sends progress feedback before execution; other actions send completion feedback after.
	if result.Action == events.ControlActionCD {
		a.sendEphemeralOrPost(ctx, channelID, threadTS, userID, controlFeedbackMessage(result.Action))
	}

	if err := a.Bridge().Handle(ctx, ctrlEnv, conn); err != nil {
		a.Log.Warn("slack: text control command failed", "action", result.Label, "err", err)
		if errors.Is(err, context.DeadlineExceeded) {
			a.sendEphemeralOrPost(ctx, channelID, threadTS, userID,
				fmt.Sprintf("⚠️ %s timed out. Please try again.", result.Label))
			return
		}
		// Provide user-friendly error message with details
		errMsg := fmt.Sprintf("❌ Failed to execute %s: %s", result.Label, formatSecurityErrorSlack(err))
		a.sendEphemeralOrPost(ctx, channelID, threadTS, userID, errMsg)
		return
	}

	a.Log.Info("slack: text control command sent", "action", result.Label, "user", userID, "session_id", env.SessionID)

	// After a successful CD, update conn's workDir so subsequent messages
	// derive the correct session ID for the target directory.
	if result.Action == events.ControlActionCD && result.Arg != "" {
		if expanded, err := config.ExpandAndAbs(result.Arg); err == nil {
			conn.SetWorkDir(expanded)
		}
	}

	// Reset/GC kills the worker without a guaranteed done event, so stale
	// pending interactions (permission/question/elicitation) may survive.
	// Cancel them now so stale interactive buttons don't route to the new worker.
	if result.Action == events.ControlActionReset || result.Action == events.ControlActionGC {
		a.Interactions.CancelAll(env.SessionID)
		// Abort any active streaming writer — GC/Reset kills the worker without a
		// done event, so the writer would otherwise remain active until TTL expiry.
		if conn := a.GetOrCreateConn(channelID, threadTS); conn != nil {
			// Give the stream close its own fixed budget independent of the
			// (short) command ctx: a ctx-cancelled StopStream returns an error
			// after closeStreamWriter already cleared the writer, stranding the
			// old stream half-open — subsequent deltas would open a SECOND
			// streaming message in the same thread.
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := conn.closeStreamWriter(closeCtx)
			closeCancel()
			if err != nil {
				a.Log.Warn("slack: failed to close stream on control command",
					"channel", channelID, "thread", threadTS, "err", err)
				conn.recordTerminalFailure(closeCtx, terminalFailureResult(err))
			}
		}
	}

	// Stop is confirmed by Gateway's done.reason=stopped_by_user event. Avoid a
	// duplicate local completion message while its terminal delivery settles.
	// CD feedback was sent before execution; other controls complete locally.
	if result.Action != events.ControlActionCD && result.Action != events.ControlActionStop {
		a.sendEphemeralOrPost(ctx, channelID, threadTS, userID, controlFeedbackMessage(result.Action))
	}
}

func (a *Adapter) handleTextWorkerCommand(ctx context.Context, teamID, channelID, threadTS, userID string, result *messaging.WorkerCommandResult) {
	conn := a.GetOrCreateConn(channelID, threadTS)
	if conn == nil {
		a.Log.Warn("slack: adapter closed, dropping worker command", "command", result.Label)
		return
	}

	envelope := a.makeEnvelope(teamID, channelID, threadTS, userID, "", conn.WorkDir())
	if envelope == nil {
		a.Log.Warn("slack: worker command failed to derive session", "command", result.Label)
		return
	}

	cmdEnv := messaging.BuildWorkerCommandEnvelope(result, envelope.SessionID, userID)

	if err := a.Bridge().Handle(ctx, cmdEnv, conn); err != nil {
		a.Log.Warn("slack: worker command failed", "command", result.Label, "err", err)
		if errors.Is(err, context.DeadlineExceeded) {
			a.sendEphemeralOrPost(ctx, channelID, threadTS, userID,
				fmt.Sprintf("⚠️ %s timed out. Please try again.", result.Label))
			return
		}
		a.sendEphemeralOrPost(ctx, channelID, threadTS, userID, fmt.Sprintf("❌ Failed to execute %s.", result.Label))
		return
	}

	a.Log.Info("slack: worker command sent", "command", result.Label, "user", userID, "session_id", envelope.SessionID)
}

func controlFeedbackMessage(action events.ControlAction) string {
	return messaging.ControlFeedbackMessage(action, messaging.ControlFeedbackEN, "✅ Done.")
}

// SlackConn wraps the adapter with channel/thread routing info
// to satisfy messaging.PlatformConn for Hub.JoinPlatformSession.
type SlackConn struct {
	adapter   *Adapter
	channelID string
	threadTS  string
	messageTS string // anchor message for typing indicator cleanup

	handlerMu      sync.Mutex // serializes control commands and message handling per thread
	streamWriter   streamContentCloser
	streamWriterMu sync.Mutex
	mu             sync.RWMutex // protects workDir
	workDir        string       // current workDir identity for session key derivation

	lastSummarySentMs atomic.Int64 // unix ms of last successful turn summary send
	voiceTriggered    atomic.Bool
	// paraBreaker is accessed from the writeLoop goroutine which serializes
	// all WriteCtx calls per conn (see pcEntry.writeLoop in platform_writer.go).
	// No additional mutex needed — concurrent access is impossible by design.
	paraBreaker textutil.ParagraphBreaker
}

// NewSlackConn creates a platform connection bound to a channel/thread.
func NewSlackConn(adapter *Adapter, channelID, threadTS, workDir string) *SlackConn {
	return &SlackConn{adapter: adapter, channelID: channelID, threadTS: threadTS, workDir: workDir}
}

// lockHandlerMu acquires the per-thread serialization lock.
// Returns an unlock function for use with defer.
//
// Known limitation: Lock() blocks without timeout if another goroutine holds
// handlerMu. The timeout on the holder side ensures eventual release. A future
// improvement could use TryLock + retry. Callers should use:
//
//	defer conn.lockHandlerMu()()
func (c *SlackConn) lockHandlerMu() (unlock func()) {
	c.handlerMu.Lock()
	return c.handlerMu.Unlock
}

func (c *SlackConn) WorkDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.workDir
}

func (c *SlackConn) SetWorkDir(dir string) {
	c.mu.Lock()
	c.workDir = dir
	adapter := c.adapter
	c.mu.Unlock()
	// Propagate to StatusManager so $WK substitution uses this conn's workDir.
	if adapter != nil && adapter.statusMgr != nil {
		adapter.statusMgr.SetWorkDir(dir)
	}
}

// notifyStatus sets processing status (nil-safe for tests).
func (c *SlackConn) notifyStatus(ctx context.Context, text string) {
	if c.adapter != nil && c.adapter.statusMgr != nil {
		_ = c.adapter.statusMgr.Notify(ctx, c.channelID, c.threadTS, StatusThinking, text)
	}
}

// clearStatus clears processing status (nil-safe for tests).
func (c *SlackConn) clearStatus(ctx context.Context) {
	if c.adapter != nil {
		_ = c.adapter.ClearStatus(ctx, c.channelID, c.threadTS)
	}
}

// WriteCtx sends an AEP envelope to the bound Slack channel/thread.
func (c *SlackConn) WriteCtx(ctx context.Context, env *events.Envelope) error {
	if env == nil {
		return fmt.Errorf("slack: nil envelope")
	}

	c.notifyStatusFromEvent(ctx, env)

	switch env.Event.Type {
	case events.Done:
		return c.handleDone(ctx, env)
	case events.Error:
		return c.handleError(ctx, env)
	case events.PermissionRequest:
		return c.handleInteraction(ctx, env, "Permission request...", c.sendPermissionRequest)
	case events.QuestionRequest:
		return c.handleInteraction(ctx, env, "Awaiting response...", c.sendQuestionRequest)
	case events.ElicitationRequest:
		return c.handleInteraction(ctx, env, "Gathering input...", c.sendElicitationRequest)
	case events.ContextUsage:
		return c.handleNotifyAndSend(ctx, env, "Loading context usage...", c.sendContextUsage)
	case events.MCPStatus:
		return c.handleNotifyAndSend(ctx, env, "Loading MCP status...", c.sendMCPStatus)
	case events.SkillsList:
		return c.handleSkillsList(ctx, env)
	case events.Message:
		return c.handleStandaloneMessage(ctx, env)
	}

	return c.handleDefaultText(ctx, env)
}

// writeWithStreaming attempts to write using the streaming API.
func (c *SlackConn) writeWithStreaming(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}

	if c.adapter == nil {
		return fmt.Errorf("slack: adapter is nil")
	}

	c.streamWriterMu.Lock()
	defer c.streamWriterMu.Unlock()

	// TTL rotation: proactively replace expired streams before
	// Slack's server-side streaming limit kicks in. The old writer no longer
	// sends a fallback itself; its stop failure is best-effort (the content
	// continues streaming into the replacement writer). Expired is a
	// NativeStreamingWriter-only capability, so the check type-asserts.
	if oldWriter, ok := c.streamWriter.(*NativeStreamingWriter); ok && oldWriter.Expired() {
		c.streamWriter = nil
		go func() {
			// The rotated writer's Close flushes its buffered tail to a
			// server-expired stream — a StreamTerminalError{ContentPresented:
			// false} here means the tail was LOST. Surface it through the
			// terminal failure accounting (fallback counter + structured
			// failure result) instead of discarding it, so a visible content
			// gap is never silent. Bound the close by its own deadline; the
			// rotation must not block the streaming hot path.
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := oldWriter.CloseContext(closeCtx); err != nil {
				c.adapter.Log.Warn("slack: rotated stream close failed",
					"channel", c.channelID, "err", err)
				c.recordTerminalFailure(closeCtx, terminalFailureResult(err))
			}
		}()
		c.adapter.Log.Info("slack: stream rotated",
			"channel", c.channelID,
			"old_msg_ts", oldWriter.messageTS)
	}

	// Create new streaming writer if needed
	if c.streamWriter == nil {
		writer := c.adapter.NewStreamingWriter(ctx, c.channelID, c.threadTS, func(ts string) {
			c.streamWriterMu.Lock()
			if c.threadTS == "" && ts != "" {
				c.threadTS = ts
			}
			c.streamWriterMu.Unlock()
		})
		if writer == nil {
			return fmt.Errorf("failed to create streaming writer")
		}
		c.streamWriter = writer
	}

	_, err := c.streamWriter.Write([]byte(text))
	return err
}

// writeWithPostMessage falls back to PostMessageContext.
// Handles long messages by chunking them into multiple calls.
// Extracts local image paths and renders them as Block Kit Image Blocks.
func (c *SlackConn) writeWithPostMessage(ctx context.Context, text string, isDelta bool) error {
	if c.adapter == nil || c.adapter.client == nil {
		return fmt.Errorf("slack: client not initialized")
	}
	if isDelta && text != "" {
		text += "\n\n"
	}

	if err := c.tryTableBlocks(ctx, text); err == nil {
		return nil
	}
	if err := c.tryImageBlocks(ctx, text); err == nil {
		return nil
	}

	chunks := ChunkContent(text, maxMessageLength)
	for _, chunk := range chunks {
		opts := []slack.MsgOption{slack.MsgOptionText(FormatMrkdwn(chunk), false)}
		if c.threadTS != "" {
			opts = append(opts, slack.MsgOptionTS(c.threadTS))
		}
		_, _, err := c.adapter.client.PostMessageContext(ctx, c.channelID, opts...)
		if err != nil {
			return err
		}
	}
	return nil
}

// tryTableBlocks extracts markdown tables and sends as Block Kit (MarkdownBlock + DataTableBlock).
// Returns error if no tables found or block send fails (caller falls back to text).
func (c *SlackConn) tryTableBlocks(ctx context.Context, text string) error {
	segments, tables := ExtractTables(text)
	if len(tables) == 0 {
		return fmt.Errorf("no tables")
	}

	blocks := BuildTableBlocks(text, segments, tables)
	if len(blocks) == 0 {
		return fmt.Errorf("no valid table blocks")
	}

	if err := ValidateBlocks(blocks); err != nil {
		return fmt.Errorf("validate: %w", err)
	}

	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(text, false),
	}
	if c.threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(c.threadTS))
	}

	_, _, pErr := c.adapter.client.PostMessageContext(ctx, c.channelID, opts...)
	if pErr != nil {
		if isInvalidBlocksError(pErr) {
			c.adapter.Log.Warn("slack: table blocks rejected", "err", pErr)
		}
		return pErr
	}
	return nil
}

// tryImageBlocks attempts to send text with images as Block Kit.
// Returns error if no images found or block send fails (caller falls back to text).
func (c *SlackConn) tryImageBlocks(ctx context.Context, text string) error {
	parts, remaining := extractImages(text)
	if len(parts) == 0 {
		return fmt.Errorf("no images")
	}

	blocks := buildImageBlocks(parts, remaining)
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(FormatMrkdwn(remaining), false),
	}
	if c.threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(c.threadTS))
	}
	_, _, err := c.adapter.client.PostMessageContext(ctx, c.channelID, opts...)
	if err != nil {
		c.adapter.Log.Warn("slack: image blocks failed, falling back to text", "err", err)
	}
	return err
}

// postFile uploads a file to Slack and posts a reference in the thread.
func (a *Adapter) postFile(ctx context.Context, channelID, threadTS, filePath, title string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if len(data) > uploadMaxSize {
		return "", fmt.Errorf("file too large: %d bytes", len(data))
	}

	params := slack.UploadFileParameters{
		Filename:        filepath.Base(filePath),
		Title:           title,
		Reader:          strings.NewReader(string(data)),
		FileSize:        len(data),
		Channel:         channelID,
		ThreadTimestamp: threadTS,
	}

	file, err := a.client.UploadFileContext(ctx, params)
	if err != nil {
		return "", fmt.Errorf("upload file: %w", err)
	}

	return file.ID, nil
}

// uploadableExtensions are file types that should be uploaded rather than sent as text.
var uploadableExtensions = []string{".pdf", ".csv", ".xlsx", ".docx"}

// tryFileUpload checks if text contains a local file path that should be uploaded.
// Returns true if a file was successfully uploaded.
func (c *SlackConn) tryFileUpload(ctx context.Context, text string) bool {
	if c.adapter == nil || c.adapter.client == nil {
		return false
	}
	trimmed := strings.TrimSpace(text)
	for _, ext := range uploadableExtensions {
		if !strings.HasSuffix(trimmed, ext) {
			continue
		}
		// Check if the trimmed text is or ends with a file path
		lines := strings.Split(trimmed, "\n")
		lastLine := strings.TrimSpace(lines[len(lines)-1])
		if _, err := os.Stat(lastLine); err != nil {
			continue
		}
		fileID, err := c.adapter.postFile(ctx, c.channelID, c.threadTS, lastLine, filepath.Base(lastLine))
		if err != nil {
			c.adapter.Log.Warn("slack: file upload failed, falling back to text", "path", lastLine, "err", err)
			return false
		}
		// Send any preceding text along with upload confirmation
		prefix := strings.Join(lines[:len(lines)-1], "\n")
		prefix = strings.TrimSpace(prefix)
		msg := fmt.Sprintf("📎 Uploaded: %s", filepath.Base(lastLine))
		if prefix != "" {
			msg = FormatMrkdwn(prefix) + "\n" + msg
		}
		opts := []slack.MsgOption{slack.MsgOptionText(msg, false)}
		if c.threadTS != "" {
			opts = append(opts, slack.MsgOptionTS(c.threadTS))
		}
		_, _, _ = c.adapter.client.PostMessageContext(ctx, c.channelID, opts...)
		_ = fileID
		return true
	}
	return false
}

// getStreamWriter returns the current stream writer (thread-safe).
func (c *SlackConn) getStreamWriter() streamContentCloser {
	c.streamWriterMu.Lock()
	defer c.streamWriterMu.Unlock()
	return c.streamWriter
}

// closeStreamWriter closes and clears the stream writer, returning the
// terminal close error. CloseContext runs outside the lock — the lock is
// released first to avoid a deadlock:
// CloseContext() → onComplete → writeWithStreaming's callback → Lock(streamWriterMu).
// A nil writer is a nil no-op; once cleared, the call stays idempotent.
func (c *SlackConn) closeStreamWriter(ctx context.Context) error {
	c.streamWriterMu.Lock()
	w := c.streamWriter
	c.streamWriter = nil
	c.streamWriterMu.Unlock()

	if w == nil {
		return nil
	}
	return w.CloseContext(ctx)
}

// Close removes the conn from the adapter registry and cleans up the stream
// writer. Close cannot propagate the stream error (PlatformConn.Close has no
// error return), so it is surfaced as a structured Warn plus the terminal
// failure counter.
func (c *SlackConn) Close() error {
	c.adapter.DeleteConn(c.channelID, c.threadTS)

	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.closeStreamWriter(closeCtx); err != nil {
		c.adapter.Log.Warn("slack: close failed to finalize stream",
			"channel", c.channelID, "thread", c.threadTS, "err", err)
		c.recordTerminalFailure(closeCtx, terminalFailureResult(err))
	}

	c.clearStatus(context.Background())

	return nil
}

func (a *Adapter) cleanupMedia(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			a.Log.Error("slack: panic in cleanupMedia", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	ticker := time.NewTicker(mediaCleanupInt)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanupMediaInDir(MediaPathPrefix)
		}
	}
}

func (a *Adapter) cleanupMediaInDir(dir string) {
	ttl := a.mediaRetention
	if ttl <= 0 {
		ttl = defaultMediaTTL
	}
	var deleted, freedBytes int64
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && time.Since(info.ModTime()) > ttl {
			if err := os.Remove(path); err != nil {
				a.Log.Warn("slack: failed to remove old media file", "path", path, "err", err)
				return nil
			}
			deleted++
			freedBytes += info.Size()
		}
		return nil
	})
	// Only log when work was done; an empty pass is routine noise.
	if deleted > 0 {
		a.Log.Debug("slack: cleaned up old media files",
			"dir", dir, "deleted", deleted, "freed_bytes", freedBytes)
		observability.RecordLifecycleGCProcessed(context.Background(), observability.LifecycleGCProcessedMediaFile, deleted)
	}
}

func (c *SlackConn) sendTurnSummary(_ context.Context, env *events.Envelope) {
	if c.adapter == nil || c.adapter.client == nil {
		return
	}
	// Throttle: at most once per turnSummaryCooldown per connection.
	now := time.Now().UnixMilli()
	last := c.lastSummarySentMs.Load()
	if now-last < messaging.TurnSummaryCooldown.Milliseconds() {
		return
	}

	d := messaging.ExtractTurnSummary(env)
	plainText := messaging.FormatTurnSummary(d)
	if plainText == "" {
		return
	}

	// Primary: DataTableBlock with rich per-field layout.
	blocks := c.buildTurnSummaryTable(d)
	richText := messaging.FormatTurnSummaryRich(d)
	if richText == "" {
		richText = plainText
	}
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(richText, false),
	}
	if c.threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(c.threadTS))
	}
	sendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := c.adapter.client.PostMessageContext(sendCtx, c.channelID, opts...)
	if err == nil {
		c.lastSummarySentMs.Store(now)
		return
	}

	if !isInvalidBlocksError(err) {
		c.adapter.Log.Warn("turn summary send failed", "err", err)
		return
	}

	// Fallback: rich plain text with emoji-prefixed fields.
	c.adapter.Log.Warn("slack: turn summary DataTableBlock rejected, falling back to rich text", "err", err)
	fbOpts := []slack.MsgOption{slack.MsgOptionText(richText, false)}
	if c.threadTS != "" {
		fbOpts = append(fbOpts, slack.MsgOptionTS(c.threadTS))
	}
	_, _, fbErr := c.adapter.client.PostMessageContext(sendCtx, c.channelID, fbOpts...)
	if fbErr != nil {
		c.adapter.Log.Warn("turn summary fallback send failed", "err", fbErr)
	} else {
		c.lastSummarySentMs.Store(now)
	}
}

func (c *SlackConn) buildTurnSummaryTable(d messaging.TurnSummaryData) []slack.Block {
	table := slack.NewDataTableBlock("Turn Summary", slack.DataTableBlockOptionBlockID("turn_summary"))

	for _, f := range d.Fields() {
		val := f.Value
		if f.Label == "🔧 Tools" {
			val = formatToolNamesSlack(d.ToolNames, d.ToolCallCount)
		}
		table.AddRow(dataTableCell(f.Label), dataTableCell(val))
	}

	return []slack.Block{table}
}

func formatToolNamesSlack(names map[string]int, total int) string {
	s := messaging.FormatToolNames(names, total)
	if len(names) == 0 {
		return s + " calls"
	}
	return strings.Replace(s, fmt.Sprintf("%d (", total), fmt.Sprintf("%d calls (", total), 1)
}

func (c *SlackConn) sendContextUsage(ctx context.Context, env *events.Envelope) error {
	if c.adapter == nil || c.adapter.client == nil {
		return fmt.Errorf("slack: client not initialized")
	}

	d, err := messaging.ExtractContextUsageData(env)
	if err != nil {
		return nil
	}
	plainText := messaging.FormatCanonicalText(d)

	// Primary: DataTableBlock (may be rejected by some workspaces)
	blocks := c.buildContextUsageTable(d)
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(plainText, false),
	}
	if c.threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(c.threadTS))
	}
	_, _, pErr := c.adapter.client.PostMessageContext(ctx, c.channelID, opts...)
	if pErr == nil {
		return nil
	}
	if !isInvalidBlocksError(pErr) {
		return pErr
	}

	// Fallback: ContextBlock (universally supported)
	c.adapter.Log.Warn("slack: context usage DataTableBlock rejected, falling back to ContextBlock", "err", pErr)
	fbBlocks := c.buildContextUsageFallback(d)
	fbOpts := []slack.MsgOption{
		slack.MsgOptionBlocks(fbBlocks...),
		slack.MsgOptionText(plainText, false),
	}
	if c.threadTS != "" {
		fbOpts = append(fbOpts, slack.MsgOptionTS(c.threadTS))
	}
	_, _, fbErr := c.adapter.client.PostMessageContext(ctx, c.channelID, fbOpts...)
	return fbErr
}

// buildContextUsageTable builds a DataTableBlock for context usage (primary format).
func (c *SlackConn) buildContextUsageTable(d events.ContextUsageData) []slack.Block {
	info := messaging.BuildContextDisplay(d)
	table := slack.NewDataTableBlock("Context Usage", slack.DataTableBlockOptionBlockID("context_usage"))

	table.AddRow(dataTableCell(info.Icon+" Context"), dataTableCell(fmt.Sprintf("%s %s", info.ProgressBar, info.TokenDisplay)))
	if info.Model != "" {
		table.AddRow(dataTableCell("Model"), dataTableCell(info.Model))
	}
	if len(info.TopCategories) > 0 {
		catParts := make([]string, len(info.TopCategories))
		for i, cat := range info.TopCategories {
			catParts[i] = fmt.Sprintf("%s: %s", cat.Name, messaging.FormatTokenCount(cat.Tokens))
		}
		table.AddRow(dataTableCell("Top Context"), dataTableCell(strings.Join(catParts, ", ")))
	}
	if info.ExtrasLine != "" {
		table.AddRow(dataTableCell("Extras"), dataTableCell(info.ExtrasLine))
	}
	if info.ActionTip != "" {
		table.AddRow(dataTableCell("Tip"), dataTableCell(info.ActionTip))
	}
	return []slack.Block{table}
}

// buildContextUsageFallback builds ContextBlock fallback when DataTableBlock is rejected.
func (c *SlackConn) buildContextUsageFallback(d events.ContextUsageData) []slack.Block {
	text := slack.NewTextBlockObject("mrkdwn", messaging.FormatCanonicalText(d), false, false)
	return []slack.Block{slack.NewContextBlock("", text)}
}

// dataTableCell creates a plain-text cell for use in DataTableBlock rows.
func dataTableCell(text string) slack.DataTableCell {
	return slack.NewDataTableRawTextCell(text)
}

func (c *SlackConn) sendMCPStatus(ctx context.Context, env *events.Envelope) error {
	if c.adapter == nil || c.adapter.client == nil {
		return fmt.Errorf("slack: client not initialized")
	}

	d, ok := messaging.ExtractMCPStatusData(env)
	if !ok {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("🔌 MCP Server Status\n")
	for _, s := range d.Servers {
		fmt.Fprintf(&sb, "%s %s — %s\n", messaging.MCPServerIcon(s.Status), s.Name, s.Status)
	}
	plainText := sb.String()

	table := slack.NewDataTableBlock("MCP Status", slack.DataTableBlockOptionBlockID("mcp_status"))
	table.AddRow(dataTableCell("🔌 MCP Status"), dataTableCell(fmt.Sprintf("%d servers", len(d.Servers))))
	for _, s := range d.Servers {
		table.AddRow(dataTableCell(messaging.MCPServerIcon(s.Status)+" "+s.Name), dataTableCell(s.Status))
	}

	blocks := []slack.Block{table}
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(plainText, false),
	}
	if c.threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(c.threadTS))
	}
	_, _, err := c.adapter.client.PostMessageContext(ctx, c.channelID, opts...)
	if err != nil {
		if isInvalidBlocksError(err) {
			c.adapter.Log.Warn("slack: MCP status DataTableBlock rejected, falling back to plain text", "err", err)
			fbOpts := []slack.MsgOption{slack.MsgOptionText(plainText, false)}
			if c.threadTS != "" {
				fbOpts = append(fbOpts, slack.MsgOptionTS(c.threadTS))
			}
			_, _, fbErr := c.adapter.client.PostMessageContext(ctx, c.channelID, fbOpts...)
			return fbErr
		}
	}
	return err
}

// formatSecurityErrorSlack converts technical security errors into user-friendly messages for Slack.
func formatSecurityErrorSlack(err error) string {
	return messaging.FormatSecurityError(err, messaging.SecurityMessagesEN)
}

var _ messaging.PlatformConn = (*SlackConn)(nil)
