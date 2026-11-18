//go:build windows

package selfupdate

import (
	"errors"
	"strings"
	"syscall"
	"unsafe"
)

// Through syscall rather than golang.org/x/sys/windows, like launcher's Job
// Object: one call does not justify a third dependency.
var procShellExecuteExW = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteExW")

// SHELLEXECUTEINFOW; Go lays the fields out with the same padding as the C struct.
type shellExecuteInfo struct {
	size       uint32
	mask       uint32
	hwnd       uintptr
	verb       *uint16
	file       *uint16
	parameters *uint16
	directory  *uint16
	show       int32
	instApp    uintptr
	idList     uintptr
	class      *uint16
	keyClass   uintptr
	hotKey     uint32
	icon       uintptr
	process    syscall.Handle
}

const (
	seeMaskNoCloseProcess = 0x40
	swHide                = 0
	errorCancelled        = 1223
)

// RunElevated runs exe with args as administrator, through the UAC prompt, and
// waits for it. Its console is hidden: it reports through its own arguments.
func RunElevated(exe string, args []string) (exitCode uint32, err error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return 0, err
	}
	params, err := syscall.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		return 0, err
	}
	info := shellExecuteInfo{mask: seeMaskNoCloseProcess, verb: verb, file: file, parameters: params, show: swHide}
	info.size = uint32(unsafe.Sizeof(info))
	if ok, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		if errors.Is(callErr, syscall.Errno(errorCancelled)) {
			return 0, errors.New("the administrator prompt was declined")
		}
		return 0, callErr
	}
	defer syscall.CloseHandle(info.process)
	if _, err := syscall.WaitForSingleObject(info.process, syscall.INFINITE); err != nil {
		return 0, err
	}
	err = syscall.GetExitCodeProcess(info.process, &exitCode)
	return exitCode, err
}
