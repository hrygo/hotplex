package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/sqlutil"
	"github.com/hrygo/hotplex/pkg/events"
)

// auditTestSchema mirrors migration 023 (SQLite) for gateway-package tests.
const auditTestSchema = `
CREATE TABLE IF NOT EXISTS user_activity (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL,
    user_id       TEXT    NOT NULL,
    user_id_type  TEXT    NOT NULL,
    platform      TEXT    NOT NULL,
    session_id    TEXT,
    action        TEXT    NOT NULL,
    resource_type TEXT,
    resource_id   TEXT,
    outcome       TEXT    NOT NULL,
    detail_json   TEXT    NOT NULL,
    event_ref     TEXT,
    ip            TEXT,
    user_agent    TEXT,
    prev_hash     TEXT    NOT NULL,
    self_hash     TEXT    NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_chain_checkpoints (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    pruned_at       INTEGER NOT NULL,
    last_self_hash  TEXT    NOT NULL,
    next_id         INTEGER NOT NULL
);
`

// gwTestCollector builds a started audit.Collector backed by SQLite for
// gateway-package tests. Returns the collector and a query helper.
func gwTestCollector(t *testing.T) (*audit.Collector, func(t *testing.T) []audit.UserActivity) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "gw_audit_test.db")
	db, err := sql.Open(sqlutil.DriverName, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	require.NoError(t, err)
	_, err = db.Exec(auditTestSchema)
	require.NoError(t, err)
	writeMu := sqlutil.NewWriteMu(sqlutil.DialectSQLite)
	store, err := audit.NewStore(db, dbutil.DialectSQLite, writeMu, slog.Default())
	require.NoError(t, err)
	spill, err := audit.OpenSpill(filepath.Join(t.TempDir(), "spill.wal"))
	require.NoError(t, err)
	c := audit.NewCollector(store, spill, nil, slog.Default(), audit.CollectorConfig{
		ChannelCap: 64, BatchSize: 10, BatchInterval: 5 * time.Millisecond,
	})
	c.Start(context.Background())
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	query := func(t *testing.T) []audit.UserActivity {
		t.Helper()
		rows, err := db.Query(`SELECT id, ts, user_id, user_id_type, platform,
			COALESCE(session_id,''), action, COALESCE(resource_type,''),
			COALESCE(resource_id,''), outcome, detail_json, COALESCE(event_ref,''),
			COALESCE(ip,''), COALESCE(user_agent,''), prev_hash, self_hash
			FROM user_activity ORDER BY id`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []audit.UserActivity
		for rows.Next() {
			var r audit.UserActivity
			require.NoError(t, rows.Scan(&r.ID, &r.Ts, &r.UserID, &r.UserIDType, &r.Platform,
				&r.SessionID, &r.Action, &r.ResourceType, &r.ResourceID, &r.Outcome,
				&r.DetailJSON, &r.EventRef, &r.IP, &r.UserAgent, &r.PrevHash, &r.SelfHash))
			out = append(out, r)
		}
		return out
	}
	return c, query
}

// TestBuildToolCallDetail_OmitsInput verifies tool input is not copied into
// audit facts, including for tools that can contain commands or file contents.
func TestBuildToolCallDetail_OmitsInput(t *testing.T) {
	t.Parallel()
	tc := events.ToolCallData{
		ID:   "tc1",
		Name: "Bash",
		Input: map[string]any{
			"command": `echo PRIVATE_COMMAND_BODY`,
		},
	}
	detail := buildToolCallDetail(&tc)
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(detail), &d))
	require.Equal(t, "Bash", d["name"])
	require.Equal(t, true, d["success"])
	require.NotContains(t, d, "input")
	require.NotContains(t, d, "input_sha256")
	require.NotContains(t, d, "input_preview")
	require.NotContains(t, detail, "PRIVATE_COMMAND_BODY")
}

// TestBuildToolCallDetail_NonSensitiveTool verifies non-sensitive tool input
// is omitted instead of retaining a preview or a guessable content hash.
func TestBuildToolCallDetail_NonSensitiveTool(t *testing.T) {
	t.Parallel()
	tc := events.ToolCallData{
		ID:   "tc2",
		Name: "Read",
		Input: map[string]any{
			"path": "/etc/hosts",
		},
	}
	detail := buildToolCallDetail(&tc)
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(detail), &d))
	require.Equal(t, "Read", d["name"])
	require.NotContains(t, d, "input")
	require.NotContains(t, d, "input_sha256")
	require.NotContains(t, d, "input_preview")
	require.NotContains(t, detail, "/etc/hosts")
}

