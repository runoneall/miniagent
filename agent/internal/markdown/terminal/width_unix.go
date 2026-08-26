//go:build !windows

package terminal

import (
	"os"
	"syscall"
	"unsafe"
)

func terminalWidth(f *os.File) int {
	if f == nil {
		return 0
	}

	info, err := f.Stat()
	if err != nil || (info.Mode()&os.ModeCharDevice) == 0 {
		return 0
	}

	type winsize struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}

	ws := &winsize{}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(ws)))
	if errno != 0 || ws.Col == 0 {
		return 0
	}

	return int(ws.Col)
}
