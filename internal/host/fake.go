package host

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
)

// DirSpec is a directory [Fake.EnsureDir] was asked for.
//
// The owner is recorded rather than applied because chown needs root and a unit
// test does not have it. What a test wants to say is that the data directory
// was asked for at 0750 owned by the service account, and saying that needs no
// filesystem that can hold an owner.
type DirSpec struct {
	Mode  fs.FileMode
	Owner Account
}

// Fake is an in-memory host for tests.
//
// It covers exactly the surface of [Ops] and no more, so the interface grows
// one method at a time and the tests cannot drift away from what the host's own
// programs are actually asked to do.
type Fake struct {
	mu sync.Mutex

	// Users and Groups are the identities this host already has. Tests seed
	// them to stand for a Dokku host, or leave "dokku" out to stand for one
	// without Dokku on it.
	Users  map[string]Account
	Groups map[string]Account

	// Dirs, Sudoers and Memberships are what was asked for, so a test can
	// assert the data directory is not world-readable without needing a
	// filesystem that can hold an owner.
	Dirs        map[string]DirSpec
	Sudoers     map[string]string
	Memberships map[string][]string

	// Calls records every invocation in order.
	Calls []string

	// Err, when set, is returned by every method.
	Err error

	// FailAt, when set, is returned by the first method whose recorded call is
	// it, or begins with it followed by a space. It is how a test drives
	// "installation fails at step five" and asserts that steps one to four were
	// undone. Naming the verb alone -- "prove-sudo" -- is enough; the arguments
	// are there for a test that has to fail on one subject and not another.
	FailAt string
}

var _ Ops = (*Fake)(nil)

// NewFake returns a host that looks like a Dokku host: it has the dokku user
// and the dokku group, and nothing of dokkup's.
//
// 1001 is the real thing, measured on the devenv:
// `dokku:x:1001:1001:,,,:/home/dokku:/bin/bash`. It is above UID_MIN, which is
// why installation puts its own account below 1000 with --system rather than
// beside Dokku's.
func NewFake() *Fake {
	return &Fake{
		Users:       map[string]Account{"dokku": {Name: "dokku", Uid: 1001, Gid: 1001}},
		Groups:      map[string]Account{"dokku": {Name: "dokku", Uid: -1, Gid: 1001}},
		Dirs:        map[string]DirSpec{},
		Sudoers:     map[string]string{},
		Memberships: map[string][]string{},
	}
}

// The maps are exported so that a test can seed them, which means a Fake can
// arrive here built by hand and empty. Filling them at the one point every
// method passes through is cheaper to read than a nil check in each.
func (f *Fake) record(call string) error {
	if f.Users == nil {
		f.Users = map[string]Account{}
	}
	if f.Groups == nil {
		f.Groups = map[string]Account{}
	}
	if f.Dirs == nil {
		f.Dirs = map[string]DirSpec{}
	}
	if f.Sudoers == nil {
		f.Sudoers = map[string]string{}
	}
	if f.Memberships == nil {
		f.Memberships = map[string][]string{}
	}

	f.Calls = append(f.Calls, call)

	if f.FailAt != "" && (call == f.FailAt || strings.HasPrefix(call, f.FailAt+" ")) {
		return fmt.Errorf("host: the fake was told to fail at %q", f.FailAt)
	}
	return f.Err
}

// nextID returns an unused system id.
//
// It counts down from 999 because that is what shadow does: SYS_UID_MIN and
// SYS_GID_MIN are unset in /etc/login.defs on Ubuntu 24.04, so the compiled
// 100..999 range applies and --system allocates downwards inside it. Probe
// accounts on the devenv landed at 999, 996 and 995.
func (f *Fake) nextID() int {
	for id := 999; id > 100; id-- {
		taken := false
		for _, account := range f.Users {
			taken = taken || account.Uid == id
		}
		for _, account := range f.Groups {
			taken = taken || account.Gid == id
		}
		if !taken {
			return id
		}
	}
	return 0
}

// LookupUser implements [Ops].
func (f *Fake) LookupUser(_ context.Context, name string) (Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("lookup-user " + name); err != nil {
		return Account{}, err
	}
	account, ok := f.Users[name]
	if !ok {
		return Account{}, fmt.Errorf("%w: %s", ErrNoSuchUser, name)
	}
	return account, nil
}

// LookupGroup implements [Ops].
func (f *Fake) LookupGroup(_ context.Context, name string) (Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("lookup-group " + name); err != nil {
		return Account{}, err
	}
	account, ok := f.Groups[name]
	if !ok {
		return Account{}, fmt.Errorf("%w: %s", ErrNoSuchGroup, name)
	}
	return account, nil
}

// EnsureGroup implements [Ops].
func (f *Fake) EnsureGroup(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("ensure-group " + name); err != nil {
		return err
	}
	if _, ok := f.Groups[name]; !ok {
		f.Groups[name] = Account{Name: name, Uid: -1, Gid: f.nextID()}
	}
	return nil
}

