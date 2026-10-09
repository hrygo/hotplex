-- Latest N events by insertion id (DESC, reversed to ASC in Go).
-- Uses id (monotonic auto-increment) rather than seq so the newest events are
-- always returned even when seq was reset by a pre-issue-#879 WS reconnect —
-- seq DESC would bury low-seq new events under the LIMIT. Mirrors
-- turns.query_latest's ORDER BY t.id DESC. Correct because flushBoth now sends
-- deltas in firstSeq order, so insertion order (id) matches logical seq order.
SELECT id, session_id, seq, type, data, direction, source, created_at, expires_at
FROM events
WHERE session_id = ?
  AND ((expires_at > 0 AND expires_at > ?) OR (expires_at = 0 AND created_at > ?))
ORDER BY id DESC
LIMIT ?
