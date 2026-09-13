//go:build windows && amd64

package tool

import (
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var fileOnlyAvailability struct {
	sync.Once
	ok bool
}

// FileOnlySubprocessAvailable reports whether the Windows AppContainer
// primitives required by native-file-only are present.
func FileOnlySubprocessAvailable() bool {
	fileOnlyAvailability.Do(func() {
		userenv := windows.NewLazySystemDLL("userenv.dll")
		create := userenv.NewProc("CreateAppContainerProfile")
		deleteProfile := userenv.NewProc("DeleteAppContainerProfile")
		if create.Find() != nil || deleteProfile.Find() != nil {
			return
		}
		name := fmt.Sprintf("yawr.fileonly.probe.%d.%d", os.Getpid(), time.Now().UnixNano())
		namePtr, _ := windows.UTF16PtrFromString(name)
		displayPtr, _ := windows.UTF16PtrFromString("Yawr file-only capability probe")
		descPtr, _ := windows.UTF16PtrFromString("Ephemeral capability probe")
		var sid *windows.SID
		result, _, _ := create.Call(
			uintptr(unsafe.Pointer(namePtr)),
			uintptr(unsafe.Pointer(displayPtr)),
			uintptr(unsafe.Pointer(descPtr)),
			0, 0,
			uintptr(unsafe.Pointer(&sid)),
		)
		if int32(result) < 0 || sid == nil {
			return
		}
		defer windows.LocalFree(windows.Handle(unsafe.Pointer(sid)))
		deleted, _, _ := deleteProfile.Call(uintptr(unsafe.Pointer(namePtr)))
		fileOnlyAvailability.ok = int32(deleted) >= 0
	})
	return fileOnlyAvailability.ok
}
