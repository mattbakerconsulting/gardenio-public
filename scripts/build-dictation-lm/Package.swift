// swift-tools-version: 5.9
// Build-time compiler that reads phrases.yaml and emits beads.bin
// (SFCustomLanguageModelData archive) for SFSpeechRecognizer custom LM.
//
// macOS 14+ only — SFCustomLanguageModelData first shipped in macOS 14 /
// iOS 17. The Xcode build phase in usr/Gardenio invokes this package via
// `swift run`; on older hosts the phase skips gracefully.
//
// Scope lock: Apple Speech framework only. No Whisper, WhisperKit,
// whisper.cpp, Deepgram, or third-party speech engines.
import PackageDescription

let package = Package(
    name: "build-dictation-lm",
    platforms: [
        .macOS(.v14)
    ],
    targets: [
        .executableTarget(
            name: "build-dictation-lm",
            path: "Sources/build-dictation-lm"
        )
    ]
)
