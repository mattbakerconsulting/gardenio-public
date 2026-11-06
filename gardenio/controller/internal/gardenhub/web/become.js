/* web/become.js: an admin's Become beside a tenant (plan gd-0SUIdKXWV3): a tenant land's owner row (project.js asks
   button()); the People lightbox that carried hidden data-become buttons (commons render.go) was the band's (gd-ZeiSZV7j2Y). Only on body data-admin with a data-become
   page, never for data-me. A click confirms, then goes to <data-become>?email=<email>; nothing is posted. */
var BECOME = (function () {
  /* The email of oidc:<email>, lowercased, else ''. */
  function emailOf(p) {
    p = String(p || '').trim().toLowerCase().replace(/^oidc:/, '');
    return /^[^@\s]+@[^@\s]+$/.test(p) ? p : '';
  }
  function may(f, p) { var e = emailOf(p); f = f || {}; return f.admin === '1' && !!f.become && !!e && e !== emailOf(f.me); }
  function href(page, p) { return page + (page.indexOf('?') < 0 ? '?' : '&') + 'email=' + encodeURIComponent(emailOf(p)); }
  function ask(name) { return 'Become ' + name + '? Your browser signs in as them until you press Stop. Writes you make are marked as made by you on their behalf.'; }
  function button(doc, f, p, name) {
    if (!may(f, p)) return null;
    var b = doc.createElement('button'); name = name || emailOf(p);
    b.type = 'button'; b.className = 'btn small ghost become'; b.textContent = 'Become';
    b.setAttribute('data-become', p); b.setAttribute('data-become-name', name); b.setAttribute('aria-label', 'Become ' + name);
    return b;
  }
  var api = { emailOf: emailOf, may: may, href: href, ask: ask, button: button };
  if (typeof document === 'undefined') return api;
  var B = document.body.dataset;
  function show(r) { if (r && r.querySelectorAll) [].forEach.call(r.querySelectorAll('button[data-become]'), function (b) { b.hidden = !may(B, b.getAttribute('data-become')); }); }
  show(document);   // what was drawn before this ran
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('button[data-become]');
    if (!b) return;
    e.preventDefault();
    var p = b.getAttribute('data-become');
    if (may(B, p) && window.confirm(ask(b.getAttribute('data-become-name') || emailOf(p)))) window.location.assign(href(B.become, p));
  });
  api.button = function (doc, f, p, name) { return button(doc || document, f || B, p, name); };
  return api;
})();
if (typeof module !== 'undefined') module.exports = BECOME;
