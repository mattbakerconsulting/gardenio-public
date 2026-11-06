package metrics

// Cost-source guard (T1, epic ka-0000000758 — "LiteLLM-only cost estimation").
//
// These tests lock the central invariant of the LiteLLM-only pivot: USD cost in
// the gardenio/kbeads Go trees may ONLY originate from a LiteLLM endpoint or a
// LiteLLM-populated bead field. No surface may ever reintroduce a hardcoded
// price table or multiply a token count by a price/rate to *estimate* cost.
//
// The guards walk the real source trees and fail the build if a forbidden
// construct reappears. They are AST-based rather than grep-based on purpose:
//   - comments and string literals are NOT identifiers, so the design doc's
//     legitimate history references ("…the dead spend_forwarder.go…") and the
//     ratelimit-probe's "~$0.000003/call" comment never trip the guard;
//   - a pointer-type declaration (`agentInputTokens *prometheus.GaugeVec`) is a
//     StarExpr, not a multiplication, so it is not mistaken for token×rate math.
//
// If a future change legitimately needs one of these constructs, it is almost
// certainly reintroducing a non-LiteLLM cost method — which is exactly what this
// epic deleted. Re-read gardenio/docs/litellm-only-cost-estimation-design.md §6
// before touching this file.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// ascendToGoMod walks up from start (a file or dir) until it finds a directory
// containing a go.mod, returning that directory; "" if none is found.
func ascendToGoMod(start string) string {
	dir := start
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// controllerRoot returns the gardenio/controller module root.
//
// CI-robustness (ka-0000008138): the RWX-era `test-gardenio` task ran `cd
// gardenio/controller` with `GOWORK=off` and a filtered checkout that does NOT
// materialize go.work (or the kbeads tree). The old detector walked up for
// go.work and fatally failed with "could not locate repo root (go.work) above
// /". Instead, anchor on this test file's OWN path via runtime.Caller(0) and
// walk up to the nearest go.mod — which is the controller module root and is
// always present, since this test lives inside it. A cwd-based walk is kept as
// a fallback for any build mode where the caller path is unusable.
func controllerRoot(t *testing.T) string {
	t.Helper()
	if _, self, _, ok := runtime.Caller(0); ok {
		if _, err := os.Stat(self); err == nil {
			if root := ascendToGoMod(self); root != "" {
				return root
			}
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if root := ascendToGoMod(wd); root != "" {
			return root
		}
	}
	t.Fatalf("could not locate controller module root (go.mod) via runtime.Caller or cwd")
	return ""
}

// repoRoot derives the monorepo root from the controller module root. The
// controller always lives at <repo>/gardenio/controller, so the repo root is two
// directories up. Used only for cosmetic relative paths in violation messages
// and to locate the optional kbeads tree.
func repoRoot(controllerDir string) string {
	return filepath.Dir(filepath.Dir(controllerDir))
}

// costSourceTrees are the Go module subtrees that touch cost/spend (design-doc
// §3 cost producers/readers). The controller tree is ALWAYS scanned. The kbeads
// tree is included only when present: filtered checkouts
// may not materialize it, so its absence is logged and the walk proceeds over
// controller alone — never a hard failure. Controller's hundreds of .go files
// keep TestCostGuard_ScansNonTrivially (>50) non-vacuous in CI.
func costSourceTrees(t *testing.T, controllerDir string) []string {
	t.Helper()
	trees := []string{controllerDir}
	kbeads := filepath.Join(repoRoot(controllerDir), "kbeads")
	if fi, err := os.Stat(kbeads); err == nil && fi.IsDir() {
		trees = append(trees, kbeads)
	} else {
		t.Logf("kbeads tree absent at %s (filtered CI checkout); "+
			"scanning the controller tree only", kbeads)
	}
	return trees
}

// walkGoFiles invokes fn for every non-generated, non-test .go file under root.
// _test.go files are skipped so the guard does not flag its own forbidden-string
// fixtures; generated code (gen/, *.pb.go) is skipped as out-of-scope.
func walkGoFiles(t *testing.T, root string, fn func(path string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == "gen" || base == "testdata" || base == "vendor" || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".pb.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		f, perr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if perr != nil {
			// A file we cannot parse is not a guard failure; surface it loudly so
			// a genuinely broken tree is noticed, but keep walking.
			t.Logf("skipping unparseable file %s: %v", path, perr)
			return nil
		}
		fn(path, f, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// forbiddenCostIdentifiers are identifier *substrings* (lowercased) that name a
// hardcoded price table or a per-token rate. Their presence as a real Go
// identifier (const/var/field/func) means someone reintroduced token×rate cost
// math. Curated to NOT collide with any legitimate identifier in the tree
// (verified by TestCostGuard_CleanTreeHasNoForbiddenIdentifiers staying green):
// e.g. plain "rate"/"price" are deliberately excluded because rate-limiter code
// (ratePerSec, burst) and unrelated pricing-free code use them.
var forbiddenCostIdentifiers = []string{
	"costpertoken",
	"pricepertoken",
	"ratepertoken",
	"tokenprice",
	"tokenrate",
	"tokencost",
	"priceperm",       // price per million tokens
	"priceper1k",      //
	"priceper1m",      //
	"per1ktoken",      //
	"per1mtoken",      //
	"perktoken",       //
	"permtoken",       //
	"usdpertoken",     //
	"costperinput",    //
	"costperoutput",   //
	"inputtokenprice", //
	"outputtokenprice",
	"modelpricing",
	"pricetable",
	"pricingtable",
}

// TestCostGuard_NoPriceTableIdentifiers (G1) fails if any Go identifier in the
// cost-source trees names a hardcoded price table or a per-token rate. A LiteLLM-
// only platform has no such symbol — every USD figure is read from LiteLLM or a
// LiteLLM-populated bead field, never computed from a baked-in price.
func TestCostGuard_NoPriceTableIdentifiers(t *testing.T) {
	controllerDir := controllerRoot(t)
	root := repoRoot(controllerDir)
	var violations []string
	for _, tree := range costSourceTrees(t, controllerDir) {
		walkGoFiles(t, tree, func(path string, f *ast.File, fset *token.FileSet) {
			ast.Inspect(f, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				lower := strings.ToLower(id.Name)
				for _, bad := range forbiddenCostIdentifiers {
					if strings.Contains(lower, bad) {
						rel, _ := filepath.Rel(root, path)
						violations = append(violations,
							rel+":"+strconv.Itoa(fset.Position(id.Pos()).Line)+
								" identifier "+id.Name+" matches forbidden price-table pattern "+bad)
					}
				}
				return true
			})
		})
	}
	if len(violations) > 0 {
		t.Fatalf("forbidden price-table / per-token-rate identifiers found "+
			"(LiteLLM is the only cost source — no hardcoded prices allowed):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// tokenIdentName reports whether expr is an identifier/selector whose final name
// component contains "token" (e.g. inputTokens, u.OutputTokens, usage.CacheRead
// is NOT matched — only names with "token"). Used to detect token×rate math.
func tokenIdentName(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return strings.Contains(strings.ToLower(e.Name), "token")
	case *ast.SelectorExpr:
		return strings.Contains(strings.ToLower(e.Sel.Name), "token")
	case *ast.ParenExpr:
		return tokenIdentName(e.X)
	case *ast.CallExpr:
		// e.g. float64(inputTokens) — unwrap a single-arg conversion.
		if len(e.Args) == 1 {
			return tokenIdentName(e.Args[0])
		}
	}
	return false
}

// isRateOperand reports whether expr is a scalar "rate" operand on the non-token
// side of a token×rate product. It matches:
//   - a numeric literal (inputTokens * 0.000003), and
//   - a NAMED const/var/field that is NOT itself token-named — an Ident
//     (inputTokens * modelRate) or a SelectorExpr (tokens * cfg.PricePerToken).
//
// Catching the named-rate operand (the Ident/SelectorExpr cases) is the point of
// this helper: the literal-only predicate it replaced missed tokens*costPerToken
// and tokens*modelRate, where the rate is a const/var rather than a baked-in
// number (gd-HpM7hACS78). Excluding token-named operands preserves the deliberate
// token×token carve-out (cache ratios, tokens-per-window), so a product whose
// BOTH sides are token-named is still not flagged. float64(...)/parens
// conversions are unwrapped so float64(modelRate) is still seen as a rate.
func isRateOperand(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.INT || e.Kind == token.FLOAT
	case *ast.Ident:
		return !tokenIdentName(e)
	case *ast.SelectorExpr:
		return !tokenIdentName(e)
	case *ast.ParenExpr:
		return isRateOperand(e.X)
	case *ast.CallExpr:
		if len(e.Args) == 1 {
			return isRateOperand(e.Args[0])
		}
	}
	return false
}

// TestCostGuard_NoTokenTimesRateMath (G1) fails if any multiplication in the
// cost-source trees multiplies a token-named value by a rate operand — the
// canonical shape of a token×price cost estimate. The rate operand may be a
// numeric literal (tokens * 0.000003) OR a named const/var/field that is not
// itself token-named (tokens * costPerToken, tokens * modelRate, tokens *
// cfg.PricePerToken) — closing the named-rate blind spot (gd-HpM7hACS78).
// Pointer type declarations (`*prometheus.GaugeVec`) are StarExpr, not
// BinaryExpr, so they are not matched; token×token products (cache ratios, etc.)
// are not matched because both operands are token-named, so neither qualifies as
// a rate operand.
func TestCostGuard_NoTokenTimesRateMath(t *testing.T) {
	controllerDir := controllerRoot(t)
	root := repoRoot(controllerDir)
	var violations []string
	for _, tree := range costSourceTrees(t, controllerDir) {
		walkGoFiles(t, tree, func(path string, f *ast.File, fset *token.FileSet) {
			ast.Inspect(f, func(n ast.Node) bool {
				bin, ok := n.(*ast.BinaryExpr)
				if !ok || bin.Op != token.MUL {
					return true
				}
				// token * <rate operand>  OR  <rate operand> * token, where the
				// rate operand is a literal or a non-token-named const/var/field.
				if (tokenIdentName(bin.X) && isRateOperand(bin.Y)) ||
					(tokenIdentName(bin.Y) && isRateOperand(bin.X)) {
					rel, _ := filepath.Rel(root, path)
					violations = append(violations,
						rel+" near "+fset.Position(bin.Pos()).String())
				}
				return true
			})
		})
	}
	if len(violations) > 0 {
		t.Fatalf("token×rate multiplication found (cost must come from LiteLLM, "+
			"never tokens×price):\n  %s", strings.Join(violations, "\n  "))
	}
}

// deadCostSymbols are the identifiers of the non-LiteLLM / dead cost plumbing
// deleted in I1 (ka-0000000717). They must never reappear as real Go
// identifiers. Comment-only history references (e.g. spend.go's "…the dead
// spend_forwarder…") are not identifiers and so do not trip this guard.
//
// NOTE: resolveSpendAccount is deliberately NOT listed — it is live, wiring the
// session-close reconciler's account label (main.go:544, podspec.go:1059).
var deadCostSymbols = []string{
	"newSpendForwarder",
	"spendForwarderConfig",
	"syncAccountSpendFromLiteLLM",
}

// TestCostGuard_DeadPathSymbolsAbsent (G4) fails if a deleted dead-path symbol
// is reintroduced as a Go identifier, locking the I1 deletion against regression.
func TestCostGuard_DeadPathSymbolsAbsent(t *testing.T) {
	controllerDir := controllerRoot(t)
	root := repoRoot(controllerDir)
	dead := make(map[string]bool, len(deadCostSymbols))
	for _, s := range deadCostSymbols {
		dead[s] = true
	}
	var violations []string
	for _, tree := range costSourceTrees(t, controllerDir) {
		walkGoFiles(t, tree, func(path string, f *ast.File, fset *token.FileSet) {
			ast.Inspect(f, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if dead[id.Name] {
					rel, _ := filepath.Rel(root, path)
					violations = append(violations,
						rel+" reintroduces dead symbol "+id.Name+" at "+fset.Position(id.Pos()).String())
				}
				return true
			})
		})
	}
	if len(violations) > 0 {
		t.Fatalf("deleted dead-path cost symbols reintroduced (I1 ka-0000000717 "+
			"removed these — cost is LiteLLM-only):\n  %s", strings.Join(violations, "\n  "))
	}
}

// TestCostGuard_ScansNonTrivially is a meta-guard: it asserts the AST walk
// actually visits a substantial number of files in the cost-source trees. Without
// it, a refactor that broke pathing (wrong root, over-eager SkipDir) would turn
// every guard above into a vacuous pass — green but enforcing nothing.
func TestCostGuard_ScansNonTrivially(t *testing.T) {
	controllerDir := controllerRoot(t)
	count := 0
	for _, tree := range costSourceTrees(t, controllerDir) {
		walkGoFiles(t, tree, func(_ string, _ *ast.File, _ *token.FileSet) { count++ })
	}
	// The controller + kbeads trees hold hundreds of .go files; 50 is a floor
	// far below reality but well above "walk silently found nothing".
	if count < 50 {
		t.Fatalf("cost-source guard scanned only %d files; expected the walk to "+
			"cover the controller+kbeads trees — pathing is likely broken", count)
	}
}

// TestCostGuard_DetectorsCatchSyntheticViolations proves the AST detectors are
// not vacuously true: a fabricated token×rate product and a forbidden identifier
// must both be flagged when fed through the same predicates the guards use.
func TestCostGuard_DetectorsCatchSyntheticViolations(t *testing.T) {
	// Covers both rate-operand shapes: literals (inputTokens*0.000003,
	// 1500*inputTokens) AND named const/var/field rates that the literal-only
	// detector used to miss (inputTokens*modelRate, inputTokens*c.Price). The
	// token×token product (inputTokens*outputTokens) must NOT be flagged, since
	// both operands are token-named and so neither qualifies as a rate operand.
	const bad = `package p
const costPerToken = 0.000003
type cfg struct{ Price float64 }
func f(inputTokens, outputTokens, modelRate float64, c cfg) float64 {
	x := inputTokens * 0.000003
	y := 1500 * inputTokens
	z := inputTokens * modelRate
	w := inputTokens * c.Price
	ratio := inputTokens * outputTokens
	return x + y + z + w + ratio
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", bad, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}

	identHit := false
	mulHits := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			lower := strings.ToLower(id.Name)
			for _, b := range forbiddenCostIdentifiers {
				if strings.Contains(lower, b) {
					identHit = true
				}
			}
		}
		if bin, ok := n.(*ast.BinaryExpr); ok && bin.Op == token.MUL {
			if (tokenIdentName(bin.X) && isRateOperand(bin.Y)) ||
				(tokenIdentName(bin.Y) && isRateOperand(bin.X)) {
				mulHits++
			}
		}
		return true
	})

	if !identHit {
		t.Error("forbidden-identifier detector failed to flag costPerToken")
	}
	// Four token×rate products must be flagged; the token×token ratio must not.
	if mulHits != 4 {
		t.Errorf("token×rate detector flagged %d products, want 4 "+
			"(inputTokens*0.000003, 1500*inputTokens, inputTokens*modelRate, "+
			"inputTokens*c.Price — token×token ratio must NOT count)", mulHits)
	}
}
