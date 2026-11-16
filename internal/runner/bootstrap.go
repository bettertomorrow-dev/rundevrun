// Bootstraps detached processes through the startup handshake.
package runner

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

func Bootstrap() error {
	pipe := os.NewFile(3, "supervisor-ack")
	if pipe == nil {
		return fmt.Errorf("missing supervisor acknowledgement")
	}
	var ack [1]byte
	_, err := io.ReadFull(pipe, ack[:])
	closeErr := pipe.Close()
	if err != nil {
		return fmt.Errorf("supervisor acknowledgement: %w", err)
	}
	if closeErr != nil {
		return closeErr
	}
	if ack[0] != 1 {
		return fmt.Errorf("invalid supervisor acknowledgement")
	}
	command, found := os.LookupEnv("__RDR_COMMAND")
	if !found {
		return fmt.Errorf("missing bootstrap command")
	}
	env := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "__RDR_COMMAND=") {
			env = append(env, item)
		}
	}
	return syscall.Exec("/bin/sh", []string{"/bin/sh", "-c", command}, env)
}
