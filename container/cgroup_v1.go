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
	cpuPath := filepath.Join(cgroupRoot, "cpu", c.cg.Name)
	memPath := filepath.Join(cgroupRoot, "memory", c.cg.Name)

	cpuDone := false
	memDone := false

	cleanupPartial := func() {
		var errs []string
		if cpuDone {
			if err := removeCgroupDir(cpuPath); err != nil {
				errs = append(errs, fmt.Sprintf("cpu: %v", err))
			}
		}
		if memDone {
			if err := removeCgroupDir(memPath); err != nil {
				errs = append(errs, fmt.Sprintf("memory: %v", err))
			}
		}
		if len(errs) > 0 {
			fmt.Fprintf(os.Stderr, "Warning: partial cleanup errors: %v\n", errs)
		}
	}

	if err := os.MkdirAll(cpuPath, 0755); err != nil {
		return fmt.Errorf("[cgroup v1] create cpu cgroup dir %s failed: %v", cpuPath, err)
	}
	cpuDone = true

	if c.cg.CPU.Period > 0 {
		periodFile := filepath.Join(cpuPath, "cpu.cfs_period_us")
		if err := checkFileExists(periodFile); err != nil {
			cleanupPartial()
			return fmt.Errorf("[cgroup v1] CPU period not supported (file %s missing): %v (cpu cgroup cleaned up)", periodFile, err)
		}
		if err := os.WriteFile(periodFile, []byte(strconv.Itoa(c.cg.CPU.Period)), 0644); err != nil {
			cleanupPartial()
			return fmt.Errorf("[cgroup v1] write cpu.cfs_period_us failed: %v (cpu cgroup cleaned up)", err)
		}
	}

	if c.cg.CPU.Quota > 0 {
		quotaFile := filepath.Join(cpuPath, "cpu.cfs_quota_us")
		if err := checkFileExists(quotaFile); err != nil {
			cleanupPartial()
			return fmt.Errorf("[cgroup v1] CPU quota not supported (file %s missing): %v (cpu cgroup cleaned up)", quotaFile, err)
		}
		if err := os.WriteFile(quotaFile, []byte(strconv.Itoa(c.cg.CPU.Quota)), 0644); err != nil {
			cleanupPartial()
			return fmt.Errorf("[cgroup v1] write cpu.cfs_quota_us failed: %v (cpu cgroup cleaned up)", err)
		}
	}

	if err := os.MkdirAll(memPath, 0755); err != nil {
		cleanupPartial()
		return fmt.Errorf("[cgroup v1] create memory cgroup dir %s failed: %v (cpu cgroup cleaned up)", memPath, err)
	}
	memDone = true

	if c.cg.Memory.Limit > 0 {
		limitFile := filepath.Join(memPath, "memory.limit_in_bytes")
		if err := checkFileExists(limitFile); err != nil {
			cleanupPartial()
			return fmt.Errorf("[cgroup v1] Memory limit not supported (file %s missing): %v (cpu & memory cgroups cleaned up)", limitFile, err)
		}
		if err := os.WriteFile(limitFile, []byte(strconv.Itoa(c.cg.Memory.Limit)), 0644); err != nil {
			cleanupPartial()
			return fmt.Errorf("[cgroup v1] write memory.limit_in_bytes failed: %v (cpu & memory cgroups cleaned up)", err)
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
		errs = append(errs, fmt.Sprintf("remove cpu cgroup %s failed: %v", cpuPath, err))
	}

	memPath := filepath.Join(cgroupRoot, "memory", c.cg.Name)
	if err := removeCgroupDir(memPath); err != nil {
		errs = append(errs, fmt.Sprintf("remove memory cgroup %s failed: %v", memPath, err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("[cgroup v1] %v", errs)
	}
	return nil
}

func checkFileExists(path string) error {
	_, err := os.Stat(path)
	return err
}
