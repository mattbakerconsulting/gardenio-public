---
name: video
description: >
  Analyze video files by extracting frames with ffmpeg and optionally transcribing
  audio with whisper-cli. Uses sub-agent vision analysis for context-efficient
  processing — images never enter the main conversation.
allowed-tools: "Bash,Read,Write,Glob,Grep,Agent"
version: "1.0.0"
author: "gardenio"
---

# Video Analysis Skill

Analyze video content by extracting frames with ffmpeg, optionally transcribing audio
with whisper-cli, and synthesizing timestamped summaries via sub-agent vision analysis.

## Usage

```
/video <path-to-video> [options]
```

**Arguments:**
- `$1` — Path to the video file (required)
- Additional arguments parsed as options:
  - `--no-audio` — Skip audio transcription
  - `--frames-per-second N` — Override frame extraction rate (default: auto based on duration)
  - `--max-frames N` — Cap total frames extracted (default: 60)

## Prerequisites

Verify tools are available (both are pre-installed in the gardenio agent image):

```bash
which ffmpeg ffprobe whisper-cli
```

All three ship in the standard agent image (every agent role — gardener, crew,
`fsm-triage` — runs it): `ffmpeg`/`ffprobe` come from the `ffmpeg` apt package in
`AGENT_PACKAGES` (`gardenio/images/agent/install/versions.env`); `whisper-cli` is
compiled from whisper.cpp in the `whisper-builder` Docker stage. No pod-spec change
or extra package install is required.

## Invocation from fsm-triage

`fsm-triage` agents use this skill to satisfy §7 repro on FSM tickets that ship a
customer-recorded video/audio attachment (e.g. GBFSM-137 / FSM-542, a 221.8 MB
video). Flow: download the JIRA attachment into the pod (authenticated as
`gardenio2@` via the `jira2-api-token`), then run `/video <path>` to get keyframes
+ a timestamped transcript as repro-reasoning inputs.

Pod constraints to respect:

- **Memory**: the `fsm-triage` VPA class is bounded `1Gi`–`4Gi`
  (`gardenio/helm/gardenio/values.yaml`). whisper-cli's `ggml-base.en` model needs
  ~142 MB RAM; keep frame batches small. For a p95 ~500 MB video, transcode/downsample
  first (`ffmpeg -i in.mp4 -vf scale=1280:-2 -r 15 small.mp4`) before frame extraction.
- **Ephemeral disk**: work under `/tmp` (node-ephemeral, wiped on restart). Clean up
  `$WORK_DIR` (Phase 6) — a 500 MB video plus extracted frames can exceed the pod's
  ephemeral quota if left around.
- **Whisper model download**: the model is fetched from Hugging Face into
  `/tmp/whisper-models/` on first use and does NOT survive a pod restart. If the pod
  has no egress to `huggingface.co`, transcription is skipped (visual-only analysis
  still works); see the error-handling notes below.

## Phase 1: Metadata Extraction

Extract video metadata to determine the analysis strategy:

```bash
WORK_DIR="/tmp/video-analysis-$(date +%s)"
mkdir -p "$WORK_DIR/frames" "$WORK_DIR/analysis"

# Get metadata as JSON
ffprobe -v quiet -print_format json -show_format -show_streams "$VIDEO_PATH" > "$WORK_DIR/metadata.json"
```

Read `metadata.json` and extract:
- **Duration** (seconds)
- **Resolution** (width x height)
- **FPS** (r_frame_rate)
- **Codec** (codec_name)
- **Has audio** (check for audio stream)

## Phase 2: Frame Extraction

Choose extraction rate based on video duration:

| Duration | Strategy | Rationale |
|----------|----------|-----------|
| 0-30s | 1 frame/second | Short enough for dense sampling |
| 30s-2min | 1 frame every 2 seconds | Balance detail vs frame count |
| 2-10min | Scene detection (`-vf "select=gt(scene\,0.3)"`) | Only capture scene changes |
| 10-30min | Keyframes (`-vf "select=eq(pict_type\,I)"`) | Sparse but representative |
| 30min+ | Thumbnail filter, cap at 60 frames | Prevent frame explosion |

```bash
# Example: 1 frame every 2 seconds
ffmpeg -i "$VIDEO_PATH" -vf "fps=0.5" -q:v 2 -f image2 "$WORK_DIR/frames/frame_%04d.jpg" 2>/dev/null

# Example: Scene detection
ffmpeg -i "$VIDEO_PATH" -vf "select=gt(scene\,0.3)" -vsync vfr -q:v 2 "$WORK_DIR/frames/frame_%04d.jpg" 2>/dev/null
```

