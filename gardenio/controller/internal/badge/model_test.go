package badge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubServer returns an httptest server that answers /v1/messages with a single
// text block containing body, and records the last request body it saw.
func stubServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var lastReq string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		lastReq = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		resp := messagesResp{Content: []contentBlock{{Type: "text", Text: body}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastReq
}

const goodSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><circle cx="50" cy="50" r="40" fill="#2563eb"/></svg>`

func TestGenerate_NilGeneratorFallsBackToTier0(t *testing.T) {
	var g *Generator // disabled
	b, err := g.Generate(context.Background(), "gd-x", "Some Plan", "prose", "")
	if err == nil {
		t.Error("expected an advisory error explaining the fallback")
	}
	if b.Tier != TierDeterministic {
		t.Fatalf("tier = %q, want tier0 fallback", b.Tier)
	}
	if b.SVG != Tier0("gd-x", "Some Plan").SVG {
		t.Error("nil generator did not return the deterministic Tier0 badge")
	}
}

func TestNewGenerator_EmptyKeyDisabled(t *testing.T) {
	if g := NewGenerator(""); g != nil {
		t.Fatal("empty api key should yield a nil (disabled) generator")
	}
}

func TestGenerate_ModelSuccess(t *testing.T) {
	srv, lastReq := stubServer(t, http.StatusOK, "here is your icon:\n```svg\n"+goodSVG+"\n```")
	g := NewGenerator("key", WithBaseURL(srv.URL), WithStyle(StyleLineArtSubtle))
	b, err := g.Generate(context.Background(), "gd-x", "Deep Clones", "clone git repos fast", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Tier != TierModel {
		t.Fatalf("tier = %q, want tier2", b.Tier)
	}
	if !strings.Contains(b.SVG, "<circle") || strings.Contains(b.SVG, "```") {
		t.Errorf("SVG not cleanly extracted from fenced model output: %q", b.SVG)
	}
	// The subtle style directive must have reached the prompt.
	if !strings.Contains(*lastReq, "wayfinding") {
		t.Errorf("subtle style directive missing from request: %s", *lastReq)
	}
}

func TestGenerate_APIErrorFallsBack(t *testing.T) {
	srv, _ := stubServer(t, http.StatusTooManyRequests, "rate limited")
	g := NewGenerator("key", WithBaseURL(srv.URL))
	b, err := g.Generate(context.Background(), "gd-y", "Title Here", "prose", "")
	if err == nil {
		t.Error("expected an error on non-200")
	}
	if b.Tier != TierDeterministic || b.SVG != Tier0("gd-y", "Title Here").SVG {
		t.Error("API error did not fall back to Tier0")
	}
}

func TestGenerate_JunkOutputFallsBack(t *testing.T) {
	srv, _ := stubServer(t, http.StatusOK, "I'm sorry, I can't draw that.")
	g := NewGenerator("key", WithBaseURL(srv.URL))
	b, err := g.Generate(context.Background(), "gd-z", "Title", "prose", "")
	if err == nil {
		t.Error("expected error when model returns no SVG")
	}
	if b.Tier != TierDeterministic {
		t.Error("non-SVG model output did not fall back to Tier0")
	}
}

func TestSanitizeSVG(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"clean", goodSVG, false},
		{"fenced", "```svg\n" + goodSVG + "\n```", false},
		{"prose wrapped", "Sure!\n" + goodSVG + "\nHope that helps.", false},
		{"no svg", "just some text", true},
		{"script tag", `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><script>alert(1)</script></svg>`, true},
		{"onload handler", `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" onload="x()"></svg>`, true},
		{"external image", `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><image href="https://evil/x.png"/></svg>`, true},
		{"external href", `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><a href="https://evil"></a></svg>`, true},
		{"wrong viewbox", `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle/></svg>`, true},
		{"missing viewbox", `<svg xmlns="http://www.w3.org/2000/svg"><circle/></svg>`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := sanitizeSVG(c.in, maxSVGBytes)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got clean svg: %q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.HasPrefix(out, "<svg") {
				t.Errorf("sanitized output not a bare svg: %q", out)
			}
		})
	}
}

