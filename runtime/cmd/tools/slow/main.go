package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

type request struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

func main() {
	delay := flag.Int("delay", 0, "delay seconds")
	flag.Parse()

	seconds := *delay
	data, _ := io.ReadAll(os.Stdin)
	if len(bytes.TrimSpace(data)) > 0 {
		var req request
		if json.Unmarshal(data, &req) == nil {
			if val, ok := req.Args["delay_seconds"]; ok {
				seconds = toInt(val, seconds)
			}
		}
	}

	if seconds > 0 {
		time.Sleep(time.Duration(seconds) * time.Second)
	}
	fmt.Fprint(os.Stdout, "ok")
}

func toInt(v any, fallback int) int {
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case int64:
		return int(val)
	default:
		return fallback
	}
}
