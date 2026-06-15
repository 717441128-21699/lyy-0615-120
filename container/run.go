package container

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
)

const (
	envRunInit = "TINYCONTAINER_INIT"
)

func Run(opts *RunOptions) error {
	if os.Getenv(envRunInit) == "1" {
		return runContainerInit(opts)
	}

	cgroupName := opts.CgroupName
	if cgroupName == "" {
		cgroupName = "tinycontainer-" + strconv.Itoa(os.Getpid())
	}

	cgroup := NewCgroup(cgroupName, opts.CPUQuota, opts.CPUPeriod, opts.Memory)
	if err := cgroup.Set(); err != nil {
		return fmt.Errorf("set cgroup failed: %v", err)
	}

	cleanupDone := make(chan struct{})
	cleanup := func() {
		select {
		case <-cleanupDone:
			return
		default:
		}
		close(cleanupDone)
		if err := cgroup.Destroy(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to destroy cgroup: %v\n", err)
		}
	}
	defer cleanup()

	cmd := exec.Command("/proc/self/exe", append([]string{"run"}, os.Args[2:]...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWNET |
			syscall.CLONE_NEWUTS,
	}

	cmd.Env = append(os.Environ(), envRunInit+"=1")

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start container process failed: %v", err)
	}

	if err := cgroup.AddProcess(cmd.Process.Pid); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("add process to cgroup failed: %v", err)
	}

	sigChan := make(chan os.Signal, 32)
	signal.Notify(sigChan,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGHUP,
	)
	defer signal.Stop(sigChan)

	go func() {
		for sig := range sigChan {
			if cmd.Process != nil {
				syscall.Kill(cmd.Process.Pid, sig.(syscall.Signal))
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			status := exitErr.Sys().(syscall.WaitStatus)
			if status.Exited() {
				os.Exit(status.ExitStatus())
			} else if status.Signaled() {
				os.Exit(128 + int(status.Signal()))
			}
		}
		return fmt.Errorf("container process exited with error: %v", err)
	}

	return nil
}
