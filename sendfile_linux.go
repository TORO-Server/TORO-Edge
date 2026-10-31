//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"syscall"
	"unsafe"
)

// 大きい本文は、メモリ上のファイル (memfd) にも置いて、sendfile で送る。
// write で送ると、本文をカーネルのバッファへ写す分の CPU を使う。sendfile はページを渡すだけで、写さない (nginx と同じ)

const mfdCloexec = 1

// newBodyFile は本文を入れたメモリ上のファイルを作る。作れなければ nil
func newBodyFile(b []byte) (*os.File, syscall.RawConn) {
	if sysMemfdCreate == 0 {
		return nil, nil
	}
	name, err := syscall.BytePtrFromString("toro-edge")
	if err != nil {
		return nil, nil
	}
	fd, _, errno := syscall.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(name)), mfdCloexec, 0)
	if errno != 0 {
		return nil, nil
	}
	f := os.NewFile(fd, "memfd:toro-edge")
	if f == nil {
		syscall.Close(int(fd))
		return nil, nil
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		f.Close()
		return nil, nil
	}
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, nil
	}
	return f, rc
}

// rawConn はつなぎの raw 口 (つなぎごとに 1 度だけ求める)
func (cn *conn) rawConn() syscall.RawConn {
	if cn.rc == nil {
		if sc, ok := cn.nc.(syscall.Conn); ok {
			cn.rc, _ = sc.SyscallConn()
		}
	}
	return cn.rc
}

var errNoSendfile = errors.New("sendfile unavailable")

// sendFile はファイルの全部を sendfile で送る。何も送らないうちに sendfile が使えないと分かったときは、送った量 0 とエラーを返す
func (cn *conn) sendFile(v *Variant) (sent int64, err error) {
	rc := cn.rawConn()
	if rc == nil {
		return 0, errNoSendfile
	}
	remaining := int64(v.size)
	var off int64
	var serr error
	cerr := v.fileRC.Control(func(ffd uintptr) {
		werr := rc.Write(func(sfd uintptr) bool {
			for remaining > 0 {
				chunk := remaining
				if chunk > 1<<30 {
					chunk = 1 << 30
				}
				n, e := syscall.Sendfile(int(sfd), int(ffd), &off, int(chunk))
				if n > 0 {
					remaining -= int64(n)
					sent += int64(n)
				}
				switch e {
				case nil:
					if n == 0 {
						serr = io.ErrUnexpectedEOF
						return true
					}
				case syscall.EAGAIN:
					return false
				case syscall.EINTR:
				default:
					serr = e
					return true
				}
			}
			return true
		})
		if serr == nil {
			serr = werr
		}
	})
	if serr == nil {
		serr = cerr
	}
	return sent, serr
}

// readFile はファイルの中身を読む (sendfile が使えないときの代わり)
func (v *Variant) readFile() ([]byte, error) {
	b := make([]byte, v.size)
	if _, err := v.file.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b, nil
}
