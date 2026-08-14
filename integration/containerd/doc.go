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
// containerd daemon and root, so the whole suite is opt-in and skips unless
// CRAWLC_TEST_INTEGRATION is set. See the README.
//
// On Linux the tests run full containers via runc. On macOS and other non-Linux
// platforms the hollow task service is used instead: crawlc responds to every
// task API call without launching an OCI runtime, which is enough to drive
// containerd's shim lifecycle without needing a container-capable host.
package containerd
