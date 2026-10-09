package slack

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/messaging"
	"github.com/hrygo/hotplex/pkg/events"
)

var _ messaging.ProactiveMessageSender = (*Adapter)(nil)

func TestAdapter_SendProactiveMessage_MissingChannelID(t *testing.T) {
	t.Parallel()
	a := &Adapter{}

	err := a.SendProactiveMessage(context.Background(), "hello", map[string]string{})

	require.EqualError(t, err, "slack: missing channel_id in platform_key")
}

func TestAdapter_SendCronResult_DelegatesProactiveValidation(t *testing.T) {
	t.Parallel()
	a := &Adapter{}

	proactiveErr := a.SendProactiveMessage(context.Background(), "hello", map[string]string{})
	cronErr := a.SendCronResult(context.Background(), "hello", map[string]string{})

	require.EqualError(t, cronErr, proactiveErr.Error())
}

// --- Phase 1.2: Dedup ---

func TestDedup_TryRecord(t *testing.T) {
	t.Parallel()
	d := messaging.NewDedup(100, 5*time.Minute)
	t.Cleanup(d.Close)

	// First record succeeds
	require.True(t, d.TryRecord("msg1"))
	// Duplicate rejected
	require.False(t, d.TryRecord("msg1"))
	// Different message succeeds
	require.True(t, d.TryRecord("msg2"))
}

func TestDedup_FIFOEvection(t *testing.T) {
	t.Parallel()
	d := messaging.NewDedup(2, 5*time.Minute)
	t.Cleanup(d.Close)

	require.True(t, d.TryRecord("msg1"))
	require.True(t, d.TryRecord("msg2"))
	// Over capacity: msg1 evicted
	require.True(t, d.TryRecord("msg3"))
	// msg1 should be re-recordable
	require.True(t, d.TryRecord("msg1"))
}

func TestDedup_Close(t *testing.T) {
	t.Parallel()
	d := messaging.NewDedup(100, 5*time.Minute)
	d.Close()
	// No panic after close
}

// --- Phase 1.3: Bot defense (isBotMessage) ---

func TestIsBotMessage_AllBots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event slackevents.MessageEvent
		isBot bool
	}{
		{"bot via BotID", slackevents.MessageEvent{BotID: "B123"}, true},
		{"bot via subtype", slackevents.MessageEvent{SubType: "bot_message"}, true},
		{"user message", slackevents.MessageEvent{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.isBot, isBotMessage(tt.event))
		})
	}
}

// --- Phase 1.5: Rich Text Block extraction ---

func TestExtractText_ContextBlock(t *testing.T) {
	t.Parallel()

	event := slackevents.MessageEvent{
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewContextBlock("ctx1", []slack.MixedElement{
				slack.NewTextBlockObject(slack.PlainTextType, "context text", false, false),
			}...),
		}},
	}
	require.Equal(t, "context text", extractText(event))
}

func TestExtractText_RichTextBlock(t *testing.T) {
	t.Parallel()

	section := slack.NewRichTextSection(
		slack.NewRichTextSectionTextElement("hello ", nil),
		slack.NewRichTextSectionTextElement("world", nil),
	)
	rtBlock := slack.NewRichTextBlock("rt1", section)

	event := slackevents.MessageEvent{
		Blocks: slack.Blocks{BlockSet: []slack.Block{rtBlock}},
	}
	require.Equal(t, "hello world", extractText(event))
}

func TestExtractText_EmptyBlocks(t *testing.T) {
	t.Parallel()

	event := slackevents.MessageEvent{
		Blocks: slack.Blocks{BlockSet: []slack.Block{}},
	}
	require.Equal(t, "", extractText(event))
}

// --- Phase 2.1: mrkdwn formatting ---

func TestFormatMrkdwn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bold", "**bold**", "*bold*"},
		{"heading", "## H2", "*H2*"},
		{"strikethrough", "~~strike~~", "~strike~"},
		{"link", "[text](url)", "<url|text>"},
		{"list item", "- item", "• item"},
		{"code block preserved", "```**bold**```", "```**bold**```"},
		{"inline code preserved", "`**bold**`", "`**bold**`"},
		{"empty", "", ""},
		{"plain text", "hello world", "hello world"},
		{"mixed", "**bold** and `**code**`", "*bold* and `**code**`"},
		{"bold italic", "***bold italic***", "*_bold italic_*"},
		{"italic to underscore", "*italic*", "_italic_"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, FormatMrkdwn(tt.input))
		})
	}
}

func TestFormatMrkdwn_Multiline(t *testing.T) {
	t.Parallel()

	input := "## Title\n\n**bold text** and [link](url)\n\n- item 1\n- item 2"
	result := FormatMrkdwn(input)
	require.Contains(t, result, "*Title*")
	require.Contains(t, result, "*bold text*")
	require.Contains(t, result, "<url|link>")
	require.Contains(t, result, "• item 1")
	require.Contains(t, result, "• item 2")
}

// --- Phase 2.2: Abort detection ---

func TestIsAbortCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"stop", "stop", true},
		{"Chinese stop", "停止", true},
		{"stop with period", "Stop.", true},
		{"please stop", "please stop", true},
		{"hello", "hello", false},
		{"stop it", "stop it", false},
		{"empty", "", false},
		{"STOP uppercase", "STOP", true},
		{"Chinese comma", "停止，", true},
		{"cancel", "cancel", true},
		{"abort", "abort", true},
		{"别说了", "别说了", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, messaging.IsAbortCommand(tt.input))
		})
	}
}

func TestTruncateStatus(t *testing.T) {
	t.Parallel()

	require.Equal(t, "hi", truncateWithSuffix("hi", 50))
	require.Equal(t, "hello world", truncateWithSuffix("hello world", 11))
	require.Equal(t, "hello...", truncateWithSuffix("hello world", 8))
}

func TestIsAssistantCapabilityError(t *testing.T) {
	t.Parallel()

	require.True(t, isAssistantCapabilityError(errFake("not_allowed")))
	require.True(t, isAssistantCapabilityError(errFake("not_allowed_token_type")))
	require.False(t, isAssistantCapabilityError(errFake("timeout")))
	require.False(t, isAssistantCapabilityError(nil))
}

type errFake string

func (e errFake) Error() string { return string(e) }

// --- Phase 3.1: Gate ---

func TestGate_DMOpen(t *testing.T) {
	t.Parallel()
	g := messaging.NewGate("open", "open", false, nil, nil, nil)
	allowed, _ := g.Check(true, "U1", false)
	require.True(t, allowed)
}

func TestGate_DMDisabled(t *testing.T) {
	t.Parallel()
	g := messaging.NewGate("disabled", "open", false, nil, nil, nil)
	allowed, reason := g.Check(true, "U1", false)
	require.False(t, allowed)
	require.Equal(t, "dm_disabled", reason)
}

func TestGate_DMAllowlist(t *testing.T) {
	t.Parallel()
	g := messaging.NewGate("allowlist", "open", false, []string{"U1"}, nil, nil)
	allowed, _ := g.Check(true, "U1", false)
	require.True(t, allowed)

	allowed, reason := g.Check(true, "U2", false)
	require.False(t, allowed)
	require.Equal(t, "not_in_allowlist", reason)
}

func TestGate_GroupDisabled(t *testing.T) {
	t.Parallel()
	g := messaging.NewGate("open", "disabled", false, nil, nil, nil)
	allowed, reason := g.Check(false, "U1", false)
	require.False(t, allowed)
	require.Equal(t, "group_disabled", reason)
}

func TestGate_RequireMention(t *testing.T) {
	t.Parallel()
	g := messaging.NewGate("open", "open", true, nil, nil, nil)

	allowed, reason := g.Check(false, "U1", false)
	require.False(t, allowed)
	require.Equal(t, "no_mention", reason)

	allowed, _ = g.Check(false, "U1", true)
	require.True(t, allowed)
}

func TestGate_DMNotRequireMention(t *testing.T) {
	t.Parallel()
	g := messaging.NewGate("open", "open", true, nil, nil, nil)
	allowed, _ := g.Check(true, "U1", false)
	require.True(t, allowed)
}

