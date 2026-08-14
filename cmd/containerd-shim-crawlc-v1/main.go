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

	"github.com/containerd/containerd/v2/pkg/shim"

	"github.com/austinvazquez/crawlc/pkg/shim/manager"
)

// RuntimeName is the runtime handler crawlc registers as. containerd derives the
// binary it execs from the last two dot-separated segments, so this name and the
// containerd-shim-crawlc-v1 binary have to move together.
const RuntimeName = "io.containerd.crawlc.v1"

func main() {
	shim.RunShim(context.Background(), manager.New(RuntimeName))
}
