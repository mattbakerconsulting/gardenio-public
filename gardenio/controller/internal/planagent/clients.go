package planagent

import (
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
)

// Compile-time proof that the REAL daemon + coopmux clients satisfy the engine's
// narrow interfaces. This is the whole point of lifting the engine here (task
// ka-0000002739): the controller-side reactive-collab reactor (epic
// ka-0000002170) constructs a *planagent.Engine with its OWN *beadsapi.Client
// and *coopmuxapi.Client — not the viewer's — and drives byte-identical
// spawn/wake/prompt logic. If a future interface change breaks either client,
// this fails to compile instead of silently forking the two call paths.
var (
	_ DaemonClient = (*beadsapi.Client)(nil)
	_ NudgeClient  = (*coopmuxapi.Client)(nil)
)
