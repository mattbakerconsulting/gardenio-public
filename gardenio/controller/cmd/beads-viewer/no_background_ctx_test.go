package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Read-bearer enforcement (gd-akDjTS2eh7) only works if every daemon call made
// on behalf of a request carries that request's context, because the user's
// bearer rides on it. A daemon call fed context.Background()/TODO() silently
// drops the bearer: with enforcement on it fails closed, with it off it reads
// as the shared service identity. This guard fails the build on that pattern.
//
// The scan is deliberately broader than "functions that take an *http.Request":
// helpers reached from a handler (template builders, collectors) usually take a
// ctx instead, so any daemon call in the package fed a fresh root context is a
// violation unless its enclosing function is allowlisted below.

// scanScope says what "reaches the daemon" means in one package: the names a
// daemon client goes by, and the receiver names whose methods fan out to it.
type scanScope struct {
	dir        string
	daemonVals map[string]bool // identifiers/fields holding the daemon client
	recvs      map[string]bool // receivers whose methods reach the daemon
}

// ctxViewerScope is the viewer's own package; ctxGardenhubScope is the garden hub the
// viewer starts a background goroutine from (its Source is the daemon client), and
// ctxCommonsScope the Commons hub, likewise (its beads attribute events to lands).
// Add a scope here for any further package the viewer starts goroutines from.
var (
	ctxViewerScope = scanScope{
		dir:        ".",
		daemonVals: map[string]bool{"daemon": true},
		recvs:      map[string]bool{"s": true},
	}
	ctxGardenhubScope = scanScope{
		dir:        "../../internal/gardenhub",
		daemonVals: map[string]bool{"daemon": true, "src": true},
		recvs:      map[string]bool{"h": true},
	}
	ctxCommonsScope = scanScope{
		dir:        "../../internal/commons",
		daemonVals: map[string]bool{"beads": true, "Beads": true, "rd": true},
		recvs:      map[string]bool{"h": true},
	}
)

// backgroundCtxAllowlist names the enclosing functions whose daemon calls
// legitimately have no human behind them. Keep it empty unless a call truly
// cannot carry a request; prefer beadsapi.WithSystemRead on the context so the
// marker is greppable at the call's origin.
var backgroundCtxAllowlist = map[string]bool{}

// daemonCallsWithRootCtx returns "pos func" for every daemon-reaching call in
// f that is passed a context rooted in context.Background()/TODO() that carries
// no identity, minus allowlisted enclosing functions. See reachesDaemon for what
// counts as a daemon call and rootedCtxs for what counts as rooted.
func daemonCallsWithRootCtx(t *testing.T, fset *token.FileSet, f *ast.File, allow map[string]bool, sc scanScope) []string {
	t.Helper()
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || allow[fn.Name.Name] {
			continue
		}
		rooted := rootedCtxs(fn.Body)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !hasRootedCtxArg(call, rooted) || !reachesDaemon(call, sc) {
				return true
			}
			out = append(out, fset.Position(call.Pos()).String()+" "+fn.Name.Name)
			return true
		})
	}
	return out
}

// derivingCtxFuncs are the context constructors whose first argument is the
// parent: the result is rooted exactly when the parent is.
var derivingCtxFuncs = map[string]bool{
	"WithCancel":        true,
	"WithCancelCause":   true,
	"WithTimeout":       true,
	"WithTimeoutCause":  true,
	"WithDeadline":      true,
	"WithDeadlineCause": true,
	"WithValue":         true,
	"WithoutCancel":     true,
}