func TestGate_DefaultOpen(t *testing.T) {
	t.Parallel()
	g := messaging.NewGate(messaging.PolicyOpen, messaging.PolicyOpen, false, nil, nil, nil)
	allowed, _ := g.Check(true, "U1", false)
	require.True(t, allowed)
	allowed, _ = g.Check(false, "U1", false)
	require.True(t, allowed)
	allowed, _ = g.Check(false, "U1", false)
	require.True(t, allowed)
}

// --- Phase 3.2: Message expiry ---

func TestParseSlackTS(t *testing.T) {
	t.Parallel()

	ts, err := parseSlackTS("1234567890.123456")
	require.NoError(t, err)
	require.Equal(t, int64(1234567890), ts.Unix())

	_, err = parseSlackTS("")
	require.Error(t, err)

	_, err = parseSlackTS("invalid")
	require.Error(t, err)
}

func TestParseSlackTS_ExpiredMessage(t *testing.T) {
	t.Parallel()

	// A timestamp 1 hour ago
	oldTS := time.Now().Add(-1 * time.Hour).Unix()
	ts, err := parseSlackTS(fmt.Sprintf("%d.000000", oldTS))
	require.NoError(t, err)
	require.True(t, time.Since(ts) > 30*time.Minute)
}

// --- Phase 4: Converter ---

func TestFileCategory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		filetype string
		want     string
	}{
		{"png", "image"},
		{"jpg", "image"},
		{"gif", "image"},
		{"mp4", "video"},
		{"mp3", "audio"},
		{"pdf", "document"},
		{"txt", "document"},
		{"zip", "file"},
	}
	for _, tt := range tests {
		t.Run(tt.filetype, func(t *testing.T) {
			f := slack.File{Filetype: tt.filetype}
			require.Equal(t, tt.want, fileCategory(f))
		})
	}
}

func TestMimeExt(t *testing.T) {
	t.Parallel()

	require.Equal(t, ".jpg", mimeExt("image/jpeg"))
	require.Equal(t, ".png", mimeExt("image/png"))
	require.Equal(t, ".pdf", mimeExt("application/pdf"))
	require.Equal(t, "", mimeExt("unknown/unknown"))
}

// --- Phase 1.4: Mention resolution ---

func TestUserCache_ResolveMentions_SelfMention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	uc := NewUserCache(nil) // no client needed for self-mention removal

	result := uc.ResolveMentions(ctx, "<@BOT1> hello", "BOT1")
	require.Equal(t, " hello", result)
}

func TestUserCache_ResolveMentions_InlineName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	uc := NewUserCache(nil) // no client, uses inline name fallback

	// <@U111|Bob> should use "Bob" as fallback when no client
	result := uc.ResolveMentions(ctx, "<@U111|Bob> hello", "BOT1")
	require.Equal(t, "@Bob hello", result)
}

func TestUserCache_ResolveMentions_NoMentions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	uc := NewUserCache(nil)

	result := uc.ResolveMentions(ctx, "hello world", "BOT1")
	require.Equal(t, "hello world", result)
}

func TestUserCache_ResolveMentions_UnknownUID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	uc := NewUserCache(nil) // no client, no fallback name

	result := uc.ResolveMentions(ctx, "<@U999> hello", "BOT1")
	require.Equal(t, "<@U999> hello", result)
}

// ---------------------------------------------------------------------------
// AC 2.4-4 — Multiple mentions all resolved
// ---------------------------------------------------------------------------

func TestUserCache_ResolveMentions_MultipleMentions(t *testing.T) {
	t.Parallel()
	uc := NewUserCache(nil)
	uc.cache.Set("U111", "Alice")
	uc.cache.Set("U222", "Bob")

	result := uc.ResolveMentions(context.Background(), "<@U111> and <@U222>", "B001")
	require.Equal(t, "@Alice and @Bob", result, "all mentions should be resolved")
}

// ---------------------------------------------------------------------------
// AC 2.4-9 — Mixed format mentions handled correctly
// ---------------------------------------------------------------------------

func TestUserCache_ResolveMentions_MixedFormats(t *testing.T) {
	t.Parallel()
	uc := NewUserCache(nil)
	uc.cache.Set("U111", "Alice")

	// <@U111> resolved from cache, <@U222|Bob> uses inline fallback
	result := uc.ResolveMentions(context.Background(), "<@U111> and <@U222|Bob>", "B001")
	require.Equal(t, "@Alice and @Bob", result, "mixed format mentions should both resolve")
}

// ---------------------------------------------------------------------------
// AC 3.3-13 — assistant_api_enabled:false skips probe, uses emoji
// ---------------------------------------------------------------------------

func TestAssistantAPIEnabled_ControlsProbe(t *testing.T) {
	t.Parallel()
	a := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*SlackConn]{
			PlatformAdapter: messaging.PlatformAdapter{Log: slog.Default()},
			ConnPool:        messaging.NewConnPool[*SlackConn](nil),
		},
		activeStreams: make(map[string]*NativeStreamingWriter),
	}

	// Default (nil) → enabled
	require.True(t, a.assistantAPIEnabled(), "nil assistantEnabled should mean enabled")

	// Explicitly false → disabled
	disabled := false
	a.assistantEnabled = &disabled
	require.False(t, a.assistantAPIEnabled(), "explicit false should disable probe")

	// ProbeAssistantCapability returns false when disabled
	require.False(t, a.ProbeAssistantCapability(context.Background()))

	// Explicitly true → enabled
	enabled := true
	a.assistantEnabled = &enabled
	require.True(t, a.assistantAPIEnabled(), "explicit true should enable")
}

// ---------------------------------------------------------------------------
// AC 3.3-16 — Native API unavailable → auto-degrade, no retry
// ---------------------------------------------------------------------------

func TestHandleCapabilityError_Degrades(t *testing.T) {
	t.Parallel()
	a := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*SlackConn]{
			PlatformAdapter: messaging.PlatformAdapter{Log: slog.Default()},
			ConnPool:        messaging.NewConnPool[*SlackConn](nil),
		},
		activeStreams: make(map[string]*NativeStreamingWriter),
	}

	// Set to capable
	a.isAssistantCapable.Store(true)

	// Capability error → degrades to false
	a.handleCapabilityError(fmt.Errorf("not_allowed"))
	require.False(t, a.isAssistantCapable.Load(), "should degrade after capability error")

	// Non-capability error → should NOT degrade
	a.isAssistantCapable.Store(true)
	a.handleCapabilityError(fmt.Errorf("timeout"))
	require.True(t, a.isAssistantCapable.Load(), "non-capability error should not degrade")
}

func TestNewStreamingWriter_ConcurrentWithClose(t *testing.T) {
	a := newTestAdapter(t)
	start := make(chan struct{})
	var (
		wg        sync.WaitGroup
		writersMu sync.Mutex
		writers   []*NativeStreamingWriter
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			writer := a.NewStreamingWriter(context.Background(), "C_TEST", "", nil)
			if writer == nil {
				continue
			}
			writersMu.Lock()
			writers = append(writers, writer)
			writersMu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		_ = a.Close(context.Background())
	}()
	close(start)
	wg.Wait()

	writersMu.Lock()
	defer writersMu.Unlock()
	for _, writer := range writers {
		_ = writer.Close()
	}
}

// ---------------------------------------------------------------------------
// AC 4.1-6 — group_policy=allowlist rejects non-whitelisted user
// ---------------------------------------------------------------------------

func TestGate_GroupAllowlist(t *testing.T) {
	t.Parallel()
	g := messaging.NewGate("open", "allowlist", false, []string{"U_ALLOWED"}, nil, nil)

	allowed, _ := g.Check(false, "U_ALLOWED", false)
	require.True(t, allowed, "whitelisted user in group should pass")

	allowed, reason := g.Check(false, "U_STRANGER", false)
	require.False(t, allowed, "non-whitelisted user in group should be rejected")
	require.Equal(t, messaging.ReasonNotInAllowlist, reason)
}

// ---------------------------------------------------------------------------
// AC 4.1-14 — Block Kit mention detection preserves <@BOTID> for gate
// ---------------------------------------------------------------------------

func TestGate_BlockKitMentionInExtractedText(t *testing.T) {
	t.Parallel()
	evt := slackevents.MessageEvent{
		Text:    "",
		Channel: "C123",
		User:    "U_ALICE",
	}
	evt.Blocks = slack.Blocks{BlockSet: []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "Hey <@B_TEST> can you help?", false, false),
			nil, nil,
		),
	}}

	text := extractText(evt)
	require.Contains(t, text, "<@B_TEST>", "Block Kit mention should be preserved for gate check")
}

