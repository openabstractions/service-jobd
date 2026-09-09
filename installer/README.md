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

## Build it

WiX needs no .NET SDK. Unzip the two NuGet packages and run the tool:

    curl -Lo wix.zip https://api.nuget.org/v3-flatcontainer/wix/5.0.2/wix.5.0.2.nupkg
    curl -Lo ui.zip  https://api.nuget.org/v3-flatcontainer/wixtoolset.ui.wixext/5.0.2/wixtoolset.ui.wixext.5.0.2.nupkg
    unzip -q wix.zip -d wix && unzip -q ui.zip -d ui

    py -3 installer/build.py --arch x64 --version 0.2.0 \
      --wix wix/tools/net6.0/any/wix.exe \
      --ext ui/wixext5/WixToolset.UI.wixext.dll --out dist \
      --src self=. --src download=<abstraction-download> --src job=<abstraction-job>

`build.py` stages every `payload.tsv` row, writes `license.rtf` through
`mklicense.py`, runs `validate.py` and calls `wix build`. A source not named
with `--src` is left out, and it refuses to build a package missing a file
`abstraction.wxs` cannot gate away — which is how the UNRESOLVED rows leave the
package without anyone editing anything.

**What comes out is unsigned**, and Windows will name the publisher unknown.
Signing is a separate, manually approved step, outside this build and outside
the release workflow, and it signs the programs inside each MSI as well as each
MSI — see `signpath.artifact-configuration.xml`.

## Check it

    py -3 installer/validate.py [package.msi ...] [--wix <wix>]

It parses `abstraction.wxs`, cross-checks it against `payload.tsv` and
`sources.tsv`, refuses a file that is in one and not the other or that two
features install, refuses a signing target that is not a PE file, refuses a
source pinned to anything but a 40-hex commit, and prints the signing targets.
`installer/signpath.artifact-configuration.xml` is checked against that list, so
adding a program without adding it to the signing configuration is a red build.

Given a built package it reads it back with `wix msi decompile` and refuses one
carrying a file `abstraction.wxs` does not install, or missing one it does and
cannot gate away.

## What may break

- **The panel and the C++ header have no publishable source.** Their rows in
  `payload.tsv` name `UNRESOLVED`, the workflow leaves them out and says so, and
  `abstraction.wxs` gates them on `$(var.Panel)` and `$(var.Cpp)`. Somebody has
  to decide which repository publishes them.
- **Nothing has been installed.** Both MSIs build, and their file, feature,
  registry, environment and custom-action tables have been read back out of the
  built packages. No package has been run. The `verify` job of the release
  workflow is what runs one; it has never been triggered.
- **Two builds of one version install alongside each other.** `abstraction.wxs`
  sets no `ProductCode`, so WiX generates one per build, and `MajorUpgrade`
  without `AllowSameVersionUpgrades` will not see the first install. The
  tag-driven workflow builds each version once and never hits this; anyone
  building locally twice does.
- **`jobd install` prints its `schtasks` lines rather than running them**, so the
  package registers the two tasks itself and the task shape now has two owners.
  Nothing compares them, and the two owners use the same two task names, so an
  uninstall deletes tasks a person registered by hand.
- **The Tools feature's title and description promise a panel** that is gated out
  and has no publishable source.
- **`sources.tsv` is behind the published tags.** Both pins resolve, neither is a
  tag any more.
