//go:build windows

package proc

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// tree is one stage and the job object holding it and its descendants.
type tree struct {
	cmd    *exec.Cmd
	job    windows.Handle
	killed bool // stop terminated the job, so the exit code is ours, not the stage's
}

// A console-less daemon would open a console window for every console child
// it starts; CREATE_NO_WINDOW prevents that. CREATE_SUSPENDED keeps the child
// from starting anything before it is inside its job.
func prepare(cmd *exec.Cmd) *tree {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW,
	}
	return &tree{cmd: cmd}
}

func (t *tree) start() error {
	if err := procNtResumeProcess.Find(); err != nil {
		return fmt.Errorf("resume entry point: %w", err)
	}
	job, err := killOnCloseJob()
	if err != nil {
		return err
	}
	if err := t.cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return err
	}
	fail := func(err error) error {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
		windows.CloseHandle(job)
		return err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(t.cmd.Process.Pid))
	if err != nil {
		return fail(fmt.Errorf("open stage process: %w", err))
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fail(fmt.Errorf("assign stage to its job: %w", err))
	}
	// os/exec closes the primary thread's handle, so the process is resumed
	// as a whole.
	if r, _, _ := procNtResumeProcess.Call(uintptr(h)); r != 0 {
		return fail(fmt.Errorf("resume stage: NTSTATUS 0x%x", r))
	}
	t.job = job
	return nil
}

// stop terminates the job, and with it every process the stage started.
// There is no grace period: Go cannot send Windows a polite stop.
func (t *tree) stop(done <-chan error, _ time.Duration) error {
	t.killed = true
	_ = windows.TerminateJobObject(t.job, 1)
	return <-done
}

// forced reports that the stage was terminated with the job, so the exit code
// it shows (1) is not one the stage chose.
func (t *tree) forced() bool { return t.killed }

// release closes the job. It is kill-on-close, so a descendant the stage left
// behind ends here too.
func (t *tree) release() {
	if t.job != 0 {
		windows.CloseHandle(t.job)
		t.job = 0
	}
}

var procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

func killOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("configure job: %w", err)
	}
	return job, nil
}

var (
	selfOnce sync.Once
	selfJob  windows.Handle
	selfErr  error
)

// ContainSelf puts this process in a kill-on-close job, held open for the
// rest of its life. Children inherit the job from the moment they are created,
// so if this process is ended outright — a scheduled task stopped, a logoff —
// they end with it, including one started but not yet in its own stage job.
// Jobs nest, so this works under a parent that already runs us in a job.
func ContainSelf() error {
	selfOnce.Do(func() {
		job, err := killOnCloseJob()
		if err != nil {
			selfErr = err
			return
		}
		if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
			windows.CloseHandle(job)
			selfErr = fmt.Errorf("assign self to a job: %w", err)
			return
		}
		selfJob = job
	})
	return selfErr
}
