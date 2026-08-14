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

// Package containerd drives a real containerd against a real crawlc shim.
//
// The tests here are the other half of crawlc's unit tests: those check that the
// annotations parse and that a delegate is spawned and reaped, this checks what
// containerd does when the shim in front of it stops answering. That needs a
// containerd daemon, a container runtime and root, so the whole suite is opt-in
// and skips unless CRAWLC_TEST_INTEGRATION is set. See the README.
//
// This file carries no build constraint on purpose: the tests are Linux-only, and
// without an unconstrained file the package would have no files at all elsewhere,
// which `go vet ./...` reports as an error on every other platform crawlc builds
// for.
package containerd
