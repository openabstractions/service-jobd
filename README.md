# service-jobd

**In development. This repository carries no tag**, so there is no release to
install: `go install` from a commit you have read, or build from a clone. The
signed Windows installer is `UNPROVEN` — see Status.

For someone running applications built on these abstractions: `jobd` is an
optional supervisor process that finishes and tidies up jobs — downloads, today
— that were started by a program which may no longer be running.

A job (a unit of work recorded in a job store — see
[abstraction-job](https://github.com/openabstractions/abstraction-job)) can
outlive the program that created it, and three things then go unattended:

- a transfer handed to Windows BITS keeps running after the application exits,
  but BITS will not release the finished file, and nothing checks its digest,
  until some process collects it;
- a machine that reboots mid-transfer leaves a job whose lease (a time-limited
  claim recorded in the store) has expired, with an incomplete file and nothing
  set to resume it;
- every calling program keeps its own idea of what is downloading, so
  restarting one clears its list while the partial files remain.

`jobd` is a loop over a job store that collects finished delegated jobs, adopts
jobs nobody is working on, and gives every program on the machine one answer to
what is in flight.

Part of [Open Abstractions](https://github.com/openabstractions/abstractions),
the parent project, which holds the scope rules, the method, the measured
results and the conformance suite.

**Nothing requires it.** Point `ABSTRACTION_STORE` at an empty directory with no
`jobd` running anywhere and a download through the layers still completes, in
the calling process. Installing `jobd` adds the ability to finish work after an
application closes; it is not a prerequisite for using the layers.

## Install

```
go install github.com/openabstractions/service-jobd@v0.2.0
```

This builds a `service-jobd` binary. Rename or symlink it to `jobd` if you want
the shorter name used below — the program does not read its own name.

## Commands

```
jobd            same as jobd status
jobd status     is a supervisor alive, and what does the store hold
jobd start      launch a detached supervisor that outlives this shell
jobd stop       stop the supervisor watching this store
jobd run        supervise in the foreground until interrupted
jobd once       one pass over the store, then exit
jobd discover   ask the announced supervisor whether it answers, and who it
                takes this process for
jobd setup      record configuration every program on this machine will read
jobd install    print the schtasks commands that register `jobd once`
jobd uninstall  print the schtasks commands that remove them
```

- `jobd status [--exit-code]` lists every job, its state, and which tier (a
  download mechanism such as BITS, or a network-attached-storage box running
  its own `jobd`) is handling it. With the flag it exits 1 unless a supervisor
  is alive, which suits a container healthcheck.
- `jobd run [--interval 30s] [--without <system>]` reconciles delegated jobs,
  delegates unclaimed jobs to a better tier when one is configured, adopts jobs
  whose lease expired, and finalises jobs that finished transferring. Repeat
  `--without` to exclude a delegation system: `--without nas --without bits`.
- `jobd once [--quiet]` is what a scheduled task or cron job should run.
  `--quiet` suppresses output when the pass found nothing to do, never errors.
- `jobd start [--interval 30s] [--without <system>]` stops any supervisor
  already watching this store, then launches a detached one — on Windows with
  no attached console, elsewhere in its own session — writing its output to
  `jobd.log` inside the store.
- `jobd install` and `jobd uninstall` print commands and run nothing.
- `jobd discover` prints what the heartbeat says, then what the supervisor's bus
  says, then who the supervisor took this process for, and exits 1 unless
  somebody answered. It is the check `jobd status` cannot make: a heartbeat
  outlives the process that wrote it.

A supervisor announces itself by writing a heartbeat, `supervisor.json`, into
the job store, and every program reads that same file to decide whether one is
alive. Both sides need only the store, so it works across a share as well as on
one machine. [Design notes](https://github.com/openabstractions/abstractions/blob/main/docs/discovery-ipc.md).

The heartbeat predicts and a connection decides. A supervisor also opens a bus —
a local transport whose name it invents and publishes in that same heartbeat —
and a program that can reach it learns at once whether anybody is still there,
rather than waiting for a timestamp to go stale. Every request on the bus
carries the caller as the kernel names it, and a caller the machine cannot name
is refused; nothing on the bus grants anything. A supervisor across a share, or
on a machine that cannot name a caller at all, announces no bus and is reached
through the store alone, exactly as before.

A text file dropped into `<store>/wanted/` is also a request: a URL per line,
optionally `sha256:<hex>` and a destination inside the store. The folder answers
by renaming the file `.accepted`, then `.done`, `.failed` or `.refused`.

## Where it stores things

By default `jobd` uses `~/.abstraction` on every operating system, including
Windows and macOS — a home-directory dotfolder, applied unconditionally. See
[Status](#status). Override it with the `ABSTRACTION_STORE` environment
variable, or with `jobd setup --store <path>`, which writes it to a
configuration file read by every program using
[abstraction-config](https://github.com/openabstractions/abstraction-config),
so a store move is told to one place. `jobd setup --show` prints the resolved
configuration and which file it came from.

Other variables: `MODELGET_STORE` (an older name, still honoured),
`ABSTRACTION_NAS_STORE` (a store on a share watched by a `jobd` elsewhere, to
which this one delegates), `ABSTRACTION_SHARED_STORE` (set when other machines
write this store through a mount whose path does not say so).

## Removing it

1. `jobd stop`.
2. If you registered a scheduled task, `jobd uninstall` prints the `schtasks`
   commands that remove it.
3. Delete the store directory — `~/.abstraction` unless you configured another.
   That removes every job record, partial file and the heartbeat. Delete the
   configuration file too if you ran `jobd setup`; `jobd setup --show` prints
   its path.
4. `go clean -i github.com/openabstractions/service-jobd`, or delete the binary
   from `$(go env GOPATH)/bin`.

## Status

Experimental. The supervision loop — reconcile, delegate, adopt, deliver — runs,
and has been exercised on Windows against both a BITS tier and a network-share
tier. Where a tier is configured `jobd` hands the transfer to it; where none is,
it performs the transfer itself.

Known gaps:

- **`jobd discover` is on `main` and not in `v0.2.0`.** It is implemented over
  the bus described above; in the tagged release it falls through to usage and
  exits 2, and a supervisor built from that tag binds nothing and announces no
  endpoint, so liveness there is the heartbeat's timestamp alone.
- **The bus is `UNPROVEN` off Windows.** It has been exercised end to end on
  Windows over a named pipe, in Go and from Python, including a caller the
  kernel refused to name. The Linux and macOS transports are compiled and not
  executed. On macOS the identity layer caps what a unix socket can say about a
  peer below what the bus asks for, so a supervisor there is expected to
  announce no bus and be reached through the store; expected, not observed.
- **The default store path is not platform-correct.** `~/.abstraction` is used
  unmodified on Windows and macOS rather than a directory conventional there.
- **`jobd install` and `jobd uninstall` always print Windows `schtasks`
  commands**, whichever operating system they run on. On Linux or macOS run
  `jobd once --quiet` from cron, a systemd timer or a launchd job instead.
- **One user only.** A supervisor must run as the same user as the programs
  that share its store. A service-account supervisor is not supported.
- **Installer packages are released by [redist](https://github.com/openabstractions/redist).**
  Its workflow builds and verifies the suite before creating a new installer
  version tag. This repository's Go supervisor tags do not identify installer
  releases; consult the redist run for its installation and signing evidence.

## Requirements

- Go 1.26 or later, to `go install` it.
- Windows, Linux or macOS. `go build` succeeds for all three; the scheduled-task
  convenience commands are Windows-only in practice — see Status.
- [abstraction-download](https://github.com/openabstractions/abstraction-download),
  [abstraction-job](https://github.com/openabstractions/abstraction-job) and
  [abstraction-config](https://github.com/openabstractions/abstraction-config),
  each at the exact version [`go.mod`](go.mod) pins. No version is repeated
  here: `go.mod` is the file the build reads, and a second copy of it on a page
  is a copy that goes stale without anything noticing.
- [go-winio](https://github.com/Microsoft/go-winio) v0.6.2, for named pipes.
  Windows has no named-pipe support in its standard library and no overlapped
  I/O, without which a client reading from an unresponsive supervisor cannot
  time out. It contributes nothing to a Linux or macOS build.

## Licence

Apache-2.0. See [LICENSE](LICENSE).
