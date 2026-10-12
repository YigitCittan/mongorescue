/**
 * MongoRescue dashboard: the pager of the Jobs table.
 *
 * GET /api/v1/jobs pages with a cursor (limit and after, the next page's after in
 * meta.next), filters and sorts on the server; tables.js loads the page of the
 * Jobs filters and column sort (tvFetchJobs) and calls jobsPagerRender when it
 * changes. The pager shows the rows per page, the range of the total and
 * Previous / Next (a cursor pages forward one page at a time; going back reuses
 * the cursors seen).
 *
 * Loaded after app.js and trust.js (mergeTranslations), before tables.js; uses
 * app.js's t, tf, escapeHtml, setText, formatCount, storageSet, pageSizeKey and
 * PAGE_SIZES, and tables.js's tablesJobsPage, tablesJobsGo and tablesJobsResize.
 * Server values reach the DOM only through textContent or escapeHtml().
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const JOBSPAGER_TRANSLATIONS = {
  en: { pager: { jobs_label: "Job pages" } },
  tr: { pager: { jobs_label: "Görev sayfaları" } },
  de: { pager: { jobs_label: "Job-Seiten" } },
  es: { pager: { jobs_label: "Páginas de tareas" } },
  fr: { pager: { jobs_label: "Pages des tâches" } },
  zh: { pager: { jobs_label: "任务分页" } },
  ja: { pager: { jobs_label: "ジョブのページ" } },
  ru: { pager: { jobs_label: "Страницы заданий" } },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(JOBSPAGER_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], JOBSPAGER_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// Shows the page of the Jobs table in its pager; hidden until a page loaded, on an
// error and without jobs.
function jobsPagerRender() {
  const nav = document.getElementById("jobs-pager");
  if (!nav || typeof tablesJobsPage !== "function") return;
  const p = tablesJobsPage();
  nav.hidden = !p.loaded || !!p.error || p.total === 0;
  if (nav.hidden) return;
  const from = p.count > 0 ? p.index * p.size + 1 : 0;
  const to = p.index * p.size + p.count;
  setText("jobs-pager-range", tf("pager.range", { from: formatCount(from), to: formatCount(to), total: formatCount(p.total) }));
  const size = document.getElementById("jobs-page-size");
  if (size && size.value !== String(p.size)) size.value = String(p.size);

  const pages = document.getElementById("jobs-pager-pages");
  if (!pages) return;
  const focused = pages.contains(document.activeElement) ? document.activeElement.dataset.jobsPage : "";
  const button = (dir, html, disabled) =>
    `<button type="button" class="pager-btn pager-nav" data-jobs-page="${dir}"${disabled ? " disabled" : ""}>${html}</button>`;
  const html = [
    button("prev", `<span aria-hidden="true">‹</span> ${escapeHtml(t("pager.previous"))}`, p.index <= 0),
    `<span class="pager-btn active" aria-current="page">${escapeHtml(formatCount(p.index + 1))}</span>`,
    button("next", `${escapeHtml(t("pager.next"))} <span aria-hidden="true">›</span>`, !p.hasNext)
  ].join("");
  if (pages.dataset.html !== html) {
    pages.innerHTML = html;
    pages.dataset.html = html;
    // Keep keyboard focus in the pager after it is redrawn.
    if (focused) {
      const again = pages.querySelector(`[data-jobs-page="${focused === "next" ? "next" : "prev"}"]:not([disabled])`);
      if (again) again.focus();
    }
  }
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

document.addEventListener("click", (e) => {
  const btn = e.target instanceof Element ? e.target.closest("[data-jobs-page]") : null;
  if (!btn || btn.disabled || typeof tablesJobsGo !== "function") return;
  tablesJobsGo(btn.dataset.jobsPage === "next" ? 1 : -1);
});

document.addEventListener("change", (e) => {
  const el = e.target;
  if (!(el instanceof HTMLSelectElement) || !el.hasAttribute("data-jobs-page-size")) return;
  const n = Number(el.value);
  if (!PAGE_SIZES.includes(n) || typeof tablesJobsResize !== "function") return;
  storageSet(pageSizeKey("jobs"), String(n));
  tablesJobsResize();
});
