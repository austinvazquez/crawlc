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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// runcRoot mirrors process.RuncRoot in containerd's runc shim, which is where
// runc's own container state lives, namespaced by containerd namespace.
//
// It is not under the test's state directory and cannot be moved there: `ctr`
// only passes runtime options for the runc runtime itself, so a container on
// crawlc always gets the default. That makes it the one piece of global state
// these tests touch, and the reason cleanup deletes runc containers by name
// instead of relying on the temp directory going away.
const runcRoot = "/run/containerd/runc"

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

// unmountRootfs detaches a bundle rootfs left by a shim that was killed before
// it could unmount cleanly. MNT_DETACH because the processes that held it open
// may have only just been killed and could still be briefly busy.
func unmountRootfs(t *testing.T, path string) {
	t.Helper()
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil &&
		!errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		t.Logf("failed to unmount %s: %v", path, err)
	}
}
