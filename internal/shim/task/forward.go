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
	"fmt"
	"io"
	"os"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// forwardingTaskService passes every task call to a delegate shim.
//
// The generated ttrpc client already implements the task service, so embedding
// it forwards all eighteen methods without writing any of them. Only the two
// that carry crawlc's own identity, rather than the container's, need an
// opinion; they are overridden below.
type forwardingTaskService struct {
	taskapi.TTRPCTaskService

	conn io.Closer
	sd   shutdown.Service
}

var _ taskapi.TTRPCTaskService = (*forwardingTaskService)(nil)

// taskAPIVersion is the dialect of the task API the embedded client speaks. It
// is part of the ttrpc service name, so it has to match the delegate's.
const taskAPIVersion = 3

// newForwardingTaskService dials a delegate that has already been started.
func newForwardingTaskService(st *DelegateState, sd shutdown.Service) (*forwardingTaskService, error) {
	// crawlc only speaks the ttrpc dialect of the task API. A grpc delegate would
	// need a different client, and silently mis-dialing one would surface much
	// later as an unreadable response.
	if p := st.Protocol; p != "" && p != "ttrpc" {
		return nil, fmt.Errorf("delegate %q speaks %q, only ttrpc is supported", st.Runtime, p)
	}

	// The version is part of the ttrpc service name, so an older delegate accepts
	// the connection and then fails every call with an unknown-service error. That
	// is the same "surfaces much later as an unreadable response" failure the
	// protocol check above avoids, and it deserves the same refusal up front.
	if v := st.Version; v != 0 && v != taskAPIVersion {
		return nil, fmt.Errorf("delegate %q speaks task API v%d, only v%d is supported", st.Runtime, v, taskAPIVersion)
	}

	// shim.Connect applies its own fixed dial timeout and ignores the context, so
	// a delegate that never accepts stalls here for as long as that allows.
	conn, err := shim.Connect(st.Address, shim.AnonReconnectDialer)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to delegate %q at %s: %w", st.Runtime, st.Address, err)
	}

	client := ttrpc.NewClient(conn)
	return &forwardingTaskService{
		TTRPCTaskService: taskapi.NewTTRPCTaskClient(client),
		conn:             client,
		sd:               sd,
	}, nil
}

// Connect reports crawlc's pid as the shim pid.
//
// containerd tracks and signals a shim by the pid this returns, and the process
// it started is crawlc, not the delegate. Passing the delegate's pid through
// would point containerd's supervision at a process it never launched, so a kill
// aimed at the shim would miss crawlc entirely. The task pid is left alone: that
// one really does belong to the delegate's container.
func (s *forwardingTaskService) Connect(ctx context.Context, r *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	res, err := s.TTRPCTaskService.Connect(ctx, r)
	if err != nil {
		return nil, err
	}
	res.ShimPid = selfPid()
	return res, nil
}

// Shutdown stops the delegate and then crawlc itself.
//
// Forwarding alone would leave crawlc running with nothing behind it, which is a
// leaked shim of exactly the kind this fixture is meant to produce deliberately
// rather than by accident. The delegate goes first so that a failure to stop it
// is still reported while crawlc is alive to report it.
func (s *forwardingTaskService) Shutdown(ctx context.Context, r *taskapi.ShutdownRequest) (*emptypb.Empty, error) {
	res, err := s.TTRPCTaskService.Shutdown(ctx, r)
	_ = s.conn.Close()
	s.sd.Shutdown()
	return res, err
}

// taskServiceFor picks what sits behind crawlc's delay: a delegate if one was
// started for this bundle, otherwise the platform's own service.
//
// The delegate is discovered from the bundle rather than passed in, because the
// process that started it and the daemon that has to talk to it are different
// processes.
func taskServiceFor(ctx context.Context, pub shim.Publisher, sd shutdown.Service) (taskapi.TTRPCTaskService, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	st, err := ReadDelegateState(cwd)
	if err != nil {
		return nil, err
	}
	if st != nil {
		return newForwardingTaskService(st, sd)
	}
	return newLocalService(ctx, pub, sd)
}
