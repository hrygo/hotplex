-- Latest N turns of the newest generation, DESC by id (reversed to ASC in Go).
-- Used by GetHistory default path: returns the most recent turns, not the oldest.
-- Mirrors turns.query_with_gen's latest_gen CTE but flips ORDER BY to DESC and
-- drops OFFSET so LIMIT bounds the newest slice rather than the oldest.
WITH latest_gen AS (
    SELECT COALESCE(MAX(generation), 0) AS gen FROM turns WHERE session_id = ?
)
SELECT t.id, t.session_id, t.client_message_id, t.generation, t.turn_num, t.seq, t.role, t.content,
       t.platform, t.user_id, t.model, t.success, t.source, t.tools_json, t.tool_count,
       t.tokens_input, t.tokens_cache_write, t.tokens_cache_read,
       (t.tokens_input + t.tokens_cache_write + t.tokens_cache_read) AS tokens_in,
       t.tokens_out, t.duration_ms, t.cost_usd, t.created_at
FROM turns t, latest_gen lg
WHERE t.session_id = ? AND t.generation = lg.gen
  AND ((t.expires_at > 0 AND t.expires_at > ?) OR (t.expires_at = 0 AND t.created_at > ?))
ORDER BY t.id DESC
LIMIT ?
