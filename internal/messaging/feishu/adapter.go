package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/internal/messaging/phrases"
	"github.com/hrygo/hotplex/internal/messaging/stt"
	"github.com/hrygo/hotplex/pkg/events"
)

func init() {
	messaging.Register(messaging.PlatformFeishu, func(log *slog.Logger) messaging.PlatformAdapterInterface {
		return &Adapter{
			BaseAdapter: messaging.BaseAdapter[*FeishuConn]{
				PlatformAdapter: messaging.PlatformAdapter{Log: log.With("channel", string(messaging.PlatformFeishu))},
			},
		}
	})
}

type Adapter struct {
	messaging.BaseAdapter[*FeishuConn]

	appID              string
	appSecret          string
	wsClient           *ws.Client
	larkClient         *lark.Client
	botOpenID          string
	botName            string // YAML config name for agent-config path resolution
	injectExclude      []string
	transcriber        Transcriber
	turnSummaryEnabled bool
	ttsPipeline        *TTSPipeline
	phrases            *phrases.Phrases
	displayName        string
	Extras             map[string]any

	mu          sync.RWMutex
	chatQueue   *ChatQueue
	rateLimiter *FeishuRateLimiter
}

func (a *Adapter) Platform() messaging.PlatformType { return messaging.PlatformFeishu }

var _ messaging.PlatformAdapterInterface = (*Adapter)(nil)
var _ messaging.ProactiveMessageSender = (*Adapter)(nil)

func (a *Adapter) GetBotID() string           { return a.botOpenID }
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

	// Shared adapter state (gate, backoff delays).
	a.ConfigureShared(config)

	// Feishu-specific: credentials.
	a.appID = config.ExtrasString("app_id")
	a.appSecret = config.ExtrasString("app_secret")

	// Store YAML config name for agent-config path resolution.
	a.botName = config.BotName

	// Platform-specific extras.
	if t, ok := config.Extras["transcriber"].(Transcriber); ok && t != nil {
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

	return nil
}

func (a *Adapter) Start(ctx context.Context) error {
	if !a.StartGuard() {
		a.Log.Debug("feishu: adapter already started, skipping")
		return nil
	}
	if a.appID == "" || a.appSecret == "" {
		return fmt.Errorf("feishu: appID and appSecret required")
	}

	a.InitSharedState()
	a.InitConnPool(func(key string) *FeishuConn {
		parts := strings.SplitN(key, "#", 2)
		threadKey := ""
		if len(parts) > 1 {
			threadKey = parts[1]
		}
		return NewFeishuConn(a, parts[0], threadKey, a.Bridge().WorkDir())
	})
	a.chatQueue = NewChatQueue(a.Log)
	a.rateLimiter = NewFeishuRateLimiter()
	a.rateLimiter.Start()

	a.larkClient = lark.NewClient(a.appID, a.appSecret,
		lark.WithLogger(SlogLogger{Logger: a.Log}),
		lark.WithHttpClient(newMediaBoundedHTTPClient(http.DefaultClient)),
	)

	if err := a.fetchBotInfo(ctx); err != nil {
		return fmt.Errorf("feishu: failed to resolve bot identity: %w", err)
	}

	a.Log.Info("feishu: starting WebSocket connection")
	go a.runWebSocket(ctx)
	go a.cleanupMedia(ctx)

	return nil
}

