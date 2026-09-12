package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
)

type request struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

func main() {
	toolName := flag.String("tool", "stub", "tool name")
	flag.Parse()

	action := ""
	args := map[string]any{}
	data, _ := io.ReadAll(os.Stdin)
	if len(bytes.TrimSpace(data)) > 0 {
		var req request
		if json.Unmarshal(data, &req) == nil {
			action = req.Action
			args = req.Args
		}
	}

	out := map[string]any{
		"result": "ok",
		"tool":   *toolName,
		"action": action,
		"args":   args,
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(out)
}
