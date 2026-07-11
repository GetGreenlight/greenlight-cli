//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// ttyPathFromFD resolves the filesystem path backing fd via fcntl(F_GETPATH).
// macOS's /dev/fd/<n> entries are not symlinks (os.Readlink on them fails
// with EINVAL), so F_GETPATH is the only reliable way to recover the
// controlling tty's device path (e.g. /dev/ttys003) from a live fd.
func ttyPathFromFD(fd int) (string, error) {
	var buf [1024]byte
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", errno
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), nil
}