func TestBuildToolCallDetail_NonSensitiveToolRedactsCredentials(t *testing.T) {
	t.Parallel()
	tc := events.ToolCallData{
		ID:   "tc-secret-preview",
		Name: "Read",
		Input: map[string]any{
			"path":          "/tmp/config.json",
			"authorization": "Bearer FAKE_PREVIEW_TOKEN_123456",
			"nested": map[string]any{
				"private_key": "-----BEGIN PRIVATE KEY-----\nFAKE_PRIVATE_BODY\n-----END PRIVATE KEY-----",
			},
		},
	}
	detail := buildToolCallDetail(&tc)
	require.NotContains(t, detail, "FAKE_PREVIEW_TOKEN_123456")
	require.NotContains(t, detail, "FAKE_PRIVATE_BODY")
	require.NotContains(t, detail, "config.json")
	require.NotContains(t, detail, "private_key")
	require.NotContains(t, detail, "input")
}

func TestBuildToolCallDetail_LowercaseBashOmitsInput(t *testing.T) {
	t.Parallel()
	tc := events.ToolCallData{ID: "tc-bash", Name: "bash", Input: map[string]any{"command": "pwd"}}
	detail := buildToolCallDetail(&tc)
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(detail), &d))
	require.NotContains(t, d, "input")
	require.NotContains(t, detail, "pwd")
}

// TestBuildToolCallDetail_NoInput covers parameterless tools.
func TestBuildToolCallDetail_NoInput(t *testing.T) {
	t.Parallel()
	tc := events.ToolCallData{ID: "tc3", Name: "ListTools"}
	detail := buildToolCallDetail(&tc)
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(detail), &d))
	require.Equal(t, "ListTools", d["name"])
	require.NotContains(t, d, "input_sha256")
	require.NotContains(t, d, "input_preview")
	require.NotContains(t, d, "input")
}

// TestBuildToolCallDetail_ACPExtFields verifies metadata values are omitted.
func TestBuildToolCallDetail_ACPExtFields(t *testing.T) {
	t.Parallel()
	tc := events.ToolCallData{
		ID:    "tc4",
		Name:  "Edit",
		Title: "edit: main.go",
		Kind:  "edit",
		Input: map[string]any{"path": "main.go"},
	}
	detail := buildToolCallDetail(&tc)
	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(detail), &d))
	require.NotContains(t, d, "title")
	require.NotContains(t, d, "kind")
	require.NotContains(t, detail, "main.go")
}

// TestMaskSensitiveInput covers the PII redaction patterns (spec §5.9).
func TestMaskSensitiveInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		// 'notContains' tokens that must NOT appear in the masked output.
		notContains string
	}{
		{
			name: "hpk api key",
			// NOTE: fake placeholder body — not a real key.
			in:          `KEY=hpk_FAKEPLACEHOLDERFORTTEST`,
			notContains: "FAKEPLACEHOLDERFORTTEST",
		},
		{
			name: "openai sk- key",
			// NOTE: fake placeholder body — not a real OpenAI key.
			in:          `Authorization: Bearer sk-FAKEPLACEHOLDER1234567890`,
			notContains: "FAKEPLACEHOLDER1234567890",
		},
		{
			name: "bearer token",
			// NOTE: fake JWT-shaped placeholder — not a real token.
			in:          `Authorization: Bearer FAKEFAKEFAKEFAKEFAKEFAKEFAKE.payload.sig`,
			notContains: "FAKEFAKEFAKEFAKEFAKEFAKEFAKE.payload.sig",
		},
		{
			name:        "password assignment",
			in:          `password=S3cretP@ssw0rd!`,
			notContains: "S3cretP@ssw0rd",
		},
		{
			name: "api_key json",
			// NOTE: uses an obviously-fake placeholder body (not a real token
			// format) so GitHub push protection does not flag this test fixture.
			// The mask regex matches the xoxb- prefix regardless of the body.
			in:          `"api_key":"xoxb-FAKEPLACEHOLDERVALUEFORTTESTONLY"`,
			notContains: "FAKEPLACEHOLDERVALUEFORTTESTONLY",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			masked := maskSensitiveInput(tc.in)
			require.NotContains(t, masked, tc.notContains, "raw secret leaked into masked output")
			require.NotEqual(t, tc.in, masked, "input was not masked at all")
		})
	}
}

// TestMaskSensitiveInput_NoFalsePositives verifies benign text is untouched.
func TestMaskSensitiveInput_NoFalsePositives(t *testing.T) {
	t.Parallel()
	benign := []string{
		"hello world",
		"git commit -m 'fix bug'",
		"/usr/local/bin/node server.js",
		"the quick brown fox",
	}
	for _, in := range benign {
		require.Equal(t, in, maskSensitiveInput(in), "benign text should pass through unchanged")
	}
}

