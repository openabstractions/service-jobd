# installer — what a Windows user actually installs

Two MSIs, `abstraction-x64.msi` and `abstraction-arm64.msi`, built by
`.github/workflows/release.yml` in `openabstractions/service-jobd`. Three
features, the shape .NET and Git for Windows already use:

| feature | default | what it is |
|---|---|---|
| Background service | on, cannot be unticked | `jobd`, and the two scheduled tasks that run it |
| Command-line tools and panel | on | `dl`, `jobctl`, the panel, and this folder on `PATH` |
| Developer files | off | headers, the Python packages, the Go module paths |

The package is **per-user**: it installs into `%LOCALAPPDATA%\Programs\Abstraction`,
asks for no administrator, and registers its scheduled tasks as the person who
ran it. That is not a preference. `schtasks` registering a logon task for
*another* account needs that account's password, and this package will never
ask for one.

## Install

    msiexec /i abstraction-x64.msi

    msiexec /i abstraction-x64.msi /qn ADDLOCAL=Service,Tools,Developer

## Check it here, without building it

    py -3 validate.py

It parses `abstraction.wxs`, cross-checks it against `payload.tsv` and
`sources.tsv`, refuses a file that is in one and not the other or that two
features install, refuses a signing target that is not a PE file, refuses a
source pinned to anything but a 40-hex commit, and prints the signing targets.
`installer/signpath.artifact-configuration.xml` is checked against that list, so
adding a program without adding it to the signing configuration is a red build.

## What may break

- **The panel and the C++ header have no publishable source.** Their rows in
  `payload.tsv` name `UNRESOLVED`, the workflow leaves them out and says so, and
  `abstraction.wxs` gates them on `$(var.Panel)` and `$(var.Cpp)`. Somebody has
  to decide which repository publishes them.
- **Nothing here has been built.** No WiX toolchain exists on the machine this
  was written on, by design; the package is built in CI and nowhere else. What
  is proven and what is not is in `research/inst83/PACKAGE.md`.
- **`jobd install` prints its `schtasks` lines rather than running them**, so the
  package registers the two tasks itself and the task shape now has two owners.
  Nothing compares them.
