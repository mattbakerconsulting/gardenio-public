// Package landmap is the map: the Lands API (lands/contract, the
// /lands/v1/ vocabulary gl and the web UI speak) served in-process next to
// the daemon. Door D.6 (gd-yKOBNCMyNk), contract design gd-EWSKPH7pa5 §3 and
// §8 steps 3-4, reconciled for owner decision gd-BTHHmA2rYc = edge-route: no
// land pod; the daemon mounts this package under /v1, so the land door
// (/land/<name>/v1/*, D.2) reaches it as /land/<name>/v1/lands/v1/lands/<name>/...
//
// This is the only package in kbeads that imports the contract
// (gardenio/lands/contract/landsv1); depguard in .golangci.yml and
// TestOnlyLandmapImportsTheContract hold that. It translates vocabulary to
// bead types, fields and daemon routes and back, and nothing else:
//
//   - It never reads the store. Every read and write is a call to one of the
//     daemon's own HTTP routes (routeTable), made in-process with the caller's
//     request context, so the user's bearer principal goes through the same
//     scoped-principal engine and D.3 action table as a kd call would.
//   - Its tables are data (tables.go): the daemon routes it calls, each
//     contract verb with the bead types and action-table action it needs, the
//     bead fields each noun projects, the event types it may ever stream and
//     the response headers it may ever send. The server package's tests check
//     the routes and verbs against registerRoutes and actionTable.
//   - Responses are contract DTOs only. Bead records are decoded into the
//     hand-written structs in records.go, whose keys are the TypeRows fields;
//     no bead type name, field key or daemon path is ever written out, and
//     upstream error text becomes a problem code (problem.go).
//
// Tenant response hygiene (D.8 gd-enMSApFne1): Guard, around the daemon's
// whole HTTP chain, holds a response its middleware or mux writes on a Lands
// API path to the same rules (problem+json, ResponseHeaders only); free text
// goes through the advisory prose scrub (scrub.go); and the response
// denylist (denylist.go: the contract's hygiene_denylist.txt plus rules
// generated from the tables here) runs over every live answer, counted in
// DenylistHits, and over every response the tests record, with golden_test.go
// replaying the contract's request fixtures through every verb.
//
// Guard counts every Lands API response in Requests (requests.go), by the
// land the map admitted, the op, the code and the caller's class: the
// v1-beta exit's measure of a land's use (lands/contract/V1_BETA_EXIT.md).
package landmap
