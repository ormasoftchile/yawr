//go:build windows && amd64

package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

const (
	procThreadAttributeSecurityCapabilities = 0x00020009
	createSuspended                         = 0x00000004
	createNoWindow                          = 0x08000000
	createUnicodeEnvironment                = 0x00000400
	extendedStartupInfoPresent              = 0x00080000
	jobObjectLimitActiveProcess             = 0x00000008
	jobObjectLimitKillOnJobClose            = 0x00002000
)

func init() {
	protectStagedExecutable = protectStagedExecutableWindows
}

func protectStagedExecutableWindows(path, expectedDigest string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_EXECUTE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if err := verifyOpenFileDigest(file, expectedDigest); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

type securityCapabilities struct {
	appContainerSID *windows.SID
	capabilities    unsafe.Pointer
	count           uint32
	reserved        uint32
}

var (
	userenvDLL                    = windows.NewLazySystemDLL("userenv.dll")
	createAppContainerProfileProc = userenvDLL.NewProc("CreateAppContainerProfile")
	deleteAppContainerProfileProc = userenvDLL.NewProc("DeleteAppContainerProfile")
)

func openPackageRegularFile(root, relative string) (*os.File, string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return nil, "", fmt.Errorf("path must be non-empty and package-relative")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("path traversal is forbidden")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	rootAttrs, err := windows.GetFileAttributes(syscall.StringToUTF16Ptr(rootAbs))
	if err != nil {
		return nil, "", err
	}
	if rootAttrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, "", fmt.Errorf("package root reparse points are forbidden")
	}
	for current := rootAbs; ; {
		if current != rootAbs {
			attrs, err := windows.GetFileAttributes(syscall.StringToUTF16Ptr(current))
			if err != nil {
				return nil, "", err
			}
			if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return nil, "", fmt.Errorf("reparse points are forbidden")
			}
		}
		rel, _ := filepath.Rel(rootAbs, current)
		if rel == clean {
			break
		}
		nextPart := strings.Split(strings.TrimPrefix(clean, rel+string(filepath.Separator)), string(filepath.Separator))[0]
		current = filepath.Join(current, nextPart)
		if current == filepath.Join(rootAbs, clean) {
			attrs, err := windows.GetFileAttributes(syscall.StringToUTF16Ptr(current))
			if err != nil {
				return nil, "", err
			}
			if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return nil, "", fmt.Errorf("reparse points are forbidden")
			}
			break
		}
	}
	f, err := os.Open(filepath.Join(rootAbs, clean))
	if err != nil {
		return nil, "", err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, "", fmt.Errorf("path is not a regular file")
	}
	final, err := finalPath(windows.Handle(f.Fd()))
	if err != nil {
		f.Close()
		return nil, "", err
	}
	finalRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		f.Close()
		return nil, "", err
	}
	inside, err := filepath.Rel(finalRoot, final)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		f.Close()
		return nil, "", fmt.Errorf("resolved path escapes package root")
	}
	return f, clean, nil
}

