package dokku

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Fake is an in-memory Dokku for tests.
//
// It covers exactly the surface of [Client] and no more, which is the point: the
// interface grows one method at a time, and adding a method here in the same
// change is what keeps the tests from drifting away from what Dokku actually
// does. Tests that use it need no container and run in milliseconds.
type Fake struct {
	mu sync.Mutex

	// DokkuVersion is reported by Version. Defaults to a recent release.
	DokkuVersion string

	// apps maps app name to its report.
	apps map[string]map[string]string

	// Calls records every invocation in order, so a test can assert that an
	// operation reached Dokku once rather than three times.
	Calls []string

	// Err, when set, is returned by every method. For exercising failure paths.
	Err error
}

var _ Client = (*Fake)(nil)

// NewFake returns an empty Dokku with no apps.
func NewFake() *Fake {
	return &Fake{
		DokkuVersion: "0.38.7",
		apps:         make(map[string]map[string]string),
	}
}

func (f *Fake) record(call string) error {
	f.Calls = append(f.Calls, call)
	return f.Err
}

// AddApp registers an app with the given report values, for test setup.
func (f *Fake) AddApp(name string, report map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if report == nil {
		report = map[string]string{}
	}
	f.apps[name] = maps.Clone(report)
}

// Version implements [Client].
func (f *Fake) Version(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("version"); err != nil {
		return "", err
	}
	return f.DokkuVersion, nil
}

// Apps implements [Client].
func (f *Fake) Apps(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("apps:list"); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(f.apps)), nil
}

// AppReport implements [Client].
func (f *Fake) AppReport(_ context.Context, app string) (map[string]string, error) {
	if err := ValidateAppName(app); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("apps:report " + app); err != nil {
		return nil, err
	}
	report, ok := f.apps[app]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAppNotFound, app)
	}
	return maps.Clone(report), nil
}
