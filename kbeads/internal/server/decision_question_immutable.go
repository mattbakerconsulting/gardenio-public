package server

import (
	"encoding/json"
	"reflect"
	"sort"

	"gardenio/kbeads/internal/model"
)

// A decision's question is fixed once it is filed (gd-qZER8OOiIb).
//
// The operator answers the prompt, options and context they were shown, and the
// Slack bridge and the requesting agent act on `chosen` against those options. A
// caller that can rewrite them after create can change what an answer means:
// swap the option a human already picked for a different action, or reword the
// prompt under an open approval. Ownership does not stop that. A static-token
// caller owns any decision whose created_by matches the X-Actor it sends, and the
// shared-queue carve-out in authorizeOwnership lets every human session, machine
// principal and bridge token past ownership on every decision (so they can
// answer it), and none of those needs to edit the question to answer it.
//
// So these fields are immutable for every caller, the filer included, on every
// generic write path: PATCH / gRPC update (checkDecisionQuestionImmutable, which
// compares post-merge with pre-merge, so a read-modify-write client echoing the
// stored values back still passes), and the single-field endpoints and the close
// body (denyDecisionStampedKeyWrite, which sees only keys). A decision that asks
// the wrong question is withdrawn with a bare close and filed again.
//
// requesting_task is part of the question too: it names the bead the decision
// is about, and a plan's approve answer seals that plan's gardener
// (spawn_lineage_plan_approval.go). If it could change after create, a filer
// could ask "approve X?", get approve, and repoint the answer at another plan.
var decisionQuestionKeys = map[string]bool{
	"prompt":          true,
	"question":        true,
	"options":         true,
	"context":         true,
	"requesting_task": true,
}

// checkDecisionQuestionImmutable rejects (400, via inputError) a PATCH that
// changes a decision's question. bead.Fields must hold the post-merge state and
// priorFields the blob as it was before the PATCH.
func checkDecisionQuestionImmutable(bead *model.Bead, priorFields json.RawMessage) error {
	if bead.Type != model.TypeDecision {
		return nil
	}
	prior := fieldsAsMap(priorFields)
	current := fieldsAsMap(bead.Fields)
	keys := make([]string, 0, len(decisionQuestionKeys))
	for k := range decisionQuestionKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		priorVal, priorOK := prior[k]
		currentVal, currentOK := current[k]
		if priorOK != currentOK || !reflect.DeepEqual(priorVal, currentVal) {
			return inputError(k + " is immutable after a decision is created; close it and file a new decision instead")
		}
	}
	return nil
}