// EnsureUser implements [Ops].
//
// A missing primary group is an error rather than one this invents, because
// that is what the real thing does: useradd --gid names a group and fails
// without it, and an installation that reached here without creating the group
// has a bug this fake should show rather than paper over.
func (f *Fake) EnsureUser(_ context.Context, spec UserSpec) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("ensure-user " + spec.Name); err != nil {
		return false, err
	}
	if _, ok := f.Users[spec.Name]; ok {
		return false, nil
	}
	group, ok := f.Groups[spec.Group]
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNoSuchGroup, spec.Group)
	}

	f.Users[spec.Name] = Account{Name: spec.Name, Uid: f.nextID(), Gid: group.Gid}
	return true, nil
}

// JoinGroup implements [Ops].
func (f *Fake) JoinGroup(_ context.Context, user, group string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("join-group " + user + " " + group); err != nil {
		return err
	}
	if _, ok := f.Users[user]; !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchUser, user)
	}
	if _, ok := f.Groups[group]; !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchGroup, group)
	}

	for _, joined := range f.Memberships[user] {
		if joined == group {
			return nil
		}
	}
	f.Memberships[user] = append(f.Memberships[user], group)
	return nil
}

// EnsureDir implements [Ops].
//
// The directory is really created, because the installer writes files into it
// straight afterwards and a test that had to fake that as well would be testing
// itself. Only the ownership is recorded rather than applied.
func (f *Fake) EnsureDir(_ context.Context, path string, mode fs.FileMode, owner Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("ensure-dir " + path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	// As in [Tools.EnsureDir]: MkdirAll's mode is filtered by the umask and
	// leaves an existing directory alone, so a test that stats the directory
	// would otherwise see neither what it asked for nor what a real
	// installation produces.
	if err := os.Chmod(path, mode); err != nil {
		return err
	}

	f.Dirs[path] = DirSpec{Mode: mode, Owner: owner}
	return nil
}

// WriteSudoers implements [Ops].
func (f *Fake) WriteSudoers(_ context.Context, path, rule string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("write-sudoers " + path); err != nil {
		return err
	}
	f.Sudoers[path] = rule
	return nil
}

// ProveSudo implements [Ops].
//
// It succeeds only if a rule has been written that names this user and this
// program. That is a shallow check, and it is the one worth having: what it
// catches is an installation reporting success without the rule, which is the
// failure that leaves a service unable to reach Dokku while every step of the
// install printed fine.
func (f *Fake) ProveSudo(_ context.Context, user, runAs, program string, _ ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("prove-sudo " + user + " " + runAs + " " + program); err != nil {
		return err
	}
	for _, rule := range f.Sudoers {
		if strings.HasPrefix(rule, user+" ") && strings.Contains(rule, program) {
			return nil
		}
	}
	return fmt.Errorf("%w: nothing on this host lets %s run %s", ErrSudoRefused, user, program)
}

// stillHasMembers reports whether any remaining identity belongs to the group,
// as a primary group or a supplementary one.
func (f *Fake) stillHasMembers(name string, gid int) bool {
	for _, account := range f.Users {
		if account.Gid == gid {
			return true
		}
	}
	for _, groups := range f.Memberships {
		if slices.Contains(groups, name) {
			return true
		}
	}
	return false
}

// RemoveUser implements [Ops].
//
// Removing the user takes a same-named primary group with it, because that is
// what userdel does -- measured on all three arrangements: same name and no
// members, the group was gone; same name plus another member, `userdel: group
// zz-probe-acct not removed because it has other members` and the group
// survived; a differently named primary group, it survived. Modelling it is
// what makes [Fake.RemoveGroup] answer "there was none" afterwards, which is
// the ordinary answer on a real host and the one a removal test has to be
// written against.
//
// Supplementary memberships go with the user and the groups themselves stay, so
// the dokku group survives here exactly as it does on a real host: intact,
// minus its dokkup member.
func (f *Fake) RemoveUser(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("remove-user " + name); err != nil {
		return false, err
	}
	account, ok := f.Users[name]
	if !ok {
		return false, nil
	}

	delete(f.Users, name)
	delete(f.Memberships, name)

	if group, ok := f.Groups[name]; ok && group.Gid == account.Gid &&
		!f.stillHasMembers(name, group.Gid) {
		delete(f.Groups, name)
	}
	return true, nil
}

// RemoveGroup implements [Ops].
func (f *Fake) RemoveGroup(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.record("remove-group " + name); err != nil {
		return false, err
	}
	if _, ok := f.Groups[name]; !ok {
		return false, nil
	}

	delete(f.Groups, name)
	return true, nil
}

// CheckProxy implements [Ops].
func (f *Fake) CheckProxy(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.record("check-proxy")
}

// CheckProxyFragment implements [Ops].
func (f *Fake) CheckProxyFragment(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.record("check-proxy-fragment " + path)
}