func (a *Adapter) fetchBotInfo(ctx context.Context) error {
	botCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := a.larkClient.Get(botCtx, "/open-apis/bot/v3/info", nil, "tenant_access_token")
	if err != nil {
		return fmt.Errorf("bot info API: %w", err)
	}

	body := resp.RawBody
	if len(body) == 0 {
		return fmt.Errorf("bot info API: empty response body")
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			AppName string `json:"app_name"`
		} `json:"bot"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse bot info: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("bot info API error: code=%d msg=%s", result.Code, result.Msg)
	}
	if result.Bot.OpenID == "" {
		return fmt.Errorf("bot open_id is empty")
	}
	a.botOpenID = result.Bot.OpenID
	if result.Bot.AppName != "" {
		a.displayName = result.Bot.AppName
	} else {
		a.displayName = "HotPlex"
	}
	a.Log.Info("feishu: bot identity resolved", "open_id", a.botOpenID, "name", a.displayName)
	return nil
}

// resolveDisplayName returns the bot display name (set during Start by fetchBotInfo).
// Falls back to "HotPlex" if the name was not resolved.
func (a *Adapter) resolveDisplayName() string {
	if a.displayName == "" {
		return "HotPlex"
	}
	return a.displayName
}

func (a *Adapter) Close(ctx context.Context) error {
	if a.Log != nil {
		a.Log.Info("feishu: adapter closing")
	}

	// Shut down persistent STT subprocess if present.
	if closer, ok := a.transcriber.(stt.Closer); ok {
		if err := closer.Close(ctx); err != nil {
			a.Log.Warn("feishu: transcriber close", "err", err)
		}
	}

	// Preserve the caller's deadline while draining accepted chat tasks.
	var queueErr error
	if a.chatQueue != nil {
		queueErr = a.chatQueue.CloseContext(ctx)
	}

	// Drain conn pool — ConnPool manages its own lock, no deadlock with FeishuConn.Close().
	conns := a.DrainConns()

	a.mu.Lock()
	a.CloseSharedState()
	if a.rateLimiter != nil {
		a.rateLimiter.Stop()
		a.rateLimiter = nil
	}
	a.mu.Unlock()

	// Close conns outside lock to prevent deadlock with FeishuConn.Close().
	for _, conn := range conns {
		_ = conn.Close()
	}

	return queueErr
}

func controlFeedbackMessageCN(action events.ControlAction) string {
	return messaging.ControlFeedbackMessage(action, messaging.ControlFeedbackCN, "✅ 已完成。")
}

// replyOrSend replies to a message if msgID is set, otherwise sends directly to chat.
func (a *Adapter) replyOrSend(ctx context.Context, msgID, chatID, text string) error {
	if msgID != "" {
		return a.replyMessage(ctx, msgID, text, false)
	}
	return a.sendTextMessage(ctx, chatID, text)
}

// SendProactiveMessage delivers a system-originated message to a Feishu chat
// or user. When chat_id and message_id are present, it replies to that message.
func (a *Adapter) SendProactiveMessage(ctx context.Context, text string, platformKey map[string]string) error {
	chatID := platformKey["chat_id"]
	if chatID != "" {
		return a.replyOrSend(ctx, platformKey["message_id"], chatID, messaging.SanitizeText(text))
	}
	openID := platformKey["open_id"]
	if openID == "" {
		return fmt.Errorf("feishu: missing chat_id or open_id in platform_key")
	}
	return a.sendTextMessageTo(ctx, receiveIDTypeOpenID, openID, messaging.SanitizeText(text))
}

// SendCronResult delivers a cron job result to a Feishu chat.
func (a *Adapter) SendCronResult(ctx context.Context, text string, platformKey map[string]string) error {
	return a.SendProactiveMessage(ctx, text, platformKey)
}

func (a *Adapter) sendTextMessage(ctx context.Context, chatID, text string) error {
	return a.sendTextMessageTo(ctx, receiveIDTypeChatID, chatID, text)
}

func (a *Adapter) sendTextMessageTo(ctx context.Context, receiveIDType, receiveID, text string) error {
	if a.larkClient == nil {
		return fmt.Errorf("feishu: lark client not initialized")
	}

	cardJSON := buildCardContent(text, cardHeader{Title: a.resolveDisplayName()})
	a.Log.Debug("feishu: sending card message", "receive_id_type", receiveIDType, "content_len", len(cardJSON))

	_, err := larkCreateMessageTo(ctx, a.larkClient, receiveIDType, receiveID, cardJSON)
	if err != nil {
		return fmt.Errorf("feishu: send message: %w", err)
	}
	a.Log.Debug("feishu: message sent", "receive_id_type", receiveIDType)
	return nil
}

//nolint:unparam // replyInThread reserved for future thread reply support
func (a *Adapter) replyMessage(ctx context.Context, messageID, content string, replyInThread bool) error {
	cardJSON := buildCardContent(content, cardHeader{Title: a.resolveDisplayName()})
	preview := cardJSON
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	a.Log.Debug("feishu: sending reply card", "msg_id", messageID, "content_len", len(cardJSON), "content_preview", preview)
	if err := a.doReplyCard(ctx, messageID, cardJSON, replyInThread); err != nil {
		return err
	}
	a.Log.Debug("feishu: reply message sent", "msg_id", messageID, "content_len", len(content))
	return nil
}

func (a *Adapter) doReplyCard(ctx context.Context, messageID, cardJSON string, _ bool) error {
	if a.larkClient == nil {
		return fmt.Errorf("feishu: lark client not initialized")
	}
	_, err := larkReplyMessage(ctx, a.larkClient, messageID, cardJSON)
	if err != nil {
		return fmt.Errorf("feishu: reply card: %w", err)
	}
	return nil
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

type textContent struct {
	Text string `json:"text"`
}

// buildCardContent builds a Feishu interactive card JSON using CardKit v2 format.
func buildCardContent(text string, header cardHeader) string {
	return buildCard(header,
		map[string]any{"wide_screen_mode": true},
		[]map[string]any{{"tag": "markdown", "content": text}},
	)
}

// encodeCard serializes a CardKit v2 card to JSON with HTML escaping disabled.
func encodeCard(card map[string]any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(card)
	return strings.TrimRight(buf.String(), "\n")
}

// buildTurnSummaryCard builds a CardKit v2 card with column_set rows matching
// Slack's TableBlock layout: two columns per row (label | value).
func buildTurnSummaryCard(d messaging.TurnSummaryData, header cardHeader) string {
	fields := d.Fields()
	if len(fields) == 0 {
		return ""
	}

	// Skip fields already shown in header tags (Turn, Model, Dir, Branch).
	skipLabels := map[string]bool{"🔄 Turn": true, "🤖 Model": true, "📂 Dir": true, "🌿 Branch": true}
	var elements []map[string]any
	for _, f := range fields {
		if !skipLabels[f.Label] {
			elements = append(elements, tableRow(f.Label, f.Value))
		}
	}
	if len(elements) == 0 {
		return ""
	}

	return buildCard(header, map[string]any{"wide_screen_mode": true}, elements)
}

// tableRow creates a CardKit v2 column_set element with two weighted columns.
func tableRow(label, value string) map[string]any {
	return map[string]any{
		"tag": "column_set",
		"columns": []map[string]any{
			{
				"tag":      "column",
				"width":    "weighted",
				"weight":   1,
				"elements": []map[string]any{{"tag": "markdown", "content": "**" + label + "**"}},
			},
			{
				"tag":      "column",
				"width":    "weighted",
				"weight":   3,
				"elements": []map[string]any{{"tag": "markdown", "content": value}},
			},
		},
	}
}

// formatSecurityError converts technical security errors into user-friendly messages.
func formatSecurityError(err error) string {
	return messaging.FormatSecurityError(err, messaging.SecurityMessagesCN)
}

func extractTextFromContent(content string) string {
	if content == "" {
		return ""
	}
	var tc textContent
	if err := json.Unmarshal([]byte(content), &tc); err != nil {
		return ""
	}
	return tc.Text
}

const (
	feishuMediaCleanupInterval = 6 * time.Hour
	feishuMediaTTL             = 24 * time.Hour
)

// feishuMediaSubdirs lists the media subdirectories owned by the Feishu adapter.
// This excludes adapter-owned subdirectories like "slack/" to prevent cross-adapter cleanup.
var feishuMediaSubdirs = []string{"images", "files", "audios", "videos", "stickers"}

// cleanupMedia periodically removes old media files downloaded from Feishu.
// Only cleans Feishu-owned subdirectories to avoid interfering with the Slack adapter's cleanup.
func (a *Adapter) cleanupMedia(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			a.Log.Error("feishu: panic in cleanupMedia", "panic", r, "stack", string(debug.Stack()))
		}
	}()

	// Run immediate cleanup on startup to handle stale files from previous runs.
	a.doCleanupMedia()

	ticker := time.NewTicker(feishuMediaCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.doCleanupMedia()
		}
	}
}

func (a *Adapter) doCleanupMedia() {
	mediaBase := filepath.Join(config.TempBaseDir(), "media")
	for _, sub := range feishuMediaSubdirs {
		a.cleanupMediaInDir(filepath.Join(mediaBase, sub))
	}
}

func (a *Adapter) cleanupMediaInDir(dir string) {
	var deleted, freedBytes int64
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && time.Since(info.ModTime()) > feishuMediaTTL {
			if err := os.Remove(path); err != nil {
				a.Log.Warn("feishu: failed to remove old media file", "path", path, "err", err)
				return nil
			}
			deleted++
			freedBytes += info.Size()
		}
		return nil
	})
	// Only log when work was done; an empty pass is routine noise.
	if deleted > 0 {
		a.Log.Debug("feishu: cleaned up old media files",
			"dir", dir, "deleted", deleted, "freed_bytes", freedBytes)
	}
}
