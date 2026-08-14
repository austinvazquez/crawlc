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
	"slices"
	"testing"
	"time"
)

// TestWedgedShimDoesNotStallStartup is the scenario crawlc was built for.
//
// containerd loads the shims left in its state directory during plugin
// initialization, before its API socket exists. A shim leaked by a previous
// containerd still owns its socket, so the new one connects to it and then waits
// for an answer. If that wait is unbounded, containerd never finishes starting:
// no API socket, and every client hangs.
//
// Pids is the call to wedge because it is the one containerd makes while deciding
// whether a loaded shim is worth keeping.
func TestWedgedShimDoesNotStallStartup(t *testing.T) {
	requireBoundedShimLoad(t)
	d := newDaemon(t)

	d.start()
	d.waitReady(readyBudget)
	d.pull()

	const id = "wedged"
	d.run(id, map[string]string{
		"delay.Pids": "forever",
		"arm.file":   armFileName,
	}, "sleep", "3600")

	// Arm only now: a shim told to wedge from the start wedges during its own
	// creation and never becomes the leaked shim this is about.
	d.arm(id)

	// The budget is the load timeout plus room for the rest of a startup. Without
	// a bound this does not fail, it hangs, which is why the whole test is gated
	// on the containerd under test having one.
	elapsed := d.restart(loadTimeout + 15*time.Second)
	t.Logf("containerd served %s after restarting onto a wedged shim", elapsed)

	// Under the load timeout means the shim answered, so the test proved nothing:
	// the arming, the annotation or the bundle path is wrong.
	if elapsed < loadTimeout {
		t.Fatalf("containerd served in %s, faster than the %s load timeout: the shim was not wedged",
			elapsed, loadTimeout)
	}

	// Giving up on the load is not the same as declaring the shim dead. A shim
	// that did not answer in time may still be running a container, so containerd
	// keeps it registered rather than reaping it and orphaning the workload.
	if !d.bundleExists(id) {
		t.Errorf("bundle %s was removed; a shim that timed out is not a dead shim", d.bundle(id))
	}
	if pids := d.shimPids(); len(pids) != 1 {
		t.Errorf("expected the wedged shim to still be running, got %v", pids)
	}
	if got := d.taskStatus(id); got != "RUNNING" {
		t.Errorf("task %s is %q, want RUNNING: the wedged shim should still be registered", id, got)
	}
}

// TestSlowShimIsStillLoaded is the other side of that branch.
//
// A bound on the load is only correct if it is a bound and not a deadline every
// shim has to beat. A shim that answers late but inside the budget has to survive
// the restart with its task intact, otherwise the fix for a hung startup becomes a
// way to reap healthy shims on a loaded machine.
func TestSlowShimIsStillLoaded(t *testing.T) {
	d := newDaemon(t)

	d.start()
	d.waitReady(readyBudget)
	d.pull()

	// Comfortably inside the load timeout, so the shim is late rather than
	// unresponsive, and comfortably above the cost of spawning `ctr`, so the
	// delay is measurable in the check below.
	const slowPidsDelay = time.Second

	const id = "slow"
	d.run(id, map[string]string{
		"delay.Pids": slowPidsDelay.String(),
		"arm.file":   armFileName,
	}, "sleep", "3600")

	before := d.shimPids()
	if len(before) != 1 {
		t.Fatalf("expected one crawlc shim, got %v", before)
	}
	d.arm(id)

	elapsed := d.restart(readyBudget)
	t.Logf("containerd served %s after restarting onto a slow shim", elapsed)

	// The startup budget here is generous enough that a shim which is not slow at
	// all would sail through every assertion below, so the delay is measured
	// directly rather than inferred from the restart. `ctr tasks ps` is the Pids
	// call, which is the one that was configured, and it is still armed.
	//
	// Without this the test passes when the annotation name, the arm file or the
	// bundle path is wrong — the same false pass its two neighbours guard against
	// by requiring the restart to have taken at least a timeout.
	pidsStart := time.Now()
	d.mustCtr(30*time.Second, "tasks", "ps", id)
	if took := time.Since(pidsStart); took < slowPidsDelay {
		t.Errorf("Pids answered in %s, faster than the configured %s delay: the shim was not slowed",
			took, slowPidsDelay)
	}

	if after := d.shimPids(); !slices.Equal(before, after) {
		t.Errorf("shims were %v before the restart and %v after: a slow shim was reaped or replaced", before, after)
	}
	if !d.bundleExists(id) {
		t.Errorf("bundle %s was removed", d.bundle(id))
	}
	if got := d.taskStatus(id); got != "RUNNING" {
		t.Errorf("task %s is %q, want RUNNING", id, got)
	}
}

// TestUnreapableShimIsRemovedFromState covers the cleanup half of the load path.
//
// A shim whose task is gone is one containerd reaps while loading it. When that
// reap fails — here because Delete never returns — containerd has to decide what
// to do with the bundle. Keeping it means loading the same doomed shim on every
// start, spending the full cleanup budget on it every time, so the bundle goes
// even though the process cannot be made to.
func TestUnreapableShimIsRemovedFromState(t *testing.T) {
	requireBoundedShimLoad(t)
	d := newDaemon(t)

	d.start()
	d.waitReady(readyBudget)
	d.pull()

	const id = "unreapable"
	d.run(id, map[string]string{
		"delay.Delete": "forever",
		"arm.file":     armFileName,
	}, "sleep", "3600")

	// A shim is only reaped during load if its task is gone, so kill the task
	// first and leave the shim behind holding nothing.
	d.mustCtr(30*time.Second, "tasks", "kill", "--signal", "SIGKILL", id)
	d.waitTaskStatus(id, "STOPPED", 30*time.Second)

	d.arm(id)

	elapsed := d.restart(loadTimeout + cleanupTimeout + 20*time.Second)
	t.Logf("containerd served %s after restarting onto an unreapable shim", elapsed)

	if elapsed < cleanupTimeout {
		t.Fatalf("containerd served in %s, faster than the %s cleanup timeout: the delete was not wedged",
			elapsed, cleanupTimeout)
	}

	if d.bundleExists(id) {
		t.Errorf("bundle %s survived a failed reap; it would be loaded again on every start", d.bundle(id))
	}

	// The shim itself is still out there, and that is the point rather than an
	// oversight. containerd falls back to Shutdown when the delete fails, but a
	// shim whose Delete never returned still believes it is holding a container,
	// and the task service declines to shut down while it holds one. containerd
	// does not escalate to a signal, because a shim that is merely unresponsive
	// may still be supervising a live workload. Removing the state is the whole
	// of what containerd can do here — hence this test.
	if pids := d.shimPids(); len(pids) != 1 {
		t.Errorf("expected the unreapable shim to still be running, got %v", pids)
	}

	// And the next start is an ordinary one, with nothing left to give up on.
	second := d.restart(readyBudget)
	t.Logf("containerd served %s on the following start", second)
	if d.bundleExists(id) {
		t.Errorf("bundle %s came back", d.bundle(id))
	}
}
