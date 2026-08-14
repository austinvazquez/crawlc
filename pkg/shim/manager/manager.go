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

// Package manager provides the crawlc shim manager, which wraps a real shim
// manager to inject the crawlc task plugin.
package manager

import (
	"context"
	"errors"
	"io"
	"os"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	apitypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/log"

	"github.com/austinvazquez/crawlc/internal/shim/task"
)

// crawlcManager owns the delegate's lifecycle and defers everything else to the
// platform's manager: runc's on Linux, crawlc's own elsewhere.
//
// Spawning crawlc's own daemon is left to that fallback even in delegate mode,
// because every manager spawns os.Executable() — which is crawlc either way. The
// only thing delegation changes is that a second shim now has to be started
// before it and reaped after it.
type crawlcManager struct {
	name     string
	fallback shim.Shim
}

var _ shim.Shim = (*crawlcManager)(nil)

// New returns a shim.Shim that injects delays and optionally proxies to a
// delegate shim whose runtime is named in the bundle's OCI annotations.
func New(name string) shim.Shim {
	return &crawlcManager{name: name, fallback: newFallbackManager(name)}
}

func (m *crawlcManager) Name() string { return m.name }

func (m *crawlcManager) Start(ctx context.Context, opts *bootapi.BootstrapParams) (*bootapi.BootstrapResult, error) {
	runtime, err := task.DelegateRuntimeFromBundle()
	if err != nil {
		return nil, err
	}
	if runtime == "" {
		return m.fallback.Start(ctx, opts)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	// The delegate goes first. If it fails there is nothing to unwind, whereas
	// starting crawlc's daemon first and then failing would leave a live shim
	// that containerd has never been told about.
	st, err := task.SpawnDelegate(ctx, cwd, runtime, opts)
	if err != nil {
		return nil, err
	}
	if err := task.WriteDelegateState(cwd, st); err != nil {
		return nil, errors.Join(err, task.ReapDelegate(ctx, cwd, st))
	}

	res, err := m.fallback.Start(ctx, opts)
	if err != nil {
		// crawlc will never run, so nothing would ever reap the delegate.
		return nil, errors.Join(err, task.ReapDelegate(ctx, cwd, st), task.RemoveDelegateState(cwd))
	}
	return res, nil
}

func (m *crawlcManager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return shim.StopStatus{}, err
	}

	var reapErr error
	st, err := task.ReadDelegateState(cwd)
	switch {
	case err != nil:
		reapErr = err
	case st != nil:
		if reapErr = task.ReapDelegate(ctx, cwd, st); reapErr == nil {
			reapErr = task.RemoveDelegateState(cwd)
		} else {
			// Leave the state file: it is the only record of a delegate that is
			// still out there, and a later delete can retry from it.
			log.G(ctx).WithError(reapErr).WithField("delegate", st.Runtime).
				Error("failed to reap delegate shim")
		}
	}

	// crawlc's own cleanup runs whatever happened above, so that one stuck
	// delegate cannot strand the shim it was attached to. The reap failure is
	// still returned, because a silently leaked delegate looks exactly like the
	// leak crawlc exists to reproduce.
	status, err := m.fallback.Stop(ctx, id)
	return status, errors.Join(reapErr, err)
}

func (m *crawlcManager) Info(ctx context.Context, optionsR io.Reader) (*apitypes.RuntimeInfo, error) {
	return m.fallback.Info(ctx, optionsR)
}
