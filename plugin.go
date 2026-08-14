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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.EventPlugin,
			plugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			pp, err := ic.GetByID(plugins.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			ss, err := ic.GetByID(plugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			b, err := behaviorFromBundle()
			if err != nil {
				return nil, err
			}
			local, err := taskServiceFor(ic.Context, pp.(shim.Publisher), ss.(shutdown.Service))
			if err != nil {
				return nil, err
			}

			return &slowTaskService{behavior: b, local: local}, nil
		},
	})
}

var _ = shim.TTRPCServerUnaryOptioner(&slowTaskService{})

// slowTaskService is whatever task service this platform could provide, with a
// delay in front of it. Delegating rather than reimplementing keeps crawlc a
// faithful shim in every respect except timing, so a test that trips over it is
// exercising containerd's handling of a slow shim and not of a fake one.
type slowTaskService struct {
	behavior *behavior
	local    taskapi.TTRPCTaskService
}

func (s *slowTaskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskapi.RegisterTTRPCTaskService(server, s.local)
	return nil
}

// UnaryServerInterceptor stalls a call before handing it to the real service.
//
// ttrpc dispatches every unary call on its own goroutine, so blocking here wedges
// only the method that was configured: the socket stays accepted and other
// methods keep answering. That asymmetry is the interesting case, because it is
// what a shim looks like when it is alive enough to connect to but not to serve.
func (s *slowTaskService) UnaryServerInterceptor() ttrpc.UnaryServerInterceptor {
	return func(ctx context.Context, unmarshal ttrpc.Unmarshaler, info *ttrpc.UnaryServerInfo, method ttrpc.Method) (any, error) {
		s.behavior.wait(filepath.Base(info.FullMethod))
		return method(ctx, unmarshal)
	}
}

// bundleAnnotations reads the OCI annotations from the bundle crawlc is running
// in, which is the shim's working directory.
//
// A missing spec is not an error: containerd invokes the shim binary for actions
// that run outside a bundle, and an unconfigured crawlc should behave like the
// ordinary shim it wraps rather than refuse to start.
func bundleAnnotations() (map[string]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get current working dir: %w", err)
	}

	spec, err := oci.ReadSpec(filepath.Join(cwd, oci.ConfigFilename))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return spec.Annotations, nil
}

func behaviorFromBundle() (*behavior, error) {
	a, err := bundleAnnotations()
	if err != nil {
		return nil, err
	}
	return parseBehavior(a)
}
