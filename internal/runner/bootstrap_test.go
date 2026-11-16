// Tests runner bootstrap behavior.
package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBootstrapAcknowledgement(t *testing.T) {
	if os.Getenv("TEST_BOOTSTRAP") == "1" {
		if Bootstrap() != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	for _, ack := range []bool{false, true} {
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestBootstrapAcknowledgement$")
		cmd.ExtraFiles = []*os.File{read}
		cmd.Env = append(os.Environ(), "TEST_BOOTSTRAP=1", "__RDR_COMMAND=touch '"+marker+"'")
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		read.Close()
		if ack {
			if _, err = write.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
		}
		write.Close()
		err = cmd.Wait()
		_, statErr := os.Stat(marker)
		if ack && (err != nil || statErr != nil) {
			t.Fatalf("acknowledged bootstrap: %v %v", err, statErr)
		}
		if !ack && (err == nil || !os.IsNotExist(statErr)) {
			t.Fatalf("EOF allowed execution: %v %v", err, statErr)
		}
	}
}
