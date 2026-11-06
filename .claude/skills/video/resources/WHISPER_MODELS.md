# Whisper Model Selection

whisper-cli (whisper.cpp) requires a GGML-format model file. Models are downloaded
on first use and cached at `/tmp/whisper-models/`.

## Available Models

| Model | Size | Speed | Quality | Languages | Best For |
|-------|------|-------|---------|-----------|----------|
| ggml-tiny.en | ~75MB | Fastest | Basic | English | Quick transcription, low resources |
| ggml-base.en | ~142MB | Fast | Good | English | **Default — good balance of speed/quality** |
| ggml-small | ~466MB | Medium | Better | Multilingual | Non-English content |
| ggml-medium | ~1.5GB | Slow | Best | Multilingual | High-accuracy needs, long-form content |

## Download URLs

All models hosted at Hugging Face:

```
https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-tiny.en.bin
https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin
https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.bin
https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-medium.bin
```

## Selection Guidance

- **English-only content**: Use `ggml-base.en` (default). Upgrade to `ggml-small` only
  if transcription quality is poor.
- **Multilingual content**: Use `ggml-small`. The `.en` models cannot handle non-English.
- **Agent pod constraints**: Prefer smaller models. Agent pods have limited ephemeral
  storage, and model downloads consume bandwidth on every pod restart since `/tmp` is
  ephemeral. The base.en model at 142MB is a good default.
- **Long videos (30min+)**: Consider `ggml-tiny.en` for faster processing. The quality
  difference is acceptable for summarization tasks.

## Runtime Flags

Useful whisper-cli options:

```bash
whisper-cli -m MODEL_PATH -f audio.wav \
  --output-srt                    # SRT subtitle format with timestamps
  --output-file PREFIX            # Output file prefix (adds .srt extension)
  -t 4                            # Thread count (match available CPUs)
  --max-len 80                    # Max chars per subtitle line
  --language auto                 # Auto-detect language (use with multilingual models)
```