func finalPath(handle windows.Handle) (string, error) {
	buf := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(handle, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	path := syscall.UTF16ToString(buf[:n])
	path = strings.TrimPrefix(path, `\\?\`)
	return filepath.Clean(path), nil
}

func executeFileOnly(ctx context.Context, command string, args []string, sandbox string, limit int64) ([]byte, []byte, int, error) {
	return executeFileOnlyWithEnvironment(ctx, command, args, sandbox, limit, nil)
}

func executeFileOnlyWithEnvironment(ctx context.Context, command string, args []string, sandbox string, limit int64, additionalEnvironment map[string]string) ([]byte, []byte, int, error) {
	profile := "yawr.fileonly." + strings.ReplaceAll(uuid.NewString(), "-", "")
	sid, err := createAppContainer(profile)
	if err != nil {
		return nil, nil, -1, fmt.Errorf("create AppContainer: %w", err)
	}
	defer func() {
		deleteAppContainer(profile)
		windows.LocalFree(windows.Handle(unsafe.Pointer(sid)))
	}()
	if err := grantPath(sandbox, sid, windows.GENERIC_READ|windows.GENERIC_EXECUTE, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT); err != nil {
		return nil, nil, -1, fmt.Errorf("grant sandbox read/execute: %w", err)
	}
	scratch := filepath.Join(sandbox, "scratch")
	if err := grantPath(scratch, sid, windows.GENERIC_ALL, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT); err != nil {
		return nil, nil, -1, fmt.Errorf("grant scratch access: %w", err)
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, nil, -1, fmt.Errorf("create job: %w", err)
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = jobObjectLimitActiveProcess | jobObjectLimitKillOnJobClose
	limits.BasicLimitInformation.ActiveProcessLimit = 1
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, nil, -1, fmt.Errorf("configure job: %w", err)
	}

	stdoutRead, stdoutWrite, err := inheritablePipe()
	if err != nil {
		return nil, nil, -1, err
	}
	defer func() {
		if stdoutRead != 0 {
			windows.CloseHandle(stdoutRead)
		}
	}()
	defer func() {
		if stdoutWrite != 0 {
			windows.CloseHandle(stdoutWrite)
		}
	}()
	stderrRead, stderrWrite, err := inheritablePipe()
	if err != nil {
		return nil, nil, -1, err
	}
	defer func() {
		if stderrRead != 0 {
			windows.CloseHandle(stderrRead)
		}
	}()
	defer func() {
		if stderrWrite != 0 {
			windows.CloseHandle(stderrWrite)
		}
	}()
	stdinRead, stdinWrite, err := inheritableInputPipe()
	if err != nil {
		return nil, nil, -1, err
	}
	windows.CloseHandle(stdinWrite)
	defer func() {
		if stdinRead != 0 {
			windows.CloseHandle(stdinRead)
		}
	}()

	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, nil, -1, err
	}
	defer attrs.Delete()
	caps := securityCapabilities{appContainerSID: sid}
	if err := attrs.Update(procThreadAttributeSecurityCapabilities, unsafe.Pointer(&caps), unsafe.Sizeof(caps)); err != nil {
		return nil, nil, -1, fmt.Errorf("set AppContainer attribute: %w", err)
	}
	inheritedHandles := []windows.Handle{stdinRead, stdoutWrite, stderrWrite}
	if err := attrs.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&inheritedHandles[0]),
		uintptr(len(inheritedHandles))*unsafe.Sizeof(inheritedHandles[0]),
	); err != nil {
		return nil, nil, -1, fmt.Errorf("set inherited handle allowlist: %w", err)
	}
	si := windows.StartupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput = stdinRead
	si.StdOutput = stdoutWrite
	si.StdErr = stderrWrite
	si.ProcThreadAttributeList = attrs.List()
	commandLine, _ := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{command}, args...)))
	app, _ := windows.UTF16PtrFromString(command)
	dir, _ := windows.UTF16PtrFromString(sandbox)
	environment := map[string]string{
		"ALLUSERSPROFILE": os.Getenv("ALLUSERSPROFILE"),
		"APPDATA":         os.Getenv("APPDATA"),
		"LOCALAPPDATA":    os.Getenv("LOCALAPPDATA"),
		"ProgramData":     os.Getenv("ProgramData"),
		"SystemDrive":     os.Getenv("SystemDrive"),
		"SystemRoot":      os.Getenv("SystemRoot"),
		"TEMP":            scratch,
		"TMP":             scratch,
		"USERPROFILE":     os.Getenv("USERPROFILE"),
		"WINDIR":          os.Getenv("WINDIR"),
	}
	for key, value := range additionalEnvironment {
		environment[key] = value
	}
	env := environmentBlock(environment)
	var pi windows.ProcessInformation
	flags := uint32(createSuspended | createNoWindow | createUnicodeEnvironment | extendedStartupInfoPresent)
	if err := windows.CreateProcess(app, commandLine, nil, nil, true, flags, &env[0], dir, &si.StartupInfo, &pi); err != nil {
		return nil, nil, -1, fmt.Errorf("create restricted process: %w", err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	windows.CloseHandle(stdoutWrite)
	stdoutWrite = 0
	windows.CloseHandle(stderrWrite)
	stderrWrite = 0
	windows.CloseHandle(stdinRead)
	stdinRead = 0
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		return nil, nil, -1, fmt.Errorf("assign process to job: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateJobObject(job, 1)
		windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		return nil, nil, -1, fmt.Errorf("resume restricted process: %w", err)
	}

	var overflow atomic.Bool
	var outBuf, errBuf bytes.Buffer
	stdoutFile := os.NewFile(uintptr(stdoutRead), "stdout")
	stdoutRead = 0
	defer stdoutFile.Close()
	stderrFile := os.NewFile(uintptr(stderrRead), "stderr")
	stderrRead = 0
	defer stderrFile.Close()
	readErr := make(chan error, 2)
	go func() {
		err := copyBounded(&outBuf, stdoutFile, limit, &overflow)
		if err != nil {
			windows.TerminateJobObject(job, 1)
		}
		readErr <- err
	}()
	go func() {
		err := copyBounded(&errBuf, stderrFile, limit, &overflow)
		if err != nil {
			windows.TerminateJobObject(job, 1)
		}
		readErr <- err
	}()
	waited := make(chan error, 1)
	go func() {
		_, waitErr := windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		waited <- waitErr
	}()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		windows.TerminateJobObject(job, 1)
		waitErr = <-waited
	}
	// Closing the job after the root exits confirms there is no surviving tree.
	windows.TerminateJobObject(job, 1)
	for i := 0; i < 2; i++ {
		<-readErr
	}
	var exit uint32
	if err := windows.GetExitCodeProcess(pi.Process, &exit); err != nil {
		return outBuf.Bytes(), errBuf.Bytes(), -1, err
	}
	if ctx.Err() != nil {
		return outBuf.Bytes(), errBuf.Bytes(), int(exit), ctx.Err()
	}
	if overflow.Load() {
		return outBuf.Bytes(), errBuf.Bytes(), int(exit), fmt.Errorf("captured output exceeds %d bytes", limit)
	}
	if waitErr != nil {
		return outBuf.Bytes(), errBuf.Bytes(), int(exit), waitErr
	}
	return outBuf.Bytes(), errBuf.Bytes(), int(exit), nil
}

func inheritablePipe() (windows.Handle, windows.Handle, error) {
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, &sa, 0); err != nil {
		return 0, 0, err
	}

	if err := windows.SetHandleInformation(read, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(read)
		windows.CloseHandle(write)
		return 0, 0, err
	}
	return read, write, nil
}

func inheritableInputPipe() (windows.Handle, windows.Handle, error) {
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, &sa, 0); err != nil {
		return 0, 0, err
	}
	if err := windows.SetHandleInformation(write, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(read)
		windows.CloseHandle(write)
		return 0, 0, err
	}
	return read, write, nil
}

func copyBounded(dst *bytes.Buffer, src io.Reader, limit int64, overflow *atomic.Bool) error {
	n, err := io.CopyN(dst, src, limit+1)
	if n > limit {
		overflow.Store(true)
		return fmt.Errorf("output overflow")
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func environmentBlock(values map[string]string) []uint16 {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if value != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var block []uint16
	for _, key := range keys {
		entry, _ := windows.UTF16FromString(key + "=" + values[key])
		block = append(block, entry...)
	}
	return append(block, 0)
}

func createAppContainer(name string) (*windows.SID, error) {
	namePtr, _ := windows.UTF16PtrFromString(name)
	displayPtr, _ := windows.UTF16PtrFromString("Yawr file-only subprocess")
	descPtr, _ := windows.UTF16PtrFromString("Ephemeral Yawr sandbox")
	var sid *windows.SID
	r1, _, _ := createAppContainerProfileProc.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(displayPtr)),
		uintptr(unsafe.Pointer(descPtr)),
		0, 0,
		uintptr(unsafe.Pointer(&sid)),
	)
	if int32(r1) < 0 {
		return nil, syscall.Errno(r1)
	}
	return sid, nil
}

func deleteAppContainer(name string) {
	namePtr, _ := windows.UTF16PtrFromString(name)
	deleteAppContainerProfileProc.Call(uintptr(unsafe.Pointer(namePtr)))
}

func grantPath(path string, sid *windows.SID, permissions windows.ACCESS_MASK, inheritance uint32) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: permissions,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, dacl)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
