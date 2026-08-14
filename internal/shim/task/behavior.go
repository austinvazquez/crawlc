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

// Package task implements the crawlc task service: a configurable delay layer
// in front of a real containerd task service, plus optional delegation to a
// second shim binary.
package task

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// AnnotationPrefix namespaces every crawlc knob in the OCI spec.
	AnnotationPrefix = "io.containerd.crawlc."

	// DelayPrefix is followed by a task API method name, or WildcardMethod:
	//
	//	io.containerd.crawlc.delay.Pids = 30s
	//	io.containerd.crawlc.delay.*    = forever
	DelayPrefix = AnnotationPrefix + "delay."

	// ArmFileKey holds a path that must exist before any delay applies. Paths are
	// resolved against the shim's working directory, which is the bundle.
	ArmFileKey = AnnotationPrefix + "arm.file"

	// WildcardMethod is the delay applied to methods with no entry of their own.
	WildcardMethod = "*"

	// ForeverValue asks for a delay that never elapses.
	ForeverValue = "forever"
)

// Forever marks a delay that is never meant to elapse. It is a sentinel rather
// than a large duration so that wait can block outright: a timer long enough to
// outlive the test would still be a timer, and the point is to reproduce a shim
// that is never going to answer.
const Forever = time.Duration(-1)

// behavior is the misbehavior crawlc was asked to exhibit, parsed once at
// startup. It is read-only afterwards, so the ttrpc handler goroutines can share
// it without locking.
type behavior struct {
	delays  map[string]time.Duration
	armFile string
}

// parseBehavior reads crawlc's knobs out of OCI spec annotations.
//
// Annotations are the only viable channel here: the shim survives the containerd
// that started it, and the scenarios worth testing all happen on a later
// containerd's load path, long after any flags or environment from the original
// invocation are out of reach. The bundle, and so its config.json, is what is
// still on disk.
func parseBehavior(annotations map[string]string) (*behavior, error) {
	b := &behavior{delays: map[string]time.Duration{}}

	for k, v := range annotations {
		switch {
		case k == ArmFileKey:
			b.armFile = v
		case strings.HasPrefix(k, DelayPrefix):
			method := strings.TrimPrefix(k, DelayPrefix)
			if method == "" {
				return nil, fmt.Errorf("annotation %q names no method", k)
			}
			d, err := parseDelay(v)
			if err != nil {
				return nil, fmt.Errorf("annotation %q: %w", k, err)
			}
			b.delays[method] = d
		}
	}

	return b, nil
}

func parseDelay(v string) (time.Duration, error) {
	if v == ForeverValue {
		return Forever, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("expected a duration or %q, got %q", ForeverValue, v)
	}
	if d < 0 {
		return 0, fmt.Errorf("negative delay %q", v)
	}
	return d, nil
}

// delayFor reports the delay configured for method, falling back to the wildcard.
func (b *behavior) delayFor(method string) (time.Duration, bool) {
	if d, ok := b.delays[method]; ok {
		return d, true
	}
	d, ok := b.delays[WildcardMethod]
	return d, ok
}

// armed reports whether delays should apply yet.
//
// Without this, a shim configured to wedge would wedge during its own creation
// and never reach the state being tested. Gating on a file rather than elapsed
// time keeps that handoff race-free: the test creates the container, touches the
// file, and only then restarts containerd.
func (b *behavior) armed() bool {
	if b.armFile == "" {
		return true
	}
	_, err := os.Stat(b.armFile)
	return err == nil
}

// wait applies the configured delay for method before the real handler runs.
//
// The caller's context is deliberately ignored. A wedged shim does not notice
// that its client gave up, and honouring cancellation here would let containerd
// unblock the very call it is supposed to time out on.
func (b *behavior) wait(method string) {
	d, ok := b.delayFor(method)
	if !ok || !b.armed() {
		return
	}
	if d == Forever {
		<-make(chan struct{})
	}
	time.Sleep(d)
}
