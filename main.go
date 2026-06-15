package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"tinycontainer/container"
)

func printUsage() {
	fmt.Println("Usage: tinycontainer <command> [arguments]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  run    Run a command inside a container")
	fmt.Println()
	fmt.Println("Run Options:")
	fmt.Println("  --rootfs <path>        Path to root filesystem (required)")
	fmt.Println("  --cpu-quota <int>      CPU quota in microseconds (default: 100000)")
	fmt.Println("  --cpu-period <int>     CPU period in microseconds (default: 100000)")
	fmt.Println("  --memory <bytes>       Memory limit in bytes (default: 256MB)")
	fmt.Println("  --hostname <name>      Container hostname (default: tinycontainer)")
	fmt.Println("  --cgroup-name <name>   Cgroup name (default: tinycontainer-<pid>)")
	fmt.Println()
	fmt.Println("Example:")
	fmt.Println("  tinycontainer run --rootfs /path/to/rootfs /bin/sh")
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		runCmd()
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func runCmd() {
	args := os.Args[2:]

	opts := &container.RunOptions{
		Rootfs:     "",
		Hostname:   "tinycontainer",
		CPUQuota:   100000,
		CPUPeriod:  100000,
		Memory:     256 * 1024 * 1024,
		CgroupName: "",
	}

	var cmdArgs []string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--rootfs":
			if i+1 >= len(args) {
				fmt.Println("Error: --rootfs requires a value")
				os.Exit(1)
			}
			opts.Rootfs = args[i+1]
			i += 2
		case "--hostname":
			if i+1 >= len(args) {
				fmt.Println("Error: --hostname requires a value")
				os.Exit(1)
			}
			opts.Hostname = args[i+1]
			i += 2
		case "--cpu-quota":
			if i+1 >= len(args) {
				fmt.Println("Error: --cpu-quota requires a value")
				os.Exit(1)
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Printf("Error: invalid cpu-quota value: %v\n", err)
				os.Exit(1)
			}
			opts.CPUQuota = val
			i += 2
		case "--cpu-period":
			if i+1 >= len(args) {
				fmt.Println("Error: --cpu-period requires a value")
				os.Exit(1)
			}
			val, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Printf("Error: invalid cpu-period value: %v\n", err)
				os.Exit(1)
			}
			opts.CPUPeriod = val
			i += 2
		case "--memory":
			if i+1 >= len(args) {
				fmt.Println("Error: --memory requires a value")
				os.Exit(1)
			}
			opts.Memory = parseMemory(args[i+1])
			i += 2
		case "--cgroup-name":
			if i+1 >= len(args) {
				fmt.Println("Error: --cgroup-name requires a value")
				os.Exit(1)
			}
			opts.CgroupName = args[i+1]
			i += 2
		default:
			cmdArgs = args[i:]
			i = len(args)
		}
	}

	if opts.Rootfs == "" {
		fmt.Println("Error: --rootfs is required")
		os.Exit(1)
	}

	if len(cmdArgs) == 0 {
		fmt.Println("Error: no command specified")
		os.Exit(1)
	}

	opts.Command = cmdArgs

	err := container.Run(opts)
	if err == nil {
		os.Exit(0)
	}

	var exitErr *container.ExitCodeError
	if errors.As(err, &exitErr) {
		if exitErr.Error() != "" {
			fmt.Fprintf(os.Stderr, "Error: %v\n", exitErr)
		}
		os.Exit(exitErr.Code)
	}

	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

func parseMemory(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	multiplier := 1

	if strings.HasSuffix(s, "g") || strings.HasSuffix(s, "gb") {
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "gb"), "g")
	} else if strings.HasSuffix(s, "m") || strings.HasSuffix(s, "mb") {
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "mb"), "m")
	} else if strings.HasSuffix(s, "k") || strings.HasSuffix(s, "kb") {
		multiplier = 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "kb"), "k")
	}

	val, err := strconv.Atoi(s)
	if err != nil {
		fmt.Printf("Error: invalid memory value: %v\n", err)
		os.Exit(1)
	}

	return val * multiplier
}