// rootedCtxs returns the identifiers in body that hold a context derived from
// context.Background()/TODO() with no identity applied on the way. The identity
// wrappers (beadsapi.WithUserBearer, WithMachinePrincipal, WithSystemRead) are
// not in derivingCtxFuncs, so passing a context through one clears it:
//
//	ctx := context.Background()                       // rooted
//	ctx, cancel := context.WithTimeout(ctx, d)        // still rooted
//	ctx = beadsapi.WithSystemRead(ctx)                // cleared: marked
//
// The walk is in source order and flow-insensitive beyond that (a later
// assignment overrides an earlier one), which is enough for the straight-line
// context plumbing in this package. A context that arrives as a parameter, or
// from a field, is not rooted: this guard cannot see where it came from, and
// the request-scoped ones are exactly the ones that carry the bearer.
func rootedCtxs(body *ast.BlockStmt) map[string]bool {
	rooted := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		// WithCancel & co. return (ctx, cancel); only the first names the ctx.
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
			if isRootedCtxExpr(as.Rhs[0], rooted) {
				rooted[id.Name] = true
			} else {
				delete(rooted, id.Name)
			}
		}
		return true
	})
	return rooted
}

// isRootedCtxExpr reports whether e evaluates to a rooted, identity-free context.
func isRootedCtxExpr(e ast.Expr, rooted map[string]bool) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return rooted[v.Name]
	case *ast.CallExpr:
		if isRootCtx(v) {
			return true
		}
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok || len(v.Args) == 0 {
			return false
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "context" && derivingCtxFuncs[sel.Sel.Name] {
			return isRootedCtxExpr(v.Args[0], rooted)
		}
	}
	return false
}

// hasRootedCtxArg reports whether any argument is a rooted context.
func hasRootedCtxArg(call *ast.CallExpr, rooted map[string]bool) bool {
	for _, a := range call.Args {
		if isRootedCtxExpr(a, rooted) {
			return true
		}
	}
	return false
}

// reachesDaemon reports whether call can reach the daemon with its ctx: it is
// a daemon method itself, a method on the Server receiver (s.resolveRequestOrigin
// and friends fan out to s.daemon), or it is handed the daemon as an argument
// (buildDashboardView(ctx, bead, s.daemon, ...)).
func reachesDaemon(call *ast.CallExpr, sc scanScope) bool {
	if isDaemonSelector(call.Fun, sc) {
		return true
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if recv, ok := sel.X.(*ast.Ident); ok && sc.recvs[recv.Name] {
			return true
		}
	}
	for _, a := range call.Args {
		if isDaemonValue(a, sc) {
			return true
		}
	}
	return false
}

// isDaemonSelector matches x.daemon.Method and daemon.Method (per scope names).
func isDaemonSelector(e ast.Expr, sc scanScope) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && isDaemonValue(sel.X, sc)
}

// isDaemonValue matches the daemon client itself: daemon or x.daemon.
func isDaemonValue(e ast.Expr, sc scanScope) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return sc.daemonVals[v.Name]
	case *ast.SelectorExpr:
		return sc.daemonVals[v.Sel.Name]
	}
	return false
}

// isRootCtx matches context.Background() and context.TODO().
func isRootCtx(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "context" && (sel.Sel.Name == "Background" || sel.Sel.Name == "TODO")
}

func TestNoBackgroundCtxDaemonCallsInHandlers(t *testing.T) {
	for _, sc := range []scanScope{ctxViewerScope, ctxGardenhubScope, ctxCommonsScope} {
		t.Run(sc.dir, func(t *testing.T) {
			fset := token.NewFileSet()
			entries, err := os.ReadDir(sc.dir)
			if err != nil {
				t.Fatalf("read dir: %v", err)
			}
			var violations []string
			scanned := 0
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
					continue
				}
				f, err := parser.ParseFile(fset, filepath.Join(sc.dir, name), nil, 0)
				if err != nil {
					t.Fatalf("parse %s: %v", name, err)
				}
				scanned++
				violations = append(violations, daemonCallsWithRootCtx(t, fset, f, backgroundCtxAllowlist, sc)...)
			}
			if scanned == 0 {
				t.Fatalf("no Go files scanned in %s", sc.dir)
			}
			sort.Strings(violations)
			if len(violations) > 0 {
				t.Fatalf("daemon calls fed context.Background()/TODO() drop the user's bearer; pass the request ctx "+
					"(or mark a human-less read with beadsapi.WithSystemRead):\n  %s", strings.Join(violations, "\n  "))
			}
		})
	}
}

