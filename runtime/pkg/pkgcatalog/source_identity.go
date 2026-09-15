package pkgcatalog

import (
	"io/fs"
	"os"
	"reflect"
)

func sameSourceIdentity(left, right fs.FileInfo) bool {
	if left == nil || right == nil {
		return false
	}
	if os.SameFile(left, right) {
		return true
	}
	// Captured and virtual sources retain their own FileInfo identity; they
	// must not be checked against an unrelated file on the host filesystem.
	return reflect.ValueOf(left).Comparable() && reflect.ValueOf(right).Comparable() && left == right
}