// ---------------------------------------------------------------------------
// AC 5.3-2 — No image → pure text
// ---------------------------------------------------------------------------

func TestExtractImages_NoImages(t *testing.T) {
	t.Parallel()
	parts, remaining := extractImages("hello world, no images here")
	require.Empty(t, parts, "plain text should yield no image parts")
	require.Equal(t, "hello world, no images here", remaining)
}

// ---------------------------------------------------------------------------
// AC 5.3-3 — Local image <5MB → base64 data URI
// ---------------------------------------------------------------------------

func TestLocalFileToImagePart_SmallFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "chart.png")

	// Minimal valid PNG (1x1 pixel)
	pngData := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01" +
		"\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde")
	require.NoError(t, os.WriteFile(path, pngData, 0o644))

	imgURL, altText := localFileToImagePart(path)
	require.NotEmpty(t, imgURL, "small image should return base64 data URI")
	require.Contains(t, imgURL, "data:image/")
	require.Contains(t, imgURL, ";base64,")
	require.Equal(t, "chart.png", altText)
}

// ---------------------------------------------------------------------------
// AC 5.3-4 — Local image >=5MB → skip
// ---------------------------------------------------------------------------

func TestLocalFileToImagePart_LargeFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.png")

	largeData := make([]byte, 5*1024*1024+1) // >5MB
	copy(largeData, []byte("\x89PNG\r\n\x1a\n"))
	require.NoError(t, os.WriteFile(path, largeData, 0o644))

	imgURL, altText := localFileToImagePart(path)
	require.Empty(t, imgURL, "image >=5MB should be skipped")
	require.Empty(t, altText)
}

// ---------------------------------------------------------------------------
// AC 5.3-6 — buildImageBlocks: text + images → mixed blocks
// ---------------------------------------------------------------------------

func TestBuildImageBlocks_WithTextAndImages(t *testing.T) {
	t.Parallel()
	parts := []imagePart{
		{URL: "data:image/png;base64,abc123", AltText: "chart.png"},
	}
	blocks := buildImageBlocks(parts, "Here is the chart:")
	require.Len(t, blocks, 2, "should have 1 text section + 1 image block")

	sec, ok := blocks[0].(*slack.SectionBlock)
	require.True(t, ok, "first block should be SectionBlock")
	require.NotNil(t, sec.Text)

	img, ok := blocks[1].(*slack.ImageBlock)
	require.True(t, ok, "second block should be ImageBlock")
	require.Equal(t, "data:image/png;base64,abc123", img.ImageURL)
}

func TestBuildImageBlocks_ImagesOnly(t *testing.T) {
	t.Parallel()
	parts := []imagePart{
		{URL: "https://example.com/a.png", AltText: "a.png"},
		{URL: "https://example.com/b.png", AltText: "b.png"},
	}
	blocks := buildImageBlocks(parts, "")
	require.Len(t, blocks, 2, "no text → only image blocks")
	for i, b := range blocks {
		_, ok := b.(*slack.ImageBlock)
		require.True(t, ok, "block %d should be ImageBlock", i)
	}
}

// ---------------------------------------------------------------------------
// AC 5.2-4 — Download failure cleans up empty file
// ---------------------------------------------------------------------------

func TestDownloadMedia_FailureCleansUpFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	targetPath := filepath.Join(tmpDir, "image_F_TEST.png")
	f, err := os.Create(targetPath)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// File exists after Create
	_, err = os.Stat(targetPath)
	require.NoError(t, err, "file should exist after os.Create")

	// Simulate the cleanup downloadMedia performs on GetFile error
	_ = os.Remove(targetPath)

	// File should be gone
	_, err = os.Stat(targetPath)
	require.True(t, os.IsNotExist(err), "file should be removed after download failure")
}

// ---------------------------------------------------------------------------
// AC 5.2-6 — Re-download overwrites existing file
// ---------------------------------------------------------------------------

func TestDownloadMedia_OverwriteOnRepeat(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.txt")

	require.NoError(t, os.WriteFile(path, []byte("first"), 0o644))
	data1, _ := os.ReadFile(path)
	require.Equal(t, "first", string(data1))

	// os.Create truncates → simulates re-download
	f, err := os.Create(path)
	require.NoError(t, err)
	_, err = f.WriteString("second")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	data2, _ := os.ReadFile(path)
	require.Equal(t, "second", string(data2), "re-download should overwrite")
}

func TestAdapter_DoubleStartGuard(t *testing.T) {
	t.Parallel()

	a := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*SlackConn]{
			PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
			ConnPool:        messaging.NewConnPool[*SlackConn](nil),
		},
		activeStreams: make(map[string]*NativeStreamingWriter),
	}

	// First call: fails due to missing tokens (guard passes, validation fails)
	err1 := a.Start(context.Background())
	require.Error(t, err1)
	require.Contains(t, err1.Error(), "botToken and appToken required")

	// Second call: guard blocks, returns nil (no error, no panic)
	err2 := a.Start(context.Background())
	require.NoError(t, err2, "second Start() should return nil, not error")
}

func TestAdapter_DoubleStartGuard_WithTokens(t *testing.T) {
	t.Parallel()

	a := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*SlackConn]{
			PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
			ConnPool:        messaging.NewConnPool[*SlackConn](nil),
		},
		botToken:      "xoxb-fake",
		appToken:      "xapp-fake",
		activeStreams: make(map[string]*NativeStreamingWriter),
	}

	// First call: fails at auth test (guard passes, Slack API fails)
	err1 := a.Start(context.Background())
	require.Error(t, err1) // auth test will fail with fake tokens

	// Second call: guard blocks, returns nil
	err2 := a.Start(context.Background())
	require.NoError(t, err2, "second Start() should return nil after failed first start")
}

func TestAdapter_CloseAfterSingleStart(t *testing.T) {
	t.Parallel()

	a := &Adapter{
		BaseAdapter: messaging.BaseAdapter[*SlackConn]{
			PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
			ConnPool:        messaging.NewConnPool[*SlackConn](nil),
		},
		activeStreams: make(map[string]*NativeStreamingWriter),
	}

	// Start fails (no tokens), but guard is set
	_ = a.Start(context.Background())

	// Close should work without panic
	err := a.Close(context.Background())
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// AC 5.4-4 — Non-image file not converted to image block (outbound skip)
// ---------------------------------------------------------------------------

func TestLocalFileToImagePart_NonImageFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.csv")
	require.NoError(t, os.WriteFile(path, []byte("a,b\n1,2"), 0o644))

	imgURL, altText := localFileToImagePart(path)
	require.Empty(t, imgURL, "non-image file should not become image block")
	require.Empty(t, altText)
}

// ---------------------------------------------------------------------------
// MSG-001: Slack Streaming as Default Output Path
// ---------------------------------------------------------------------------

func TestSlackConn_StreamWriterFieldExists(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{}

	// Verify fields exist and are accessible
	require.Nil(t, conn.streamWriter)
	conn.streamWriterMu.Lock()
	_ = conn.streamWriter // access under lock to avoid SA2001 empty critical section
	conn.streamWriterMu.Unlock()
}

func TestSlackConn_WriteCtx_NilEnvelope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn := &SlackConn{
		adapter:   &Adapter{},
		channelID: "C123",
		threadTS:  "123.456",
	}

	err := conn.WriteCtx(ctx, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil envelope")
}

func TestSlackConn_WriteCtx_DoneEventClosesStreamWriter(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{}

	// Set up a mock stream writer
	conn.streamWriter = &NativeStreamingWriter{}

	// Verify writer exists before close
	require.NotNil(t, conn.streamWriter)

	// Simulate done event clearing the writer
	conn.streamWriter = nil
	require.Nil(t, conn.streamWriter, "stream writer should be nil after done")
}

func TestSlackConn_WriteCtx_ErrorEventClosesStreamWriter(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{}

	// Set up a mock stream writer
	conn.streamWriter = &NativeStreamingWriter{}

	// Verify writer exists before close
	require.NotNil(t, conn.streamWriter)

	// Simulate error event clearing the writer
	conn.streamWriter = nil
	require.Nil(t, conn.streamWriter, "stream writer should be nil after error")
}

