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

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	apitypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
)

// unixManager is a minimal shim manager for the BSD-derived platforms.
//
// containerd's own managers live with their runtimes, and runc's is Linux-only,
// so there is nothing to reuse here. This mirrors the parts of it that are not
// about running a container: allocate the socket, hand it to a daemonised copy of
// this binary, and report the address back. What is left out — cgroups, OOM score
// adjustment, sched_core, subreaper setup — is Linux-only anyway.
type unixManager struct {
	name string
}

var _ shim.Shim = (*unixManager)(nil)

func newFallbackManager(name string) shim.Shim {
	return &unixManager{name: name}
}

func (m *unixManager) Name() string { return m.name }

func (m *unixManager) Start(ctx context.Context, opts *bootapi.BootstrapParams) (_ *bootapi.BootstrapResult, retErr error) {
	id := opts.GetInstanceID()

	cmd, err := m.command(ctx, id, opts)
	if err != nil {
		return nil, err
	}

	socketDir := opts.GetSocketDir()
	if socketDir == "" {
		socketDir = filepath.Join(defaults.DefaultStateDir, "s")
	}

	address, err := shim.CreateSocketAddress(ctx, socketDir, opts.GetContainerdGrpcAddress(), id, false)
	if err != nil {
		return nil, err
	}

	socket, err := shim.NewSocket(address)
	if err != nil {
		if !shim.SocketEaddrinuse(err) {
			return nil, fmt.Errorf("create new shim socket: %w", err)
		}
		// A socket already in use with someone listening is a shim we started
		// earlier, so report it rather than displacing it. crawlc reaches this
		// often, since leaving shims behind is the point.
		if shim.CanConnect(address) {
			return &bootapi.BootstrapResult{
				Version:  3,
				Protocol: "ttrpc",
				Address:  address,
			}, nil
		}
		if err := shim.RemoveSocket(address); err != nil {
			return nil, fmt.Errorf("remove pre-existing socket: %w", err)
		}
		if socket, err = shim.NewSocket(address); err != nil {
			return nil, fmt.Errorf("try create new shim socket 2x: %w", err)
		}
	}
	defer func() {
		if retErr != nil {
			_ = socket.Close()
			_ = shim.RemoveSocket(address)
		}
	}()

	f, err := socket.File()
	if err != nil {
		return nil, fmt.Errorf("failed to get socket file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Fd 3 by convention: the child looks for its listener there when it is given
	// no -socket flag. See serveListener in containerd's pkg/shim.
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = cmd.Process.Kill()
		}
	}()
	// Reap the intermediate process. The daemon it spawned is re-parented to init
	// and outlives this invocation, which is what makes a shim a shim.
	go func() { _ = cmd.Wait() }()

	return &bootapi.BootstrapResult{
		Version:  3,
		Protocol: "ttrpc",
		Address:  address,
	}, nil
}

func (m *unixManager) command(ctx context.Context, id string, opts *bootapi.BootstrapParams) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	args := []string{
		"-namespace", ns,
		"-id", id,
		"-address", opts.GetContainerdGrpcAddress(),
	}
	if opts.GetLogLevel() <= bootapi.LogLevel_LOG_LEVEL_DEBUG {
		args = append(args, "-debug")
	}

	// Deliberately not exec.CommandContext: the shim has to outlive this start
	// invocation, and binding it to the context would kill the daemon the moment
	// the caller returned. containerd's own managers spawn theirs the same way.
	cmd := exec.Command(self, args...) //nolint:noctx // the daemon must outlive ctx
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")
	// A new process group keeps the shim from dying with the containerd that
	// started it, which is the precondition for every leaked-shim scenario.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func (m *unixManager) Stop(_ context.Context, _ string) (shim.StopStatus, error) {
	// The hollow task service owns no processes, so there is nothing to signal and
	// nothing to wait for. The bundle is torn down by containerd either way.
	//
	// Pid is left at zero rather than set to os.Getpid(): this runs in the
	// short-lived "delete" invocation, so its pid belongs to no task and to no
	// shim, and containerd publishes it on the task's exit event.
	return shim.StopStatus{
		Pid:        0,
		ExitStatus: 0,
		ExitedAt:   time.Now(),
	}, nil
}

func (m *unixManager) Info(_ context.Context, _ io.Reader) (*apitypes.RuntimeInfo, error) {
	return &apitypes.RuntimeInfo{
		Name: m.name,
		Annotations: map[string]string{
			"org.crawlc.hollow": "true",
		},
	}, nil
}
