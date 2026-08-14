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

package containerd

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Environment knobs. Everything the suite needs from outside is read here, so
// that a failing run can be reproduced by exporting the same variables.
const (
	// EnvEnable opts in. The suite starts containerd daemons, runs containers
	// and leaves wedged shims behind on failure, none of which belongs in a
	// plain `go test ./...`, so it stays off unless asked for.
	EnvEnable = "CRAWLC_TEST_INTEGRATION"

	// EnvShim is the crawlc shim binary under test. Defaults to where `make
	// build` and a bake build put it, and then to PATH.
	EnvShim = "CRAWLC_TEST_SHIM"

	// EnvContainerd and EnvCtr are the containerd under test and its CLI. Both
	// default to PATH, which is how CI points the suite at a release tarball.
	EnvContainerd = "CRAWLC_TEST_CONTAINERD"
	EnvCtr        = "CRAWLC_TEST_CTR"

	// EnvRunc is the OCI runtime crawlc's task service drives on Linux. It is
	// needed by name rather than only through containerd, because cleaning up
	// after a shim that was killed rather than asked means deleting runc's state
	// directly. Defaults to PATH. Linux-only; ignored on other platforms.
	EnvRunc = "CRAWLC_TEST_RUNC"

	// EnvImage is the image containers are created from. It needs a shell and a
	// sleep, and is pulled once per test daemon.
	EnvImage = "CRAWLC_TEST_IMAGE"

	// EnvSnapshotter overrides containerd's default snapshotter. Worth setting
	// to "native" where the test root sits on overlayfs already, since overlay
	// cannot stack on itself and the default snapshotter fails to mount.
	EnvSnapshotter = "CRAWLC_TEST_SNAPSHOTTER"

	// EnvBoundedShimLoad asserts that the containerd under test bounds shim
	// loading with io.containerd.timeout.shim.load. Tests that need it skip
	// without it, because on a containerd that lacks the fix they do not fail —
	// they hang, which is the bug.
	//
	// It is a flag rather than a version check because at the time of writing
	// the fix is not in a release yet. Once it is, this can become a comparison
	// against `containerd --version`.
	EnvBoundedShimLoad = "CRAWLC_TEST_BOUNDED_SHIM_LOAD"
)

const (
	// shimBinaryName is what containerd execs for RuntimeName: the handler's
	// last two dot-separated segments, prefixed with containerd-shim-. The
	// binary has to be found under exactly this name on the daemon's PATH.
	shimBinaryName = "containerd-shim-crawlc-v1"

	// RuntimeName is crawlc's runtime handler, as passed to `ctr run --runtime`.
	RuntimeName = "io.containerd.crawlc.v1"

	// testNamespace is the namespace every container is created in. It is also
	// the directory name under the state dir that bundles land in.
	testNamespace = "default"

	defaultImage = "docker.io/library/busybox:latest"
)

// Resolved once by TestMain, read-only afterwards.
var (
	containerdBin string
	ctrBin        string
	runcBin       string
	shimBin       string
	testImage     string
	snapshotter   string
)

func TestMain(m *testing.M) {
	flag.Parse()

	if enabled() {
		if err := setup(); err != nil {
			// Opting in and being unable to run is a failure rather than a skip:
			// a suite that quietly does nothing in CI is worse than no suite.
			fmt.Fprintf(os.Stderr, "%s is set but the integration suite cannot run: %v\n", EnvEnable, err)
			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}

func enabled() bool { return os.Getenv(EnvEnable) != "" }

func setup() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root: containerd and the shim both need it")
	}

	var err error
	if containerdBin, err = lookBinary(EnvContainerd, "containerd"); err != nil {
		return err
	}
	if ctrBin, err = lookBinary(EnvCtr, "ctr"); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		// runc is needed on Linux to clean up container state after a shim is
		// killed. On other platforms the hollow task service runs no containers,
		// so there is no runc state to clean up.
		if runcBin, err = lookBinary(EnvRunc, "runc"); err != nil {
			return err
		}
	}
	if shimBin, err = lookShim(); err != nil {
		return err
	}

	testImage = os.Getenv(EnvImage)
	if testImage == "" && runtime.GOOS == "linux" {
		testImage = defaultImage
	}
	snapshotter = os.Getenv(EnvSnapshotter)

	return nil
}

// lookBinary resolves a binary from env, falling back to PATH.
func lookBinary(env, name string) (string, error) {
	if p := os.Getenv(env); p != "" {
		if err := checkExecutable(p); err != nil {
			return "", fmt.Errorf("%s=%q: %w", env, p, err)
		}
		return p, nil
	}

	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH, and %s is unset: %w", name, env, err)
	}
	return p, nil
}

// lookShim resolves the crawlc shim, preferring a freshly built one over
// whatever happens to be installed.
//
// The name is checked rather than trusted: containerd derives the binary it execs
// from the runtime handler, so a shim under any other name is one containerd will
// never find, and a test that starts by hanging is worth turning into an error
// here.
func lookShim() (string, error) {
	// An explicit path is authoritative, the same way it is in lookBinary. Falling
	// back from a path the caller named would test a different binary than the one
	// they asked for and say nothing about it — which in CI, where the whole point
	// is to drive the released artifact, would be a green run on the wrong shim.
	if p := os.Getenv(EnvShim); p != "" {
		if err := checkExecutable(p); err != nil {
			return "", fmt.Errorf("%s=%q: %w", EnvShim, p, err)
		}
		if filepath.Base(p) != shimBinaryName {
			return "", fmt.Errorf("%s=%q must be named %s for containerd to find it", EnvShim, p, shimBinaryName)
		}
		return p, nil
	}

	// ../bin is where `make build` puts it; ../bin/linux_<arch> is where a bake
	// build does, since a local export covering several platforms gets a
	// directory each. Every candidate below already carries the right base name.
	var candidates []string
	for _, rel := range []string{
		filepath.Join("..", "bin", shimBinaryName),
		filepath.Join("..", "bin", runtime.GOOS+"_"+runtime.GOARCH, shimBinaryName),
	} {
		if abs, err := filepath.Abs(rel); err == nil {
			candidates = append(candidates, abs)
		}
	}
	if p, err := exec.LookPath(shimBinaryName); err == nil {
		candidates = append(candidates, p)
	}

	var firstErr error
	for _, p := range candidates {
		if err := checkExecutable(p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return p, nil
	}

	if firstErr != nil {
		return "", fmt.Errorf("no usable %s: %w", shimBinaryName, firstErr)
	}
	return "", fmt.Errorf("no %s found: build one with `make build` or set %s", shimBinaryName, EnvShim)
}

func checkExecutable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%q is not an executable file", path)
	}
	return nil
}

// requireIntegration skips a test that needs a containerd of its own.
func requireIntegration(t *testing.T) {
	t.Helper()
	if !enabled() {
		t.Skipf("set %s=1 to run the integration tests", EnvEnable)
	}
}

// requireBoundedShimLoad skips a test that would hang rather than fail on a
// containerd without a bounded shim load. See EnvBoundedShimLoad.
func requireBoundedShimLoad(t *testing.T) {
	t.Helper()
	if os.Getenv(EnvBoundedShimLoad) == "" {
		t.Skipf("set %s=1 when the containerd under test bounds shim loading with %s",
			EnvBoundedShimLoad, "io.containerd.timeout.shim.load")
	}
}
