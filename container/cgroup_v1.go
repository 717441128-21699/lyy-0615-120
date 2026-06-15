package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type cgroupV1 struct {
	cg *Cgroup
}

func newCgroupV1(cg *Cgroup) *cgroupV1 {
	return &cgroupV1{cg: cg}
}

func (c *cgroupV1) Version() CgroupVersion {
	return CgroupV1
}

func (c *cgroupV1) Set() error {
	if err := c.setupCPU(); err != nil {
		return fmt.Errorf("[cgroup v1] setup cpu failed: %v", err)
	}

	if err := c.setupMemory(); err != nil {
		if destroyErr := c.Destroy(); destroyErr != nil {
			return fmt.Errorf("[cgroup v1] setup memory failed: %v; cleanup also failed: %v", err, destroyErr)
		}
		return fmt.Errorf("[cgroup v1] setup memory failed: %v (partial resources cleaned up)", err)
	}

	return nil
}

func (c *cgroupV1) setupCPU() error {
	cpuPath := filepath.Join(cgroupRoot, "cpu", c.cg.Name)
	if err := writeCgroupFile(filepath.Join(cpuPath, "cgroup.procs"), ""); err != nil {
		return fmt.Errorf("create cpu cgroup dir failed: %v", err)
	}

	if c.cg.CPU.Period > 0 {
		periodFile := filepath.Join(cpuPath, "cpu.cfs_period_us")
		if _, err := statCgroupFile(periodFile); err != nil {
			return fmt.Errorf("cpu.cfs_period_us not supported on this system: %v", err)
		}
		if err := writeCgroupFile(periodFile, strconv.Itoa(c.cg.CPU.Period)); err != nil {
			return fmt.Errorf("write cpu.cfs_period_us failed: %v", err)
		}
	}

	if c.cg.CPU.Quota > 0 {
		quotaFile := filepath.Join(cpuPath, "cpu.cfs_quota_us")
		if _, err := statCgroupFile(quotaFile); err != nil {
			return fmt.Errorf("cpu.cfs_quota_us not supported on this system: %v", err)
		}
		if err := writeCgroupFile(quotaFile, strconv.Itoa(c.cg.CPU.Quota)); err != nil {
			return fmt.Errorf("write cpu.cfs_quota_us failed: %v", err)
		}
	}

	return nil
}

func (c *cgroupV1) setupMemory() error {
	memPath := filepath.Join(cgroupRoot, "memory", c.cg.Name)
	if err := writeCgroupFile(filepath.Join(memPath, "cgroup.procs"), ""); err != nil {
		return fmt.Errorf("create memory cgroup dir failed: %v", err)
	}

	if c.cg.Memory.Limit > 0 {
		limitFile := filepath.Join(memPath, "memory.limit_in_bytes")
		if _, err := statCgroupFile(limitFile); err != nil {
			return fmt.Errorf("memory.limit_in_bytes not supported on this system: %v", err)
		}
		if err := writeCgroupFile(limitFile, strconv.Itoa(c.cg.Memory.Limit)); err != nil {
			return fmt.Errorf("write memory.limit_in_bytes failed: %v", err)
		}
	}

	return nil
}

func (c *cgroupV1) AddProcess(pid int) error {
	cpuPath := filepath.Join(cgroupRoot, "cpu", c.cg.Name, "cgroup.procs")
	if err := writeCgroupProc(cpuPath, pid); err != nil {
		return fmt.Errorf("[cgroup v1] add process to cpu cgroup failed: %v", err)
	}

	memPath := filepath.Join(cgroupRoot, "memory", c.cg.Name, "cgroup.procs")
	if err := writeCgroupProc(memPath, pid); err != nil {
		return fmt.Errorf("[cgroup v1] add process to memory cgroup failed: %v", err)
	}

	return nil
}

func (c *cgroupV1) Destroy() error {
	var errs []string

	cpuPath := filepath.Join(cgroupRoot, "cpu", c.cg.Name)
	if err := removeCgroupDir(cpuPath); err != nil {
		errs = append(errs, fmt.Sprintf("remove cpu cgroup failed: %v", err))
	}

	memPath := filepath.Join(cgroupRoot, "memory", c.cg.Name)
	if err := removeCgroupDir(memPath); err != nil {
		errs = append(errs, fmt.Sprintf("remove memory cgroup failed: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("[cgroup v1] %v", errs)
	}
	return nil
}

func statCgroupFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
