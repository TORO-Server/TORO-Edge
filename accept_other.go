//go:build !unix

package main

import (
	"errors"
	"net"
	"syscall"
)

func isTemporaryAcceptError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENOMEM)
}
