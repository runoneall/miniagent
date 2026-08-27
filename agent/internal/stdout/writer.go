package stdout

import (
	"io"
	"os"
)

type writer struct {
	out         io.Writer
	newLineFlag bool
}

var Writer = &writer{
	out:         os.Stdout,
	newLineFlag: false,
}
