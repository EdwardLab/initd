package boot

import (
	"os"
	"strings"
)

func ReadKernelCmdline() string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
