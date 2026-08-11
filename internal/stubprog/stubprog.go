// Package stubprog writes the stand-in programs the tests run in place of the
// host's own -- dokku, sudo, systemctl, cosign.
//
// It exists for one line of itself. A program written and then run by the same
// process can fail to start with ETXTBSY, because the kernel refuses to execute
// a file another process holds open for writing: the descriptor is closed
// before the exec, but a fork anywhere else in the process copies it into a
// child that has not reached its own exec yet, and until it does, that child is
// a process holding the file open for writing. Nothing is wrong with the test
// that fails, and nothing it does can make it fail again, which is why it
// passed everywhere until it did not -- on the release job's own gate:
//
//	--- FAIL: TestDokkuIsReachedThroughSudoOnlyWhenAnAccountIsNamed/the_account_the_sudoers_rule_names
//	    exec_test.go:179: Version: dokku version: fork/exec /tmp/.../sudo: text file busy
//
// under Go 1.26 with -race -shuffle=on, holding up a release in which nothing
// was broken.
//
// syscall.ForkLock is the remedy the runtime provides. It is taken for writing
// around every fork, and the standard library never takes it for reading on a
// system that has O_CLOEXEC -- which is every system dokkup runs on. Holding it
// for reading across the write is therefore uncontended in practice, and makes
// the window it protects empty: no fork can begin between the open and the
// close, so no child can be holding a copy of that descriptor by the time
// anything tries to run the file.
//
// Measured on Linux 6.18 aarch64, 200 programs written and run at once, five
// rounds each way: between 12 and 40 of every 200 failed with ETXTBSY without
// the lock held, and 0 of 1,000 with it. On macOS the same program never fails
// either way, which is why this was only ever going to be seen in CI.
package stubprog

import (
	"fmt"
	"os"
	"syscall"
)

// Write creates an executable program at path whose contents are script. It
// returns the error rather than failing the test itself, so that each caller
// can name the host program it was standing in for.
func Write(path, script string) error {
	// See the package comment: the read side of ForkLock is what keeps this
	// descriptor out of a forked child, and the exec that follows out of
	// ETXTBSY.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()

	//nolint:gosec // an executable has to be executable
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
