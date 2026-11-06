# build-dictation-lm

Build-time compiler that reads `phrases.yaml` (emitted by
`scripts/build-dictation-corpus`) and produces `beads.bin`
(`SFCustomLanguageModelData` archive) for
`SFSpeechLanguageModel.prepareCustomLanguageModel` + the
`request.customizedLanguageModel` path.

## Scope lock

**Apple Speech framework only.** No Whisper, WhisperKit, whisper.cpp,
Deepgram, AssemblyAI, or third-party speech engines.

## Requirements

- macOS 14+ (`SFCustomLanguageModelData` first shipped in macOS 14 / iOS 17)
- Xcode 15+ / Swift 5.9+

## Usage

```sh
swift run build-dictation-lm \
  --input build/dictation/phrases.yaml \
  --output build/dictation/beads.bin
```

`--client-id` overrides the default `ai.acme.gardenio.dictation` identifier
if the `phrases.yaml` doesn't specify one.

If `--input` does not exist the command exits `0` with a skip notice.
This makes it safe to wire into an Xcode "Run Script" build phase —
local Debug builds that haven't fetched the corpus artifact still build,
and the app falls back to `contextualStrings` at runtime.

## Integration

Invoked by `usr/Gardenio/scripts/build-custom-lm.sh` as an Xcode
build phase before the Resources copy phase of the Gardenio app target.
The resulting `beads.bin` is then copied into
`usr/Gardenio/Gardenio/Voice/beads.bin` and bundled as an app resource.

The Gasvoice menu-bar helper (`usr/Gasvoice/`) consumes the same
`beads.bin` — drop it at
`usr/Gasvoice/Sources/Gasvoice/Resources/beads.bin`. `CustomLMLoader`
there resolves it via `Bundle.module.url(forResource:withExtension:)`.

## YAML format

The parser is deliberately narrow — it understands only the keys and
whitespace the Go emitter in `scripts/build-dictation-corpus/emit.go`
produces, to avoid pulling a YAML dependency into the build phase:

```yaml
locale: "en-US"
identifier: "ai.acme.gardenio.dictation"
version: 1
custom_words:
  - "kbeads"
  - "gardenio-e2e"
phrases:
  - "bead k d dash ABCDEF"
  - "coopmux dashboard"
```

If the corpus pipeline's YAML shape changes, update `PhrasesYAML.parse`
in `Sources/build-dictation-lm/main.swift` in lockstep.
