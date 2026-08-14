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

import "os"

// selfPid returns the PID of this shim process.
//
// It is what containerd is told to supervise, and what a liveness probe should
// find alive, so both the hollow service and the forwarding one report it rather
// than any pid belonging to a delegate.
func selfPid() uint32 { return uint32(os.Getpid()) }