// TestSensitiveToolNames_Coverage is a sanity check that the high-value tools
// named in spec §5.2 are flagged sensitive.
func TestSensitiveToolNames_Coverage(t *testing.T) {
	t.Parallel()
	must := []string{"bash", "write", "edit", "multiedit", "webfetch", "websearch"}
	for _, name := range must {
		require.True(t, sensitiveToolNames[name], "%s must be in sensitiveToolNames (spec §5.2)", name)
	}
	require.False(t, sensitiveToolNames["read"], "Read is non-sensitive")
	require.False(t, sensitiveToolNames["glob"], "Glob is non-sensitive")
}

// TestEmitToolCallAudit_Enqueues verifies the full bridge path: calling
// emitToolCallAudit persists a tool.call row with correct attribution.
func TestEmitToolCallAudit_Enqueues(t *testing.T) {
	t.Parallel()
	c, query := gwTestCollector(t)
	b := &Bridge{auditCollector: c}
	fc := &forwardContext{
		sessionID:    "sess-1",
		sessPlatform: "feishu",
		sessOwner:    "ou_testuser",
	}
	tc := events.ToolCallData{ID: "tc-audit-1", Name: "Bash", Input: map[string]any{"command": "PRIVATE_TOOL_BODY"}}

	b.emitToolCallAudit(fc, &tc)

	require.Eventually(t, func() bool { return len(query(t)) >= 1 }, 2*time.Second, 5*time.Millisecond)
	rows := query(t)
	require.Len(t, rows, 1)
	r := rows[0]
	require.Equal(t, audit.ActionToolCall, r.Action)
	require.Equal(t, audit.OutcomeSuccess, r.Outcome)
	require.Equal(t, "ou_testuser", r.UserID)
	require.Equal(t, audit.UserIDTypePlatform, r.UserIDType)
	require.Equal(t, "feishu", r.Platform)
	require.Equal(t, "sess-1", r.SessionID)
	require.Equal(t, "tool", r.ResourceType)
	require.Equal(t, "tc-audit-1", r.ResourceID)
	require.Contains(t, r.DetailJSON, "Bash")
	require.NotContains(t, r.DetailJSON, "PRIVATE_TOOL_BODY")
	require.NotContains(t, r.DetailJSON, `"input"`)
	require.NotContains(t, r.DetailJSON, `"input_sha256"`)
}

// TestEmitToolCallAudit_AnonymousFallback verifies empty owner falls back to
// the anonymous sentinel (spec §5.4).
func TestEmitToolCallAudit_AnonymousFallback(t *testing.T) {
	t.Parallel()
	c, query := gwTestCollector(t)
	b := &Bridge{auditCollector: c}
	fc := &forwardContext{sessionID: "sess-2", sessPlatform: "webchat", sessOwner: ""}
	tc := events.ToolCallData{ID: "tc-3", Name: "Read", Input: map[string]any{"path": "/x"}}

	b.emitToolCallAudit(fc, &tc)

	require.Eventually(t, func() bool { return len(query(t)) >= 1 }, 2*time.Second, 5*time.Millisecond)
	r := query(t)[0]
	require.Equal(t, audit.AnonymousUserID, r.UserID)
}

// TestEmitToolCallAudit_NilCollectorNoop verifies a nil collector never panics.
func TestEmitToolCallAudit_NilCollectorNoop(t *testing.T) {
	t.Parallel()
	b := &Bridge{auditCollector: nil}
	fc := &forwardContext{sessionID: "s", sessPlatform: "slack", sessOwner: "U1"}
	require.NotPanics(t, func() {
		b.emitToolCallAudit(fc, &events.ToolCallData{ID: "x", Name: "Read"})
	})
}

// TestEmitToolCallAudit_Backpressure verifies high-frequency emits do not
// block the caller beyond the collector's enqueue budget (spec §5.10: tool.call
// is high-frequency; collector spill handles overflow without blocking forward).
func TestEmitToolCallAudit_Backpressure(t *testing.T) {
	t.Parallel()
	c, _ := gwTestCollector(t)
	b := &Bridge{auditCollector: c}
	fc := &forwardContext{sessionID: "sess-bp", sessPlatform: "slack", sessOwner: "U1"}
	tc := events.ToolCallData{ID: "bp", Name: "Read", Input: map[string]any{"n": 1}}

	// Enqueue well beyond channel cap (64); spill should absorb without panic.
	for i := 0; i < 500; i++ {
		b.emitToolCallAudit(fc, &tc)
	}
	// No assertion on row count — the point is the loop returns without blocking
	// or panicking. The collector's spill mechanism guarantees zero loss.
}

