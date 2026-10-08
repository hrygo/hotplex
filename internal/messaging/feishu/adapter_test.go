package feishu

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/pkg/events"
)

func TestExtractResponseText_NilEnvelope(t *testing.T) {
	t.Parallel()
	_, ok := messaging.ExtractResponseText(nil)
	require.False(t, ok)
}

func TestExtractResponseText_StringData(t *testing.T) {
	t.Parallel()
	env := &events.Envelope{
		Event: events.Event{
			Type: "text",
			Data: "hello world",
		},
	}
	text, ok := messaging.ExtractResponseText(env)
	require.True(t, ok)
	require.Equal(t, "hello world", text)
}

func TestExtractResponseText_MessageDeltaData(t *testing.T) {
	t.Parallel()
	env := &events.Envelope{
		Event: events.Event{
			Type: events.MessageDelta,
			Data: events.MessageDeltaData{
				Content: "streaming content",
			},
		},
	}
	text, ok := messaging.ExtractResponseText(env)
	require.True(t, ok)
	require.Equal(t, "streaming content", text)
}

func TestExtractResponseText_MapData(t *testing.T) {
	t.Parallel()
	env := &events.Envelope{
		Event: events.Event{
			Type: "text",
			Data: map[string]any{
				"content": "map content",
			},
		},
	}
	text, ok := messaging.ExtractResponseText(env)
	require.True(t, ok)
	require.Equal(t, "map content", text)
}

func TestExtractResponseText_DoneEvent(t *testing.T) {
	t.Parallel()
	env := &events.Envelope{
		Event: events.Event{
			Type: "done",
			Data: events.DoneData{Success: true},
		},
	}
	_, ok := messaging.ExtractResponseText(env)
	require.False(t, ok)
}

func TestExtractResponseText_RawData(t *testing.T) {
	t.Parallel()
	env := &events.Envelope{
		Event: events.Event{
			Type: "raw",
			Data: events.RawData{
				Raw: map[string]any{
					"text": "raw text",
				},
			},
		},
	}
	text, ok := messaging.ExtractResponseText(env)
	require.True(t, ok)
	require.Equal(t, "raw text", text)
}

func TestFeishuConn_WriteCtx_NilEnvelope(t *testing.T) {
	t.Parallel()
	adapter := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*FeishuConn]{
			PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Dedup: messaging.NewDedup(100, 12*time.Hour)},
			ConnPool:        messaging.NewConnPool[*FeishuConn](nil),
		},
	}
	conn := NewFeishuConn(adapter, "test_chat", "", "")

	err := conn.WriteCtx(context.Background(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil envelope")
}

func TestAdapter_ConfigureWith(t *testing.T) {
	t.Parallel()
	a := &Adapter{}
	err := a.ConfigureWith(messaging.AdapterConfig{
		Extras: map[string]any{
			"app_id":     "app123",
			"app_secret": "secret456",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "app123", a.appID)
	require.Equal(t, "secret456", a.appSecret)
	require.Equal(t, messaging.PlatformFeishu, a.Platform())
}

func TestCleanupMedia_UsesConfiguredRetention(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*FeishuConn]{
			PlatformAdapter: messaging.PlatformAdapter{Log: logger},
		},
	}
	require.NoError(t, a.ConfigureWith(messaging.AdapterConfig{
		Extras: map[string]any{"media_retention": 2 * time.Hour},
	}))

	tmpDir := t.TempDir()
	expiredFile := filepath.Join(tmpDir, "expired.txt")
	require.NoError(t, os.WriteFile(expiredFile, []byte("expired"), 0o600))
	oldTime := time.Now().Add(-3 * time.Hour)
	require.NoError(t, os.Chtimes(expiredFile, oldTime, oldTime))

	a.cleanupMediaInDir(tmpDir)

	require.NoFileExists(t, expiredFile)
}

func TestAdapter_Start_MissingCredentials(t *testing.T) {
	t.Parallel()
	a := &Adapter{}
	err := a.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "appID and appSecret required")
}

