package advice

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// render_test.go proves the R4 render-demand measure (ka-0000002748): the
// gardenio_advice_prime_render_bytes gauge the tripwire watches is derived from
// AbbrevRenderBytes, so these tests pin (a) that the measure crosses the 48000-
// byte (80% of 60000) threshold when a real archetype's abbreviated pool grows
// large, (b) that it stays comfortably under at a small pool ("quiet at today's
// levels"), and (c) that inline beads — which bypass the abbreviated budget in
// gb prime — are excluded from the demand.

const r4WarnBytes = 48000 // 80% of advice.DefaultRenderByteBudget (60000)

// syntheticAdvice builds a non-inline global-scope MatchedAdvice whose body is
// bodyChars runes long, so AbbrevRenderBytes renders a full ~summaryMax excerpt
// per item — the heaviest realistic abbreviated form.
func syntheticAdvice(id string, bodyChars int) MatchedAdvice {
	return MatchedAdvice{
		Bead: &beadsapi.BeadDetail{
			ID:          id,
			Title:       "Synthetic advice bead for render-demand sizing " + id,
			Description: strings.Repeat("word ", bodyChars/5),
			Labels:      []string{"scope:global"},
		},
		Scope: "global",
	}
}

func buildPool(n, bodyChars int) []MatchedAdvice {
	pool := make([]MatchedAdvice, 0, n)
	for i := 0; i < n; i++ {
		// IDs vary so nothing dedups; length is representative of real kd-ids.
		pool = append(pool, syntheticAdvice("gd-"+strings.Repeat("x", 8)+itoa(i), bodyChars))
	}
	return pool
}

// itoa avoids importing strconv for a one-liner; ids only need to differ.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestAbbrevRenderBytes_SmallPoolQuiet(t *testing.T) {
	// A small pool (today's-levels stand-in) must sit well under the 48000-byte
	// tripwire so the alert stays quiet at healthy volumes.
	got := AbbrevRenderBytes(buildPool(20, DefaultSummaryMaxChars), DefaultSummaryMaxChars)
	if got >= r4WarnBytes {
		t.Errorf("small pool render demand = %d bytes, want < %d (tripwire must stay quiet)", got, r4WarnBytes)
	}
	if got == 0 {
		t.Errorf("small non-inline pool must render some bytes, got 0")
	}
}

func TestAbbrevRenderBytes_LargePoolCrossesTripwire(t *testing.T) {
	// A large pool of full-excerpt items must cross the 48000-byte tripwire, so
	// the render-demand gauge the R4 alert watches actually fires before silent
	// truncation can recur.
	got := AbbrevRenderBytes(buildPool(120, DefaultSummaryMaxChars), DefaultSummaryMaxChars)
	if got <= r4WarnBytes {
		t.Errorf("large pool render demand = %d bytes, want > %d (tripwire must fire)", got, r4WarnBytes)
	}
}

func TestAbbrevRenderBytes_Monotonic(t *testing.T) {
	// Demand must grow with pool size — the property that makes the gauge a valid
	// leading indicator of budget pressure.
	small := AbbrevRenderBytes(buildPool(10, DefaultSummaryMaxChars), DefaultSummaryMaxChars)
	large := AbbrevRenderBytes(buildPool(40, DefaultSummaryMaxChars), DefaultSummaryMaxChars)
	if large <= small {
		t.Errorf("render demand not monotonic in pool size: 10-item=%d, 40-item=%d", small, large)
	}
}

func TestAbbrevRenderBytes_ExcludesInline(t *testing.T) {
	// Inline beads bypass the abbreviated budget in gb prime, so they must not
	// count toward the render demand the tripwire watches.
	pool := buildPool(5, DefaultSummaryMaxChars)
	base := AbbrevRenderBytes(pool, DefaultSummaryMaxChars)

	// Append several inline beads; demand must not change.
	for i := 0; i < 50; i++ {
		inline := syntheticAdvice("gd-inline"+itoa(i), DefaultSummaryMaxChars)
		inline.Bead.Labels = append(inline.Bead.Labels, "inline:true")
		pool = append(pool, inline)
	}
	withInline := AbbrevRenderBytes(pool, DefaultSummaryMaxChars)
	if withInline != base {
		t.Errorf("inline beads must be excluded from render demand: base=%d, with 50 inline=%d", base, withInline)
	}

	// A pool of ONLY inline beads renders zero abbreviated demand.
	onlyInline := make([]MatchedAdvice, 0, 10)
	for i := 0; i < 10; i++ {
		b := syntheticAdvice("gd-only"+itoa(i), DefaultSummaryMaxChars)
		b.Bead.Labels = append(b.Bead.Labels, "inline:true")
		onlyInline = append(onlyInline, b)
	}
	if got := AbbrevRenderBytes(onlyInline, DefaultSummaryMaxChars); got != 0 {
		t.Errorf("all-inline pool must render 0 abbreviated bytes, got %d", got)
	}
}

func TestAbbrevRenderBytes_SkipsNilBead(t *testing.T) {
	// A nil bead in the matched slice must be skipped, not panic.
	pool := []MatchedAdvice{{Bead: nil, Scope: "global"}}
	if got := AbbrevRenderBytes(pool, DefaultSummaryMaxChars); got != 0 {
		t.Errorf("nil bead must contribute 0 bytes, got %d", got)
	}
}
