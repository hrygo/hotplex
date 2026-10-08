package admin

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/audit"
	"github.com/hrygo/hotplex/internal/dbutil"
)

// mockAuditStore implements audit.Store for ActivityService tests.
type mockAuditStore struct {
	queryFn       func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error)
	statsFn       func(ctx context.Context, q audit.Query) (audit.ActivityStats, error)
	identityLinks []audit.IdentityLink
}

func (m *mockAuditStore) BeginTx(ctx context.Context) (audit.Tx, error) {
	return nil, errors.New("not implemented")
}
func (m *mockAuditStore) Query(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, q)
	}
	return nil, nil
}
func (m *mockAuditStore) Stats(ctx context.Context, q audit.Query) (audit.ActivityStats, error) {
	if m.statsFn != nil {
		return m.statsFn(ctx, q)
	}
	return audit.ActivityStats{ByOutcome: map[string]int64{}, ByPlatform: map[string]int64{}}, nil
}
func (m *mockAuditStore) QueryAsc(ctx context.Context, fromID int64, limit int) ([]audit.UserActivity, error) {
	return nil, nil // service tests don't exercise the verifier path
}
func (m *mockAuditStore) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return 0, nil
}
func (m *mockAuditStore) SaveCheckpoint(ctx context.Context, c audit.Checkpoint) error {
	return nil
}
func (m *mockAuditStore) LatestCheckpoint(ctx context.Context) (*audit.Checkpoint, error) {
	return nil, nil
}
func (m *mockAuditStore) ListIdentityLinks(ctx context.Context, principalUserID string) ([]audit.IdentityLink, error) {
	if principalUserID == "" {
		return m.identityLinks, nil
	}
	var out []audit.IdentityLink
	for _, link := range m.identityLinks {
		if link.PrincipalUserID == principalUserID {
			out = append(out, link)
		}
	}
	return out, nil
}
func (m *mockAuditStore) UpsertIdentityLink(ctx context.Context, link audit.IdentityLink) error {
	m.identityLinks = append(m.identityLinks, link)
	return nil
}
func (m *mockAuditStore) DeleteIdentityLink(ctx context.Context, id string) error { return nil }
func (m *mockAuditStore) Close() error                                            { return nil }
func (m *mockAuditStore) Dialect() dbutil.Dialect {
	return dbutil.DialectSQLite
}

func TestActivityService_Query_MasksPII(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			return []audit.UserActivity{
				{
					ID:        1,
					Ts:        time.Now().UnixMilli(),
					UserID:    "u1",
					Action:    "auth.login",
					Outcome:   "success",
					IP:        "192.168.1.100",
					UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
				},
			}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	rows, err := svc.Query(context.Background(), audit.Query{IncludePII: false})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// IP last octet zeroed
	require.Equal(t, "192.168.1.0", rows[0].IP)
	// UserAgent truncated
	require.True(t, len(rows[0].UserAgent) <= 53) // 50 + "..."
	require.True(t, strings.HasSuffix(rows[0].UserAgent, "..."))
}

