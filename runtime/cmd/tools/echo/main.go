package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

type request struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

func main() {
	message := flag.String("message", "", "message to echo")
	flag.Parse()

	msg := *message
	data, _ := io.ReadAll(os.Stdin)
	if len(bytes.TrimSpace(data)) > 0 {
		var req request
		if json.Unmarshal(data, &req) == nil {
			if val, ok := req.Args["message"].(string); ok {
				msg = val
			}
		}
	}

	if msg != "" {
		fmt.Fprint(os.Stdout, msg)
	}
}
