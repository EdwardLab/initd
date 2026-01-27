package logging

import (
	"fmt"
	"io"
	"time"

	"golang.org/x/sys/unix"
)

func MonotonicNow() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0
	}
	return time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec)
}

func KernelPrintf(output io.Writer, unit string, pid int, format string, args ...interface{}) {
	if output == nil {
		return
	}
	message := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(output, "[%s] %s[%d]: %s\n", formatMonotonic(MonotonicNow()), unit, pid, message)
}

func formatMonotonic(value time.Duration) string {
	seconds := float64(value) / float64(time.Second)
	return fmt.Sprintf("%12.6f", seconds)
}
