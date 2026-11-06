package pgbudget

import (
	"fmt"
	"math"
	"strings"
)

// The report is plain text: the Commons shows a message's text pre-wrapped,
// not as Markdown, so the "tables" are numbered lists.

// TopN is how many statements and tables the report lists.
const TopN = 5

// Text is the #machines post. filed names the tasks this run filed, by bead
// id; capped counts the breaches the daily cap held back.
func (r *Report) Text(filed []Filed, capped int) string {
	var w strings.Builder
	fmt.Fprintf(&w, "Postgres efficiency, the 24h to %s\n", r.AsOf.Format("15:04Z 2 Jan"))
	fmt.Fprintf(&w, "Statement time %s%s.\n", secs(r.TotalSeconds), change(r.TotalSeconds, r.PrevTotalSeconds, r.PrevTotalSeconds > 0, " on the day before"))

	w.WriteString("\nTop statements by total time\n")
	top := r.TopStatements(TopN)
	if len(top) == 0 {
		w.WriteString("No statement series in vmsingle: is the postgres exporter up?\n")
	}
	for i, s := range top {
		fmt.Fprintf(&w, "%d. %s %s (%s%s) · %s calls · %s mean", i+1, s.Datname, secs(s.Today.ExecSeconds), pct(s.Today.Share),
			change(s.Today.ExecSeconds, s.Yesterday.ExecSeconds, s.Seen, ""), count(s.Today.Calls), secs(s.Today.Mean()))
		if len(s.Callers) > 0 {
			fmt.Fprintf(&w, " · %s", strings.Join(s.Callers, ", "))
		}
		if len(s.Today.Over) > 0 {
			fmt.Fprintf(&w, " · over budget: %s", strings.Join(s.Today.Over, "; "))
		}
		fmt.Fprintf(&w, "\n   queryid %s: %s\n", s.QueryID, oneLine(s.Query))
	}

	w.WriteString("\nTables by 24h growth\n")
	grow := r.TopGrowth(TopN)
	if len(grow) == 0 {
		w.WriteString("No table has a size a day back yet.\n")
	}
	for i, t := range grow {
		fmt.Fprintf(&w, "%d. %s %s, %s", i+1, t.Name(), bytesIEC(t.Bytes), signedBytes(t.Growth))
		if t.HasPrevGrowth {
			fmt.Fprintf(&w, " (day before %s)", signedBytes(t.PrevGrowth))
		}
		if len(t.Over) > 0 {
			fmt.Fprintf(&w, " · over budget: %s", strings.Join(t.Over, "; "))
		}
		w.WriteString("\n")
	}

	w.WriteString("\nBudget alerts that fired\n")
	if len(r.Fired) == 0 {
		w.WriteString("None.\n")
	}
	for _, f := range r.Fired {
		fmt.Fprintf(&w, "- %s\n", f)
	}

	if len(filed) > 0 || capped > 0 {
		w.WriteString("\nOver budget two days running\n")
		for _, f := range filed {
			fmt.Fprintf(&w, "- %s filed for %s\n", f.BeadID, f.Subject)
		}
		if capped > 0 {
			fmt.Fprintf(&w, "- %d more held back by the %d-a-day cap; they file on a later day if they stay over\n", capped, MaxTasksPerDay)
		}
	}
	return strings.TrimRight(w.String(), "\n")
}

func oneLine(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if q == "" {
		return "(no query text)"
	}
	return q
}

// change is " +12%" style against prev, "" when prev is unknown, " new" when
// prev is 0.
func change(cur, prev float64, known bool, suffix string) string {
	switch {
	case !known:
		return ""
	case prev <= 0:
		if cur <= 0 {
			return ""
		}
		return ", new" + suffix
	}
	d := (cur - prev) / prev
	if math.Abs(d) < 0.005 {
		return ", flat" + suffix
	}
	return fmt.Sprintf(", %+.0f%%%s", d*100, suffix)
}

func pct(f float64) string {
	p := f * 100
	if p != 0 && math.Abs(p) < 1 {
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", p)
}

// secs prints a duration in seconds as ms, s, or thousands of s.
func secs(s float64) string {
	switch {
	case s < 1:
		return fmt.Sprintf("%.0f ms", s*1000)
	case s < 10:
		return fmt.Sprintf("%.1f s", s)
	}
	return count(s) + " s"
}

// count prints a whole number with thousands separators.
func count(f float64) string {
	n := int64(math.Round(f))
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

func bytesIEC(b float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for math.Abs(b) >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 || math.Abs(b) >= 100 {
		return fmt.Sprintf("%.0f %s", b, units[i])
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

func signedBytes(b float64) string {
	if b < 0 {
		return "-" + bytesIEC(-b)
	}
	return "+" + bytesIEC(b)
}
