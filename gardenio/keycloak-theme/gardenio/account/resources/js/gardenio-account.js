// gardenio account console: two fixes CSS cannot make (gd-ROY5CMuLfW).
//
// index.ftl loads this as a module in <head>, before the React app mounts, so
// everything here is event delegation or a MutationObserver: nothing assumes
// the console's markup is already there, and nothing replaces React's.

// PatternFly's Page treats a page narrower than its xl breakpoint as a phone:
// the sidebar is an overlay drawer, and it closes only on a tap in the main
// pane, not when a link in it is chosen. Choosing one left the drawer over the
// page it had just opened. Close it through the hamburger itself, so React's
// own open/closed state stays the one that is drawn.
const PF_XL = 1200;

document.addEventListener('click', (event) => {
  const link = event.target.closest?.('.pf-v5-c-page__sidebar a.pf-v5-c-nav__link');
  const page = document.querySelector('.pf-v5-c-page');
  const sidebar = document.querySelector('.pf-v5-c-page__sidebar');
  const toggle = document.getElementById('nav-toggle');
  if (!link || !page || !sidebar || !toggle) return;
  if (page.clientWidth >= PF_XL || !sidebar.classList.contains('pf-m-expanded')) return;
  toggle.click();
  // The chosen link is now hidden; take focus to the page it opened.
  document.querySelector('.pf-v5-c-page__main')?.focus({ preventScroll: true });
});

// Icon-only kebab toggles ship without a name (axe button-name, critical): the
// masthead's account menu on a phone, and each Signing in row's menu on a
// phone, whose one item is the row's own "Set up ..." link (hidden there).
function nameOf(toggle) {
  if (toggle.dataset.testid === 'options-kebab-toggle') return 'Account menu';
  const twin = toggle.parentElement?.querySelector('.pf-v5-c-button.pf-m-link');
  return twin?.textContent.trim() || 'More actions';
}

function nameToggles() {
  for (const toggle of document.querySelectorAll(
    '.pf-v5-c-menu-toggle.pf-m-plain:not([aria-label]):not([aria-labelledby])',
  )) {
    if (!toggle.textContent.trim()) toggle.setAttribute('aria-label', nameOf(toggle));
  }
}

let queued = false;
new MutationObserver(() => {
  if (queued) return;
  queued = true;
  requestAnimationFrame(() => {
    queued = false;
    nameToggles();
  });
}).observe(document.documentElement, { childList: true, subtree: true });
