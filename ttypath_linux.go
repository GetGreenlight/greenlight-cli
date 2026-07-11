//go:build linux

package main

import (
	"fmt"
	"os"
)

// ttyPathFromFD resolves the filesystem path backing fd. Unused today
// (spawnTerminalCloseHelper is a no-op on Linux, issue #273), but correct:
// /proc/self/fd/<n> entries are real symlinks on Linux, unlike macOS's
// /dev/fd/<n>.
func ttyPathFromFD(fd int) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
}
