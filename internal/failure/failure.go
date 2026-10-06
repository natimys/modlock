// Package failure defines stable machine-readable operation errors.
package failure

import (
	"context"
	"errors"
)

const (
	Network             = "network"
	Git                 = "git_authorization"
	UnsupportedFormat   = "unsupported_format"
	UnsupportedProtocol = "unsupported_protocol"
	Conflict            = "file_conflict"
	Busy                = "instance_busy"
	Recovery            = "recovery_failed"
	Cancelled           = "cancelled"
	InvalidRequest      = "invalid_request"
	Internal            = "internal"
)

type Error struct {
	Code  string
	Cause error
}

func (e *Error) Error() string { return e.Cause.Error() }
func (e *Error) Unwrap() error { return e.Cause }
func Wrap(code string, cause error) error {
	if cause == nil {
		return nil
	}
	return &Error{Code: code, Cause: cause}
}

func Code(err error) string {
	if errors.Is(err, context.Canceled) {
		return Cancelled
	}
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return Internal
}
