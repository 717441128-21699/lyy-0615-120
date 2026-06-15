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

	if err := writeCgroupFile(filepath.Join(cgPath, "cgroup.procs"), ""); err != nil {
		return fmt.Errorf("[cgroup v2] create cgroup dir failed: %v", err)
	}

	controllers, err := getAvailableControllersV2()
	if err != nil {
		if destroyErr := c.Destroy(); destroyErr != nil {
			return fmt.Errorf("[cgroup v2] get available controllers failed: %v; cleanup also failed: %v", err, destroyErr)
		}
		return fmt.Errorf("[cgroup v2] get available controllers failed: %v (cgroup cleaned up)", err)
	}

	if err := c.enableControllers(cgPath, controllers); err != nil {
		if destroyErr := c.Destroy(); destroyErr != nil {
			return fmt.Errorf("[cgroup v2] enable controllers failed: %v; cleanup also failed: %v", err, destroyErr)
		}
		return fmt.Errorf("[cgroup v2] enable controllers failed: %v (cgroup cleaned up)", err)
	}

	if err := c.setupCPU(cgPath); err != nil {
		if destroyErr := c.Destroy(); destroyErr != nil {
			return fmt.Errorf("[cgroup v2] setup cpu failed: %v; cleanup also failed: %v", err, destroyErr)
		}
		return fmt.Errorf("[cgroup v2] setup cpu failed: %v (cgroup cleaned up)", err)
	}

	if err := c.setupMemory(cgPath); err != nil {
		if destroyErr := c.Destroy(); destroyErr != nil {
			return fmt.Errorf("[cgroup v2] setup memory failed: %v; cleanup also failed: %v", err, destroyErr)
		}
		return fmt.Errorf("[cgroup v2] setup memory failed: %v (cgroup cleaned up)", err)
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

func (c *cgroupV2) enableControllers(cgPath string, available []string) error {
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

func (c *cgroupV2) setupCPU(cgPath string) error {
	if c.cg.CPU.Period <= 0 {
		return nil
	}

	maxFile := filepath.Join(cgPath, "cpu.max")
	if _, err := os.Stat(maxFile); os.IsNotExist(err) {
		return fmt.Errorf("cpu.max not supported on this system (cgroup v2 without cpu controller)")
	}

	var maxVal string
	if c.cg.CPU.Quota <= 0 {
		maxVal = fmt.Sprintf("max %d", c.cg.CPU.Period)
	} else {
		maxVal = fmt.Sprintf("%d %d", c.cg.CPU.Quota, c.cg.CPU.Period)
	}

	if err := writeCgroupFile(maxFile, maxVal); err != nil {
		return fmt.Errorf("write cpu.max failed: %v", err)
	}

	return nil
}

func (c *cgroupV2) setupMemory(cgPath string) error {
	if c.cg.Memory.Limit <= 0 {
		return nil
	}

	maxFile := filepath.Join(cgPath, "memory.max")
	if _, err := os.Stat(maxFile); os.IsNotExist(err) {
		return fmt.Errorf("memory.max not supported on this system (cgroup v2 without memory controller)")
	}

	if err := writeCgroupFile(maxFile, strconv.Itoa(c.cg.Memory.Limit)); err != nil {
		return fmt.Errorf("write memory.max failed: %v", err)
	}

	return nil
}

func (c *cgroupV2) AddProcess(pid int) error {
	cgPath := filepath.Join(cgroupRoot, c.cg.Name, "cgroup.procs")
	if err := writeCgroupProc(cgPath, pid); err != nil {
		return fmt.Errorf("[cgroup v2] add process to cgroup failed: %v", err)
	}
	return nil
}

func (c *cgroupV2) Destroy() error {
	cgPath := filepath.Join(cgroupRoot, c.cg.Name)
	if err := removeCgroupDir(cgPath); err != nil {
		return fmt.Errorf("[cgroup v2] remove cgroup failed: %v", err)
	}
	return nil
}