// TestNoBackgroundCtxGuard_ReportsViolation is the positive control: the same
// scan, run over a fixture with one violation and one clean call, must report
// exactly the violation (so the guard cannot go green by matching nothing).
func TestNoBackgroundCtxGuard_ReportsViolation(t *testing.T) {
	const src = `package x

import "context"

func handler(s *S) {
	_ = s.resolveOrigin(context.Background())
	build(context.Background(), s.daemon)
	_, _ = s.daemon.GetBead(context.Background(), "a")
}

func todo(daemon D) {
	_, _ = daemon.GetBead(context.TODO(), "a")
}

func clean(ctx context.Context, s *S) {
	_, _ = s.daemon.GetBead(ctx, "a")
	_ = context.Background()
}

func allowed(s *S) {
	_, _ = s.daemon.GetBead(context.Background(), "a")
}

func viaVariable(s *S) {
	ctx := context.Background()
	_, _ = s.daemon.GetBead(ctx, "a")
}

func viaDerived(s *S) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, "k", "v")
	_, _ = s.daemon.GetBead(ctx, "a")
}

func marked(s *S) {
	ctx, cancel := context.WithCancel(beadsapi.WithSystemRead(context.Background()))
	defer cancel()
	_, _ = s.daemon.GetBead(ctx, "a")
	bearer := beadsapi.WithUserBearer(context.Background(), "tok")
	_, _ = s.daemon.GetBead(bearer, "a")
	machine := context.Background()
	machine = beadsapi.WithMachinePrincipal(machine, ts)
	_, _ = s.daemon.GetBead(machine, "a")
}

func reassigned(ctx context.Context, s *S) {
	c := context.Background()
	c = ctx
	_, _ = s.daemon.GetBead(c, "a")
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got := daemonCallsWithRootCtx(t, fset, f, map[string]bool{"allowed": true}, ctxViewerScope)
	counts := map[string]int{}
	for _, g := range got {
		counts[g[strings.LastIndex(g, " ")+1:]]++
	}
	// handler: direct daemon call, s.<method>(root ctx), and daemon passed as an arg.
	// viaVariable/viaDerived: a root context laundered through a variable or the
	// stdlib deriving constructors is still a bare root. marked/reassigned are
	// clean: an identity wrapper, or overwriting the variable with a request ctx,
	// clears the root.
	want := map[string]int{"handler": 3, "todo": 1, "viaVariable": 1, "viaDerived": 1}
	if len(counts) != len(want) {
		t.Fatalf("fixture violations = %v, want exactly %v", got, want)
	}
	for fn, n := range want {
		if counts[fn] != n {
			t.Fatalf("fixture violations = %v, want %v (clean/allowed/marked/reassigned unreported)", got, want)
		}
	}
}

// TestNoBackgroundCtxGuard_GardenhubScope is the positive control for the hub
// scope: a bare-root fetch through the hub's src is reported, the marked one is
// not (this is the gd-HMMeTk5yVz regression shape).
func TestNoBackgroundCtxGuard_GardenhubScope(t *testing.T) {
	const src = `package x

import "context"

func bad(h *Hub) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = h.refresh(ctx)
	_, _ = h.src.ListBeadsFiltered(ctx, q)
}

func good(h *Hub) {
	ctx, cancel := context.WithCancel(beadsapi.WithSystemRead(context.Background()))
	defer cancel()
	_ = h.refresh(ctx)
	_, _ = h.src.ListBeadsFiltered(ctx, q)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	got := daemonCallsWithRootCtx(t, fset, f, nil, ctxGardenhubScope)
	if len(got) != 2 {
		t.Fatalf("violations = %v, want exactly the 2 in bad()", got)
	}
	for _, g := range got {
		if !strings.HasSuffix(g, " bad") {
			t.Fatalf("violation %q not in bad()", g)
		}
	}
}
