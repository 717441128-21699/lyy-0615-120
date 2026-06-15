package container

type RunOptions struct {
	Rootfs     string
	Hostname   string
	Command    []string
	CPUQuota   int
	CPUPeriod  int
	Memory     int
	CgroupName string
}

type Cgroup struct {
	Name   string
	CPU    CPUCgroup
	Memory MemoryCgroup
}

type CPUCgroup struct {
	Quota  int
	Period int
}

type MemoryCgroup struct {
	Limit int
}
