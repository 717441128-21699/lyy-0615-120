package container

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	cgroupRoot = "/sys/fs/cgroup"
)

type CgroupVersion int

const (
	CgroupV1 CgroupVersion = iota
	CgroupV2
)

type CgroupManager interface {
	Set() error
	AddProcess(pid int) error
	Destroy() error
	Version() CgroupVersion
}

type Cgroup struct {
	Name    string
	CPU     CPUCgroup
	Memory  MemoryCgroup
	manager CgroupManager
}

func DetectCgroupVersion() (CgroupVersion, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return 0, fmt.Errorf("open /proc/mounts failed: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if fields[2] == "cgroup2" {
			return CgroupV2, nil
		}
	}

	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err == nil {
		return CgroupV2, nil
	}

	if _, err := os.Stat(filepath.Join(cgroupRoot, "cpu", "cgroup.procs")); err == nil {
		return CgroupV1, nil
	}

	return 0, fmt.Errorf("cannot detect cgroup version, please ensure cgroup is mounted at %s", cgroupRoot)
}

func NewCgroup(name string, cpuQuota, cpuPeriod, memory int) (*Cgroup, error) {
	version, err := DetectCgroupVersion()
	if err != nil {
		return nil, err
	}

	cg := &Cgroup{
		Name:   name,
		CPU:    CPUCgroup{Quota: cpuQuota, Period: cpuPeriod},
		Memory: MemoryCgroup{Limit: memory},
	}

	switch version {
	case CgroupV1:
		cg.manager = newCgroupV1(cg)
	case CgroupV2:
		cg.manager = newCgroupV2(cg)
	}

	return cg, nil
}

func (c *Cgroup) Set() error {
	if c.manager == nil {
		return fmt.Errorf("cgroup manager not initialized")
	}
	return c.manager.Set()
}

func (c *Cgroup) AddProcess(pid int) error {
	if c.manager == nil {
		return fmt.Errorf("cgroup manager not initialized")
	}
	return c.manager.AddProcess(pid)
}

func (c *Cgroup) Destroy() error {
	if c.manager == nil {
		return nil
	}
	return c.manager.Destroy()
}

func (c *Cgroup) Version() CgroupVersion {
	if c.manager == nil {
		return -1
	}
	return c.manager.Version()
}

func writeCgroupFile(path string, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir %s failed: %v", filepath.Dir(path), err)
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func writeCgroupProc(path string, pid int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open %s failed: %v", path, err)
	}
	defer f.Close()

	if _, err := f.WriteString(strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("write pid to %s failed: %v", path, err)
	}
	return nil
}

func removeCgroupDir(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	tasksFile := filepath.Join(path, "cgroup.procs")
	tasks, err := os.ReadFile(tasksFile)
	if err == nil && len(strings.TrimSpace(string(tasks))) > 0 {
		fmt.Fprintf(os.Stderr, "Warning: cgroup %s still has processes, trying to kill them\n", path)
		scanner := bufio.NewScanner(strings.NewReader(string(tasks)))
		for scanner.Scan() {
			pidStr := strings.TrimSpace(scanner.Text())
			if pidStr == "" {
				continue
			}
			if pid, err := strconv.Atoi(pidStr); err == nil {
				syscall.Kill(pid, syscall.Signal(9))
			}
		}
	}

	if err := os.Remove(path); err != nil {
		if strings.Contains(err.Error(), "directory not empty") ||
			strings.Contains(err.Error(), "ENOTEMPTY") {
			fmt.Fprintf(os.Stderr, "Warning: cgroup %s not empty, falling back to recursive remove\n", path)
			return os.RemoveAll(path)
		}
		return err
	}
	return nil
}
