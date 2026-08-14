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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseBehavior(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		wantDelays  map[string]time.Duration
		wantArmFile string
		wantErr     bool
	}{
		{
			name:       "no annotations",
			wantDelays: map[string]time.Duration{},
		},
		{
			name:        "unrelated annotations are ignored",
			annotations: map[string]string{"io.kubernetes.cri.sandbox-id": "abc"},
			wantDelays:  map[string]time.Duration{},
		},
		{
			name:        "per method duration",
			annotations: map[string]string{DelayPrefix + "Pids": "30s"},
			wantDelays:  map[string]time.Duration{"Pids": 30 * time.Second},
		},
		{
			name:        "forever",
			annotations: map[string]string{DelayPrefix + "Connect": ForeverValue},
			wantDelays:  map[string]time.Duration{"Connect": Forever},
		},
		{
			name:        "wildcard",
			annotations: map[string]string{DelayPrefix + WildcardMethod: "1s"},
			wantDelays:  map[string]time.Duration{WildcardMethod: time.Second},
		},
		{
			name:        "arm file",
			annotations: map[string]string{ArmFileKey: "crawlc.arm"},
			wantDelays:  map[string]time.Duration{},
			wantArmFile: "crawlc.arm",
		},
		{
			name:        "zero delay is valid",
			annotations: map[string]string{DelayPrefix + "Delete": "0s"},
			wantDelays:  map[string]time.Duration{"Delete": 0},
		},
		{
			name:        "empty method name",
			annotations: map[string]string{DelayPrefix: "1s"},
			wantErr:     true,
		},
		{
			name:        "unparseable duration",
			annotations: map[string]string{DelayPrefix + "Pids": "soon"},
			wantErr:     true,
		},
		{
			name:        "negative duration",
			annotations: map[string]string{DelayPrefix + "Pids": "-1s"},
			wantErr:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseBehavior(tc.annotations)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got behavior %+v", b)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(b.delays) != len(tc.wantDelays) {
				t.Fatalf("delays = %v, want %v", b.delays, tc.wantDelays)
			}
			for method, want := range tc.wantDelays {
				if got := b.delays[method]; got != want {
					t.Errorf("delay for %q = %v, want %v", method, got, want)
				}
			}
			if b.armFile != tc.wantArmFile {
				t.Errorf("armFile = %q, want %q", b.armFile, tc.wantArmFile)
			}
		})
	}
}

func TestDelayForFallsBackToWildcard(t *testing.T) {
	b, err := parseBehavior(map[string]string{
		DelayPrefix + WildcardMethod: "1s",
		DelayPrefix + "Pids":         "5s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// An explicit entry wins over the wildcard, so a test can wedge one method
	// while leaving the rest merely slow.
	if d, ok := b.delayFor("Pids"); !ok || d != 5*time.Second {
		t.Errorf("delayFor(Pids) = %v, %v; want 5s, true", d, ok)
	}
	if d, ok := b.delayFor("Connect"); !ok || d != time.Second {
		t.Errorf("delayFor(Connect) = %v, %v; want 1s, true", d, ok)
	}
}

func TestDelayForUnconfigured(t *testing.T) {
	b, err := parseBehavior(map[string]string{DelayPrefix + "Pids": "5s"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d, ok := b.delayFor("Connect"); ok {
		t.Errorf("delayFor(Connect) = %v, true; want not configured", d)
	}
}

func TestArmed(t *testing.T) {
	dir := t.TempDir()
	armFile := filepath.Join(dir, "crawlc.arm")

	b, err := parseBehavior(map[string]string{ArmFileKey: armFile})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if b.armed() {
		t.Fatal("armed before the file exists")
	}
	if err := os.WriteFile(armFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !b.armed() {
		t.Fatal("not armed after the file was created")
	}
}

func TestArmedWithoutArmFile(t *testing.T) {
	b, err := parseBehavior(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !b.armed() {
		t.Fatal("an unconfigured arm file should leave delays armed")
	}
}

func TestWaitSkipsUnconfiguredAndUnarmed(t *testing.T) {
	// Both cases must be free rather than merely fast: wait sits in front of every
	// task API call, including those of an unconfigured crawlc.
	for _, tc := range []struct {
		name        string
		annotations map[string]string
	}{
		{name: "unconfigured method", annotations: map[string]string{DelayPrefix + "Pids": ForeverValue}},
		{name: "not yet armed", annotations: map[string]string{
			DelayPrefix + "Connect": ForeverValue,
			ArmFileKey:              filepath.Join(t.TempDir(), "absent"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseBehavior(tc.annotations)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				b.wait("Connect")
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("wait blocked when it should not have")
			}
		})
	}
}

func TestWaitBlocksForever(t *testing.T) {
	b, err := parseBehavior(map[string]string{DelayPrefix + "Connect": ForeverValue})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The goroutine is deliberately leaked: a forever delay is never meant to
	// return, which is the whole point of the sentinel.
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.wait("Connect")
	}()

	select {
	case <-done:
		t.Fatal("wait returned for a forever delay")
	case <-time.After(200 * time.Millisecond):
	}
}
