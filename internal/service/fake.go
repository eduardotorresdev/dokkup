package service

import (
	"context"
	"sync"
)

// Fake is an in-memory service manager for tests.
//
// It covers exactly the surface of [Manager] and no more, so the interface
// grows one method at a time and the tests cannot drift away from what systemd
// is actually asked to do.
type Fake struct {
	mu sync.Mutex

	// Active is what IsActive reports. A successful Restart or Enable sets it,
	// and a successful Disable clears it, so a test can assert that removal
	// stopped the service before it removed the user it runs as.
	Active bool

	// Calls records every invocation in order, so a test can assert that a
	// rollback restarted the service exactly twice rather than hoping it did.
	Calls []string

	// Err, when set, is returned by every method. For exercising failure paths.
	Err error
}

var _ Manager = (*Fake)(nil)

func (f *Fake) record(call string) error {
	f.Calls = append(f.Calls, call)
	return f.Err
}

// Restart implements [Manager].
func (f *Fake) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("restart"); err != nil {
		return err
	}
	f.Active = true
	return nil
}

// ResetFailed implements [Manager].
func (f *Fake) ResetFailed(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.record("reset-failed")
}

// DaemonReload implements [Manager].
func (f *Fake) DaemonReload(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.record("daemon-reload")
}

// Enable implements [Manager].
func (f *Fake) Enable(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("enable"); err != nil {
		return err
	}
	f.Active = true
	return nil
}

// Disable implements [Manager].
func (f *Fake) Disable(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("disable"); err != nil {
		return err
	}
	f.Active = false
	return nil
}

// Reload implements [Manager].
//
// The unit is recorded with the call because the one caller reloads nginx
// rather than dokkup, and a test that could not tell the two apart would not
// notice a seam that reloaded the wrong service.
func (f *Fake) Reload(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.record("reload " + unit)
}

// IsActive implements [Manager].
func (f *Fake) IsActive(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("is-active"); err != nil {
		return false, err
	}
	return f.Active, nil
}

// Restarts reports how many times the unit was restarted.
func (f *Fake) Restarts() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, call := range f.Calls {
		if call == "restart" {
			n++
		}
	}
	return n
}
