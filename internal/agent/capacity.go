package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/webcodingcat/clowder/internal/state"
)

// readCapacity reads what this host has for containers: its memory
// (MemTotal in /proc/meminfo) and its CPUs.
func readCapacity() (state.Resources, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return state.Resources{}, fmt.Errorf("read the memory of this host: %w", err)
	}
	defer func() { _ = file.Close() }() // read only
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// "MemTotal:       16303244 kB"
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[0] != "MemTotal:" || fields[2] != "kB" {
			continue
		}
		kibibytes, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return state.Resources{}, fmt.Errorf("read the memory of this host: %w", err)
		}
		return state.Resources{MemoryBytes: kibibytes * 1024, CPUs: float64(runtime.NumCPU())}, nil
	}
	if err := scanner.Err(); err != nil {
		return state.Resources{}, fmt.Errorf("read the memory of this host: %w", err)
	}
	return state.Resources{}, errors.New("read the memory of this host: no MemTotal in /proc/meminfo")
}
