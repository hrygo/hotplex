package codexcli

import (
	"context"
	"fmt"

	"github.com/hrygo/hotplex/internal/worker"
)

type ServerCommander struct {
	manager  *CodexAppServerManager
	threadID string
}

func NewServerCommander(manager *CodexAppServerManager, threadID string) *ServerCommander {
	return &ServerCommander{manager: manager, threadID: threadID}
}

func (sc *ServerCommander) SendControlRequest(ctx context.Context, subtype string, body map[string]any) (map[string]any, error) {
	switch subtype {
	case "set_model":
		return nil, fmt.Errorf("codexcli: set_model not supported: %w", worker.ErrNotImplemented)
	case "set_permission_mode":
		return nil, worker.ErrNotImplemented
	case "get_context_usage":
		return sc.manager.LastContextUsage(sc.threadID), nil
	case "mcp_status":
		resp, err := sc.manager.ListMCPServerStatusContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("codexcli: mcp_status: %w", err)
		}
		return map[string]any{"status": resp}, nil
	case "mcp_refresh":
		if err := sc.manager.RefreshMCPServerContext(ctx); err != nil {
			return nil, fmt.Errorf("codexcli: mcp_refresh: %w", err)
		}
		return map[string]any{"status": "ok"}, nil
	case "mcp_oauth":
		name, _ := body["server_name"].(string)
		if name == "" {
			return nil, fmt.Errorf("codexcli: mcp_oauth: missing server_name")
		}
		resp, err := sc.manager.MCPServerOAuthLoginContext(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("codexcli: mcp_oauth: %w", err)
		}
		return map[string]any{"oauth_url": string(resp)}, nil
	default:
		return nil, fmt.Errorf("codexcli: unknown control subtype: %s", subtype)
	}
}

func (sc *ServerCommander) Compact(ctx context.Context, _ map[string]any) error {
	_, err := sc.manager.CompactThreadContext(ctx, sc.threadID)
	if err != nil {
		return fmt.Errorf("codexcli: compact: %w", err)
	}
	return nil
}