func TestActivityService_Query_IncludePII(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			return []audit.UserActivity{
				{
					ID:        1,
					UserID:    "u1",
					Action:    "auth.login",
					Outcome:   "success",
					IP:        "192.168.1.100",
					UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
				},
			}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	rows, err := svc.Query(context.Background(), audit.Query{IncludePII: true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// IP NOT masked
	require.Equal(t, "192.168.1.100", rows[0].IP)
	// UserAgent NOT truncated
	require.Contains(t, rows[0].UserAgent, "Chrome/120.0.0.0")
}

func TestActivityService_QueryByUser(t *testing.T) {
	t.Parallel()
	var capturedQuery audit.Query
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			capturedQuery = q
			return []audit.UserActivity{{ID: 1, UserID: "target-user"}}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	rows, err := svc.QueryByUser(context.Background(), "target-user", audit.Query{Action: "auth.login"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "target-user", capturedQuery.UserID)
	require.Equal(t, "auth.login", capturedQuery.Action)
}

func TestActivityService_QueryPrincipal_ExpandsLinkedSubjects(t *testing.T) {
	t.Parallel()
	var gotQuery audit.Query
	store := &mockAuditStore{
		identityLinks: []audit.IdentityLink{
			{PrincipalUserID: "u-local", Subject: "ou_feishu"},
			{PrincipalUserID: "u-local", Subject: "U_slack"},
			{PrincipalUserID: "other", Subject: "ignored"},
		},
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			gotQuery = q
			return []audit.UserActivity{{ID: 1, UserID: "ou_feishu"}}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	rows, resolved, err := svc.QueryPrincipal(context.Background(), "u-local", audit.Query{Action: audit.ActionMessageInbound})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, []string{"u-local", "ou_feishu", "U_slack"}, resolved)
	require.Empty(t, gotQuery.UserID)
	require.Equal(t, []string{"u-local", "ou_feishu", "U_slack"}, gotQuery.UserIDs)
	require.Equal(t, audit.ActionMessageInbound, gotQuery.Action)
}

func TestActivityService_Query_StoreError(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			return nil, errors.New("db down")
		},
	}
	svc := NewActivityService(store, slog.Default())

	_, err := svc.Query(context.Background(), audit.Query{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "audit query")
	require.Contains(t, err.Error(), "db down")
}

func TestActivityService_Export_JSON(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			return []audit.UserActivity{
				{ID: 1, UserID: "u1", Action: "auth.login", Outcome: "success"},
			}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	data, contentType, err := svc.Export(context.Background(), audit.Query{}, "json", "admin-user")
	require.NoError(t, err)
	require.Equal(t, "application/json; charset=utf-8", contentType)
	require.Contains(t, string(data), `"user_id": "u1"`)
}

func TestActivityService_Export_DefaultMasksPII(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			require.False(t, q.IncludePII)
			return []audit.UserActivity{
				{
					ID:        1,
					UserID:    "u1",
					IP:        "192.168.1.100",
					UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
				},
			}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	data, _, err := svc.Export(context.Background(), audit.Query{}, "json", "admin-user")
	require.NoError(t, err)
	var rows []audit.UserActivity
	require.NoError(t, json.Unmarshal(data, &rows))
	require.Len(t, rows, 1)
	require.Equal(t, "192.168.1.0", rows[0].IP)
	require.True(t, strings.HasSuffix(rows[0].UserAgent, "..."))
}

func TestActivityService_Export_IncludePII(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			require.True(t, q.IncludePII)
			return []audit.UserActivity{
				{
					ID:        1,
					UserID:    "u1",
					IP:        "192.168.1.100",
					UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
				},
			}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	data, _, err := svc.Export(context.Background(), audit.Query{IncludePII: true}, "json", "admin-user")
	require.NoError(t, err)
	var rows []audit.UserActivity
	require.NoError(t, json.Unmarshal(data, &rows))
	require.Len(t, rows, 1)
	require.Equal(t, "192.168.1.100", rows[0].IP)
	require.Contains(t, rows[0].UserAgent, "Chrome/120.0.0.0")
}

func TestActivityService_Export_CSV(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			return []audit.UserActivity{
				{ID: 1, Ts: 1700000000000, UserID: "u1", Action: "auth.login", Outcome: "success", ChainEpoch: "legacy"},
			}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	data, contentType, err := svc.Export(context.Background(), audit.Query{}, "csv", "admin-user")
	require.NoError(t, err)
	require.Equal(t, "text/csv; charset=utf-8", contentType)
	// Parse CSV
	r := csv.NewReader(strings.NewReader(string(data)))
	records, err := r.ReadAll()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(records), 2) // header + 1 row
	require.Equal(t, "chain_epoch", records[0][0])
	require.Equal(t, "legacy", records[1][0])
	require.Equal(t, "id", records[0][1])
	require.Equal(t, "1", records[1][1])
}

func TestActivityService_Export_DefaultFormat(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			return []audit.UserActivity{{ID: 1}}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	_, contentType, err := svc.Export(context.Background(), audit.Query{}, "", "admin-user")
	require.NoError(t, err)
	require.Equal(t, "application/json; charset=utf-8", contentType)
}

func TestActivityService_Export_UnknownFormat(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			return []audit.UserActivity{{ID: 1}}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	_, _, err := svc.Export(context.Background(), audit.Query{}, "xml", "admin-user")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown format")
}

func TestActivityService_Export_AnonymousExporter(t *testing.T) {
	t.Parallel()
	store := &mockAuditStore{
		queryFn: func(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
			return []audit.UserActivity{{ID: 1}}, nil
		},
	}
	svc := NewActivityService(store, slog.Default())

	// Empty exporterUserID should default to "anonymous"
	data, _, err := svc.Export(context.Background(), audit.Query{}, "json", "")
	require.NoError(t, err)
	require.NotEmpty(t, data)
}

func TestMaskPII_IPv4(t *testing.T) {
	t.Parallel()
	ua := &audit.UserActivity{IP: "10.0.0.255"}
	maskPII(ua)
	require.Equal(t, "10.0.0.0", ua.IP)
}

func TestMaskPII_IPv6(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Full 8-group form: /64 mask keeps the first 4 groups, zeros the rest.
		// net.IP returns the canonical (shortened) representation.
		{"full_form", "2001:0db8:85a3:0000:0000:8a2e:0370:7334", "2001:db8:85a3::"},
		// Shorthand forms must now be masked correctly (previously skipped).
		{"loopback", "::1", "::"},
		{"link_local", "fe80::1", "fe80::"},
		// IPv4-mapped IPv6 is parsed as IPv4 by To4(), so it masks to /24.
		{"v4_mapped", "::ffff:192.168.1.5", "192.168.1.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ua := &audit.UserActivity{IP: tc.in}
			maskPII(ua)
			require.Equal(t, tc.want, ua.IP)
		})
	}
}

