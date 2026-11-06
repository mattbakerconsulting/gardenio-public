package gardenermode

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestStaggeredCron_Deterministic(t *testing.T) {
	a := StaggeredCron("gd-gardenerAlpha", 30*time.Minute)
	b := StaggeredCron("gd-gardenerAlpha", 30*time.Minute)
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
}

func TestStaggeredCron_30mShape(t *testing.T) {
	got := StaggeredCron("gd-gardenerAlpha", 30*time.Minute)
	// Shape: "M,M+30 * * * *" with 0 <= M < 30.
	var m1, m2 int
	if _, err := fmtSscanf(got, &m1, &m2); err != nil {
		t.Fatalf("bad shape %q: %v", got, err)
	}
	if m1 < 0 || m1 >= 30 || m2 != m1+30 {
		t.Fatalf("bad minutes in %q", got)
	}
}

// fmtSscanf isolates the parse so the shape test reads clearly.
func fmtSscanf(s string, m1, m2 *int) (int, error) {
	n, err := sscanf(s, m1, m2)
	return n, err
}

func sscanf(s string, m1, m2 *int) (int, error) {
	var rest string
	n, err := fmt.Sscanf(s, "%d,%d %s", m1, m2, &rest)
	_ = rest
	return n, err
}

func TestStaggeredCron_6hShape(t *testing.T) {
	got := StaggeredCron("gd-gardenerAlpha", 6*time.Hour)
	// Shape: "M H,H+6,H+12,H+18 * * *" with 0<=M<60, 0<=H<6.
	if !strings.HasSuffix(got, " * * *") {
		t.Fatalf("bad shape %q", got)
	}
	parts := strings.SplitN(got, " ", 3)
	hours := strings.Split(parts[1], ",")
	if len(hours) != 4 {
		t.Fatalf("want 4 hour slots in %q", got)
	}
}

func TestStaggeredCron_DailyShape(t *testing.T) {
	got := StaggeredCron("gd-gardenerAlpha", 24*time.Hour)
	// Shape: "M H * * *" with 0<=M<60, 0<=H<24 — one fire a day.
	var m, h int
	var rest string
	if _, err := fmt.Sscanf(got, "%d %d %s", &m, &h, &rest); err != nil {
		t.Fatalf("bad shape %q: %v", got, err)
	}
	if m < 0 || m >= 60 || h < 0 || h >= 24 || !strings.HasSuffix(got, fmt.Sprintf("%d %d * * *", m, h)) {
		t.Fatalf("bad daily cron %q", got)
	}
	if again := StaggeredCron("gd-gardenerAlpha", 24*time.Hour); again != got {
		t.Fatalf("not deterministic: %q vs %q", got, again)
	}
}

// beadIDCorpus returns n deterministically generated bead ids of the real
// shape (`gd-` + 10 alphanumerics). The seed is FIXED, so every run sees the
// identical corpus: the statistical assertions below are deterministic and can
// never flake, while still describing thousands of ids rather than a handful.
func beadIDCorpus(seed int64, n int) []string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	r := rand.New(rand.NewSource(seed))
	ids := make([]string, n)
	var b strings.Builder
	for i := range ids {
		b.Reset()
		b.WriteString("gd-")
		for j := 0; j < 10; j++ {
			b.WriteByte(alphabet[r.Intn(len(alphabet))])
		}
		ids[i] = b.String()
	}
	return ids
}

