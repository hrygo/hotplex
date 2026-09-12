package codexcli

import "errors"

// Native IDs must be validated before installing subscriptions or stop targets.
// The error deliberately does not contain native response contents.
var errInvalidLifecycleAck = errors.New("invalid native lifecycle acknowledgement")
