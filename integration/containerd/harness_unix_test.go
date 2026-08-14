//go:build unix

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
	"syscall"
	"time"
)

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

// killPid sends SIGKILL to a shim process by PID. Best effort: the process
// may have already exited by the time cleanup runs.
func killPid(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
