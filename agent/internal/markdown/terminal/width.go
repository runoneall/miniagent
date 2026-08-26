package terminal

import (
	"io"
	"os"
)

func detectWrapWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}

	return terminalWidth(f)
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}
