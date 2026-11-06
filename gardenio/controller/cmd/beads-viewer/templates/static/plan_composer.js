// plan_composer.js — Slack-like send keybinding for the plan-agent composer
// (task ka-0000002000, Phase B of the chat-UX scope on plan epic ka-0000004110;
// operator ask ka-0000000394: "enter should send the message, not create a
// newline. shift+enter can be newline as is traditional").
//
// The .plan-assistant-form <textarea name=message> in plan_agent.html has no
// keydown handler of its own, so a bare Enter inserts a newline and sending
// requires clicking Send. This shim makes the composer behave like Slack:
//
//   - bare Enter          → submit the form (send the message)
//   - Shift+Enter         → fall through to a native newline (unchanged)
//   - Enter mid-IME       → fall through (never send a half-composed CJK/emoji
//                           string): both evt.isComposing and the legacy
//                           keyCode===229 sentinel are treated as "still typing".
//
// Send routes through form.requestSubmit() so the WHOLE existing submit
// lifecycle runs exactly as a Send click would: the `required`/maxlength
// validation, the htmx hx-post, and the hx-on::before/after-request +
// planComposerAfterRequest clear-on-send / restore-on-failure dance in
// plan_events.js (ka-0000002657, ka-0000006168). We deliberately do NOT call
// hx-post ourselves — requestSubmit fires the form's real submit path so the
// Phase C native method+action fallback (ka-0000005067) also still applies if
// htmx has not (re)processed the swapped-in form.
//
// Document-level delegation (not a per-node listener) so it keeps working after
// every SSE outerHTML swap of #plan-agent re-creates the textarea — there is
// nothing to re-bind.
(function () {
  'use strict';

  if (typeof document === 'undefined' || typeof document.addEventListener !== 'function') {
    return;
  }

  // isPlanComposerInput reports whether the event target is the plan-agent
  // compose textarea inside a .plan-assistant-form. Scoped tightly so Enter
  // keeps its native newline behaviour in every OTHER textarea on the page
  // (the inline reword / summary / doc editors, the critique compose box, etc.).
  function planComposerForm(target) {
    if (!target || !target.classList || !target.classList.contains('plan-assistant-input')) {
      return null;
    }
    return target.closest ? target.closest('.plan-assistant-form') : null;
  }

  // submitForm runs the form's real submit path. requestSubmit() fires the
  // submit event (so htmx's hx-post + the hx-on lifecycle run and native
  // validation gates empty/too-long messages) exactly as clicking Send does;
  // the dispatchEvent fallback covers engines without requestSubmit.
  function submitForm(form) {
    if (typeof form.requestSubmit === 'function') {
      form.requestSubmit();
    } else {
      form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    }
  }

  document.addEventListener('keydown', function (evt) {
    if (evt.key !== 'Enter') return;
    // Shift+Enter → traditional newline; leave the keystroke alone.
    if (evt.shiftKey) return;
    // Any other modifier chord (Ctrl/Cmd/Alt+Enter) is not our gesture; don't
    // hijack it — let the browser / other handlers see it untouched.
    if (evt.ctrlKey || evt.metaKey || evt.altKey) return;
    // Mid-IME composition: Enter is committing the candidate, not sending.
    // evt.isComposing is the standard signal; keyCode 229 is the legacy
    // sentinel some IMEs still report on the commit keydown.
    if (evt.isComposing || evt.keyCode === 229) return;

    var form = planComposerForm(evt.target);
    if (!form) return;

    // This Enter is a send: swallow the newline the textarea would insert and
    // route through the form's submit path instead.
    evt.preventDefault();
    submitForm(form);
  });
})();