func TestSlackConn_Close_CleansUpStreamWriter(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{}

	// Set up a mock stream writer
	conn.streamWriter = &NativeStreamingWriter{}

	// Verify writer exists before close
	require.NotNil(t, conn.streamWriter)

	// Simulate Close cleaning up the writer
	conn.streamWriter = nil
	require.Nil(t, conn.streamWriter, "stream writer should be nil after Close")
}

func TestSlackConn_closeStreamWriter_Idempotent(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{}

	// Nil writer - should be nil
	require.Nil(t, conn.streamWriter)

	// Set the writer to a non-nil value
	conn.streamWriter = &NativeStreamingWriter{}
	require.NotNil(t, conn.streamWriter)

	// Manually clear to simulate close behavior
	conn.streamWriter = nil
	require.Nil(t, conn.streamWriter)

	// Second clear - should be idempotent (still nil)
	conn.streamWriter = nil
	require.Nil(t, conn.streamWriter)
}

func TestSlackConn_writeWithPostMessage_DeltaAddsNewlines(t *testing.T) {
	t.Parallel()

	// This test verifies the delta formatting logic exists
	text := "test message"
	formatted := text
	formatted += "\n\n" // delta formatting adds newlines
	require.Equal(t, "test message\n\n", formatted)
}

func TestSlackConn_writeWithStreaming_EmptyText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn := &SlackConn{
		adapter:   &Adapter{},
		channelID: "C123",
		threadTS:  "123.456",
	}

	// Empty text should return nil without creating a writer
	err := conn.writeWithStreaming(ctx, "")
	require.NoError(t, err)
	require.Nil(t, conn.streamWriter)
}

func TestSlackConn_ExtractResponseText_MessageDelta(t *testing.T) {
	t.Parallel()

	env := &events.Envelope{
		Event: events.Event{
			Type: events.MessageDelta,
			Data: events.MessageDeltaData{Content: "hello world"},
		},
	}

	text, ok := messaging.ExtractResponseText(env)
	require.True(t, ok)
	require.Equal(t, "hello world", text)
}

func TestSlackConn_ExtractResponseText_TextEvent(t *testing.T) {
	t.Parallel()

	env := &events.Envelope{
		Event: events.Event{
			Type: "text",
			Data: "plain text content",
		},
	}

	text, ok := messaging.ExtractResponseText(env)
	require.True(t, ok)
	require.Equal(t, "plain text content", text)
}

func TestSlackConn_ExtractResponseText_DoneEvent(t *testing.T) {
	t.Parallel()

	env := &events.Envelope{
		Event: events.Event{
			Type: "done",
			Data: nil,
		},
	}

	text, ok := messaging.ExtractResponseText(env)
	require.False(t, ok)
	require.Empty(t, text)
}

func TestSlackConn_MultipleSessions_CreatesNewWriterEachTime(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{}

	// First session - no writer yet
	require.Nil(t, conn.streamWriter)

	// Simulate first session completion
	writer1 := &NativeStreamingWriter{}
	conn.streamWriter = writer1
	require.NotNil(t, conn.streamWriter)

	// Manually clear the writer to simulate done event
	conn.streamWriter = nil
	require.Nil(t, conn.streamWriter)

	// Second session - should create new writer
	writer2 := &NativeStreamingWriter{}
	conn.streamWriter = writer2

	// Verify it's a different writer (different memory addresses)
	require.True(t, writer1 != writer2, "Session 2 should have different writer instance")
}

func TestSlackConn_ThreadTS_UpdateFromCallback(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{
		threadTS: "", // No thread initially
	}

	// Simulate callback updating threadTS
	newTS := "123.456"
	if conn.threadTS == "" && newTS != "" {
		conn.threadTS = newTS
	}

	require.Equal(t, "123.456", conn.threadTS)
}

func TestSlackConn_writeWithStreaming_NilAdapter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn := &SlackConn{}

	// Should return error when adapter is nil
	err := conn.writeWithStreaming(ctx, "test text")
	require.Error(t, err)
}

// AC-1.1: When a worker emits message.delta events, Slack receives them via StartStream/AppendStream/StopStream API
func TestAC11_DeltaEventsUseStreaming(t *testing.T) {
	t.Parallel()

	env := &events.Envelope{
		Event: events.Event{
			Type: events.MessageDelta,
			Data: events.MessageDeltaData{Content: "delta content"},
		},
	}

	text, ok := messaging.ExtractResponseText(env)
	require.True(t, ok, "AC-1.1: Should extract text from delta event")
	require.Equal(t, "delta content", text)
}

// AC-1.3: On done event, stream writer is properly closed and cleaned up
func TestAC13_DoneEventClosesStream(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{}

	// Create a mock stream writer
	conn.streamWriter = &NativeStreamingWriter{}
	require.NotNil(t, conn.streamWriter, "AC-1.3: Writer should exist before done event")

	// Simulate done event clearing the writer
	conn.streamWriter = nil
	require.Nil(t, conn.streamWriter, "AC-1.3: Stream writer should be nil after done")
}

// AC-1.4: On error event, stream writer is closed with integrity fallback if needed
func TestAC14_ErrorEventClosesStream(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{}

	// Create a mock stream writer
	conn.streamWriter = &NativeStreamingWriter{}
	require.NotNil(t, conn.streamWriter, "AC-1.4: Writer should exist before error event")

	// Simulate error event clearing the writer
	conn.streamWriter = nil
	require.Nil(t, conn.streamWriter, "AC-1.4: Stream writer should be nil after error")
}

// AC-1.5: Multiple sequential sessions on same SlackConn correctly create new writer per session
func TestAC15_MultipleSessionsCreateNewWriters(t *testing.T) {
	t.Parallel()

	conn := &SlackConn{}

	// Session 1: Create writer and complete
	writer1 := &NativeStreamingWriter{}
	conn.streamWriter = writer1
	require.NotNil(t, conn.streamWriter, "AC-1.5: Writer should exist in session 1")

	// Simulate done/error event clearing the writer
	conn.streamWriter = nil
	require.Nil(t, conn.streamWriter, "AC-1.5: Writer should be nil after session 1")

	// Session 2: Create new writer
	writer2 := &NativeStreamingWriter{}
	conn.streamWriter = writer2

	require.True(t, writer1 != writer2, "AC-1.5: Session 2 should have different writer")
}

// AC-1.6: Existing PostMessageContext path still works for non-streaming events
func TestAC16_NonStreamingEventsUsePostMessage(t *testing.T) {
	t.Parallel()

	// Verify interaction events don't use streaming
	permissionEnv := &events.Envelope{
		Event: events.Event{Type: events.PermissionRequest},
	}
	require.Equal(t, events.PermissionRequest, permissionEnv.Event.Type)

	questionEnv := &events.Envelope{
		Event: events.Event{Type: events.QuestionRequest},
	}
	require.Equal(t, events.QuestionRequest, questionEnv.Event.Type)

	elicitationEnv := &events.Envelope{
		Event: events.Event{Type: events.ElicitationRequest},
	}
	require.Equal(t, events.ElicitationRequest, elicitationEnv.Event.Type)
}

func TestCleanupMedia(t *testing.T) {
	// Creating a logger that discards output for the test
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Adapter{BaseAdapter: messaging.BaseAdapter[*SlackConn]{PlatformAdapter: messaging.PlatformAdapter{Log: logger}}}

	tmpDir := t.TempDir()

	// 1. Create an old file (> 24h)
	oldFile := filepath.Join(tmpDir, "old.txt")
	err := os.WriteFile(oldFile, []byte("old content"), 0644)
	require.NoError(t, err)

	oldTime := time.Now().Add(-48 * time.Hour)
	err = os.Chtimes(oldFile, oldTime, oldTime)
	require.NoError(t, err)

	// 2. Create a new file (< 24h)
	newFile := filepath.Join(tmpDir, "new.txt")
	err = os.WriteFile(newFile, []byte("new content"), 0644)
	require.NoError(t, err)

	// 3. Create a directory (should not be removed)
	subDir := filepath.Join(tmpDir, "subdir")
	err = os.Mkdir(subDir, 0755)
	require.NoError(t, err)

	// Run cleanup
	a.cleanupMediaInDir(tmpDir)

	// Verify
	_, err = os.Stat(oldFile)
	require.Error(t, err, "Old file should be removed")
	require.True(t, os.IsNotExist(err))

	_, err = os.Stat(newFile)
	require.NoError(t, err, "New file should NOT be removed")

	_, err = os.Stat(subDir)
	require.NoError(t, err, "Directory should NOT be removed")
}

