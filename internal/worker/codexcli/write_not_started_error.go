package codexcli

// writeNotStartedError means the encoder never took ownership of this frame.
// It is distinct from cancellation during a possibly partial pipe write, which
// may require process recovery. It does not authorize automatic input replay.
type writeNotStartedError struct{ cause error }

func (e *writeNotStartedError) Error() string { return e.cause.Error() }
func (e *writeNotStartedError) Unwrap() error { return e.cause }
