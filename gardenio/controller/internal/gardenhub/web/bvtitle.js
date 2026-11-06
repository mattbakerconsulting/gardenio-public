/* web/bvtitle.js: the Workbench head's title (#bv-title), clamped to two lines by garden.css, keeps the whole in its tooltip
   (gd-Sl7E3J3kd6), and a tap opens it, as a phone shows no tooltip (gd-2UXcdhdN6c); a new title starts clamped. Not
   :focus, which web/workbench.js's open() gives it. */
(function () {
  var titleEl = document.getElementById('bv-title');
  if (!titleEl || typeof MutationObserver === 'undefined') return;
  titleEl.dataset.unclamp = '';
  titleEl.addEventListener('click', function () { titleEl.classList.toggle('open'); });
  new MutationObserver(function () { var t = titleEl.textContent.trim(); if (titleEl.title !== t) { titleEl.title = t; titleEl.classList.remove('open'); } })
    .observe(titleEl, { childList: true, characterData: true, subtree: true });
})();
