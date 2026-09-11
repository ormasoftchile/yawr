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
	exitCode := flag.Int("exit-code", 1, "exit code")
	sleepSeconds := flag.Int("sleep", 0, "sleep seconds")
	flag.Parse()

	action := "fail"
	code := *exitCode
	sleep := *sleepSeconds

	data, _ := io.ReadAll(os.Stdin)
	if len(bytes.TrimSpace(data)) > 0 {
		var req request
		if json.Unmarshal(data, &req) == nil {
			action = req.Action
			if val, ok := req.Args["exit_code"]; ok {
				code = toInt(val, code)
			}
			if val, ok := req.Args["seconds"]; ok {
				sleep = toInt(val, sleep)
			}
		}
	}

	if action == "sleep" || sleep > 0 {
		time.Sleep(time.Duration(sleep) * time.Second)
		os.Exit(0)
	}

	fmt.Fprintln(os.Stderr, "error: forced failure")
	os.Exit(code)
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
