package main

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// nudgeFallbackBound is how many work-prompt injects in a row may fall back to
// the built-in prompt before gb init exits non-zero (gd-PPzLrS2k8e). Each inject
// is one coop session, so at the ~10-minute session cadence of a dead-looping
// agent this is ~30 minutes. The container's RestartPolicy=Always then restarts
// gb init as a fresh process, with a fresh daemon client and the pod-spec env.
const nudgeFallbackBound = 3

// consecutiveNudgeFallbacks counts work-prompt injects in a row that used the
// built-in fallback. A resolved prompt resets it. runInitLoop reads it at the
// top of each restart iteration.
var consecutiveNudgeFallbacks atomic.Int32

// nudgeFallbacksExhausted reports the current fallback streak and whether it
// has reached nudgeFallbackBound.
func nudgeFallbacksExhausted() (int32, bool) {
	n := consecutiveNudgeFallbacks.Load()
	return n, n >= nudgeFallbackBound
}

// lastGoodNudge holds the last template resolved from config for each prompt
// type, so a later resolution failure in this process can re-use the real
// prompt instead of the bare fallback.
var lastGoodNudge = struct {
	sync.Mutex
	byType map[string]string
}{byType: map[string]string{}}

func rememberNudgeTemplate(promptType, tmpl string) {
	lastGoodNudge.Lock()
	defer lastGoodNudge.Unlock()
	lastGoodNudge.byType[promptType] = tmpl
}

func lastGoodNudgeTemplate(promptType string) (string, bool) {
	lastGoodNudge.Lock()
	defer lastGoodNudge.Unlock()
	tmpl, ok := lastGoodNudge.byType[promptType]
	return tmpl, ok
}

// resolveNudgeMessage resolves the work-prompt nudge text from config, falling
// back to a safe default. Shared by injectPrompt (startup + working/idle
// re-injects) and recoverFromErrorAndNudge (error-screen recovery).
//
// On a resolution failure it logs the reason, then re-uses the last template
// this process resolved for the same prompt type. With no such template it
// builds fallbackNudgeMessage and counts it toward nudgeFallbackBound.
//
// A session resumed while its own bead still carries an unread CI wait leads
// with ciWaitWakeHint, so the woken agent reads its CI result first.
func resolveNudgeMessage() string {
	return ciWaitWakeHint() + resolveWorkNudge()
}

// resolveWorkNudge is resolveNudgeMessage without the CI-wait lead-in.
func resolveWorkNudge() string {
	promptType := detectNudgeType()
	vars := buildNudgeVars()
	tmpl, err := resolveNudgeTemplate(promptType)
	if err == nil {
		rememberNudgeTemplate(promptType, tmpl)
		consecutiveNudgeFallbacks.Store(0)
		return substituteNudgeVars(tmpl, vars)
	}
	if cached, ok := lastGoodNudgeTemplate(promptType); ok {
		logInit("WARNING: nudge-prompt resolution failed: %v — re-using the last resolved %q template", err, promptType)
		consecutiveNudgeFallbacks.Store(0)
		return substituteNudgeVars(cached, vars)
	}
	n := consecutiveNudgeFallbacks.Add(1)
	logInit("WARNING: nudge-prompt resolution failed: %v — using fallback (%d/%d in a row)", err, n, nudgeFallbackBound)
	return fallbackNudgeMessage(vars)
}

// fallbackNudgeMessage is the built-in work prompt used when no nudge-prompts
// template resolves. It names the role, project and task, and tells the agent to
// gb stop when it has nothing to do: the old bare "Run gb ready to find work."
// left a plan gardener running gb ready and ending its turn 73 times in 5 h
// (gd-PPzLrS2k8e).
func fallbackNudgeMessage(vars nudgeVars) string {
	var b strings.Builder
	if vars.Role != "" {
		fmt.Fprintf(&b, "You are a `%s` agent on project `%s`.", vars.Role, vars.Project)
	} else {
		fmt.Fprintf(&b, "You are an agent on project `%s`.", vars.Project)
	}
	b.WriteString(" (Your configured work prompt could not be loaded; this is the built-in fallback.)")
	if vars.BoatPrompt != "" {
		b.WriteString(" Here is your task: ")
		b.WriteString(vars.BoatPrompt)
	}
	b.WriteString(vars.TaskHint)
	if vars.BoatPrompt == "" && vars.TaskHint == "" {
		b.WriteString(" Run `gb ready` to find work for your role and claim one task.")
	}
	b.WriteString(" When you have done what you can, or there is no work for you, call `gb stop` with a wrap-up — do not end the turn just to wait.")
	return b.String()
}
