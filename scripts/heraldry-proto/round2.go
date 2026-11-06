package main

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	xdraw "golang.org/x/image/draw"
)

// Round 2 (step 0.4b, gd-jwBIGusQUd): arms for every open epic, scored
// against the rubric (gardenio/docs/heraldry/rubric.md), on R13's eight
// tinctures, with the contrast matrix both ways (Q14) and the 16 px stake at
// 2× and 3× (Q16).

// Entry is one row of round 2's arms.tsv: an open epic and its arms.
type Entry struct {
	Kind     string // plan, phase or epic
	ID       string
	Parent   string // the plan a phase belongs to; "" otherwise
	Short    string
	Arms     Arms
	Tincture string // a palette name; "auto" (R13's default) or "plan" (the plan's, rubric S6) on input
	TReason  string // why this tincture, or "default"
	Gloss    map[string]string
	Reason   string
	Round1   string // kept, revised or new

	TinctureNote string           // how an auto or plan tincture was resolved
	Grades       map[string]Grade // check → grade
	G2Raw        Grade            // G2 without the keyline (Q14 (b))
}

// Grade is one check's verdict: P, F, N/A or U (ungraded), with its evidence.
type Grade struct{ V, Note string }

func (g Grade) String() string {
	if g.Note == "" {
		return g.V
	}
	return g.V + ": " + g.Note
}

// Checks is the rubric's twelve, in its order.
var Checks = []string{"S1", "S2", "S3", "S4", "S5", "S6", "S7", "G1", "G2", "G3", "G4", "G5"}

