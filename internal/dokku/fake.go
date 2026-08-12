package dokku

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
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

	// processTypes maps app name to its process types and their scale.
	processTypes map[string]map[string]int

	// domains maps app name to the domains it is served at.
	domains map[string][]string

	// logs maps app name to the lines Logs will deliver.
	logs map[string][]string

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
		processTypes: make(map[string]map[string]int),
		domains:      make(map[string][]string),
		logs:         make(map[string][]string),
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

// SetProcessTypes gives an app its process types and their scale, for test
// setup. It registers the app if it is not already there, so that a test
// interested in one thing has to set up only that thing.
func (f *Fake) SetProcessTypes(app string, scale map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ensure(app)
	f.processTypes[app] = maps.Clone(scale)
}

// SetDomains gives an app the domains it is served at, for test setup.
func (f *Fake) SetDomains(app string, domains []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ensure(app)
	f.domains[app] = slices.Clone(domains)
}

// SetLogs gives an app the lines Logs will deliver, for test setup.
//
// A line narrowed by process type is matched the way Dokku writes one, on the
// `app[<process type>.<index>]:` its output carries, so a test can prove the
// narrowing reached the lines it meant to.
func (f *Fake) SetLogs(app string, lines []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ensure(app)
	f.logs[app] = slices.Clone(lines)
}

// ensure registers an app with an empty report if it is not already known.
// Callers hold the lock.
func (f *Fake) ensure(app string) {
	if _, ok := f.apps[app]; !ok {
		f.apps[app] = map[string]string{}
	}
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

// ProcessTypes implements [Client].
func (f *Fake) ProcessTypes(_ context.Context, app string) (map[string]int, error) {
	if err := ValidateAppName(app); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("ps:report " + app); err != nil {
		return nil, err
	}
	if _, ok := f.apps[app]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrAppNotFound, app)
	}
	types := make(map[string]int, len(f.processTypes[app]))
	maps.Copy(types, f.processTypes[app])
	return types, nil
}

// Domains implements [Client].
func (f *Fake) Domains(_ context.Context, app string) ([]string, error) {
	if err := ValidateAppName(app); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("domains:report " + app); err != nil {
		return nil, err
	}
	if _, ok := f.apps[app]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrAppNotFound, app)
	}
	if len(f.domains[app]) == 0 {
		return nil, nil
	}
	return slices.Clone(f.domains[app]), nil
}

// Logs implements [Client].
//
// Following blocks until the context is cancelled once the stored lines are
// delivered, because that is what a followed stream does: it ends when the
// caller stops watching and not before.
func (f *Fake) Logs(ctx context.Context, app string, opts LogOptions, fn func(line string) error) error {
	if err := ValidateAppName(app); err != nil {
		return err
	}
	if opts.ProcessType != "" {
		if err := validateProcessType(opts.ProcessType); err != nil {
			return err
		}
	}

	lines, err := f.logLines(app, opts)
	if err != nil {
		return err
	}

	// Delivered with the lock released. fn is the caller's own code and may
	// well ask this same fake something, which under the lock would deadlock.
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Stripped with the same expression [ExecClient.Logs] strips with, and
		// not by hand, so the two sides of the seam cannot come to disagree.
		// A test is free to store the coloured lines a real Dokku writes, and
		// must then see what a real client would have delivered.
		if err := fn(ansiColour.ReplaceAllString(line, "")); err != nil {
			return err
		}
	}
	if opts.Follow {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// logLines records the call and returns the lines opts asks for.
func (f *Fake) logLines(app string, opts LogOptions) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Recorded with the options in it: a test proving the logs were asked for
	// is otherwise unable to prove they were asked for with what the operator
	// chose.
	if err := f.record(strings.Join(logArgs(app, opts), " ")); err != nil {
		return nil, err
	}
	if _, ok := f.apps[app]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrAppNotFound, app)
	}

	lines := slices.Clone(f.logs[app])
	if opts.ProcessType != "" {
		lines = slices.DeleteFunc(lines, func(line string) bool {
			return !strings.Contains(line, "["+opts.ProcessType+".")
		})
	}
	if opts.Tail > 0 && len(lines) > opts.Tail {
		lines = lines[len(lines)-opts.Tail:]
	}
	return lines, nil
}
