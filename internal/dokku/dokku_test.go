package dokku_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
)

func TestValidateAppNameRejectsNamesThatCouldBeReadAsFlagsOrShellSyntax(t *testing.T) {
	t.Parallel()

	rejected := map[string]string{
		"leading dash reads as a flag":     "-rf",
		"long flag reads as a flag":        "--force",
		"semicolon chains a command":       "app;rm -rf /",
		"backtick substitutes a command":   "app`id`",
		"dollar-paren substitutes":         "app$(id)",
		"pipe redirects":                   "app|sh",
		"space splits into two arguments":  "app other",
		"newline splits into two commands": "app\nrm",
		"slash escapes into a path":        "../etc/passwd",
		"uppercase is not a Dokku name":    "MyApp",
		"empty is not a name":              "",
	}

	for reason, name := range rejected {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			if err := dokku.ValidateAppName(name); err == nil {
				t.Fatalf("ValidateAppName(%q) accepted a name it must reject", name)
			}
		})
	}
}

func TestValidateAppNameAcceptsRealDokkuNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"api", "web-app", "app2", "0auth", strings.Repeat("a", 63)} {
		if err := dokku.ValidateAppName(name); err != nil {
			t.Errorf("ValidateAppName(%q) rejected a valid name: %v", name, err)
		}
	}
}

func TestValidateAppNameRejectsNamesLongerThanDokkuAllows(t *testing.T) {
	t.Parallel()

	if err := dokku.ValidateAppName(strings.Repeat("a", 64)); err == nil {
		t.Fatal("ValidateAppName accepted a 64-character name")
	}
}

func TestFakeReportsTheAppsItWasGiven(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.AddApp("web", map[string]string{"app-locked": "false"})
	fake.AddApp("api", nil)

	apps, err := fake.Apps(context.Background())
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if want := []string{"api", "web"}; !slicesEqual(apps, want) {
		t.Fatalf("Apps = %v, want %v", apps, want)
	}
}

func TestFakeReportsAppNotFoundForAnUnknownApp(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()

	_, err := fake.AppReport(context.Background(), "missing")
	if !errors.Is(err, dokku.ErrAppNotFound) {
		t.Fatalf("AppReport error = %v, want ErrAppNotFound", err)
	}
}

func TestFakeRejectsAnInvalidAppNameBeforeLookingItUp(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()

	if _, err := fake.AppReport(context.Background(), "--force"); err == nil {
		t.Fatal("AppReport accepted an invalid app name")
	}
	// Validation must happen before anything is recorded: the fake stands in for
	// a real invocation, and a rejected name must never reach one.
	if len(fake.Calls) != 0 {
		t.Fatalf("Calls = %v, want none for a rejected name", fake.Calls)
	}
}

