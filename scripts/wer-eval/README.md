# wer-eval

Word Error Rate harness for the E2 dictation evaluation
(ka-0000008259, parent ka-0000000044).

## Purpose

E2's exit criterion is **measurable WER drop** when dictating the
held-out set with the custom language model (`beads.bin`) versus the
E1 glossary-bias baseline. This tool consumes transcripts recorded
during the eval, computes word-level WER, and emits a markdown report
that can be posted as an artifact comment on the parent epic.

## Scope lock

Apple Speech framework only. The tool does not invoke any STT engine
itself — it only scores transcripts produced by `SFSpeechRecognizer`
on the Catalyst/iOS app (`usr/Gardenio/`). No Whisper, WhisperKit,
whisper.cpp, Deepgram, AssemblyAI, or cloud STT.

## Dataset shape

JSON. One entry per held-out phrase; one hypothesis per (voice,
condition) cell. Conditions must include at minimum `glossary` and
`custom_lm` for the exit-criterion banner to appear.

```json
{
  "conditions": ["no_bias", "glossary", "custom_lm"],
  "voices":     ["voice1", "voice2", "voice3"],
  "items": [
    {
      "id":        "ka-0000000575",
      "reference": "ka-0000000575",
      "hypotheses": {
        "voice1": {
          "no_bias":   "k dash tee zero six end one six and jay i v",
          "glossary":  "ka-0000000575",
          "custom_lm": "ka-0000000575"
        }
      }
    }
  ]
}
```

Bead ID references should match the corpus-pipeline spelling
templates (see `scripts/build-dictation-corpus/ids.go`).

## Usage

```sh
go run ./scripts/wer-eval \
  -input  eval/wer-eval.json \
  -output eval/wer-report.md
```

The report contains aggregate WER per condition, per-item WER for each
(voice, condition) cell, and an explicit **E2 exit criterion** banner
that says whether `custom_lm` beat `glossary`.

## Recording procedure

1. Pick 50 beads titles (prefer IDs + component names — the places
   stock recognition regresses).
2. Have 2–3 voices read each title aloud, once per condition.
   Toggle bias state via the Gardenio Settings tab:
     - `no_bias`: glossary-bias toggle **off**, no `beads.bin` bundled
     - `glossary`: glossary-bias on, no `beads.bin` bundled (E1 baseline)
     - `custom_lm`: glossary-bias on, `beads.bin` bundled (E2 target)
3. Transcribe each recording via the app and paste into the
   corresponding hypothesis cell.
4. Run `wer-eval` and attach the report to the parent epic.

## Tests

```sh
go test ./scripts/wer-eval/...
```

Covers:
- word-level Levenshtein edit distance (insert / delete / substitute)
- tokenizer (punctuation stripping + lowercasing)
- aggregate WER + exit-delta calculation
- report-writer banner output
