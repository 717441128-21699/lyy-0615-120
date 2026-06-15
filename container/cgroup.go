package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const (
	cgroupRoot    = "/sys/fs/cgroup"
	cgroupCPU     = "cpu"
	cgroupMemory  = "memory"
	cgroupCPUAcct = "cpuacct"
)

func NewCgroup(name string, cpuQuota, cpuPeriod, memory int) *Cgroup {
	return &Cgroup{
		Name: name,
		CPU: CPUCgroup{
			Quota:  cpuQuota,
			Period: cpuPeriod,
		},
		Memory: MemoryCgroup{
			Limit: memory,
		},
	}
}

func (c *Cgroup) Set() error {
	if err := c.setupCPU(); err != nil {
		return fmt.Errorf("setup cpu cgroup failed: %v", err)
	}
	if err := c.setupMemory(); err != nil {
		return fmt.Errorf("setup memory cgroup failed: %v", err)
	}
	return nil
}

func (c *Cgroup) setupCPU() error {
	cpuPath := filepath.Join(cgroupRoot, cgroupCPU, c.Name)
	if err := os.MkdirAll(cpuPath, 0755); err != nil {
		return fmt.Errorf("create cpu cgroup dir failed: %v", err)
	}

	if c.CPU.Period > 0 {
		periodFile := filepath.Join(cpuPath, "cpu.cfs_period_us")
		if err := os.WriteFile(periodFile, []byte(strconv.Itoa(c.CPU.Period)), 0644); err != nil {
			return fmt.Errorf("write cpu.cfs_period_us failed: %v", err)
		}
	}

	if c.CPU.Quota > 0 {
		quotaFile := filepath.Join(cpuPath, "cpu.cfs_quota_us")
		if err := os.WriteFile(quotaFile, []byte(strconv.Itoa(c.CPU.Quota)), 0644); err != nil {
			return fmt.Errorf("write cpu.cfs_quota_us failed: %v", err)
		}
	}

	return nil
}

func (c *Cgroup) setupMemory() error {
	memPath := filepath.Join(cgroupRoot, cgroupMemory, c.Name)
	if err := os.MkdirAll(memPath, 0755); err != nil {
		return fmt.Errorf("create memory cgroup dir failed: %v", err)
	}

	if c.Memory.Limit > 0 {
		limitFile := filepath.Join(memPath, "memory.limit_in_bytes")
		if err := os.WriteFile(limitFile, []byte(strconv.Itoa(c.Memory.Limit)), 0644); err != nil {
			return fmt.Errorf("write memory.limit_in_bytes failed: %v", err)
		}
	}

	return nil
}

func (c *Cgroup) AddProcess(pid int) error {
	cpuPath := filepath.Join(cgroupRoot, cgroupCPU, c.Name, "cgroup.procs")
	if err := writeCgroupProc(cpuPath, pid); err != nil {
		return fmt.Errorf("add process to cpu cgroup failed: %v", err)
	}

	memPath := filepath.Join(cgroupRoot, cgroupMemory, c.Name, "cgroup.procs")
	if err := writeCgroupProc(memPath, pid); err != nil {
		return fmt.Errorf("add process to memory cgroup failed: %v", err)
	}

	return nil
}

func writeCgroupProc(path string, pid int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(strconv.Itoa(pid))
	return err
}

func (c *Cgroup) Destroy() error {
	var errs []error

	cpuPath := filepath.Join(cgroupRoot, cgroupCPU, c.Name)
	if err := removeCgroupDir(cpuPath); err != nil {
		errs = append(errs, fmt.Errorf("remove cpu cgroup failed: %v", err))
	}

	memPath := filepath.Join(cgroupRoot, cgroupMemory, c.Name)
	if err := removeCgroupDir(memPath); err != nil {
		errs = append(errs, fmt.Errorf("remove memory cgroup failed: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("cgroup destroy errors: %v", errs)
	}
	return nil
}

func removeCgroupDir(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	tasksFile := filepath.Join(path, "cgroup.procs")
	tasks, err := os.ReadFile(tasksFile)
	if err == nil && len(tasks) > 0 {
		fmt.Fprintf(os.Stderr, "Warning: cgroup %s still has processes\n", path)
	}

	return os.Remove(path)
}
