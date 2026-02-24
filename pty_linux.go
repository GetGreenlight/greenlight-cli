//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	ioctlReadTermios  = syscall.TCGETS
	ioctlWriteTermios = syscall.TCSETS
)

func ptrOf(v *syscall.Termios) unsafe.Pointer {
	return unsafe.Pointer(v)
}

func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	// unlockpt
	var unlock int
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("unlockpt: %w", errno)
	}

	// ptsname
	var ptyno uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptyno))); errno != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("ptsname: %w", errno)
	}

	slaveName := "/dev/pts/" + strconv.Itoa(int(ptyno))
	s, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("open slave %s: %w", slaveName, err)
	}

	return m, s, nil
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
