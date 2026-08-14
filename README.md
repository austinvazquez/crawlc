# crawlc

A containerd shim that is slow on purpose.

`runc` runs a container; crawlc crawls. It is the real
[runc task service](https://github.com/containerd/containerd/tree/main/cmd/containerd-shim-runc-v2)
with a configurable delay in front of every task API call, so containers behave
normally except for the timing you asked to break.

It exists to test the containerd side of a shim going bad: the timeouts, the
cleanup paths, and the code that has to decide whether an unresponsive shim is
dead or just slow.

> crawlc is a test fixture. It deliberately leaves wedged processes behind. Do
> not install it on a machine you care about.

## Platforms

| Platform | Default task service | Runs real containers | Delegation |
|---|---|---|---|
| linux, freebsd | runc on linux, hollow on freebsd | linux only | yes |
| darwin | hollow | no, unless delegated | yes |
| windows | — | unsupported | no |

The delay layer is platform-independent. What is not portable is the runtime
underneath it: runc's task service is `//go:build linux`, and containerd exports
no equivalent elsewhere. Off Linux crawlc serves a *hollow* task service that
tracks task state without ever starting a process — enough to connect to, answer,
and be reaped, which is all containerd's shim lifecycle actually exercises.

When a hollow shim is not enough, point crawlc at another shim instead; see
[Delegating to another shim](#delegating-to-another-shim). That is how macOS gets
real containers, by putting [nerdbox](https://github.com/containerd/nerdbox)
behind crawlc.

Windows is blocked upstream rather than here. containerd's `pkg/shim` cannot
serve a shim on Windows at all: `serveListener`, `reap` and `openLog` in its
`shim_windows.go` all return `ErrNotImplemented`, because Windows shims are
named-pipe based and hcsshim's runhcs brings its own serving loop. Supporting it
means porting the shim framework, so crawlc compiles to a stub there instead.

## Configuring the misbehavior

crawlc is driven by OCI annotations, read once from the bundle's `config.json`.

Annotations rather than flags or environment because the interesting scenarios
outlive the containerd that started the shim. When a later containerd loads a
leftover bundle, the bundle on disk is the only configuration still reachable.

| Annotation | Value | Effect |
|---|---|---|
| `io.containerd.crawlc.delay.<Method>` | duration, or `forever` | Stall `<Method>` before the real handler runs |
| `io.containerd.crawlc.delay.*` | duration, or `forever` | Default for methods with no entry of their own |
| `io.containerd.crawlc.arm.file` | path | Apply no delay until this file exists |
| `io.containerd.crawlc.delegate` | runtime name, or a path | Forward every call to another shim |

`<Method>` is a task API method name as containerd calls it — `Connect`, `Pids`,
`Create`, `Start`, `State`, `Delete`, `Shutdown`. An explicit entry beats the
wildcard, so you can wedge one call and leave the rest merely slow.

Two details worth knowing:

- **`forever` blocks, it does not sleep.** Nothing short of `SIGKILL` gets the
  call moving again, and the delay ignores the caller's context on purpose. A
  genuinely wedged shim does not notice that its client gave up, and honouring
  cancellation would let containerd unblock the call it is supposed to time out
  on.
- **Only the configured method wedges.** ttrpc dispatches each unary call on its
  own goroutine, so the socket stays accepted and every other method keeps
  answering. That asymmetry is the realistic case: a shim alive enough to connect
  to, but not to serve.

### Arming

A shim told to wedge immediately wedges during its own creation and never reaches
the state you wanted to test. `arm.file` defers that: the delay applies only once
the file exists. Paths are relative to the shim's working directory, which is the
bundle.

Gating on a file rather than elapsed time keeps the handoff race-free — create the
container, `touch` the file, and only then do the thing under test.

## Delegating to another shim

By default crawlc is backed by runc on Linux and by the hollow service elsewhere.
Set `io.containerd.crawlc.delegate` and it instead starts a second shim and
forwards every task call to it, still applying the configured delays on the way
through. crawlc becomes a slow layer in front of a runtime that behaves normally.

```sh
ctr run -d \
  --runtime io.containerd.crawlc.v1 \
  --annotation io.containerd.crawlc.delegate=io.containerd.nerdbox.v1 \
  --annotation io.containerd.crawlc.delay.Pids=30s \
  docker.io/library/busybox:latest slowbox sleep 3600
```

The value is either a runtime handler, mapped to a binary exactly as containerd
would (`io.containerd.nerdbox.v1` → `containerd-shim-nerdbox-v1` on `PATH`), or a
path, so a locally built shim can be pointed at directly:

```
io.containerd.crawlc.delegate=/home/you/nerdbox/_output/containerd-shim-nerdbox-v1
```

This is what makes crawlc useful on macOS: nerdbox runs Linux containers there
under VM isolation, so delegating to it gives real containers with crawlc's timing
faults in front of them.

Delegation is only for shims — programs speaking containerd's task API over ttrpc,
like nerdbox, runhcs or `containerd-shim-runc-v2`. An OCI runtime *binary* such as
`runc` itself is a different interface, and wrapping one would mean reimplementing
process supervision, IO and event publishing rather than forwarding calls.

### What crawlc does not forward

Two calls carry crawlc's identity rather than the container's:

- **`Connect`** reports crawlc's pid as the shim pid. containerd supervises and
  signals a shim by that pid, and the process it started is crawlc. Passing the
  delegate's through would aim containerd's supervision at a process it never
  launched. The task pid is forwarded untouched.
- **`Shutdown`** stops the delegate and then crawlc. Forwarding alone would leave
  crawlc running with nothing behind it.

Everything else is passed straight through.

### Lifecycle

The delegate runs in the same bundle on its own socket, under the container id
plus `.crawlc-delegate`. That suffix is load-bearing rather than cosmetic. Socket
addresses are `sha256(grpcAddress/namespace/id)`, so a delegate started under the
container's own id resolves to the socket crawlc already holds — and containerd's
manager treats an in-use socket with a live peer as "already started", handing
back that address without spawning anything. crawlc would proxy to itself, and
every signal would report success. containerd uses the same trick for a shim's
debug socket.

crawlc records the delegate in `crawlc-delegate.json` inside the bundle, because
the process that starts it, the daemon that talks to it, and the later `delete`
that reaps it are three different processes.

Reaping sends a task-API `Shutdown` before running the delegate's `delete` action.
The delete action alone tears down task state and leaves a running daemon alive,
which leaks a shim per container — a particularly confusing failure in a fixture
whose purpose is leaking the *first* shim deliberately.

If a delegate cannot be reaped, crawlc's own cleanup still runs, the error is
returned rather than swallowed, and `crawlc-delegate.json` is left in place as the
only record of a shim still out there, so a later delete can retry from it.

## Example: a leaked shim stalling containerd startup

A shim left behind by a containerd whose shutdown timed out still owns its socket,
so the next containerd connects to it and then waits for an answer that never
comes. Shims are loaded during plugin initialization, so that wait blocks startup
itself.

Create a container whose shim will wedge on `Pids`, which is what containerd calls
while loading a bundle to decide whether the shim is worth keeping:

```sh
ctr run -d \
  --runtime io.containerd.crawlc.v1 \
  --annotation io.containerd.crawlc.delay.Pids=forever \
  --annotation io.containerd.crawlc.arm.file=crawlc.arm \
  docker.io/library/busybox:latest wedged sleep 3600
```

Arm it, then restart containerd while the shim is still running:

```sh
touch /run/containerd/io.containerd.runtime.v2.task/default/wedged/crawlc.arm
systemctl restart containerd
```

Without a bound on the load, containerd never finishes starting: its API socket is
never created and `ctr version` hangs. With one, the load gives up on the shim
inside `io.containerd.timeout.shim.load` and startup completes.

To leave a genuinely orphaned shim behind rather than a cleanly stopped one, kill
containerd with `SIGKILL` — a clean shutdown reaps its shims.

Clean up afterwards; a wedged shim survives containerd:

```sh
pkill -f containerd-shim-crawlc-v1
```

## Tests

`make integration` runs the scenario above and its neighbours for real — a
containerd of its own per test, a container on crawlc, a restart onto the shim
left behind — and asserts what containerd did about it. It needs root, a
containerd, `ctr` and `runc`, so it is opt-in and a plain `go test ./...` skips
it:

```sh
make build
sudo -E env "PATH=$PATH" make integration
```

`sudo -E` alone is not enough: sudo replaces `PATH` with its `secure_path`
regardless of `-E`, and the Go toolchain is rarely on it, so the Makefile's
`go env` calls come back empty and the build fails before a test runs.

Everything it needs from outside is an environment variable:

| Variable | Effect |
|---|---|
| `CRAWLC_TEST_INTEGRATION` | Required. Without it every test skips |
| `CRAWLC_TEST_CONTAINERD`, `CRAWLC_TEST_CTR` | The containerd under test and its CLI. Default to `PATH` |
| `CRAWLC_TEST_SHIM` | The shim under test. Defaults to `./bin`, then `PATH` |
| `CRAWLC_TEST_RUNC` | The OCI runtime. Defaults to `PATH` |
| `CRAWLC_TEST_IMAGE` | Image containers are created from. Defaults to busybox |
| `CRAWLC_TEST_SNAPSHOTTER` | Set to `native` where the test root is itself on overlayfs, since overlay cannot stack on itself |
| `CRAWLC_TEST_BOUNDED_SHIM_LOAD` | Asserts the containerd under test bounds shim loading, see below |

Two of the tests reproduce the stalled startup above. On a containerd that does
not bound its shim load they do not fail, they hang, which is the bug — so they
run only when `CRAWLC_TEST_BOUNDED_SHIM_LOAD` says the containerd under test has
the bound. CI leaves it unset against a containerd release and the two skip.

## License

Apache 2.0. Portions are adapted from containerd's `containerd-shim-runc-fp-v1`
test shim, which is Apache 2.0 as well.
