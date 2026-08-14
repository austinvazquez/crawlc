//go:build linux

/*
   Copyright The crawlc Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package containerd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The shim timeouts every test daemon is configured with. They are written into
// the config rather than left at containerd's defaults so that the budgets the
// tests assert against are the ones containerd is actually using, whatever the
// release under test defaults to.
const (
	loadTimeout     = 5 * time.Second
	cleanupTimeout  = 5 * time.Second
	shutdownTimeout = 3 * time.Second
)

// readyBudget is how long a daemon with nothing interesting to load may take to
// serve. Generous: it covers plugin initialization on a cold, loaded CI runner,
// and no test measures anything against it.
const readyBudget = 30 * time.Second

// annotationPrefix mirrors the constant of the same meaning in crawlc itself,
// which cannot be imported here because crawlc is package main. The duplication
// is deliberate beyond that: these tests drive crawlc the way a user does,
// through the documented annotation names, so a rename that breaks users breaks
// them too.
const annotationPrefix = "io.containerd.crawlc."

// armFileName is the arm file every test uses, relative to the bundle.
const armFileName = "crawlc.arm"

// bundlesDir is where the v2 runtime keeps bundles under the state directory.
const bundlesDir = "io.containerd.runtime.v2.task"

// runcRoot mirrors process.RuncRoot in containerd's runc shim, which is where
// runc's own container state lives, namespaced by containerd namespace.
//
// It is not under the test's state directory and cannot be moved there: `ctr`
// only passes runtime options for the runc runtime itself, so a container on
// crawlc always gets the default. That makes it the one piece of global state
// these tests touch, and the reason cleanup deletes runc containers by name
// instead of relying on the temp directory going away.
const runcRoot = "/run/containerd/runc"

// daemon is a containerd started for a single test, with its own root, state and
// socket, so that tests neither see each other's shims nor touch a containerd
// already running on the machine.
type daemon struct {
	t *testing.T

	root    string
	state   string
	address string
	config  string
	logPath string

	// Set while running.
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	logFile   *os.File
	startedAt time.Time
	done      chan struct{}
	waitErr   error

	// ids is every container created through run, for cleanup. Bundles are not
	// enough on their own: containerd deletes the bundle of a shim it gave up
	// on, and runc's record of the container outlives it.
	ids []string

	// logged keeps the log from being printed twice when a failure inside the
	// test is followed by cleanup.
	logged bool
}

func newDaemon(t *testing.T) *daemon {
	t.Helper()
	requireIntegration(t)

	// Not t.TempDir(): containerd's socket path lives here and a unix socket
	// address is limited to about 108 bytes, which the test name and sequence
	// number in a t.TempDir() path can eat into. This keeps it short and the
	// cleanup below removes it.
	dir, err := os.MkdirTemp("", "crawlc-it")
	if err != nil {
		t.Fatalf("failed to create test dir: %v", err)
	}

	d := &daemon{
		t:       t,
		root:    filepath.Join(dir, "root"),
		state:   filepath.Join(dir, "state"),
		address: filepath.Join(dir, "c.sock"),
		config:  filepath.Join(dir, "config.toml"),
		logPath: filepath.Join(dir, "containerd.log"),
	}

	for _, p := range []string{d.root, d.state} {
		if err := os.MkdirAll(p, 0o711); err != nil {
			t.Fatalf("failed to create %s: %v", p, err)
		}
	}
	if err := os.WriteFile(d.config, []byte(d.configTOML()), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	t.Cleanup(func() {
		d.cleanup()
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("failed to remove test dir: %v", err)
		}
	})

	return d
}

func (d *daemon) configTOML() string {
	return fmt.Sprintf(`version = 3

# Written out rather than left to the defaults so that the budgets asserted on
# are the ones containerd is using.
[timeouts]
  "io.containerd.timeout.shim.load" = %q
  "io.containerd.timeout.shim.cleanup" = %q
  "io.containerd.timeout.shim.shutdown" = %q
`, loadTimeout, cleanupTimeout, shutdownTimeout)
}

// start launches containerd. It does not wait for it to serve; waitReady does.
func (d *daemon) start() {
	d.t.Helper()

	if d.cmd != nil {
		d.t.Fatal("daemon is already running")
	}

	// Appended to across restarts, so one log holds the whole test.
	logFile, err := os.OpenFile(d.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		d.t.Fatalf("failed to open daemon log: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, containerdBin,
		"--config", d.config,
		"--root", d.root,
		"--state", d.state,
		"--address", d.address,
		"--log-level", "debug",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// containerd execs the shim by name off its own PATH, so the shim under test
	// has to lead it. runc and the rest still come from the inherited PATH.
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(shimBin)+string(os.PathListSeparator)+os.Getenv("PATH"))

	started := time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		_ = logFile.Close()
		d.t.Fatalf("failed to start containerd: %v", err)
	}

	done := make(chan struct{})
	go func() {
		d.waitErr = cmd.Wait()
		close(done)
	}()

	d.cmd, d.cancel, d.logFile, d.startedAt, d.done = cmd, cancel, logFile, started, done
}

// waitReady blocks until containerd serves its API, and returns how long that
// took measured from start.
//
// The elapsed time is the point of several tests: a shim that never answers used
// to keep containerd from ever creating this socket.
func (d *daemon) waitReady(budget time.Duration) time.Duration {
	d.t.Helper()

	deadline := time.Now().Add(budget)
	for {
		select {
		case <-d.done:
			d.dumpLog()
			d.t.Fatalf("containerd exited before serving: %v", d.waitErr)
		default:
		}

		if d.serving() {
			return time.Since(d.startedAt)
		}
		if time.Now().After(deadline) {
			d.dumpLog()
			d.t.Fatalf("containerd did not serve within %s", budget)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// serving reports whether the API socket answers.
//
// The dial is the fast check: the socket is created last, when plugin
// initialization is done, and a stale socket file left by a killed daemon
// refuses connections rather than accepting them. The version call then confirms
// the server behind it is really serving.
func (d *daemon) serving() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", d.address)
	if err != nil {
		return false
	}
	if err := conn.Close(); err != nil {
		return false
	}

	_, err = d.ctr(5*time.Second, "version")
	return err == nil
}

// kill stops containerd the way a crash does, leaving its shims behind. A clean
// shutdown reaps them, which is the opposite of what most of these tests need.
func (d *daemon) kill() {
	d.t.Helper()
	d.signalAndWait(syscall.SIGKILL, 10*time.Second)
}

// stop shuts containerd down cleanly.
func (d *daemon) stop() {
	d.t.Helper()
	d.signalAndWait(syscall.SIGTERM, 30*time.Second)
}

func (d *daemon) signalAndWait(sig syscall.Signal, budget time.Duration) {
	d.t.Helper()

	if d.cmd == nil {
		return
	}
	if err := d.cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		d.t.Errorf("failed to signal containerd with %s: %v", sig, err)
	}

	select {
	case <-d.done:
	case <-time.After(budget):
		// Escalate rather than hang the test: the process is going away either
		// way, and a stuck SIGTERM is worth seeing in the output.
		d.t.Errorf("containerd did not exit within %s of %s, killing", budget, sig)
		d.cancel()
		<-d.done
	}

	d.cancel()
	if err := d.logFile.Close(); err != nil {
		d.t.Errorf("failed to close daemon log: %v", err)
	}
	d.cmd, d.cancel, d.logFile, d.done = nil, nil, nil, nil
}

// restart kills containerd and starts a new one on the same root and state, which
// is what makes the shims left behind into shims a later containerd has to load.
func (d *daemon) restart(budget time.Duration) time.Duration {
	d.t.Helper()
	d.kill()
	d.start()
	return d.waitReady(budget)
}

// ctr runs the containerd CLI against this daemon.
func (d *daemon) ctr(budget time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	full := append([]string{"--address", d.address, "--namespace", testNamespace}, args...)
	cmd := exec.CommandContext(ctx, ctrBin, full...)
	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ctr %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func (d *daemon) mustCtr(budget time.Duration, args ...string) string {
	d.t.Helper()
	out, err := d.ctr(budget, args...)
	if err != nil {
		d.t.Fatalf("%v", err)
	}
	return out
}

// pull fetches the test image into this daemon's content store.
func (d *daemon) pull() {
	d.t.Helper()

	args := []string{"images", "pull", "--platform", "linux/" + runtime.GOARCH}
	if snapshotter != "" {
		// The transfer service refuses to unpack for a non-default snapshotter
		// ("no unpack platforms defined"), so a run with an overridden
		// snapshotter takes the client-side pull path instead.
		args = append(args, "--snapshotter", snapshotter, "--local")
	}
	args = append(args, testImage)

	d.mustCtr(5*time.Minute, args...)
}

// run creates and starts a container on crawlc. The annotation keys are relative
// to crawlc's prefix, so callers pass "delay.Pids" rather than the full name.
func (d *daemon) run(id string, annotations map[string]string, args ...string) {
	d.t.Helper()

	argv := []string{"run", "-d", "--runtime", RuntimeName}
	if snapshotter != "" {
		argv = append(argv, "--snapshotter", snapshotter)
	}
	// Sorted so that a failing command line is the same one every run.
	keys := make([]string, 0, len(annotations))
	for k := range annotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--annotation", annotationPrefix+k+"="+annotations[k])
	}
	argv = append(argv, testImage, id)
	argv = append(argv, args...)

	// A run that was killed outright — `go test -timeout`, a cancelled CI job —
	// never reaches its cleanup, and runc's state is the one part of it that
	// outlives the temp directory. Clearing it here is what makes the suite
	// rerunnable after that, instead of failing with "container with given ID
	// already exists" until someone cleans up by hand.
	d.removeRuncState(id)

	d.ids = append(d.ids, id)
	d.mustCtr(2*time.Minute, argv...)
}

// removeRuncState deletes a container from runc's state, killing whatever is
// still running in it. Best effort: a container that was never created is the
// normal case.
func (d *daemon) removeRuncState(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, runcBin,
		"--root", filepath.Join(runcRoot, testNamespace),
		"delete", "--force", id,
	)
	if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(out), "does not exist") {
		d.t.Logf("runc delete %s: %v: %s", id, err, out)
	}
}

// bundle is the on-disk bundle for a container, which is also the shim's working
// directory and so the root the arm file is resolved against.
func (d *daemon) bundle(id string) string {
	return filepath.Join(d.state, bundlesDir, testNamespace, id)
}

func (d *daemon) bundleExists(id string) bool {
	_, err := os.Stat(d.bundle(id))
	return err == nil
}

// arm creates the arm file, switching the configured delays on. Nothing before
// this point is delayed, which is what lets a test set up a container whose shim
// is about to stop answering.
func (d *daemon) arm(id string) {
	d.t.Helper()

	f, err := os.Create(filepath.Join(d.bundle(id), armFileName))
	if err != nil {
		d.t.Fatalf("failed to arm %s: %v", id, err)
	}
	if err := f.Close(); err != nil {
		d.t.Fatalf("failed to arm %s: %v", id, err)
	}
}

// taskStatus returns the status column `ctr tasks ls` reports for id, or "" if
// the task is not listed.
func (d *daemon) taskStatus(id string) string {
	d.t.Helper()

	out := d.mustCtr(30*time.Second, "tasks", "ls")
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == id {
			return fields[2]
		}
	}
	return ""
}

// waitTaskStatus polls until a task reaches want, which is how a test waits for
// a killed task to actually be gone before restarting containerd.
func (d *daemon) waitTaskStatus(id, want string, budget time.Duration) {
	d.t.Helper()

	deadline := time.Now().Add(budget)
	var last string
	for time.Now().Before(deadline) {
		if last = d.taskStatus(id); last == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	d.t.Fatalf("task %s was %q, not %q, after %s", id, last, want, budget)
}

// shimPids lists the crawlc shims started by this daemon.
//
// The daemon's own address is the discriminator: containerd passes it to every
// shim it spawns, so this never picks up a shim belonging to another test or to
// a containerd already running on the machine.
func (d *daemon) shimPids() []int {
	d.t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		d.t.Fatalf("failed to read /proc: %v", err)
	}

	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			// The process exited between the listing and the read.
			continue
		}
		argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if len(argv) == 0 || filepath.Base(argv[0]) != shimBinaryName {
			continue
		}
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "-address" && argv[i+1] == d.address {
				pids = append(pids, pid)
				break
			}
		}
	}
	sort.Ints(pids)
	return pids
}

// waitNoShims polls until no crawlc shim of this daemon is left, and fails if any
// outlives the budget. Shim exit is asynchronous to the call that triggered it,
// so the wait is what makes "the shim was reaped" checkable.
func (d *daemon) waitNoShims(budget time.Duration) {
	d.t.Helper()

	deadline := time.Now().Add(budget)
	var last []int
	for time.Now().Before(deadline) {
		if last = d.shimPids(); len(last) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	d.t.Fatalf("crawlc shims %v still running after %s", last, budget)
}

// cleanup tears down everything the test may have left running, in the order it
// has to happen: containerd first so nothing is restarted under us, then the
// shims, then the container processes they were supervising, then the mounts
// those processes' bundles still hold.
//
// A wedged shim is the normal end state here rather than a failure, so none of
// this is reported as a test error. What is reported is the containerd log, and
// only when the test failed.
func (d *daemon) cleanup() {
	d.t.Helper()
	if d.cmd != nil {
		d.signalAndWait(syscall.SIGKILL, 10*time.Second)
	}

	for _, pid := range d.shimPids() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	// The containers outlive the shims that were supervising them, and runc's
	// record of them outlives the test directory, so they go by name rather than
	// by walking the bundles: a bundle containerd gave up on is already gone.
	for _, id := range d.ids {
		d.removeRuncState(id)
	}

	for _, bundle := range d.bundles() {
		// Belt and braces for a container runc has no record of: the init
		// process is still a `sleep` that would outlive the test run.
		if raw, err := os.ReadFile(filepath.Join(bundle, "init.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}

		// A shim killed mid-life never unmounts its rootfs, and the leftover
		// mount would defeat the removal of the test directory. MNT_DETACH
		// because a mount whose processes have only just been killed can still
		// be busy.
		rootfs := filepath.Join(bundle, "rootfs")
		if err := unix.Unmount(rootfs, unix.MNT_DETACH); err != nil &&
			!errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			d.t.Logf("failed to unmount %s: %v", rootfs, err)
		}
	}

	if d.t.Failed() {
		d.dumpLog()
	}
}

func (d *daemon) bundles() []string {
	dir := filepath.Join(d.state, bundlesDir, testNamespace)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// dumpLog reports the tail of the containerd log. Only the tail: at debug level
// a startup is thousands of lines, and the interesting ones are the last.
func (d *daemon) dumpLog() {
	d.t.Helper()

	const tail = 80

	if d.logged {
		return
	}
	d.logged = true

	raw, err := os.ReadFile(d.logPath)
	if err != nil {
		d.t.Logf("failed to read containerd log: %v", err)
		return
	}
	trimmed := strings.TrimRight(string(raw), "\n")
	var lines []string
	if trimmed != "" {
		lines = strings.Split(trimmed, "\n")
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	if len(lines) == 0 {
		d.t.Logf("containerd log is empty")
		return
	}
	d.t.Logf("containerd log (last %d lines):\n%s", len(lines), strings.Join(lines, "\n"))
}
