package admin

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/audit"
)

// ActivityService provides by-user audit query + export per spec section 5.8.
type ActivityService struct {
	store audit.Store
	log   *slog.Logger
}

// NewActivityService returns an ActivityService backed by the given audit store.
// A nil log falls back to slog.Default().
func NewActivityService(store audit.Store, log *slog.Logger) *ActivityService {
	if log == nil {
		log = slog.Default()
	}
	return &ActivityService{store: store, log: log}
}

// Query returns audit rows matching the query, with PII redaction when
// q.IncludePII is false (spec §5.9).
func (s *ActivityService) Query(ctx context.Context, q audit.Query) ([]audit.UserActivity, error) {
	rows, err := s.store.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("audit query: %w", err)
	}
	if !q.IncludePII {
		for i := range rows {
			maskPII(&rows[i])
		}
	}
	return rows, nil
}

// QueryByUser is a convenience wrapper that sets q.UserID.
func (s *ActivityService) QueryByUser(ctx context.Context, userID string, q audit.Query) ([]audit.UserActivity, error) {
	q.UserID = userID
	return s.Query(ctx, q)
}

// Stats returns aggregate counts (total / by-outcome / by-platform) scoped by
// the query filters. Aggregates contain no PII, so no masking is applied.
func (s *ActivityService) Stats(ctx context.Context, q audit.Query) (audit.ActivityStats, error) {
	stats, err := s.store.Stats(ctx, q)
	if err != nil {
		return stats, fmt.Errorf("audit stats: %w", err)
	}
	return stats, nil
}

// QueryPrincipal expands a canonical principal user ID into all explicitly
// linked platform-native audit subjects, then queries the unified timeline.
func (s *ActivityService) QueryPrincipal(ctx context.Context, principalUserID string, q audit.Query) ([]audit.UserActivity, []string, error) {
	userIDs, err := s.ResolvePrincipalUserIDs(ctx, principalUserID)
	if err != nil {
		return nil, nil, err
	}
	q.UserID = ""
	q.UserIDs = userIDs
	rows, err := s.Query(ctx, q)
	return rows, userIDs, err
}

// ResolvePrincipalUserIDs returns the principal itself plus linked subjects.
// This is intentionally explicit: no email/name fuzzy matching is performed.
func (s *ActivityService) ResolvePrincipalUserIDs(ctx context.Context, principalUserID string) ([]string, error) {
	principalUserID = strings.TrimSpace(principalUserID)
	if principalUserID == "" {
		return nil, fmt.Errorf("principal_user_id is required")
	}
	links, err := s.store.ListIdentityLinks(ctx, principalUserID)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{principalUserID: {}}
	out := []string{principalUserID}
	for _, link := range links {
		subject := strings.TrimSpace(link.Subject)
		if subject == "" {
			continue
		}
		if _, ok := seen[subject]; ok {
			continue
		}
		seen[subject] = struct{}{}
		out = append(out, subject)
	}
	return out, nil
}

func (s *ActivityService) ListIdentityLinks(ctx context.Context, principalUserID string) ([]audit.IdentityLink, error) {
	return s.store.ListIdentityLinks(ctx, principalUserID)
}

func (s *ActivityService) ListAllIdentityLinks(ctx context.Context) ([]audit.IdentityLink, error) {
	return s.store.ListIdentityLinks(ctx, "")
}

func (s *ActivityService) UpsertIdentityLink(ctx context.Context, link audit.IdentityLink) error {
	return s.store.UpsertIdentityLink(ctx, link)
}

func (s *ActivityService) DeleteIdentityLink(ctx context.Context, id string) error {
	return s.store.DeleteIdentityLink(ctx, id)
}

