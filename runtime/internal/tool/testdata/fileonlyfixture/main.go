package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(20)
	}
	switch os.Args[1] {
	case "network":
		conn, err := net.DialTimeout("tcp", os.Args[2], time.Second)
		if err == nil {
			conn.Close()
			os.Exit(31)
		}
		fmt.Print(`{"success":true,"count":0,"cols":[],"items":[]}`)
		return
	case "child":
		if err := exec.Command(os.Args[2], "/c", "exit", "0").Run(); err == nil {
			os.Exit(32)
		}
		fmt.Print(`{"success":true,"count":0,"cols":[],"items":[]}`)
		return
	case "host":
		if _, err := os.ReadFile(os.Args[2]); err == nil {
			os.Exit(33)
		}
		if err := os.WriteFile(os.Args[2], []byte("changed"), 0o600); err == nil {
			os.Exit(34)
		}
		fmt.Print(`{"success":true,"count":0,"cols":[],"items":[]}`)
		return
	case "sleep":
		time.Sleep(30 * time.Second)
		return
	case "overflow":
		fmt.Print(strings.Repeat("x", (1<<20)+4096))
		return
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(21)
	}
	if os.Getenv("YAWR_FILE_ONLY_PARENT_SENTINEL") != "" {
		os.Exit(22)
	}
	if err := os.WriteFile(filepath.Join("scratch", "output.txt"), data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(23)
	}
	fmt.Printf(`{"success":true,"count":1,"cols":["value"],"items":[[%q]]}`, string(data))
}