func TestAdapter_Close(t *testing.T) {
	t.Parallel()
	a := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*FeishuConn]{
			PlatformAdapter: messaging.PlatformAdapter{
				Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
				Dedup: messaging.NewDedup(100, 12*60*60*1e9),
			},
			ConnPool: messaging.NewConnPool[*FeishuConn](nil),
		},
	}
	require.NoError(t, a.Close(context.Background()))
	require.True(t, a.ConnPool.IsClosed())
	require.Nil(t, a.Dedup)
}

func TestDedup_TryRecord(t *testing.T) {
	t.Parallel()
	d := messaging.NewDedup(100, 12*60*60*1e9)
	require.True(t, d.TryRecord("msg1"))
	require.False(t, d.TryRecord("msg1"))
	require.True(t, d.TryRecord("msg2"))
}

func TestDedup_FIFOEviction(t *testing.T) {
	t.Parallel()
	d := messaging.NewDedup(2, 12*60*60*1e9)
	require.True(t, d.TryRecord("a"))
	require.True(t, d.TryRecord("b"))
	require.True(t, d.TryRecord("c"))  // evicts "a"
	require.True(t, d.TryRecord("a"))  // re-accepted after eviction
	require.False(t, d.TryRecord("a")) // duplicate
}

func TestIsAbortCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"stop", true},
		{"Stop", true},
		{"Stop.", true},
		{"stop!", true},
		{"取消", true},
		{"please stop", true},
		{"hello", false},
		{"stopping", false},
		{"stopped", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			require.Equal(t, tt.want, messaging.IsAbortCommand(tt.input))
		})
	}
}

func TestResolveMentions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		text     string
		mentions []*mentionStub
		botID    string
		want     string
	}{
		{
			name:  "no mentions",
			text:  "hello world",
			botID: "",
			want:  "hello world",
		},
		{
			name:     "replace user mention",
			text:     "hello @_user_1 there",
			mentions: []*mentionStub{{key: "@_user_1", openID: "ou_123", name: "Alice"}},
			botID:    "",
			want:     "hello @Alice there",
		},
		{
			name:     "strip bot self-mention",
			text:     "@_user_1 @_user_2",
			mentions: []*mentionStub{{key: "@_user_1", openID: "bot_abc", name: "Bot"}, {key: "@_user_2", openID: "ou_456", name: "Bob"}},
			botID:    "bot_abc",
			want:     "@Bob",
		},
		{
			name:     "strip bot self-mention preserves surrounding text",
			text:     "hey @_user_1 @_user_2",
			mentions: []*mentionStub{{key: "@_user_1", openID: "bot_abc", name: "Bot"}, {key: "@_user_2", openID: "ou_456", name: "Bob"}},
			botID:    "bot_abc",
			want:     "hey @Bob",
		},
		{
			name:     "preserve @_all",
			text:     "@_all hello",
			mentions: []*mentionStub{},
			botID:    "",
			want:     "@_all hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var larkMentions []*larkim.MentionEvent
			for _, m := range tt.mentions {
				larkMentions = append(larkMentions, &larkim.MentionEvent{
					Key:  &m.key,
					Id:   &larkim.UserId{OpenId: &m.openID},
					Name: &m.name,
				})
			}
			got := ResolveMentions(tt.text, larkMentions, tt.botID)
			require.Equal(t, tt.want, got)
		})
	}
}

type mentionStub struct {
	key    string
	openID string
	name   string
}

