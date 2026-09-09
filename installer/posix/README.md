# installer/posix — what a Linux or macOS user actually installs

Three assets, built by the release workflow in `openabstractions/redist`:

| asset | what it is |
|---|---|
| `abstraction-<version>-linux-amd64.tar.gz` | a tarball with `install.sh` |
| `abstraction-<version>-linux-arm64.tar.gz` | the same, for aarch64 |
| `abstraction-<version>-macos-universal.pkg` | one `pkg`, x86_64 and arm64 in one binary |

All three are **per-user**. They install into the home directory, ask for no
root and no administrator password, and the background sweep each registers
belongs to the person who installed it — a systemd **user** unit on Linux, a
**LaunchAgent** on macOS. Neither is a system service, for the same reason the
Windows package is per-user: a supervisor that finishes your downloads runs as
you, and registering one for another account needs that account's password.

## Why a tarball on Linux and not `.deb` and `.rpm`

A `.deb` or an `.rpm` is a system package by construction. It is unpacked by
root, it lands under `/usr`, and there is no supported way for it to register a
systemd **user** unit for the person who typed `apt install` — `dpkg` does not
know who that was, and postinst scripts that guess get it wrong on multi-user
machines. Making this product a `.deb` would mean making it a system daemon,
which is a different product with different security properties and a different
answer to "what happens when you log out".

The second reason is cost. Two formats, two signing keys, two repository
layouts, and a per-distribution matrix that grows every time a distribution
does. The tarball is one artifact per architecture, works on every distribution
including the ones with no packaging story, and is the format `rustup`, `go`,
Node and every Go single-binary tool already ship.

What we give up: no `apt upgrade`, no dependency solving (there are no
dependencies — the binaries are static, `CGO_ENABLED=0`), and no distribution
review. Reinstalling over an existing install is how you upgrade, and
`uninstall.sh` then `install.sh` is how you are sure.

## Linux — what it places, and what removes it

    tar -xzf abstraction-0.3.0-linux-amd64.tar.gz
    cd abstraction-0.3.0-linux-amd64
    ./install.sh

| what | where |
|---|---|
| `jobd`, `dl`, `jobctl` | `~/.local/bin/` |
| the sweep unit and its timer | `~/.config/systemd/user/abstraction-jobd.{service,timer}` |
| the Python packages, `USING.txt`, `LICENSE` | `~/.local/share/abstraction/dev/` |
| the uninstaller and the list it works from | `~/.local/share/abstraction/{uninstall.sh,MANIFEST}` |

`install.sh` then runs `systemctl --user enable --now abstraction-jobd.timer`.
The timer fires 30 seconds after the user manager starts — which is your login —
and every 5 minutes after that, which is the same shape as the two Windows
scheduled tasks, `jobd-logon` and `jobd`.

It does **not** edit any shell profile. If `~/.local/bin` is not on `PATH` it
prints the one line to add and says why it will not add it for you.

It does **not** fail when there is no systemd user manager — a container, WSL
without systemd, a machine with no user bus. It installs the three programs,
prints that nothing will sweep in the background and what to run instead, and
records `timer no` in the manifest. Absence is reported, never passed.

Removal, exactly:

    ~/.local/share/abstraction/uninstall.sh

It disables and stops the timer, deletes every path in `MANIFEST` and nothing
else, removes every directory that is then empty up to your home directory, and
prints the command for the two things it deliberately leaves: `~/.abstraction`,
your job store, and `~/.config/abstraction`, what `jobd setup` recorded.

## macOS — what it places, and what removes it

    open abstraction-0.3.0-macos-universal.pkg

One `productbuild` archive around one `pkgbuild` component, identifier
`com.openabstractions.abstraction`, with
`<domains enable_currentUserHome="true" enable_localSystem="false"/>` so the only
destination the installer offers is the current user's home.

| what | where |
|---|---|
| `jobd`, `dl`, `jobctl`, universal | `~/.local/bin/` |
| the LaunchAgent | `~/Library/LaunchAgents/com.openabstractions.jobd.plist` |
| the Python packages, `USING.txt`, `LICENSE` | `~/.local/share/abstraction/dev/` |
| the uninstaller and its two lists | `~/.local/share/abstraction/{uninstall.sh,FILES,MANIFEST}` |

**LaunchAgent identifier: `com.openabstractions.jobd`**, at
`~/Library/LaunchAgents/com.openabstractions.jobd.plist`. `RunAtLoad` is the
logon task; `StartInterval 300` is the five-minute sweep. `scripts/postinstall`
substitutes the absolute path of `jobd`, writes `MANIFEST`, chowns everything to
the installing user, and runs `launchctl bootstrap gui/<uid>`. If that user has
no graphical session right now, it says so and the agent loads at the next
login; it does not fail the install.

Removal, exactly the same command as on Linux:

    ~/.local/share/abstraction/uninstall.sh

It runs `launchctl bootout`, deletes every path in `MANIFEST`, prunes the empty
directories, runs `pkgutil --forget com.openabstractions.abstraction`, and
prints the command for `~/.abstraction` and
`~/Library/Application Support/abstraction`.

## Build it

Both build from published sources checked out at the commits `sources.tsv`
pins, never from a working tree. `build.py` re-reads `git rev-parse HEAD` in
each checkout and refuses a build where it does not match.

    py -3 installer/posix/build.py --platform linux --arch amd64 --version 0.3.0 \
      --out dist --src jobd=<service-jobd> --src download=<abstraction-download> \
      --src job=<abstraction-job>

    python3 installer/posix/build.py --platform macos --version 0.3.0 \
      --out dist --src jobd=<service-jobd> --src download=<abstraction-download> \
      --src job=<abstraction-job>

