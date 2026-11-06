package agent

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// notify is sd_notify; outside systemd it does nothing.
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

// watchdogInterval returns 0 if there is no watchdog.
func watchdogInterval() time.Duration {
	microseconds, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || microseconds <= 0 {
		return 0
	}
	return time.Duration(microseconds) * time.Microsecond / 2
}
