package codexcli

// responseWaitError means the frame was written, but its response was not
// observed within the caller's budget. Keep this phase distinct from a failed
// stdin write: only the latter is evidence for singleton write-stall recovery.
type responseWaitError struct{ cause error }

func (e *responseWaitError) Error() string { return e.cause.Error() }
func (e *responseWaitError) Unwrap() error { return e.cause }
