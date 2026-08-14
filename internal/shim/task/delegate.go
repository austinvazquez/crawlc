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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/proto"
)

const (
	// DelegateAnnotation names the runtime crawlc should forward to, either a
	// runtime handler like io.containerd.nerdbox.v1 or an absolute binary path.
	// Unset means the built-in behaviour: runc on Linux, hollow elsewhere.
	DelegateAnnotation = AnnotationPrefix + "delegate"

	// delegateIDSuffix separates the delegate's socket from crawlc's own.
	//
	// Socket addresses are sha256(grpcAddress/namespace/id), so a delegate
	// started under the container's own id resolves to the socket crawlc is
	// already listening on. containerd's manager treats an in-use socket with a
	// live peer as "already started" and hands back that address without
	// spawning anything, which would leave crawlc proxying to itself — and it
	// reports success while doing so. The suffix is what keeps the two apart.
	// containerd relies on the same trick for a shim's debug socket.
	delegateIDSuffix = ".crawlc-delegate"

	// delegateStateFile records the delegate inside the bundle. It has to be on
	// disk rather than in memory because the processes that need it are
	// different: `start` spawns the delegate, the long-running daemon dials it,
	// and a later `delete` has to reap it. containerd writes no file by this
	// name, and shims write nothing to the bundle at all, so it cannot collide.
	delegateStateFile = "crawlc-delegate.json"
)

// DelegateState is what crawlc needs to reach and later reap its delegate.
type DelegateState struct {
	Runtime string `json:"runtime"`
	Binary  string `json:"binary"`
	// ID is the delegate's socket identity, deliberately not the container's.
	ID string `json:"id"`
	// TaskID is the container, which is what task API calls are addressed to.
	TaskID       string `json:"taskID"`
	Address      string `json:"address"`
	Protocol     string `json:"protocol"`
	Version      int32  `json:"version"`
	GRPCAddress  string `json:"grpcAddress"`
	TTRPCAddress string `json:"ttrpcAddress"`
}

// delegateSocketID derives the delegate's socket identity from the container id.
func delegateSocketID(id string) string {
	return id + delegateIDSuffix
}

// resolveDelegateBinary turns the annotation value into a binary to exec.
//
// A path is taken as given so that an unreleased or locally built shim can be
// pointed at directly; anything else is treated as a runtime handler and mapped
// through containerd's own naming, so io.containerd.nerdbox.v1 finds
// containerd-shim-nerdbox-v1 on PATH exactly as containerd would.
func resolveDelegateBinary(runtime string) (string, error) {
	if runtime == "" {
		return "", errors.New("empty delegate runtime")
	}
	if strings.ContainsRune(runtime, os.PathSeparator) {
		return runtime, nil
	}
	binary := shim.BinaryName(runtime)
	if binary == "" {
		return "", fmt.Errorf("delegate %q is not a valid runtime name", runtime)
	}
	return binary, nil
}

// DelegateRuntimeFromBundle returns the configured delegate runtime, or "" for none.
func DelegateRuntimeFromBundle() (string, error) {
	a, err := bundleAnnotations()
	if err != nil {
		return "", err
	}
	return a[DelegateAnnotation], nil
}

func delegateStatePath(bundlePath string) string {
	return filepath.Join(bundlePath, delegateStateFile)
}

