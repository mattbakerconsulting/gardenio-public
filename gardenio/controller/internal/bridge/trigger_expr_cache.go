package bridge

import "sync"

// Process-global compiled-program cache keyed by CEL source string. Compiling
// a CEL program (parse + type-check + plan) is far more expensive than an
// evaluation, and the same trigger expression is re-evaluated on every
// Evaluate cycle — so memoising the compiled program is the single biggest
// hot-path win. The cache is shared across all trigger sources (bead, webhook,
// schedule) since the source string fully determines the program.
//
// Eviction is a simple capacity guard: when the map exceeds
// exprProgramCacheCap, it is cleared wholesale. Trigger sets are small and
// stable, so the cache reaches steady state and the clear effectively never
// fires in production; it exists only to bound the worst case where an
// unbounded stream of distinct ad-hoc `gb trigger test` expressions would
// otherwise accumulate. A wholesale clear (rather than LRU bookkeeping) keeps
// the lock path trivial and contention-free.
var (
	exprProgramCacheMu sync.RWMutex
	exprProgramCache   = make(map[string]*CompiledExpr)
)

func exprProgramCacheGet(src string) (*CompiledExpr, bool) {
	exprProgramCacheMu.RLock()
	c, ok := exprProgramCache[src]
	exprProgramCacheMu.RUnlock()
	return c, ok
}

func exprProgramCachePut(src string, c *CompiledExpr) {
	exprProgramCacheMu.Lock()
	if len(exprProgramCache) >= exprProgramCacheCap {
		exprProgramCache = make(map[string]*CompiledExpr, exprProgramCacheCap)
	}
	exprProgramCache[src] = c
	exprProgramCacheMu.Unlock()
}