After extraction, count frames and verify:
```bash
FRAME_COUNT=$(ls "$WORK_DIR/frames/"*.jpg 2>/dev/null | wc -l)
```

If frame count exceeds `--max-frames` (default 60), downsample by selecting evenly-spaced frames.

Generate a frame manifest with timestamps:
```bash
# Map frame numbers back to timestamps based on extraction rate
# Write to $WORK_DIR/frames/manifest.txt with format: frame_0001.jpg 00:00:02
```

## Phase 3: Audio Transcription (Optional)

Skip if `--no-audio` flag is set or no audio stream exists.

```bash
# Extract audio as 16kHz mono WAV (whisper-cli requirement)
ffmpeg -i "$VIDEO_PATH" -ar 16000 -ac 1 -f wav "$WORK_DIR/audio.wav" 2>/dev/null

# Download whisper model if not cached
WHISPER_MODEL_DIR="/tmp/whisper-models"
WHISPER_MODEL="$WHISPER_MODEL_DIR/ggml-base.en.bin"
if [ ! -f "$WHISPER_MODEL" ]; then
  mkdir -p "$WHISPER_MODEL_DIR"
  curl -fsSL "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.en.bin" \
    -o "$WHISPER_MODEL"
fi

# Transcribe with timestamps
whisper-cli -m "$WHISPER_MODEL" -f "$WORK_DIR/audio.wav" --output-srt --output-file "$WORK_DIR/transcript" -t 4
```

The transcript SRT file (`$WORK_DIR/transcript.srt`) provides timestamped text segments.

See [WHISPER_MODELS.md](resources/WHISPER_MODELS.md) for model selection guidance.

## Phase 4: Sub-Agent Vision Analysis

**Critical**: Frame images must ONLY be read inside sub-agent contexts. The main agent
never reads image files directly — this keeps the main conversation context lean.

Batch frames into groups of 8-10 and dispatch sub-agents in parallel:

```
For each batch of frames:
  Launch an Agent (subagent_type: general-purpose) with prompt:
    "Analyze these video frames and write a detailed description to a file.
     Read each frame image, then write your analysis to $WORK_DIR/analysis/batch_N.md

     Frames to analyze: [list of frame paths with timestamps from manifest]

     For each frame, describe:
     - Timestamp and frame number
     - Scene content (people, objects, text, environment)
     - Actions or movement (what's happening)
     - Notable details (UI elements, text overlays, transitions)
     - Relationship to adjacent frames (continuity, scene changes)

     Write output as markdown with one section per frame, using the timestamp as heading."
```

Wait for all sub-agents to complete, then verify analysis files exist:
```bash
ls "$WORK_DIR/analysis/"*.md
```

## Phase 5: Synthesis

Read all analysis files and the transcript (if available), then compile the final report.

**Output format:**

```markdown
## Video Analysis: <filename>

### Metadata
| Property | Value |
|----------|-------|
| Duration | MM:SS |
| Resolution | WxH |
| FPS | N |
| Codec | name |
| Audio | yes/no |

### Timeline

#### [00:00 - 00:15] Scene Title
Description of what happens in this segment, combining visual analysis
with transcript content where available.

#### [00:15 - 00:32] Scene Title
...

### Key Moments
1. **[00:05]** — Description of significant moment
2. **[00:23]** — Description of significant moment
3. **[01:15]** — Description of significant moment

### Transcript Highlights
> [00:05] "Notable quote from audio..."
> [00:45] "Another relevant quote..."

### Summary
2-5 sentence narrative summary of the video content, purpose, and key takeaways.
```

## Phase 6: Cleanup

Remove temporary directory unless the user explicitly requests frame preservation:
```bash
rm -rf "$WORK_DIR"
```

## Error Handling

- **ffmpeg not found**: Print error and exit — cannot proceed without it
- **whisper-cli not found**: Warn and skip audio transcription (visual-only analysis)
- **No frames extracted**: Try alternative extraction method (keyframes instead of scene detection)
- **Whisper model download fails**: Warn and skip transcription
- **Sub-agent failure**: Log which batch failed, proceed with partial results
- **Video file not found/unreadable**: Print clear error with path
