//go:build windows

package terminal

import "os"

func terminalWidth(*os.File) int {
	return 0
}
