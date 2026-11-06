import Foundation
import Speech

// build-dictation-lm
//
// Reads the phrases.yaml artifact produced by scripts/build-dictation-corpus
// and optionally a pronunciations.json file of custom grapheme→phoneme
// entries, then writes a SFCustomLanguageModelData archive (beads.bin)
// suitable for SFSpeechRecognizer's custom language model path
// (SFSpeechLanguageModel.prepareCustomLanguageModel + request.customizedLanguageModel).
//
// Usage:
//   swift run build-dictation-lm \
//     --input build/dictation/phrases.yaml \
//     --pronunciations scripts/build-dictation-lm/pronunciations.json \
//     --output build/dictation/beads.bin
//
// Either --input or --pronunciations may be omitted; whichever exists is
// used. If both sources are missing the command exits 0 with a skip
// notice, so Xcode build phases and install scripts calling this tool do
// not fail on clean clones that haven't run the corpus pipeline.
//
// Scope lock: Apple Speech framework only.

@main
struct BuildDictationLM {
    static func main() async {
        let args = Array(CommandLine.arguments.dropFirst())
        guard let opts = Options.parse(args) else {
            Options.printUsage()
            exit(2)
        }

        let fm = FileManager.default

        // Load phrases.yaml if present. Missing file is a soft-skip so
        // pronunciation-only builds keep working.
        var phrases = PhrasesDoc()
        if let inputURL = opts.input {
            if fm.fileExists(atPath: inputURL.path) {
                do {
                    phrases = try PhrasesYAML.load(from: inputURL)
                    FileHandle.standardError.write(Data(
                        "[build-dictation-lm] loaded phrases.yaml: identifier=\(phrases.identifier) locale=\(phrases.locale) custom_words=\(phrases.customWords.count) phrases=\(phrases.phrases.count)\n".utf8
                    ))
                } catch {
                    FileHandle.standardError.write(Data(
                        "[build-dictation-lm] error parsing phrases.yaml: \(error)\n".utf8
                    ))
                    exit(1)
                }
            } else {
                FileHandle.standardError.write(Data(
                    "[build-dictation-lm] skipping phrases: not found at \(inputURL.path)\n".utf8
                ))
            }
        }

        // Load pronunciations.json if present. Also a soft-skip on missing
        // file — common on clean clones before anyone authors one.
        var pronunciations: [PronunciationEntry] = []
        if let pronURL = opts.pronunciations {
            if fm.fileExists(atPath: pronURL.path) {
                do {
                    pronunciations = try PronunciationsJSON.load(from: pronURL)
                    FileHandle.standardError.write(Data(
                        "[build-dictation-lm] loaded pronunciations.json: entries=\(pronunciations.count)\n".utf8
                    ))
                } catch {
                    FileHandle.standardError.write(Data(
                        "[build-dictation-lm] error parsing pronunciations.json: \(error)\n".utf8
                    ))
                    exit(1)
                }
            } else {
                FileHandle.standardError.write(Data(
                    "[build-dictation-lm] skipping pronunciations: not found at \(pronURL.path)\n".utf8
                ))
            }
        }

        // If nothing to compile, soft-exit so callers can keep going.
        if phrases.customWords.isEmpty && phrases.phrases.isEmpty && pronunciations.isEmpty {
            FileHandle.standardError.write(Data(
                "[build-dictation-lm] nothing to compile (no phrases, no pronunciations) — skipping\n".utf8
            ))
            exit(0)
        }

        do {
            try fm.createDirectory(
                at: opts.output.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )

            try await compile(doc: phrases, pronunciations: pronunciations,
                              to: opts.output, clientId: opts.clientIdentifier)

            FileHandle.standardError.write(Data(
                "[build-dictation-lm] wrote \(opts.output.path)\n".utf8
            ))
            exit(0)
        } catch {
            FileHandle.standardError.write(Data(
                "[build-dictation-lm] error: \(error)\n".utf8
            ))
            exit(1)
        }
    }

