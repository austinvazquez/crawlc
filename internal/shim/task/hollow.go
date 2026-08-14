//go:build !linux

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
	"sync"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/errdefs"
	"google.golang.org/protobuf/types/known/emptypb"
)

// hollowTaskService is a task service that tracks task state without ever
// running a container.
//
// It exists so that crawlc is useful on platforms where there is no runtime to
// wrap: runc is Linux-only, and containerd ships no importable task service for
// anything else. What crawlc tests does not need a real container — containerd's
// shim lifecycle only requires a shim that connects, answers, and can be reaped,
// so the workload is the one part that can be dropped without weakening the test.
//
// On Linux the real runc service is used instead, so the fidelity is there when
// the platform can provide it.
type hollowTaskService struct {
	sd shutdown.Service

	mu      sync.Mutex
	id      string
	deleted bool // Delete has been called and returned
	status  tasktypes.Status
	pid     uint32

	exitCode uint32
	exitedAt time.Time
	exited   chan struct{}
}

var _ taskapi.TTRPCTaskService = (*hollowTaskService)(nil)

func newHollowTaskService(sd shutdown.Service) *hollowTaskService {
	return &hollowTaskService{
		sd:     sd,
		status: tasktypes.Status_UNKNOWN,
		exited: make(chan struct{}),
	}
}

func (s *hollowTaskService) Create(_ context.Context, r *taskapi.CreateTaskRequest) (*taskapi.CreateTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.id = r.GetID()
	s.status = tasktypes.Status_CREATED
	s.pid = selfPid()
	return &taskapi.CreateTaskResponse{Pid: s.pid}, nil
}

func (s *hollowTaskService) Start(_ context.Context, _ *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.status = tasktypes.Status_RUNNING
	return &taskapi.StartResponse{Pid: s.pid}, nil
}

func (s *hollowTaskService) State(_ context.Context, _ *taskapi.StateRequest) (*taskapi.StateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return &taskapi.StateResponse{
		ID:         s.id,
		Pid:        s.pid,
		Status:     s.status,
		ExitStatus: s.exitCode,
		ExitedAt:   protobuf.ToTimestamp(s.exitedAt),
	}, nil
}

func (s *hollowTaskService) Delete(_ context.Context, _ *taskapi.DeleteRequest) (*taskapi.DeleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.markExitedLocked()
	s.deleted = true
	return &taskapi.DeleteResponse{
		Pid:        s.pid,
		ExitStatus: s.exitCode,
		ExitedAt:   protobuf.ToTimestamp(s.exitedAt),
	}, nil
}

func (s *hollowTaskService) Pids(_ context.Context, _ *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// A stopped task reports no processes, which is the signal containerd uses to
	// decide a loaded shim is worth reaping. Reporting one while running keeps the
	// shim registered instead, so both sides of that branch are reachable.
	if s.status != tasktypes.Status_RUNNING && s.status != tasktypes.Status_CREATED {
		return &taskapi.PidsResponse{}, nil
	}
	return &taskapi.PidsResponse{
		Processes: []*tasktypes.ProcessInfo{{Pid: s.pid}},
	}, nil
}

func (s *hollowTaskService) Kill(_ context.Context, _ *taskapi.KillRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.markExitedLocked()
	return &emptypb.Empty{}, nil
}

func (s *hollowTaskService) Wait(ctx context.Context, _ *taskapi.WaitRequest) (*taskapi.WaitResponse, error) {
	select {
	case <-s.exited:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return &taskapi.WaitResponse{
		ExitStatus: s.exitCode,
		ExitedAt:   protobuf.ToTimestamp(s.exitedAt),
	}, nil
}

func (s *hollowTaskService) Connect(_ context.Context, _ *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return &taskapi.ConnectResponse{
		ShimPid: selfPid(),
		TaskPid: s.pid,
		Version: "crawlc",
	}, nil
}

func (s *hollowTaskService) Shutdown(_ context.Context, r *taskapi.ShutdownRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	canShutdown := s.id == "" || s.deleted
	s.mu.Unlock()

	// Mirror the runc task service: if now=false and we still hold a task that
	// has not been deleted, defer shutdown. containerd removes the bundle and
	// moves on; the shim stays running until killed externally.
	if !r.GetNow() && !canShutdown {
		return &emptypb.Empty{}, nil
	}
	s.sd.Shutdown()
	return &emptypb.Empty{}, nil
}

// markExitedLocked records the exit once, so that repeated Delete or Kill calls
// keep reporting the same status rather than moving the exit time under a client
// that is retrying.
func (s *hollowTaskService) markExitedLocked() {
	if s.status == tasktypes.Status_STOPPED {
		return
	}
	s.status = tasktypes.Status_STOPPED
	s.exitedAt = time.Now()
	select {
	case <-s.exited:
	default:
		close(s.exited)
	}
}

// The remainder have no meaning without a real container. They report
// ErrNotImplemented rather than a bare success so that a test relying on one
// fails loudly instead of quietly believing it happened.

func (s *hollowTaskService) Pause(context.Context, *taskapi.PauseRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *hollowTaskService) Resume(context.Context, *taskapi.ResumeRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *hollowTaskService) Checkpoint(context.Context, *taskapi.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *hollowTaskService) Exec(context.Context, *taskapi.ExecProcessRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *hollowTaskService) ResizePty(context.Context, *taskapi.ResizePtyRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *hollowTaskService) CloseIO(context.Context, *taskapi.CloseIORequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *hollowTaskService) Update(context.Context, *taskapi.UpdateTaskRequest) (*emptypb.Empty, error) {
	return nil, errdefs.ErrNotImplemented
}

func (s *hollowTaskService) Stats(context.Context, *taskapi.StatsRequest) (*taskapi.StatsResponse, error) {
	return nil, errdefs.ErrNotImplemented
}
