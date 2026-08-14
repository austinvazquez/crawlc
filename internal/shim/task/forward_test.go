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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"google.golang.org/protobuf/types/known/emptypb"
)

// fakeDelegate stands in for the ttrpc client to a delegate shim. Only the
// methods the overrides touch are implemented; the embedded nil interface makes
// any other call panic, which is the desired outcome for a test that reaches
// somewhere it should not.
type fakeDelegate struct {
	taskapi.TTRPCTaskService

	connectResp *taskapi.ConnectResponse
	connectErr  error
	shutdownErr error
	shutdowns   int
}

func (f *fakeDelegate) Connect(context.Context, *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	if f.connectErr != nil {
		return nil, f.connectErr
	}
	return f.connectResp, nil
}

func (f *fakeDelegate) Shutdown(context.Context, *taskapi.ShutdownRequest) (*emptypb.Empty, error) {
	f.shutdowns++
	return &emptypb.Empty{}, f.shutdownErr
}

type fakeShutdown struct{ calls int }

func (f *fakeShutdown) Shutdown()                                    { f.calls++ }
func (f *fakeShutdown) RegisterCallback(func(context.Context) error) {}
func (f *fakeShutdown) Done() <-chan struct{}                        { return nil }
func (f *fakeShutdown) Err() error                                   { return nil }

type fakeCloser struct{ closed int }

func (f *fakeCloser) Close() error { f.closed++; return nil }

// TestForwardConnectReportsOwnShimPid covers the one substitution containerd
// depends on: it supervises the shim by the pid Connect reports, and that has to
// be crawlc rather than the delegate crawlc launched.
func TestForwardConnectReportsOwnShimPid(t *testing.T) {
	const delegatePid = 424242
	f := &fakeDelegate{connectResp: &taskapi.ConnectResponse{
		ShimPid: delegatePid,
		TaskPid: 99,
		Version: "delegate",
	}}
	s := &forwardingTaskService{TTRPCTaskService: f, conn: &fakeCloser{}, sd: &fakeShutdown{}}

	res, err := s.Connect(context.Background(), &taskapi.ConnectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ShimPid == delegatePid {
		t.Error("ShimPid was passed through; containerd would supervise the delegate")
	}
	if res.ShimPid != selfPid() {
		t.Errorf("ShimPid = %d, want crawlc's own %d", res.ShimPid, selfPid())
	}
	// The task pid genuinely belongs to the delegate's container.
	if res.TaskPid != 99 {
		t.Errorf("TaskPid = %d, want 99 untouched", res.TaskPid)
	}
}

func TestForwardConnectPropagatesError(t *testing.T) {
	want := errors.New("delegate is down")
	s := &forwardingTaskService{
		TTRPCTaskService: &fakeDelegate{connectErr: want},
		conn:             &fakeCloser{},
		sd:               &fakeShutdown{},
	}
	if _, err := s.Connect(context.Background(), &taskapi.ConnectRequest{}); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// TestForwardShutdownStopsBoth is the leak guard: forwarding Shutdown without
// stopping crawlc would leave it running with nothing behind it.
func TestForwardShutdownStopsBoth(t *testing.T) {
	f := &fakeDelegate{}
	closer := &fakeCloser{}
	sd := &fakeShutdown{}
	s := &forwardingTaskService{TTRPCTaskService: f, conn: closer, sd: sd}

	if _, err := s.Shutdown(context.Background(), &taskapi.ShutdownRequest{}); err != nil {
		t.Fatal(err)
	}
	if f.shutdowns != 1 {
		t.Errorf("delegate shutdowns = %d, want 1", f.shutdowns)
	}
	if sd.calls != 1 {
		t.Errorf("crawlc shutdowns = %d, want 1; crawlc would be left running", sd.calls)
	}
	if closer.closed != 1 {
		t.Errorf("connection closes = %d, want 1", closer.closed)
	}
}

// A delegate that fails to stop must still not strand crawlc, or a fixture that
// leaks the first shim on purpose starts leaking the second one too.
func TestForwardShutdownStopsCrawlcEvenIfDelegateFails(t *testing.T) {
	want := errors.New("delegate refused")
	sd := &fakeShutdown{}
	s := &forwardingTaskService{
		TTRPCTaskService: &fakeDelegate{shutdownErr: want},
		conn:             &fakeCloser{},
		sd:               sd,
	}

	if _, err := s.Shutdown(context.Background(), &taskapi.ShutdownRequest{}); !errors.Is(err, want) {
		t.Fatalf("error not reported: got %v, want %v", err, want)
	}
	if sd.calls != 1 {
		t.Error("crawlc was left running after a failed delegate shutdown")
	}
}

func TestNewForwardingTaskServiceRejectsNonTTRPC(t *testing.T) {
	_, err := newForwardingTaskService(&DelegateState{
		Runtime:  "io.containerd.example.v1",
		Protocol: "grpc",
		Address:  "vsock://3:1024",
	}, &fakeShutdown{})
	if err == nil {
		t.Fatal("expected a grpc delegate to be rejected rather than mis-dialed")
	}
}

// TestTaskServiceForUsesDelegateWhenConfigured checks the branch selection.
//
// It asserts on the dial failing rather than on a working delegate: reaching the
// dial at all is the proof that state in the bundle routed the service to
// forwarding. The fallback branch is deliberately not exercised here, because on
// Linux it constructs the real runc task service, which needs a live publisher
// and belongs in the live test rather than a unit one.
func TestTaskServiceForUsesDelegateWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	const runtime = "io.containerd.example.v1"
	if err := WriteDelegateState(dir, &DelegateState{
		Runtime:  runtime,
		Protocol: "ttrpc",
		Address:  "unix://" + filepath.Join(dir, "absent.sock"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = taskServiceFor(context.Background(), nil, &fakeShutdown{})
	if err == nil {
		t.Fatal("expected the dial to an absent delegate socket to fail")
	}
	if !strings.Contains(err.Error(), runtime) {
		t.Fatalf("error does not name the delegate, so the forwarding branch may not have run: %v", err)
	}
}
