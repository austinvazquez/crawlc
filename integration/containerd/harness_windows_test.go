//go:build windows

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
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// kill stops containerd the way a crash does, leaving its shims behind.
func (d *daemon) kill() {
	d.t.Helper()
	d.forceStop(10 * time.Second)
}

// stop shuts containerd down cleanly. On Windows there is no SIGTERM, so this
// also uses a hard kill — containerd is expected to handle the process exit.
func (d *daemon) stop() {
	d.t.Helper()
	d.forceStop(30 * time.Second)
}

func (d *daemon) forceStop(budget time.Duration) {
	d.t.Helper()

	if d.cmd == nil {
		return
	}
	if err := d.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		d.t.Errorf("failed to kill containerd: %v", err)
	}

	select {
	case <-d.done:
	case <-time.After(budget):
		d.t.Errorf("containerd did not exit within %s", budget)
		d.cancel()
		<-d.done
	}

	d.cancel()
	if err := d.logFile.Close(); err != nil {
		d.t.Errorf("failed to close daemon log: %v", err)
	}
	d.cmd, d.cancel, d.logFile, d.done = nil, nil, nil, nil
}

// killPid terminates a shim process by PID. Best effort.
func killPid(pid int) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Kill()
}

// shimPids lists crawlc shims started by this daemon.
//
// On Windows there is no /proc, so this queries WMI via PowerShell to find
// processes by name and then filters by the -address argument that containerd
// passes to every shim it spawns.
func (d *daemon) shimPids() []int {
	d.t.Helper()
	ctx := d.t.Context()

	// Get-CimInstance is the modern WMI interface available on Windows 8+.
	// The CommandLine property carries the full argv, so we can filter on the
	// unique socket address without touching any global state.
	script := `Get-CimInstance Win32_Process -Filter "Name='` + shimBinaryName + `.exe'" |` +
		` Where-Object { $_.CommandLine -like '*-address ` + d.address + `*' } |` +
		` Select-Object -ExpandProperty ProcessId`
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil
	}

	var pids []int
	for _, line := range strings.Fields(strings.TrimSpace(string(out))) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

// removeRuncState is a no-op on Windows: the hollow task service has no runc
// state to clean up.
func (d *daemon) removeRuncState(_ string) {}

// unmountRootfs is a no-op on Windows: the hollow task service never mounts
// a rootfs, so there is nothing to detach.
func unmountRootfs(_ *testing.T, _ string) {}
