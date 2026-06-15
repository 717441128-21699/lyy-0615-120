package container

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
)

type mountPoint struct {
	path   string
	fsType string
	source string
	flags  uintptr
	data   string
}

var mountedPoints []mountPoint

func runContainerInit(opts *RunOptions) error {
	if err := selfJoinCgroup(); err != nil {
		return stageErr("init_cgroup_join", err)
	}

	if err := setupHostname(opts.Hostname); err != nil {
		return stageErr("init_hostname", err)
	}

	if err := setupMount(opts.Rootfs); err != nil {
		cleanupMounts()
		return stageErr("init_mount", err)
	}

	if err := pivotRoot(opts.Rootfs); err != nil {
		cleanupMounts()
		return stageErr("init_pivot_root", err)
	}

	if err := syscall.Chdir("/"); err != nil {
		cleanupMounts()
		return stageErr("init_chdir", fmt.Errorf("chdir to / failed: %v", err))
	}

	if err := mountProc(); err != nil {
		cleanupMounts()
		return stageErr("init_mount_proc", err)
	}
	defer syscall.Unmount("/proc", 0)

	if err := runAsInit(opts.Command); err != nil {
		return stageErr("init_run_command", err)
	}

	return nil
}

func selfJoinCgroup() error {
	cgroupName := os.Getenv(envCgroupName)
	versionStr := os.Getenv(envCgroupV)
	if cgroupName == "" || versionStr == "" {
		return nil
	}

	version, err := strconv.Atoi(versionStr)
	if err != nil {
		return fmt.Errorf("invalid cgroup version: %v", err)
	}

	selfPid := os.Getpid()
	var paths []string

	switch CgroupVersion(version) {
	case CgroupV1:
		paths = []string{
			filepath.Join(cgroupRoot, "cpu", cgroupName, "cgroup.procs"),
			filepath.Join(cgroupRoot, "memory", cgroupName, "cgroup.procs"),
		}
	case CgroupV2:
		paths = []string{
			filepath.Join(cgroupRoot, cgroupName, "cgroup.procs"),
		}
	}

	for _, p := range paths {
		if err := writeCgroupProc(p, selfPid); err != nil {
			return fmt.Errorf("self join cgroup failed writing to %s: %v", p, err)
		}
	}

	fmt.Fprintf(os.Stderr, "Init process %d self-joined cgroup %s\n", selfPid, cgroupName)
	return nil
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
	fmt.Fprintf(os.Stderr, "Started user command as PID %d (container PID namespace)\n", proc.Pid)

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
				fmt.Fprintf(os.Stderr, "All children reaped, init process exiting with code %d\n", firstExitStatus)
				return ExitError(firstExitStatus, nil)
			}
			continue
		}

		if pid == proc.Pid {
			firstExited = true
			if status.Exited() {
				firstExitStatus = status.ExitStatus()
				fmt.Fprintf(os.Stderr, "Main process %d exited with code %d\n", pid, firstExitStatus)
			} else if status.Signaled() {
				sig := status.Signal()
				firstExitStatus = 128 + int(sig)
				fmt.Fprintf(os.Stderr, "Main process %d killed by signal %v (exit code %d)\n", pid, sig, firstExitStatus)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Reaped orphan process %d\n", pid)
		}
	}
}

func forwardSignals(sigChan <-chan os.Signal, targetPid int) {
	for sig := range sigChan {
		if sig == syscall.SIGCHLD {
			continue
		}
		fmt.Fprintf(os.Stderr, "Init forwarding signal %v to %d\n", sig, targetPid)
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
	mountedPoints = append(mountedPoints, mountPoint{path: "/", flags: syscall.MS_REC | syscall.MS_PRIVATE})

	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount rootfs failed: %v", err)
	}
	mountedPoints = append(mountedPoints, mountPoint{path: rootfs, source: rootfs, flags: syscall.MS_BIND | syscall.MS_REC})

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

	if err := syscall.Mount("proc", procPath, "proc", 0, ""); err != nil {
		return fmt.Errorf("mount proc failed: %v", err)
	}
	mountedPoints = append(mountedPoints, mountPoint{path: "/proc", fsType: "proc", source: "proc"})

	return nil
}

func cleanupMounts() {
	for i := len(mountedPoints) - 1; i >= 0; i-- {
		mp := mountedPoints[i]
		fmt.Fprintf(os.Stderr, "Cleaning up mount: %s\n", mp.path)
		if err := syscall.Unmount(mp.path, syscall.MNT_DETACH); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to unmount %s: %v\n", mp.path, err)
		}
	}
	mountedPoints = nil
}
