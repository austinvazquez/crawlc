//go:build !linux && !windows

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
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// shimPids lists the crawlc shims started by this daemon.
//
// On non-Linux platforms there is no /proc, so this uses pgrep to find
// candidate processes and ps to verify that each one was started by this
// daemon (keyed on its unique socket address).
func (d *daemon) shimPids() []int {
	d.t.Helper()

	ctx := d.t.Context()

	// pgrep -f matches the full command line, not just the process name.
	out, err := exec.CommandContext(ctx, "pgrep", "-f", shimBinaryName).Output()
	if err != nil {
		// A non-zero exit from pgrep means no matches — that is not an error.
		return nil
	}

	var pids []int
	for _, pidStr := range strings.Fields(strings.TrimSpace(string(out))) {
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		// Confirm this PID belongs to this test's daemon by checking the
		// -address argument, which containerd passes to every shim it spawns.
		argsOut, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
		if err != nil {
			continue
		}
		if strings.Contains(string(argsOut), "-address "+d.address) {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}

// removeRuncState is a no-op on non-Linux: the hollow task service has no runc
// state to clean up.
func (d *daemon) removeRuncState(_ string) {}

// unmountRootfs is a no-op on non-Linux: the hollow task service never mounts
// a rootfs, so there is nothing to detach.
func unmountRootfs(_ *testing.T, _ string) {}
