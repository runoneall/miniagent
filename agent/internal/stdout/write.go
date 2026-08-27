package stdout

func (w *writer) Write(p []byte) (n int, err error) {
	var output []byte

	for _, b := range p {
		if b == '\r' || (b == '\n' && w.newLineFlag) {
			continue
		}

		output = append(output, b)
		w.newLineFlag = (b == '\n')
	}

	if len(output) > 0 {
		_, err = w.out.Write(output)
	}

	return len(p), err
}