// LoadArms reads arms.tsv: a # header, then kind, id, parent, short title,
// placed arms (tl:🎲 c:🛡️ br:🖼️), tincture, tincture reason, glosses
// (🛡️=arms; 🎲=the die layout), reason line, round-1 status.
func LoadArms(r io.Reader) ([]*Entry, error) {
	var out []*Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for line := 1; sc.Scan(); line++ {
		l := sc.Text()
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		c := strings.Split(l, "\t")
		if len(c) != 10 {
			return nil, fmt.Errorf("line %d: %d columns, want 10", line, len(c))
		}
		e := &Entry{Kind: c[0], ID: c[1], Parent: c[2], Short: c[3], Tincture: c[5], TReason: c[6], Reason: c[8], Round1: c[9],
			Gloss: map[string]string{}, Grades: map[string]Grade{}}
		if e.Parent == "-" {
			e.Parent = ""
		}
		for f := range strings.FieldsSeq(c[4]) {
			k, v, ok := strings.Cut(f, ":")
			i, known := placeIndex[k]
			if !ok || !known || v == "" {
				return nil, fmt.Errorf("line %d: %q is not place:emoji", line, f)
			}
			if e.Arms[i] != "" {
				return nil, fmt.Errorf("line %d: place %s given twice", line, k)
			}
			e.Arms[i] = v
		}
		for g := range strings.SplitSeq(c[7], ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(g), "=")
			if !ok {
				return nil, fmt.Errorf("line %d: gloss %q is not emoji=meaning", line, g)
			}
			e.Gloss[string(baseRunes(strings.TrimSpace(k)))] = strings.TrimSpace(v)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

var placeIndex = map[string]int{"tl": 0, "tr": 1, "c": 2, "bl": 3, "br": 4}

// LoadGrades reads grades.tsv: id, then one column per check the reviewer
// grades by hand (S1–S7 from the bead and the reason line; G3, G4 and G5
// from the pictures), each "P", "F: evidence", "N/A: reason" or "U: why".
// A missing file or an empty cell leaves the check to the measurement or
// ungraded.
func LoadGrades(path string) (map[string]map[string]Grade, error) {
	out := map[string]map[string]Grade{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cols []string
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		if strings.HasPrefix(c[0], "#") {
			cols = c
			cols[0] = strings.TrimSpace(strings.TrimPrefix(cols[0], "#"))
			continue
		}
		if len(c) < 2 || cols == nil {
			continue
		}
		row := map[string]Grade{}
		for i := 1; i < len(c) && i < len(cols); i++ {
			v := strings.TrimSpace(c[i])
			if v == "" {
				continue
			}
			k, note, _ := strings.Cut(v, ":")
			row[cols[i]] = Grade{strings.TrimSpace(k), strings.TrimSpace(note)}
		}
		out[c[0]] = row
	}
	return out, sc.Err()
}

func (e *Entry) charges() []string {
	var out []string
	for _, c := range e.Arms {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

func (e *Entry) key() string { return e.Tincture + " " + strings.Join(e.Arms[:], ",") }

// Round2 holds the round's state while it is scored and drawn.
type Round2 struct {
	Entries []*Entry
	ByID    map[string]*Entry
	Matrix  *Matrix
	Src     Charges
	Name    func(string) string
	Pairs   []Pair // G4: every pair in the register, closest first
	Warn    []string
}

// validate applies R2, R3, R5 and R11 to the round as the daemon would, and
// collects R5's warnings (same set of charges, or same tuple on another
// tincture).
func (r *Round2) validate(lib []Charge) error {
	inLib := map[string]bool{}
	for _, c := range lib {
		inLib[string(baseRunes(c.Emoji))] = true
	}
	var errs []string
	for _, e := range r.Entries {
		cs := e.charges()
		if len(cs) == 0 || len(cs) > 5 {
			errs = append(errs, fmt.Sprintf("%s: %d charges", e.ID, len(cs)))
		}
		seen := map[string]bool{}
		for _, c := range cs {
			b := string(baseRunes(c))
			if !inLib[b] {
				errs = append(errs, fmt.Sprintf("%s: %s is not in the library", e.ID, c))
			}
			if seen[b] {
				errs = append(errs, fmt.Sprintf("%s: %s is used twice", e.ID, c))
			}
			seen[b] = true
			if _, ok := e.Gloss[b]; !ok {
				errs = append(errs, fmt.Sprintf("%s: %s has no gloss", e.ID, c))
			}
		}
		if e.Kind == "phase" && r.ByID[e.Parent] == nil {
			errs = append(errs, fmt.Sprintf("%s: phase of %q, which is not in the round", e.ID, e.Parent))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("arms.tsv:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// resolveTinctures fixes every "auto" and "plan" tincture, then checks R5's
// uniqueness. Named tinctures first, then auto (R13: the free tincture with
// the best measured contrast for the charges, see pick), then phases (their
// plan's tincture, rubric S6, unless a charge fails G1 or G2 on it).
func (r *Round2) resolveTinctures() error {
	taken := map[string]string{}
	for _, e := range r.Entries {
		if _, ok := Tinctures[e.Tincture]; !ok && e.Tincture != "auto" && e.Tincture != "plan" {
			return fmt.Errorf("%s: tincture %q is not in the palette", e.ID, e.Tincture)
		}
	}
	// pick is R13's default: among the free tinctures, those where the
	// (charge, palette) pairs score best, a point for clearing 3:1 (G2) and a
	// point for not sinking (G1), so a charge that fails 3:1 everywhere still
	// lands where it does not sink; ties go to a tincture chosen by a hash of the charges, so
	// the default spreads over the palette and one arms' choice never moves
	// another's.
	pick := func(e *Entry) string {
		type cand struct {
			tn string
			ok float64
		}
		var cs []cand
		best := -1.0
		for _, tn := range TinctureOrder {
			if taken[tn+" "+strings.Join(e.Arms[:], ",")] != "" {
				continue
			}
			ok := 0.0
			for _, c := range e.charges() {
				for _, m := range []Mode{Day, Night} {
					v := r.verdict(c, tn, m)
					if v.Pass {
						ok++
					}
					if v.Sink <= SinkShare {
						ok++
					}
				}
			}
			cs = append(cs, cand{tn, ok})
			best = math.Max(best, ok)
		}
		var tied []string
		for _, c := range cs {
			if c.ok == best {
				tied = append(tied, c.tn)
			}
		}
		h := fnv.New32a()
		h.Write([]byte(strings.Join(e.Arms[:], ",")))
		return tied[int(h.Sum32()%uint32(len(tied)))]
	}
	order := func(want string) []*Entry {
		var out []*Entry
		for _, e := range r.Entries {
			if e.Tincture == want {
				out = append(out, e)
			}
		}
		return out
	}
	for _, e := range r.Entries {
		if _, ok := Tinctures[e.Tincture]; ok {
			taken[e.key()] = e.ID
		}
	}
	for _, e := range order("auto") {
		e.Tincture = pick(e)
		e.TinctureNote = "the system's default (R13): best measured contrast for these charges"
		taken[e.key()] = e.ID
	}
	for _, e := range order("plan") {
		p := r.ByID[e.Parent]
		e.Tincture = p.Tincture
		e.TinctureNote = "its plan's (rubric S6)"
		ok := true
		for _, c := range e.charges() {
			for _, m := range []Mode{Day, Night} {
				if v := r.verdict(c, e.Tincture, m); !v.Pass || v.Sink > SinkShare {
					ok = false
				}
			}
		}
		if !ok || taken[e.key()] != "" {
			was := e.Tincture
			e.Tincture = pick(e)
			e.TinctureNote = fmt.Sprintf("its plan's is %s, where a charge fails G1 or G2; the best passing tincture instead", was)
		}
		taken[e.key()] = e.ID
	}
	seen := map[string]string{}
	sets := map[string]string{}
	for _, e := range r.Entries {
		if other, ok := seen[e.key()]; ok {
			return fmt.Errorf("R5: %s and %s bear the same arms (%s)", e.ID, other, e.key())
		}
		seen[e.key()] = e.ID
		cs := e.charges()
		for i := range cs {
			cs[i] = string(baseRunes(cs[i]))
		}
		sort.Strings(cs)
		set := strings.Join(cs, "")
		if other, ok := sets[set]; ok {
			r.Warn = append(r.Warn, fmt.Sprintf("R5 warning: %s bears the same set of charges as %s", e.ID, other))
		}
		sets[set] = e.ID
	}
	return nil
}

func (r *Round2) verdict(c, tn string, m Mode) Verdict {
	if row, ok := r.Matrix.Cell[c]; ok {
		return row[cellKey(tn, m)]
	}
	for e, row := range r.Matrix.Cell {
		if string(baseRunes(e)) == string(baseRunes(c)) {
			return row[cellKey(tn, m)]
		}
	}
	st, _ := r.Matrix.StatsFor(c, r.Src)
	return st.Judge(Tinctures[tn].Colour(m))
}

func (r *Round2) stats(c string) ChargeStats {
	st, _ := r.Matrix.StatsFor(c, r.Src)
	return st
}

// grade fills the checks the measurements decide (G1, G2, the objective half
// of G3, G4's pre-filter and distances) and the mechanical semantic checks
// (S6 from the shared charges and glosses, S7 from glosses across the
// register), then lays the reviewer's grades over them.
func (r *Round2) grade(hand map[string]map[string]Grade) {
	// S7: the first arms in file order to use a charge fix its meaning.
	meaning := map[string]struct{ id, gloss string }{}
	for _, e := range r.Entries {
		var fails []string
		for _, c := range e.charges() {
			b := string(baseRunes(c))
			g := e.Gloss[b]
			if m, ok := meaning[b]; ok && !sameGloss(m.gloss, g) {
				fails = append(fails, fmt.Sprintf("%s is %q here but %q on %s", c, g, m.gloss, m.id))
			} else if !ok {
				meaning[b] = struct{ id, gloss string }{e.ID, g}
			}
		}
		if len(fails) > 0 {
			e.Grades["S7"] = Grade{"F", strings.Join(fails, "; ")}
		} else {
			e.Grades["S7"] = Grade{"P", ""}
		}
	}
	// S6: a phase shares a charge with its plan, with the same gloss, in a
	// corner, and is not the plan's arms with one place changed.
	phases := map[string][]*Entry{}
	for _, e := range r.Entries {
		if e.Kind == "phase" {
			phases[e.Parent] = append(phases[e.Parent], e)
		}
	}
	for _, e := range r.Entries {
		switch {
		case e.Kind == "phase":
			e.Grades["S6"] = r.phaseFamily(e, r.ByID[e.Parent])
		case len(phases[e.ID]) > 0:
			var bad []string
			for _, ph := range phases[e.ID] {
				if g := r.phaseFamily(ph, e); g.V != "P" {
					bad = append(bad, ph.ID)
				}
			}
			if len(bad) > 0 {
				e.Grades["S6"] = Grade{"F", "phases failing: " + strings.Join(bad, ", ")}
			} else {
				e.Grades["S6"] = Grade{"P", fmt.Sprintf("all %d phases share a charge", len(phases[e.ID]))}
			}
		default:
			e.Grades["S6"] = Grade{"N/A", "not a plan with phases, nor a phase"}
		}
	}
	for _, e := range r.Entries {
		r.graphic(e)
	}
	r.pairs()
	for _, e := range r.Entries {
		for k, g := range hand[e.ID] {
			if k == "G2raw" {
				continue
			}
			if k == "G3" {
				// The picture decides (rubric: a measurement triages); the
				// measurement stays in the note.
				m := e.Grades["G3"]
				note := "by eye"
				if g.Note != "" {
					note += ": " + g.Note
				}
				if m.V == "F" {
					note += "; measured: " + m.Note
				}
				e.Grades["G3"] = Grade{g.V, note}
				continue
			}
			if k == "S6" || k == "S7" {
				if g.V == "F" || e.Grades[k].V != "F" {
					e.Grades[k] = g
				}
				continue
			}
			e.Grades[k] = g
		}
		for _, k := range Checks {
			if _, ok := e.Grades[k]; !ok {
				e.Grades[k] = Grade{"U", "not graded yet"}
			}
		}
	}
	// A plan passes S6 only if every phase does, after the reviewer's grades.
	for id, phs := range phases {
		var bad []string
		for _, ph := range phs {
			if ph.Grades["S6"].V == "F" {
				bad = append(bad, ph.ID)
			}
		}
		if p := r.ByID[id]; p != nil && len(bad) > 0 {
			p.Grades["S6"] = Grade{"F", "phases failing: " + strings.Join(bad, ", ")}
		}
	}
}

func sameGloss(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func (r *Round2) phaseFamily(ph, plan *Entry) Grade {
	var shared []string
	inCorner, sameMeaning := false, false
	for i, c := range ph.Arms {
		if c == "" {
			continue
		}
		for _, pc := range plan.Arms {
			if pc != "" && string(baseRunes(pc)) == string(baseRunes(c)) {
				shared = append(shared, c)
				b := string(baseRunes(c))
				if sameGloss(ph.Gloss[b], plan.Gloss[b]) {
					sameMeaning = true
					if i != 2 {
						inCorner = true
					}
				}
			}
		}
	}
	diff := 0
	for i := range ph.Arms {
		if string(baseRunes(ph.Arms[i])) != string(baseRunes(plan.Arms[i])) {
			diff++
		}
	}
	switch {
	case len(shared) == 0:
		return Grade{"F", "(a) shares no charge with its plan " + plan.ID}
	case !sameMeaning:
		return Grade{"F", "(b) the shared charge means something else on the plan"}
	case !inCorner:
		return Grade{"F", "(c) the shared charge sits in the centre"}
	case diff <= 1:
		return Grade{"F", "(d) the plan's arms with one place changed"}
	}
	return Grade{"P", "shares " + strings.Join(shared, " ")}
}

// graphic grades G1, G2 (raw and with the keyline) and G3's objective half.
func (r *Round2) graphic(e *Entry) {
	var sinks, raw []string
	for _, c := range e.charges() {
		for _, m := range []Mode{Day, Night} {
			v := r.verdict(c, e.Tincture, m)
			if v.Sink > SinkShare {
				sinks = append(sinks, fmt.Sprintf("%s sinks %.0f%% on %s %s", c, 100*v.Sink, e.Tincture, m.Name))
			}
			if !v.Pass {
				raw = append(raw, fmt.Sprintf("%s %.1f:1 (edge %.1f:1) on %s %s", c, v.Median, v.Far, e.Tincture, m.Name))
			}
		}
	}
	if len(sinks) > 0 {
		e.Grades["G1"] = Grade{"F", strings.Join(sinks, "; ")}
	} else {
		e.Grades["G1"] = Grade{"P", ""}
	}
	if len(raw) > 0 {
		e.G2Raw = Grade{"F", strings.Join(raw, "; ")}
	} else {
		e.G2Raw = Grade{"P", ""}
	}
	// With the keyline the charge's edge is the keyline, which clears the
	// field in both palettes for every tincture (TestKeylineOutlinesCharges).
	klOK := true
	for _, m := range []Mode{Day, Night} {
		f := Tinctures[e.Tincture].Colour(m)
		if contrast(lum(KeylineFor(f)), lum(f)) < Threshold {
			klOK = false
		}
	}
	if klOK {
		e.Grades["G2"] = Grade{"P", "with the keyline"}
	} else {
		e.Grades["G2"] = Grade{"F", "the keyline itself is under 3:1"}
	}
	// G3, objective half: the centre passes G2 raw and is at least as large
	// as any corner. A pairing (empty centre) grades both corners on G2.
	c := e.Arms[2]
	var notes []string
	if c == "" {
		if raw := e.G2Raw; raw.V == "F" {
			notes = append(notes, "pairing: a corner fails G2 raw")
		}
	} else {
		for _, m := range []Mode{Day, Night} {
			if v := r.verdict(c, e.Tincture, m); !v.Pass {
				notes = append(notes, fmt.Sprintf("centre %s fails G2 raw by %s", c, m.Name))
			}
		}
		ca := r.stats(c).Area
		for i, k := range e.Arms {
			if i == 2 || k == "" {
				continue
			}
			if a := r.stats(k).Area; a > ca*1.05 {
				notes = append(notes, fmt.Sprintf("%s %s is larger than the centre (%.0f%% vs %.0f%% of its square)", strings.ToUpper(Places[i].Name), k, 100*a, 100*ca))
			}
		}
	}
	if len(notes) > 0 {
		e.Grades["G3"] = Grade{"F", strings.Join(notes, "; ")}
	} else {
		e.Grades["G3"] = Grade{"U", "measured: passes; by eye: not yet"}
	}
}

// Pair is two arms of the register at G4's distance: the mean redmean over
// their 24 px pictures at 2×, day and night averaged.
type Pair struct {
	A, B      *Entry
	Dist      float64
	Prefilter bool // same tincture, same places, centres of one colour family
}

func (r *Round2) pairs() {
	pics := map[string][2]*image.NRGBA{}
	for _, e := range r.Entries {
		d, _ := Render(e.Arms, 24, Day, Tinctures[e.Tincture], Style{DPR: 2}, r.Src)
		n, _ := Render(e.Arms, 24, Night, Tinctures[e.Tincture], Style{DPR: 2}, r.Src)
		pics[e.ID] = [2]*image.NRGBA{onSurface(d, Day.Surface), onSurface(n, Night.Surface)}
	}
	occupied := func(e *Entry) string {
		var b strings.Builder
		for _, c := range e.Arms {
			if c == "" {
				b.WriteByte('.')
			} else {
				b.WriteByte('x')
			}
		}
		return b.String()
	}
	fam := func(e *Entry) string {
		if e.Arms[2] == "" {
			return ""
		}
		return Family(r.stats(e.Arms[2]).MedianColour)
	}
	r.Pairs = nil
	for i, a := range r.Entries {
		for _, b := range r.Entries[i+1:] {
			pa, pb := pics[a.ID], pics[b.ID]
			d := (meanRedmean(pa[0], pb[0]) + meanRedmean(pa[1], pb[1])) / 2
			pre := a.Tincture == b.Tincture && occupied(a) == occupied(b) && fam(a) == fam(b)
			r.Pairs = append(r.Pairs, Pair{a, b, d, pre})
		}
	}
	sort.Slice(r.Pairs, func(i, j int) bool { return r.Pairs[i].Dist < r.Pairs[j].Dist })
	// The closest pairs are judged by eye (grades.tsv); a pre-filter flag
	// farther apart than every pair judged by eye passes on the measure.
	closest := map[string][]string{}
	for _, p := range r.Pairs[:min(G4Closest, len(r.Pairs))] {
		closest[p.A.ID] = append(closest[p.A.ID], fmt.Sprintf("%s %.0f", p.B.ID, p.Dist))
		closest[p.B.ID] = append(closest[p.B.ID], fmt.Sprintf("%s %.0f", p.A.ID, p.Dist))
	}
	nearest := map[string]float64{}
	for _, p := range r.Pairs {
		for _, e := range []*Entry{p.A, p.B} {
			if _, ok := nearest[e.ID]; !ok {
				nearest[e.ID] = p.Dist
			}
		}
	}
	cut := 0.0
	if len(r.Pairs) >= G4Closest {
		cut = r.Pairs[G4Closest-1].Dist
	}
	for _, e := range r.Entries {
		if f := closest[e.ID]; len(f) > 0 {
			e.Grades["G4"] = Grade{"U", "among the register's closest pairs, to judge by eye: " + strings.Join(f, ", ")}
		} else {
			e.Grades["G4"] = Grade{"P", fmt.Sprintf("measured: nearest arms at %.0f, farther than the %d closest pairs (to %.0f), which were judged by eye", nearest[e.ID], G4Closest, cut)}
		}
	}
}

// G4Closest is how many of the register's closest pairs the reviewer looks
// at by eye (rubric G4: at least ten). The pre-filter flags far more pairs
// than that in one register of 137 arms (see summary.md), so beyond these the
// measure decides until rubric v2 sets a threshold.
const G4Closest = 30

func onSurface(img *image.NRGBA, bg color.NRGBA) *image.NRGBA {
	out := image.NewNRGBA(img.Bounds())
	xdraw.Draw(out, out.Bounds(), &image.Uniform{bg}, image.Point{}, xdraw.Src)
	xdraw.Draw(out, out.Bounds(), img, image.Point{}, xdraw.Over)
	return out
}

func meanRedmean(a, b *image.NRGBA) float64 {
	var s float64
	n := 0
	for y := 0; y < a.Bounds().Dy(); y++ {
		for x := 0; x < a.Bounds().Dx(); x++ {
			s += redmean(a.NRGBAAt(x, y), b.NRGBAAt(x, y))
			n++
		}
	}
	return s / float64(n)
}

// Score is the rubric §5 score: passes over the checks that are neither N/A
// nor ungraded, with G2 as graded (keyline) or raw (Q14 (b)).
func (e *Entry) Score(raw bool) (pass, of, ungraded int, band string) {
	hard := false
	for _, k := range Checks {
		g := e.Grades[k]
		if k == "G2" && raw {
			g = e.G2Raw
		}
		switch g.V {
		case "P":
			pass++
			of++
		case "F":
			of++
			if k == "S5" || k == "G2" || k == "G4" {
				hard = true
			}
		case "U":
			ungraded++
		}
	}
	switch {
	case of == 0:
		band = "ungraded"
	case hard:
		band = "revise (hard check)"
	case pass == of:
		band = "accept"
	case float64(pass)/float64(of) >= 0.83:
		band = "accept with notes"
	default:
		band = "revise"
	}
	return
}

func (e *Entry) scoreText(raw bool) string {
	p, of, u, band := e.Score(raw)
	s := fmt.Sprintf("%d/%d", p, of)
	if of > 0 {
		s += fmt.Sprintf(" (%.0f%%)", 100*float64(p)/float64(of))
	}
	if u > 0 {
		s += fmt.Sprintf(", %d ungraded", u)
	}
	return s + ", " + band
}

func (e *Entry) placed() string {
	var parts []string
	for i, c := range e.Arms {
		if c != "" {
			parts = append(parts, Places[i].Name+":"+c)
		}
	}
	return strings.Join(parts, " ")
}

func (e *Entry) run() string { return strings.Join(e.charges(), "") }

// round2 scores and draws the round into out.
func round2(n Noto, armsPath, gradesPath, libPath, out string) error {
	if err := loadFonts(); err != nil {
		return err
	}
	src := cache(n)
	f, err := os.Open(armsPath)
	if err != nil {
		return err
	}
	entries, err := LoadArms(f)
	f.Close()
	if err != nil {
		return err
	}
	lib, err := LoadLibrary(libPath)
	if err != nil {
		return err
	}
	hand, err := LoadGrades(gradesPath)
	if err != nil {
		return err
	}
	r := &Round2{Entries: entries, ByID: map[string]*Entry{}, Src: src, Name: nameOf(lib)}
	for _, e := range entries {
		r.ByID[e.ID] = e
	}
	if err := r.validate(lib); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "round2: measuring the library against eight tinctures")
	if r.Matrix, err = BuildMatrix(lib, src); err != nil {
		return err
	}
	if err := r.resolveTinctures(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "round2: grading")
	r.grade(hand)
	if err := r.Matrix.WriteTSV(filepath.Join(out, "contrast-matrix.tsv"), r.Name); err != nil {
		return err
	}
	if err := r.writeScored(filepath.Join(out, "scored.tsv")); err != nil {
		return err
	}
	if err := r.writeSummary(filepath.Join(out, "summary.md")); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "round2: drawing the sheets")
	if err := r.roomPages(out); err != nil {
		return err
	}
	if err := r.page40(filepath.Join(out, "r2-2-40px@2x.png")); err != nil {
		return err
	}
	if err := r.page24(filepath.Join(out, "r2-3-24px@2x.png")); err != nil {
		return err
	}
	if err := r.stakePage(filepath.Join(out, "r2-4-stake-16px@3x.png")); err != nil {
		return err
	}
	if err := r.tincturePages(out); err != nil {
		return err
	}
	if err := r.pairsPage(filepath.Join(out, "r2-6-g4-closest-pairs.png")); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "round2: drawing the contrast matrix")
	return matrixSheets(r.Matrix, src, out)
}

func (r *Round2) writeScored(path string) error {
	var sb strings.Builder
	sb.WriteString("# kind\tid\tparent\tshort_title\tarms\trun\ttincture\ttincture_reason\tglosses\treason\tround1")
	for _, k := range Checks {
		sb.WriteString("\t" + k)
	}
	sb.WriteString("\tG2_raw\tscore_keyline\tscore_raw\n")
	for _, e := range r.Entries {
		tr := e.TReason
		if e.TinctureNote != "" {
			tr = e.TinctureNote
		}
		var gl []string
		for _, c := range e.charges() {
			gl = append(gl, c+"="+e.Gloss[string(baseRunes(c))])
		}
		parent := e.Parent
		if parent == "" {
			parent = "-"
		}
		fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s", e.Kind, e.ID, parent, e.Short, e.placed(), e.run(), e.Tincture, tr, strings.Join(gl, "; "), e.Reason, e.Round1)
		for _, k := range Checks {
			sb.WriteString("\t" + e.Grades[k].String())
		}
		fmt.Fprintf(&sb, "\t%s\t%s\t%s\n", e.G2Raw, e.scoreText(false), e.scoreText(true))
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func (r *Round2) writeSummary(path string) error {
	var sb strings.Builder
	kinds := map[string]int{}
	for _, e := range r.Entries {
		kinds[e.Kind]++
	}
	fmt.Fprintf(&sb, "# Round 2 summary (generated by `go run . round2`)\n\n")
	fmt.Fprintf(&sb, "%d arms: %d plans, %d phases, %d other epics. One register (gardenio).\n\n", len(r.Entries), kinds["plan"], kinds["phase"], kinds["epic"])
	for _, raw := range []bool{false, true} {
		title := "with the keyline (Q14 (a))"
		if raw {
			title = "raw, refusing failing pairs (Q14 (b))"
		}
		bands := map[string]int{}
		for _, e := range r.Entries {
			_, _, _, b := e.Score(raw)
			bands[b]++
		}
		fmt.Fprintf(&sb, "## Bands, G2 %s\n\n| Band | Arms |\n|---|---:|\n", title)
		for _, b := range []string{"accept", "accept with notes", "revise", "revise (hard check)", "ungraded"} {
			fmt.Fprintf(&sb, "| %s | %d |\n", b, bands[b])
		}
		sb.WriteString("\n")
	}
	sb.WriteString("## Fails per check\n\n| Check | PASS | FAIL | N/A | ungraded |\n|---|---:|---:|---:|---:|\n")
	for _, k := range append(slices.Clone(Checks), "G2raw") {
		c := map[string]int{}
		for _, e := range r.Entries {
			g := e.Grades[k]
			if k == "G2raw" {
				g = e.G2Raw
			}
			c[g.V]++
		}
		fmt.Fprintf(&sb, "| %s | %d | %d | %d | %d |\n", k, c["P"], c["F"], c["N/A"], c["U"])
	}
	sb.WriteString("\n## Tinctures in use\n\n| Tincture | Day | Night | Arms | of which by default |\n|---|---|---|---:|---:|\n")
	for _, tn := range TinctureOrder {
		n, auto := 0, 0
		for _, e := range r.Entries {
			if e.Tincture == tn {
				n++
				if strings.HasPrefix(e.TinctureNote, "the system's default") {
					auto++
				}
			}
		}
		t := Tinctures[tn]
		fmt.Fprintf(&sb, "| %s | `%s` | `%s` | %d | %d |\n", tn, t.DayHex, t.NightHex, n, auto)
	}
	sb.WriteString("\n## The contrast matrix: library charges under 3:1 (rubric G2's rule), per tincture\n\n| Tincture | Day fails | Night fails | Keyline day | Keyline night |\n|---|---:|---:|---|---|\n")
	fails := r.Matrix.Fails()
	for _, tn := range TinctureOrder {
		t := Tinctures[tn]
		fmt.Fprintf(&sb, "| %s | %d | %d | `%s` | `%s` |\n", tn, fails[cellKey(tn, Day)], fails[cellKey(tn, Night)], hexOf(KeylineFor(t.Colour(Day))), hexOf(KeylineFor(t.Colour(Night))))
	}
	anyFail, allFail := 0, 0
	for _, c := range r.Matrix.Charges {
		nf := 0
		for _, v := range r.Matrix.Cell[c.Emoji] {
			if !v.Pass {
				nf++
			}
		}
		if nf > 0 {
			anyFail++
		}
		if nf == 2*len(TinctureOrder) {
			allFail++
		}
	}
	fmt.Fprintf(&sb, "\nOf %d charges, %d fail on at least one (tincture, palette) and %d fail on all sixteen. Total cells: %d, failing: %d.\n",
		len(r.Matrix.Charges), anyFail, allFail, len(r.Matrix.Charges)*2*len(TinctureOrder), sum(fails))
	sb.WriteString("\n## G4: 24 px distances (mean redmean, 24 CSS px at 2x, day and night averaged)\n\n")
	ds := make([]float64, len(r.Pairs))
	pre := 0
	for i, p := range r.Pairs {
		ds[i] = p.Dist
		if p.Prefilter {
			pre++
		}
	}
	if len(ds) > 0 {
		q := func(f float64) float64 { return ds[min(len(ds)-1, int(f*float64(len(ds))))] }
		fmt.Fprintf(&sb, "%d pairs. Min %.1f, 1%% %.1f, 5%% %.1f, 25%% %.1f, median %.1f, max %.1f. Pre-filter flags %d pairs (same tincture, same places, centres of one colour family); the %d closest pairs below were judged by eye.\n\n", len(ds), ds[0], q(0.01), q(0.05), q(0.25), q(0.5), ds[len(ds)-1], pre, G4Closest)
		fmt.Fprintf(&sb, "| # | Distance | Pre-filter | A | B |\n|---:|---:|---|---|---|\n")
		for i, p := range r.Pairs {
			if i >= G4Closest {
				break
			}
			fmt.Fprintf(&sb, "| %d | %.1f | %v | %s %s %s | %s %s %s |\n", i+1, p.Dist, p.Prefilter, p.A.ID, p.A.run(), p.A.Tincture, p.B.ID, p.B.run(), p.B.Tincture)
		}
	}
	if len(r.Warn) > 0 {
		sb.WriteString("\n## R5 warnings\n\n")
		for _, w := range r.Warn {
			sb.WriteString("- " + w + "\n")
		}
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func sum(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}

// groups orders the round for the sheets: each plan then its phases, plans
// in file order, then the other epics.
func (r *Round2) groups() [][]*Entry {
	var out [][]*Entry
	for _, e := range r.Entries {
		if e.Kind != "plan" {
			continue
		}
		g := []*Entry{e}
		for _, p := range r.Entries {
			if p.Kind == "phase" && p.Parent == e.ID {
				g = append(g, p)
			}
		}
		out = append(out, g)
	}
	var other []*Entry
	for _, e := range r.Entries {
		if e.Kind == "epic" {
			other = append(other, e)
		}
	}
	return append(out, other)
}

// roomPages draws the 64 and 40 px pictures first (owner, round 1: room
// first), one row per arms, at 2×: 64 day, night, then with the keyline; 40
// the same. Pages of about forty rows, never splitting a plan from its phases
// unless the plan alone is longer.
func (r *Round2) roomPages(out string) error {
	var pages [][]*Entry
	var cur []*Entry
	for _, g := range r.groups() {
		if len(cur) > 0 && len(cur)+len(g) > 42 {
			pages = append(pages, cur)
			cur = nil
		}
		cur = append(cur, g...)
	}
	if len(cur) > 0 {
		pages = append(pages, cur)
	}
	for i, pg := range pages {
		img, err := r.roomPage(pg, i+1, len(pages))
		if err != nil {
			return err
		}
		if err := writePNG(filepath.Join(out, fmt.Sprintf("r2-1-64-40px-%d@2x.png", i+1)), img); err != nil {
			return err
		}
	}
	return nil
}

const (
	textW  = 1000
	pad    = 16
	header = 120
)

func (r *Round2) roomPage(rows []*Entry, page, pages int) (*image.NRGBA, error) {
	const d = 2
	h64, h40 := heightFor(64*d), heightFor(40*d)
	cellW := []int{64 * d, 64 * d, 64 * d, 64 * d, 40 * d, 40 * d, 40 * d, 40 * d}
	picsW := 0
	for _, w := range cellW {
		picsW += w + 2*pad
	}
	width := textW + picsW + pad
	rowH := h64 + 2*pad + 40
	fBody, fBold, fSmall := face(false, 19), face(true, 21), face(false, 17)
	// Measure each row's text first so rows can grow.
	heights := make([]int, len(rows))
	total := header
	for i, e := range rows {
		heights[i] = max(rowH, 34+26*len(r.rowLines(e, fBody, fSmall))+pad)
		total += heights[i]
	}
	sh := newSheet(width, total+pad, Day.Surface)
	sh.text(pad, 36, fmt.Sprintf("Heraldry round 2: every open epic at 64 and 40 CSS px, drawn at 2x (page %d of %d)", page, pages), Day.Ink, face(true, 26))
	sh.text(pad, 68, "Columns: 64 day, 64 night, 64 day keyline, 64 night keyline | 40 day, 40 night, 40 day keyline, 40 night keyline. View at 2x (or 50% zoom) for true size.", Day.Ink, fBody)
	sh.text(pad, 96, "Score: rubric S1-S7, G1-G5 (gardenio/docs/heraldry/rubric.md). G2 is scored with the keyline (Q14 (a)); \"raw\" is the score if failing pairs were refused instead (Q14 (b)).", Day.Ink, fBody)
	y := header
	for i, e := range rows {
		bg := Day.Surface
		if i%2 == 1 {
			bg = mustHex("#f2ebd3")
		}
		sh.fill(image.Rect(0, y, textW, y+heights[i]), bg)
		sh.text(pad, y+30, fmt.Sprintf("%s  %s", e.ID, e.Short), Day.Ink, fBold)
		ly := y + 30
		for _, l := range r.rowLines(e, fBody, fSmall) {
			ly += 26
			f := fBody
			if l.small {
				f = fSmall
			}
			sh.text(pad+l.indent, ly, l.text, l.colour, f)
		}
		x := textW
		for k, spec := range []struct {
			w  int
			m  Mode
			kl bool
		}{{64, Day, false}, {64, Night, false}, {64, Day, true}, {64, Night, true}, {40, Day, false}, {40, Night, false}, {40, Day, true}, {40, Night, true}} {
			sh.fill(image.Rect(x, y, x+cellW[k]+2*pad, y+heights[i]), spec.m.Surface)
			img, err := Render(e.Arms, spec.w, spec.m, Tinctures[e.Tincture], Style{DPR: d, Keyline: spec.kl}, r.Src)
			if err != nil {
				return nil, err
			}
			top := y + pad
			if spec.w == 40 {
				top = y + pad + (h64-h40)/2
			}
			sh.put(img, x+pad, top, 1)
			x += cellW[k] + 2*pad
		}
		y += heights[i]
	}
	return sh.img, nil
}

type textLine struct {
	text   string
	indent int
	small  bool
	colour color.NRGBA
}

var (
	failRed  = mustHex("#b3261e")
	passInk  = mustHex("#2d6a2e")
	mutedInk = mustHex("#6b5a48")
)
