package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type cgroupV2 struct {
	cg *Cgroup
}

func newCgroupV2(cg *Cgroup) *cgroupV2 {
	return &cgroupV2{cg: cg}
}

func (c *cgroupV2) Version() CgroupVersion {
	return CgroupV2
}

func (c *cgroupV2) Set() error {
	cgPath := filepath.Join(cgroupRoot, c.cg.Name)

	cgDone := false

	cleanupPartial := func() {
		if cgDone {
			if err := removeCgroupDir(cgPath); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: partial cleanup of %s failed: %v\n", cgPath, err)
			}
		}
	}

	if err := os.MkdirAll(cgPath, 0755); err != nil {
		return fmt.Errorf("[cgroup v2] create cgroup dir %s failed: %v", cgPath, err)
	}
	cgDone = true

	controllers, err := getAvailableControllersV2()
	if err != nil {
		cleanupPartial()
		return fmt.Errorf("[cgroup v2] get available controllers failed: %v (cgroup cleaned up)", err)
	}

	cpuSupported := false
	memSupported := false
	for _, ctrl := range controllers {
		if ctrl == "cpu" {
			cpuSupported = true
		}
		if ctrl == "memory" {
			memSupported = true
		}
	}

	if err := enableControllersV2(cgPath, controllers); err != nil {
		cleanupPartial()
		return fmt.Errorf("[cgroup v2] enable controllers failed: %v (cgroup cleaned up)", err)
	}

	if c.cg.CPU.Quota > 0 || c.cg.CPU.Period > 0 {
		if !cpuSupported {
			cleanupPartial()
			return fmt.Errorf("[cgroup v2] CPU controller not available on this system (cgroup cleaned up)")
		}
		if err := c.setupCPUV2(cgPath); err != nil {
			cleanupPartial()
			return fmt.Errorf("[cgroup v2] setup CPU failed: %v (cgroup cleaned up)", err)
		}
	}

	if c.cg.Memory.Limit > 0 {
		if !memSupported {
			cleanupPartial()
			return fmt.Errorf("[cgroup v2] Memory controller not available on this system (cgroup cleaned up)")
		}
		if err := c.setupMemoryV2(cgPath); err != nil {
			cleanupPartial()
			return fmt.Errorf("[cgroup v2] setup Memory failed: %v (cgroup cleaned up)", err)
		}
	}

	return nil
}

func getAvailableControllersV2() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		return nil, fmt.Errorf("read cgroup.controllers failed: %v", err)
	}
	return strings.Fields(string(data)), nil
}

func enableControllersV2(cgPath string, available []string) error {
	needed := []string{"cpu", "memory"}
	subtreeFile := filepath.Join(cgroupRoot, "cgroup.subtree_control")

	var controllersToEnable []string
	for _, ctrl := range needed {
		for _, avail := range available {
			if ctrl == avail {
				controllersToEnable = append(controllersToEnable, "+"+ctrl)
				break
			}
		}
	}

	if len(controllersToEnable) > 0 {
		content := strings.Join(controllersToEnable, " ")
		if err := os.WriteFile(subtreeFile, []byte(content), 0644); err != nil {
			return fmt.Errorf("write cgroup.subtree_control with %q failed: %v", content, err)
		}
	}

	return nil
}

func (c *cgroupV2) setupCPUV2(cgPath string) error {
	maxFile := filepath.Join(cgPath, "cpu.max")
	if err := checkFileExists(maxFile); err != nil {
		return fmt.Errorf("cpu.max interface file not available: %v", err)
	}

	var maxVal string
	if c.cg.CPU.Quota <= 0 {
		maxVal = fmt.Sprintf("max %d", c.cg.CPU.Period)
	} else {
		if c.cg.CPU.Period <= 0 {
			maxVal = fmt.Sprintf("%d 100000", c.cg.CPU.Quota)
		} else {
			maxVal = fmt.Sprintf("%d %d", c.cg.CPU.Quota, c.cg.CPU.Period)
		}
	}

	if err := os.WriteFile(maxFile, []byte(maxVal), 0644); err != nil {
		return fmt.Errorf("write cpu.max=%q failed: %v", maxVal, err)
	}

	return nil
}

func (c *cgroupV2) setupMemoryV2(cgPath string) error {
	maxFile := filepath.Join(cgPath, "memory.max")
	if err := checkFileExists(maxFile); err != nil {
		return fmt.Errorf("memory.max interface file not available: %v", err)
	}

	if err := os.WriteFile(maxFile, []byte(strconv.Itoa(c.cg.Memory.Limit)), 0644); err != nil {
		return fmt.Errorf("write memory.max=%d failed: %v", c.cg.Memory.Limit, err)
	}

	return nil
}

func (c *cgroupV2) AddProcess(pid int) error {
	cgPath := filepath.Join(cgroupRoot, c.cg.Name, "cgroup.procs")
	if err := writeCgroupProc(cgPath, pid); err != nil {
		return fmt.Errorf("[cgroup v2] add process to cgroup failed writing to %s: %v", cgPath, err)
	}
	return nil
}

func (c *cgroupV2) Destroy() error {
	cgPath := filepath.Join(cgroupRoot, c.cg.Name)
	if err := removeCgroupDir(cgPath); err != nil {
		return fmt.Errorf("[cgroup v2] remove cgroup dir %s failed: %v", cgPath, err)
	}
	return nil
}