// Export returns the audit rows in the requested format ("json" or "csv").
// An empty format defaults to JSON. PII is included only when q.IncludePII is
// true; the HTTP layer gates that flag on admin:write scope. The caller (HTTP
// handler) is responsible for emitting the system.audit_export meta-audit row.
func (s *ActivityService) Export(ctx context.Context, q audit.Query, format, exporterUserID string) ([]byte, string, error) {
	if exporterUserID == "" {
		exporterUserID = audit.AnonymousUserID
	}
	rows, err := s.Query(ctx, q)
	if err != nil {
		return nil, "", fmt.Errorf("audit export query: %w", err)
	}
	var data []byte
	var contentType string
	switch format {
	case "csv":
		data, err = encodeCSV(rows)
		if err != nil {
			return nil, "", fmt.Errorf("audit export csv: %w", err)
		}
		contentType = "text/csv; charset=utf-8"
	case "json", "":
		data, err = encodeJSON(rows)
		if err != nil {
			return nil, "", fmt.Errorf("audit export json: %w", err)
		}
		contentType = "application/json; charset=utf-8"
	default:
		return nil, "", fmt.Errorf("audit export: unknown format %q", format)
	}
	s.log.Info("audit export",
		"exporter", exporterUserID,
		"format", format,
		"rows", len(rows),
		"user_id_filter", q.UserID,
	)
	return data, contentType, nil
}

// maskPII redacts ip (last octet → 0 for IPv4, /64 for IPv6) and truncates
// user_agent to browser family (first 50 chars + "..."). Per spec §5.9.
//
// IPv6 handling uses net.ParseIP so that all notations — full form, shorthand
// (::1, fe80::1), and IPv4-mapped (::ffff:192.168.1.1) — are masked correctly.
func maskPII(ua *audit.UserActivity) {
	if ua.IP != "" {
		if ip := net.ParseIP(ua.IP); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				ua.IP = ip4.Mask(net.CIDRMask(24, 32)).String()
			} else {
				// IPv6: zero the interface/host bits beyond the /64 prefix.
				ua.IP = ip.Mask(net.CIDRMask(64, 128)).String()
			}
		}
		// If the string isn't a valid IP literal (rare — could be a
		// malformed actor input), leave it untouched rather than guess.
	}
	if ua.UserAgent != "" && len(ua.UserAgent) > 50 {
		ua.UserAgent = ua.UserAgent[:50] + "..."
	}
}

func encodeJSON(rows []audit.UserActivity) ([]byte, error) {
	return json.MarshalIndent(rows, "", "  ")
}

func encodeCSV(rows []audit.UserActivity) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{
		"chain_epoch", "id", "ts", "user_id", "user_id_type", "platform", "session_id",
		"action", "resource_type", "resource_id", "outcome",
		"ip", "user_agent", "self_hash",
	}); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if err := w.Write([]string{
			sanitizeCSVCell(r.ChainEpoch),
			fmt.Sprintf("%d", r.ID),
			time.UnixMilli(r.Ts).UTC().Format(time.RFC3339Nano),
			sanitizeCSVCell(r.UserID),
			sanitizeCSVCell(r.UserIDType),
			sanitizeCSVCell(r.Platform),
			sanitizeCSVCell(r.SessionID),
			sanitizeCSVCell(r.Action),
			sanitizeCSVCell(r.ResourceType),
			sanitizeCSVCell(r.ResourceID),
			sanitizeCSVCell(r.Outcome),
			sanitizeCSVCell(r.IP),
			sanitizeCSVCell(r.UserAgent),
			sanitizeCSVCell(r.SelfHash),
		}); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// csvFormulaChars are the leading characters spreadsheet applications treat
// as formula introducers. Prefixing with a single quote forces the cell to
// be treated as text, neutralizing formula-injection payloads (OWASP CSV
// Injection / CWE-1236). The user_id and user_agent fields are fully
// attacker-controllable, so every text cell is sanitized defensively.
var csvFormulaChars = string([]byte{'=', '+', '-', '@', '\t', '\r'})

// sanitizeCSVCell prevents CSV formula injection (CWE-1236). If a cell
// value begins with a character that spreadsheet apps interpret as a
// formula (=, +, -, @, tab, CR), a single quote is prepended so the cell
// renders as text. Empty values are returned as-is.
func sanitizeCSVCell(s string) string {
	if s == "" {
		return ""
	}
	if strings.ContainsRune(csvFormulaChars, rune(s[0])) {
		return "'" + s
	}
	return s
}
