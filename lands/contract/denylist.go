package contract

import _ "embed"

// HygieneDenylist is hygiene_denylist.txt, the contract's response denylist.
// The map (kbeads/internal/landmap) runs it over every live answer it writes
// and counts what it finds; the daemon image has no copy of this module's
// files, so the list travels in the binary. It is the module's only runtime
// value and its only embed (TestNoEmbed). gl imports only landsv1 (its
// TestNoBackingStoreDependencies), so the list never ships in gl, whose
// strings tripwire would otherwise find every token in it.
//
//go:embed hygiene_denylist.txt
var HygieneDenylist string