func TestEmitPermissionRequestAudit(t *testing.T) {
	t.Parallel()
	c, query := gwTestCollector(t)
	b := &Bridge{auditCollector: c}
	fc := &forwardContext{
		sessionID:    "sess-req-1",
		sessOwner:    "u-req-1",
		sessPlatform: "slack",
	}

	b.emitPermissionRequestAudit(fc, &events.PermissionRequestData{
		ID:          "req-pr-1",
		ToolName:    "Bash",
		Description: "Execute shell script",
		Args:        []string{"ls", "-la"},
	})

	require.Eventually(t, func() bool { return len(query(t)) >= 1 }, 2*time.Second, 5*time.Millisecond)
	r := query(t)[0]
	require.Equal(t, audit.ActionPermissionRequest, r.Action)
	require.Equal(t, audit.OutcomeSuccess, r.Outcome)
	require.Equal(t, "u-req-1", r.UserID)
	require.Equal(t, "slack", r.Platform)
	require.Equal(t, "sess-req-1", r.SessionID)
	require.Equal(t, "permission", r.ResourceType)
	require.Equal(t, "req-pr-1", r.ResourceID)
	require.NotContains(t, r.DetailJSON, "Bash")
	require.NotContains(t, r.DetailJSON, "Execute shell script")
	require.NotContains(t, r.DetailJSON, "ls")
}

func TestEmitInteractionAudit_PermissionResponse(t *testing.T) {
	t.Parallel()
	c, query := gwTestCollector(t)
	h := &Handler{auditCollector: c}

	// Test Allowed = true
	h.emitInteractionAudit("u-1", "webchat", "sess-p1", events.PermissionResponse, events.PermissionResponseData{
		ID:      "req-p1",
		Allowed: true,
		Reason:  "approved by user",
	}, "")

	require.Eventually(t, func() bool { return len(query(t)) >= 1 }, 2*time.Second, 5*time.Millisecond)
	r := query(t)[0]
	require.Equal(t, audit.ActionPermissionResponse, r.Action)
	require.Equal(t, audit.OutcomeSuccess, r.Outcome)
	require.Equal(t, "u-1", r.UserID)
	require.Equal(t, "webchat", r.Platform)
	require.Equal(t, "sess-p1", r.SessionID)
	require.Equal(t, "permission", r.ResourceType)
	require.Equal(t, "req-p1", r.ResourceID)
	require.Contains(t, r.DetailJSON, `"allowed":true`)
	require.NotContains(t, r.DetailJSON, "approved by user")

	// Test Allowed = false (Denied)
	h.emitInteractionAudit("u-1", "webchat", "sess-p2", events.PermissionResponse, map[string]any{
		"id":      "req-p2",
		"allowed": false,
		"reason":  "user denied",
	}, "")

	require.Eventually(t, func() bool { return len(query(t)) >= 2 }, 2*time.Second, 5*time.Millisecond)
	r2 := query(t)[1]
	require.Equal(t, audit.ActionPermissionResponse, r2.Action)
	require.Equal(t, audit.OutcomeDenied, r2.Outcome)
	require.Contains(t, r2.DetailJSON, `"allowed":false`)
	require.NotContains(t, r2.DetailJSON, "user denied")
}

func TestEmitInteractionAudit_QuestionResponse(t *testing.T) {
	t.Parallel()
	c, query := gwTestCollector(t)
	h := &Handler{auditCollector: c}

	h.emitInteractionAudit("u-2", "feishu", "sess-q1", events.QuestionResponse, events.QuestionResponseData{
		ID:      "req-q1",
		Answers: map[string]string{"q1": "Option A"},
	}, "")

	require.Eventually(t, func() bool { return len(query(t)) >= 1 }, 2*time.Second, 5*time.Millisecond)
	r := query(t)[0]
	require.Equal(t, audit.ActionQuestionResponse, r.Action)
	require.Equal(t, audit.OutcomeSuccess, r.Outcome)
	require.Equal(t, "question", r.ResourceType)
	require.Equal(t, "req-q1", r.ResourceID)
	require.NotContains(t, r.DetailJSON, "Option A")
}

func TestEmitInteractionAudit_ElicitationResponse(t *testing.T) {
	t.Parallel()
	c, query := gwTestCollector(t)
	h := &Handler{auditCollector: c}

	h.emitInteractionAudit("u-3", "slack", "sess-e1", events.ElicitationResponse, events.ElicitationResponseData{
		ID:      "req-e1",
		Action:  "decline",
		Content: map[string]any{"answer": "PRIVATE_ANSWER"},
	}, "")

	require.Eventually(t, func() bool { return len(query(t)) >= 1 }, 2*time.Second, 5*time.Millisecond)
	r := query(t)[0]
	require.Equal(t, audit.ActionElicitationResponse, r.Action)
	require.Equal(t, audit.OutcomeDenied, r.Outcome)
	require.Equal(t, "elicitation", r.ResourceType)
	require.Equal(t, "req-e1", r.ResourceID)
	require.Contains(t, r.DetailJSON, `"action":"decline"`)
	require.NotContains(t, r.DetailJSON, "PRIVATE_ANSWER")
}
