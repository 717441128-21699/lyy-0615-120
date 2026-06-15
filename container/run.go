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
	envRunInit    = "TINYCONTAINER_INIT"
	envCgroupName = "TINYCONTAINER_CGROUP_NAME"
	envCgroupV    = "TINYCONTAINER_CGROUP_VERSION"
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

	cgroupName := opts.CgroupName
	if cgroupName == "" {
		cgroupName = "tinycontainer-" + strconv.Itoa(os.Getpid())
	}

	var cgroupVer CgroupVersion = -1
	var cg *Cgroup
	var cmd *exec.Cmd
	exitCode := 1

	containerStarted := false
	cgroupCreated := false

	var cleanupErr error
	var verifyErr error

	defer func() {
		if containerStarted && cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	if err := preflightChecks(opts); err != nil {
		return ExitError(1, err)
	}

	var err error
	cg, err = NewCgroup(cgroupName, opts.CPUQuota, opts.CPUPeriod, opts.Memory)
	if err != nil {
		return ExitError(1, stageErr("cgroup_version_detect", err))
	}
	cgroupVer = cg.Version()

	fmt.Fprintf(os.Stderr, "Using cgroup v%d\n", cgroupVer+1)

	if err := cg.Set(); err != nil {
		return ExitError(1, stageErr("cgroup_setup", err))
	}
	cgroupCreated = true

	defer func() {
		if !cgroupCreated {
			return
		}

		fmt.Fprintf(os.Stderr, "Cleaning up cgroup %s ...\n", cgroupName)
		cleanupErr = cg.Destroy()
		if cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "Cleanup error: %v\n", cleanupErr)
		} else {
			fmt.Fprintf(os.Stderr, "Cgroup %s cleanup issued\n", cgroupName)
		}

		missing := verifyCgroupCleanup(cgroupName, cgroupVer)
		if len(missing) > 0 {
			for _, p := range missing {
				fmt.Fprintf(os.Stderr, "ERROR: cgroup directory still exists after cleanup: %s\n", p)
			}
			verifyErr = fmt.Errorf("cgroup directories still exist after cleanup: %v", missing)
		} else {
			fmt.Fprintf(os.Stderr, "All cgroup directories verified removed\n")
		}
	}()

	cmd = exec.Command("/proc/self/exe", append([]string{"run"}, os.Args[2:]...)...)
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
		envCgroupV+"="+strconv.Itoa(int(cgroupVer)),
	)

	if err := cmd.Start(); err != nil {
		return ExitError(1, stageErr("process_start", fmt.Errorf("start container process failed: %v", err)))
	}
	containerStarted = true

	if err := cg.AddProcess(cmd.Process.Pid); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		containerStarted = false
		return ExitError(1, stageErr("cgroup_add_process", err))
	}
	fmt.Fprintf(os.Stderr, "Process %d added to cgroup %s\n", cmd.Process.Pid, cgroupName)

	sigChan := make(chan os.Signal, 32)
	signal.Notify(sigChan,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGHUP,
	)

	go func() {
		for sig := range sigChan {
			if cmd.Process != nil {
				fmt.Fprintf(os.Stderr, "Forwarding signal %v to container process %d\n", sig, cmd.Process.Pid)
				syscall.Kill(cmd.Process.Pid, sig.(syscall.Signal))
			}
		}
	}()
	defer signal.Stop(sigChan)

	exitCode = 0
	if waitErr := cmd.Wait(); waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
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
			fmt.Fprintf(os.Stderr, "Container wait error: %v\n", waitErr)
		}
	} else {
		fmt.Fprintf(os.Stderr, "Container process exited successfully (code 0)\n")
	}

	containerStarted = false
	signal.Stop(sigChan)
	close(sigChan)

	// ---------- 到这里 defer 开始按逆序执行 ----------
	// 执行顺序:
	//   1. signal.Stop(sigChan)  ← 最后一个 defer，先执行
	//   2. cgroup cleanup + verify ← 第二个 defer
	//   3. kill+wait 容器进程  ← 第一个 defer（已经 containerStarted=false，跳过）

	// 然后返回给调用方

	finalExit := exitCode
	if cleanupErr != nil {
		return ExitError(1, fmt.Errorf("container exit %d but cgroup cleanup failed: %v", exitCode, cleanupErr))
	}
	if verifyErr != nil {
		return ExitError(1, fmt.Errorf("container exit %d: %v", exitCode, verifyErr))
	}

	if finalExit == 0 {
		return nil
	}
	return ExitError(finalExit, nil)
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

func verifyCgroupCleanup(cgroupName string, version CgroupVersion) []string {
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

	var stillExist []string
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			stillExist = append(stillExist, p)
		}
	}
	return stillExist
}
