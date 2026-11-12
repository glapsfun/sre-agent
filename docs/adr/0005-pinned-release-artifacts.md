# ADR 0005: Pinned release artifacts

Status: retrospective record of implemented behavior.

## Observed decision

The Bash launcher pins a stable version and selects one of four native
Linux/macOS amd64/arm64 executables. It downloads the asset and `SHA256SUMS`
into locked staging, verifies integrity before publishing the cache, and
rechecks every launch. The tag workflow builds and tests on native platforms,
injects the tag as the binary version, uploads a draft release, publishes it,
and verifies downloaded assets on all four platforms.

Evidence: [launcher](../../plugins/sre-agent/skills/sre-agent/scripts/sre),
[launcher fixtures](../../tests/sre-launcher.test.sh), and
[release workflow](../../.github/workflows/release.yml).

## Inferred rationale

Pinning and per-launch checks appear intended to make the invoked version
predictable and detect changed downloads or cached binaries. Native execution
before and after publication checks more than successful compilation. This
rationale is inferred from the workflow and tests, not historical decision notes.

## Consequences and tradeoffs

A verified cache works offline. A new cache needs network access and curl;
all launches need a checksum tool. The launcher refuses invalid artifacts and
does not silently execute a fallback. A new version requires explicit source,
manifest, pin, and fixture alignment. Checksums trust the manifest and do not
supply independently signed provenance. Publication is not automatically
idempotent after partial draft creation or upload, and failed post-publication
checks do not retract a visible release. Deliberate maintainer recovery is
required; a source build remains an alternative when the pinned asset is
unavailable.

See [release operations](../releases.md) for the version contract, verification,
and partial-release recovery procedure.