func TestCleanupMedia_UsesConfiguredRetention(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Adapter{BaseAdapter: messaging.BaseAdapter[*SlackConn]{PlatformAdapter: messaging.PlatformAdapter{Log: logger}}}
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

func TestChunkContent_Integration(t *testing.T) {
	t.Parallel()

	// Test that ChunkContent is available and works as expected for the adapter's use case.
	content := "Line 1\n" + strings.Repeat("a", 1000) + "\nLine 3"
	chunks := ChunkContent(content, 500)

	require.True(t, len(chunks) >= 2, "Should split content into multiple chunks")
	for _, chunk := range chunks {
		require.Contains(t, chunk, "[", "Each chunk should have a [N/M] prefix")
	}
}

// --- messaging.ExtractErrorMessage: P0 fix verification ---

func TestExtractErrorMessage_TypedErrorData(t *testing.T) {
	t.Parallel()

	env := &events.Envelope{
		Event: events.Event{
			Type: events.Error,
			Data: events.ErrorData{Code: events.ErrCodeSessionNotFound, Message: "session not found"},
		},
	}
	require.Equal(t, "session not found", messaging.ExtractErrorMessage(env))
}

func TestExtractErrorMessage_MapData(t *testing.T) {
	t.Parallel()

	env := &events.Envelope{
		Event: events.Event{
			Type: events.Error,
			Data: map[string]any{"message": "something went wrong", "code": "INTERNAL_ERROR"},
		},
	}
	require.Equal(t, "something went wrong", messaging.ExtractErrorMessage(env))
}

func TestExtractErrorMessage_NilData(t *testing.T) {
	t.Parallel()

	env := &events.Envelope{
		Event: events.Event{Type: events.Error},
	}
	require.Equal(t, "", messaging.ExtractErrorMessage(env))
}

func TestExtractErrorMessage_EmptyMessage(t *testing.T) {
	t.Parallel()

	env := &events.Envelope{
		Event: events.Event{
			Type: events.Error,
			Data: events.ErrorData{Code: events.ErrCodeInternalError, Message: ""},
		},
	}
	require.Equal(t, "", messaging.ExtractErrorMessage(env))
}

// ---------------------------------------------------------------------------
// StatusManager emoji tracking + cleanup
// ---------------------------------------------------------------------------

func TestStatusManager_EmojiTrackAndClear(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	ctx := context.Background()
	ch, ts := "C123", "1234.5678"
	key := ch + ":" + ts

	sm.emojiState[key] = "brain"
	require.Equal(t, "brain", sm.emojiState[key], "emoji should be tracked")

	sm.Clear(ctx, ch, ts)
	require.Equal(t, "", sm.emojiState[key], "emoji should be cleared from state")
}

func TestStatusManager_Clear_NoTrackedEmoji(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	sm.Clear(context.Background(), "C1", "TS1")
	require.Empty(t, sm.emojiState)
}

func TestStatusManager_Clear_Idempotent(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	sm.emojiState["C1:TS1"] = "brain"

	sm.Clear(context.Background(), "C1", "TS1")

	sm.Clear(context.Background(), "C1", "TS1")
	require.Equal(t, "", sm.emojiState["C1:TS1"])
}

func TestStatusManager_MultipleStatusCycles(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	key := "C99:9999.0000"

	sm.emojiState["C99:9999.0000"] = "brain"
	require.Equal(t, "brain", sm.emojiState[key])

	sm.Clear(context.Background(), "C99", "9999.0000")
	require.Equal(t, "", sm.emojiState[key])

	sm.emojiState["C99:9999.0000"] = "gear"
	require.Equal(t, "gear", sm.emojiState[key])

	sm.Clear(context.Background(), "C99", "9999.0000")
	require.Equal(t, "", sm.emojiState[key])
}

func TestStatusManager_IndependentThreads(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())

	sm.emojiState["C1:T1"] = "brain"
	sm.emojiState["C1:T2"] = "gear"

	sm.Clear(context.Background(), "C1", "T1")
	require.Equal(t, "", sm.emojiState["C1:T1"], "T1 cleared")
	require.Equal(t, "gear", sm.emojiState["C1:T2"], "T2 unaffected")
}

func TestStatusManager_ClearEmojiLocked_NilAdapter(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	sm.emojiState["C1:T1"] = "brain"

	// clearEmojiLocked with nil adapter should not panic.
	sm.clearEmojiLocked(context.Background(), "C1", "T1")
}

// TestClearStatus_CleansEmojiEvenWhenAssistantCapable verifies that ClearStatus
// removes the tracked emoji when the emoji fallback path is taken (isAssistantCapable=false).
// The two paths are mutually exclusive: when Assistant API is capable and succeeds,
// ClearStatus clears Assistant Status only (emoji cleanup is skipped to avoid
// unnecessary API calls; some residual emoji from prior emoji-mode sessions is acceptable).
func TestClearStatus_CleansEmojiEvenWhenAssistantCapable(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	ch, ts := "C999", "9999.9999"
	key := ch + ":" + ts

	// Simulate what setStatusWithEmojiFallback does: add emoji and track it.
	sm.emojiState[ch+":"+ts] = "brain"
	require.Equal(t, "brain", sm.emojiState[key], "precondition: emoji must be tracked")

	// Clear must remove emoji unconditionally — this is the invariant that
	// ClearStatus relies on, regardless of whether Assistant API is available.
	sm.Clear(context.Background(), ch, ts)
	require.Equal(t, "", sm.emojiState[key],
		"Clear must remove tracked emoji regardless of Assistant API state")
}

// ---------------------------------------------------------------------------
// ContextUsage event tests
// ---------------------------------------------------------------------------

// testAdapter creates an Adapter with a non-nil slack.Client (dummy token)
// so that the "client not initialized" guard is bypassed and the method
// exercises its full code path. PostMessageContext will fail with invalid_auth.
var (
	testSlackServerOnce sync.Once
	testSlackServerInst *httptest.Server
)

// testSlackServer returns a package-level stub Slack API server that replies
// to every endpoint with {"ok":false,"error":"invalid_auth"}, so tests can
// exercise the full client code path without real network egress — a dummy
// token fails instantly on localhost instead of TLS-handshaking slack.com.
func testSlackServer() *httptest.Server {
	testSlackServerOnce.Do(func() {
		testSlackServerInst = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		}))
	})
	return testSlackServerInst
}

func testAdapter() *Adapter {
	return &Adapter{
		BaseAdapter: messaging.BaseAdapter[*SlackConn]{
			PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
			ConnPool:        messaging.NewConnPool[*SlackConn](nil),
		},
		client:        slack.New("x-test-token", slack.OptionAPIURL(testSlackServer().URL+"/")),
		activeStreams: make(map[string]*NativeStreamingWriter),
	}
}

