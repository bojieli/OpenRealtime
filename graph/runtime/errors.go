// Package runtime mounts and executes immutable OpenRealtime Graph IR.
package runtime

import "errors"

var (
	ErrChannelClosed   = errors.New("graph channel is closed")
	ErrPortUnbound     = errors.New("graph port has no connected lane")
	ErrPortCardinality = errors.New("graph port operation does not match its lane cardinality")
	ErrAlreadyRunning  = errors.New("graph mount is already running or has completed")
	ErrGraphClosed     = errors.New("graph mount closed")
)
