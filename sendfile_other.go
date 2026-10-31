//go:build !linux

package main

import (
	"errors"
	"os"
	"syscall"
)

// Linux 以外では sendfile を使わない (本文は write で送る)

func newBodyFile(b []byte) (*os.File, syscall.RawConn) { return nil, nil }

var errNoSendfile = errors.New("sendfile unavailable")

func (cn *conn) sendFile(v *Variant) (int64, error) { return 0, errNoSendfile }

func (v *Variant) readFile() ([]byte, error) { return nil, errNoSendfile }
