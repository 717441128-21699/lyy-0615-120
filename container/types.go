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

type CPUCgroup struct {
	Quota  int
	Period int
}

type MemoryCgroup struct {
	Limit int
}

type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return ""
}

func ExitError(code int, err error) *ExitCodeError {
	return &ExitCodeError{Code: code, Err: err}
}
