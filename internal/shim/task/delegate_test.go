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

package task

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
)

// TestDelegateSocketIDAvoidsCollision is the regression test for the failure
// this whole mechanism exists to avoid: a delegate started under the container's
// own id resolves to crawlc's socket, and containerd's manager then reports
// success while starting nothing, leaving crawlc proxying to itself.
func TestDelegateSocketIDAvoidsCollision(t *testing.T) {
	const (
		root = "/run/containerd/s"
		grpc = "/run/containerd/containerd.sock"
		id   = "web"
	)
	ctx := namespaces.WithNamespace(context.Background(), "default")

	own, err := shim.CreateSocketAddress(ctx, root, grpc, id, false)
	if err != nil {
		t.Fatal(err)
	}
	delegate, err := shim.CreateSocketAddress(ctx, root, grpc, delegateSocketID(id), false)
	if err != nil {
		t.Fatal(err)
	}

	if own == delegate {
		t.Fatalf("delegate socket collides with crawlc's own: %s", own)
	}

	// Same inputs must stay stable, or a later delete would compute a different
	// socket than the one that was started.
	again, err := shim.CreateSocketAddress(ctx, root, grpc, delegateSocketID(id), false)
	if err != nil {
		t.Fatal(err)
	}
	if again != delegate {
		t.Errorf("delegate socket is not deterministic: %s then %s", delegate, again)
	}
}

func TestResolveDelegateBinary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime string
		want    string
		wantErr bool
	}{
		{
			name:    "runtime handler maps through containerd naming",
			runtime: "io.containerd.nerdbox.v1",
			want:    "containerd-shim-nerdbox-v1",
		},
		{
			name:    "runc",
			runtime: "io.containerd.runc.v2",
			want:    "containerd-shim-runc-v2",
		},
		{
			name:    "absolute path is taken as given",
			runtime: "/usr/local/bin/containerd-shim-nerdbox-v1",
			want:    "/usr/local/bin/containerd-shim-nerdbox-v1",
		},
		{
			name:    "relative path is taken as given",
			runtime: "./_output/containerd-shim-nerdbox-v1",
			want:    "./_output/containerd-shim-nerdbox-v1",
		},
		{
			name:    "empty",
			runtime: "",
			wantErr: true,
		},
		{
			name:    "not a runtime name",
			runtime: "nerdbox",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDelegateBinary(tc.runtime)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDelegateStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Absent is not an error: most bundles have no delegate.
	st, err := ReadDelegateState(dir)
	if err != nil {
		t.Fatalf("unexpected error for absent state: %v", err)
	}
	if st != nil {
		t.Fatalf("expected nil state, got %+v", st)
	}

	want := &DelegateState{
		Runtime:      "io.containerd.nerdbox.v1",
		Binary:       "containerd-shim-nerdbox-v1",
		ID:           delegateSocketID("web"),
		TaskID:       "web",
		Address:      "unix:///run/containerd/s/abc",
		Protocol:     "ttrpc",
		Version:      3,
		GRPCAddress:  "/run/containerd/containerd.sock",
		TTRPCAddress: "/run/containerd/containerd.sock.ttrpc",
	}
	if err := WriteDelegateState(dir, want); err != nil {
		t.Fatal(err)
	}

	got, err := ReadDelegateState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != *want {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, want)
	}

	if err := RemoveDelegateState(dir); err != nil {
		t.Fatal(err)
	}
	// Removing twice must not fail, because a delete can run more than once.
	if err := RemoveDelegateState(dir); err != nil {
		t.Fatalf("second remove should be a no-op: %v", err)
	}
	if st, _ := ReadDelegateState(dir); st != nil {
		t.Fatal("state survived removal")
	}
}

func TestDelegateStateCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, delegateStateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A corrupt file must be loud. Treating it as "no delegate" would leak the
	// shim it describes.
	if _, err := ReadDelegateState(dir); err == nil {
		t.Fatal("expected an error for a corrupt state file")
	}
}

// TestDelegateStateFileIsNotABundleName guards the collision analysis: the file
// crawlc adds must not be one containerd already writes.
func TestDelegateStateFileIsNotABundleName(t *testing.T) {
	for _, reserved := range []string{
		"config.json", "bootstrap.json", "shim-binary-path", "address", "log", "work", "rootfs", "options.json", "runtime",
	} {
		if delegateStateFile == reserved {
			t.Fatalf("delegate state file %q collides with a containerd bundle entry", delegateStateFile)
		}
	}
}
