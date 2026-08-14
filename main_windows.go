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
	"fmt"
	"os"
)

// Windows is not supported, and the obstacle is upstream rather than here.
//
// containerd's pkg/shim cannot serve a shim on Windows at all: serveListener,
// reap and openLog in its shim_windows.go all return ErrNotImplemented. Windows
// shims are named-pipe based and hcsshim's runhcs supplies its own serving loop
// instead of using containerd's, so crawlc would have to grow one too. That is a
// port of the shim framework, not a build tag, so it is left undone rather than
// half-done.
//
// The rest of crawlc is already portable: the delay layer has no platform
// dependencies, and the hollow task service that stands in for runc off Linux
// would serve Windows unchanged once there is something to serve it over.
func main() {
	fmt.Fprintln(os.Stderr, "crawlc: windows is unsupported, containerd's pkg/shim cannot serve a shim there")
	os.Exit(1)
}
