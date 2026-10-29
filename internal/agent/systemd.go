package agent

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// notify tells systemd about the agent (sd_notify): READY=1 once it serves,
// WATCHDOG=1 while it is well. Outside systemd it does nothing.
func notify(message string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	// an abstract socket starts with @, which stands for a zero byte
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + socket[1:]
	}
	connection, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	defer func() { _ = connection.Close() }() // a datagram: nothing to flush
	if _, err := connection.Write([]byte(message)); err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	return nil
}

// watchdogInterval is how often to report to systemd's watchdog: half its
// timeout (WatchdogSec), or 0 without a watchdog.
func watchdogInterval() time.Duration {
	microseconds, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || microseconds <= 0 {
		return 0
	}
	return time.Duration(microseconds) * time.Microsecond / 2
}
