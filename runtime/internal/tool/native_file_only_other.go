//go:build !windows || !amd64

package tool

import (
	"context"
	"fmt"
	"os"
)

func openPackageRegularFile(root, relative string) (*os.File, string, error) {
	return nil, "", fmt.Errorf("unsupported platform")
}

func executeFileOnly(ctx context.Context, command string, args []string, sandbox string, limit int64) ([]byte, []byte, int, error) {
	return nil, nil, -1, fmt.Errorf("unsupported platform")
}
