package badge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The prompt goldens pin the exact text the model is sent (gd-jo06g2WlQ8).
// prompt-no-arms.golden.txt was captured from the prompt as it stood before
// arms were fed in, so TestPrompt_Golden failing on it means an armless
// bead's badge prompt changed. Regenerate after an INTENTIONAL prompt change:
//
//	GB_UPDATE_GOLDEN=1 go test ./internal/badge/ -run TestPrompt_Golden
//
// then review the diff in the .golden.txt files before committing.

const (
	promptFixtureTitle = "Deep Clones"
	promptFixtureProse = "Clone git repos fast so a new agent pod starts working in seconds."
	// promptFixtureArms is plan gd-pQZO9ie11D's arms.
	promptFixtureArms = "🏅🖌️🔎"
)

var promptGoldenFixtures = []struct {
	name  string
	style string
	arms  string
}{
	{"prompt-no-arms", StyleLineArtBold, ""},
	{"prompt-arms", StyleLineArtBold, promptFixtureArms},
	{"prompt-poster-no-arms", StylePoster, ""},
	{"prompt-poster-arms", StylePoster, promptFixtureArms},
}

func TestPrompt_Golden(t *testing.T) {
	update := os.Getenv("GB_UPDATE_GOLDEN") != ""
	for _, f := range promptGoldenFixtures {
		t.Run(f.name, func(t *testing.T) {
			got := buildPrompt(f.style, promptFixtureTitle, promptFixtureProse, f.arms)
			path := filepath.Join(goldenDir, f.name+".golden.txt")
			if update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (set GB_UPDATE_GOLDEN=1 to create it)", path, err)
			}
			if got != string(want) {
				t.Errorf("prompt drifted from %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}
}

// TestBuildPrompt_ArmsForms: the legacy six-token shield form a bead stored
// before step 5.6 rewrites it reads as the same charges, so it gives the same
// prompt as the run; spaces in the run are allowed; arms that do not parse
// fall back to the armless prompt rather than failing the badge.
func TestBuildPrompt_ArmsForms(t *testing.T) {
	run := buildPrompt(StyleLineArtBold, promptFixtureTitle, promptFixtureProse, promptFixtureArms)
	none := buildPrompt(StyleLineArtBold, promptFixtureTitle, promptFixtureProse, "")
	for _, c := range []struct {
		name, stored, want string
	}{
		{"legacy shield form", "brown 🏅 . 🖌️ . 🔎", run},
		{"spaced run", "🏅 🖌️ 🔎", run},
		{"not in the library", "🏅🖌️zz", none},
		{"blank", "   ", none},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := buildPrompt(StyleLineArtBold, promptFixtureTitle, promptFixtureProse, c.stored); got != c.want {
				t.Errorf("buildPrompt(%q) =\n%s\nwant\n%s", c.stored, got, c.want)
			}
		})
	}
}

// recordingServer answers /v1/messages with goodSVG and records the decoded
// request and its raw body.
func recordingServer(t *testing.T) (*httptest.Server, *messagesReq, *map[string]any) {
	t.Helper()
	var req messagesReq
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.Unmarshal(body, &req)
		_ = json.Unmarshal(body, &raw)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messagesResp{Content: []contentBlock{
			{Type: "thinking"},
			{Type: "text", Text: goodSVG},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv, &req, &raw
}

func TestGenerate_ArmsReachRequest(t *testing.T) {
	srv, req, raw := recordingServer(t)
	g := NewGenerator("key", WithBaseURL(srv.URL))
	b, err := g.Generate(context.Background(), "gd-a", promptFixtureTitle, promptFixtureProse, promptFixtureArms)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Tier != TierModel {
		t.Fatalf("tier = %q, want tier2 (the text block after a thinking block is the reply)", b.Tier)
	}

	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want exactly one user message (no assistant prefill)", req.Messages)
	}
	prompt := req.Messages[0].Content
	for _, want := range []string{
		"Arms: " + promptFixtureArms,
		"1. sports medal",
		"2. paintbrush",
		"3. magnifying glass tilted right",
		"Title: " + promptFixtureTitle,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("request prompt missing %q:\n%s", want, prompt)
		}
	}

	if req.Model != DefaultModel {
		t.Errorf("model = %q, want DefaultModel %q", req.Model, DefaultModel)
	}
	if req.MaxTokens != maxTokens {
		t.Errorf("max_tokens = %d, want %d", req.MaxTokens, maxTokens)
	}
	if _, ok := (*raw)["thinking"]; ok {
		t.Errorf("request sets thinking; it must be left at the model's default: %v", *raw)
	}
}

func TestGenerate_NoArmsRequestUnchanged(t *testing.T) {
	srv, req, _ := recordingServer(t)
	g := NewGenerator("key", WithBaseURL(srv.URL))
	if _, err := g.Generate(context.Background(), "gd-n", promptFixtureTitle, promptFixtureProse, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(goldenDir, "prompt-no-arms.golden.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content != string(want) {
		t.Errorf("armless request prompt is not today's text:\n%+v", req.Messages)
	}
}

// promptVersionFingerprints pins a fingerprint of every prompt buildPrompt
// writes to the PromptVersion it was cached under. A prompt change with no
// bump would serve the old picture from the cache, so a fingerprint with no
// pin for the current PromptVersion fails. After an intentional prompt change,
// bump PromptVersion and add its fingerprint here; keep the old rows.
var promptVersionFingerprints = map[int]string{
	1: "63b300a6a4bd509c8038728a45ad24bc987d06bd0af58469308cdec74e2ea0d1",
}

// promptFingerprint is the SHA-256 of buildPrompt for every style, with and
// without arms, over the golden fixtures.
func promptFingerprint() string {
	h := sha256.New()
	for _, style := range Styles {
		for _, arms := range []string{"", promptFixtureArms} {
			h.Write([]byte(buildPrompt(style, promptFixtureTitle, promptFixtureProse, arms)))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestPromptVersionPinsPrompts(t *testing.T) {
	got := promptFingerprint()
	want, ok := promptVersionFingerprints[PromptVersion]
	if !ok || got != want {
		t.Fatalf("the badge prompts changed but PromptVersion is still %d, so the cache would serve pictures drawn from the old prompts.\n"+
			"Bump PromptVersion in model.go and pin the new prompts: promptVersionFingerprints[%d] = %q", PromptVersion, PromptVersion+1, got)
	}
}
