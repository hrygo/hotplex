INSERT INTO turns (session_id, client_message_id, generation, turn_num, seq, role, content,
    platform, user_id, model, success, source, tools_json, tool_count,
    tokens_input, tokens_cache_write, tokens_cache_read, tokens_out,
    duration_ms, cost_usd, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
