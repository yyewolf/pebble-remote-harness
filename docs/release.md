# Releasing

Cutting a release is one command:

```bash
git tag v0.2.0
git push origin v0.2.0
```

`.github/workflows/release.yml` does the rest. Everything on the release page
is built from that tag by CI — nothing is uploaded by hand, and no version
bump is ever committed back to the repo.

## What comes out

| Asset | Built by | Notes |
|---|---|---|
| `pebble-remote-harness-<target>-<v>.vsix` | `vsix` job | one per platform, `prh` and the Kilo plugin inside |
| `prh-<os>-<arch>` | `daemon` job | standalone daemon, static, no libc dependency |
| `prh-companion-<v>.apk` | `companion` job | debug-signed, sideload directly |
| `prh-watchapp-<v>.pbw` | `watchapp` job | `emery` only |
| `prh-kilo-plugin-<v>.tgz` | `plugin` job | for a Kilo setup not driven by the extension |
| `SHA256SUMS` | `release` job | covers every asset above |

The `release` job needs all five to succeed, so a release page is never
partially populated.

## Versioning

The tag is the only version. Each job stamps it into its own manifest at build
time:

- **extension, plugin, watchapp** — `npm version --no-git-tag-version`
- **companion** — `PRH_VERSION_NAME` and `PRH_VERSION_CODE` are read by
  `app/build.gradle.kts`, which falls back to literals for a local build

Android's `versionCode` is derived rather than hand-maintained:
`1.2.3` → `10203`. Nobody has to remember to bump it, and two branches cannot
disagree about what it should be.

A tag with a pre-release suffix (`v2.0.0-rc1`) is marked as a pre-release on
GitHub and is **not** published to the marketplace. Its `versionCode` drops the
suffix, so `2.0.0-rc1` and `2.0.0` collide — deliberate, since only one of them
is meant to reach a device.

## Platform coverage

The daemon is released for `linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64` and `windows/amd64`. CGO is off, so every one of these is a
static binary produced by the host toolchain — no cross compiler, and the Linux
build runs unmodified on musl, which is why there is an `alpine-x64` VSIX.

**The extension is not released for Windows.** Hop 0 — the Kilo plugin talking
to prh — is an AF_UNIX socket, and Node implements the local domain on Windows
as named pipes only, so the extension could start a daemon it then cannot
reach. The `windows/amd64` binary is still published for running prh
standalone. Supporting Windows properly means giving `plugin/src/index.js`,
`extension/src/daemon.ts` and `api/internal/config` a named-pipe transport;
until then, shipping a `win32-x64` VSIX would only produce a broken install.

## The Android signing key

The APK is the **debug** build, signed with the well-known Android debug key.
That is what `make package` produces and what the sideloading instructions
assume. The consequences are worth stating plainly: it cannot go to the Play
Store, and it cannot be installed over a build signed with a different key —
users have to uninstall first.

To move to a real key, add a `signingConfig` to `companion/app/build.gradle.kts`
reading from environment variables, three secrets (`ANDROID_KEYSTORE_BASE64`,
`ANDROID_KEYSTORE_PASSWORD`, `ANDROID_KEY_ALIAS`), and a decode step in the
`companion` job before `make companion`. Do it before the first release anyone
else installs — the key cannot be changed afterwards without every user
uninstalling.

## Secrets

| Secret | Used by | Required |
|---|---|---|
| `VSCODE_PUBLISH_TOKEN` | `marketplace` job | no — the job logs and skips if unset |

`GITHUB_TOKEN` is provided automatically and is what creates the release.

The marketplace job is deliberately separate from the `release` job and runs
after it. The marketplace rejects a version it already has, and that failure
must not be able to take down the GitHub release that everyone else installs
from.

### The token itself

`VSCODE_PUBLISH_TOKEN` is a **Azure DevOps personal access token**, not a
GitHub one: organisation *All accessible organizations*, scope *Marketplace →
Manage*. It is passed to `vsce` as `VSCE_PAT`. The publisher ID in
`extension/package.json` (`yyewolf`) has to exist and the token has to belong
to an account that owns it.

`"private": true` was removed from `extension/package.json` when publishing was
set up — vsce refuses to publish a package marked private. Do not put it back.

## CI

`.github/workflows/ci.yml` runs on every branch and pull request and builds
each component on its own runner. The four toolchains here share nothing, and
splitting them means a Pebble SDK download failing cannot hide a Go test
failure behind it.

The `api` job cross-compiles every released platform on top of running the
tests. That is not redundant: it is what catches a build tag or a syscall that
only resolves on Linux, which is otherwise found by a user on a Mac after the
release is cut.

The Pebble SDK setup is shared between both workflows as a composite action in
`.github/actions/pebble-sdk`. It pins Python to 3.12 — pebble-tool does not run
on anything newer, and the runner's default python is — and caches the SDK,
which is a ~100MB download from Rebble's server.