// TestStaggeredCron_SpreadsFleetAcrossMinutes replaces an earlier test that
// pinned 14 production bead ids and asserted that those particular 14 did not
// collide on a fire minute. That asserted a property of the SAMPLE, not of the
// code: 14 ids into 30 minute slots contain a triple collision ~29% of the
// time purely by the birthday effect, so the old green was luck and it turned
// red the moment the id corpus changed. It could not have caught a regression.
//
// The invariant the stagger actually exists for is SPREAD: the hash must
// scatter gardeners across the available fire minutes so a shared cron default
// can never produce a synchronized spawn herd.
func TestStaggeredCron_SpreadsFleetAcrossMinutes(t *testing.T) {
	t.Run("uniform across the whole minute space", func(t *testing.T) {
		// A 30m period yields K = 30 minute slots. With N ids the expected
		// occupancy is mu = N/K, and per-slot occupancy is Binomial(N, 1/K),
		// so sigma = sqrt(N*(1/K)*(1-1/K)).
		//   N = 3000, K = 30  =>  mu = 100, sigma = sqrt(3000*(1/30)*(29/30)) ~= 9.8
		// The bound below is mu +/- 50, i.e. +/-5.1 sigma. A correct hash
		// breaches that with probability ~1e-5 even before the fixed seed is
		// taken into account, which makes this deterministic outright.
		// A real regression is nowhere near the line: collapsing to a constant
		// slot puts all 3000 ids in one bin (30x mu) and empties the other 29.
		const (
			n      = 3000
			slots  = 30
			mean   = n / slots // 100
			maxOcc = mean + 50 // 150
			minOcc = mean - 50 // 50
		)
		occ := map[string]int{}
		for _, id := range beadIDCorpus(1, n) {
			occ[StaggeredCron(id, 30*time.Minute)]++
		}
		if len(occ) != slots {
			t.Errorf("got %d distinct fire minutes, want all %d occupied", len(occ), slots)
		}
		for cron, count := range occ {
			if count > maxOcc || count < minOcc {
				t.Errorf("slot %q holds %d of %d ids, want within [%d,%d] (mu=%d)",
					cron, count, n, minOcc, maxOcc, mean)
			}
		}
	})

	t.Run("spread does not depend on character order", func(t *testing.T) {
		// Real bead ids are not random noise. The ka- archive namespace is
		// zero-padded sequential digits, so ids that are PERMUTATIONS of one
		// another are common (ka-0000001271 vs ka-0000001721 vs ...). A hash
		// that folds bytes order-insensitively -- a plain byte sum, say --
		// maps every permutation of a multiset onto ONE minute, which is a
		// spawn herd built from exactly the id shape this repo uses.
		//
		// FNV-1a scatters them, but only across HALF the slots, and that is
		// worth stating explicitly because it is a real property of the
		// production hash rather than an accident of this corpus:
		//
		//   FNV-1a is `h ^= b; h *= 16777619`. The prime is odd, and
		//   multiplying by an odd number mod 2^32 preserves the low bit, so
		//   the hash's low bit is just the XOR of every input byte's low bit
		//   -- which is permutation-invariant. 30 is even, so h%30 inherits
		//   that parity. Permutations of one multiset therefore always land
		//   on slots of a single parity: 15 reachable minutes, not 30.
		//
		// So the ceiling here is 15, not 30. Uniform over those 15 slots,
		// 200 draws fill essentially all of them
		// (E[distinct] = 15*(1-(14/15)^200) ~= 15.0). The bound at 12 sits
		// just under that and far above an order-blind hash, which scores 1.
		const (
			perms     = 200
			wantSlots = 12 // of the 15 reachable; see parity note above
		)
		r := rand.New(rand.NewSource(3))
		digits := []byte("0123456789")
		seen := map[string]bool{}
		slots := map[string]bool{}
		for len(seen) < perms {
			r.Shuffle(len(digits), func(i, j int) { digits[i], digits[j] = digits[j], digits[i] })
			id := "gd-" + string(digits)
			if seen[id] {
				continue
			}
			seen[id] = true
			slots[StaggeredCron(id, 30*time.Minute)] = true
		}
		if len(slots) < wantSlots {
			t.Errorf("%d permutations of one digit multiset landed on only %d distinct minutes, want >= %d "+
				"(order-sensitive hash reaches ~15 of 30 by parity; an order-blind hash scores 1)",
				perms, len(slots), wantSlots)
		}
	})

	t.Run("realistic fleet lands on many distinct minutes", func(t *testing.T) {
		// The operational question: how many distinct fire minutes does a real
		// fleet occupy? For f ids into K slots a uniform hash gives
		//   E[distinct] = K * (1 - ((K-1)/K)^f)
		//   f = 14, K = 30  =>  30 * (1 - (29/30)^14) ~= 11.3
		// Averaged over 2000 independent fleets the sample mean is tight
		// (std error well under 0.05), so 10.5 sits comfortably below
		// expectation yet far above any clumping regression: collapsing to a
		// constant slot scores exactly 1.0.
		const (
			fleets    = 2000
			fleetSize = 14
			wantMean  = 10.5
		)
		ids := beadIDCorpus(2, fleets*fleetSize)
		total := 0
		for i := 0; i < fleets; i++ {
			seen := map[string]bool{}
			for _, id := range ids[i*fleetSize : (i+1)*fleetSize] {
				seen[StaggeredCron(id, 30*time.Minute)] = true
			}
			total += len(seen)
		}
		got := float64(total) / float64(fleets)
		if got < wantMean {
			t.Errorf("mean distinct fire minutes per %d-gardener fleet = %.2f, want >= %.1f "+
				"(uniform hash ~= 11.3; a constant slot scores 1.0)", fleetSize, got, wantMean)
		}
	})
}