    /// Compile phrases + pronunciations into an SFCustomLanguageModelData
    /// archive at `outputURL`. Uses the DSL initialiser available on
    /// macOS 14+ / iOS 17+.
    static func compile(doc: PhrasesDoc,
                        pronunciations: [PronunciationEntry],
                        to outputURL: URL,
                        clientId: String) async throws {
        let identifier = doc.identifier.isEmpty ? clientId : doc.identifier
        let version = doc.version <= 0 ? "1.0.0" : "\(doc.version).0.0"

        let data = SFCustomLanguageModelData(
            locale: Locale(identifier: doc.locale.isEmpty ? "en-US" : doc.locale),
            identifier: identifier,
            version: version
        ) {
            // Custom words get a high count so the LM boosts them
            // aggressively. They're domain jargon (bead IDs, component
            // names, people handles) that Apple's stock LM has no prior
            // for.
            for word in doc.customWords {
                SFCustomLanguageModelData.PhraseCount(phrase: word, count: 100)
            }
            // Phrases carry real in-corpus n-grams plus spoken-form
            // spelling templates for bead IDs (generated by the corpus
            // pipeline). Lower weight per phrase — they reinforce
            // context, not single-word priors.
            for phrase in doc.phrases {
                SFCustomLanguageModelData.PhraseCount(phrase: phrase, count: 10)
            }
            // Custom pronunciations teach the recognizer how a grapheme
            // sounds (X-SAMPA phonemes). They do NOT boost priors — a
            // pronunciation alone makes "acme" a recognized word, but
            // stock English "fix" still wins on prior. Pair each one
            // with a high-weight PhraseCount so the grapheme actually
            // beats the generic homophone. Count 1000 — chosen higher
            // than ordinary custom_words (100) because these graphemes
            // are fighting stock-LM priors head-on.
            for entry in pronunciations {
                SFCustomLanguageModelData.CustomPronunciation(
                    grapheme: entry.grapheme,
                    phonemes: entry.phonemes
                )
                SFCustomLanguageModelData.PhraseCount(phrase: entry.grapheme, count: 1000)
            }
        }

        try await data.export(to: outputURL)
    }
}

// MARK: - Options

struct Options {
    var input: URL?
    var pronunciations: URL?
    var output: URL
    var clientIdentifier: String

    static func parse(_ args: [String]) -> Options? {
        var input: URL?
        var pronunciations: URL?
        var output: URL?
        var clientId = "ai.acme.gardenio.dictation"
        var i = 0
        while i < args.count {
            let a = args[i]
            switch a {
            case "--input", "-i":
                guard i + 1 < args.count else { return nil }
                input = URL(fileURLWithPath: args[i + 1])
                i += 2
            case "--pronunciations", "-p":
                guard i + 1 < args.count else { return nil }
                pronunciations = URL(fileURLWithPath: args[i + 1])
                i += 2
            case "--output", "-o":
                guard i + 1 < args.count else { return nil }
                output = URL(fileURLWithPath: args[i + 1])
                i += 2
            case "--client-id":
                guard i + 1 < args.count else { return nil }
                clientId = args[i + 1]
                i += 2
            case "-h", "--help":
                return nil
            default:
                FileHandle.standardError.write(Data("unknown arg: \(a)\n".utf8))
                return nil
            }
        }
        guard let output else { return nil }
        guard input != nil || pronunciations != nil else { return nil }
        return Options(input: input, pronunciations: pronunciations,
                       output: output, clientIdentifier: clientId)
    }

    static func printUsage() {
        let msg = """
        usage: build-dictation-lm --output beads.bin \\
                                  [--input phrases.yaml] \\
                                  [--pronunciations pronunciations.json] \\
                                  [--client-id ID]

        Compiles phrases and/or custom pronunciations into a
        SFCustomLanguageModelData archive suitable for
        SFSpeechLanguageModel.prepareCustomLanguageModel.

        At least one of --input or --pronunciations must be provided. If
        a given file does not exist it is soft-skipped so local Debug
        builds and installers don't fail on clean clones.
        """
        FileHandle.standardError.write(Data((msg + "\n").utf8))
    }
}

// MARK: - phrases.yaml parser

struct PhrasesDoc {
    var locale: String = "en-US"
    var identifier: String = ""
    var version: Int = 1
    var customWords: [String] = []
    var phrases: [String] = []
}

