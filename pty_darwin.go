//go:build darwin

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	ioctlReadTermios  = syscall.TIOCGETA
	ioctlWriteTermios = syscall.TIOCSETA
)

func ptrOf(v *syscall.Termios) unsafe.Pointer {
	return unsafe.Pointer(v)
}

func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	// grantpt
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCPTYGRANT, 0); errno != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("grantpt: %w", errno)
	}

	// unlockpt
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCPTYUNLK, 0); errno != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("unlockpt: %w", errno)
	}

	// ptsname
	var n [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&n[0]))); errno != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("ptsname: %w", errno)
	}

	slaveName := string(n[:clen(n[:])])
	s, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("open slave %s: %w", slaveName, err)
	}

	return m, s, nil
}

func clen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}

// Winsize matches the kernel struct winsize.
type Winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func getWinsize(fd uintptr) (*Winsize, error) {
	ws := &Winsize{}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(ws))); errno != 0 {
		return nil, errno
	}
	return ws, nil
}

func setWinsize(fd uintptr, ws *Winsize) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(ws))); errno != 0 {
		return errno
	}
	return nil
}
