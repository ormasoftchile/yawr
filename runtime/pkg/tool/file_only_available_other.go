//go:build !windows || !amd64

package tool

func FileOnlySubprocessAvailable() bool { return false }
