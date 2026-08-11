package stubprog_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/stubprog"
)

func TestAWrittenProgramRunsAndDoesWhatItsScriptSays(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "program")
	out := filepath.Join(dir, "out")

	if err := stubprog.Write(path, "#!/bin/sh\nprintf '%s' \"$1\" > '"+out+"'\n"); err != nil {
		t.Fatalf("writing the program: %v", err)
	}
	if err := exec.CommandContext(t.Context(), path, "spoken to").Run(); err != nil {
		t.Fatalf("running the program: %v", err)
	}

	said, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading what the program wrote: %v", err)
	}
	if string(said) != "spoken to" {
		t.Errorf("the program wrote %q, so it is not the script that ran", said)
	}
}

// The point of the package. Every one of these writes races every other one's
// fork, which on Linux is enough to fail with ETXTBSY if the write is a plain
// os.WriteFile. Twenty-four is where the trade sits: with the lock dropped, 15
// of 20 runs on Linux caught it, against 10 of 20 at twelve and 19 of 20 at
// thirty-two. It costs 23ms there and about four seconds on macOS, where
// starting a process is far more expensive and the failure never happens.
//
// It is a probabilistic test of a probabilistic failure and cannot prove the
// lock is held. What it can do is stop the fix being quietly removed, since a
// removal would have to survive this on run after run.
func TestProgramsWrittenWhileOthersAreBeingRunStillRun(t *testing.T) {
	t.Parallel()

	const programs = 24

	ctx := t.Context()
	dir := t.TempDir()
	failures := make(chan error, programs)

	var wg sync.WaitGroup
	for i := range programs {
		wg.Add(1)
		go func() {
			defer wg.Done()

			path := filepath.Join(dir, "program"+strconv.Itoa(i))
			if err := stubprog.Write(path, "#!/bin/sh\nexit 0\n"); err != nil {
				failures <- err
				return
			}
			if err := exec.CommandContext(ctx, path).Run(); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)

	for err := range failures {
		t.Errorf("a program written beside others being run could not be run: %v", err)
	}
}
