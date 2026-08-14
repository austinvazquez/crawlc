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
	"testing"
	"time"
)

// TestContainerLifecycle runs a container on crawlc with nothing configured.
//
// It is the fixture's own test. Everything else here breaks crawlc on purpose and
// then asserts something about containerd, which is only meaningful if an
// unconfigured crawlc is an ordinary shim first: create, start, kill, delete, and
// a shim that goes away at the end of it. When this fails, the failures in the
// other tests are crawlc's rather than containerd's.
func TestContainerLifecycle(t *testing.T) {
	d := newDaemon(t)
	d.start()
	d.waitReady(readyBudget)
	d.pull()

	const id = "crawlc-lifecycle"
	d.run(id, nil, "sleep", "3600")

	if got := d.taskStatus(id); got != "RUNNING" {
		t.Fatalf("task %s is %q, want RUNNING", id, got)
	}
	if pids := d.shimPids(); len(pids) != 1 {
		t.Fatalf("expected one crawlc shim, got %v", pids)
	}

	d.mustCtr(30*time.Second, "tasks", "kill", "--signal", "SIGKILL", id)
	d.waitTaskStatus(id, "STOPPED", 30*time.Second)

	d.mustCtr(30*time.Second, "tasks", "delete", id)
	d.mustCtr(30*time.Second, "containers", "delete", id)

	d.waitNoShims(30 * time.Second)
	if d.bundleExists(id) {
		t.Errorf("bundle %s outlived the container", d.bundle(id))
	}

	// Every other test kills containerd. This one lets it shut down, which is the
	// only place the suite checks that a containerd that has hosted a crawlc shim
	// still exits on SIGTERM.
	d.stop()
}