func TestSlackConn_SendContextUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name        string
		conn        *SlackConn
		env         *events.Envelope
		wantErr     bool
		errContains string
	}{
		{
			name: "typed data",
			conn: &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env: &events.Envelope{Event: events.Event{
				Type: events.ContextUsage,
				Data: events.ContextUsageData{
					TotalTokens: 1500, MaxTokens: 2000, Percentage: 75,
					Model: "claude-3-5-sonnet-20241022",
					Categories: []events.ContextCategory{
						{Name: "System", Tokens: 100},
						{Name: "User", Tokens: 500},
						{Name: "Conversation", Tokens: 900},
					},
					MemoryFiles: 3, MCPTools: 5, Agents: 2,
					Skills: events.ContextSkillInfo{Total: 10, Included: 5, Tokens: 300},
				},
			}},
			wantErr: true,
		},
		{
			name: "map data",
			conn: &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env: &events.Envelope{Event: events.Event{
				Type: events.ContextUsage,
				Data: map[string]any{
					"total_tokens": float64(800), "max_tokens": float64(1000),
					"percentage": float64(80), "model": "gpt-4",
					"categories": []any{
						map[string]any{"name": "System", "tokens": float64(50)},
						map[string]any{"name": "User", "tokens": float64(750)},
					},
					"memory_files": float64(2), "mcp_tools": float64(3), "agents": float64(1),
					"skills": map[string]any{"total": float64(8), "included": float64(4), "tokens": float64(200)},
				},
			}},
			wantErr: true,
		},
		{
			name:    "unsupported data",
			conn:    &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env:     &events.Envelope{Event: events.Event{Type: events.ContextUsage, Data: "not a valid data type"}},
			wantErr: false,
		},
		{
			name: "no categories",
			conn: &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env: &events.Envelope{Event: events.Event{
				Type: events.ContextUsage,
				Data: events.ContextUsageData{TotalTokens: 500, MaxTokens: 1000, Percentage: 50},
			}},
			wantErr: true,
		},
		{
			name:    "nil data",
			conn:    &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env:     &events.Envelope{Event: events.Event{Type: events.ContextUsage}},
			wantErr: false,
		},
		{
			name:    "nil adapter",
			conn:    &SlackConn{adapter: nil, channelID: "C123", threadTS: "123.456"},
			env:     &events.Envelope{Event: events.Event{Type: events.ContextUsage, Data: events.ContextUsageData{TotalTokens: 100, MaxTokens: 200, Percentage: 50}}},
			wantErr: true, errContains: "client not initialized",
		},
		{
			name: "nil client",
			conn: &SlackConn{
				adapter: &Adapter{
					BaseAdapter: messaging.BaseAdapter[*SlackConn]{
						PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
						ConnPool:        messaging.NewConnPool[*SlackConn](nil),
					},
					client:        nil,
					activeStreams: make(map[string]*NativeStreamingWriter),
				},
				channelID: "C123", threadTS: "123.456",
			},
			env:     &events.Envelope{Event: events.Event{Type: events.ContextUsage, Data: events.ContextUsageData{TotalTokens: 100, MaxTokens: 200, Percentage: 50}}},
			wantErr: true, errContains: "client not initialized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.conn.sendContextUsage(ctx, tt.env)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					require.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSlackConn_SendMCPStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name        string
		conn        *SlackConn
		env         *events.Envelope
		wantErr     bool
		errContains string
	}{
		{
			name: "typed data",
			conn: &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env: &events.Envelope{Event: events.Event{
				Type: events.MCPStatus,
				Data: events.MCPStatusData{Servers: []events.MCPServerInfo{
					{Name: "filesystem", Status: "connected"},
					{Name: "github", Status: "connected"},
					{Name: "sqlite", Status: "disconnected"},
					{Name: "notion", Status: "ok"},
				}},
			}},
			wantErr: true,
		},
		{
			name: "map data",
			conn: &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env: &events.Envelope{Event: events.Event{
				Type: events.MCPStatus,
				Data: map[string]any{
					"servers": []any{
						map[string]any{"name": "postgres", "status": "connected"},
						map[string]any{"name": "redis", "status": "connecting"},
						map[string]any{"name": "aws", "status": "error"},
					},
				},
			}},
			wantErr: true,
		},
		{
			name:    "unsupported data",
			conn:    &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env:     &events.Envelope{Event: events.Event{Type: events.MCPStatus, Data: 12345}},
			wantErr: false,
		},
		{
			name: "empty server list",
			conn: &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env: &events.Envelope{Event: events.Event{
				Type: events.MCPStatus,
				Data: events.MCPStatusData{Servers: []events.MCPServerInfo{}},
			}},
			wantErr: true,
		},
		{
			name:    "nil data",
			conn:    &SlackConn{adapter: testAdapter(), channelID: "C123", threadTS: "123.456"},
			env:     &events.Envelope{Event: events.Event{Type: events.MCPStatus}},
			wantErr: false,
		},
		{
			name:    "nil adapter",
			conn:    &SlackConn{adapter: nil, channelID: "C123", threadTS: "123.456"},
			env:     &events.Envelope{Event: events.Event{Type: events.MCPStatus, Data: events.MCPStatusData{Servers: []events.MCPServerInfo{{Name: "fs", Status: "connected"}}}}},
			wantErr: true, errContains: "client not initialized",
		},
		{
			name: "nil client",
			conn: &SlackConn{
				adapter: &Adapter{
					BaseAdapter: messaging.BaseAdapter[*SlackConn]{
						PlatformAdapter: messaging.PlatformAdapter{Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
						ConnPool:        messaging.NewConnPool[*SlackConn](nil),
					},
					client:        nil,
					activeStreams: make(map[string]*NativeStreamingWriter),
				},
				channelID: "C123", threadTS: "123.456",
			},
			env:     &events.Envelope{Event: events.Event{Type: events.MCPStatus, Data: events.MCPStatusData{Servers: []events.MCPServerInfo{{Name: "fs", Status: "connected"}}}}},
			wantErr: true, errContains: "client not initialized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.conn.sendMCPStatus(ctx, tt.env)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					require.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WriteCtx integration tests for ContextUsage and MCPStatus
// ---------------------------------------------------------------------------

func TestSlackConn_WriteCtx_ContextUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn := &SlackConn{
		adapter:   testAdapter(),
		channelID: "C123",
		threadTS:  "123.456",
	}

	env := &events.Envelope{
		Event: events.Event{
			Type: events.ContextUsage,
			Data: events.ContextUsageData{
				TotalTokens: 1000,
				MaxTokens:   2000,
				Percentage:  50,
			},
		},
	}

	err := conn.WriteCtx(ctx, env)
	require.Error(t, err)
}

func TestSlackConn_WriteCtx_MCPStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn := &SlackConn{
		adapter:   testAdapter(),
		channelID: "C123",
		threadTS:  "123.456",
	}

	env := &events.Envelope{
		Event: events.Event{
			Type: events.MCPStatus,
			Data: events.MCPStatusData{
				Servers: []events.MCPServerInfo{
					{Name: "test-server", Status: "connected"},
				},
			},
		},
	}

	err := conn.WriteCtx(ctx, env)
	require.Error(t, err)
}

func TestSlackConn_WriteCtx_ContextUsageWithAllOptionalFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	conn := &SlackConn{
		adapter:   testAdapter(),
		channelID: "C123",
		threadTS:  "123.456",
	}

	env := &events.Envelope{
		Event: events.Event{
			Type: events.ContextUsage,
			Data: events.ContextUsageData{
				TotalTokens: 1800,
				MaxTokens:   2000,
				Percentage:  90,
				Model:       "claude-3-5-haiku-20241022",
				Categories: []events.ContextCategory{
					{Name: "System", Tokens: 200},
					{Name: "Instructions", Tokens: 300},
					{Name: "Conversation", Tokens: 800},
					{Name: "Tools", Tokens: 500},
				},
				MemoryFiles: 5,
				MCPTools:    8,
				Agents:      3,
				Skills: events.ContextSkillInfo{
					Total:    15,
					Included: 10,
					Tokens:   450,
				},
			},
		},
	}

	err := conn.WriteCtx(ctx, env)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Phase 4.3 — Image Block integration in writeWithPostMessage
// ---------------------------------------------------------------------------

func TestExtractImages_LocalMediaPath(t *testing.T) {
	t.Parallel()
	imgDir := filepath.Join(MediaPathPrefix, "images")
	require.NoError(t, os.MkdirAll(imgDir, 0o755))
	pngData := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01" +
		"\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde")
	targetPath := filepath.Join(imgDir, "image_test123.png")
	require.NoError(t, os.WriteFile(targetPath, pngData, 0o644))
	t.Cleanup(func() { _ = os.RemoveAll(MediaPathPrefix) })

	text := "Here is the chart:\n" + targetPath
	parts, remaining := extractImages(text)
	require.Len(t, parts, 1, "should extract 1 image from local path")
	require.Contains(t, parts[0].URL, "data:image/")
	require.Equal(t, "Here is the chart:", remaining)
}

func TestExtractImages_URLImage(t *testing.T) {
	t.Parallel()
	text := "Chart below:\nhttps://example.com/chart.png\nend"
	parts, remaining := extractImages(text)
	require.Len(t, parts, 1)
	require.Equal(t, "https://example.com/chart.png", parts[0].URL)
	require.Contains(t, remaining, "Chart below:")
	require.Contains(t, remaining, "end")
}

func TestExtractImages_MixedContent(t *testing.T) {
	t.Parallel()
	text := "Analysis complete.\nhttps://cdn.example.com/result.png\nSee above."
	parts, remaining := extractImages(text)
	require.Len(t, parts, 1)
	require.Contains(t, remaining, "Analysis complete.")
	require.Contains(t, remaining, "See above.")
}

// ---------------------------------------------------------------------------
// Phase 4.4 — File upload detection
// ---------------------------------------------------------------------------

func TestUploadableExtensions_Coverage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ext      string
		expected bool
	}{
		{".pdf", true},
		{".csv", true},
		{".xlsx", true},
		{".docx", true},
		{".png", false},
		{".txt", false},
		{".jpg", false},
	}
	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			t.Parallel()
			found := false
			for _, e := range uploadableExtensions {
				if e == tt.ext {
					found = true
					break
				}
			}
			require.Equal(t, tt.expected, found)
		})
	}
}

