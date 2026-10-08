UPDATE sessions
SET
    last_content_expires_at = CASE
        WHEN last_content_expires_at IS NULL OR last_content_expires_at < ? THEN ?
        ELSE last_content_expires_at
    END,
    history_expires_at = CASE
        WHEN history_expires_at IS NULL OR history_expires_at < ? THEN ?
        ELSE history_expires_at
    END
WHERE id = ?
  AND lifecycle_policy = 'v2'
  AND deleted_at IS NULL