// ReadDelegateState returns nil without error when no delegate was configured,
// which is the common case and not a failure.
func ReadDelegateState(bundlePath string) (*DelegateState, error) {
	b, err := os.ReadFile(delegateStatePath(bundlePath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var st DelegateState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", delegateStateFile, err)
	}
	return &st, nil
}

// WriteDelegateState persists delegate state inside the bundle.
func WriteDelegateState(bundlePath string, st *DelegateState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(delegateStatePath(bundlePath), b, 0o600)
}

// RemoveDelegateState removes the delegate state file from the bundle.
// Removing a file that does not exist is not an error.
func RemoveDelegateState(bundlePath string) error {
	err := os.Remove(delegateStatePath(bundlePath))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// delegateLogLevel converts the level containerd started crawlc at into the one
// shim.Command expects.
//
// Leaving CommandConfig.LogLevel at its zero value would not mean "unset": it is
// logrus' panic level, so the delegate would be told to log at panic and a
// delegated shim would stay silent even with containerd in debug.
func delegateLogLevel(l bootapi.LogLevel) log.Level {
	switch {
	case l <= bootapi.LogLevel_LOG_LEVEL_TRACE:
		return log.TraceLevel
	case l <= bootapi.LogLevel_LOG_LEVEL_DEBUG:
		return log.DebugLevel
	case l <= bootapi.LogLevel_LOG_LEVEL_INFO:
		return log.InfoLevel
	case l <= bootapi.LogLevel_LOG_LEVEL_WARN:
		return log.WarnLevel
	case l <= bootapi.LogLevel_LOG_LEVEL_ERROR:
		return log.ErrorLevel
	case l <= bootapi.LogLevel_LOG_LEVEL_FATAL:
		return log.FatalLevel
	default:
		return log.PanicLevel
	}
}

// SpawnDelegate runs the delegate shim's "start" action and records where it
// ended up. This is the same handshake containerd performs against crawlc, one
// level down: exec the binary, read a BootstrapResult off stdout, keep the
// address.
func SpawnDelegate(ctx context.Context, bundlePath, runtime string, opts *bootapi.BootstrapParams) (*DelegateState, error) {
	binary, err := resolveDelegateBinary(runtime)
	if err != nil {
		return nil, err
	}

	id := delegateSocketID(opts.GetInstanceID())
	cmd, err := shim.Command(ctx, &shim.CommandConfig{ //nolint:staticcheck // the deprecation targets callers outside a shim
		ID:           id,
		RuntimePath:  binary,
		BundlePath:   bundlePath,
		GRPCAddress:  opts.GetContainerdGrpcAddress(),
		TTRPCAddress: opts.GetContainerdTtrpcAddress(),
		WorkDir:      bundlePath,
		SocketDir:    opts.GetSocketDir(),
		LogLevel:     delegateLogLevel(opts.GetLogLevel()),
		Action:       "start",
	})
	if err != nil {
		return nil, err
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("delegate %q start failed: %w: %s", runtime, err, strings.TrimSpace(string(out)))
	}

	var res bootapi.BootstrapResult
	if err := proto.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("delegate %q returned an unparseable bootstrap: %w", runtime, err)
	}
	if res.GetAddress() == "" {
		return nil, fmt.Errorf("delegate %q returned no address", runtime)
	}

	return &DelegateState{
		Runtime:      runtime,
		Binary:       binary,
		ID:           id,
		TaskID:       opts.GetInstanceID(),
		Address:      res.GetAddress(),
		Protocol:     res.GetProtocol(),
		Version:      res.GetVersion(),
		GRPCAddress:  opts.GetContainerdGrpcAddress(),
		TTRPCAddress: opts.GetContainerdTtrpcAddress(),
	}, nil
}

// shutdownDelegate asks a running delegate to exit, and is best effort by
// design: a delegate that has already died fails the dial, which is success as
// far as reaping is concerned. Errors are not returned because the delete action
// that follows is what determines whether the reap worked.
func shutdownDelegate(ctx context.Context, st *DelegateState) {
	conn, err := shim.Connect(st.Address, shim.AnonReconnectDialer)
	if err != nil {
		return
	}
	client := ttrpc.NewClient(conn)
	defer func() { _ = client.Close() }()

	_, _ = taskapi.NewTTRPCTaskClient(client).Shutdown(ctx, &taskapi.ShutdownRequest{
		ID:  st.TaskID,
		Now: true,
	})
}

// ReapDelegate stops the delegate and runs its "delete" action.
//
// Without this every crawlc container leaves a second shim behind. That matters
// more here than in an ordinary shim: crawlc exists to leak the *first* shim on
// purpose, so an accidentally leaked second one is indistinguishable from the
// condition under test.
func ReapDelegate(ctx context.Context, bundlePath string, st *DelegateState) error {
	// The delete action tears down task state but leaves a running daemon alone,
	// so a live delegate has to be told to exit over the task API first. Skipping
	// this leaks the delegate process while reporting success, which is the exact
	// shape of bug crawlc is built to expose.
	shutdownDelegate(ctx, st)

	// The container id, not the socket id. A shim's delete action uses the id to
	// find its own state — runc's derives the bundle as dirname(cwd)/id and asks
	// runc to delete a container of that name — and the delegate's task was
	// created under the container's id, because Create is forwarded verbatim.
	// Passing the suffixed socket id instead points the delete at a bundle that
	// does not exist and a container that was never created, so it reports
	// success while cleaning up nothing.
	cmd, err := shim.Command(ctx, &shim.CommandConfig{ //nolint:staticcheck // as above
		ID:           st.TaskID,
		RuntimePath:  st.Binary,
		BundlePath:   bundlePath,
		GRPCAddress:  st.GRPCAddress,
		TTRPCAddress: st.TTRPCAddress,
		WorkDir:      bundlePath,
		Action:       "delete",
	})
	if err != nil {
		return err
	}

	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("delegate %q delete failed: %w", st.Runtime, err)
	}

	// The response is only read to confirm the delegate spoke the protocol; the
	// exit status belongs to the delegate's task, not to crawlc's.
	var res taskapi.DeleteResponse
	if err := proto.Unmarshal(out, &res); err != nil {
		return fmt.Errorf("delegate %q returned an unparseable delete response: %w", st.Runtime, err)
	}
	return nil
}