func TestFakeReturnsACopySoCallersCannotMutateItsState(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.AddApp("web", map[string]string{"app-locked": "false"})

	report, err := fake.AppReport(context.Background(), "web")
	if err != nil {
		t.Fatalf("AppReport: %v", err)
	}
	report["app-locked"] = "true"

	again, err := fake.AppReport(context.Background(), "web")
	if err != nil {
		t.Fatalf("AppReport: %v", err)
	}
	if again["app-locked"] != "false" {
		t.Fatalf("app-locked = %q, want %q: the fake handed out its own map", again["app-locked"], "false")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFakeReportsTheProcessTypesAndDomainsItWasGiven(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.SetProcessTypes("web", map[string]int{"web": 2, "worker": 1})
	fake.SetDomains("web", []string{"probe.example.com", "alt.example.com"})

	types, err := fake.ProcessTypes(context.Background(), "web")
	if err != nil {
		t.Fatalf("ProcessTypes: %v", err)
	}
	if want := map[string]int{"web": 2, "worker": 1}; !maps.Equal(types, want) {
		t.Errorf("process types = %v, want %v", types, want)
	}

	domains, err := fake.Domains(context.Background(), "web")
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if want := []string{"probe.example.com", "alt.example.com"}; !slicesEqual(domains, want) {
		t.Errorf("domains = %v, want %v", domains, want)
	}
}

// An app with nothing running and no domain of its own is an ordinary app, not
// a failure: it is every app between being created and being deployed.
func TestFakeAnswersEmptyForAnAppWithNoProcessesAndNoDomains(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.AddApp("web", nil)

	types, err := fake.ProcessTypes(context.Background(), "web")
	if err != nil {
		t.Fatalf("ProcessTypes: %v", err)
	}
	if len(types) != 0 {
		t.Errorf("process types = %v, want none", types)
	}

	domains, err := fake.Domains(context.Background(), "web")
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if domains != nil {
		t.Errorf("domains = %v, want nil", domains)
	}
}

func TestFakeReturnsCopiesOfTheProcessTypesAndDomainsItHolds(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.SetProcessTypes("web", map[string]int{"web": 1})
	fake.SetDomains("web", []string{"probe.example.com"})

	types, err := fake.ProcessTypes(context.Background(), "web")
	if err != nil {
		t.Fatalf("ProcessTypes: %v", err)
	}
	types["web"] = 99

	domains, err := fake.Domains(context.Background(), "web")
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	domains[0] = "hijacked.example.com"

	types, err = fake.ProcessTypes(context.Background(), "web")
	if err != nil {
		t.Fatalf("ProcessTypes: %v", err)
	}
	if types["web"] != 1 {
		t.Errorf("web scale = %d, want 1: the fake handed out its own map", types["web"])
	}

	domains, err = fake.Domains(context.Background(), "web")
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if domains[0] != "probe.example.com" {
		t.Errorf("first domain = %q, want %q: the fake handed out its own slice", domains[0], "probe.example.com")
	}
}

// The lines the fake was given, narrowed the way the real client narrows them:
// Dokku is asked for one process type and for so many past lines, and answers
// with a subset of what it has.
func TestFakeStreamsTheLogLinesTheOptionsAskFor(t *testing.T) {
	t.Parallel()

	stored := []string{
		"2026-08-12T12:48:51Z app[web.1]: hello one",
		"2026-08-12T12:48:52Z app[worker.1]: working",
		"2026-08-12T12:48:53Z app[web.2]: hello two",
		"2026-08-12T12:48:54Z app[web.1]: hello three",
	}

	for name, tc := range map[string]struct {
		opts     dokku.LogOptions
		want     []string
		recorded string
	}{
		"everything, in the order it was written": {
			opts:     dokku.LogOptions{},
			want:     stored,
			recorded: "logs web",
		},
		"only the process type asked for": {
			opts:     dokku.LogOptions{ProcessType: "worker"},
			want:     []string{"2026-08-12T12:48:52Z app[worker.1]: working"},
			recorded: "logs --ps worker web",
		},
		"only the last lines asked for": {
			opts: dokku.LogOptions{Tail: 2},
			want: []string{
				"2026-08-12T12:48:53Z app[web.2]: hello two",
				"2026-08-12T12:48:54Z app[web.1]: hello three",
			},
			recorded: "logs --num 2 web",
		},
		"narrowed and then counted, as Dokku does it": {
			opts:     dokku.LogOptions{ProcessType: "web", Tail: 1},
			want:     []string{"2026-08-12T12:48:54Z app[web.1]: hello three"},
			recorded: "logs --num 1 --ps web web",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := dokku.NewFake()
			fake.SetLogs("web", stored)

			var lines []string
			err := fake.Logs(context.Background(), "web", tc.opts, func(line string) error {
				lines = append(lines, line)
				return nil
			})
			if err != nil {
				t.Fatalf("Logs: %v", err)
			}
			if !slicesEqual(lines, tc.want) {
				t.Errorf("lines = %q, want %q", lines, tc.want)
			}
			// Recorded as the command a real Dokku would have been sent, so
			// that a test can prove not only that the logs were asked for but
			// that they were asked for with what the operator chose.
			if want := []string{tc.recorded}; !slicesEqual(fake.Calls, want) {
				t.Errorf("Calls = %v, want %v", fake.Calls, want)
			}
		})
	}
}

// A followed stream ends when the caller stops watching, and the caller has to
// be able to tell that from Dokku having failed.
func TestFakeEndsAFollowedStreamWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.SetLogs("web", []string{"one", "two"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delivered := 0
	err := fake.Logs(ctx, "web", dokku.LogOptions{Follow: true}, func(string) error {
		delivered++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Logs error = %v, want context.Canceled", err)
	}
	if delivered != 1 {
		t.Errorf("the callback was called %d times after the caller went away, want 1", delivered)
	}
}

func TestFakeStopsStreamingWhenTheCallbackFails(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.SetLogs("web", []string{"one", "two", "three"})

	refused := errors.New("the caller has seen enough")
	delivered := 0
	err := fake.Logs(context.Background(), "web", dokku.LogOptions{Follow: true}, func(string) error {
		delivered++
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Logs error = %v, want the error the callback returned", err)
	}
	// errors.Is alone would not notice the wrapping, because CommandError
	// unwraps to what it holds. The fake has to be as faithful about this as
	// the real client: a caller recognising its own sentinel must not depend on
	// which of the two it is talking to.
	var commandErr *dokku.CommandError
	if errors.As(err, &commandErr) {
		t.Errorf("the caller's own decision was reported as a failure of Dokku's: %v", err)
	}
	if delivered != 1 {
		t.Errorf("the callback was called %d times after refusing the first line, want 1", delivered)
	}
}

func TestFakeRejectsNamesThatCouldBeReadAsFlagsBeforeStreamingLogs(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		app  string
		opts dokku.LogOptions
	}{
		"an app name that is a flag":     {app: "--force"},
		"a process type that is a flag":  {app: "web", opts: dokku.LogOptions{ProcessType: "--ps"}},
		"a process type with a space in": {app: "web", opts: dokku.LogOptions{ProcessType: "web worker"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := dokku.NewFake()
			fake.SetLogs("web", []string{"one"})

			err := fake.Logs(context.Background(), tc.app, tc.opts, func(string) error {
				t.Error("a line was delivered for a name that must be rejected")
				return nil
			})
			if err == nil {
				t.Fatal("Logs accepted a name it must reject")
			}
			// Validation comes before anything is recorded: the fake stands in
			// for a real invocation, and a rejected name must never reach one.
			if len(fake.Calls) != 0 {
				t.Fatalf("Calls = %v, want none for a rejected name", fake.Calls)
			}
		})
	}
}

// The fake stands in for the real client, so it has to hand a test the line the
// real client would have handed it. Dokku colours every line it writes and
// nothing turns that off, so what a test stores is coloured and what it must
// receive is not.
func TestFakeDeliversLogLinesTheWayTheRealClientDoes(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.SetLogs("web", []string{
		"\x1b[36m2026-08-12T12:48:51.511890541Z app[web.1]:\x1b[0m hello line 14",
		"\x1b[33m2026-08-12T12:48:53.519809667Z app[worker.1]:\x1b[0m working",
	})

	var lines []string
	// Narrowed as well, so that the colours are proven not to be in the way of
	// the process type the narrowing reads.
	opts := dokku.LogOptions{ProcessType: "web"}
	err := fake.Logs(context.Background(), "web", opts, func(line string) error {
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}

	want := []string{"2026-08-12T12:48:51.511890541Z app[web.1]: hello line 14"}
	if !slicesEqual(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}