func TestDownloadMedia_NilClient(t *testing.T) {
	t.Parallel()
	a := &Adapter{BaseAdapter: messaging.BaseAdapter[*FeishuConn]{PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}}
	_, err := a.downloadMedia(context.Background(), &MediaInfo{Type: "image", Key: "key", MessageID: "msg"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing lark client")
}

func TestDownloadMedia_NilMedia(t *testing.T) {
	t.Parallel()
	a := &Adapter{BaseAdapter: messaging.BaseAdapter[*FeishuConn]{PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}}
	_, err := a.downloadMedia(context.Background(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing lark client")
}

func TestDownloadMedia_EmptyMessageID(t *testing.T) {
	t.Parallel()
	a := &Adapter{BaseAdapter: messaging.BaseAdapter[*FeishuConn]{PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}}
	_, err := a.downloadMedia(context.Background(), &MediaInfo{Type: "image", Key: "key", MessageID: ""})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")
}

func TestDownloadMedia_EmptyKey(t *testing.T) {
	t.Parallel()
	a := &Adapter{BaseAdapter: messaging.BaseAdapter[*FeishuConn]{PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}}
	_, err := a.downloadMedia(context.Background(), &MediaInfo{Type: "image", Key: "", MessageID: "msg"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")
}

func TestDownloadMedia_AllTypes(t *testing.T) {
	t.Parallel()
	// Test that all media types are accepted by the function signature.
	// (Full download test requires mock lark client; this verifies type routing.)
	types := []string{"image", "file", "audio", "video", "sticker"}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			a := &Adapter{BaseAdapter: messaging.BaseAdapter[*FeishuConn]{PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}}
			_, err := a.downloadMedia(context.Background(), &MediaInfo{Type: typ, Key: "testkey", MessageID: "msg"})
			// Without a lark client, all types should fail with "missing lark client"
			require.Error(t, err)
			require.Contains(t, err.Error(), "missing lark client")
		})
	}
}

func TestAdapter_DoubleStartGuard(t *testing.T) {
	t.Parallel()

	a := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*FeishuConn]{
			PlatformAdapter: messaging.PlatformAdapter{
				Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
				Dedup: messaging.NewDedup(100, 12*60*60*1e9),
			},
			ConnPool: messaging.NewConnPool[*FeishuConn](nil),
		},
	}

	// First call: fails due to missing credentials (guard passes, validation fails)
	err1 := a.Start(context.Background())
	require.Error(t, err1)
	require.Contains(t, err1.Error(), "appID and appSecret required")

	// Second call: guard blocks, returns nil (no error, no panic)
	err2 := a.Start(context.Background())
	require.NoError(t, err2, "second Start() should return nil, not error")
}

func TestAdapter_CloseAfterSingleStart(t *testing.T) {
	t.Parallel()

	a := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*FeishuConn]{
			PlatformAdapter: messaging.PlatformAdapter{
				Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
				Dedup: messaging.NewDedup(100, 12*60*60*1e9),
			},
			ConnPool: messaging.NewConnPool[*FeishuConn](nil),
		},
	}

	// Start fails (no credentials), but guard is set
	_ = a.Start(context.Background())

	// Close should work without panic
	err := a.Close(context.Background())
	require.NoError(t, err)
}

func TestMediaExtByType(t *testing.T) {
	t.Parallel()
	// Verify fallback extensions for each media type.
	// These are tested via mimeExt, but also verify the constant map.
	tests := []struct {
		typ string
		ext string
	}{
		{"image", ".jpg"},
		{"file", ""},
		{"audio", ".opus"},
		{"video", ".mp4"},
		{"sticker", ".gif"},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			require.Equal(t, tt.ext, mediaExtByType[tt.typ])
		})
	}
}

func TestAdapter_MakeEnvelope(t *testing.T) {
	t.Parallel()

	br := messaging.NewBridge(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		messaging.PlatformFeishu,
		nil, nil, nil,
		"claude_code", "", "/tmp/hotplex/workspace",
		"",
	)

	a := &Adapter{botOpenID: "ou_bot123"}
	err := a.ConfigureWith(messaging.AdapterConfig{Bridge: br})
	require.NoError(t, err)

	env := a.makeEnvelope("oc_chat1", "msg_001", "ou_user1", "飞书消息", "")

	require.NotNil(t, env)
	require.Equal(t, "ou_user1", env.OwnerID)
	require.NotEmpty(t, env.SessionID)

	data, ok := env.Event.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "飞书消息", data["content"])
}

func TestAdapter_MakeEnvelope_CustomWorkDir(t *testing.T) {
	t.Parallel()

	br := messaging.NewBridge(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		messaging.PlatformFeishu,
		nil, nil, nil,
		"claude_code", "", "/default",
		"",
	)

	a := &Adapter{botOpenID: "ou_bot123"}
	err := a.ConfigureWith(messaging.AdapterConfig{Bridge: br})
	require.NoError(t, err)

	env1 := a.makeEnvelope("oc_chat1", "", "ou_user1", "hi", "/custom")
	env2 := a.makeEnvelope("oc_chat1", "", "ou_user1", "hi", "")

	require.NotEqual(t, env1.SessionID, env2.SessionID)
}