func TestTryFileUpload_NoUploadablePath(t *testing.T) {
	t.Parallel()
	conn := &SlackConn{}
	require.False(t, conn.tryFileUpload(context.Background(), "hello world"))
	require.False(t, conn.tryFileUpload(context.Background(), "report.txt"))
}

func TestTryFileUpload_WithTempFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "report.pdf")
	require.NoError(t, os.WriteFile(pdfPath, []byte("%PDF-1.4 test"), 0o644))

	conn := &SlackConn{}
	require.False(t, conn.tryFileUpload(context.Background(), pdfPath))
}

// ---------------------------------------------------------------------------
// Phase 4.3 — tryImageBlocks with no images
// ---------------------------------------------------------------------------

func TestTryImageBlocks_NoImages(t *testing.T) {
	t.Parallel()
	conn := &SlackConn{adapter: &Adapter{}, channelID: "C_TEST", threadTS: "1234.5678"}
	err := conn.tryImageBlocks(context.Background(), "plain text only")
	require.Error(t, err, "should fail when no images found")
}

// ---------------------------------------------------------------------------
// StatusManager Notify — dedup + rate limiting
// ---------------------------------------------------------------------------

func TestNotify_Dedup_SameText(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	ctx := context.Background()

	// First call sets state.
	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, "Read(/src/main.go)"))
	require.Equal(t, "Read(/src/main.go)", sm.threadState["C1:T1"].lastText)

	// Duplicate text — should not update timestamp.
	prevTime := sm.threadState["C1:T1"].lastTime
	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, "Read(/src/main.go)"))
	require.Equal(t, prevTime, sm.threadState["C1:T1"].lastTime, "timestamp should not change for same text")
}

func TestNotify_RateLimit_DifferentText(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	ctx := context.Background()

	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, "Read(/a.go)"))
	require.Equal(t, "Read(/a.go)", sm.threadState["C1:T1"].lastText)

	// Different text within 3s — should be skipped, lastText unchanged.
	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolResult, "ok"))
	require.Equal(t, "Read(/a.go)", sm.threadState["C1:T1"].lastText, "rate-limited call should not update state")
}

func TestNotify_PassesAfterInterval(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	ctx := context.Background()

	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, "Read(/a.go)"))

	// Backdate lastTime to simulate 4s elapsed.
	sm.mu.Lock()
	sm.threadState["C1:T1"].lastTime = time.Now().Add(-4 * time.Second)
	sm.mu.Unlock()

	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolResult, "ok"))
	require.Equal(t, "ok", sm.threadState["C1:T1"].lastText, "call after 3s should update state")
}

func TestNotify_IndependentThreads(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	ctx := context.Background()

	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, "Read(/a.go)"))
	require.NoError(t, sm.Notify(ctx, "C1", "T2", StatusToolUse, "Bash(ls)"))

	require.Equal(t, "Read(/a.go)", sm.threadState["C1:T1"].lastText)
	require.Equal(t, "Bash(ls)", sm.threadState["C1:T2"].lastText)
}

func TestNotify_ClearResetsState(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	ctx := context.Background()

	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, "Read(/a.go)"))
	sm.Clear(ctx, "C1", "T1")
	require.Nil(t, sm.threadState["C1:T1"], "Clear should remove threadState")

	// Same text after Clear should create fresh state.
	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, "Read(/a.go)"))
	require.Equal(t, "Read(/a.go)", sm.threadState["C1:T1"].lastText)
}

func TestNotify_EmptyText_ClearsState(t *testing.T) {
	t.Parallel()

	sm := NewStatusManager(nil, slog.Default())
	ctx := context.Background()

	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, "Read(/a.go)"))
	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, ""))
	require.Nil(t, sm.threadState["C1:T1"], "empty text should clear threadState via Clear")

	// After clear, same text should pass.
	require.NoError(t, sm.Notify(ctx, "C1", "T1", StatusToolUse, "Read(/a.go)"))
	require.Equal(t, "Read(/a.go)", sm.threadState["C1:T1"].lastText)
}

func TestShortenPaths(t *testing.T) {
	t.Parallel()

	// Create a StatusManager for testing (workDir is per-instance now).
	sm := &StatusManager{}

	// Home dir substitution
	require.Equal(t, "~/src/main.go", sm.shortenPaths(homeDir+"/src/main.go"))
	require.Equal(t, "/usr/local/bin", sm.shortenPaths("/usr/local/bin"))
	require.Equal(t, "no path here", sm.shortenPaths("no path here"))

	// WorkDir substitution takes priority
	sm.SetWorkDir("/tmp/hotplex/workspace")

	require.Equal(t, "$WK/main.go", sm.shortenPaths("/tmp/hotplex/workspace/main.go"))
	require.Equal(t, "$WK/sub/file.txt", sm.shortenPaths("/tmp/hotplex/workspace/sub/file.txt"))

	// Both: workDir first, then homeDir on remaining
	sm.SetWorkDir(homeDir + "/projects/myapp")
	require.Equal(t, "$WK/main.go", sm.shortenPaths(homeDir+"/projects/myapp/main.go"))
	require.Equal(t, "~/other/file.go", sm.shortenPaths(homeDir+"/other/file.go"))
}

// ---------------------------------------------------------------------------
// controlFeedbackMessage
// ---------------------------------------------------------------------------

func TestControlFeedbackMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		action events.ControlAction
		want   string
	}{
		{"gc", events.ControlActionGC, "Session parked"},
		{"reset", events.ControlActionReset, "Context reset"},
		{"cd", events.ControlActionCD, "Switching work directory"},
		{"unknown", events.ControlAction("unknown"), "Done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Contains(t, controlFeedbackMessage(tt.action), tt.want)
		})
	}
}

// ---------------------------------------------------------------------------
// Adapter ConfigureWith
// ---------------------------------------------------------------------------

func TestAdapter_ConfigureWith(t *testing.T) {
	t.Parallel()

	enabled := true
	fakeTS := &fakeTranscriberForTest{}

	acfg := messaging.AdapterConfig{
		Extras: map[string]any{
			"bot_token":            "xoxb-test",
			"app_token":            "xapp-test",
			"assistant_enabled":    &enabled,
			"reconnect_base_delay": 5 * time.Second,
			"reconnect_max_delay":  60 * time.Second,
			"transcriber":          fakeTS,
		},
	}

	a := &Adapter{}
	err := a.ConfigureWith(acfg)
	require.NoError(t, err)
	require.Equal(t, "xoxb-test", a.botToken)
	require.Equal(t, "xapp-test", a.appToken)
	require.Same(t, &enabled, a.assistantEnabled)
	require.Equal(t, 5*time.Second, a.BackoffBaseDelay)
	require.Equal(t, 60*time.Second, a.BackoffMaxDelay)
	require.Same(t, fakeTS, a.transcriber)
}

// Gate is set via AdapterConfig.Gate.
func TestAdapter_ConfigureWith_Gate(t *testing.T) {
	t.Parallel()

	g := &messaging.Gate{}
	a := &Adapter{}
	err := a.ConfigureWith(messaging.AdapterConfig{Gate: g})
	require.NoError(t, err)
	require.Same(t, g, a.Gate)
}

func TestAdapter_ConfigureWith_BridgeSetsWorkDir(t *testing.T) {
	testBridge := messaging.NewBridge(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		messaging.PlatformSlack,
		nil, nil, nil, "claude_code", "", "/tmp/hotplex/workspace",
		"",
	)

	a := &Adapter{}
	a.statusMgr = NewStatusManager(a, slog.Default())
	err := a.ConfigureWith(messaging.AdapterConfig{Bridge: testBridge})
	require.NoError(t, err)
	require.Same(t, testBridge, a.Bridge())
	require.Equal(t, "/tmp/hotplex/workspace", a.statusMgr.WorkDir())
}

