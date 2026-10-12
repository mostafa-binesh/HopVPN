package main

import (
	"errors"
	"testing"
)

type timeoutTestError struct{}

func (timeoutTestError) Error() string   { return "i/o timeout" }
func (timeoutTestError) Timeout() bool   { return true }
func (timeoutTestError) Temporary() bool { return true }

func TestProbeErrorClassifiesWrappedNetworkTimeout(t *testing.T) {
	err := errors.Join(errors.New("upstream handshake"), timeoutTestError{})
	if got := probeError(err); got != "connection timed out" {
		t.Fatalf("probeError() = %q, want connection timed out", got)
	}
}
