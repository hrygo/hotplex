SELECT id, session_id, client_message_id, generation, turn_num, seq, role, content,
       platform, user_id, model, success, source, tools_json, tool_count,
       tokens_input, tokens_cache_write, tokens_cache_read,
       (tokens_input + tokens_cache_write + tokens_cache_read) AS tokens_in,
       tokens_out, duration_ms, cost_usd, created_at
FROM turns
WHERE session_id = ? AND id < ?
  AND ((expires_at > 0 AND expires_at > ?) OR (expires_at = 0 AND created_at > ?))
ORDER BY id DESC
LIMIT ?
