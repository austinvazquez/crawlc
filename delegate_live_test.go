//go:build !windows

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

package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/fifo"
	"golang.org/x/sys/unix"
)

// TestSpawnAndReapDelegate drives a real shim binary through the lifecycle.
//
// It is opt-in because it needs one on disk. Set CRAWLC_TEST_DELEGATE to a shim
// binary — any shim will do, the point is the handshake rather than the runtime:
//
//	CRAWLC_TEST_DELEGATE=$PWD/bin/containerd-shim-runc-v2 go test -run Delegate ./...
//
// The socket directory is a temp dir rather than /run/containerd/s so that the
// test needs no privileges and leaves nothing behind on a shared machine.
func TestSpawnAndReapDelegate(t *testing.T) {
	binary := os.Getenv("CRAWLC_TEST_DELEGATE")
	if binary == "" {
		t.Skip("set CRAWLC_TEST_DELEGATE to a shim binary to run this")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("CRAWLC_TEST_DELEGATE=%q: %v", binary, err)
	}

	ctx := namespaces.WithNamespace(context.Background(), "default")

	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bundle, "rootfs"), 0o711); err != nil {
		t.Fatal(err)
	}

	// Not t.TempDir(): its paths are long, and a shim socket is the directory
	// plus a 64-character sha256, which overruns the ~108 byte limit on a unix
	// socket path. containerd defaults to /run/containerd/s for the same reason.
	sockDir, err := os.MkdirTemp("/tmp", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	// A shim opens the bundle's log FIFO write-only and without O_CREAT, so it
	// exits immediately unless the reader already exists. containerd creates and
	// drains it before starting any shim; this stands in for that, and without it
	// the delegate dies before it can answer anything.
	logFifo, err := fifo.OpenFifo(ctx, filepath.Join(bundle, "log"),
		unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFifo.Close() })
	go func() { _, _ = io.Copy(io.Discard, logFifo) }()

	opts := &bootapi.BootstrapParams{
		InstanceID:            "crawlc-live-test",
		Namespace:             "default",
		ContainerdGrpcAddress: "/run/containerd/containerd.sock",
		SocketDir:             &sockDir,
	}

	st, err := spawnDelegate(ctx, bundle, binary, opts)
	if err != nil {
		t.Fatalf("spawnDelegate: %v", err)
	}

	if st.ID != delegateSocketID(opts.GetInstanceID()) {
		t.Errorf("delegate id = %q, want the suffixed form", st.ID)
	}
	if st.Address == "" {
		t.Fatal("delegate returned no address")
	}
	if st.Protocol != "ttrpc" {
		t.Errorf("protocol = %q, want ttrpc", st.Protocol)
	}

	// The delegate must have landed on its own socket, not on the one crawlc
	// would occupy for this container.
	socks, err := os.ReadDir(sockDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(socks) != 1 {
		t.Fatalf("expected exactly one delegate socket, got %d", len(socks))
	}

	if err := writeDelegateState(bundle, st); err != nil {
		t.Fatal(err)
	}
	got, err := readDelegateState(bundle)
	if err != nil || got == nil {
		t.Fatalf("state not readable back: %v", err)
	}

	// Forward a real call through to the delegate. Connect is the useful one: it
	// is what containerd itself calls while loading a shim, and it is the method
	// crawlc rewrites, so a pass here covers both the transport and the override.
	t.Run("forward Connect", func(t *testing.T) {
		fwd, err := newForwardingTaskService(got, &liveShutdown{})
		if err != nil {
			t.Fatalf("failed to dial the delegate: %v", err)
		}

		res, err := fwd.Connect(ctx, &taskapi.ConnectRequest{ID: opts.GetInstanceID()})
		if err != nil {
			t.Fatalf("Connect through the delegate failed: %v", err)
		}
		if res.ShimPid != selfPid() {
			t.Errorf("ShimPid = %d, want the test process %d", res.ShimPid, selfPid())
		}
		t.Logf("delegate answered: taskPid=%d version=%q", res.TaskPid, res.Version)
	})

	if err := reapDelegate(ctx, bundle, got); err != nil {
		t.Fatalf("reapDelegate: %v", err)
	}
	if err := removeDelegateState(bundle); err != nil {
		t.Fatal(err)
	}
	if st, _ := readDelegateState(bundle); st != nil {
		t.Fatal("delegate state survived the reap")
	}

	// The reap must take the process with it. The delete action alone cleans up
	// task state and leaves a live daemon running, which once made this test pass
	// while leaking a shim per run.
	if _, err := os.Stat(strings.TrimPrefix(got.Address, "unix://")); err == nil {
		d := net.Dialer{Timeout: time.Second}
		if conn, derr := d.DialContext(ctx, "unix", strings.TrimPrefix(got.Address, "unix://")); derr == nil {
			_ = conn.Close()
			t.Error("delegate is still listening after the reap")
		}
	}
}

// liveShutdown records shutdown requests without ending the test process.
type liveShutdown struct{ calls int }

func (l *liveShutdown) Shutdown()                                    { l.calls++ }
func (l *liveShutdown) RegisterCallback(func(context.Context) error) {}
func (l *liveShutdown) Done() <-chan struct{}                        { return nil }
func (l *liveShutdown) Err() error                                   { return nil }
