package config

import (
	"encoding/json"
	"io"
)

const Indent = "    "

func jsonNewEncoder(w io.Writer) *json.Encoder {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", Indent)

	return encoder
}
