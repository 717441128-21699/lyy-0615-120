package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: stresstest <cpu|mem|both> [args]")
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  stresstest cpu 4          # Spawn 4 CPU-bound goroutines")
		fmt.Println("  stresstest mem 256        # Allocate 256MB memory")
		fmt.Println("  stresstest both 2 128     # 2 CPU threads + 128MB memory")
		os.Exit(1)
	}

	mode := os.Args[1]

	switch mode {
	case "cpu":
		n := runtime.NumCPU()
		if len(os.Args) >= 3 {
			if v, err := strconv.Atoi(os.Args[2]); err == nil {
				n = v
			}
		}
		cpuStress(n)
	case "mem":
		mb := 128
		if len(os.Args) >= 3 {
			if v, err := strconv.Atoi(os.Args[2]); err == nil {
				mb = v
			}
		}
		memStress(mb)
	case "both":
		n := runtime.NumCPU()
		mb := 128
		if len(os.Args) >= 3 {
			if v, err := strconv.Atoi(os.Args[2]); err == nil {
				n = v
			}
		}
		if len(os.Args) >= 4 {
			if v, err := strconv.Atoi(os.Args[3]); err == nil {
				mb = v
			}
		}
		go memStress(mb)
		cpuStress(n)
	default:
		fmt.Printf("Unknown mode: %s\n", mode)
		os.Exit(1)
	}
}

func cpuStress(n int) {
	fmt.Printf("Starting CPU stress test with %d goroutines...\n", n)
	fmt.Println("Press Ctrl+C to stop")

	for i := 0; i < n; i++ {
		go func(id int) {
			fmt.Printf("CPU goroutine %d started\n", id)
			counter := 0
			for {
				counter++
				if counter%100000000 == 0 {
					fmt.Printf("CPU%d: %dM iterations\n", id, counter/1000000)
				}
			}
		}(i)
	}

	select {}
}

func memStress(mb int) {
	fmt.Printf("Starting memory stress test: allocating %d MB...\n", mb)
	size := mb * 1024 * 1024
	buf := make([]byte, size)

	fmt.Printf("Filling %d MB with data...\n", mb)
	for i := range buf {
		buf[i] = byte(i % 256)
	}

	fmt.Printf("Memory allocated: %d MB. Keeping it alive...\n", mb)
	fmt.Println("Press Ctrl+C to stop")

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	iter := 0
	for range ticker.C {
		iter++
		sum := 0
		for i := 0; i < size; i += 4096 {
			sum += int(buf[i])
		}
		fmt.Printf("Memory check iteration %d, checksum: %d\n", iter, sum)
		runtime.KeepAlive(buf)
	}
}
