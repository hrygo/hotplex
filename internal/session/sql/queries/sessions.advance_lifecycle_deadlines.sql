UPDATE sessions
SET
    last_input_at = CASE WHEN last_input_at IS NULL OR last_input_at < ? THEN ? ELSE last_input_at END,
    archive_at = CASE WHEN archive_at IS NULL OR archive_at < ? THEN ? ELSE archive_at END,
    conversation_expires_at = CASE WHEN conversation_expires_at IS NULL OR conversation_expires_at < ? THEN ? ELSE conversation_expires_at END,
    history_expires_at = CASE WHEN history_expires_at IS NULL OR history_expires_at < ? THEN ? ELSE history_expires_at END,
    updated_at = CASE WHEN updated_at < ? THEN ? ELSE updated_at END
WHERE id = ?
  AND lifecycle_policy = ?
  AND lifecycle_policy_revision = ?
  AND deleted_at IS NULL
  AND state <> 'deleted';
