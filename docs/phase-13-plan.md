# Phase 13 Plan — Release Pipeline

Status: Done
Based on: `tech-design.md` (§1.1, §6.4, §9, §9.1, §9.2), `backend-init-plan.md` §9 item 13, deploy plan (`radio-atlantida-deploy/docs/deploy-init-plan.md`: `IMAGE_TAG` format, no `latest`), frontend plan §9.2 (specification snapshot)
Last updated: 2026-10-14

## 1. Goal

A pushed tag `vX.Y.Z` produces a release: the image
`ghcr.io/astronin/radio-atlantida-backend:X.Y.Z` in GitHub Container
Registry and a GitHub release `vX.Y.Z` with the `openapi.json` of that
tag attached, which the frontend copies into its snapshot (`npm run
api:sync -- <file>`). The deploy repository then only sets
`IMAGE_TAG=X.Y.Z`.

Phase 1 left the hooks: `main.version` (default `dev`) feeds the OpenAPI
document, `ci.yml` builds the image without pushing and uploads the
specification as a workflow artifact.

## 2. Decisions (2026-10-14)

1. **Tags.** Only `vX.Y.Z` (strict semver, no pre-release suffix) starts a
   release; the image tag and the version are `X.Y.Z` without the `v`,
   because the update script validates `IMAGE_TAG` against
   `^\d+\.\d+\.\d+$`. Any other `v*` tag fails the workflow with a
   message. No `latest` and no moving tags: the deploy repository refers
   to exact tags only.
2. **A released tag is immutable.** If the image tag already exists in
   the registry, the workflow fails instead of overwriting it; a fixed
   release gets a new patch version.
3. **Releases come from `main`.** The tagged commit must be on `main`;
   otherwise the workflow fails before building.
4. **The release runs the full CI first.** `ci.yml` becomes reusable
   (`workflow_call`); `release.yml` calls it and builds only when lint,
   tests, OpenAPI lint, and the image build pass. No duplicated job
   definitions.
5. **The version is set at build time.** The Dockerfile takes
   `ARG VERSION=dev` and links it with `-ldflags "-X main.version=..."`.
   `make build` passes `git describe --tags --always --dirty`. The binary
   gets a `backend version` subcommand (prints the version), and the
   startup log line carries it, so a support question starts from the
   exact release.
6. **The specification comes from the image.** The workflow runs
   `backend openapi` in the image it just built, so the attached
   `openapi.json` is exactly the contract of the shipped binary, with
   `info.version` equal to the tag.
7. **Platform: `linux/amd64` only.** Production is Docker Desktop on a
   Windows 10/11 PC (§9.1); development builds the image locally. An
   arm64 image can be added when a machine needs it.
8. **OCI labels** on the image: `org.opencontainers.image.source` (links
   the private package to this repository, so its access follows the
   repository), `version`, `revision`, `created`, `title`. The image is
   built with plain `docker build` (BuildKit, the cache mounts of the
   Dockerfile), run once to extract the specification, and pushed; no
   build attestations.
9. **No new actions.** Registry login is `docker login` with
   the workflow's `GITHUB_TOKEN` through stdin; the release is created
   with the `gh` CLI of the runner, with generated notes. Permissions are
   per job: `packages: write` to push, `contents: write` for the release,
   everything else `contents: read`.
10. **`GO_VERSION` is checked.** CI fails when `GO_VERSION` in the
    Dockerfile differs from the `go` directive in `go.mod` (it was only a
    comment so far).

## 3. Files

| File | Change |
|------|--------|
| `.github/workflows/release.yml` | New: check tag and branch → CI (reusable) → build, check image tag is free, push, extract `openapi.json` from the image → GitHub release with the specification |
| `.github/workflows/ci.yml` | `workflow_call` trigger; `GO_VERSION` check |
| `docker/Dockerfile` | `ARG VERSION`, ldflags, OCI labels via build args |
| `cmd/backend/main.go` | `backend version` subcommand |
| `cmd/backend/serve.go` | Version in the startup log line |
| `Makefile` | `VERSION` from `git describe`; `build` links it; `image` builds the image locally with it |

## 4. Releasing (runbook)

1. Merge to `main`; CI is green.
2. `git tag v1.0.0 && git push origin v1.0.0`.
3. The `release` workflow pushes `ghcr.io/astronin/radio-atlantida-backend:1.0.0`
   and creates the release `v1.0.0` with `openapi.json`.
4. Frontend: download the asset, `npm run api:sync -- <file>`, commit, tag
   the frontend with the same version.
5. Deploy: `update.bat 1.0.0` once both images exist.

A failed release (for example a flaky test) is re-run from the Actions
page; the tag is not moved. If the image was already pushed and only the
release step failed, the re-run stops at the immutability check; create
the release by hand (`gh release create v1.0.0 openapi.json`) or tag a new
patch.

## 5. Known limits

- The frontend does not download the release asset itself yet; its
  `api:sync` takes a path. Automating that is a frontend task.
- The workflows pass `actionlint` (with shellcheck, run in its container),
  but the release path itself runs only on a real tag: the first release
  is its acceptance test.
- The GHCR package is private; the first push creates it. Its visibility
  and the read token for the production machine are set by hand
  (deploy plan D1).

## 6. Done when

`backend version` prints the linked version (unit test); `make build`
and `make image` link the `git describe` version; the image built with
`--build-arg VERSION=1.2.3` answers `backend version` and `backend
openapi` with `1.2.3` and carries the OCI labels (checked locally); CI
passes with the `GO_VERSION` check; `release.yml` refuses a non-semver
tag and a commit not on `main`, and on a valid tag pushes the image and
publishes the release with `openapi.json`.
