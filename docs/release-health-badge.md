# Release health badge

> **Superseded (2026-10-27)** by [`releases-design.md`](../gardenio/docs/releases-design.md) (gd-hls6BkIEiO). The GitLab badges, `.rwx/docker.yml` and `.gitlab-ci.yml` it describes no longer exist. Kept for history.

At-a-glance visibility into release-train health on the GitLab project page.
Tracks bead `ka-0000005261`; motivated by the 2026-04-22 → 2026-04-23 stuck-train
incident where the release pipeline was silently broken for ~2 weeks.

## What you see

Two badges in the README and on the project overview:

- **release** — latest golden calver tag (e.g. `release: 2026.113.6`).
- **release time** — elapsed time from tag push to images promoted from
  `-rc` to the final calver tag + `latest` (e.g. `release time: 12m 43s`).
  Color degrades green → yellow → red past 15 and 30 minutes respectively so
  creeping pipeline time is visible.

## How it works

1. On a release tag push, `.rwx/docker.yml` posts two commit statuses:
   - `RWX: Deploy: gardenio-rwx` (existing)
   - `RWX: Promote images` (added in this change) — set to `success` once
     the `promote-images` task flips every `<calver>-rc` tag to the final
     `<calver>` + `latest` tag.
2. The GitLab CI `publish-release-metadata` job (`.gitlab-ci.yml`) runs
   after `create-release` succeeds, polls for `RWX: Promote images`, then
   writes two JSON files to the GitLab **Generic Package Registry** at
   package `release-health`, version `latest` (overwritten every release —
   the latest upload wins on `GET`):
   - `release-metadata.json` — `{schemaVersion, label, message, color, duration_seconds}`
   - `release-duration.json` — same schema, formatted message for the duration badge
   A versioned copy is also written under `release-health/<calver>/…` for
   historical traceability (best-effort).
3. The badges point at [shields.io's `/endpoint`][shields-endpoint] URL
   which renders an SVG from the JSON file. Duration is recalculated each
   release and appears in the badge within seconds of the upload completing.

[shields-endpoint]: https://shields.io/endpoint

## JSON schema

```json
{
  "schemaVersion": 1,
  "label": "release",
  "message": "2026.113.6",
  "color": "success",
  "duration_seconds": 763
}
```

`duration_seconds` is the pipeline-level duration (tag push → promote-images
green). It is carried in both files so downstream consumers can read it
programmatically without re-parsing the duration string.

## Badge URLs (README + project Settings → Badges)

Both badges are driven from the same package path. Keep these two badges
in sync between the README and `Settings → General → Badges`.

### Release version

- **Name**: `release`
- **Link URL**: `https://gitlab.com/example-org/gardenio/-/releases`
- **Image URL**:
  ```
  https://img.shields.io/endpoint?url=https%3A%2F%2Fgitlab.com%2Fapi%2Fv4%2Fprojects%2Fexample-org%252Fgardenio%2Fpackages%2Fgeneric%2Frelease-health%2Flatest%2Frelease-metadata.json&logo=gitlab
  ```

### Release duration

- **Name**: `release time`
- **Link URL**: `https://gitlab.com/example-org/gardenio/-/pipelines?scope=tags`
- **Image URL**:
  ```
  https://img.shields.io/endpoint?url=https%3A%2F%2Fgitlab.com%2Fapi%2Fv4%2Fprojects%2Fexample-org%252Fgardenio%2Fpackages%2Fgeneric%2Frelease-health%2Flatest%2Frelease-duration.json&logo=gitlab
  ```

## Adding the project badges

Project badges are not config-as-code in GitLab — they live in the project
settings. Two options:

### Option A — UI

Settings → General → Badges → Add badge → paste the Name / Link URL /
Image URL from the section above. Repeat for each badge.

### Option B — API (one-shot)

```sh
PROJECT_ID=$(curl -sf -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  "https://gitlab.com/api/v4/projects/Acme%2FPlatform%2Fgardenio" \
  | jq -r .id)

for spec in \
  "release|https://gitlab.com/example-org/gardenio/-/releases|https://img.shields.io/endpoint?url=https%3A%2F%2Fgitlab.com%2Fapi%2Fv4%2Fprojects%2Fexample-org%252Fgardenio%2Fpackages%2Fgeneric%2Frelease-health%2Flatest%2Frelease-metadata.json&logo=gitlab" \
  "release%20time|https://gitlab.com/example-org/gardenio/-/pipelines?scope=tags|https://img.shields.io/endpoint?url=https%3A%2F%2Fgitlab.com%2Fapi%2Fv4%2Fprojects%2Fexample-org%252Fgardenio%2Fpackages%2Fgeneric%2Frelease-health%2Flatest%2Frelease-duration.json&logo=gitlab"; do
  NAME=${spec%%|*}; REST=${spec#*|}; LINK=${REST%%|*}; IMG=${REST#*|}
  curl -sf -X POST \
    -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
    --data-urlencode "name=${NAME}" \
    --data-urlencode "link_url=${LINK}" \
    --data-urlencode "image_url=${IMG}" \
    "https://gitlab.com/api/v4/projects/${PROJECT_ID}/badges"
done
```

The token needs `api` scope. A project access token is sufficient.

## Caveat: Generic Package visibility

Generic Packages inherit project visibility. On a private project,
`https://gitlab.com/api/v4/projects/.../packages/generic/...` returns 401
without an authorized caller — shields.io cannot fetch it anonymously. If
this project remains private and the badge shows `inaccessible` on shields,
the follow-up is to either:

1. Mirror the two JSON files to public GitLab Pages (separate visibility
   toggle from project), or
2. Mirror to a public sidecar project / gist, or
3. Make the project itself public.

Option 1 is the cheapest (one Pages job, no new repo). Tracked as a
follow-up when the acceptance criteria land but the badges don't render.