func TestSanitizeSVG_RejectsOversize(t *testing.T) {
	big := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">` +
		strings.Repeat(`<circle cx="1" cy="1" r="1"/>`, 1000) + `</svg>`
	if _, err := sanitizeSVG(big, maxSVGBytes); err == nil {
		t.Error("expected oversize SVG to be rejected")
	}
	// The same ~30 KB is past the poster cap too; a ~12 KB poster is not.
	if _, err := sanitizeSVG(big, maxPosterSVGBytes); err == nil {
		t.Error("expected a ~30 KB SVG to be rejected at the poster cap")
	}
	mid := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">` +
		strings.Repeat(`<circle cx="1" cy="1" r="1"/>`, 400) + `</svg>`
	if _, err := sanitizeSVG(mid, maxSVGBytes); err == nil {
		t.Error("expected a ~12 KB SVG to be rejected at the line-art cap")
	}
	if _, err := sanitizeSVG(mid, maxPosterSVGBytes); err != nil {
		t.Errorf("a ~12 KB poster was rejected: %v", err)
	}
}

// A poster asks for more: the poster prompt, the larger token cap, and the
// larger byte cap, so a ~12 KB layered picture is kept rather than dropped to
// Tier-0.
func TestGenerate_PosterStyle(t *testing.T) {
	poster := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">` +
		strings.Repeat(`<circle cx="1" cy="1" r="1"/>`, 400) + `</svg>`
	var req messagesReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messagesResp{Content: []contentBlock{{Type: "text", Text: poster}}})
	}))
	defer srv.Close()

	g := NewGenerator("key", WithBaseURL(srv.URL), WithStyle(StylePoster))
	b, err := g.Generate(context.Background(), "gd-p", "Deep Clones", "clone git repos fast", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Tier != TierModel {
		t.Fatalf("tier = %q, want tier2 (a ~12 KB poster is under the poster cap)", b.Tier)
	}
	if req.MaxTokens != posterMaxTokens {
		t.Errorf("max_tokens = %d, want %d", req.MaxTokens, posterMaxTokens)
	}
	prompt := req.Messages[0].Content
	if !strings.Contains(prompt, "Poster art") || !strings.Contains(prompt, "128px and above") {
		t.Errorf("poster prompt missing its directive or size rule:\n%s", prompt)
	}
	if strings.Contains(prompt, "24px") {
		t.Errorf("poster prompt still carries the 24px rule:\n%s", prompt)
	}
	if g.client.Timeout != RequestTimeout(StylePoster) {
		t.Errorf("client timeout = %v, want %v", g.client.Timeout, RequestTimeout(StylePoster))
	}
}

func TestGenerate_ContextCancel(t *testing.T) {
	srv, _ := stubServer(t, http.StatusOK, goodSVG)
	g := NewGenerator("key", WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call
	b, err := g.Generate(ctx, "gd-c", "Cancelled", "prose", "")
	if err == nil {
		t.Error("expected error on cancelled context")
	}
	if b.Tier != TierDeterministic {
		t.Error("cancelled call did not fall back to Tier0")
	}
}

func TestGenerate_OptionsApplied(t *testing.T) {
	// WithModel + WithHTTPClient must reach the request: assert the model id
	// travels in the body and the injected client is the one used.
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req messagesReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messagesResp{Content: []contentBlock{{Type: "text", Text: goodSVG}}})
	}))
	defer srv.Close()

	g := NewGenerator("key",
		WithBaseURL(srv.URL),
		WithModel("custom-model-x"),
		WithStyle(""), // empty style must be ignored (keeps default)
		WithHTTPClient(srv.Client()),
	)
	b, err := g.Generate(context.Background(), "gd-o", "Opt Plan", "prose", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Tier != TierModel {
		t.Fatalf("tier = %q, want tier2", b.Tier)
	}
	if gotModel != "custom-model-x" {
		t.Errorf("WithModel not applied: request model = %q", gotModel)
	}
	if g.style != StyleLineArtBold {
		t.Errorf("empty WithStyle should keep default, got %q", g.style)
	}
}

func TestStyleDirective(t *testing.T) {
	if !strings.Contains(styleDirective(StyleLineArtSubtle), "wayfinding") {
		t.Error("subtle directive wrong")
	}
	if !strings.Contains(styleDirective(StyleLineArtBold), "sticker") {
		t.Error("bold directive wrong")
	}
	if !strings.Contains(styleDirective(StylePoster), "silhouette") {
		t.Error("poster directive wrong")
	}
	if !strings.Contains(styleDirective("nonsense"), "sticker") {
		t.Error("unknown style should default to bold")
	}
}
