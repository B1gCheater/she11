// Package commander: stable machine-readable failure classes.
//
// The text of an error is for humans reading a terminal; error_code is for a
// caller (an AI, a script) that has to decide what to do next. They are
// independent: the same code can carry a different message, and the message
// must never be parsed to recover the class.
package commander

import (
	"errors"
	"fmt"

	"she11/src/shared/proto"
)

// Failure classes exposed in structuredContent.error_code. An empty string
// means "no failure" on a success result.
const (
	errCodeNone            = ""
	errCodeNoRelay         = "no_relay_configured"
	errCodeRelayDown       = "relay_unreachable"
	errCodeTargetNotFound  = "target_not_found"
	errCodeTargetAmbiguous = "target_ambiguous"
	errCodeClientOffline   = "client_offline"
	errCodeShellMissing    = "shell_unavailable"
	errCodeSessionLimit    = "session_limit"
	errCodeOpenTimeout     = "open_timeout"
	errCodeAuthDenied      = "auth_denied"
	errCodeProtoMismatch   = "protocol_mismatch"
	errCodeClientRejected  = "client_rejected"
	errCodeExecTimeout     = "exec_timeout"
	errCodeLost            = "connection_lost"
	errCodeDrainTimeout    = "output_drain_timeout"
	errCodeSessionNotFound = "session_not_found"
	errCodeCapability      = "capability_unsupported"
	errCodeInvalidArgument = "invalid_argument"
	errCodeInternal        = "internal"
)

// codedError attaches a stable class to an error without changing its text.
type codedError struct {
	code string
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

func codedErrorf(code, format string, args ...interface{}) error {
	return &codedError{code: code, err: fmt.Errorf(format, args...)}
}

// errCodeOf returns the stable class of err, falling back to "internal" for a
// failure that was never classified. A classified error anywhere in the chain
// wins, which is what lets callers wrap without losing the code.
func errCodeOf(err error) string {
	if err == nil {
		return errCodeNone
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	var oe *openError
	if errors.As(err, &oe) {
		return codeOfOpen(oe.code)
	}
	return errCodeInternal
}

// codeOfOpen maps a protocol rejection to the same vocabulary the commander
// uses, so an AI does not have to learn two taxonomies.
func codeOfOpen(code int) string {
	switch code {
	case proto.CodeClientOffline:
		return errCodeClientOffline
	case proto.CodeClientDenied, proto.CodeAuthzDenied, proto.CodeBadMagic, proto.CodeBadPassword, proto.CodeNotEnrolled:
		return errCodeAuthDenied
	case proto.CodeShellUnavailable, proto.CodeNoShell:
		return errCodeShellMissing
	case proto.CodeSessionLimit:
		return errCodeSessionLimit
	case proto.CodeServerBusy:
		return errCodeRelayDown
	case proto.CodeTimeout:
		return errCodeOpenTimeout
	case proto.CodeProtoMismatch:
		return errCodeProtoMismatch
	case proto.CodeExecCwd, proto.CodeExecEnv:
		return errCodeInvalidArgument
	default:
		return errCodeClientRejected
	}
}
