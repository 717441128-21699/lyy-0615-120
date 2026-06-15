package container

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

func runContainerInit(opts *RunOptions) error {
	if err := setupHostname(opts.Hostname); err != nil {
		return fmt.Errorf("setup hostname failed: %v", err)
	}

	if err := setupMount(opts.Rootfs); err != nil {
		return fmt.Errorf("setup mount failed: %v", err)
	}

	if err := pivotRoot(opts.Rootfs); err != nil {
		return fmt.Errorf("pivot root failed: %v", err)
	}

	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir to / failed: %v", err)
	}

	if err := mountProc(); err != nil {
		return fmt.Errorf("mount proc failed: %v", err)
	}

	defer syscall.Unmount("/proc", 0)

	return runAsInit(opts.Command)
}

func runAsInit(command []string) error {
	cmdPath, err := exec.LookPath(command[0])
	if err != nil {
		return fmt.Errorf("lookup command %s failed: %v", command[0], err)
	}

	proc, err := os.StartProcess(cmdPath, command, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Env:   os.Environ(),
		Sys: &syscall.SysProcAttr{
			Setsid: true,
		},
	})
	if err != nil {
		return fmt.Errorf("start process failed: %v", err)
	}

	sigChan := make(chan os.Signal, 32)
	signal.Notify(sigChan)

	go forwardSignals(sigChan, proc.Pid)

	var firstExitStatus int
	var firstExited bool

	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, 0, nil)
		if err != nil {
			if err == syscall.ECHILD {
				if !firstExited {
					return fmt.Errorf("no children")
				}
				os.Exit(firstExitStatus)
			}
			continue
		}

		if pid == proc.Pid {
			firstExited = true
			if status.Exited() {
				firstExitStatus = status.ExitStatus()
			} else if status.Signaled() {
				firstExitStatus = 128 + int(status.Signal())
			}
		}
	}
}

func forwardSignals(sigChan <-chan os.Signal, targetPid int) {
	for sig := range sigChan {
		syscall.Kill(targetPid, sig.(syscall.Signal))
	}
}

func setupHostname(hostname string) error {
	return syscall.Sethostname([]byte(hostname))
}

func setupMount(rootfs string) error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("remount root as private failed: %v", err)
	}

	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount rootfs failed: %v", err)
	}

	return nil
}

func pivotRoot(rootfs string) error {
	putOld := filepath.Join(rootfs, ".pivot_root")
	if err := os.MkdirAll(putOld, 0700); err != nil {
		return fmt.Errorf("create pivot_root dir failed: %v", err)
	}

	if err := syscall.PivotRoot(rootfs, putOld); err != nil {
		return fmt.Errorf("pivot_root syscall failed: %v", err)
	}

	oldRoot := filepath.Join("/", ".pivot_root")
	if err := syscall.Unmount(oldRoot, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root failed: %v", err)
	}

	if err := os.RemoveAll("/.pivot_root"); err != nil {
		return fmt.Errorf("remove .pivot_root dir failed: %v", err)
	}

	return nil
}

func mountProc() error {
	procPath := "/proc"
	if err := os.MkdirAll(procPath, 0755); err != nil {
		return fmt.Errorf("create proc dir failed: %v", err)
	}

	return syscall.Mount("proc", procPath, "proc", 0, "")
}