The Linux tarball is deterministic: fixed mtimes, uid 0, sorted names, gzip with
no timestamp. Two builds of one commit are byte-identical. The macOS package is
not claimed to be; `pkgbuild` writes a bom and a payload archive of its own.

`--platform macos` needs `lipo`, `pkgbuild` and `productbuild`, and refuses by
name on a machine that has none of them rather than skipping the package.

## Signing — what the owner has to produce

**Nothing here is signed.** The tarball says so on its last line, the macOS
welcome and conclusion panes say so, and the release notes say so. Signing is a
separate step and it needs material only the owner can produce.

### macOS, in this order

1. **`Developer ID Application: Reinis Lusis (2U744DSR8L)`, exported as a
   `.p12`.** In Keychain Access, select the certificate *and* its private key,
   File → Export Items, format Personal Information Exchange. Give it a
   passphrase. Then `base64 -i cert.p12 | pbcopy`.
   - repository secret `MACOS_CERTIFICATE` — that base64 text
   - repository secret `MACOS_CERTIFICATE_PASSWORD` — the passphrase you chose

   What it is for: the workflow writes it back to a file, creates a **temporary**
   keychain for that one run, imports it, signs, and deletes the keychain. It is
   never added to a keychain on any of your machines.

2. **The team identifier.** `2U744DSR8L`, from the certificate name.
   - repository secret `MACOS_TEAM_ID`

   What it is for: `notarytool` needs it, and `codesign` uses it to pick the
   identity when the temporary keychain holds more than one.

3. **An App Store Connect API key**, at appstoreconnect.apple.com → Users and
   Access → Integrations → App Store Connect API. Create a key with the
   **Developer** role. You can download the `.p8` **once**; the page shows the
   Key ID beside it and the Issuer ID above the list.
   - repository secret `APPLE_API_KEY_ID` — the 10-character Key ID
   - repository secret `APPLE_API_ISSUER_ID` — the issuer UUID
   - repository secret `APPLE_API_KEY_P8` — the whole contents of the `.p8`,
     including its first and last armour lines

   What it is for: `xcrun notarytool submit --key … --key-id … --issuer …`
   uploads the signed package to Apple, which scans it and issues a ticket.
   An API key is used rather than an Apple ID and app-specific password because
   it carries no account password and can be revoked on its own.

   Why an App Store Connect key and not the Developer ID certificate again:
   they are different things. The certificate proves who built it; notarisation
   is Apple telling every Mac that it has seen this exact file and found no
   malware. Without the ticket, Gatekeeper on a machine that downloaded the file
   from a browser refuses to open it, signed or not.

4. Nothing else. There is no macOS *installer* certificate here — `productsign`
   wants `Developer ID Installer`, which is a **second** certificate on the same
   account. Create it at developer.apple.com → Certificates → `+` → Developer ID
   Installer, download it, install it in Keychain Access, and export it the same
   way as step 1.
   - repository secret `MACOS_INSTALLER_CERTIFICATE`
   - repository secret `MACOS_INSTALLER_CERTIFICATE_PASSWORD`

   What it is for: a `.pkg` is signed with the Installer identity, not the
   Application one. The three programs inside it are signed with the Application
   identity from step 1. Both are needed; a `.pkg` signed with the Application
   certificate is rejected.

   **Until this exists, the macOS package cannot be signed at all**, and the
   Developer ID Application certificate on its own is not enough.

Order matters only in that 4 is a different certificate from 1 and people
routinely believe it is the same one.

### Linux

Nothing to buy and nothing to install. There is no Gatekeeper and no
SmartScreen. What a Linux user checks is the SHA-256 published beside the
tarball, which the release already carries.

The optional step, when there is something to say about who built it, is a
detached OpenPGP signature per asset (`.tar.gz.asc`) and a published fingerprint.
That needs one secret, `GPG_PRIVATE_KEY`, an ASCII-armoured export, plus
`GPG_PASSPHRASE`. It is worth doing only once the public key lives somewhere a
stranger can find it independently of the release page.

### Windows

Still no certificate. Unchanged by anything here.

## What may break

- **Nothing on macOS has been executed.** `pkgbuild`, `productbuild`,
  `lipo`, the postinstall script, `launchctl bootstrap` and `pkgutil --forget`
  have never run. They are `UNPROVEN` until a `macos-latest` runner runs them.
- **The `enable_currentUserHome` install location is the least-travelled part.**
  A component built with `--install-location /` and installed into the home
  domain lands relative to the home directory; if that turns out to be wrong on
  a current macOS the payload lands at the filesystem root instead, and the
  first CI run is what will say so.
- **`jobd install` prints `schtasks` lines on every platform.** Run it on Linux
  or macOS and it tells you to type Windows commands. These packages therefore
  register the timer and the agent themselves, and now three places own the
  shape of that schedule instead of two.
- **`jobd` has no published tag.** `openabstractions/service-jobd` carries no
  tags at all, so `sources.tsv` pins it to a commit on `main` and marks the tag
  column `-`. `dl` and `jobctl` come from `go/v0.3.0` in their own repositories.
- **`~/.local/bin` is on `PATH` by default on most Linux distributions and on no
  macOS.** Both installers print the line; neither writes it.
- **The Windows package offers three features and these offer none.** A tarball
  and a `pkg` with `customize="never"` install everything they contain,
  developer files included. That is a deliberate divergence from
  `abstraction.wxs`, which makes Developer opt-in.
- **Upgrading is reinstalling.** Neither format removes a file that a previous
  version installed and this one does not.
