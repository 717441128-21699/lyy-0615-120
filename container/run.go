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

const (
	envRunInit     = "TINYCONTAINER_INIT"
	envCgroupName  = "TINYCONTAINER_CGROUP_NAME"
	envCgroupV     = "TINYCONTAINER_CGROUP_VERSION"
)

type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string {
	return fmt.Sprintf("stage [%s] failed: %v", e.stage, e.err)
}

func (e *stageError) Unwrap() error {
	return e.err
}

func stageErr(stage string, err error, a ...interface{}) error {
	if len(a) > 0 {
		return &stageError{stage: stage, err: fmt.Errorf(err.Error(), a...)}
	}
	return &stageError{stage: stage, err: err}
}

func Run(opts *RunOptions) error {
	if os.Getenv(envRunInit) == "1" {
		return runContainerInit(opts)
	}

	var cleanupFuncs []func()
	cleanup := func() {
		for i := len(cleanupFuncs) - 1; i >= 0; i-- {
			cleanupFuncs[i]()
		}
	}
	defer cleanup()

	if err := preflightChecks(opts); err != nil {
		return err
	}

	cgroupName := opts.CgroupName
	if cgroupName == "" {
		cgroupName = "tinycontainer-" + strconv.Itoa(os.Getpid())
	}

	cgroup, err := NewCgroup(cgroupName, opts.CPUQuota, opts.CPUPeriod, opts.Memory)
	if err != nil {
		return stageErr("cgroup_version_detect", err)
	}

	fmt.Fprintf(os.Stderr, "Using cgroup v%d\n", cgroup.Version()+1)

	if err := cgroup.Set(); err != nil {
		return stageErr("cgroup_setup", err)
	}
	cleanupFuncs = append(cleanupFuncs, func() {
		if err := cgroup.Destroy(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to destroy cgroup %s: %v\n", cgroupName, err)
		} else {
			fmt.Fprintf(os.Stderr, "Cgroup %s cleaned up successfully\n", cgroupName)
		}
	})

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

	cmd.Env = append(os.Environ(),
		envRunInit+"=1",
		envCgroupName+"="+cgroupName,
		envCgroupV+"="+strconv.Itoa(int(cgroup.Version())),
	)

	if err := cmd.Start(); err != nil {
		return stageErr("process_start", fmt.Errorf("start container process failed: %v", err))
	}

	if err := cgroup.AddProcess(cmd.Process.Pid); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return stageErr("cgroup_add_process", err)
	}
	fmt.Fprintf(os.Stderr, "Process %d added to cgroup %s\n", cmd.Process.Pid, cgroupName)

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
				fmt.Fprintf(os.Stderr, "Forwarding signal %v to container process %d\n", sig, cmd.Process.Pid)
				syscall.Kill(cmd.Process.Pid, sig.(syscall.Signal))
			}
		}
	}()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			status := exitErr.Sys().(syscall.WaitStatus)
			if status.Exited() {
				exitCode = status.ExitStatus()
				fmt.Fprintf(os.Stderr, "Container process exited with code %d\n", exitCode)
			} else if status.Signaled() {
				sig := status.Signal()
				exitCode = 128 + int(sig)
				fmt.Fprintf(os.Stderr, "Container process killed by signal %v (exit code %d)\n", sig, exitCode)
			}
		} else {
			exitCode = 1
			fmt.Fprintf(os.Stderr, "Container wait error: %v\n", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "Container process exited successfully (code 0)\n")
	}

	verifyCgroupCleanup(cgroupName, cgroup.Version())

	os.Exit(exitCode)
	return nil
}

func preflightChecks(opts *RunOptions) error {
	if _, err := os.Stat(opts.Rootfs); os.IsNotExist(err) {
		return stageErr("preflight_rootfs", fmt.Errorf("rootfs path does not exist: %s", opts.Rootfs))
	} else if err != nil {
		return stageErr("preflight_rootfs", fmt.Errorf("cannot access rootfs %s: %v", opts.Rootfs, err))
	}

	if len(opts.Command) == 0 {
		return stageErr("preflight_command", fmt.Errorf("no command specified"))
	}

	cmdPath := opts.Command[0]
	if !filepath.IsAbs(cmdPath) {
		cmdPath = filepath.Join("/bin", cmdPath)
	}
	fullPath := filepath.Join(opts.Rootfs, cmdPath)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return stageErr("preflight_command", fmt.Errorf("command does not exist in rootfs: %s (tried %s)", opts.Command[0], fullPath))
	} else if err != nil {
		return stageErr("preflight_command", fmt.Errorf("cannot access command %s: %v", fullPath, err))
	}

	return nil
}

func verifyCgroupCleanup(cgroupName string, version CgroupVersion) {
	var paths []string
	switch version {
	case CgroupV1:
		paths = []string{
			filepath.Join(cgroupRoot, "cpu", cgroupName),
			filepath.Join(cgroupRoot, "memory", cgroupName),
		}
	case CgroupV2:
		paths = []string{
			filepath.Join(cgroupRoot, cgroupName),
		}
	}

	allClean := true
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			allClean = false
			fmt.Fprintf(os.Stderr, "ERROR: cgroup directory still exists: %s\n", p)
		}
	}
	if allClean {
		fmt.Fprintf(os.Stderr, "All cgroup directories verified clean\n")
	}
}
