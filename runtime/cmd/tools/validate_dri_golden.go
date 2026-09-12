//go:build ignore
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
)

func main() {
	p, err := parser.New(platform.Real())
	if err != nil { fmt.Println("new:", err); os.Exit(1) }
	bad := 0
	for _, path := range os.Args[1:] {
		_, err := p.Parse(context.Background(), path)
		if err != nil {
			fmt.Printf("ERR %s\n  %s\n", path, err)
			bad++
			continue
		}
		fmt.Printf("OK  %s\n", path)
	}
	if bad > 0 { os.Exit(2) }
}