/// Minimal parser for the phrases.yaml shape emitted by
/// `scripts/build-dictation-corpus/emit.go`. Deliberately narrow:
/// supports only the keys this pipeline writes, in the exact whitespace
/// style the Go emitter produces. Pulling in YAMS would bloat the build
/// phase for no gain.
///
/// Recognised schema:
///     locale: "en-US"
///     identifier: "ai.acme.gardenio.dictation"
///     version: 1
///     custom_words:
///       - "kbeads"
///       - "gardenio-e2e"
///     phrases:
///       - "bead k d dash ABCDEF"
///       - "coopmux dashboard"
enum PhrasesYAML {
    static func load(from url: URL) throws -> PhrasesDoc {
        let text = try String(contentsOf: url, encoding: .utf8)
        return try parse(text)
    }

    static func parse(_ text: String) throws -> PhrasesDoc {
        var doc = PhrasesDoc()
        enum Section { case none, customWords, phrases }
        var section: Section = .none

        for rawLine in text.split(separator: "\n", omittingEmptySubsequences: false) {
            let line = String(rawLine)
            // strip full-line comments
            if line.trimmingCharacters(in: .whitespaces).hasPrefix("#") { continue }
            if line.trimmingCharacters(in: .whitespaces).isEmpty { continue }

            // List item under the current section.
            if let item = matchListItem(line) {
                switch section {
                case .customWords: doc.customWords.append(item)
                case .phrases: doc.phrases.append(item)
                case .none: break
                }
                continue
            }

            // Top-level key.
            if let (key, value) = matchKeyValue(line) {
                switch key {
                case "locale":
                    doc.locale = unquote(value)
                    section = .none
                case "identifier":
                    doc.identifier = unquote(value)
                    section = .none
                case "version":
                    doc.version = Int(unquote(value)) ?? 1
                    section = .none
                case "custom_words":
                    section = .customWords
                case "phrases":
                    section = .phrases
                default:
                    section = .none
                }
            }
        }

        return doc
    }

    /// `  - "text"` → `text`. Accepts single or double quotes, or bare.
    private static func matchListItem(_ line: String) -> String? {
        let t = line.drop(while: { $0 == " " || $0 == "\t" })
        guard t.hasPrefix("- ") else { return nil }
        let rest = t.dropFirst(2)
        return unquote(String(rest))
    }

    /// `key: value` at top level. Returns nil for indented lines so list
    /// items don't get parsed as keys.
    private static func matchKeyValue(_ line: String) -> (String, String)? {
        if line.first == " " || line.first == "\t" { return nil }
        guard let colon = line.firstIndex(of: ":") else { return nil }
        let key = String(line[..<colon]).trimmingCharacters(in: .whitespaces)
        let afterColon = line.index(after: colon)
        let value = String(line[afterColon...]).trimmingCharacters(in: .whitespaces)
        return (key, value)
    }

    /// Strip one layer of matching quotes + decode JSON-style escapes.
    /// The Go emitter writes double-quoted scalars via json.Marshal, so
    /// JSONDecoder correctly handles escapes.
    private static func unquote(_ s: String) -> String {
        let t = s.trimmingCharacters(in: .whitespaces)
        if t.hasPrefix("\"") && t.hasSuffix("\"") && t.count >= 2 {
            let data = Data(t.utf8)
            if let decoded = try? JSONDecoder().decode(String.self, from: data) {
                return decoded
            }
            return String(t.dropFirst().dropLast())
        }
        if t.hasPrefix("'") && t.hasSuffix("'") && t.count >= 2 {
            return String(t.dropFirst().dropLast())
        }
        return t
    }
}

// MARK: - pronunciations.json parser

/// One grapheme → one or more X-SAMPA phoneme strings. Each phoneme
/// string is a single pronunciation variant ("f I k s" for /fɪks/).
struct PronunciationEntry: Decodable {
    let grapheme: String
    let phonemes: [String]
}

/// Parses a pronunciations.json file shaped as a JSON array of
/// {grapheme, phonemes} objects. JSON chosen over YAML so we can lean on
/// Foundation's JSONDecoder instead of hand-rolling more parser code.
///
///     [
///       {"grapheme": "acme", "phonemes": ["f I k s"]}
///     ]
enum PronunciationsJSON {
    static func load(from url: URL) throws -> [PronunciationEntry] {
        let data = try Data(contentsOf: url)
        let entries = try JSONDecoder().decode([PronunciationEntry].self, from: data)
        // Drop malformed entries defensively — Apple's CustomPronunciation
        // init rejects empty graphemes/phonemes at runtime.
        return entries.filter { !$0.grapheme.isEmpty && !$0.phonemes.isEmpty }
    }
}
