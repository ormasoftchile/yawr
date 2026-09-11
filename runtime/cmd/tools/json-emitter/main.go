package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

type request struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

func main() {
	args := map[string]any{}
	data, _ := io.ReadAll(os.Stdin)
	if len(bytes.TrimSpace(data)) > 0 {
		var req request
		if json.Unmarshal(data, &req) == nil {
			args = req.Args
		}
	}

	out := map[string]any{
		"result": "ok",
		"echo":   args,
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(out)
}