func TestMaskPII_ShortUserAgent(t *testing.T) {
	t.Parallel()
	ua := &audit.UserActivity{UserAgent: "curl/7.68.0"}
	maskPII(ua)
	// Short UA not truncated
	require.Equal(t, "curl/7.68.0", ua.UserAgent)
}

func TestMaskPII_LongUserAgent(t *testing.T) {
	t.Parallel()
	longUA := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	ua := &audit.UserActivity{UserAgent: longUA}
	maskPII(ua)
	require.True(t, strings.HasSuffix(ua.UserAgent, "..."))
	require.LessOrEqual(t, len(ua.UserAgent), 53)
}

func TestEncodeJSON(t *testing.T) {
	t.Parallel()
	rows := []audit.UserActivity{
		{ID: 1, UserID: "u1", Action: "auth.login"},
	}
	data, err := encodeJSON(rows)
	require.NoError(t, err)
	var decoded []audit.UserActivity
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Len(t, decoded, 1)
	require.Equal(t, "u1", decoded[0].UserID)
}

func TestEncodeCSV(t *testing.T) {
	t.Parallel()
	rows := []audit.UserActivity{
		{ID: 1, Ts: 1700000000000, UserID: "u1", Action: "auth.login", Outcome: "success"},
		{ID: 2, Ts: 1700000001000, UserID: "u2", Action: "session.create", Outcome: "success"},
	}
	data, err := encodeCSV(rows)
	require.NoError(t, err)
	r := csv.NewReader(strings.NewReader(string(data)))
	records, err := r.ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 3) // header + 2 rows
	require.Equal(t, "chain_epoch", records[0][0])
	require.Equal(t, "id", records[0][1])
	require.Equal(t, "user_id", records[0][3])
	require.Equal(t, "1", records[1][1])
	require.Equal(t, "u1", records[1][3])
	require.Equal(t, "2", records[2][1])
	require.Equal(t, "u2", records[2][3])
}

// TestEncodeCSV_FormulaInjectionSanitized verifies that attacker-controlled
// fields (user_id, user_agent) starting with =, +, -, @ get a single-quote
// prefix so spreadsheet apps render them as text, not formulae. This is
// OWASP CSV Injection / CWE-1236 mitigation (review issue #3).
func TestEncodeCSV_FormulaInjectionSanitized(t *testing.T) {
	t.Parallel()
	rows := []audit.UserActivity{
		{
			ID:        1,
			Ts:        1700000000000,
			UserID:    "=cmd|'/c calc'!A0", // classic Excel injection
			Action:    "auth.login",
			Outcome:   "success",
			UserAgent: "@SUM(1+1)*cmd|'/c calc'!A0", // multi-prefix coverage
		},
	}
	data, err := encodeCSV(rows)
	require.NoError(t, err)
	r := csv.NewReader(strings.NewReader(string(data)))
	records, err := r.ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 2) // header + 1 row
	// chain_epoch adds one leading column; user_id is index 3 and user_agent index 12.
	require.Equal(t, "'=cmd|'/c calc'!A0", records[1][3], "user_id must be prefixed to neutralize formula")
	require.Equal(t, "'@SUM(1+1)*cmd|'/c calc'!A0", records[1][12], "user_agent must be prefixed")
}

// TestSanitizeCSVCell verifies the cell sanitizer covers all OWASP-listed
// formula introducers and leaves benign values untouched.
func TestSanitizeCSVCell(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"normal_value", "normal_value"},
		{"=SUM(A1:A2)", "'=SUM(A1:A2)"},
		{"+1234", "'+1234"},
		{"-5", "'-5"},
		{"@admin", "'@admin"},
		{"\ttab-led", "'\ttab-led"},
		{"\rCR-led", "'\rCR-led"},
		{"hello=world", "hello=world"}, // = only dangerous as the FIRST char
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, c.want, sanitizeCSVCell(c.in))
		})
	}
}
