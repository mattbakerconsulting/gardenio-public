# build-dictation-corpus

Pulls text from every bead in the kbeads store (all projects) and emits
three artifacts used by the Gardenio Catalyst app and the Gasvoice macOS
menu-bar helper to bias Apple's speech recognizer toward our jargon:

| Artifact        | Consumer                                                    |
|-----------------|-------------------------------------------------------------|
| `glossary.json` | `SFSpeechRecognitionRequest.contextualStrings` (top ~100 terms) |
| `phrases.yaml`  | `SFCustomLanguageModelData` template input (1k–10k phrases) |
| `corpus.txt`    | Raw clean text, reserved for future use                     |

**Scope lock:** Apple Speech framework only. This tool produces inputs for
`SFSpeechRecognizer` and `SFCustomLanguageModelData` — no Whisper, no cloud
STT, no third-party speech engines.

## Run locally

```sh
cd scripts/build-dictation-corpus
go build -o build-dictation-corpus
./build-dictation-corpus \
    --http-url http://localhost:8080 \
    --out build/dictation
```

Flags:

| Flag              | Default                                                       | Purpose                                              |
|-------------------|---------------------------------------------------------------|------------------------------------------------------|
| `--http-url`      | `$BEADS_HTTP_URL` or the cluster-internal service DNS         | kbeads REST base URL                                 |
| `--token`         | `$BEADS_TOKEN`                                                | Bearer token for the API                             |
| `--fleet`         | `$BEADS_FLEET`                                                | `X-Beads-Fleet` header                               |
| `--out`           | `build/dictation`                                             | Output directory                                     |
| `--glossary-size` | `100`                                                         | `SFSpeechRecognitionRequest.contextualStrings` size  |
| `--max-phrases`   | `10000`                                                       | Cap on `phrases.yaml` entries                        |
| `--identifier`    | `ai.acme.gardenio.dictation`                                   | `SFCustomLanguageModelData.Configuration` identifier |
| `--workers`       | `16`                                                          | Parallel comment fetchers                            |
| `--dry-run`       | `false`                                                       | Fetch + process only; no files written               |

## Sources

For every bead the tool reads:

- `title`, `description`, `notes`
- all comments
- decision/report string fields (`prompt`, `rationale`, `response_text`,
  `content`, `context`, `report_type`)
- agent-bead fields (`prompt`, `wrapup`, `close_reason`, `schedule_title`) —
  these typically carry MR descriptions and operator instructions

## Cleaning

- fenced code blocks (```` ``` ````) — stripped
- inline code (`` ` ``) — stripped
- URLs (`http://` / `https://`) — stripped
- markdown links `[text](#)` — keep the text, drop the target
- HTML-ish tags — stripped
- whitespace / blank lines — collapsed
- bead IDs (`ka-0000001713`) — **preserved**; also emitted as spelling
  templates in `phrases.yaml` so dictation catches them

## ID spelling

A bead id like `ka-0000000316` becomes three phrases in `phrases.yaml`:

```
- "bead k d dash U v W U b O k C x P"
- "k d dash U v W U b O k C x P"
- "see k d dash U v W U b O k C x P"
```

Digits are expanded to english words (`6` → `"six"`, `0` → `"zero"`) so
the speech recognizer can choose between `"oh"` and `"zero"`.

## Nightly regen

Nothing regenerates the corpus on a schedule today. The RWX nightly that did
(archived under `docs/archive/rwx/`) was deleted on 2026-09-16, and checks.yml
only vets and tests the tool. That nightly uploaded the artifacts to
`gs://<bucket>/dictation/latest/` and dropped a dated snapshot at
`gs://<bucket>/dictation/snapshots/YYYY-MM-DD/`. Xcode build phases fetch from `latest/` for the Catalyst and Gasvoice targets.