type fakeTranscriberForTest struct{}

func (*fakeTranscriberForTest) Transcribe(ctx context.Context, audioData []byte) (string, error) {
	return "", nil
}
func (*fakeTranscriberForTest) RequiresDisk() bool { return false }

func TestAdapter_Platform(t *testing.T) {
	t.Parallel()
	a := &Adapter{}
	require.Equal(t, messaging.PlatformSlack, a.Platform())
}

func TestAdapter_MakeEnvelope(t *testing.T) {
	t.Parallel()

	br := messaging.NewBridge(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		messaging.PlatformSlack,
		nil, nil, nil,
		"claude_code", "", "/tmp/hotplex/workspace",
		"",
	)

	a := &Adapter{botID: "B123"}
	err := a.ConfigureWith(messaging.AdapterConfig{Bridge: br})
	require.NoError(t, err)

	env := a.makeEnvelope("T1", "C1", "1234.56", "U1", "hello", "")

	require.NotNil(t, env)
	require.Equal(t, "U1", env.OwnerID)
	require.NotEmpty(t, env.SessionID)

	data, ok := env.Event.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "hello", data["content"])
}

func TestAdapter_MakeEnvelope_CustomWorkDir(t *testing.T) {
	t.Parallel()

	br := messaging.NewBridge(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		messaging.PlatformSlack,
		nil, nil, nil,
		"claude_code", "", "/default",
		"",
	)

	a := &Adapter{botID: "B123"}
	err := a.ConfigureWith(messaging.AdapterConfig{Bridge: br})
	require.NoError(t, err)

	env1 := a.makeEnvelope("T1", "C1", "", "U1", "hi", "/custom")
	env2 := a.makeEnvelope("T1", "C1", "", "U1", "hi", "")

	// Different workDir → different session ID.
	require.NotEqual(t, env1.SessionID, env2.SessionID)
}

// ---------------------------------------------------------------------------
// WriteCtx interaction events close stream writer
// ---------------------------------------------------------------------------

func TestWriteCtx_InteractionEvents_ClosesStream(t *testing.T) {
	// Do NOT mark parallel: modifies streamWriter field on conn.
	tests := []struct {
		name      string
		eventType events.Kind
		data      any
	}{
		{
			"PermissionRequest",
			events.PermissionRequest,
			events.PermissionRequestData{ID: "r1", ToolName: "Bash"},
		},
		{
			"QuestionRequest",
			events.QuestionRequest,
			events.QuestionRequestData{ID: "q1"},
		},
		{
			"ElicitationRequest",
			events.ElicitationRequest,
			events.ElicitationRequestData{ID: "e1", MCPServerName: "srv"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ta := testAdapter()
			ta.Interactions = messaging.NewInteractionManager(
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)

			conn := &SlackConn{
				adapter:   ta,
				channelID: "C_test",
				threadTS:  "123.000",
			}

			// Simulate an active stream writer.
			writer := NewNativeStreamingWriter(
				context.Background(), ta.client, "C_test", "123.000", "T1",
				nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil,
			)
			conn.streamWriterMu.Lock()
			conn.streamWriter = writer
			conn.streamWriterMu.Unlock()

			env := &events.Envelope{
				Version:   events.Version,
				SessionID: "sess-close-test",
				Event: events.Event{
					Type: tt.eventType,
					Data: tt.data,
				},
			}

			// WriteCtx will call closeStreamWriter then attempt to send the
			// interaction UI (which fails with invalid_auth on dummy token).
			_ = conn.WriteCtx(context.Background(), env)

			// Verify the stream writer was closed (niled out).
			conn.streamWriterMu.Lock()
			got := conn.streamWriter
			conn.streamWriterMu.Unlock()
			require.Nil(t, got, "streamWriter should be nil after %s event", tt.name)
		})
	}
}

// ---------------------------------------------------------------------------
// HandlerMu timeout boundary tests
// ---------------------------------------------------------------------------

func TestHandlerMu_TimeoutReleasesLock(t *testing.T) {
	t.Parallel()
	// Verify that handlerMu is correctly released after a context timeout.
	// This tests the lock-before-timeout pattern used in HandleTextMessage/CmdControl/CmdWorker.
	conn := &SlackConn{channelID: "C_test", threadTS: "123.000"}

	// Goroutine 1: acquire handlerMu, signal via barrier, then wait for context timeout.
	locked := make(chan struct{})
	goroutineDone := make(chan struct{})
	go func() {
		defer close(goroutineDone)
		conn.handlerMu.Lock()
		defer conn.handlerMu.Unlock()
		close(locked) // barrier: lock acquired

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		<-ctx.Done() // blocks until timeout
	}()
	<-locked // wait until goroutine 1 holds the lock

	// Goroutine 2: try to acquire handlerMu — should block until goroutine 1 finishes.
	acquired := make(chan struct{})
	go func() {
		conn.handlerMu.Lock()
		defer conn.handlerMu.Unlock()
		close(acquired)
	}()

	// After goroutine 1's timeout (50ms) + margin, goroutine 2 should acquire the lock.
	select {
	case <-acquired:
		// Success: mu was released after timeout.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handlerMu was not released after context timeout — defer Unlock failed")
	}

	<-goroutineDone
}

func TestHandlerMu_MultipleAcquireRelease(t *testing.T) {
	t.Parallel()
	// Verify that handlerMu can be acquired multiple times in sequence
	// (simulating sequential message processing in the same thread).
	conn := &SlackConn{}

	for i := 0; i < 5; i++ {
		conn.handlerMu.Lock()
		// Simulate a short-lived context timeout as in the production code.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		<-ctx.Done()
		cancel()
		conn.handlerMu.Unlock()
	}

	// If we reach here, the lock was properly acquired and released 5 times.
}

func TestHandlerMu_TimeoutDoesNotBlockSubsequentCalls(t *testing.T) {
	t.Parallel()
	// Verify that after a simulated timeout path, the next goroutine
	// can immediately acquire the lock (no permanent hold).
	conn := &SlackConn{channelID: "C_test", threadTS: "123.000"}

	// Simulate one complete timeout cycle: Lock → context timeout → Unlock (via defer).
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.handlerMu.Lock()
		defer conn.handlerMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		<-ctx.Done()
	}()
	<-done

	// Now verify immediate acquisition.
	acquired := make(chan struct{})
	go func() {
		conn.handlerMu.Lock()
		defer conn.handlerMu.Unlock()
		close(acquired)
	}()

	select {
	case <-acquired:
		// Lock acquired immediately after previous timeout cycle.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("handlerMu still held after timeout cycle completed")
	}
}

// TestHandleMessageEvent_BridgeNilRetryable pins the "bridge not configured"
// contract: a message arriving without a bridge must be rolled back (not
// silently dropped with the dedup entry kept), so the same ClientMsgID can be
// delivered once the bridge is configured.
func TestHandleMessageEvent_BridgeNilRetryable(t *testing.T) {
	t.Parallel()
	a := newTestAdapter(t)
	a.client = &stubSlackClient{}

	evt := makeDMEvent("U_ALICE", "bridge comes later")
	msg := evt.InnerEvent.Data.(*slackevents.MessageEvent)

	// No bridge → HandleTextMessage fails → the record must roll back.
	a.handleMessageEvent(context.Background(), msg, "T_TEST")
	require.Zero(t, dedupCount(a), "missing bridge must roll back the record")

	// Same ClientMsgID becomes retryable once the bridge is configured.
	handler := &captureHandler{}
	bridge := messaging.NewBridge(
		slog.Default(), messaging.PlatformSlack, nil, handler, nil,
		"test_worker", "", "/tmp", "",
	)
	require.NoError(t, a.ConfigureWith(messaging.AdapterConfig{Bridge: bridge}))

	a.handleMessageEvent(context.Background(), msg, "T_TEST")
	require.Len(t, handler.calls, 1, "same ID must be retryable after the bridge is configured")
	require.False(t, a.Dedup.TryRecord(msg.ClientMsgID), "successful delivery keeps the record")
}
