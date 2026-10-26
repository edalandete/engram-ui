//go:build windows

package launcher

import (
	"os/exec"
	"syscall"
	"unsafe"
)

// Windows has no Pdeathsig. The equivalent is a Job Object with
// KILL_ON_JOB_CLOSE: the job's last handle closes when this process dies —
// however it dies, including Task Manager's "End task", a runtime panic or an
// OOM kill — and the kernel then terminates everything assigned to it.
//
// Without this, killing engram-ui hard leaves `engram serve` running: holding
// the same SQLite file, listening on a port nobody recorded, and still
// autosyncing if that is enabled. The next launch spawns another, and they
// accumulate with no way to find them.
//
// Driven through syscall.NewLazyDLL rather than golang.org/x/sys/windows on
// purpose. This repo has two dependencies and the win32 surface needed here is
// three calls; adding x/sys for them would be the larger cost.
var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW        = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj   = kernel32.NewProc("AssignProcessToJobObject")
)

const (
	classExtendedLimitInformation = 9
	limitKillOnJobClose           = 0x00002000

	// syscall exposes PROCESS_TERMINATE but not PROCESS_SET_QUOTA, and
	// AssignProcessToJobObject needs both.
	processSetQuota = 0x0100
)

// jobBasicLimits mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION. Only LimitFlags is
// set; the other fields exist so the struct size matches what the kernel reads.
type jobBasicLimits struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// jobExtendedLimits mirrors JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type jobExtendedLimits struct {
	BasicLimitInformation jobBasicLimits
	IoInfo                jobIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// jobHandle is deliberately never closed: closing it is what kills the job, so
// it has to outlive everything but us. Process exit closes it for free.
var jobHandle uintptr

// prepareReap has nothing to do on Windows: a process can only join a Job
// Object once it exists, so all the work happens in adoptReap.
func prepareReap(cmd *exec.Cmd) {}

// adoptReap ties cmd's lifetime to this process.
//
// Best-effort: if the job cannot be created the child still runs, and Stop
// still kills it on an orderly exit. Only the guarantee for an abrupt exit is
// lost, which is exactly where we were before.
func adoptReap(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if jobHandle == 0 {
		h, _, _ := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			return
		}
		var info jobExtendedLimits
		info.BasicLimitInformation.LimitFlags = limitKillOnJobClose
		ok, _, _ := procSetInformationJobObject.Call(
			h,
			classExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			unsafe.Sizeof(info),
		)
		if ok == 0 {
			_ = syscall.CloseHandle(syscall.Handle(h))
			return
		}
		jobHandle = h
	}

	ph, err := syscall.OpenProcess(processSetQuota|syscall.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer syscall.CloseHandle(ph)
	_, _, _ = procAssignProcessToJobObj.Call(jobHandle, uintptr(ph))
}
