package cursor

import "errors"

var ErrTokenExpired = errors.New("token expired")

type transientUpstreamError struct {
	err error
}

func (e transientUpstreamError) Error() string {
	if e.err == nil {
		return "transient upstream error"
	}
	return e.err.Error()
}

func (e transientUpstreamError) Unwrap() error {
	return e.err
}
