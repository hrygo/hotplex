package eventstore

// Turn role constants.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// TurnWriteRequest is the write payload for a single turn row.
// Produced by Bridge (assistant turns on done, user turns on input),
// consumed by the Collector's background batch writer.
type TurnWriteRequest struct {
	SessionID        string
	ClientMessageID  string
	Generation       int64
	TurnNum          int
	Seq              int64
	Role             string // "user" | "assistant"
	Content          string
	Platform         string
	UserID           string
	Model            string
	Success          *bool // nil for user turns
	Source           string
	ToolsJSON        string // marshaled map[string]int
	ToolCount        int
	TokensInput      int64
	TokensCacheWrite int64
	TokensCacheRead  int64
	TokensOut        int64
	DurationMs       int64
	CostUSD          float64
	CreatedAt        int64 // Unix ms
	ExpiresAt        int64 // Unix ms; zero assigns the configured new-content deadline.
}

func nullableClientMessageID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
