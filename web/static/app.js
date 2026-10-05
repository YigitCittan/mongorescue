/**
 * MongoRescue dashboard client.
 *
 * Vanilla JavaScript, no build step. Security invariants:
 *  - every server-derived value is passed through escapeHtml() before it reaches innerHTML;
 *  - there are no inline event handlers: all interaction goes through delegated
 *    listeners keyed on data-action attributes, whose values are only used as data;
 *  - IDs placed in URLs are always wrapped in encodeURIComponent();
 *  - requests use the same-origin session cookie; unsafe methods carry X-CSRF-Token.
 */

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const state = {
  stats: null,
  config: null,
  jobs: [],
  backups: [],
  restores: [],
  channels: [],
  rules: [],
  connections: [],
  users: [],
  apikeys: [],
  // Recent MCP tool calls (GET /api/v1/audit), loaded when Settings → Security opens.
  audit: [],
  auditError: "",
  storageTargets: [],
  // Settings groups as returned by GET /api/v1/settings (secrets masked).
  settings: null,
  settingsError: "",
  loaded: {
    jobs: false, backups: false, restores: false, channels: false, rules: false,
    connections: false, users: false, apikeys: false, storageTargets: false, settings: false, audit: false
  }
};

// Signed-in user, CSRF token for unsafe requests and how the session was authenticated,
// with the dashboard role and the effective scope (GET /api/v1/auth/me). The scope
// only shapes the UI; the server enforces it on every request.
const auth = { user: null, csrf: "", mode: "", role: "", scope: "", keyScope: "" };

// Scopes from the least to the most privileged, and the scope of each dashboard role.
const SCOPE_RANK = { read: 1, operator: 2, admin: 3 };
const ROLE_SCOPES = { viewer: "read", operator: "operator", admin: "admin" };

// can reports whether the signed-in caller's effective scope includes scope.
function can(scope) {
  const have = SCOPE_RANK[auth.scope] || 0;
  const need = SCOPE_RANK[scope] || 4;
  return have >= need;
}

const STORAGE_KEYS = {
  theme: "mongorescue_theme"
};

// Older versions kept an API key in localStorage; it is removed on load.
const LEGACY_API_KEY_STORAGE = "mongorescue_api_key";
// A short-lived build kept a "signed in before" hint here; it is removed on load.
const LEGACY_SIGNED_IN_STORAGE = "mongorescue_signed_in";
const SAFE_METHODS = ["GET", "HEAD", "OPTIONS"];
const MIN_PASSWORD_LENGTH = 12;
const MAX_PASSWORD_BYTES = 72;

let pollTimer = null;
let sessionExpiredShown = false;

// Result of the last "Test connection" in the connection form.
const connTest = { uri: null, ok: false, saveAnyway: false };

// Result of the last "Test storage" in the storage target form, keyed by the
// serialized form payload so any edit invalidates it.
const storageTest = { sig: null, ok: false, saveAnyway: false };

// Stored secrets come back in this redacted form; sending it back unchanged
// tells the server to keep the stored value.
const MASKED_SECRET = "******";

const CHANNEL_TYPES = ["webhook", "telegram", "email", "twilio"];
const THEMES = ["system", "light", "dark"];
const POLL_INTERVAL_MS = 10000;
const ACTIVE_POLL_MS = 2000;
const ACTIVE_STATUSES = ["pending", "in_progress"];

// Operations started from this browser, keyed by record ID, so their outcome can be
// reported once the background run finishes (the API answers 202 Accepted).
const trackedOps = { backups: new Map(), restores: new Map() };
// Failed backups whose retry request is in flight (their Retry buttons stay disabled).
const retryingBackups = new Set();
// Backup shown in the details dialog, re-rendered when the list refreshes.
let detailsBackupId = "";
// Job shown in the job details dialog, and its next activations from
// GET /api/v1/jobs/{id} (refetched when the job's schedule changes).
let detailsJobId = "";
const jobNextRuns = { key: "", runs: null, failed: false, seq: 0, rpoMinutes: 0, rpoDefault: false };
// Job being edited in the job form ("" while creating a new job) and the updated_at
// it had when the form opened (sent back as the save's precondition).
let editingJobId = "";
let editingJobUpdatedAt = "";
// Jobs whose enable/disable request is in flight.
const togglingJobs = new Set();
// Runs listed in a job's run history.
const JOB_HISTORY_LIMIT = 20;
let activePollTimer = null;
const EMPTY_SHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";

let refreshInFlight = false;

// ---------------------------------------------------------------------------
// Safe localStorage access (private mode / disabled storage must not break the UI)
// ---------------------------------------------------------------------------

function storageGet(key) {
  try {
    return window.localStorage.getItem(key) || "";
  } catch (err) {
    return "";
  }
}

function storageSet(key, value) {
  try {
    if (value) {
      window.localStorage.setItem(key, value);
    } else {
      window.localStorage.removeItem(key);
    }
  } catch (err) {
    // storage unavailable: the setting simply is not persisted
  }
}

// ---------------------------------------------------------------------------
// Theme (system / light / dark), persisted under STORAGE_KEYS.theme
// ---------------------------------------------------------------------------

function storedTheme() {
  const value = storageGet(STORAGE_KEYS.theme);
  return THEMES.includes(value) ? value : "system";
}

function applyTheme(pref) {
  const root = document.documentElement;
  if (pref === "light" || pref === "dark") {
    root.setAttribute("data-theme", pref);
  } else {
    root.removeAttribute("data-theme");
  }
  document.querySelectorAll('[data-action="set-theme"]').forEach(btn => {
    btn.setAttribute("aria-pressed", String(btn.dataset.theme === pref));
  });
}

function setTheme(pref) {
  const value = THEMES.includes(pref) ? pref : "system";
  storageSet(STORAGE_KEYS.theme, value === "system" ? "" : value);
  applyTheme(value);
}

// Apply as early as possible to avoid a flash of the wrong theme.
applyTheme(storedTheme());

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

document.addEventListener("DOMContentLoaded", () => {
  if (typeof initI18n === "function") {
    initI18n();
    onLanguageChange(renderAll);
  }
  applyTheme(storedTheme());

  setupListControls();
  setupTabs();
  setupActions();
  setupModals();
  setupForms();
  setupUserMenu();
  setupRowMenu();

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && auth.user) refreshAll();
  });

  boot();
});

// ---------------------------------------------------------------------------
// Tabs
// ---------------------------------------------------------------------------

function setupTabs() {
  const tabs = Array.from(document.querySelectorAll(".tab-btn"));
  tabs.forEach(btn => {
    btn.addEventListener("click", () => activateTab(btn.dataset.tab, false));
    btn.addEventListener("keydown", (e) => {
      if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
      e.preventDefault();
      const idx = tabs.indexOf(btn);
      const next = tabs[(idx + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length];
      activateTab(next.dataset.tab, true);
    });
  });

  // #backups?status=failed&page=2 opens a tab with its list filters and page.
  const { tab, params } = readHash();
  if (LIST_KINDS.includes(tab)) applyListParams(tab, params);
  else if (typeof tablesApplyHash === "function") tablesApplyHash(tab, params);
  const fromHash = `tab-${tab}`;
  if (tab && document.getElementById(fromHash)) {
    activateTab(fromHash, false);
  }
  LIST_KINDS.forEach(kind => renderListControls(kind));
}

function activateTab(id, focus) {
  const pane = document.getElementById(id);
  if (!pane) return;
  document.querySelectorAll(".tab-btn").forEach(btn => {
    const active = btn.dataset.tab === id;
    btn.classList.toggle("active", active);
    btn.setAttribute("aria-selected", String(active));
    btn.tabIndex = active ? 0 : -1;
    if (active) btn.scrollIntoView({ block: "nearest", inline: "nearest" });
    if (active && focus) btn.focus();
  });
  document.querySelectorAll(".tab-pane").forEach(p => {
    const active = p.id === id;
    p.classList.toggle("active", active);
    p.hidden = !active;
  });
  // Settings are not polled; pick up changes made elsewhere when the tab opens.
  if (id === "tab-settings" && auth.user) loadSettings(false);
  if (id === "tab-overview" && typeof overviewRefresh === "function") overviewRefresh(false);
  if (typeof navHoldsHash === "function" && navHoldsHash()) return;
  try {
    window.history.replaceState(null, "", tabHash(id));
  } catch (err) {
    // history API unavailable (e.g. sandboxed frame): ignore
  }
}

// ---------------------------------------------------------------------------
// Delegated actions
// ---------------------------------------------------------------------------

// Rows with data-row-action open their details on click, Enter or Space. Clicks on
// controls inside the row (buttons, links, expandable errors) and text selections
// keep their own behaviour.
function runRowAction(row) {
  const id = row.dataset.id || "";
  switch (row.dataset.rowAction) {
    case "job-details":
      openJobDetails(id);
      break;
    case "backup-details":
      openBackupDetails(id);
      break;
    case "restore-details":
      openRestoreDetails(id);
      break;
  }
}

function handleRowClick(e) {
  const row = e.target.closest("tr[data-row-action]");
  if (!row || e.target.closest("a, button, input, select, textarea, label, details, summary")) return;
  const selection = window.getSelection ? String(window.getSelection()) : "";
  if (selection.trim() !== "") return;
  runRowAction(row);
}

function setupActions() {
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    const row = e.target instanceof Element && e.target.matches("tr[data-row-action]") ? e.target : null;
    if (!row) return;
    e.preventDefault();
    runRowAction(row);
  });
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-action]");
    if (!btn) {
      handleRowClick(e);
      return;
    }
    if (btn.disabled) return;
    const id = btn.dataset.id || "";
    // A menu item closes its menu first, so dialogs return focus to the "⋯" button.
    const menu = document.getElementById("row-menu");
    if (menu && menu.contains(btn)) closeRowMenu(true);
    switch (btn.dataset.action) {
      case "row-menu":
        toggleRowMenu(btn);
        break;
      case "set-theme":
        setTheme(btn.dataset.theme || "system");
        break;
      case "toggle-user-menu":
        toggleUserMenu();
        break;
      case "logout":
        logout();
        break;
      case "change-own-password":
        openPasswordModal("");
        break;
      case "change-user-password":
        openPasswordModal(id);
        break;
      case "new-user":
        openUserModal();
        break;
      case "delete-user":
        deleteUser(id);
        break;
      case "new-api-key":
        openApiKeyModal();
        break;
      case "copy-api-key":
        copyApiKey();
        break;
      case "revoke-api-key":
        revokeApiKey(id);
        break;
      case "refresh-audit":
        loadAudit();
        break;
      case "new-connection":
        openConnectionModal("");
        break;
      case "open-encryption-settings":
        activateTab("tab-settings", false);
        showSettingsSection("encryption", true);
        break;
      case "dismiss-encryption-warning":
        dismissEncryptionWarning();
        break;
      case "edit-connection":
        openConnectionModal(id);
        break;
      case "test-connection":
        testConnection(id, btn);
        break;
      case "delete-connection":
        deleteConnection(id);
        break;
      case "test-connection-form":
        testConnectionForm();
        break;
      case "picker-from-list":
        pickerFromList(btn.dataset.picker || "");
        break;
      case "settings-section":
        showSettingsSection(btn.dataset.section || "", false);
        break;
      case "new-storage":
        openStorageModal("");
        break;
      case "new-storage-for":
        openStorageModalFor(btn.dataset.select || "");
        break;
      case "edit-storage":
        openStorageModal(id);
        break;
      case "test-storage":
        testStorageTarget(id, btn);
        break;
      case "default-storage":
        setDefaultStorageTarget(id, btn);
        break;
      case "delete-storage":
        deleteStorageTarget(id);
        break;
      case "test-storage-form":
        testStorageForm();
        break;
      case "generate-key":
        generateKeyPair(btn);
        break;
      case "copy-identity":
        copyField("enc-gen-identity", "enc-copy-label");
        break;
      case "download-identity":
        downloadIdentity();
        break;
      case "use-generated-key":
        useGeneratedKey(btn);
        break;
      case "discard-generated-key":
        discardGeneratedKey(true);
        break;
      case "refresh":
        refreshAll();
        break;
      case "goto-tab":
        activateTab(btn.dataset.tab || "", true);
        break;
      case "close-modal":
        closeModal(btn.dataset.target || "");
        break;
      case "new-job":
        openJobModal();
        break;
      case "backup-now":
        openBackupNowModal();
        break;
      case "trigger-job":
        triggerJob(id, btn);
        break;
      case "delete-job":
        deleteJob(id);
        break;
      case "job-details":
        openJobDetails(id);
        break;
      case "edit-job":
        openJobModal(id);
        break;
      case "toggle-job":
        toggleJob(id, btn);
        break;
      case "copy-id":
        copyText(btn.dataset.copy || "", btn);
        break;
      case "restore-backup":
        openRestoreModal(id, btn.dataset.db || "");
        break;
      case "delete-backup":
        deleteBackup(id);
        break;
      case "retry-backup":
        retryBackup(id);
        break;
      case "clear-filters": {
        const kind = listKindOf(btn);
        if (kind) clearFilters(kind);
        break;
      }
      case "page": {
        const kind = listKindOf(btn);
        if (kind) goToPage(kind, btn.dataset.page);
        break;
      }
      case "backup-details":
        openBackupDetails(id);
        break;
      case "new-channel":
        openChannelModal("");
        break;
      case "edit-channel":
        openChannelModal(id);
        break;
      case "test-channel":
        testChannel(id, btn);
        break;
      case "delete-channel":
        deleteChannel(id);
        break;
      case "new-rule":
        openRuleModal("");
        break;
      case "edit-rule":
        openRuleModal(id);
        break;
      case "delete-rule":
        deleteRule(id);
        break;
      default:
        // Run control actions (cancel, pause/resume, logs) live in runs.js.
        handleRunAction(btn.dataset.action, id, btn);
    }
  });
}

// ---------------------------------------------------------------------------
// Server health
// ---------------------------------------------------------------------------

async function checkHealth() {
  const dot = document.getElementById("conn-status");
  try {
    const res = await fetch("/api/v1/health");
    const json = await res.json();
    const ok = res.ok && json.success;
    setConnection(dot, ok ? "ok" : "down", ok && json.data ? json.data.version : "");
    return ok ? json.data : null;
  } catch (err) {
    setConnection(dot, "down", "");
    return null;
  }
}

function setConnection(dot, status, version) {
  // Re-renders (a language change) pass no version: keep the one shown.
  if (status === "ok" && version) setAppVersion(version);
  if (!dot) return;
  dot.dataset.status = status;
  const shown = formatVersion(version);
  let label = t("nav.conn_checking");
  if (status === "ok") label = shown ? `${t("nav.conn_ok")} (${shown})` : t("nav.conn_ok");
  if (status === "down") label = t("nav.conn_down");
  dot.title = label;
  dot.setAttribute("aria-label", label);
}

// formatVersion returns the build version as shown in the header: "v0.4.1" for a
// release, other builds (such as "dev") as they are, "" when unknown.
function formatVersion(version) {
  const v = String(version || "").trim();
  if (!v) return "";
  return /^\d/.test(v) ? `v${v}` : v;
}

// setAppVersion shows the build version next to the brand. It keeps the last
// known version while the server is unreachable.
function setAppVersion(version) {
  const el = document.getElementById("app-version");
  if (!el) return;
  const shown = formatVersion(version);
  el.textContent = shown;
  el.hidden = !shown;
}

// Wrapper for authenticated API requests. Sends the session cookie, adds the CSRF
// token to unsafe methods, turns 401 into the sign-in screen and, on 403, reloads
// the session once (the CSRF token may have rotated) before retrying.
async function apiFetch(url, options = {}, retried = false) {
  const method = String(options.method || "GET").toUpperCase();
  const headers = { ...(options.headers || {}) };
  if (!SAFE_METHODS.includes(method) && auth.csrf) {
    headers["X-CSRF-Token"] = auth.csrf;
  }

  const res = await fetch(url, { ...options, method, headers, credentials: "same-origin" });
  if (res.status === 401) {
    handleUnauthorized();
    throw new Error(t("auth.session_expired"));
  }
  if (res.status === 403 && !retried) {
    if (!(await loadMe())) {
      handleUnauthorized();
      throw new Error(t("auth.session_expired"));
    }
    if (!SAFE_METHODS.includes(method)) return apiFetch(url, options, true);
  }
  return res;
}

// Parses the JSON envelope. Non-JSON bodies (proxies, HTML error pages) become a
// readable error instead of a SyntaxError; 503 during shutdown gets a clear message.
async function apiJSON(url, options) {
  const res = await apiFetch(url, options);
  const type = res.headers.get("content-type") || "";
  let json = null;
  if (type.includes("application/json")) {
    try {
      json = await res.json();
    } catch (err) {
      json = null;
    }
  }
  if (!json || typeof json !== "object") {
    throw new Error(tf("toasts.unexpected_response", { status: res.status }));
  }
  if (res.status === 403 && !json.error) {
    json.success = false;
    json.error = t("auth.forbidden");
  }
  if (res.status === 503 && /shutting down/i.test(String(json.error || ""))) {
    json.success = false;
    json.error = t("toasts.shutting_down");
  }
  if (!res.ok && !json.error) {
    json.success = false;
    json.error = tf("toasts.unexpected_response", { status: res.status });
  }
  json.httpStatus = res.status;
  return json;
}

// ---------------------------------------------------------------------------
// Data loading
// ---------------------------------------------------------------------------

async function refreshAll() {
  if (refreshInFlight || !auth.user) return;
  refreshInFlight = true;
  try {
    await Promise.all([
      checkHealth(),
      loadStats(),
      loadConnections(),
      loadStorageTargets(),
      state.loaded.settings ? null : loadSettings(false),
      loadUsers(),
      loadApiKeys(),
      loadJobs(),
      loadActiveRuns(),
      loadBackups(),
      loadRestores(),
      loadListDatabases(),
      loadNotifications()
    ]);
  } finally {
    refreshInFlight = false;
  }
  if (!auth.user) return;
  renderWarnings();
  // Integrity status: sweep, storage scans, drift warning (trust.js).
  trustRefresh();
  renderStats();
  renderJobs();
  renderBackups();
  // Restores name their source connection via the backup records loaded alongside.
  renderRestores();
  renderRules();
  scheduleActivePoll();
  // The overview's history and the jobs' sparklines (overview.js, throttled).
  if (typeof overviewRefresh === "function") overviewRefresh(false);
  // Waiting approvals and the Deleted view toggle (protection.js).
  if (typeof protectionRefresh === "function") protectionRefresh();
}

// While any backup or restore is still running, refresh those lists every few
// seconds (only while the tab is visible) so completion shows up promptly. The
// server's active counts (/api/v1/stats) cover runs on other pages or filtered out.
function hasActiveOperations() {
  const stats = state.stats || {};
  return (Number(stats.active_backups) || 0) > 0 || (Number(stats.active_restores) || 0) > 0 ||
    state.backups.some(b => ACTIVE_STATUSES.includes(b.status)) ||
    state.restores.some(r => ACTIVE_STATUSES.includes(r.status)) ||
    trackedOps.backups.size > 0 || trackedOps.restores.size > 0;
}

function scheduleActivePoll() {
  if (activePollTimer || document.hidden || !auth.user || !hasActiveOperations()) return;
  activePollTimer = setTimeout(async () => {
    activePollTimer = null;
    if (document.hidden || !auth.user) return;
    // Live progress (runs.js) refreshes with the lists every ACTIVE_POLL_MS.
    await Promise.all([loadActiveRuns(), loadBackups(), loadRestores(), loadStats()]);
    renderStats();
    scheduleActivePoll();
  }, ACTIVE_POLL_MS);
}

function trackBackup(record) {
  if (record && record.id) {
    trackedOps.backups.set(record.id, { database: record.database || "" });
    scheduleActivePoll();
  }
}

function trackRestore(record) {
  if (record && record.id) {
    trackedOps.restores.set(record.id, { database: record.target_database || "" });
    scheduleActivePoll();
  }
}

// Reports the outcome of operations started here once their record leaves the
// running state. Toasts render via textContent, so server text is never HTML.
function reportFinished(kind, records) {
  const tracked = trackedOps[kind];
  tracked.forEach((info, id) => {
    const rec = records.find(r => r.id === id);
    if (!rec) {
      tracked.delete(id);
      return;
    }
    if (ACTIVE_STATUSES.includes(rec.status)) return;
    tracked.delete(id);
    if (typeof noteAnnounced === "function") noteAnnounced(kind, id);
    const db = (kind === "backups" ? rec.database : rec.target_database) || info.database;
    // "View" opens the run's details dialog (nav.js).
    const view = typeof navOpenDetail === "function"
      ? { action: { label: t("toast.view"), run: () => navOpenDetail(kind, id) } } : undefined;
    if (rec.status === "cancelled") {
      reportCancelled(kind, rec, db);
    } else if (rec.status === "failed") {
      const key = kind === "backups" ? "toasts.backup_failed_detail" : "toasts.restore_failed_detail";
      showToast(tf(key, { db, error: truncate(errorSummary(rec.error_message), 160) }), "error", view);
    } else {
      const key = kind === "backups" ? "toasts.backup_succeeded" : "toasts.restore_succeeded";
      showToast(tf(key, { db }), "success", view);
    }
    if (typeof overviewRefresh === "function") overviewRefresh(true);
  });
}

// ---------------------------------------------------------------------------
// Load errors and unreadable records
// ---------------------------------------------------------------------------

// Loaders whose last request was answered with an error, by name (a key of
// data_health.sources), with the error. One banner lists them, so a list that failed
// to load never looks like an empty one.
const loadErrors = new Map();

// Records the answer of loader name: an error answer (success false) shows message
// or its error in the load-error banner, a successful one clears it. An expired
// session (401) is left to the sign-in flow. It returns json.success.
function noteLoad(name, json, message) {
  if (json && json.success) {
    loadErrors.delete(name);
  } else if (!json || json.httpStatus !== 401) {
    loadErrors.set(name, message || (json && json.error) || t("toasts.request_failed"));
  }
  renderLoadErrors();
  return !!(json && json.success);
}

// Shows the loaders that failed. Server text is set as textContent, never as HTML.
function renderLoadErrors() {
  const banner = document.getElementById("load-errors");
  const list = document.getElementById("load-errors-list");
  if (!banner || !list) return;
  list.replaceChildren(...Array.from(loadErrors, ([name, message]) => {
    const li = document.createElement("li");
    li.textContent = tf("data_health.load_failed", { what: t(`data_health.sources.${name}`), error: truncate(message, 300) });
    return li;
  }));
  banner.hidden = loadErrors.size === 0;
}

// Most rows of each table named in the unreadable-records banner text.
const MAX_CORRUPT_IDS = 5;

// Shows the persistent banner for stored rows the server cannot read
// (corrupt_records of GET /api/v1/stats, sent to administrators only).
function renderCorruptRecords() {
  const banner = document.getElementById("corrupt-records-warning");
  if (!banner) return;
  const rows = state.stats && Array.isArray(state.stats.corrupt_records) ? state.stats.corrupt_records : [];
  banner.hidden = rows.length === 0;
  if (rows.length === 0) return;
  const byTable = new Map();
  rows.forEach(r => {
    const table = String(r.table || "?");
    if (!byTable.has(table)) byTable.set(table, []);
    byTable.get(table).push(String(r.id || "?"));
  });
  const summary = Array.from(byTable, ([table, ids]) =>
    `${table}: ${ids.slice(0, MAX_CORRUPT_IDS).join(", ")}${ids.length > MAX_CORRUPT_IDS ? ", …" : ""}`).join("; ");
  document.getElementById("corrupt-records-title").textContent = tf("data_health.corrupt_title", { n: rows.length, summary });
  document.getElementById("corrupt-records-text").textContent = t("data_health.corrupt_body");
  document.getElementById("corrupt-records-list").replaceChildren(...rows.slice(0, 20).map(r => {
    const li = document.createElement("li");
    li.textContent = tf("data_health.corrupt_row", { table: String(r.table || "?"), id: String(r.id || "?"), error: truncate(String(r.error || ""), 200) });
    return li;
  }));
}

async function loadStats() {
  try {
    const json = await apiJSON("/api/v1/stats");
    if (json.success) state.stats = json.data || null;
    // Figures the server could not read show as zero: say so instead.
    const degraded = json.success && state.stats && state.stats.degraded;
    noteLoad("stats", degraded ? { success: false, error: state.stats.degraded_reason } : json);
  } catch (err) {
    console.error("Failed to load stats:", err);
  }
  renderCorruptRecords();
}

async function loadJobs() {
  try {
    const json = await apiJSON("/api/v1/jobs");
    if (!noteLoad("jobs", json)) return;
    state.jobs = json.data || [];
    state.loaded.jobs = true;
    renderJobs();
  } catch (err) {
    console.error("Failed to load jobs:", err);
  }
}

async function loadNotifications() {
  try {
    const [cJson, rJson] = await Promise.all([
      apiJSON("/api/v1/notifications/channels"),
      apiJSON("/api/v1/notifications/rules")
    ]);
    if (noteLoad("channels", cJson)) {
      state.channels = cJson.data || [];
      state.loaded.channels = true;
      renderChannels();
    }
    if (noteLoad("rules", rJson)) {
      state.rules = rJson.data || [];
      state.loaded.rules = true;
      renderRules();
    }
  } catch (err) {
    console.error("Failed to load notifications:", err);
  }
}

// ---------------------------------------------------------------------------
// Backup and restore lists: server-side filters and pagination
// ---------------------------------------------------------------------------

const PAGE_SIZES = [25, 50, 100];
const DEFAULT_PAGE_SIZE = 25;
const FILTER_DEBOUNCE_MS = 300;
const MAX_FILTER_TEXT = 256;
// Most IDs one ?id= backups query may name.
const MAX_FILTER_IDS = 200;
const LIST_KINDS = ["backups", "restores"];
// Options of the status filters, in order: the API value and its label key. This is
// the only list of filterable statuses; the selects are filled from it.
const LIST_STATUSES = [
  { value: "completed", label: "filters.status_completed" },
  { value: "failed", label: "filters.status_failed" },
  { value: "in_progress", label: "filters.status_running" },
  { value: "cancelled", label: "run.status_cancelled" },
  // Completed backups whose archive a storage scan no longer found (trust.js).
  { value: "missing", label: "status.missing", kinds: ["backups"] },
  // Deleted backups wait for their purge (the "Deleted" view, protection.js); purged
  // ones are history. Both are hidden unless chosen here.
  { value: "deleted", label: "status.deleted", kinds: ["backups"] },
  { value: "purged", label: "status.purged", kinds: ["backups"] }
];

// The status filter options of list kind (an option without kinds applies to both).
function listStatuses(kind) {
  return LIST_STATUSES.filter(s => !s.kinds || s.kinds.includes(kind));
}
const LIST_RANGES = ["today", "7d", "30d", "custom"];
// The ID of a run of several backups (?run= of the Backups list).
const RUN_ID_RE = /^run_[A-Za-z0-9_]{1,120}$/;
const DATE_INPUT_RE = /^\d{4}-\d{2}-\d{2}$/;
// Newest records fetched to report operations started here that are not on the page.
const TRACK_LOOKUP_LIMIT = 50;
// Attempts followed when the backup details dialog builds a retry chain.
const RETRY_CHAIN_MAX = 25;
const BACKUP_CACHE_MAX = 2000;

// Filters each list keeps in the URL hash, in display order.
const LIST_FILTERS = {
  backups: ["q", "status", "database", "trigger", "range", "from", "to", "run"],
  restores: ["q", "status", "database", "range", "from", "to"]
};

function emptyFilters(kind) {
  const f = {};
  LIST_FILTERS[kind].forEach(k => { f[k] = ""; });
  return f;
}

// Filter, page and result state of the Backups and Restores tables. total is the
// number of matches the server reported for the current filters.
const lists = {
  backups: { filters: emptyFilters("backups"), sort: "", page: 1, size: DEFAULT_PAGE_SIZE, total: 0, seq: 0, error: "", databases: [], timer: null },
  restores: { filters: emptyFilters("restores"), sort: "", page: 1, size: DEFAULT_PAGE_SIZE, total: 0, seq: 0, error: "", databases: [], timer: null }
};

// Every backup record seen in any response, so details dialogs, restore forms and
// restore rows can find a backup that is not on the current page.
const backupCache = new Map();

function rememberBackups(items) {
  (items || []).forEach(b => {
    if (!b || !b.id) return;
    backupCache.delete(b.id);
    backupCache.set(b.id, b);
  });
  while (backupCache.size > BACKUP_CACHE_MAX) backupCache.delete(backupCache.keys().next().value);
}

function pageSizeKey(kind) {
  return `mongorescue_${kind}_page_size`;
}

function storedPageSize(kind) {
  const n = Number(storageGet(pageSizeKey(kind)));
  return PAGE_SIZES.includes(n) ? n : DEFAULT_PAGE_SIZE;
}

function listKindOf(el) {
  const holder = el instanceof Element ? el.closest("[data-list]") : null;
  const kind = holder ? holder.dataset.list : "";
  return LIST_KINDS.includes(kind) ? kind : "";
}

function activeTabId() {
  const pane = document.querySelector(".tab-pane.active");
  return pane ? pane.id : "";
}

// ----- URL hash (#backups?status=failed&page=2) -----

function listHashParams(kind) {
  const L = lists[kind];
  const p = new URLSearchParams();
  LIST_FILTERS[kind].forEach(k => {
    const v = L.filters[k];
    if (!v || ((k === "from" || k === "to") && L.filters.range !== "custom")) return;
    p.set(k, v);
  });
  if (L.sort) p.set("sort", L.sort);
  if (L.page > 1) p.set("page", String(L.page));
  return p.toString();
}

function tabHash(id) {
  const name = String(id || "").replace(/^tab-/, "");
  let qs = LIST_KINDS.includes(name) ? listHashParams(name) : "";
  if (!LIST_KINDS.includes(name) && typeof tablesHashParams === "function") qs = tablesHashParams(name);
  return `#${name}${qs ? `?${qs}` : ""}`;
}

// Writes the active tab and its list state into the URL. push adds a history entry
// (page changes and filter choices), so Back returns to the previous view.
function writeHash(push) {
  const id = activeTabId();
  if (!id) return;
  // An open details dialog owns the URL (#/backups/<id>, nav.js).
  if (typeof navHoldsHash === "function" && navHoldsHash()) return;
  const hash = tabHash(id);
  if (hash === window.location.hash) return;
  try {
    if (push) {
      window.history.pushState(null, "", hash);
    } else {
      window.history.replaceState(null, "", hash);
    }
  } catch (err) {
    // history API unavailable (e.g. sandboxed frame): ignore
  }
}

function readHash() {
  const raw = String(window.location.hash || "").replace(/^#/, "");
  const at = raw.indexOf("?");
  let params;
  try {
    params = new URLSearchParams(at >= 0 ? raw.slice(at + 1) : "");
  } catch (err) {
    params = new URLSearchParams();
  }
  return { tab: at >= 0 ? raw.slice(0, at) : raw, params };
}

// Applies hash parameters to list kind; reports whether its state changed. Unknown
// or malformed values are dropped.
function applyListParams(kind, params) {
  const L = lists[kind];
  const get = k => String(params.get(k) || "").slice(0, MAX_FILTER_TEXT);
  const next = emptyFilters(kind);
  next.q = get("q");
  next.database = get("database");
  if (listStatuses(kind).some(s => s.value === get("status"))) next.status = get("status");
  if (kind === "backups" && BACKUP_TRIGGERS.includes(get("trigger"))) next.trigger = get("trigger");
  if (kind === "backups" && RUN_ID_RE.test(get("run"))) next.run = get("run");
  if (LIST_RANGES.includes(get("range"))) next.range = get("range");
  if (next.range === "custom") {
    if (DATE_INPUT_RE.test(get("from"))) next.from = get("from");
    if (DATE_INPUT_RE.test(get("to"))) next.to = get("to");
  }
  const page = Math.max(1, Math.min(1e6, Math.floor(Number(params.get("page"))) || 1));
  // Only the columns tables.js offers for this list are kept.
  const sort = typeof tablesValidSort === "function" ? tablesValidSort(kind, get("sort")) : "";
  const changed = page !== L.page || sort !== L.sort || LIST_FILTERS[kind].some(k => next[k] !== L.filters[k]);
  L.filters = next;
  L.sort = sort;
  L.page = page;
  return changed;
}

// Back/forward and edited URLs: show the tab and list state the hash names.
function applyHashState() {
  const { tab, params } = readHash();
  const id = `tab-${tab}`;
  if (!tab || !document.getElementById(id)) return;
  const changed = LIST_KINDS.includes(tab) && applyListParams(tab, params);
  // Jobs, Connections and Notifications keep their filters in tables.js.
  const tableChanged = !LIST_KINDS.includes(tab) && typeof tablesApplyHash === "function" && tablesApplyHash(tab, params);
  if (activeTabId() !== id) activateTab(id, false);
  if (changed) {
    renderListControls(tab);
    if (auth.user) loadList(tab);
  }
  if (tableChanged) tablesRefresh(tab);
}

// ----- Query -----

function startOfDay(d) {
  const x = new Date(d.getTime());
  x.setHours(0, 0, 0, 0);
  return x;
}

function addDays(d, n) {
  const x = new Date(d.getTime());
  x.setDate(x.getDate() + n);
  return x;
}

// Local midnight of a date input value (YYYY-MM-DD), or null.
function parseDateInput(value) {
  if (!DATE_INPUT_RE.test(String(value || ""))) return null;
  const [y, m, d] = value.split("-").map(Number);
  const date = new Date(y, m - 1, d);
  return Number.isNaN(date.getTime()) ? null : date;
}

// The started_at range of the date filter in local days: from inclusive, to exclusive.
function rangeBounds(f) {
  const today = startOfDay(new Date());
  switch (f.range) {
    case "today":
      return { from: today, to: null };
    case "7d":
      return { from: addDays(today, -6), to: null };
    case "30d":
      return { from: addDays(today, -29), to: null };
    case "custom": {
      let from = parseDateInput(f.from);
      let to = parseDateInput(f.to);
      if (from && to && to < from) [from, to] = [to, from];
      return { from, to: to ? addDays(to, 1) : null };
    }
    default:
      return { from: null, to: null };
  }
}

function listParams(kind) {
  const L = lists[kind];
  const f = L.filters;
  const p = new URLSearchParams();
  if (f.q.trim()) p.set("q", f.q.trim());
  if (f.status) p.set("status", f.status);
  if (f.database) p.set("database", f.database);
  if (kind === "backups" && f.trigger) p.set("trigger", f.trigger);
  if (kind === "backups" && f.run) p.set("run_id", f.run);
  const { from, to } = rangeBounds(f);
  if (from) p.set("from", from.toISOString());
  if (to) p.set("to", to.toISOString());
  if (L.sort) p.set("sort", L.sort);
  p.set("limit", String(L.size));
  p.set("offset", String((L.page - 1) * L.size));
  return p.toString();
}

function activeFilterCount(kind) {
  const f = lists[kind].filters;
  let n = 0;
  if (f.q.trim()) n++;
  if (f.status) n++;
  if (f.database) n++;
  if (kind === "backups" && f.trigger) n++;
  if (kind === "backups" && f.run) n++;
  if (f.range && (f.range !== "custom" || f.from || f.to)) n++;
  return n;
}

function lastPage(kind) {
  const L = lists[kind];
  return Math.max(1, Math.ceil(L.total / L.size));
}

// ----- Loading -----

async function loadBackups() {
  return loadList("backups");
}

async function loadRestores() {
  return loadList("restores");
}

// Loads the current page of list kind with its filters. Responses that arrive after
// a newer request was started are dropped.
async function loadList(requested) {
  // kind may come from the URL hash: only the two literal list names are accepted, so
  // it can never name another property of state or lists, or another API path.
  const kind = listKind(requested);
  if (!kind) return;
  const L = lists[kind];
  const seq = ++L.seq;
  try {
    const json = await apiJSON(`/api/v1/${kind}?${listParams(kind)}`);
    if (seq !== L.seq) return;
    if (!noteLoad(kind, json, json.error || t("filters.load_failed"))) {
      L.error = json.error || t("filters.load_failed");
      L.total = 0;
      state[kind] = [];
      state.loaded[kind] = true;
      renderList(kind);
      return;
    }
    const items = json.data || [];
    const total = json.meta ? Math.max(0, Number(json.meta.total) || 0) : items.length;
    L.total = total;
    // Records were deleted or the page is past the end: show the last page instead.
    if (items.length === 0 && total > 0 && L.page > lastPage(kind)) {
      L.page = lastPage(kind);
      writeHash(false);
      await loadList(kind);
      return;
    }
    L.error = "";
    state[kind] = items;
    state.loaded[kind] = true;
    pruneCancellingRuns();
    if (kind === "backups") rememberBackups(items);
    if (kind === "restores") await loadRestoreBackups(items);
    await reportTracked(kind, items);
    if (seq !== L.seq) return;
    renderList(kind);
    if (kind === "backups") refreshOpenBackupDialogs();
  } catch (err) {
    console.error("Failed to load list:", kind, err);
  }
}

// listKind returns the list name for value ("backups" or "restores"), or "" for any
// other value.
function listKind(value) {
  switch (value) {
    case "backups":
      return "backups";
    case "restores":
      return "restores";
    default:
      return "";
  }
}

function renderList(kind) {
  if (kind === "backups") {
    renderBackups();
    renderJobs();
  } else {
    renderRestores();
  }
}

// Operations started here may not be on the current page (other filters, another
// page): look them up among the newest records before reporting their outcome.
async function reportTracked(kind, items) {
  const tracked = trackedOps[kind];
  if (tracked.size === 0) return;
  let records = items;
  if (Array.from(tracked.keys()).some(id => !items.some(r => r.id === id))) {
    try {
      const json = await apiJSON(`/api/v1/${kind}?limit=${TRACK_LOOKUP_LIMIT}`);
      if (!json.success) return;
      records = items.concat(json.data || []);
      if (kind === "backups") rememberBackups(json.data || []);
    } catch (err) {
      return;
    }
  }
  reportFinished(kind, records);
}

// Backups named by restore rows that the server did not return (deleted), so they
// are not asked for again.
const missingBackups = new Set();

// Fetches, in one request (?id=a,b,c), the backups of restore rows that are not in
// backupCache, so their source connection can be shown.
async function loadRestoreBackups(restores) {
  const ids = Array.from(new Set((restores || []).map(r => r.backup_id)))
    .filter(id => id && !backupCache.has(id) && !missingBackups.has(id))
    .slice(0, MAX_FILTER_IDS);
  if (ids.length === 0) return;
  try {
    const json = await apiJSON(`/api/v1/backups?id=${ids.map(encodeURIComponent).join(",")}&limit=${ids.length}`);
    if (!json.success) return;
    rememberBackups(json.data || []);
    ids.forEach(id => { if (!backupCache.has(id)) missingBackups.add(id); });
  } catch (err) {
    console.error("Failed to load the backups of restores:", err);
  }
}

// Distinct databases offered by the database filters.
async function loadListDatabases() {
  await Promise.all(LIST_KINDS.map(async kind => {
    try {
      const json = await apiJSON(`/api/v1/${kind}/databases`);
      if (!noteLoad(`${kind}_databases`, json)) return;
      lists[kind].databases = Array.isArray(json.data) ? json.data.map(String) : [];
      renderDatabaseOptions(kind);
    } catch (err) {
      console.error(`Failed to load ${kind} databases:`, err);
    }
  }));
}

// ----- Changing filters and pages -----

function setFilter(kind, key, value, push) {
  const L = lists[kind];
  let v = String(value || "");
  if (key === "q" || key === "database") v = v.slice(0, MAX_FILTER_TEXT);
  if (!LIST_FILTERS[kind].includes(key) || L.filters[key] === v) return;
  L.filters[key] = v;
  if (key === "range" && v !== "custom") {
    L.filters.from = "";
    L.filters.to = "";
  }
  L.page = 1;
  writeHash(push);
  renderListControls(kind);
  if (key === "range" && v === "custom") {
    const from = document.getElementById(`${kind}-filter-from`);
    if (from) from.focus();
  }
  loadList(kind);
}

function clearFilters(kind) {
  const L = lists[kind];
  clearTimeout(L.timer);
  L.filters = emptyFilters(kind);
  L.page = 1;
  writeHash(true);
  renderListControls(kind, true);
  loadList(kind);
  const q = document.getElementById(`${kind}-filter-q`);
  if (q) q.focus();
}

function goToPage(kind, page) {
  const L = lists[kind];
  const p = Math.max(1, Math.min(lastPage(kind), Math.floor(Number(page)) || 1));
  if (p === L.page) return;
  L.page = p;
  writeHash(true);
  renderPager(kind);
  loadList(kind);
}

function setPageSize(kind, value) {
  const L = lists[kind];
  const n = Number(value);
  if (!PAGE_SIZES.includes(n) || n === L.size) return;
  // Keep the first row of the current page in view.
  const first = (L.page - 1) * L.size;
  L.size = n;
  L.page = Math.floor(first / n) + 1;
  storageSet(pageSizeKey(kind), String(n));
  writeHash(false);
  loadList(kind);
}

// Fills the status filter of list kind from LIST_STATUSES (labels are translated by
// applyTranslations through data-i18n).
function fillStatusOptions(kind) {
  const sel = document.getElementById(`${kind}-filter-status`);
  if (!sel) return;
  sel.textContent = "";
  [{ value: "", label: "filters.status_all" }].concat(listStatuses(kind)).forEach(s => {
    const opt = document.createElement("option");
    opt.value = s.value;
    opt.setAttribute("data-i18n", s.label);
    opt.textContent = t(s.label);
    sel.appendChild(opt);
  });
}

function setupListControls() {
  LIST_KINDS.forEach(kind => {
    lists[kind].size = storedPageSize(kind);
    fillStatusOptions(kind);
  });
  document.addEventListener("input", (e) => {
    const el = e.target;
    if (!(el instanceof HTMLInputElement) || el.dataset.filter !== "q") return;
    const kind = listKindOf(el);
    if (!kind) return;
    const L = lists[kind];
    clearTimeout(L.timer);
    L.timer = setTimeout(() => setFilter(kind, "q", el.value, false), FILTER_DEBOUNCE_MS);
  });
  document.addEventListener("keydown", (e) => {
    const el = e.target;
    if (e.key !== "Enter" || !(el instanceof HTMLInputElement) || el.dataset.filter !== "q") return;
    const kind = listKindOf(el);
    if (!kind) return;
    e.preventDefault();
    clearTimeout(lists[kind].timer);
    setFilter(kind, "q", el.value, false);
  });
  document.addEventListener("change", (e) => {
    const el = e.target;
    if (!(el instanceof HTMLElement)) return;
    const kind = listKindOf(el);
    if (!kind) return;
    if (el.dataset.pageSize) {
      setPageSize(kind, el.value);
    } else if (el.dataset.filter && el.dataset.filter !== "q") {
      setFilter(kind, el.dataset.filter, el.value, true);
    }
  });
  window.addEventListener("popstate", applyHashState);
  window.addEventListener("hashchange", applyHashState);
}

// ----- Rendering the filter bar and the pager -----

function renderDatabaseOptions(kind) {
  const sel = document.getElementById(`${kind}-filter-database`);
  if (!sel) return;
  const L = lists[kind];
  const names = L.databases.slice();
  if (L.filters.database && !names.includes(L.filters.database)) names.unshift(L.filters.database);
  const key = JSON.stringify(names);
  if (sel.dataset.key !== key) {
    const allKey = kind === "backups" ? "filters.database_all" : "filters.target_all";
    sel.textContent = "";
    const all = document.createElement("option");
    all.value = "";
    all.setAttribute("data-i18n", allKey);
    all.textContent = t(allKey);
    sel.appendChild(all);
    names.forEach(name => {
      const opt = document.createElement("option");
      opt.value = name;
      opt.textContent = name;
      sel.appendChild(opt);
    });
    sel.dataset.key = key;
  }
  sel.value = L.filters.database;
}

// Shows list kind's state in its filter bar and pager. The search box is only
// overwritten when it is not being typed in (or force is set).
function renderListControls(kind, force) {
  const L = lists[kind];
  const f = L.filters;
  const q = document.getElementById(`${kind}-filter-q`);
  if (q && (force || document.activeElement !== q) && q.value !== f.q) q.value = f.q;
  LIST_FILTERS[kind].forEach(k => {
    if (k === "q" || k === "database") return;
    const el = document.getElementById(`${kind}-filter-${k}`);
    if (el && el.value !== f[k]) el.value = f[k];
  });
  renderDatabaseOptions(kind);
  const dates = document.getElementById(`${kind}-filter-dates`);
  if (dates) dates.hidden = f.range !== "custom";
  const n = activeFilterCount(kind);
  const badge = document.getElementById(`${kind}-filter-count`);
  if (badge) {
    badge.hidden = n === 0;
    badge.textContent = n > 0 ? tf("filters.active_n", { n }) : "";
  }
  const clear = document.getElementById(`${kind}-filter-clear`);
  if (clear) clear.hidden = n === 0;
  const run = document.getElementById(`${kind}-filter-run-chip`);
  if (run) {
    run.hidden = !f.run;
    run.textContent = f.run ? tf("filters.run_filter", { id: f.run }) : "";
  }
  renderPager(kind);
}

// Shows the Backups list filtered to the backups of run runId (a Backup now of
// several databases or a job run).
function showRunBackups(runId) {
  const id = String(runId || "");
  if (!RUN_ID_RE.test(id)) return;
  const L = lists.backups;
  L.filters = emptyFilters("backups");
  L.filters.run = id;
  L.page = 1;
  activateTab("tab-backups", false);
  writeHash(true);
  renderListControls("backups", true);
  loadList("backups");
}

function formatCount(n) {
  try {
    return new Intl.NumberFormat(uiLocale()).format(n);
  } catch (err) {
    return String(n);
  }
}

// Page numbers to show around page (of last): the first and last page, the current
// one with its neighbours, and "gap" where pages are skipped.
function pageItems(page, last) {
  if (last <= 7) return Array.from({ length: last }, (_, i) => i + 1);
  let start = Math.max(2, page - 1);
  let end = Math.min(last - 1, page + 1);
  if (page <= 3) {
    start = 2;
    end = 4;
  } else if (page >= last - 2) {
    start = last - 3;
    end = last - 1;
  }
  const items = [1];
  if (start > 2) items.push("gap");
  for (let i = start; i <= end; i++) items.push(i);
  if (end < last - 1) items.push("gap");
  items.push(last);
  return items;
}

function renderPager(kind) {
  const nav = document.getElementById(`${kind}-pager`);
  if (!nav) return;
  const L = lists[kind];
  nav.hidden = !state.loaded[kind] || L.total === 0 || !!L.error;
  if (nav.hidden) return;
  const last = lastPage(kind);
  const page = Math.min(L.page, last);
  const from = (page - 1) * L.size + 1;
  const to = Math.min(L.total, page * L.size);
  setText(`${kind}-pager-range`, tf("pager.range", { from: formatCount(from), to: formatCount(to), total: formatCount(L.total) }));
  const size = document.getElementById(`${kind}-page-size`);
  if (size && size.value !== String(L.size)) size.value = String(L.size);

  const pages = document.getElementById(`${kind}-pager-pages`);
  const focused = pages.contains(document.activeElement) ? document.activeElement : null;
  const focusKey = focused ? (focused.dataset.nav || "page") : "";
  const button = (target, html, opts) => {
    const attrs = [
      `type="button"`,
      `class="pager-btn${opts.current ? " active" : ""}${opts.nav ? " pager-nav" : ""}"`,
      `data-action="page"`,
      `data-page="${Number(target)}"`
    ];
    if (opts.nav) attrs.push(`data-nav="${opts.nav}"`);
    if (opts.label) attrs.push(`aria-label="${escapeHtml(opts.label)}"`);
    if (opts.current) attrs.push(`aria-current="page"`);
    if (opts.disabled) attrs.push("disabled");
    return `<button ${attrs.join(" ")}>${html}</button>`;
  };
  const parts = [button(page - 1, `<span aria-hidden="true">‹</span> ${escapeHtml(t("pager.previous"))}`, { nav: "prev", disabled: page <= 1 })];
  pageItems(page, last).forEach(item => {
    if (item === "gap") {
      parts.push(`<span class="pager-gap" aria-hidden="true">…</span>`);
    } else {
      parts.push(button(item, escapeHtml(formatCount(item)), { label: tf("pager.page_n", { n: item }), current: item === page }));
    }
  });
  parts.push(button(page + 1, `${escapeHtml(t("pager.next"))} <span aria-hidden="true">›</span>`, { nav: "next", disabled: page >= last }));
  const html = parts.join("");
  if (pages.dataset.html !== html) {
    pages.innerHTML = html;
    pages.dataset.html = html;
    // Keep keyboard focus in the pager after it is redrawn.
    if (focusKey) {
      const again = focusKey === "page"
        ? pages.querySelector('[aria-current="page"]')
        : pages.querySelector(`[data-nav="${focusKey}"]:not([disabled])`) || pages.querySelector('[aria-current="page"]');
      if (again) again.focus();
    }
  }
}

// ----- Job run history and retry chains (fetched for their dialog) -----

// Last JOB_HISTORY_LIMIT backups of the job in the job details dialog.
const jobHistory = { jobId: "", runs: null, failed: false, seq: 0 };

async function loadJobHistory(jobID) {
  if (!jobID) return;
  const seq = ++jobHistory.seq;
  try {
    const json = await apiJSON(`/api/v1/backups?job_id=${encodeURIComponent(jobID)}&limit=${JOB_HISTORY_LIMIT}`);
    if (seq !== jobHistory.seq) return;
    if (!json.success) throw new Error(json.error || "");
    jobHistory.jobId = jobID;
    jobHistory.runs = json.data || [];
    jobHistory.failed = false;
    rememberBackups(jobHistory.runs);
  } catch (err) {
    if (seq !== jobHistory.seq) return;
    jobHistory.failed = true;
  }
  renderJobDetails();
}

// Every attempt of the retry chain of the backup in the details dialog, oldest first.
const backupChain = { id: "", records: null, seq: 0 };

async function findBackup(id) {
  if (backupCache.has(id)) return backupCache.get(id);
  const json = await apiJSON(`/api/v1/backups?id=${encodeURIComponent(id)}&limit=1`);
  if (!json.success) throw new Error(json.error || "");
  rememberBackups(json.data || []);
  return (json.data || []).find(x => x.id === id) || null;
}

async function backupChildren(id) {
  const json = await apiJSON(`/api/v1/backups?retry_of=${encodeURIComponent(id)}&sort=asc&limit=${RETRY_CHAIN_MAX}`);
  if (!json.success) throw new Error(json.error || "");
  rememberBackups(json.data || []);
  return json.data || [];
}

// Builds the chain from the server: the first attempt found by following retry_of,
// then the retries of every attempt in the chain (retry_of=<id>).
async function loadBackupChain(id) {
  const seq = ++backupChain.seq;
  try {
    const b = backupCache.get(id);
    if (!b) return;
    let root = b;
    const seen = new Set([b.id]);
    while (root.retry_of && !seen.has(root.retry_of) && seen.size < RETRY_CHAIN_MAX) {
      const parent = await findBackup(root.retry_of);
      if (!parent) break;
      seen.add(parent.id);
      root = parent;
    }
    const chain = [root];
    const ids = new Set([root.id]);
    for (let i = 0; i < chain.length && chain.length < RETRY_CHAIN_MAX; i++) {
      (await backupChildren(chain[i].id)).forEach(r => {
        if (!ids.has(r.id)) {
          ids.add(r.id);
          chain.push(r);
        }
      });
    }
    if (seq !== backupChain.seq || backupChain.id !== id) return;
    backupChain.records = chain.map(x => x.id);
  } catch (err) {
    if (seq !== backupChain.seq) return;
    backupChain.records = null;
  }
  renderBackupDetails();
}

// Refetches what the open backup dialogs show after the lists were refreshed.
function refreshOpenBackupDialogs() {
  const jobModal = document.getElementById("modal-job-details");
  if (jobModal && jobModal.classList.contains("open") && detailsJobId) loadJobHistory(detailsJobId);
  const backupModal = document.getElementById("modal-backup-details");
  if (backupModal && backupModal.classList.contains("open") && detailsBackupId) loadBackupChain(detailsBackupId);
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

function renderAll() {
  renderUserMenu();
  const dot = document.getElementById("conn-status");
  if (dot) setConnection(dot, dot.dataset.status || "unknown", "");
  renderStats();
  renderJobs();
  renderBackups();
  renderRestores();
  renderChannels();
  renderRules();
  renderConnections();
  updateConnectionGating();
  renderUsers();
  renderApiKeys();
  renderAudit();
  renderStorageTargets();
  renderSettingsLanguage();
  renderLoadErrors();
  renderCorruptRecords();
}

function renderStats() {
  const stats = state.stats || {};

  // Storage used
  setText("stat-storage", state.stats ? formatBytes(stats.total_bytes || 0) : "—");
  const where = storageDescription(stats.storage_type);
  const storageSub = document.getElementById("stat-storage-sub");
  if (storageSub) {
    storageSub.textContent = where;
    storageSub.title = where;
  }

  // Backups (completed archives) + failures in the last 24h. The KPIs come from
  // /api/v1/stats and cover every record, not the page shown in the table.
  setText("stat-backups", state.stats ? String(stats.completed_backups || 0) : "—");
  const failed24h = Number(stats.failed_backups_24h) || 0;
  const backupsSub = document.getElementById("stat-backups-sub");
  if (backupsSub) {
    backupsSub.classList.toggle("text-danger", failed24h > 0);
    backupsSub.textContent = !state.stats
      ? ""
      : failed24h > 0
        ? tf("metrics.failed_24h", { n: failed24h })
        : stats.total_backups > 0 ? t("metrics.no_failures_24h") : "";
  }

  // Scheduled jobs + next run
  setText("stat-jobs", state.stats ? String(stats.active_jobs || 0) : "—");
  const now = Date.now();
  let next = null;
  state.jobs.forEach(job => {
    if (job.enabled === false) return;
    const d = parseDate(job.next_run);
    if (d && d.getTime() >= now - 60000 && (!next || d < next.date)) next = { date: d, job };
  });
  const enabledJobs = state.jobs.filter(j => j.enabled !== false).length;
  const jobsSub = document.getElementById("stat-jobs-sub");
  if (jobsSub) {
    jobsSub.textContent = !state.loaded.jobs
      ? ""
      : next
        ? tf("metrics.next_run", { d: formatSpan(next.date.getTime() - now), job: next.job.name || next.job.id })
        : enabledJobs > 0 ? tf("metrics.enabled_n", { n: enabledJobs }) : "";
    jobsSub.title = next ? formatAbsolute(next.date) : "";
  }

  // Last backup
  const valueEl = document.getElementById("stat-last");
  const lastSub = document.getElementById("stat-last-sub");
  const last = stats.last_backup || null;
  if (valueEl) {
    if (!last) {
      valueEl.textContent = "—";
      valueEl.title = "";
    } else {
      const d = parseDate(last.started_at);
      const [kind, label] = backupStatus(last.status);
      const [shown, tooltip] = !d ? ["—", ""] : typeof timeDisplay === "function" ? timeDisplay(d) : [formatRelative(d), formatAbsolute(d)];
      valueEl.innerHTML = `<span class="stat-time">${escapeHtml(shown)}</span>${statusBadge(kind, label, last.error_message)}`;
      valueEl.title = tooltip;
    }
  }
  if (lastSub) {
    lastSub.textContent = !state.stats
      ? ""
      : last ? `${jobLabel(last.job_id)} · ${last.database}` : t("metrics.no_backups");
  }

  setText("count-backups", state.stats ? String(stats.total_backups || 0) : "");
  setText("count-jobs", state.loaded.jobs ? String(state.jobs.length) : "");
  setText("count-restores", state.stats ? String(stats.total_restores || 0) : "");
  setText("count-notifications", state.loaded.channels ? String(state.channels.length) : "");
  // Screen readers hear about runs that finish in the background (forms.js).
  if (typeof announceBackgroundChanges === "function") announceBackgroundChanges();
}

function storageDescription(type) {
  const def = defaultStorageTarget();
  if (def) return `${def.name} · ${storageLocation(def)}`;
  return String(type || "");
}

// How a backup was started; records written before triggers existed have none.
const BACKUP_TRIGGERS = ["scheduled", "on_demand", "manual", "mcp"];

function backupTrigger(b) {
  const trigger = b.trigger || (b.job_id ? "scheduled" : "manual");
  return BACKUP_TRIGGERS.includes(trigger) ? t(`metrics.trigger_${trigger}`) : "";
}

// "connection · job · trigger" line under a backup's database name.
function backupOrigin(b) {
  const conn = b.connection_name || (b.connection_id ? connectionName(b.connection_id) : "");
  return [conn, b.job_id ? jobLabel(b.job_id) : "", backupTrigger(b)].filter(Boolean).join(" · ");
}

// Short note under a job's database when it backs up only some collections.
function collectionScope(job) {
  const include = job.collections || [];
  const exclude = job.exclude_collections || [];
  if (include.length === 0 && exclude.length === 0) return "";
  const text = include.length > 0
    ? tf("picker.scope_include", { list: include.join(", ") })
    : tf("picker.scope_exclude", { list: exclude.join(", ") });
  return `<div class="cell-sub">${ellipsis(text, "ell-sm")}</div>`;
}

function jobLabel(jobID) {
  if (!jobID) return t("metrics.manual");
  const job = state.jobs.find(j => j.id === jobID);
  return job ? (job.name || job.id) : jobID;
}

function renderJobs() {
  const tbody = document.getElementById("jobs-tbody");
  if (!tbody) return;
  if (!state.loaded.jobs) return;

  if (state.jobs.length === 0) {
    setTbody(tbody, state.loaded.connections && state.connections.length === 0
      ? emptyRow(10, t("conn.jobs_need_connection"), "new-connection", "plus", t("conn.add"))
      : emptyRow(10, t("tables.empty_jobs"), "new-job", "plus", t("nav.new_job")));
    return;
  }

  const jobs = typeof tablesRows === "function" ? tablesRows("jobs", state.jobs) : state.jobs;
  if (jobs === null || jobs.length === 0) {
    // Loading the server-side filter (tables.js), or no job matches it.
    setTbody(tbody, jobs === null ? tablesSkeleton("jobs") : tablesNoMatch("jobs"));
    renderJobDetails();
    return;
  }

  setTbody(tbody, jobs.map(job => {
    const lastBackup = state.stats && state.stats.job_last_backups ? state.stats.job_last_backups[job.id] : null;
    let lastDot = "";
    // A job with several databases shows its last run (ok, partial, failed), never
    // the newest backup of one of its databases (jobdbs.js).
    const lastRun = typeof jobLastRunMark === "function" ? jobLastRunMark(job) : null;
    if (lastRun !== null) {
      lastDot = lastRun;
    } else if (lastBackup) {
      const [kind, label] = backupStatus(lastBackup.status);
      lastDot = statusMark(kind, label);
    }
    const enabled = job.enabled !== false;
    const meaning = describeCron(job.cron_expression);
    const id = escapeHtml(job.id);
    return `<tr class="row-clickable" data-row-action="job-details" data-id="${id}" tabindex="0">
      ${bulkCell("jobs", job.id, job.name || job.id)}
      <td class="cell-primary">${ellipsis(job.name || job.id, "ell-md")}${idCopy(job.id, "cell-sub")}</td>
      <td>${job.connection_id ? ellipsis(connectionName(job.connection_id), "ell-sm") : mutedDash()}</td>
      <td>${typeof jobDatabaseCell === "function" ? jobDatabaseCell(job) : ellipsis(job.database, "mono ell-sm") + collectionScope(job)}</td>
      <td><span class="mono" title="${escapeHtml(meaning)}">${escapeHtml(job.cron_expression)}</span>${meaning ? `<div class="cell-sub">${ellipsis(meaning, "ell-md")}</div>` : ""}</td>
      <td class="cell-wrap-sm">${escapeHtml(retentionText(job))}</td>
      <td>${jobStateBadge(job)}</td>
      <td>${lastDot}${timeCell(job.last_run)}${trustJobChip(job)}${typeof jobSparkline === "function" ? jobSparkline(job.id) : ""}</td>
      <td>${enabled ? timeCell(job.next_run) : mutedDash()}</td>
      <td class="col-actions"><div class="row-actions">
        <button type="button" class="btn btn-secondary btn-sm" data-action="trigger-job" data-id="${id}">${escapeHtml(t("actions.run_now"))}</button>
        ${rowMenuButton("job", job.id)}
      </div></td>
    </tr>`;
  }).join(""));
  renderJobDetails();
}

// Human-readable form of a cron expression, or "" when it has no simple reading
// (the expression itself is always shown next to it). Times are the server's.
function describeCron(expr) {
  const s = String(expr || "").trim();
  if (!s) return "";
  const descriptors = {
    "@yearly": "cron.yearly", "@annually": "cron.yearly", "@monthly": "cron.monthly",
    "@weekly": "cron.weekly", "@daily": "cron.daily", "@midnight": "cron.daily", "@hourly": "cron.hourly"
  };
  if (descriptors[s]) return t(descriptors[s]);
  if (s.startsWith("@every ")) {
    const ms = parseGoDuration(s.substring(7));
    return Number.isFinite(ms) && ms > 0 ? tf("cron.every", { d: humanDuration(ms) }) : "";
  }
  const f = s.split(/\s+/);
  if (f.length !== 5) return "";
  const [min, hour, dom, mon, dow] = f;
  const num = v => /^\d{1,2}$/.test(v);
  const step = v => { const m = /^\*\/(\d{1,2})$/.exec(v); return m ? Number(m[1]) : 0; };
  const pad = v => String(v).padStart(2, "0");
  const time = `${pad(hour)}:${pad(min)}`;
  if (mon !== "*") return "";
  if (min === "*" && hour === "*" && dom === "*" && dow === "*") return t("cron.every_minute");
  if (step(min) && hour === "*" && dom === "*" && dow === "*") return tf("cron.every_n_minutes", { n: step(min) });
  if (!num(min) || (dom !== "*" && dow !== "*")) return "";
  if (hour === "*" && dom === "*" && dow === "*") return Number(min) === 0 ? t("cron.hourly") : tf("cron.every_hour_at", { m: pad(min) });
  if (step(hour) && Number(min) === 0 && dom === "*" && dow === "*") return tf("cron.every_n_hours", { n: step(hour) });
  if (!num(hour)) return "";
  if (dom === "*" && dow === "*") return tf("cron.daily_at", { time });
  if (num(dom) && dow === "*") return tf("cron.monthly_at", { day: Number(dom), time });
  const days = cronWeekdays(dow);
  if (!days) return "";
  // Some languages start the sentence with the (lower-case) weekday name.
  const text = tf("cron.weekdays_at", { days, time });
  return text.charAt(0).toLocaleUpperCase(uiLocale()) + text.slice(1);
}

// Localised names of a cron day-of-week field such as "1-5" or "0,6", or "".
function cronWeekdays(field) {
  let fmt;
  try {
    fmt = new Intl.DateTimeFormat(uiLocale(), { weekday: "long", timeZone: "UTC" });
  } catch (err) {
    return "";
  }
  // 4 January 2026 is a Sunday (day 0; cron also accepts 7 for Sunday).
  const name = d => fmt.format(new Date(Date.UTC(2026, 0, 4 + (Number(d) % 7))));
  const parts = field.split(",");
  const out = [];
  for (const part of parts) {
    const range = /^([0-7])-([0-7])$/.exec(part);
    if (range) {
      out.push(`${name(range[1])}–${name(range[2])}`);
    } else if (/^[0-7]$/.test(part)) {
      out.push(name(part));
    } else {
      return "";
    }
  }
  try {
    return new Intl.ListFormat(uiLocale(), { style: "long", type: "conjunction" }).format(out);
  } catch (err) {
    return out.join(", ");
  }
}

function retentionText(job) {
  const days = Number(job.retention_days) || 0;
  const count = Number(job.retention_count) || 0;
  if (days > 0 && count > 0) return `${tf("retention.days_n", { n: days })} · ${tf("retention.max_n", { n: count })}`;
  if (days > 0) return tf("retention.days_n", { n: days });
  if (count > 0) return tf("retention.last_n", { n: count });
  return t("retention.indefinite");
}

function backupStatus(status) {
  switch (status) {
    case "completed":
      return ["success", t("status.succeeded")];
    case "failed":
      return ["danger", t("status.failed")];
    case "in_progress":
      return ["running", t("status.running")];
    case "pending":
      return ["neutral", t("status.pending")];
    case "pruned":
      return ["neutral", t("status.pruned")];
    case "cancelled":
      return ["warn", t("run.status_cancelled")];
    case "missing":
      return ["danger", t("status.missing")];
    case "deleted":
      return ["warn", t("status.deleted")];
    case "purged":
      return ["neutral", t("status.purged")];
    default:
      return ["neutral", String(status || "")];
  }
}

function renderBackups() {
  const tbody = document.getElementById("backups-tbody");
  if (!tbody) return;
  if (!state.loaded.backups) return;
  renderBackupDetails();
  renderListControls("backups");

  if (lists.backups.error) {
    setTbody(tbody, emptyRow(8, lists.backups.error, "clear-filters", "", t("filters.clear"), "", "btn-secondary"));
    return;
  }
  if (state.backups.length === 0 && activeFilterCount("backups") > 0) {
    setTbody(tbody, emptyRow(8, t("filters.no_backups_match"), "clear-filters", "", t("filters.clear"), "", "btn-secondary"));
    return;
  }
  if (state.backups.length === 0) {
    setTbody(tbody, state.loaded.connections && state.connections.length === 0
      ? emptyRow(8, t("conn.backups_need_connection"), "new-connection", "plus", t("conn.add"))
      : emptyRow(8, t("tables.empty_backups"), "backup-now", "", t("nav.instant_backup")));
    return;
  }

  setTbody(tbody, state.backups.map(b => {
    const [kind, label] = backupStatus(b.status);
    const failed = b.status === "failed";
    const restorable = b.status === "completed";
    const lock = b.encrypted === true
      ? `<span class="enc-icon" title="${escapeHtml(`${t("badges.encrypted")} (${b.encryption_mode || "age"})`)}">${icon("lock")}<span class="sr-only">${escapeHtml(t("badges.encrypted"))}</span></span>`
      : "";
    // A failed or empty dump may still carry the SHA-256 of zero bytes; only show real checksums.
    const hasChecksum = restorable && b.sha256 && b.sha256 !== EMPTY_SHA256;
    const sha = hasChecksum
      ? `<span class="mono muted" title="${escapeHtml(b.sha256)}">${escapeHtml(String(b.sha256).substring(0, 12))}</span>`
      : mutedDash();
    const errorLine = failed && b.error_message
      ? errorDetail(b.error_message, truncate(errorSummary(b.error_message), 70))
      : "";
    const size = restorable || Number(b.size_bytes) > 0 ? escapeHtml(formatBytes(b.size_bytes)) : mutedDash();
    const restoreBtn = restorable
      ? `<button type="button" class="btn btn-secondary btn-sm" data-action="restore-backup" data-id="${escapeHtml(b.id)}" data-db="${escapeHtml(b.database)}">${escapeHtml(t("actions.rescue_restore"))}</button>`
      : "";
    // The latest failed attempt of a chain carries the retry action; the others
    // show which attempt retried them (retried_by, computed by the server).
    const retryBtn = failed && !b.retried_by
      ? `<button type="button" class="btn btn-secondary btn-sm" data-action="retry-backup" data-id="${escapeHtml(b.id)}"${retryingBackups.has(b.id) ? " disabled" : ""}>${escapeHtml(t("actions.retry"))}</button>`
      : "";
    return `<tr class="row-clickable" data-row-action="backup-details" data-id="${escapeHtml(b.id)}" tabindex="0">
      ${bulkCell("backups", b.id, `${b.database} · ${b.id}`)}
      <td class="cell-primary"><div class="name-line">${ellipsis(b.database, "ell-md")}${lock}${trustPinIcon(b)}</div><div class="cell-sub">${ellipsis(backupOrigin(b), "ell-md")}</div>${idCopy(b.id, "cell-sub")}${retryLinks(b)}</td>
      <td>${statusBadge(kind, label, b.error_message)}${errorLine}${runProgressHtml(b)}${trustBackupBadges(b)}</td>
      <td>${timeCell(b.started_at)}</td>
      <td class="num">${durationCell(b)}</td>
      <td class="num">${size}${backupStorageCell(b)}</td>
      <td>${sha}</td>
      <td class="col-actions"><div class="row-actions">
        ${restoreBtn}
        ${retryBtn}
        ${cancelRunButton("backup", b)}
        ${rowMenuButton("backup", b.id)}
      </div></td>
    </tr>`;
  }).join(""));
}

function durationCell(item) {
  const secs = Number(item.duration_seconds);
  if (!secs && item.status !== "completed") return mutedDash();
  return escapeHtml(formatDuration(secs || 0));
}

// Compact form of a backup ID for "retry of" notes; the full ID is in the title.
function shortBackupId(id) {
  const s = String(id || "");
  return s.length > 16 ? `…${s.slice(-12)}` : s;
}

// "Retry of …" and "Retried at … → …" notes under a backup's ID. Both come from the
// row itself (retry_of, and retried_by: its newest retry), so they work across pages.
function retryLinks(b) {
  const lines = [];
  if (b.retry_of) {
    lines.push(`<div class="cell-sub retry-link" title="${escapeHtml(b.retry_of)}">${escapeHtml(tf("backup_details.retry_of", { id: shortBackupId(b.retry_of) }))}</div>`);
  }
  const r = b.retried_by;
  if (r && r.id) {
    const d = parseDate(r.started_at);
    const text = tf("backup_details.retried", { time: d ? formatAbsolute(d) : "?", id: shortBackupId(r.id) });
    lines.push(`<div class="cell-sub retry-link" title="${escapeHtml(r.id)}">${escapeHtml(text)}</div>`);
  }
  return lines.join("");
}

function renderRestores() {
  const tbody = document.getElementById("restores-tbody");
  if (!tbody) return;
  if (!state.loaded.restores) return;
  renderListControls("restores");
  renderRestoreDetails();

  if (lists.restores.error) {
    setTbody(tbody, emptyRow(6, lists.restores.error, "clear-filters", "", t("filters.clear"), "", "btn-secondary"));
    return;
  }
  if (state.restores.length === 0 && activeFilterCount("restores") > 0) {
    setTbody(tbody, emptyRow(6, t("filters.no_restores_match"), "clear-filters", "", t("filters.clear"), "", "btn-secondary"));
    return;
  }
  if (state.restores.length === 0) {
    setTbody(tbody, emptyRow(6, t("tables.empty_restores"), "goto-tab", "", t("tables.go_backups"), "tab-backups"));
    return;
  }

  setTbody(tbody, state.restores.map(r => {
    const [kind, label] = backupStatus(r.status === "completed" ? "completed" : r.status);
    const chips = [];
    if (r.dry_run) chips.push(`<span class="chip">${escapeHtml(t("status.dry_run"))}</span>`);
    if (r.verified) chips.push(`<span class="chip">${escapeHtml(t("status.verified"))}</span>`);
    const errorLine = r.status === "failed" && r.error_message
      ? errorDetail(r.error_message, truncate(errorSummary(r.error_message), 90))
      : "";
    const { source, target, crossServer } = restoreConnections(r);
    const selection = Array.isArray(r.selected_collections) && r.selected_collections.length > 0
      ? `<div class="cell-sub">${ellipsis(tf("tables.selected_collections", { list: r.selected_collections.join(", ") }))}</div>`
      : "";
    // A restore into another server is marked so it cannot pass for a local one.
    const cross = crossServer
      ? `<div class="cell-sub cross-server" title="${escapeHtml(`${source || "?"} → ${target}`)}">${ellipsis(source || "?")}<span aria-hidden="true">→</span><span class="sr-only">${escapeHtml(t("tables.cross_server"))}:</span>${ellipsis(target)}</div>`
      : "";
    return `<tr class="row-clickable" data-row-action="restore-details" data-id="${escapeHtml(r.id)}" tabindex="0">
      ${bulkCell("restores", r.id, `${r.target_database} · ${r.id}`)}
      <td class="cell-primary">${ellipsis(r.target_database, "ell-md")}${cross}${selection}${idCopy(r.id, "cell-sub")}</td>
      <td>${ellipsis(r.source_database)}${source ? `<div class="cell-sub">${ellipsis(source)}</div>` : ""}</td>
      <td><div class="status-line">${statusBadge(kind, label, r.error_message)}${chips.join("")}${cancelRunButton("restore", r)}</div>${errorLine}${runProgressHtml(r)}</td>
      <td>${timeCell(r.started_at)}</td>
      <td class="num">${durationCell(r)}</td>
    </tr>`;
  }).join(""));
}

// Source and target connection names of a restore record, and whether it went to
// another server. The source comes from the record's snapshot or its backup (fetched
// by loadRestoreBackups when it is not on a loaded page); the target defaults to the
// source. Without a known source the restore is never marked as cross-server.
function restoreConnections(r) {
  const backup = backupCache.get(r.backup_id) || {};
  const sourceId = r.source_connection_id || backup.connection_id || "";
  const source = r.source_connection_name || backup.connection_name || (sourceId ? connectionName(sourceId) : "");
  const targetId = r.target_connection_id || "";
  const target = r.target_connection_name || (targetId ? connectionName(targetId) : "");
  if (targetId && sourceId && targetId === sourceId) return { source, target: source, crossServer: false };
  const crossServer = sourceId && targetId ? true : Boolean(source && target && source !== target);
  return { source, target: target || source, crossServer };
}

function channelTypeLabel(type) {
  return CHANNEL_TYPES.includes(type) ? t(`notify.type_${type}`) : String(type || "");
}

function eventLabel(ev) {
  const key = String(ev).replace(".", "_");
  return t(`notify.events.${key}`, t(`restore_checks.event_${key}`, String(ev)));
}

function enabledBadge(enabled) {
  return enabled
    ? statusBadge("success", t("status.enabled"))
    : statusBadge("neutral", t("status.disabled"));
}

function renderChannels() {
  const tbody = document.getElementById("channels-tbody");
  if (!tbody) return;
  if (!state.loaded.channels) return;

  if (state.channels.length === 0) {
    setTbody(tbody, emptyRow(5, t("notify.empty_channels"), "new-channel", "plus", t("notify.add_channel")));
    return;
  }

  const channels = typeof tablesRows === "function" ? tablesRows("channels", state.channels) : state.channels;
  if (channels.length === 0) {
    setTbody(tbody, tablesNoMatch("channels"));
    return;
  }

  setTbody(tbody, channels.map(ch => {
    const ld = ch.last_delivery;
    let delivery = `<span class="muted">${escapeHtml(t("notify.never"))}</span>`;
    if (ld) {
      const d = parseDate(ld.time);
      const kind = ld.success ? "success" : "danger";
      const label = ld.success ? t("notify.delivered") : t("notify.delivery_failed");
      const title = ld.success ? (d ? formatAbsolute(d) : "") : String(ld.error || "");
      const when = d
        ? ` <span class="muted">· <time datetime="${escapeHtml(d.toISOString())}" title="${escapeHtml(formatAbsolute(d))}">${escapeHtml(formatRelative(d))}</time></span>`
        : "";
      delivery = `<span class="delivery${ld.success ? "" : " text-danger"}" title="${escapeHtml(title)}">${statusMark(kind, "")}${escapeHtml(label)}${when}</span>`;
    }
    return `<tr>
      <td class="cell-primary">${escapeHtml(ch.name)}${idCopy(ch.id, "cell-sub")}</td>
      <td>${escapeHtml(channelTypeLabel(ch.type))}</td>
      <td>${enabledBadge(ch.enabled)}</td>
      <td>${delivery}</td>
      <td class="col-actions"><div class="row-actions">
        <button type="button" class="btn btn-secondary btn-sm" data-action="test-channel" data-id="${escapeHtml(ch.id)}">${escapeHtml(t("notify.send_test"))}</button>
        <button type="button" class="btn btn-secondary btn-sm" data-action="edit-channel" data-id="${escapeHtml(ch.id)}">${escapeHtml(t("notify.edit"))}</button>
        <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="delete-channel" data-id="${escapeHtml(ch.id)}">${escapeHtml(t("actions.delete"))}</button>
      </div></td>
    </tr>`;
  }).join(""));
}

function renderRules() {
  const tbody = document.getElementById("rules-tbody");
  if (!tbody) return;
  if (!state.loaded.rules) return;

  if (state.rules.length === 0) {
    setTbody(tbody, emptyRow(6, t("notify.empty_rules"), "new-rule", "plus", t("notify.add_rule")));
    return;
  }

  const channelName = id => {
    const ch = state.channels.find(c => c.id === id);
    return ch ? ch.name : id;
  };

  setTbody(tbody, state.rules.map(rule => {
    const eventsHtml = (rule.events || [])
      .map(ev => `<span class="chip">${escapeHtml(eventLabel(ev))}</span>`)
      .join("");
    const jobIDs = rule.job_ids || [];
    const jobsText = jobIDs.length === 0 ? t("notify.all_jobs") : jobIDs.map(jobLabel).join(", ");
    const channelIDs = rule.channel_ids || [];
    const channelsText = channelIDs.length === 0 ? t("notify.no_channels") : channelIDs.map(channelName).join(", ");
    return `<tr>
      <td class="cell-primary">${escapeHtml(rule.name)}</td>
      <td><div class="chip-list">${eventsHtml}</div></td>
      <td class="cell-wrap">${escapeHtml(jobsText)}</td>
      <td class="cell-wrap">${escapeHtml(channelsText)}</td>
      <td>${enabledBadge(rule.enabled)}</td>
      <td class="col-actions"><div class="row-actions">
        <button type="button" class="btn btn-secondary btn-sm" data-action="edit-rule" data-id="${escapeHtml(rule.id)}">${escapeHtml(t("notify.edit"))}</button>
        <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="delete-rule" data-id="${escapeHtml(rule.id)}">${escapeHtml(t("actions.delete"))}</button>
      </div></td>
    </tr>`;
  }).join(""));
}

// ---------------------------------------------------------------------------
// Render helpers (all return HTML with every dynamic value escaped)
// ---------------------------------------------------------------------------

const ICON_PATHS = {
  plus: '<path d="M8 3.5v9M3.5 8h9"/>',
  check: '<path d="M3.5 8.5 6.5 11.5 12.5 4.5"/>',
  x: '<path d="M4.5 4.5l7 7M11.5 4.5l-7 7"/>',
  lock: '<rect x="3.5" y="7" width="9" height="6.5" rx="1.5"/><path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2"/>',
  more: '<circle cx="3.5" cy="8" r="0.6"/><circle cx="8" cy="8" r="0.6"/><circle cx="12.5" cy="8" r="0.6"/>',
  copy: '<rect x="5.5" y="5.5" width="8" height="8" rx="1.5"/><path d="M10.5 5.5V4A1.5 1.5 0 0 0 9 2.5H4A1.5 1.5 0 0 0 2.5 4v5A1.5 1.5 0 0 0 4 10.5h1.5"/>'
};

function icon(name, extraClass) {
  const paths = ICON_PATHS[name];
  if (!paths) return "";
  const cls = extraClass ? `icon ${extraClass}` : "icon";
  const weight = name === "check" || name === "x" || name === "more" ? "2" : "1.5";
  return `<svg class="${cls}" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="${weight}" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">${paths}</svg>`;
}

// Success always carries a check glyph and failure an x, so status never relies
// on color alone (and success teal is never mistaken for the brand accent).
function statusGlyph(kind, iconClass) {
  if (kind === "success") return icon("check", iconClass);
  if (kind === "danger") return icon("x", iconClass);
  return '<span class="badge-dot status-dot" aria-hidden="true"></span>';
}

function statusBadge(kind, label, title) {
  const titleAttr = title ? ` title="${escapeHtml(title)}"` : "";
  return `<span class="badge badge-${escapeHtml(kind)}"${titleAttr}>${statusGlyph(kind, "badge-icon")}${escapeHtml(label)}</span>`;
}

function statusMark(kind, label) {
  const labelAttr = label ? ` title="${escapeHtml(label)}" role="img" aria-label="${escapeHtml(label)}"` : ' aria-hidden="true"';
  return `<span class="status-mark status-${escapeHtml(kind)}"${labelAttr}>${statusGlyph(kind)}</span>`;
}

function emptyRow(colspan, text, action, iconName, label, tab, variant) {
  const btnClass = variant || "btn-primary";
  const tabAttr = tab ? ` data-tab="${escapeHtml(tab)}"` : "";
  const button = action
    ? `<button type="button" class="btn ${btnClass} btn-sm" data-action="${escapeHtml(action)}"${tabAttr}>${icon(iconName)}<span>${escapeHtml(label)}</span></button>`
    : "";
  return `<tr class="empty-row"><td colspan="${Number(colspan) || 1}"><div class="empty-state"><p>${escapeHtml(text)}</p>${button}</div></td></tr>`;
}

// Text that may be long (names, IDs): one line with an ellipsis, full text on hover.
function ellipsis(text, extraClass) {
  const cls = extraClass ? `ellipsis ${extraClass}` : "ellipsis";
  return `<span class="${cls}" title="${escapeHtml(text)}">${escapeHtml(text)}</span>`;
}

// Small icon button that copies value (an ID) to the clipboard.
function copyButton(value) {
  const label = escapeHtml(t("ui.copy_id"));
  return `<button type="button" class="copy-btn" data-action="copy-id" data-copy="${escapeHtml(value)}" title="${label}" aria-label="${label}">${icon("copy")}</button>`;
}

// A truncated monospace ID (full ID on hover) followed by its copy button.
function idCopy(id, extraClass) {
  const cls = extraClass ? `id-copy ${extraClass}` : "id-copy";
  return `<div class="${cls}">${ellipsis(id, "mono ell-md")}${copyButton(id)}</div>`;
}

// Copies text to the clipboard (falling back to execCommand outside secure
// contexts) and briefly marks the button that asked for it.
async function copyText(text, btn) {
  if (!text) return;
  let copied = false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      copied = true;
    }
  } catch (err) {
    copied = false;
  }
  if (!copied) {
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.className = "sr-only";
    document.body.appendChild(area);
    area.select();
    try {
      copied = document.execCommand("copy");
    } catch (err) {
      copied = false;
    }
    area.remove();
    if (btn && document.contains(btn)) btn.focus();
  }
  if (!copied) {
    showToast(t("ui.copy_failed"), "error");
    return;
  }
  showToast(t("ui.copied"), "success");
  if (btn) {
    btn.classList.add("copied");
    btn.innerHTML = icon("check");
    setTimeout(() => {
      btn.classList.remove("copied");
      btn.innerHTML = icon("copy");
    }, 1500);
  }
}

// Error line of a table cell: a short summary that expands (click, Enter or
// Space) into the full message, so nothing is lost to truncation.
function errorDetail(full, summary) {
  const text = String(full || "");
  if (!text) return "";
  return `<details class="cell-sub error-detail"><summary class="cell-error" title="${escapeHtml(text)}">${escapeHtml(summary || text)}</summary><div class="error-full">${escapeHtml(text)}</div></details>`;
}

function mutedDash() {
  return '<span class="muted">—</span>';
}

function timeCell(value) {
  const d = parseDate(value);
  if (!d) return mutedDash();
  // Relative or absolute, as chosen in the user menu (nav.js).
  const [shown, tooltip] = typeof timeDisplay === "function" ? timeDisplay(d) : [formatRelative(d), formatAbsolute(d)];
  return `<time datetime="${escapeHtml(d.toISOString())}" title="${escapeHtml(tooltip)}">${escapeHtml(shown)}</time>`;
}

// Replaces a table body only when its markup changed, so the periodic refresh does
// not reset scroll, selection or keyboard focus inside unchanged tables.
const renderedTbodies = new WeakMap();

function setTbody(tbody, html) {
  if (renderedTbodies.get(tbody) !== html) {
    tbody.innerHTML = html;
    renderedTbodies.set(tbody, html);
    if (typeof tablesRendered === "function") tablesRendered(tbody);
    // A re-render replaces the row whose action menu is open.
    if (rowMenu.trigger && !document.contains(rowMenu.trigger)) closeRowMenu(false);
  }
  // Row selection (bulk.js) is applied to the rows after every render.
  bulkSync(tbody.id);
}

// ---------------------------------------------------------------------------
// Row action menus: the "⋯" button of a table row opens one shared floating
// menu (fixed-positioned, so the table's scroll container never clips it).
// Items are ordinary data-action buttons handled by setupActions.
// ---------------------------------------------------------------------------

const rowMenu = { trigger: null };

function rowMenuButton(kind, id) {
  const label = escapeHtml(t("ui.more_actions"));
  return `<button type="button" class="btn btn-secondary btn-sm btn-icon" data-action="row-menu" data-menu="${escapeHtml(kind)}" data-id="${escapeHtml(id)}" aria-haspopup="menu" aria-expanded="false" title="${label}" aria-label="${label}">${icon("more")}</button>`;
}

// Items of a row menu: [action, label, danger].
function rowMenuItems(kind, id) {
  if (kind === "job") {
    return [["job-details", t("actions.details")], ["edit-job", t("ui.edit")], ...jobRunMenuItems(id), ["delete-job", t("actions.delete"), true]];
  }
  if (kind === "backup") {
    // A deleted backup offers Undo instead (protection.js).
    const deleted = typeof protectionBackupMenuItems === "function" ? protectionBackupMenuItems(id) : null;
    if (deleted) return deleted;
    // Run control (runs.js), verify now and pin / unpin (trust.js).
    return [["backup-details", t("actions.details")], ...backupRunMenuItems(id), ...trustBackupMenuItems(id), ["delete-backup", t("actions.delete"), true]];
  }
  return [];
}

function toggleRowMenu(btn) {
  if (rowMenu.trigger === btn) {
    closeRowMenu(true);
    return;
  }
  closeRowMenu(false);
  const menu = document.getElementById("row-menu");
  if (!menu) return;
  menu.textContent = "";
  const id = btn.dataset.id || "";
  rowMenuItems(btn.dataset.menu || "", id).forEach(([action, label, danger], i) => {
    if (danger && i > 0) {
      const sep = document.createElement("div");
      sep.className = "menu-sep";
      sep.setAttribute("role", "separator");
      menu.appendChild(sep);
    }
    const item = document.createElement("button");
    item.type = "button";
    item.className = danger ? "menu-item menu-item-danger" : "menu-item";
    item.setAttribute("role", "menuitem");
    item.dataset.action = action;
    item.dataset.id = id;
    item.textContent = label;
    menu.appendChild(item);
  });
  rowMenu.trigger = btn;
  menu.setAttribute("aria-label", t("ui.more_actions"));
  btn.setAttribute("aria-expanded", "true");
  menu.hidden = false;
  positionRowMenu();
  const first = menu.querySelector(".menu-item");
  if (first) first.focus();
}

// Right-aligns the menu under its button, or above it near the bottom edge.
function positionRowMenu() {
  const menu = document.getElementById("row-menu");
  if (!menu || !rowMenu.trigger) return;
  const r = rowMenu.trigger.getBoundingClientRect();
  const w = menu.offsetWidth;
  const h = menu.offsetHeight;
  const left = Math.max(8, Math.min(r.right - w, window.innerWidth - w - 8));
  const below = r.bottom + 4 + h <= window.innerHeight - 8;
  menu.style.left = `${left}px`;
  menu.style.top = `${below ? r.bottom + 4 : Math.max(8, r.top - 4 - h)}px`;
}

function closeRowMenu(focusTrigger) {
  const menu = document.getElementById("row-menu");
  const trigger = rowMenu.trigger;
  rowMenu.trigger = null;
  if (menu) menu.hidden = true;
  if (trigger) {
    trigger.setAttribute("aria-expanded", "false");
    if (focusTrigger && document.contains(trigger)) trigger.focus();
  }
}

function setupRowMenu() {
  const menu = document.getElementById("row-menu");
  if (!menu) return;
  // Outside clicks close the menu (clicks on items close it in setupActions).
  document.addEventListener("click", (e) => {
    if (!rowMenu.trigger) return;
    if (menu.contains(e.target) || rowMenu.trigger.contains(e.target)) return;
    closeRowMenu(false);
  }, true);
  menu.addEventListener("keydown", (e) => {
    const items = Array.from(menu.querySelectorAll(".menu-item"));
    const idx = items.indexOf(document.activeElement);
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      closeRowMenu(true);
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const next = e.key === "ArrowDown" ? (idx + 1) % items.length : (idx - 1 + items.length) % items.length;
      items[next].focus();
    } else if (e.key === "Home" || e.key === "End") {
      e.preventDefault();
      items[e.key === "Home" ? 0 : items.length - 1].focus();
    } else if (e.key === "Tab") {
      e.preventDefault();
      closeRowMenu(true);
    }
  });
  window.addEventListener("resize", () => closeRowMenu(false));
  // Scrolling the page or a table moves the button away from the menu.
  document.addEventListener("scroll", (e) => {
    if (rowMenu.trigger && !menu.contains(e.target)) closeRowMenu(false);
  }, true);
}

function setText(id, text) {
  const el = document.getElementById(id);
  if (el) el.textContent = text;
}

// errorSummary extracts the most useful part of a backend error: when the
// message carries mongodump/mongorestore stderr, the tool's own "Failed: ..."
// line is shown instead of the generic "exit status 1" prefix.
function errorSummary(text) {
  const s = String(text || "");
  const idx = s.indexOf("(stderr:");
  if (idx === -1) return s;
  const stderr = s.substring(idx + 8).replace(/\)\s*$/, "");
  const failed = stderr.lastIndexOf("Failed:");
  const detail = (failed === -1 ? stderr : stderr.substring(failed + 7))
    .replace(/\d{4}-\d{2}-\d{2}T[\d:.]+[+-]\d{4}\s*/g, "")
    .trim();
  // Tool errors nest causes as "a: b: c"; the innermost cause is the useful one.
  const parts = detail.split(": ").filter(Boolean);
  let cause = parts.pop() || "";
  while (cause.length < 32 && parts.length) cause = `${parts.pop()}: ${cause}`;
  return cause || s;
}

function truncate(text, max) {
  const s = String(text || "").replace(/\s+/g, " ").trim();
  return s.length > max ? `${s.substring(0, max - 1)}…` : s;
}

// ---------------------------------------------------------------------------
// Forms
// ---------------------------------------------------------------------------

function setupForms() {
  document.getElementById("form-new-job").addEventListener("submit", async (e) => {
    e.preventDefault();
    // A single database comes from the shared picker, several from jobdbs.js.
    const source = typeof jobDbsSource === "function" ? jobDbsSource() : pickerValue("job");
    if (!source) return;
    const payload = {
      name: getValue("job-name"),
      collections: [],
      exclude_collections: [],
      ...source,
      cron_expression: getValue("job-cron"),
      retention_days: parseInt(getValue("job-retention-days"), 10) || 0,
      retention_count: parseInt(getValue("job-retention-count"), 10) || 0,
      ...storageSelection("job-storage"),
      ...trustJobPayload(),
      include_users_and_roles: document.getElementById("job-users-roles").checked,
      ...(typeof readinessJobPayload === "function" ? readinessJobPayload() : {}),
      ...(typeof monitoringJobPayload === "function" ? monitoringJobPayload() : {}),
      enabled: document.getElementById("job-enabled").checked
    };
    // Editing replaces the job in place (PUT keeps its id, history and gzip setting);
    // updated_at makes the server refuse the save if the job changed since it opened.
    const editing = editingJobId;
    if (editing && editingJobUpdatedAt) payload.updated_at = editingJobUpdatedAt;
    const url = editing ? `/api/v1/jobs/${encodeURIComponent(editing)}` : "/api/v1/jobs";
    const submit = e.submitter || document.getElementById("job-submit");
    if (submit) submit.disabled = true;
    try {
      const json = await apiJSON(url, {
        method: editing ? "PUT" : "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload)
      });
      if (json.success) {
        // A shorter retention takes effect later (protection.js).
        const deferred = typeof protectionJobSaved === "function" && protectionJobSaved(json.data);
        if (!deferred) showToast(editing ? t("job_edit.updated") : t("toasts.job_created"), "success");
        closeModal("modal-new-job");
        refreshAll();
        // The schedule and RPO feed the readiness report: refetch it now.
        if (typeof overviewRefresh === "function") overviewRefresh(true);
      } else if (editing && json.httpStatus === 409) {
        showToast(t("job_edit.conflict"), "error");
        refreshAll();
      } else {
        showToast(json.error || (editing ? t("job_edit.update_failed") : t("toasts.job_failed")), "error");
      }
    } catch (err) {
      showToast(err.message, "error");
    } finally {
      if (submit) submit.disabled = false;
    }
  });

  document.getElementById("form-instant-backup").addEventListener("submit", async (e) => {
    e.preventDefault();
    // One database comes from the shared picker, several from jobdbs.js.
    const source = typeof instantDbsSource === "function" ? instantDbsSource() : pickerValue("instant");
    if (!source) return;
    try {
      closeModal("modal-instant-backup");
      const json = await apiJSON("/api/v1/backups", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...source, ...storageSelection("instant-storage") })
      });
      if (json.success && source.databases) {
        // Several databases: one run, one toast that opens its backups.
        instantDbsStarted(json.data);
      } else if (json.success) {
        showToast(t("toasts.backup_started"), "info");
        trackBackup(json.data);
      } else {
        showToast(json.error || t("toasts.backup_failed"), "error");
      }
    } catch (err) {
      showToast(err.message, "error");
    } finally {
      refreshAll();
    }
  });

  const safeClone = document.getElementById("restore-safe-clone");
  safeClone.addEventListener("change", () => updateRestoreMode(true));
  document.getElementById("restore-confirm-in-place").addEventListener("change", () => updateRestoreMode(false));
  document.getElementById("restore-target-db").addEventListener("input", updateRestoreUsersRoles);
  onLanguageChange(() => updateRestoreUsersRoles());
  setupRestoreCollections();
  setupRestorePreflight();

  document.getElementById("form-restore").addEventListener("submit", async (e) => {
    e.preventDefault();
    const isSafeClone = document.getElementById("restore-safe-clone").checked;
    const targetDB = getValue("restore-target-db");
    const dropTarget = document.getElementById("restore-drop-target").checked;
    const usersRoles = document.getElementById("restore-users-roles");
    const restoreUsersRoles = !isSafeClone && !usersRoles.disabled && usersRoles.checked;

    // The API defaults to a safe clone; writing into a named database needs explicit consent.
    const confirmInPlace = !isSafeClone && document.getElementById("restore-confirm-in-place").checked;
    if (!isSafeClone && !confirmInPlace) {
      document.getElementById("restore-confirm-in-place").focus();
      return;
    }
    const selected = restoreSelection();
    if (selected === null) return;
    // A failed preflight blocks the restore until "Restore anyway" is ticked.
    if (restorePreflightBlocks()) {
      document.getElementById("restore-force").focus();
      return;
    }
    const dropConfirm = selected.length > 0
      ? tf("modal_restore.drop_confirm_selected", { list: selected.join(", ") })
      : t("modal_restore.drop_confirm");
    if (!isSafeClone && dropTarget && !(await confirmDialog({ body: dropConfirm, danger: true, confirmLabel: t("dialog.confirm") }))) {
      return;
    }
    // Restoring users and roles replaces every user and role defined on the database.
    if (restoreUsersRoles && !(await confirmDialog({
      body: tf("modal_restore.users_roles_confirm", { db: targetDB || getValue("restore-backup-id") }),
      danger: true,
      confirmLabel: t("dialog.confirm")
    }))) {
      return;
    }

    const body = restoreRequestBody(selected);
    if (restorePreflightFailing() && document.getElementById("restore-force").checked) body.force = true;

    try {
      closeModal("modal-restore");
      const json = await apiJSON("/api/v1/restore", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body)
      });
      if (json.success) {
        const target = json.data && json.data.target_database ? json.data.target_database : "";
        showToast(tf("toasts.restore_started", { db: target }), "info");
        trackRestore(json.data);
      } else if (json.data && Array.isArray(json.data.checks)) {
        // Refused by the server's preflight (409): show its checks in the dialog again.
        Object.assign(restorePreflight, { loading: false, result: json.data, error: "" });
        openModal("modal-restore");
        renderRestorePreflight();
        showToast(json.error || t("toasts.restore_failed"), "error");
      } else {
        showToast(json.error || t("toasts.restore_failed"), "error");
      }
    } catch (err) {
      showToast(err.message, "error");
    } finally {
      refreshAll();
    }
  });

  document.getElementById("form-setup").addEventListener("submit", submitSetup);
  document.getElementById("form-login").addEventListener("submit", submitLogin);
  document.getElementById("form-connection").addEventListener("submit", saveConnection);
  document.getElementById("connection-uri").addEventListener("input", resetConnectionTest);
  document.getElementById("form-user").addEventListener("submit", saveUser);
  document.getElementById("form-password").addEventListener("submit", savePassword);
  document.getElementById("form-api-key").addEventListener("submit", createApiKey);
  setupPicker("job");
  setupPicker("instant");
  setupSettingsForms();
  setupStorageForm();

  const typeSelect = document.getElementById("channel-type");
  typeSelect.addEventListener("change", () => showChannelFields(typeSelect.value));
  document.getElementById("form-channel").addEventListener("submit", saveChannel);
  document.getElementById("form-rule").addEventListener("submit", saveRule);
}

// Opens the job form: empty for a new job, or prefilled with job jobID to edit it.
function openJobModal(jobID) {
  const job = jobID ? state.jobs.find(j => j.id === jobID) : null;
  if (jobID && !job) return;
  const form = document.getElementById("form-new-job");
  if (form) form.reset();
  editingJobId = job ? job.id : "";
  editingJobUpdatedAt = job ? job.updated_at || "" : "";
  setI18nText("modal-new-job-title", job ? "job_edit.title" : "modal_job.title");
  setI18nText("job-submit", job ? "job_edit.save" : "modal_job.submit");
  if (job) {
    // Editing replaces the details dialog, which would otherwise sit on top.
    closeModal("modal-job-details");
    setValue("job-name", job.name || "");
    setValue("job-cron", job.cron_expression || "");
    setValue("job-retention-days", Number(job.retention_days) || 0);
    setValue("job-retention-count", Number(job.retention_count) || 0);
    document.getElementById("job-enabled").checked = job.enabled !== false;
    document.getElementById("job-users-roles").checked = job.include_users_and_roles === true;
    presetPicker("job", job);
    fillStorageSelect(document.getElementById("job-storage"), job.storage_target_id);
  } else {
    // New jobs start from the retention defaults configured under Settings > General.
    const general = settingsGroup("general");
    if (general.default_retention_days !== undefined) setValue("job-retention-days", general.default_retention_days);
    if (general.default_retention_count !== undefined) setValue("job-retention-count", general.default_retention_count);
    document.getElementById("job-enabled").checked = true;
    document.getElementById("job-users-roles").checked = false;
    resetPicker("job", "");
    fillStorageSelect(document.getElementById("job-storage"));
  }
  // Verification override, restore test and the retention preview (trust.js).
  trustFillJobForm(job);
  // The recovery point objective (readiness.js).
  if (typeof readinessFillJobForm === "function") readinessFillJobForm(job);
  // The job's heartbeat URL (monitoring.js).
  if (typeof monitoringFillJobForm === "function") monitoringFillJobForm(job);
  // Single / Selected / All / Pattern database selection (jobdbs.js).
  if (typeof jobDbsFill === "function") jobDbsFill(job);
  openModal("modal-new-job");
}

// Sets an element's text from translation key and keeps it translated when the
// language changes (applyTranslations reads data-i18n).
function setI18nText(id, key) {
  const el = document.getElementById(id);
  if (!el) return;
  el.dataset.i18n = key;
  el.textContent = t(key);
}

// Enables or disables job jobID. The job is fetched fresh and sent back unchanged
// apart from enabled, with its updated_at as precondition, so a toggle never
// reverts an edit made elsewhere since the list was loaded.
async function toggleJob(jobID, btn) {
  const listed = state.jobs.find(j => j.id === jobID);
  if (!listed || togglingJobs.has(jobID)) return;
  const enable = listed.enabled === false;
  togglingJobs.add(jobID);
  if (btn) btn.disabled = true;
  try {
    const fresh = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}`);
    if (!fresh.success || !fresh.data) {
      showToast(fresh.error || t("job_edit.update_failed"), "error");
      return;
    }
    const job = fresh.data;
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: job.name || "",
        cron_expression: job.cron_expression || "",
        database: job.database || "",
        database_selection: job.database_selection,
        parallelism: job.parallelism || 1,
        collections: job.collections || [],
        exclude_collections: job.exclude_collections || [],
        connection_id: job.connection_id || "",
        storage_target_id: job.storage_target_id || "",
        enabled: enable,
        updated_at: job.updated_at
      })
    });
    if (json.success) {
      showToast(enable ? t("job_edit.enabled_toast") : t("job_edit.disabled_toast"), "success");
    } else if (json.httpStatus === 409) {
      showToast(t("job_edit.conflict"), "error");
    } else {
      showToast(json.error || t("job_edit.update_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    togglingJobs.delete(jobID);
    if (btn) btn.disabled = false;
    refreshAll();
  }
}

function openJobDetails(jobID) {
  if (!state.jobs.some(j => j.id === jobID)) return;
  // Opened from a backup's details, the job dialog comes back to the front.
  closeModal("modal-backup-details");
  if (detailsJobId !== jobID) {
    jobNextRuns.key = "";
    jobNextRuns.runs = null;
    jobNextRuns.rpoMinutes = 0;
    jobHistory.jobId = "";
    jobHistory.runs = null;
    jobHistory.failed = false;
  }
  detailsJobId = jobID;
  openModal("modal-job-details");
  renderJobDetails();
  loadJobHistory(jobID);
  if (typeof jobDbsLoadRuns === "function") jobDbsLoadRuns(jobID);
}

// Fetches the next activations of the job in the details dialog when its schedule
// changed since the last fetch or the first listed run has passed.
function refreshJobNextRuns(job) {
  const key = `${job.id}|${job.cron_expression}|${job.enabled !== false}|${job.updated_at || ""}`;
  const first = jobNextRuns.runs && jobNextRuns.runs.length > 0 ? parseDate(jobNextRuns.runs[0]) : null;
  const stale = first && first.getTime() <= Date.now();
  if (jobNextRuns.key === key && !stale) return;
  jobNextRuns.key = key;
  jobNextRuns.failed = false;
  const seq = ++jobNextRuns.seq;
  apiJSON(`/api/v1/jobs/${encodeURIComponent(job.id)}`)
    .then(json => {
      if (seq !== jobNextRuns.seq) return;
      if (!json.success) throw new Error(json.error || "");
      jobNextRuns.runs = (json.data && json.data.next_runs) || [];
      jobNextRuns.rpoMinutes = (json.data && Number(json.data.effective_rpo_minutes)) || 0;
      jobNextRuns.rpoDefault = !!(json.data && json.data.rpo_default);
    })
    .catch(() => {
      if (seq !== jobNextRuns.seq) return;
      jobNextRuns.runs = null;
      jobNextRuns.failed = true;
    })
    .finally(() => {
      if (seq === jobNextRuns.seq) renderJobDetails();
    });
}

// Appends a label/value pair to a <dl class="kv">. value is text (set through
// textContent) or a Node built with textContent; empty values show a dash.
function appendKv(list, label, value, mono) {
  const dt = document.createElement("dt");
  dt.textContent = label;
  const dd = document.createElement("dd");
  if (mono) dd.className = "mono";
  if (value instanceof Node) {
    dd.appendChild(value);
  } else {
    dd.textContent = value === undefined || value === null || value === "" ? "—" : String(value);
  }
  list.append(dt, dd);
}

// "<absolute> (<relative>)" for a timestamp, or "".
function absoluteWithRelative(value) {
  const d = parseDate(value);
  return d ? `${formatAbsolute(d)} (${formatRelative(d)})` : "";
}

// Element holding the HTML of a render helper that escapes every dynamic value.
function htmlNode(html, tag) {
  const el = document.createElement(tag || "span");
  el.innerHTML = html;
  return el;
}

// Fills the job details dialog of detailsJobId from the loaded jobs and backups.
// Server values are set through textContent or escapeHtml.
function renderJobDetails() {
  const modal = document.getElementById("modal-job-details");
  if (!modal || !modal.classList.contains("open") || !detailsJobId) return;
  const job = state.jobs.find(j => j.id === detailsJobId);
  if (!job) {
    closeModal("modal-job-details");
    return;
  }
  const enabled = job.enabled !== false;
  setText("job-details-name", job.name || job.id);
  // The header carries the last run's outcome of a multi-database job (jobdbs.js).
  if (typeof jobLastRunMark === "function") {
    const headMark = jobLastRunMark(job);
    if (headMark) {
      const head = document.getElementById("job-details-name");
      head.append(" ");
      head.appendChild(htmlNode(headMark));
    }
  }

  const overview = document.getElementById("job-details-overview");
  overview.textContent = "";
  appendKv(overview, t("job_details.name"), job.name);
  const idLine = htmlNode(idCopy(job.id), "div");
  appendKv(overview, t("job_details.id"), idLine.firstElementChild || job.id);
  appendKv(overview, t("job_details.state"), htmlNode(jobStateBadge(job)));
  appendKv(overview, t("job_details.connection"), job.connection_id ? connectionName(job.connection_id) : "");
  // A multi-database job shows its selection instead (jobdbs.js).
  if (typeof jobDbsDetailsOverview !== "function" || !jobDbsDetailsOverview(job, overview)) {
    appendKv(overview, t("job_details.database"), job.database, true);
    const include = job.collections || [];
    const exclude = job.exclude_collections || [];
    appendKv(overview, t("job_details.collections"), include.length > 0
      ? tf("job_details.only", { list: include.join(", ") })
      : exclude.length > 0 ? tf("job_details.except", { list: exclude.join(", ") }) : t("job_details.all_collections"));
  }
  appendKv(overview, t("job_details.target"), job.storage_target_id ? storageTargetName(job.storage_target_id) : storageDescription(job.storage_type));

  const schedule = document.getElementById("job-details-schedule");
  schedule.textContent = "";
  appendKv(schedule, t("job_details.cron"), job.cron_expression, true);
  appendKv(schedule, t("job_details.meaning"), describeCron(job.cron_expression) || t("job_details.custom"));
  refreshJobNextRuns(job);
  let next;
  if (!enabled) {
    const until = parseDate(job.paused_until);
    next = until ? tf("run.resumes_at", { time: formatAbsolute(until) }) : t("job_details.paused");
  } else if (jobNextRuns.runs && jobNextRuns.runs.length > 0) {
    next = document.createElement("ol");
    next.className = "next-runs";
    jobNextRuns.runs.forEach(value => {
      const d = parseDate(value);
      if (!d) return;
      const li = document.createElement("li");
      const time = document.createElement("time");
      time.dateTime = d.toISOString();
      time.title = formatRelative(d);
      time.textContent = formatAbsolute(d);
      li.appendChild(time);
      next.appendChild(li);
    });
  } else {
    next = jobNextRuns.failed || jobNextRuns.runs ? "" : t("tables.loading");
  }
  appendKv(schedule, t("job_details.next_runs"), next);
  appendKv(schedule, t("job_details.last_run"), absoluteWithRelative(job.last_run) || t("job_details.never"));
  // The outcome of a multi-database job's last run, as in the jobs table (jobdbs.js).
  if (typeof jobLastRunMark === "function") {
    const mark = jobLastRunMark(job, true);
    if (mark) appendKv(schedule, t("jobdb.last_run_result"), htmlNode(mark, "div"));
  }

  const options = document.getElementById("job-details-options");
  options.textContent = "";
  appendKv(options, t("job_details.retention"), retentionText(job));
  appendKv(options, t("job_details.compression"), job.gzip ? t("job_details.gzip_on") : t("job_details.off"));
  appendKv(options, t("job_details.users_roles"), job.include_users_and_roles ? t("job_details.users_roles_on") : t("job_details.off"));
  // The effective recovery point objective, from GET /api/v1/jobs/{id} (readiness.js).
  if (typeof readinessDuration === "function" && jobNextRuns.rpoMinutes > 0) {
    const rpo = readinessDuration(jobNextRuns.rpoMinutes * 60);
    appendKv(options, t("readiness.details_rpo"), jobNextRuns.rpoDefault ? tf("readiness.details_rpo_default", { rpo }) : rpo);
  }
  const enc = settingsGroup("encryption");
  appendKv(options, t("job_details.encryption"), !state.loaded.settings
    ? ""
    : enc.enabled ? tf("job_details.enc_on", { mode: enc.mode === "passphrase" ? t("job_details.enc_passphrase") : "age (X25519)" }) : t("job_details.off"));
  appendKv(options, t("job_details.created"), absoluteWithRelative(job.created_at));
  appendKv(options, t("job_details.updated"), absoluteWithRelative(job.updated_at));
  // Verification override, restore tests and the retention history (trust.js).
  trustJobDetails(job, options);

  renderJobHistory(job);
  // Runs grouped by run, and newly discovered databases (jobdbs.js).
  if (typeof jobDbsRenderDetails === "function") jobDbsRenderDetails(job);

  // Pause (optionally until a time) and resume live in runs.js.
  const toggle = document.getElementById("job-details-toggle");
  setI18nText("job-details-toggle", enabled ? "run.pause" : "run.resume");
  toggle.dataset.action = enabled ? "pause-job" : "resume-job";
  toggle.dataset.id = job.id;
  toggle.disabled = togglingJobs.has(job.id);
  const stop = document.getElementById("job-details-stop");
  if (stop) {
    stop.hidden = !jobActiveRun(job.id);
    stop.dataset.id = job.id;
  }
  document.getElementById("job-details-run").dataset.id = job.id;
  document.getElementById("job-details-empty-run").dataset.id = job.id;
  document.getElementById("job-details-edit").dataset.id = job.id;
}

// Run history of the job details dialog: its last JOB_HISTORY_LIMIT backups, fetched
// from the server (?job_id=…&limit=20), with the success rate of the finished ones.
function renderJobHistory(job) {
  const loaded = jobHistory.jobId === job.id && Array.isArray(jobHistory.runs);
  const runs = loaded ? jobHistory.runs : [];
  const finished = runs.filter(b => ["completed", "pruned", "failed"].includes(b.status));
  const ok = finished.filter(b => b.status !== "failed").length;
  const rate = document.getElementById("job-details-rate");
  rate.textContent = !loaded
    ? (jobHistory.failed ? "" : t("tables.loading"))
    : finished.length > 0
      ? tf("job_details.success_rate", { pct: Math.round((ok / finished.length) * 100), ok, n: finished.length })
      : "";
  rate.className = `detail-meta${finished.length > 0 && ok < finished.length ? " text-danger" : ""}`;
  document.getElementById("job-details-history-wrap").hidden = runs.length === 0;
  document.getElementById("job-details-history-empty").hidden = !loaded || runs.length > 0;
  setTbody(document.getElementById("job-details-runs"), runs.map(b => {
    const [kind, label] = backupStatus(b.status);
    const d = parseDate(b.started_at);
    const started = d
      ? `<time datetime="${escapeHtml(d.toISOString())}" title="${escapeHtml(formatRelative(d))}">${escapeHtml(formatAbsolute(d))}</time>`
      : mutedDash();
    const size = b.status === "completed" || Number(b.size_bytes) > 0 ? escapeHtml(formatBytes(b.size_bytes)) : mutedDash();
    const error = b.status === "failed" && b.error_message
      ? `<span class="cell-error" title="${escapeHtml(b.error_message)}">${escapeHtml(truncate(errorSummary(b.error_message), 60))}</span>`
      : mutedDash();
    return `<tr class="row-clickable" data-row-action="backup-details" data-id="${escapeHtml(b.id)}" tabindex="0">
      <td><button type="button" class="link-btn mono" data-action="backup-details" data-id="${escapeHtml(b.id)}" title="${escapeHtml(b.id)}">${escapeHtml(shortBackupId(b.id))}</button></td>
      <td>${statusBadge(kind, label)}</td>
      <td>${started}</td>
      <td class="num">${durationCell(b)}</td>
      <td class="num">${size}</td>
      <td>${error}</td>
    </tr>`;
  }).join(""));
}

// Preselects a job's source in picker prefix: its connection, database and
// collection filter. The database and collections are applied once their lists
// have loaded (or typed into the manual fields when a list cannot be loaded).
function presetPicker(prefix, job) {
  const p = pickers[prefix];
  const include = job.collections || [];
  const exclude = job.exclude_collections || [];
  const mode = include.length > 0 ? "include" : exclude.length > 0 ? "exclude" : "all";
  const names = mode === "include" ? include : exclude;
  p.preset = { database: job.database || "", names };
  fillConnectionSelect(pickerEl(p, "connection"), job.connection_id);
  setValue(`${prefix}-database`, job.database || "");
  setValue(`${prefix}-collections-manual`, names.join(", "));
  document.querySelectorAll(`input[name="${prefix}-coll-mode"]`).forEach(r => { r.checked = r.value === mode; });
  updatePickerCollectionsBox(p);
  loadPickerDatabases(p);
}

function openBackupNowModal() {
  const form = document.getElementById("form-instant-backup");
  if (form) form.reset();
  if (typeof instantDbsReset === "function") instantDbsReset();
  resetPicker("instant", "");
  fillStorageSelect(document.getElementById("instant-storage"));
  openModal("modal-instant-backup");
}

function openRestoreModal(backupID, sourceDB) {
  const form = document.getElementById("form-restore");
  if (form) form.reset();
  setValue("restore-backup-id", backupID);
  setText("restore-backup-label", backupID);
  setText("restore-source-db", sourceDB);
  document.getElementById("restore-safe-clone").checked = true;
  setValue("restore-target-db", sourceDB);
  document.getElementById("restore-confirm-in-place").checked = false;
  document.getElementById("restore-drop-target").checked = false;
  document.getElementById("restore-users-roles").checked = false;
  // Safe clone is on by default; the verify policy decides the initial checkbox.
  document.getElementById("restore-verify").checked = verifyDefault(true);
  // Target server defaults to the one the backup came from.
  const backup = backupCache.get(backupID) || {};
  setText("restore-storage", backupStorageName(backup) || "—");
  const select = document.getElementById("restore-target-connection");
  let sameAsSource = null;
  if (!backup.connection_id || !state.connections.some(c => c.id === backup.connection_id)) {
    sameAsSource = document.createElement("option");
    sameAsSource.value = "";
    sameAsSource.textContent = backup.connection_name
      ? tf("conn.source_named", { name: backup.connection_name })
      : t("conn.same_as_source");
  }
  fillConnectionSelect(select, backup.connection_id, sameAsSource);
  if (sameAsSource) select.value = "";
  Array.from(select.options).forEach(opt => {
    if (opt.value && opt.value === backup.connection_id) opt.textContent = tf("conn.source_named", { name: opt.textContent });
  });
  document.getElementById("restore-verify-restore").checked = true;
  document.getElementById("restore-force").checked = false;
  updateRestoreMode(false);
  resetRestoreCollections(backupID);
  openModal("modal-restore");
  resetRestorePreflight();
}

// Shows the in-place target + consent box when safe clone is off and keeps the
// submit button disabled until the operator explicitly acknowledges the overwrite.
function updateRestoreMode(fromSafeCloneToggle) {
  const isSafeClone = document.getElementById("restore-safe-clone").checked;
  const inPlace = document.getElementById("restore-in-place");
  inPlace.hidden = isSafeClone;
  if (fromSafeCloneToggle) {
    // Overwriting a live namespace defaults to verifying the backup first
    // (unless Settings > General says always or never).
    document.getElementById("restore-verify").checked = verifyDefault(isSafeClone);
    if (isSafeClone) document.getElementById("restore-confirm-in-place").checked = false;
  }
  updateRestoreSubmit();
  updateRestoreUsersRoles();
}

// Enables the submit button once an in-place restore is acknowledged and no failed
// preflight check stands in the way (unless "Restore anyway" is ticked).
function updateRestoreSubmit() {
  const isSafeClone = document.getElementById("restore-safe-clone").checked;
  const acked = document.getElementById("restore-confirm-in-place").checked;
  const failing = restorePreflightFailing();
  const forceWrap = document.getElementById("restore-force-wrap");
  const force = document.getElementById("restore-force");
  if (forceWrap) {
    forceWrap.hidden = !failing;
    if (!failing) force.checked = false;
  }
  document.getElementById("restore-submit").disabled = (!isSafeClone && !acked) || restorePreflightBlocks();
}

// Builds the body of a restore request (POST /api/v1/restore, and its preflight)
// from the dialog; selected lists the chosen collections ([] = whole database).
function restoreRequestBody(selected) {
  const isSafeClone = document.getElementById("restore-safe-clone").checked;
  const usersRoles = document.getElementById("restore-users-roles");
  const targetConnection = document.getElementById("restore-target-connection").value;
  return {
    backup_id: getValue("restore-backup-id"),
    safe_clone: isSafeClone,
    confirm_in_place: !isSafeClone && document.getElementById("restore-confirm-in-place").checked,
    target_database: isSafeClone ? "" : getValue("restore-target-db"),
    dry_run: document.getElementById("restore-dry-run").checked,
    drop_target: document.getElementById("restore-drop-target").checked,
    verify: document.getElementById("restore-verify").checked,
    verify_restore: document.getElementById("restore-verify-restore").checked,
    ...(!isSafeClone && !usersRoles.disabled && usersRoles.checked ? { restore_users_and_roles: true } : {}),
    ...(selected.length > 0 ? { selected_collections: selected } : {}),
    ...(targetConnection ? { target_connection_id: targetConnection } : {})
  };
}

// ---------------------------------------------------------------------------
// Restore dialog: preflight (go/no-go checks before the restore is started)
// ---------------------------------------------------------------------------

// The preflight of the restore dialog, re-run (debounced) whenever an option changes.
// seq discards answers to older requests; result is the last summary
// ({ ok, checks: [{ id, status, message }] }), error why none could be run.
const restorePreflight = { seq: 0, timer: 0, loading: false, result: null, error: "" };
const RESTORE_PREFLIGHT_DELAY_MS = 400;

function setupRestorePreflight() {
  const form = document.getElementById("form-restore");
  // Options that do not change what the restore does to the target are ignored.
  const ignored = new Set(["restore-force", "restore-verify-restore", "restore-verify", "restore-colls-search"]);
  const changed = (e) => {
    if (e.target && ignored.has(e.target.id)) return;
    scheduleRestorePreflight();
  };
  form.addEventListener("change", changed);
  form.addEventListener("input", changed);
  ["restore-colls-all", "restore-colls-none"].forEach(id => {
    document.getElementById(id).addEventListener("click", scheduleRestorePreflight);
  });
  document.getElementById("restore-force").addEventListener("change", updateRestoreSubmit);
  onLanguageChange(() => renderRestorePreflight());
}

// Forgets the previous dialog's preflight and checks the new one.
function resetRestorePreflight() {
  clearTimeout(restorePreflight.timer);
  Object.assign(restorePreflight, { seq: restorePreflight.seq + 1, timer: 0, loading: false, result: null, error: "" });
  renderRestorePreflight();
  scheduleRestorePreflight();
}

function scheduleRestorePreflight() {
  clearTimeout(restorePreflight.timer);
  restorePreflight.timer = setTimeout(runRestorePreflight, RESTORE_PREFLIGHT_DELAY_MS);
}

// restorePreflightFailing reports whether the last preflight has a failed check.
function restorePreflightFailing() {
  const r = restorePreflight.result;
  return Boolean(r && r.ok === false);
}

// restorePreflightBlocks reports whether a failed check blocks the submit: dry runs
// are never blocked, and "Restore anyway" overrides the checks.
function restorePreflightBlocks() {
  if (!restorePreflightFailing() || document.getElementById("restore-dry-run").checked) return false;
  return !document.getElementById("restore-force").checked;
}

async function runRestorePreflight() {
  restorePreflight.timer = 0;
  const modal = document.getElementById("modal-restore");
  if (!modal || !modal.classList.contains("open") || !getValue("restore-backup-id")) return;
  const seq = ++restorePreflight.seq;
  const selected = restoreSelection(true);
  if (selected === null) {
    // "Selected collections" without a selection yet: nothing to check.
    Object.assign(restorePreflight, { loading: false, result: null, error: "" });
    renderRestorePreflight();
    return;
  }
  restorePreflight.loading = true;
  renderRestorePreflight();
  let result = null;
  let error = "";
  try {
    const json = await apiJSON("/api/v1/restores/preflight", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(restoreRequestBody(selected))
    });
    if (json.success && json.data && Array.isArray(json.data.checks)) result = json.data;
    else error = json.error || t("restore_checks.preflight_unavailable");
  } catch (err) {
    error = err.message;
  }
  if (seq !== restorePreflight.seq) return;
  Object.assign(restorePreflight, { loading: false, result, error });
  renderRestorePreflight();
}

// Shows the go/no-go summary of the restore dialog's preflight.
function renderRestorePreflight() {
  const box = document.getElementById("restore-preflight");
  if (!box) return;
  const { loading, result, error } = restorePreflight;
  box.hidden = !loading && !result && !error;
  box.classList.remove("preflight-ok", "preflight-warn", "preflight-fail");
  const list = document.getElementById("restore-preflight-list");
  let summary = "";
  if (result) {
    const checks = result.checks || [];
    const failed = checks.filter(c => c.status === "fail").length;
    const warned = checks.filter(c => c.status === "warn").length;
    summary = failed ? tf("restore_checks.preflight_blocked", { n: failed })
      : warned ? tf("restore_checks.preflight_ready_warn", { n: warned })
        : t("restore_checks.preflight_ready");
    box.classList.add(failed ? "preflight-fail" : warned ? "preflight-warn" : "preflight-ok");
    fillPreflightList(list, checks);
  } else {
    list.textContent = "";
  }
  if (error && !loading) summary = tf("restore_checks.preflight_error", { error });
  if (loading) summary = summary ? `${summary} ${t("restore_checks.preflight_running")}` : t("restore_checks.preflight_running");
  setText("restore-preflight-summary", summary);
  updateRestoreSubmit();
}

// Fills list with one row per preflight check: a status badge, the check's name and
// its message (server text, set as text).
function fillPreflightList(list, checks) {
  if (!list) return;
  list.textContent = "";
  for (const c of checks || []) {
    const item = document.createElement("li");
    item.className = "preflight-item";
    const badge = htmlNode(preflightBadge(c.status)).firstElementChild;
    if (badge) item.appendChild(badge);
    const name = document.createElement("span");
    name.className = "preflight-check";
    name.textContent = t(`restore_checks.check_${c.id}`, String(c.id || ""));
    const message = document.createElement("span");
    message.className = "preflight-message";
    message.textContent = c.message || "";
    item.append(name, message);
    list.appendChild(item);
  }
}

// preflightBadge returns the status badge of a preflight check.
function preflightBadge(status) {
  const kind = status === "pass" ? "success" : status === "fail" ? "danger" : "warn";
  return statusBadge(kind, t(`restore_checks.status_${status}`, String(status || "")));
}

// "Restore users and roles" is offered only for in-place restores into the backup's
// own database from a backup taken with them (the server refuses anything else); the
// hint says why it is disabled.
function updateRestoreUsersRoles() {
  const box = document.getElementById("restore-users-roles");
  const hint = document.getElementById("restore-users-roles-hint");
  if (!box || !hint) return;
  const backup = backupCache.get(getValue("restore-backup-id")) || {};
  const source = String(backup.database || document.getElementById("restore-source-db").textContent || "");
  const target = getValue("restore-target-db").trim();
  const inPlace = !document.getElementById("restore-safe-clone").checked;
  let key = "modal_restore.users_roles_hint";
  if (!backup.users_and_roles) {
    key = "modal_restore.users_roles_none";
  } else if (target !== "" && target !== source) {
    key = "modal_restore.users_roles_rename";
  }
  box.disabled = !inPlace || key !== "modal_restore.users_roles_hint";
  if (box.disabled) box.checked = false;
  if (key === "modal_restore.users_roles_rename") {
    // The message names the database, so it is set directly (and redone on language changes).
    delete hint.dataset.i18n;
    hint.textContent = tf(key, { db: source });
  } else {
    setI18nText("restore-users-roles-hint", key);
  }
}

// ---------------------------------------------------------------------------
// Restore dialog: selective restore of some collections of the backup
// ---------------------------------------------------------------------------

// Collections of the backup in the restore dialog, read from its archive by the
// server (GET /api/v1/backups/{id}/collections) the first time "Selected
// collections" is chosen. items is null until loaded; manual means the list is
// unavailable and names are typed instead.
const restoreColls = { backupID: "", seq: 0, items: null, loading: false, error: "", warning: "", manual: false, selected: new Set() };

function restoreCollMode() {
  const checked = document.querySelector('input[name="restore-coll-mode"]:checked');
  return checked ? checked.value : "all";
}

// Resets the collections section for backupID (whole database, nothing loaded).
function resetRestoreCollections(backupID) {
  restoreColls.backupID = backupID;
  restoreColls.seq++;
  restoreColls.items = null;
  restoreColls.loading = false;
  restoreColls.error = "";
  restoreColls.warning = "";
  restoreColls.manual = false;
  restoreColls.selected = new Set();
  const all = document.querySelector('input[name="restore-coll-mode"][value="all"]');
  if (all) all.checked = true;
  setValue("restore-colls-search", "");
  setValue("restore-colls-manual", "");
  updateRestoreCollections();
}

// Shows or hides the list for the chosen mode, loading it on first use.
function updateRestoreCollections() {
  const selected = restoreCollMode() === "selected";
  document.getElementById("restore-colls-box").hidden = !selected;
  setI18nText("restore-drop-hint", selected ? "modal_restore.drop_hint_selected" : "modal_restore.drop_hint");
  if (selected && restoreColls.items === null && !restoreColls.loading && !restoreColls.error) {
    loadRestoreCollections();
  }
  renderRestoreCollections();
}

async function loadRestoreCollections() {
  const id = restoreColls.backupID;
  if (!id) return;
  const seq = ++restoreColls.seq;
  restoreColls.loading = true;
  restoreColls.error = "";
  renderRestoreCollections();
  try {
    const json = await apiJSON(`/api/v1/backups/${encodeURIComponent(id)}/collections`);
    if (seq !== restoreColls.seq) return;
    if (!json.success) throw new Error(json.error || "");
    const data = json.data || {};
    // System collections are managed by the server and never offered.
    restoreColls.items = (data.collections || []).filter(c => c && c.name && !String(c.name).startsWith("system."));
    restoreColls.warning = data.source === "record" ? String(data.warning || "") : "";
    restoreColls.manual = restoreColls.items.length === 0;
  } catch (err) {
    if (seq !== restoreColls.seq) return;
    restoreColls.items = [];
    restoreColls.error = truncate(errorSummary(err.message), 300) || "?";
    restoreColls.manual = true;
  } finally {
    if (seq === restoreColls.seq) {
      restoreColls.loading = false;
      renderRestoreCollections();
    }
  }
}

// Items matching the search box.
function visibleRestoreCollections() {
  const query = getValue("restore-colls-search").toLowerCase();
  const items = restoreColls.items || [];
  return query ? items.filter(c => c.name.toLowerCase().includes(query)) : items;
}

// Renders the checkbox list, the count, the status line and the hints. Names come
// from the archive and are set through textContent only.
function renderRestoreCollections() {
  const list = document.getElementById("restore-colls-list");
  const manual = document.getElementById("restore-colls-manual");
  const search = document.getElementById("restore-colls-search");
  if (!list) return;
  const items = restoreColls.items || [];
  const listed = !restoreColls.loading && !restoreColls.manual;
  list.textContent = "";
  search.disabled = !listed;
  document.getElementById("restore-colls-all").disabled = !listed;
  document.getElementById("restore-colls-none").disabled = !listed;
  manual.hidden = restoreColls.loading || !restoreColls.manual;
  // Searching and bulk selection only apply to a listed archive.
  document.getElementById("restore-colls-toolbar").hidden = restoreColls.manual;

  const visible = listed ? visibleRestoreCollections() : [];
  visible.forEach(c => {
    const label = document.createElement("label");
    label.className = "restore-coll";
    const box = document.createElement("input");
    box.type = "checkbox";
    box.value = c.name;
    box.checked = restoreColls.selected.has(c.name);
    const name = document.createElement("span");
    name.className = "restore-coll-name mono";
    name.textContent = c.name;
    name.title = c.name;
    label.append(box, name);
    if (c.type === "view" || c.type === "timeseries") {
      const chip = document.createElement("span");
      chip.className = c.type === "view" ? "chip" : "chip chip-accent";
      chip.textContent = t(c.type === "view" ? "modal_restore.type_view" : "modal_restore.type_timeseries");
      label.appendChild(chip);
    }
    if (c.type === "view" && c.view_on) {
      const on = document.createElement("span");
      on.className = "restore-coll-on";
      on.textContent = tf("modal_restore.view_on", { name: c.view_on });
      label.appendChild(on);
    }
    list.appendChild(label);
  });

  let status = "";
  if (restoreColls.loading) status = t("modal_restore.coll_loading");
  else if (restoreColls.error) status = tf("modal_restore.coll_failed", { error: restoreColls.error });
  else if (restoreColls.manual) status = t("modal_restore.coll_none_listed");
  else if (visible.length === 0) status = t("modal_restore.coll_no_match");
  setText("restore-colls-status", status);
  setText("restore-colls-count", listed ? tf("modal_restore.coll_count", { selected: restoreColls.selected.size, total: items.length }) : "");

  const warning = document.getElementById("restore-colls-warning");
  warning.hidden = !restoreColls.warning || restoreColls.loading;
  warning.textContent = restoreColls.warning ? tf("modal_restore.coll_from_record", { reason: restoreColls.warning }) : "";
  renderRestoreViewHint();
}

// Warns when a selected view's source collection is not selected: the view is
// restored, but reads nothing until its source exists in the target.
function renderRestoreViewHint() {
  const hint = document.getElementById("restore-colls-view-hint");
  const missing = (restoreColls.items || [])
    .filter(c => c.type === "view" && c.view_on && restoreColls.selected.has(c.name) && !restoreColls.selected.has(c.view_on))
    .map(c => `${c.name} → ${c.view_on}`);
  hint.hidden = missing.length === 0 || restoreColls.manual;
  hint.textContent = missing.length ? tf("modal_restore.view_source_hint", { list: missing.join(", ") }) : "";
}

// Checks or unchecks every collection matching the search box.
function setVisibleRestoreCollections(on) {
  visibleRestoreCollections().forEach(c => {
    if (on) restoreColls.selected.add(c.name);
    else restoreColls.selected.delete(c.name);
  });
  renderRestoreCollections();
}

// Returns the selected collection names, or null when "Selected collections" is on
// and none is selected (after focusing the field to fix, unless quiet).
function restoreSelection(quiet) {
  if (restoreCollMode() !== "selected") return [];
  if (restoreColls.loading) {
    if (!quiet) showToast(t("modal_restore.coll_loading"), "info");
    return null;
  }
  const names = restoreColls.manual
    ? parseList(getValue("restore-colls-manual"))
    : (restoreColls.items || []).map(c => c.name).filter(n => restoreColls.selected.has(n));
  if (names.length === 0) {
    if (quiet) return null;
    const focus = restoreColls.manual ? document.getElementById("restore-colls-manual") : document.querySelector("#restore-colls-list input");
    if (focus) focus.focus();
    showToast(t("modal_restore.need_collections"), "error");
    return null;
  }
  return names;
}

function setupRestoreCollections() {
  document.querySelectorAll('input[name="restore-coll-mode"]').forEach(radio => {
    radio.addEventListener("change", updateRestoreCollections);
  });
  document.getElementById("restore-colls-search").addEventListener("input", renderRestoreCollections);
  // Rebuilt on language changes only, so periodic refreshes never reset the list.
  onLanguageChange(() => renderRestoreCollections());
  document.getElementById("restore-colls-all").addEventListener("click", () => setVisibleRestoreCollections(true));
  document.getElementById("restore-colls-none").addEventListener("click", () => setVisibleRestoreCollections(false));
  document.getElementById("restore-colls-list").addEventListener("change", (e) => {
    const box = e.target;
    if (!box || box.type !== "checkbox") return;
    if (box.checked) restoreColls.selected.add(box.value);
    else restoreColls.selected.delete(box.value);
    setText("restore-colls-count", tf("modal_restore.coll_count", { selected: restoreColls.selected.size, total: (restoreColls.items || []).length }));
    renderRestoreViewHint();
  });
}

async function triggerJob(jobID, btn) {
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}/run`, { method: "POST" });
    if (json.success) {
      showToast(t("toasts.job_started"), "info");
      // A multi-database job answers with its run (its backups start in the
      // background and show up in the active runs); a single one with its backup.
      const isRun = json.data && Array.isArray(json.data.databases) && !json.data.storage_key;
      if (isRun) {
        if (typeof jobDbsLoadRuns === "function" && detailsJobId === jobID) jobDbsLoadRuns(jobID);
        scheduleActivePoll();
      } else {
        trackBackup(json.data);
      }
    } else {
      showToast(json.error || t("toasts.job_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    if (btn) btn.disabled = false;
    refreshAll();
  }
}

async function deleteJob(jobID) {
  const job = state.jobs.find(j => j.id === jobID);
  if (!(await confirmDialog({
    title: t("dialog.delete_title"),
    body: [t("toasts.confirm_delete_job"), job ? `${job.name || job.id} · ${job.database}` : jobID],
    danger: true,
    confirmLabel: t("actions.delete")
  }))) return;
  try {
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}`, { method: "DELETE" });
    if (json.success) {
      showToast(t("toasts.job_deleted"), "success");
      bulkForget("jobs", jobID);
      refreshAll();
    } else {
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

async function deleteBackup(backupID) {
  const b = backupCache.get(backupID);
  const started = b ? parseDate(b.started_at) : null;
  const facts = b ? [b.database, formatBytes(b.size_bytes), started ? formatAbsolute(started) : ""].filter(Boolean).join(" · ") : "";
  // Deletes are soft: the confirmation says until when the archive stays (protection.js).
  const soft = typeof protectionDeleteConfirm === "function";
  if (!(await confirmDialog({
    title: soft ? t("protection.delete_title") : t("dialog.delete_title"),
    body: [soft ? protectionDeleteConfirm() : t("toasts.confirm_delete_backup"), backupID, facts],
    danger: true,
    undoNote: soft ? protectionUndoNote() : "",
    confirmLabel: t("actions.delete")
  }))) return;
  try {
    const json = await apiJSON(`/api/v1/backups/${encodeURIComponent(backupID)}`, { method: "DELETE" });
    if (json.success && typeof protectionApprovalPending === "function" && protectionApprovalPending(json)) {
      return;
    }
    if (json.success) {
      if (soft) protectionDeletedToast(json.data);
      else showToast(t("toasts.backup_deleted"), "success");
      backupCache.delete(backupID);
      bulkForget("backups", backupID);
      if (detailsBackupId === backupID) closeModal("modal-backup-details");
      refreshAll();
    } else {
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

// Starts a new backup with the parameters of failed backup backupID. The failed
// record stays in the list; the new one links to it through retry_of.
async function retryBackup(backupID) {
  if (!backupID || retryingBackups.has(backupID)) return;
  retryingBackups.add(backupID);
  setRetryButtonsDisabled(backupID, true);
  try {
    const json = await apiJSON(`/api/v1/backups/${encodeURIComponent(backupID)}/retry`, { method: "POST" });
    if (json.success) {
      showToast(t("toasts.retry_started"), "info");
      trackBackup(json.data);
    } else {
      showToast(json.error || t("toasts.retry_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    retryingBackups.delete(backupID);
    setRetryButtonsDisabled(backupID, false);
    refreshAll();
  }
}

function setRetryButtonsDisabled(backupID, disabled) {
  document.querySelectorAll('[data-action="retry-backup"]').forEach(btn => {
    if (btn.dataset.id === backupID) btn.disabled = disabled;
  });
}

function openBackupDetails(backupID) {
  if (!backupCache.has(backupID)) return;
  detailsBackupId = backupID;
  if (backupChain.id !== backupID) {
    backupChain.id = backupID;
    backupChain.records = null;
  }
  renderBackupDetails();
  openModal("modal-backup-details");
  loadBackupChain(backupID);
  // Rendering needs the dialog open (it is skipped while closed).
  renderBackupDetails();
}

// The retry chain of backup b, oldest first, from the latest records seen: the chain
// fetched for the dialog, or b alone until it has loaded.
function backupRetryChain(b) {
  const ids = backupChain.id === b.id && backupChain.records ? backupChain.records : [b.id];
  return ids
    .map(id => (id === b.id ? b : backupCache.get(id)))
    .filter(Boolean)
    .sort((x, y) => String(x.started_at || "").localeCompare(String(y.started_at || "")));
}

// Fills the details dialog of detailsBackupId. Everything the server sent is set
// through textContent; error messages are already redacted by the server.
function renderBackupDetails() {
  const modal = document.getElementById("modal-backup-details");
  if (!modal || !modal.classList.contains("open") || !detailsBackupId) return;
  const b = backupCache.get(detailsBackupId);
  const list = document.getElementById("backup-details-list");
  const errorBox = document.getElementById("backup-details-error");
  const chainList = document.getElementById("backup-details-chain");
  const retryBtn = document.getElementById("backup-details-retry");
  if (!b) {
    closeModal("modal-backup-details");
    return;
  }

  list.textContent = "";
  const row = (label, value, mono) => appendKv(list, label, value, mono);
  const absolute = absoluteWithRelative;
  const failed = b.status === "failed";
  const [statusKind, statusLabel] = backupStatus(b.status);
  const secs = Number(b.duration_seconds);
  const conn = b.connection_name || connectionName(b.connection_id);
  row(t("backup_details.id"), htmlNode(idCopy(b.id), "div").firstElementChild || b.id);
  row(t("backup_details.status"), htmlNode(statusBadge(statusKind, statusLabel)));
  row(t("backup_details.trigger"), backupTrigger(b));
  if (b.job_id && state.jobs.some(j => j.id === b.job_id)) {
    // The job links back to its details (the dialog underneath, when opened from there).
    const link = document.createElement("button");
    link.type = "button";
    link.className = "link-btn";
    link.dataset.action = "job-details";
    link.dataset.id = b.job_id;
    link.textContent = jobLabel(b.job_id);
    row(t("backup_details.job"), link);
  } else if (b.job_id) {
    row(t("backup_details.job"), jobLabel(b.job_id));
  }
  row(t("backup_details.connection"), conn);
  row(t("backup_details.database"), b.database, true);
  row(t("backup_details.collections"), (b.collections || []).length > 0 ? b.collections.join(", ") : t("backup_details.all_collections"));
  row(t("backup_details.target"), backupStorageName(b) || b.storage_type);
  row(t("backup_details.started"), absolute(b.started_at));
  row(failed ? t("backup_details.failed_at") : t("backup_details.finished"), absolute(b.completed_at));
  row(t("backup_details.duration"), secs > 0 ? formatDuration(secs) : "");
  if (b.status === "completed") {
    row(t("tables.size"), formatBytes(b.size_bytes));
    if (b.sha256) row(t("tables.sha256"), b.sha256, true);
  }
  backupCancelRows(b, row);
  // Verification, pin, restore test and import (trust.js).
  trustBackupDetailRows(b, row);
  // When and by whom it was deleted, and until when it can be undone (protection.js).
  if (typeof protectionBackupDetailRows === "function") protectionBackupDetailRows(b, row);
  // Progress, cancel button, phase timeline and log viewer (runs.js).
  renderRunPanel("backup", "backup", b);

  errorBox.hidden = !b.error_message;
  document.getElementById("backup-details-error-text").textContent = b.error_message || "";

  chainList.textContent = "";
  const chain = backupRetryChain(b);
  if (b.retry_of && !chain.some(x => x.id === b.retry_of)) {
    // The first attempt was deleted; keep its ID visible.
    const li = document.createElement("li");
    li.className = "mono muted";
    li.textContent = b.retry_of;
    chainList.appendChild(li);
  }
  chain.forEach(x => {
    const li = document.createElement("li");
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "link-btn mono";
    btn.dataset.action = "backup-details";
    btn.dataset.id = x.id;
    btn.textContent = shortBackupId(x.id);
    btn.title = x.id;
    if (x.id === b.id) btn.setAttribute("aria-current", "true");
    const [, label] = backupStatus(x.status);
    const d = parseDate(x.started_at);
    const meta = document.createElement("span");
    meta.className = "muted";
    meta.textContent = ` · ${label}${d ? ` · ${formatAbsolute(d)}` : ""}`;
    li.append(btn, meta);
    chainList.appendChild(li);
  });
  document.getElementById("backup-details-chain-group").hidden = chainList.children.length < 2;

  retryBtn.hidden = !(failed && !b.retried_by && !chain.some(x => x.retry_of === b.id));
  retryBtn.dataset.id = b.id;
  retryBtn.disabled = retryingBackups.has(b.id);
}

// ---------------------------------------------------------------------------
// Notifications: channels & rule workflows
// ---------------------------------------------------------------------------

function showChannelFields(type) {
  document.querySelectorAll("#form-channel .channel-fields").forEach(el => {
    el.hidden = el.dataset.channelType !== type;
  });
}

function setValue(id, value) {
  const el = document.getElementById(id);
  if (el) el.value = value === undefined || value === null ? "" : String(value);
}

function getValue(id) {
  const el = document.getElementById(id);
  return el ? el.value.trim() : "";
}

function parseList(text) {
  return String(text || "").split(/[,;\n]/).map(s => s.trim()).filter(Boolean);
}

function parseHeaders(text) {
  const headers = {};
  String(text || "").split("\n").forEach(line => {
    const trimmed = line.trim();
    if (!trimmed) return;
    const idx = trimmed.indexOf(":");
    if (idx <= 0) {
      throw new Error(`${t("notify.invalid_header")} ${trimmed}`);
    }
    headers[trimmed.substring(0, idx).trim()] = trimmed.substring(idx + 1).trim();
  });
  return headers;
}

function openChannelModal(id) {
  const ch = id ? state.channels.find(c => c.id === id) : null;
  if (id && !ch) return;
  const form = document.getElementById("form-channel");
  if (form) form.reset();

  const type = ch ? ch.type : "webhook";
  setValue("channel-id", ch ? ch.id : "");
  setValue("channel-name", ch ? ch.name : "");
  setValue("channel-type", type);
  document.getElementById("channel-enabled").checked = ch ? !!ch.enabled : true;

  const w = (ch && ch.webhook) || {};
  setValue("webhook-url", w.url);
  setValue("webhook-headers", Object.entries(w.headers || {}).map(([k, v]) => `${k}: ${v}`).join("\n"));
  setValue("webhook-secret", w.secret);

  const tg = (ch && ch.telegram) || {};
  setValue("telegram-token", tg.bot_token);
  setValue("telegram-chat", tg.chat_id);
  setValue("telegram-parse-mode", tg.parse_mode || "");

  const em = (ch && ch.email) || {};
  setValue("email-host", em.host);
  setValue("email-port", em.port || 587);
  setValue("email-security", em.security || "starttls");
  setValue("email-username", em.username);
  setValue("email-password", em.password);
  setValue("email-from", em.from);
  setValue("email-to", (em.to || []).join(", "));

  const tw = (ch && ch.twilio) || {};
  setValue("twilio-sid", tw.account_sid);
  setValue("twilio-token", tw.auth_token);
  setValue("twilio-from", tw.from);
  setValue("twilio-to", (tw.to || []).join(", "));

  setText("channel-modal-title", ch ? t("notify.modal_channel_edit") : t("notify.modal_channel_title"));
  showChannelFields(type);
  openModal("modal-channel");
}

function buildChannelPayload() {
  const type = getValue("channel-type");
  const payload = {
    name: getValue("channel-name"),
    type,
    enabled: document.getElementById("channel-enabled").checked
  };
  switch (type) {
    case "webhook":
      payload.webhook = {
        url: getValue("webhook-url"),
        headers: parseHeaders(document.getElementById("webhook-headers").value),
        secret: getValue("webhook-secret")
      };
      break;
    case "telegram":
      payload.telegram = {
        bot_token: getValue("telegram-token"),
        chat_id: getValue("telegram-chat"),
        parse_mode: getValue("telegram-parse-mode")
      };
      break;
    case "email":
      payload.email = {
        host: getValue("email-host"),
        port: parseInt(getValue("email-port"), 10) || 0,
        security: getValue("email-security"),
        username: getValue("email-username"),
        password: document.getElementById("email-password").value,
        from: getValue("email-from"),
        to: parseList(getValue("email-to"))
      };
      break;
    case "twilio":
      payload.twilio = {
        account_sid: getValue("twilio-sid"),
        auth_token: getValue("twilio-token"),
        from: getValue("twilio-from"),
        to: parseList(getValue("twilio-to"))
      };
      break;
  }
  return payload;
}

async function saveChannel(e) {
  e.preventDefault();
  let payload;
  try {
    payload = buildChannelPayload();
  } catch (err) {
    showToast(err.message, "error");
    return;
  }
  const id = getValue("channel-id");
  const url = id ? `/api/v1/notifications/channels/${encodeURIComponent(id)}` : "/api/v1/notifications/channels";
  try {
    const json = await apiJSON(url, {
      method: id ? "PUT" : "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    });
    if (json.success) {
      showToast(t("notify.toast_channel_saved"), "success");
      closeModal("modal-channel");
      loadNotifications();
    } else {
      showToast(json.error || t("notify.toast_save_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

async function testChannel(id, btn) {
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON(`/api/v1/notifications/channels/${encodeURIComponent(id)}/test`, { method: "POST" });
    if (json.success) {
      showToast(t("notify.toast_test_sent"), "success");
    } else {
      showToast(`${t("notify.toast_test_failed")} ${truncate(json.error || "", 200)}`, "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    if (btn) btn.disabled = false;
    loadNotifications();
  }
}

async function deleteChannel(id) {
  if (!(await confirmDialog({ title: t("dialog.delete_title"), body: t("notify.confirm_delete_channel"), danger: true, confirmLabel: t("actions.delete") }))) return;
  try {
    const json = await apiJSON(`/api/v1/notifications/channels/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (json.success) {
      showToast(t("notify.toast_channel_deleted"), "success");
      loadNotifications();
    } else {
      showToast(json.error || t("notify.toast_save_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

function fillMultiSelect(selectId, items, selected) {
  const select = document.getElementById(selectId);
  if (!select) return;
  select.textContent = "";
  const known = new Set(items.map(it => it.id));
  // Keep references to items that no longer exist so saving does not silently drop them.
  selected.filter(id => !known.has(id)).forEach(id => items.push({ id, label: id }));
  items.forEach(it => {
    const opt = document.createElement("option");
    opt.value = it.id;
    opt.textContent = it.label;
    opt.selected = selected.includes(it.id);
    select.appendChild(opt);
  });
}

function openRuleModal(id) {
  const rule = id ? state.rules.find(r => r.id === id) : null;
  if (id && !rule) return;
  const form = document.getElementById("form-rule");
  if (form) form.reset();

  setValue("rule-id", rule ? rule.id : "");
  setValue("rule-name", rule ? rule.name : "");
  document.getElementById("rule-enabled").checked = rule ? !!rule.enabled : true;

  const ruleEvents = rule ? (rule.events || []) : ["backup.failed"];
  document.querySelectorAll('#form-rule input[name="rule-event"]').forEach(cb => {
    cb.checked = ruleEvents.includes(cb.value);
  });

  fillMultiSelect("rule-jobs",
    state.jobs.map(j => ({ id: j.id, label: `${j.name || j.id} (${j.database})` })),
    rule ? (rule.job_ids || []) : []);
  fillMultiSelect("rule-channels",
    state.channels.map(c => ({ id: c.id, label: `${c.name} — ${channelTypeLabel(c.type)}` })),
    rule ? (rule.channel_ids || []) : []);

  setText("rule-modal-title", rule ? t("notify.modal_rule_edit") : t("notify.modal_rule_title"));
  openModal("modal-rule");
}

function selectedValues(selectId) {
  const select = document.getElementById(selectId);
  return select ? Array.from(select.selectedOptions).map(o => o.value) : [];
}

async function saveRule(e) {
  e.preventDefault();
  const payload = {
    name: getValue("rule-name"),
    enabled: document.getElementById("rule-enabled").checked,
    events: Array.from(document.querySelectorAll('#form-rule input[name="rule-event"]:checked')).map(cb => cb.value),
    job_ids: selectedValues("rule-jobs"),
    channel_ids: selectedValues("rule-channels")
  };
  if (payload.events.length === 0) {
    showToast(t("notify.need_event"), "error");
    return;
  }
  if (payload.channel_ids.length === 0) {
    showToast(t("notify.need_channel"), "error");
    return;
  }

  const id = getValue("rule-id");
  const url = id ? `/api/v1/notifications/rules/${encodeURIComponent(id)}` : "/api/v1/notifications/rules";
  try {
    const json = await apiJSON(url, {
      method: id ? "PUT" : "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    });
    if (json.success) {
      showToast(t("notify.toast_rule_saved"), "success");
      closeModal("modal-rule");
      loadNotifications();
    } else {
      showToast(json.error || t("notify.toast_save_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

async function deleteRule(id) {
  if (!(await confirmDialog({ title: t("dialog.delete_title"), body: t("notify.confirm_delete_rule"), danger: true, confirmLabel: t("actions.delete") }))) return;
  try {
    const json = await apiJSON(`/api/v1/notifications/rules/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (json.success) {
      showToast(t("notify.toast_rule_deleted"), "success");
      loadNotifications();
    } else {
      showToast(json.error || t("notify.toast_save_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

// ---------------------------------------------------------------------------
// Session lifecycle: setup, sign-in, sign-out, user menu
// ---------------------------------------------------------------------------

async function boot() {
  // API keys are no longer kept in the browser; drop any key an older version stored.
  storageSet(LEGACY_API_KEY_STORAGE, "");
  storageSet(LEGACY_SIGNED_IN_STORAGE, "");
  checkHealth();

  let setupRequired = false;
  try {
    const res = await fetch("/api/v1/setup/status", { credentials: "same-origin" });
    const json = await readJSON(res);
    setupRequired = !!(json && json.success && json.data && json.data.setup_required);
  } catch (err) {
    // Status unknown: fall through to the sign-in check, which reports errors itself.
  }

  if (setupRequired) {
    showSetup();
  } else if (await loadMe()) {
    enterApp();
  } else {
    showLogin("");
  }
}

// Reads a JSON envelope without assuming the body is JSON (proxies, HTML pages).
async function readJSON(res) {
  const type = res.headers.get("content-type") || "";
  if (!type.includes("application/json")) return null;
  try {
    return await res.json();
  } catch (err) {
    return null;
  }
}

// Reads the sign-in state. Signed-out visitors get 200 with user: null; older
// servers answer 401 instead. Both mean "not signed in".
async function loadMe() {
  try {
    const res = await fetch("/api/v1/auth/me", { credentials: "same-origin" });
    if (!res.ok) return false;
    const json = await readJSON(res);
    if (!json || !json.success || !json.data || !json.data.user) return false;
    setAuth(json.data);
    return true;
  } catch (err) {
    return false;
  }
}

function setAuth(data) {
  auth.user = data.user || null;
  auth.csrf = data.csrf_token || "";
  auth.mode = data.auth || "session";
  // Sign-in and setup answers carry the user (with its role); /auth/me also the
  // effective scope. An unknown role gets read only, like on the server.
  auth.role = data.role || (auth.user && auth.user.role) || "";
  auth.scope = data.scope || ROLE_SCOPES[auth.role] || "read";
  auth.keyScope = data.key_scope || "";
  renderUserMenu();
  if (typeof applyRole === "function") applyRole();
}

function clearAuth() {
  auth.user = null;
  auth.csrf = "";
  auth.mode = "";
  auth.role = "";
  auth.scope = "";
  auth.keyScope = "";
  if (typeof applyRole === "function") applyRole();
}

function showScreen(name) {
  document.body.classList.remove("is-booting");
  document.getElementById("auth-screen").hidden = name !== "auth";
  document.getElementById("app-main").hidden = name !== "app";
  document.getElementById("user-menu").hidden = name !== "app";
  if (name !== "app") toggleUserMenu(false);
}

function showSetup() {
  stopPolling();
  showScreen("auth");
  document.getElementById("setup-card").hidden = false;
  document.getElementById("login-card").hidden = true;
  hideFormError("setup-error");
  document.getElementById("setup-code").focus();
}

function showLogin(notice) {
  stopPolling();
  closeAllModals();
  resetData();
  showScreen("auth");
  document.getElementById("setup-card").hidden = true;
  document.getElementById("login-card").hidden = false;
  const noticeEl = document.getElementById("login-notice");
  noticeEl.textContent = notice || "";
  noticeEl.hidden = !notice;
  hideFormError("login-error");
  setValue("login-password", "");
  const username = document.getElementById("login-username");
  if (username.value) {
    document.getElementById("login-password").focus();
  } else {
    username.focus();
  }
  // The single sign-on button and a failed sign-in (sso.js).
  if (typeof ssoRenderLogin === "function") ssoRenderLogin();
}

function enterApp() {
  sessionExpiredShown = false;
  showScreen("app");
  refreshAll();
  startPolling();
}

function startPolling() {
  stopPolling();
  pollTimer = setInterval(() => {
    if (!document.hidden && auth.user) refreshAll();
  }, POLL_INTERVAL_MS);
}

function stopPolling() {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = null;
  if (activePollTimer) clearTimeout(activePollTimer);
  activePollTimer = null;
}

// Called for any 401 from an authenticated request: the session is gone.
function handleUnauthorized() {
  if (sessionExpiredShown) return;
  sessionExpiredShown = true;
  const wasSignedIn = !!auth.user;
  clearAuth();
  showLogin(wasSignedIn ? t("auth.session_expired") : "");
}

// Forgets everything loaded for the previous session.
function resetData() {
  state.stats = null;
  ["jobs", "backups", "restores", "channels", "rules", "connections", "users", "apikeys", "storageTargets"].forEach(k => {
    state[k] = [];
    state.loaded[k] = false;
  });
  state.settings = null;
  state.settingsError = "";
  state.loaded.settings = false;
  loadErrors.clear();
  renderLoadErrors();
  renderCorruptRecords();
  resetSettingsForms();
  trackedOps.backups.clear();
  trackedOps.restores.clear();
  backupCache.clear();
  missingBackups.clear();
  jobHistory.jobId = "";
  jobHistory.runs = null;
  backupChain.id = "";
  backupChain.records = null;
  bulkReset();
  LIST_KINDS.forEach(kind => {
    lists[kind].total = 0;
    lists[kind].error = "";
    lists[kind].databases = [];
  });
  document.querySelectorAll("#app-main tbody").forEach(tbody => {
    const cols = tbody.closest("table").querySelectorAll("thead th").length || 1;
    renderedTbodies.delete(tbody);
    tbody.innerHTML = typeof tablesSkeletonFor === "function"
      ? tablesSkeletonFor(tbody)
      : `<tr class="empty-row"><td colspan="${cols}"><div class="empty-state"><p>${escapeHtml(t("tables.loading"))}</p></div></td></tr>`;
  });
  if (typeof tablesReset === "function") tablesReset();
}

function closeAllModals() {
  modalStack.slice().reverse().forEach(m => closeModal(m.id));
}

function validateNewPassword(password, confirmation) {
  if (password.length < MIN_PASSWORD_LENGTH) return t("auth.err_password_short");
  if (new TextEncoder().encode(password).length > MAX_PASSWORD_BYTES) return t("auth.err_password_long");
  if (password !== confirmation) return t("auth.err_password_mismatch");
  return "";
}

function showFormError(id, message) {
  const el = document.getElementById(id);
  if (!el) return;
  el.textContent = message;
  el.hidden = !message;
}

function hideFormError(id) {
  showFormError(id, "");
}

// Maps a failed public auth request (setup / login) to a user-facing message.
function authErrorMessage(res, json, fallbackKey) {
  if (res.status === 429) return t("auth.err_rate_limited");
  if (res.status === 503 && json && /shutting down/i.test(String(json.error || ""))) return t("toasts.shutting_down");
  if (json && json.error) return json.error;
  if (!json) return tf("toasts.unexpected_response", { status: res.status });
  return t(fallbackKey);
}

async function postPublic(url, body) {
  const res = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body)
  });
  return { res, json: await readJSON(res) };
}

async function submitSetup(e) {
  e.preventDefault();
  const code = getValue("setup-code");
  const username = getValue("setup-username");
  const password = document.getElementById("setup-password").value;
  const confirmation = document.getElementById("setup-password-confirm").value;

  let error = "";
  if (!code) error = t("auth.err_code_required");
  else if (!username) error = t("auth.err_username_required");
  else error = validateNewPassword(password, confirmation);
  if (error) {
    showFormError("setup-error", error);
    return;
  }

  const submit = e.submitter || document.querySelector("#form-setup [type=submit]");
  if (submit) submit.disabled = true;
  try {
    const { res, json } = await postPublic("/api/v1/setup", { setup_code: code, username, password });
    if (res.ok && json && json.success) {
      setValue("setup-password", "");
      setValue("setup-password-confirm", "");
      setValue("setup-code", "");
      if (json.data && json.data.user && json.data.csrf_token) {
        setAuth(json.data);
      } else if (!(await loadMe())) {
        showLogin("");
        return;
      }
      enterApp();
      showToast(t("auth.setup_done"), "success");
    } else if (res.status === 409) {
      setValue("login-username", username);
      showLogin(t("auth.err_setup_done"));
    } else {
      showFormError("setup-error", authErrorMessage(res, json, "auth.err_setup_failed"));
    }
  } catch (err) {
    showFormError("setup-error", err.message);
  } finally {
    if (submit) submit.disabled = false;
  }
}

async function submitLogin(e) {
  e.preventDefault();
  const username = getValue("login-username");
  const password = document.getElementById("login-password").value;
  if (!username || !password) {
    showFormError("login-error", t("auth.err_login_required"));
    return;
  }

  const submit = e.submitter || document.querySelector("#form-login [type=submit]");
  if (submit) submit.disabled = true;
  try {
    const { res, json } = await postPublic("/api/v1/auth/login", { username, password });
    if (res.ok && json && json.success) {
      setValue("login-password", "");
      if (json.data && json.data.user && json.data.csrf_token) {
        setAuth(json.data);
      } else if (!(await loadMe())) {
        showFormError("login-error", t("auth.err_login"));
        return;
      }
      enterApp();
    } else if (res.status === 401) {
      showFormError("login-error", t("auth.err_login"));
      setValue("login-password", "");
      document.getElementById("login-password").focus();
    } else {
      showFormError("login-error", authErrorMessage(res, json, "auth.err_login"));
    }
  } catch (err) {
    showFormError("login-error", err.message);
  } finally {
    if (submit) submit.disabled = false;
  }
}

async function logout() {
  toggleUserMenu(false);
  let endSession = "";
  try {
    const res = await apiFetch("/api/v1/auth/logout", { method: "POST" });
    const json = await readJSON(res);
    // Single sign-on users may also be signed out at the identity provider.
    const url = json && json.data ? json.data.end_session_url : "";
    if (typeof url === "string" && /^https?:\/\//i.test(url)) endSession = url;
  } catch (err) {
    // The session is discarded locally either way.
  }
  sessionExpiredShown = true;
  clearAuth();
  if (endSession) {
    window.location.assign(endSession);
    return;
  }
  showLogin(t("auth.signed_out"));
}

function renderUserMenu() {
  const name = auth.user ? auth.user.username || "" : "";
  setText("user-menu-name", name);
  setText("user-menu-fullname", name);
  // Single sign-on users have no password to change.
  const pw = document.querySelector("#user-menu-list [data-action='change-own-password']");
  if (pw) pw.hidden = !!(auth.user && auth.user.auth_provider === "oidc");
}

function toggleUserMenu(force) {
  const btn = document.getElementById("user-menu-btn");
  const menu = document.getElementById("user-menu-list");
  if (!btn || !menu) return;
  const open = typeof force === "boolean" ? force : menu.hidden;
  menu.hidden = !open;
  btn.setAttribute("aria-expanded", String(open));
  if (open) {
    const first = menu.querySelector(".menu-item");
    if (first) first.focus();
  }
}

function setupUserMenu() {
  const wrapper = document.getElementById("user-menu");
  const menu = document.getElementById("user-menu-list");
  document.addEventListener("click", (e) => {
    if (!menu.hidden && !wrapper.contains(e.target)) toggleUserMenu(false);
  });
  wrapper.addEventListener("keydown", (e) => {
    if (menu.hidden) return;
    const items = Array.from(menu.querySelectorAll(".menu-item"));
    const idx = items.indexOf(document.activeElement);
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      toggleUserMenu(false);
      document.getElementById("user-menu-btn").focus();
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const next = e.key === "ArrowDown" ? (idx + 1) % items.length : (idx - 1 + items.length) % items.length;
      items[next].focus();
    } else if (e.key === "Tab") {
      toggleUserMenu(false);
    }
  });
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

async function loadConnections() {
  try {
    const json = await apiJSON("/api/v1/connections");
    if (!noteLoad("connections", json)) return;
    state.connections = json.data || [];
    state.loaded.connections = true;
    renderConnections();
    updateConnectionGating();
  } catch (err) {
    console.error("Failed to load connections:", err);
  }
}

// Host list of a (redacted) MongoDB URI, without scheme, credentials, path or options.
function maskedHost(uri) {
  const s = String(uri || "");
  const m = s.match(/^mongodb(\+srv)?:\/\/(?:[^@/]*@)?([^/?#]+)/i);
  if (!m) return s;
  return m[1] ? `${m[2]} (SRV)` : m[2].split(",").join(", ");
}

function connectionName(id) {
  if (!id) return "";
  const c = state.connections.find(x => x.id === id);
  return c ? c.name : id;
}

function connectionTestBadge(c) {
  if (!parseDate(c.last_test_at)) return statusBadge("neutral", t("conn.untested"));
  return c.last_test_ok
    ? statusBadge("success", t("conn.reachable"))
    : statusBadge("danger", t("status.failed"), c.last_test_error);
}

function renderConnections() {
  const tbody = document.getElementById("connections-tbody");
  if (!tbody || !state.loaded.connections) return;

  if (state.connections.length === 0) {
    setTbody(tbody, emptyRow(6, t("conn.empty"), "new-connection", "plus", t("conn.add")));
    return;
  }

  const connections = typeof tablesRows === "function" ? tablesRows("connections", state.connections) : state.connections;
  if (connections.length === 0) {
    setTbody(tbody, tablesNoMatch("connections"));
    return;
  }

  setTbody(tbody, connections.map(c => {
    const errorLine = parseDate(c.last_test_at) && !c.last_test_ok && c.last_test_error
      ? errorDetail(c.last_test_error, truncate(errorSummary(c.last_test_error), 70))
      : "";
    const desc = c.description ? `<div class="cell-sub">${ellipsis(c.description)}</div>` : "";
    return `<tr>
      <td class="cell-primary">${ellipsis(c.name)}${desc}${idCopy(c.id, "cell-sub")}</td>
      <td><span class="mono host-cell" title="${escapeHtml(c.uri || "")}">${escapeHtml(maskedHost(c.uri))}</span></td>
      <td>${connectionTestBadge(c)}${errorLine}</td>
      <td>${c.server_version ? `<span class="mono">${escapeHtml(c.server_version)}</span>` : mutedDash()}</td>
      <td>${timeCell(c.last_test_at)}</td>
      <td class="col-actions"><div class="row-actions">
        <button type="button" class="btn btn-secondary btn-sm" data-action="test-connection" data-id="${escapeHtml(c.id)}">${escapeHtml(t("conn.test_short"))}</button>
        <button type="button" class="btn btn-secondary btn-sm" data-action="edit-connection" data-id="${escapeHtml(c.id)}">${escapeHtml(t("notify.edit"))}</button>
        <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="delete-connection" data-id="${escapeHtml(c.id)}">${escapeHtml(t("actions.delete"))}</button>
      </div></td>
    </tr>`;
  }).join(""));
}

// Without any connection nothing can be backed up: surface the first-run call to
// action and disable the actions that need a connection.
function updateConnectionGating() {
  const none = state.loaded.connections && state.connections.length === 0;
  const onboarding = document.getElementById("onboarding");
  if (onboarding) onboarding.hidden = !none;
  document.querySelectorAll("[data-needs-connection]").forEach(btn => {
    btn.disabled = none;
    btn.title = none ? t("conn.add_first") : "";
  });
  setText("count-connections", state.loaded.connections ? String(state.connections.length) : "");
}

function openConnectionModal(id) {
  const c = id ? state.connections.find(x => x.id === id) : null;
  if (id && !c) return;
  const form = document.getElementById("form-connection");
  if (form) form.reset();
  setValue("connection-id", c ? c.id : "");
  setValue("connection-name", c ? c.name : "");
  setValue("connection-uri", c ? c.uri : "");
  setValue("connection-description", c ? c.description : "");
  setText("connection-modal-title", c ? t("conn.modal_edit") : t("conn.modal_new"));
  resetConnectionTest();
  // Paste URI or Build, as last used (forms.js).
  if (typeof uriBuilderReset === "function") uriBuilderReset();
  openModal("modal-connection");
}

function resetConnectionTest() {
  connTest.uri = null;
  connTest.ok = false;
  connTest.saveAnyway = false;
  const result = document.getElementById("connection-test-result");
  result.textContent = "";
  result.className = "test-result";
  setText("connection-submit", t("notify.save"));
}

function showConnectionTestResult(kind, text) {
  const result = document.getElementById("connection-test-result");
  result.className = `test-result test-${kind}`;
  result.innerHTML = kind === "pending" ? escapeHtml(text) : `${statusGlyph(kind === "ok" ? "success" : "danger")}<span>${escapeHtml(text)}</span>`;
}

function describeTest(data) {
  if (data && data.ok) {
    return tf("conn.test_ok", { version: data.server_version || "?", ms: Math.round(Number(data.latency_ms) || 0) });
  }
  return tf("conn.test_failed", { error: truncate(errorSummary(data && data.error ? data.error : ""), 160) });
}

// Tests the URI in the form. An unchanged (redacted) URI of a saved connection is
// tested server-side with the stored secret; anything else via /connections/test.
async function testConnectionForm() {
  const id = getValue("connection-id");
  const uri = getValue("connection-uri");
  if (!uri) {
    (typeof uriFocusTarget === "function" ? uriFocusTarget() : document.getElementById("connection-uri")).focus();
    return false;
  }
  const existing = id ? state.connections.find(c => c.id === id) : null;
  const useStored = existing && existing.uri === uri;
  const btn = document.getElementById("connection-test-btn");
  btn.disabled = true;
  showConnectionTestResult("pending", t("conn.testing"));
  let data;
  try {
    const json = useStored
      ? await apiJSON(`/api/v1/connections/${encodeURIComponent(id)}/test`, { method: "POST" })
      : await apiJSON("/api/v1/connections/test", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ uri })
      });
    data = json.success ? (json.data || {}) : { ok: false, error: json.error || "" };
  } catch (err) {
    data = { ok: false, error: err.message };
  } finally {
    btn.disabled = false;
  }
  connTest.uri = uri;
  connTest.ok = !!data.ok;
  showConnectionTestResult(data.ok ? "ok" : "fail", describeTest(data));
  if (useStored) loadConnections();
  return connTest.ok;
}

async function saveConnection(e) {
  e.preventDefault();
  const id = getValue("connection-id");
  const uri = getValue("connection-uri");
  const payload = {
    name: getValue("connection-name"),
    uri,
    description: getValue("connection-description")
  };

  // Test before saving; a failing test must be overridden explicitly.
  if (!connTest.saveAnyway) {
    const ok = connTest.uri === uri ? connTest.ok : await testConnectionForm();
    if (!ok) {
      connTest.saveAnyway = true;
      setText("connection-submit", t("conn.save_anyway"));
      showToast(t("conn.test_before_save"), "error");
      return;
    }
  }

  const url = id ? `/api/v1/connections/${encodeURIComponent(id)}` : "/api/v1/connections";
  try {
    const json = await apiJSON(url, {
      method: id ? "PUT" : "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    });
    if (json.success) {
      showToast(t("conn.saved"), "success");
      closeModal("modal-connection");
      await loadConnections();
    } else {
      showToast(json.error || t("notify.toast_save_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

async function testConnection(id, btn) {
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON(`/api/v1/connections/${encodeURIComponent(id)}/test`, { method: "POST" });
    const data = json.success ? (json.data || {}) : { ok: false, error: json.error || "" };
    showToast(`${connectionName(id)}: ${describeTest(data)}`, data.ok ? "success" : "error");
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    if (btn) btn.disabled = false;
    loadConnections();
  }
}

async function deleteConnection(id) {
  if (!(await confirmDialog({ title: t("dialog.delete_title"), body: tf("conn.confirm_delete", { name: connectionName(id) }), danger: true, confirmLabel: t("actions.delete") }))) return;
  try {
    const json = await apiJSON(`/api/v1/connections/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (json.success) {
      showToast(t("conn.deleted"), "success");
      loadConnections();
    } else {
      // 409: jobs still reference the connection; the server names them.
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

function fillConnectionSelect(select, selectedId, extraFirst) {
  select.textContent = "";
  if (extraFirst) select.appendChild(extraFirst);
  if (state.connections.length === 0) {
    const opt = document.createElement("option");
    opt.value = "";
    opt.textContent = t("conn.none");
    opt.disabled = true;
    opt.selected = true;
    select.appendChild(opt);
    return;
  }
  state.connections.forEach(c => {
    const opt = document.createElement("option");
    opt.value = c.id;
    opt.textContent = c.name;
    select.appendChild(opt);
  });
  if (selectedId && state.connections.some(c => c.id === selectedId)) {
    select.value = selectedId;
  } else if (!extraFirst) {
    select.value = state.connections[0].id;
  }
}

// ---------------------------------------------------------------------------
// Source picker: connection -> database -> optional collections
// ---------------------------------------------------------------------------

const PICKER_MANUAL = "__manual__";
const pickers = {};

function pickerEl(p, suffix) {
  return document.getElementById(`${p.prefix}-${suffix}`);
}

function setupPicker(prefix) {
  // preset holds the database and collections of a job being edited until the
  // lists they are picked from have loaded; any user choice discards it.
  const p = { prefix, manual: false, dbs: null, dbSeq: 0, collSeq: 0, dbError: "", preset: null };
  pickers[prefix] = p;
  pickerEl(p, "connection").addEventListener("change", () => {
    p.preset = null;
    loadPickerDatabases(p);
  });
  pickerEl(p, "database-select").addEventListener("change", (e) => {
    p.preset = null;
    if (e.target.value === PICKER_MANUAL) {
      setPickerManual(p, true);
      pickerEl(p, "database").focus();
    } else {
      loadPickerCollections(p);
    }
  });
  pickerEl(p, "database").addEventListener("change", () => {
    p.preset = null;
    loadPickerCollections(p);
  });
  document.querySelectorAll(`input[name="${prefix}-coll-mode"]`).forEach(radio => {
    radio.addEventListener("change", () => updatePickerCollectionsBox(p));
  });
}

function resetPicker(prefix, connectionId) {
  const p = pickers[prefix];
  p.preset = null;
  fillConnectionSelect(pickerEl(p, "connection"), connectionId);
  setValue(`${prefix}-database`, "");
  setValue(`${prefix}-collections-manual`, "");
  const all = document.querySelector(`input[name="${prefix}-coll-mode"][value="all"]`);
  if (all) all.checked = true;
  updatePickerCollectionsBox(p);
  loadPickerDatabases(p);
}

function setPickerManual(p, manual) {
  p.manual = manual;
  const select = pickerEl(p, "database-select");
  const input = pickerEl(p, "database");
  select.hidden = manual;
  select.required = !manual;
  input.hidden = !manual;
  input.required = manual;
  // A job covering several databases needs neither field (jobdbs.js).
  if (p.prefix === "job" && typeof jobDbsApplyRequired === "function") jobDbsApplyRequired();
  if (p.prefix === "instant" && typeof instantDbsApplyRequired === "function") instantDbsApplyRequired();
  pickerEl(p, "database-label").htmlFor = manual ? input.id : select.id;
  const fromList = document.querySelector(`[data-action="picker-from-list"][data-picker="${p.prefix}"]`);
  if (fromList) fromList.hidden = !manual || !p.dbs || p.dbs.length === 0;
  setText(`${p.prefix}-database-hint`, manual && p.dbError ? p.dbError : "");
}

function pickerFromList(prefix) {
  const p = pickers[prefix];
  if (!p) return;
  setPickerManual(p, false);
  const select = pickerEl(p, "database-select");
  select.value = "";
  select.focus();
  loadPickerCollections(p);
}

async function loadPickerDatabases(p) {
  const connId = pickerEl(p, "connection").value;
  const select = pickerEl(p, "database-select");
  const seq = ++p.dbSeq;
  p.dbs = null;
  p.dbError = "";
  clearPickerCollections(p);
  select.textContent = "";
  const loading = document.createElement("option");
  loading.value = "";
  loading.textContent = connId ? t("picker.loading_dbs") : t("conn.none");
  select.appendChild(loading);
  select.disabled = true;
  setPickerManual(p, false);
  if (!connId) return;

  try {
    const json = await apiJSON(`/api/v1/connections/${encodeURIComponent(connId)}/databases`);
    if (seq !== p.dbSeq) return;
    if (!json.success) throw new Error(json.error || "");
    p.dbs = json.data || [];
    select.textContent = "";
    const placeholder = document.createElement("option");
    placeholder.value = "";
    placeholder.textContent = t("picker.choose_db");
    select.appendChild(placeholder);
    p.dbs.forEach(db => {
      const opt = document.createElement("option");
      opt.value = db.name;
      const detail = db.empty ? t("picker.empty_db") : formatBytes(db.size_bytes);
      opt.textContent = `${db.name} · ${detail}`;
      select.appendChild(opt);
    });
    const manual = document.createElement("option");
    manual.value = PICKER_MANUAL;
    manual.textContent = t("picker.manual");
    select.appendChild(manual);
    select.disabled = false;
    setPickerManual(p, p.dbs.length === 0);
    applyPickerPreset(p);
  } catch (err) {
    if (seq !== p.dbSeq) return;
    select.disabled = false;
    p.dbError = tf("picker.dbs_failed", { error: truncate(errorSummary(err.message), 120) || "?" });
    setPickerManual(p, true);
    applyPickerPreset(p);
  }
  // The multi-database selector of the job form lists the same databases (jobdbs.js).
  if (typeof jobDbsOnDatabases === "function") jobDbsOnDatabases(p);
}

// Selects the preset database once the database list has loaded (typing it into
// the manual field when the list does not have it) and loads its collections.
function applyPickerPreset(p) {
  const preset = p.preset;
  if (!preset || !preset.database) return;
  if (p.dbs && p.dbs.some(db => db.name === preset.database)) {
    setPickerManual(p, false);
    pickerEl(p, "database-select").value = preset.database;
  } else {
    setPickerManual(p, true);
    setValue(`${p.prefix}-database`, preset.database);
  }
  loadPickerCollections(p);
}

function pickerDatabase(p) {
  // While an edited job's lists are still loading, its own database stands.
  if (p.preset && p.preset.database) return p.preset.database;
  if (p.manual) return pickerEl(p, "database").value.trim();
  const value = pickerEl(p, "database-select").value;
  return value === PICKER_MANUAL ? "" : value;
}

function pickerMode(p) {
  const checked = document.querySelector(`input[name="${p.prefix}-coll-mode"]:checked`);
  return checked ? checked.value : "all";
}

function clearPickerCollections(p) {
  p.collSeq++;
  const select = pickerEl(p, "collections");
  select.textContent = "";
  select.hidden = false;
  pickerEl(p, "collections-manual").hidden = true;
  setText(`${p.prefix}-collections-hint`, "");
}

function updatePickerCollectionsBox(p) {
  pickerEl(p, "collections-box").hidden = pickerMode(p) === "all";
}

async function loadPickerCollections(p) {
  const connId = pickerEl(p, "connection").value;
  const db = pickerDatabase(p);
  clearPickerCollections(p);
  const seq = p.collSeq;
  if (!connId || !db) return;
  setText(`${p.prefix}-collections-hint`, t("picker.loading_colls"));
  try {
    const json = await apiJSON(`/api/v1/connections/${encodeURIComponent(connId)}/databases/${encodeURIComponent(db)}/collections`);
    if (seq !== p.collSeq) return;
    if (!json.success) throw new Error(json.error || "");
    const select = pickerEl(p, "collections");
    (json.data || []).forEach(c => {
      const opt = document.createElement("option");
      opt.value = c.name;
      opt.textContent = c.type === "view" ? `${c.name} (${t("picker.view")})` : c.name;
      select.appendChild(opt);
    });
    setText(`${p.prefix}-collections-hint`, t("picker.colls_hint"));
    applyCollectionsPreset(p, select);
  } catch (err) {
    if (seq !== p.collSeq) return;
    pickerEl(p, "collections").hidden = true;
    pickerEl(p, "collections-manual").hidden = false;
    setText(`${p.prefix}-collections-hint`, t("picker.colls_failed"));
    applyCollectionsPreset(p, null);
  }
}

// Selects the preset collections in the loaded list; names the list does not
// have (or a list that failed to load) go to the manual field instead.
function applyCollectionsPreset(p, select) {
  const preset = p.preset;
  p.preset = null;
  if (!preset || preset.names.length === 0) return;
  const manual = pickerEl(p, "collections-manual");
  const options = select ? Array.from(select.options) : [];
  const available = new Set(options.map(o => o.value));
  if (select && preset.names.every(n => available.has(n))) {
    options.forEach(o => { o.selected = preset.names.includes(o.value); });
    return;
  }
  if (select) select.hidden = true;
  manual.hidden = false;
  manual.value = preset.names.join(", ");
}

// Returns the picker selection, or null after focusing the first invalid field.
function pickerValue(prefix) {
  const p = pickers[prefix];
  const connSelect = pickerEl(p, "connection");
  if (!connSelect.value) {
    connSelect.focus();
    showToast(t("conn.add_first"), "error");
    return null;
  }
  const database = pickerDatabase(p);
  if (!database) {
    (p.manual ? pickerEl(p, "database") : pickerEl(p, "database-select")).focus();
    showToast(t("picker.need_database"), "error");
    return null;
  }
  const mode = pickerMode(p);
  const manualInput = pickerEl(p, "collections-manual");
  let names = manualInput.hidden
    ? selectedValues(`${prefix}-collections`)
    : parseList(manualInput.value);
  if (p.preset) names = p.preset.names;
  if (mode !== "all" && names.length === 0) {
    (manualInput.hidden ? pickerEl(p, "collections") : manualInput).focus();
    showToast(t("picker.need_collections"), "error");
    return null;
  }
  const value = { connection_id: connSelect.value, database };
  if (mode === "include") value.collections = names;
  if (mode === "exclude") value.exclude_collections = names;
  return value;
}

// ---------------------------------------------------------------------------
// Settings: navigation between sub-sections
// ---------------------------------------------------------------------------

const SETTINGS_SECTIONS = ["general", "storage", "integrity", "encryption", "recovery", "security", "monitoring", "audit", "users", "sso", "apikeys", "sessions"];
const ADMIN_SETTINGS_SECTIONS = ["audit", "users", "sso"];

function showSettingsSection(name, focus) {
  if (!SETTINGS_SECTIONS.includes(name)) return;
  // Users and the audit log are for administrators only.
  if (ADMIN_SETTINGS_SECTIONS.includes(name) && !can("admin")) name = "general";
  document.querySelectorAll(".settings-nav-btn").forEach(btn => {
    const active = btn.dataset.section === name;
    btn.classList.toggle("active", active);
    if (active) {
      btn.setAttribute("aria-current", "page");
      // On narrow screens the section list scrolls sideways; keep the active one visible.
      if (btn.offsetParent !== null) btn.scrollIntoView({ block: "nearest", inline: "nearest" });
      if (focus) btn.focus();
    } else {
      btn.removeAttribute("aria-current");
    }
  });
  SETTINGS_SECTIONS.forEach(s => {
    const panel = document.getElementById(`settings-${s}`);
    if (panel) panel.hidden = s !== name;
  });
  // The activity log is only fetched when someone looks at it.
  if (name === "security") loadAudit();
  // Sessions too (forms.js).
  if (name === "sessions" && typeof loadSessions === "function") loadSessions();
  // Metadata backups and the recovery kit (recovery.js).
  if (name === "recovery" && typeof recoveryRefresh === "function") recoveryRefresh();
  // The audit log of every action (auditlog.js).
  if (name === "audit" && typeof auditlogRefresh === "function") auditlogRefresh(false);
}

function setupSettingsNav() {
  const nav = document.querySelector(".settings-nav");
  if (!nav) return;
  nav.addEventListener("keydown", (e) => {
    const keys = ["ArrowDown", "ArrowUp", "ArrowRight", "ArrowLeft", "Home", "End"];
    if (!keys.includes(e.key)) return;
    const items = Array.from(nav.querySelectorAll(".settings-nav-btn"));
    const idx = items.indexOf(document.activeElement);
    if (idx === -1) return;
    e.preventDefault();
    let next = idx;
    if (e.key === "Home") next = 0;
    else if (e.key === "End") next = items.length - 1;
    else if (e.key === "ArrowDown" || e.key === "ArrowRight") next = (idx + 1) % items.length;
    else next = (idx - 1 + items.length) % items.length;
    showSettingsSection(items[next].dataset.section, true);
  });
}

// ---------------------------------------------------------------------------
// Settings: general, security, encryption (GET/PUT /api/v1/settings)
// ---------------------------------------------------------------------------

const SETTINGS_FORMS = {
  general: { form: "form-general", error: "general-error", status: "general-status" },
  security: { form: "form-security", error: "security-error", status: "security-status" },
  encryption: { form: "form-encryption", error: "encryption-error", status: "encryption-status" }
};

// Forms edited since they were last filled from the server; refreshes leave them alone.
const dirtySettings = new Set();
const saveStatusTimers = {};

// A key pair from "Generate key pair" that has not been taken into use yet.
const generatedKey = { identity: "", recipient: "" };

const GO_DURATION_RE = /^(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h))+$/;
const GO_DURATION_UNITS = { ns: 1e-6, us: 1e-3, "µs": 1e-3, "μs": 1e-3, ms: 1, s: 1000, m: 60000, h: 3600000 };
const AGE_RECIPIENT_RE = /^age1[02-9ac-hj-np-z]{58}$/;
const AGE_IDENTITY_RE = /^AGE-SECRET-KEY-1[02-9AC-HJ-NP-Z]{58}$/;
const MIN_PASSPHRASE_LENGTH = 16;
const MIN_SESSION_MS = 60000;

function settingsGroup(name) {
  return (state.settings && state.settings[name]) || {};
}

async function loadSettings(force) {
  try {
    const json = await apiJSON("/api/v1/settings");
    if (!noteLoad("settings", json)) {
      state.settingsError = json.error || t("toasts.request_failed");
    } else {
      state.settings = json.data || {};
      state.settingsError = "";
      state.loaded.settings = true;
    }
  } catch (err) {
    state.settingsError = err.message;
  }
  fillSettingsForms(force);
  renderWarnings();
}

// WARNING_ENCRYPTION_OFF is the ID of the persistent "encryption is off after the
// upgrade" warning in GET /api/v1/settings (settings.WarningEncryptionOff).
const WARNING_ENCRYPTION_OFF = "encryption_off_after_upgrade";

// Shows the persistent warnings the server reports with the settings.
function renderWarnings() {
  const banner = document.getElementById("encryption-warning");
  if (!banner) return;
  const list = state.settings && Array.isArray(state.settings.warnings) ? state.settings.warnings : [];
  banner.hidden = !list.some(w => w && w.id === WARNING_ENCRYPTION_OFF);
  // Recovery kit reminder and unencrypted metadata backups (recovery.js).
  if (typeof recoveryRenderWarnings === "function") recoveryRenderWarnings(list);
  // A single sign-on kept the last administrator's role (sso.js).
  if (typeof ssoRenderWarnings === "function") ssoRenderWarnings(list);
}

async function dismissEncryptionWarning() {
  if (!(await confirmDialog({ body: t("enc.upgrade_warning_dismiss_confirm") }))) return;
  try {
    const json = await apiJSON(`/api/v1/settings/warnings/${WARNING_ENCRYPTION_OFF}/dismiss`, { method: "POST" });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    state.settings = { ...(state.settings || {}), warnings: (json.data && json.data.warnings) || [] };
    renderWarnings();
  } catch (err) {
    showToast(err.message, "error");
  }
}

// Fills every settings form that the operator is not currently editing.
function fillSettingsForms(force) {
  Object.keys(SETTINGS_FORMS).forEach(group => {
    const cfg = SETTINGS_FORMS[group];
    const submit = document.querySelector(`#${cfg.form} [type=submit]`);
    if (submit) submit.disabled = !state.loaded.settings;
    if (!state.loaded.settings) {
      if (state.settingsError) showFormError(cfg.error, tf("settings.load_failed", { error: state.settingsError }));
      return;
    }
    if (dirtySettings.has(group) && !force) return;
    dirtySettings.delete(group);
    hideFormError(cfg.error);
    clearInvalid(cfg.form);
    if (group === "general") fillGeneral(settingsGroup("general"));
    if (group === "security") fillSecurity(settingsGroup("security"));
    if (group === "encryption") fillEncryption(settingsGroup("encryption"));
  });
  renderRetiredKeys();
  // Settings → Integrity (trust.js).
  trustFillSettings(force);
  // Settings → Recovery (recovery.js).
  if (typeof recoveryFillSettings === "function") recoveryFillSettings(force);
  // Settings → Audit log (auditlog.js).
  if (typeof auditlogFillSettings === "function") auditlogFillSettings(force);
  // Settings → Single sign-on (sso.js).
  if (typeof ssoFillSettings === "function") ssoFillSettings(force);
  // Settings → Monitoring (monitoring.js).
  if (typeof monitoringFillSettings === "function") monitoringFillSettings(force);
}

function fillGeneral(g) {
  setValue("set-retention-days", g.default_retention_days ?? 0);
  setValue("set-retention-count", g.default_retention_count ?? 0);
  document.getElementById("set-gzip").checked = g.default_gzip !== false;
  setValue("set-backup-timeout", g.backup_timeout || "");
  setValue("set-backup-stall-timeout", g.backup_stall_timeout || "");
  setValue("set-restore-timeout", g.restore_timeout || "");
  setValue("set-verify-policy", ["always", "auto", "never"].includes(g.restore_verify_policy) ? g.restore_verify_policy : "auto");
  setValue("set-log-retention-days", g.log_retention_days ?? 30);
  updateDurationPreviews("form-general");
}

function fillSecurity(sec) {
  setValue("set-session-idle", sec.session_idle_timeout || "");
  setValue("set-session-absolute", sec.session_absolute_timeout || "");
  setValue("set-secure-cookies", ["auto", "always", "never"].includes(sec.secure_cookies) ? sec.secure_cookies : "auto");
  document.getElementById("set-trust-proxy").checked = !!sec.trust_proxy_headers;
  setValue("set-cors-origins", (sec.cors_origins || []).join("\n"));
  document.getElementById("set-metrics-public").checked = !!sec.metrics_public;
  document.getElementById("set-mcp-enabled").checked = sec.mcp_enabled !== false;
  // Delete grace period, two-person rule and pending changes (protection.js).
  if (typeof protectionFillSecurity === "function") protectionFillSecurity(sec);
  updateDurationPreviews("form-security");
}

function fillEncryption(enc) {
  document.getElementById("enc-enabled").checked = !!enc.enabled;
  const mode = enc.mode === "passphrase" ? "passphrase" : "x25519";
  document.querySelectorAll('input[name="enc-mode"]').forEach(r => { r.checked = r.value === mode; });
  setValue("enc-recipients", (enc.recipients || []).join("\n"));
  setValue("enc-identity", enc.identity || "");
  // A stored passphrase is masked; mirroring it into the confirmation keeps it unchanged.
  setValue("enc-passphrase", enc.passphrase || "");
  setValue("enc-passphrase-confirm", enc.passphrase || "");
  updateEncryptionMode();
}

function resetSettingsForms() {
  dirtySettings.clear();
  Object.keys(SETTINGS_FORMS).forEach(group => {
    const cfg = SETTINGS_FORMS[group];
    const form = document.getElementById(cfg.form);
    if (form) form.reset();
    hideFormError(cfg.error);
    setText(cfg.status, "");
  });
  discardGeneratedKey(false);
  updateEncryptionMode();
  showSettingsSection("general", false);
}

function setupSettingsForms() {
  setupSettingsNav();
  Object.keys(SETTINGS_FORMS).forEach(group => {
    const cfg = SETTINGS_FORMS[group];
    const form = document.getElementById(cfg.form);
    const markDirty = (e) => {
      if (e.target.closest(".generated-key")) return;
      dirtySettings.add(group);
      setText(cfg.status, "");
      if (e.target.getAttribute("aria-invalid") === "true") e.target.removeAttribute("aria-invalid");
    };
    form.addEventListener("input", markDirty);
    form.addEventListener("change", markDirty);
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      saveSettingsGroup(group, e.submitter);
    });
    const submit = form.querySelector("[type=submit]");
    if (submit) submit.disabled = true;
  });
  document.querySelectorAll("input[data-duration]").forEach(input => {
    input.addEventListener("input", () => updateDurationPreview(input));
  });
  document.querySelectorAll('input[name="enc-mode"]').forEach(r => r.addEventListener("change", updateEncryptionMode));
  document.getElementById("enc-enabled").addEventListener("change", updateEncryptionMode);
  document.getElementById("enc-gen-saved").addEventListener("change", (e) => {
    document.getElementById("enc-use-key").disabled = !e.target.checked;
  });
  // A generated private key lives only in this page until it is taken into use.
  window.addEventListener("beforeunload", (e) => {
    if (!generatedKey.identity) return;
    e.preventDefault();
    e.returnValue = "";
  });
  updateEncryptionMode();
}

// Parses a Go duration string ("1h30m", "90s", "0") into milliseconds, or NaN.
function parseGoDuration(value) {
  const s = String(value || "").trim();
  if (s === "0") return 0;
  if (!GO_DURATION_RE.test(s)) return NaN;
  let total = 0;
  for (const m of s.matchAll(/(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/g)) {
    total += parseFloat(m[1]) * GO_DURATION_UNITS[m[2]];
  }
  return total;
}

// Compact reading of a duration, such as "7d", "1d 6h" or "1m 30s".
function humanDuration(ms) {
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const parts = [];
  let rest = Math.round(ms / 1000);
  [["d", 86400], ["h", 3600], ["m", 60], ["s", 1]].forEach(([unit, size]) => {
    const n = Math.floor(rest / size);
    rest -= n * size;
    if (n > 0 && parts.length < 2) parts.push(`${n}${unit}`);
  });
  return parts.join(" ");
}

function updateDurationPreview(input) {
  const preview = document.getElementById(`${input.id}-preview`);
  if (!preview) return;
  const raw = input.value.trim();
  const ms = parseGoDuration(raw);
  preview.classList.toggle("is-invalid", raw !== "" && isNaN(ms));
  if (raw === "") preview.textContent = "";
  else if (isNaN(ms)) preview.textContent = t("settings.duration_invalid");
  else if (ms === 0) preview.textContent = t("settings.no_limit");
  else preview.textContent = `= ${humanDuration(ms)}`;
}

function updateDurationPreviews(formId) {
  document.querySelectorAll(`#${formId} input[data-duration]`).forEach(updateDurationPreview);
}

function updateEncryptionMode() {
  const checked = document.querySelector('input[name="enc-mode"]:checked');
  const mode = checked ? checked.value : "x25519";
  document.querySelectorAll(".enc-mode-fields").forEach(el => {
    el.hidden = el.dataset.encMode !== mode;
  });
  const options = document.getElementById("enc-options");
  if (options) options.classList.toggle("is-muted", !document.getElementById("enc-enabled").checked);
}

function fieldLabel(id) {
  const label = document.querySelector(`label[for="${id}"]`);
  return label ? label.textContent.trim() : id;
}

function clearInvalid(formId) {
  document.querySelectorAll(`#${formId} [aria-invalid="true"]`).forEach(el => el.removeAttribute("aria-invalid"));
}

// A validation failure: the message plus the field to focus.
class FieldError extends Error {
  constructor(field, message) {
    super(message);
    this.field = field;
  }
}

function intSetting(id) {
  const raw = getValue(id);
  if (!/^\d+$/.test(raw)) throw new FieldError(id, tf("settings.err_int", { field: fieldLabel(id) }));
  return parseInt(raw, 10);
}

function durationSetting(id, minMs) {
  const raw = getValue(id);
  const ms = parseGoDuration(raw);
  if (raw === "" || isNaN(ms)) throw new FieldError(id, tf("settings.err_duration", { field: fieldLabel(id) }));
  if (minMs && ms < minMs) throw new FieldError(id, tf("settings.err_duration_min", { field: fieldLabel(id), min: humanDuration(minMs) }));
  return { raw, ms };
}

function collectGeneral() {
  return {
    default_retention_days: intSetting("set-retention-days"),
    default_retention_count: intSetting("set-retention-count"),
    default_gzip: document.getElementById("set-gzip").checked,
    backup_timeout: durationSetting("set-backup-timeout").raw,
    backup_stall_timeout: durationSetting("set-backup-stall-timeout").raw,
    restore_timeout: durationSetting("set-restore-timeout").raw,
    restore_verify_policy: getValue("set-verify-policy"),
    log_retention_days: intSetting("set-log-retention-days")
  };
}

function collectSecurity() {
  const idle = durationSetting("set-session-idle", MIN_SESSION_MS);
  const absolute = durationSetting("set-session-absolute", MIN_SESSION_MS);
  if (absolute.ms < idle.ms) throw new FieldError("set-session-absolute", t("settings.err_session_order"));
  const origins = parseList(getValue("set-cors-origins"));
  origins.forEach(origin => {
    let ok = false;
    try {
      const u = new URL(origin);
      ok = (u.protocol === "https:" || u.protocol === "http:") && u.origin === origin;
    } catch (err) {
      ok = false;
    }
    if (!ok) throw new FieldError("set-cors-origins", tf("settings.err_cors", { value: truncate(origin, 60) }));
  });
  return {
    session_idle_timeout: idle.raw,
    session_absolute_timeout: absolute.raw,
    secure_cookies: getValue("set-secure-cookies"),
    trust_proxy_headers: document.getElementById("set-trust-proxy").checked,
    cors_origins: origins,
    metrics_public: document.getElementById("set-metrics-public").checked,
    mcp_enabled: document.getElementById("set-mcp-enabled").checked,
    ...(typeof protectionCollectSecurity === "function" ? protectionCollectSecurity() : {})
  };
}

// Validates the encryption form. overrides lets "Use this key" add a generated
// identity and recipient without touching the visible fields first.
function collectEncryption(overrides) {
  const checked = document.querySelector('input[name="enc-mode"]:checked');
  const mode = (overrides && overrides.mode) || (checked ? checked.value : "x25519");
  const enabled = document.getElementById("enc-enabled").checked;
  const recipients = getValue("enc-recipients").split(/[\s,]+/).map(s => s.trim()).filter(Boolean);
  if (overrides && overrides.recipient && !recipients.includes(overrides.recipient)) recipients.push(overrides.recipient);
  const identity = overrides && overrides.identity ? overrides.identity : getValue("enc-identity");
  const passphrase = document.getElementById("enc-passphrase").value;
  const confirmation = document.getElementById("enc-passphrase-confirm").value;

  if (mode === "x25519") {
    const bad = recipients.find(r => !AGE_RECIPIENT_RE.test(r));
    if (bad) throw new FieldError("enc-recipients", tf("enc.err_recipient", { value: truncate(bad, 24) }));
    if (enabled && recipients.length === 0) throw new FieldError("enc-recipients", t("enc.err_recipients_required"));
    if (identity && identity !== MASKED_SECRET && !AGE_IDENTITY_RE.test(identity)) {
      throw new FieldError("enc-identity", t("enc.err_identity"));
    }
  } else {
    const changed = passphrase !== MASKED_SECRET;
    if (enabled && !passphrase) throw new FieldError("enc-passphrase", t("enc.err_passphrase_required"));
    if (passphrase && changed && passphrase.length < MIN_PASSPHRASE_LENGTH) {
      throw new FieldError("enc-passphrase", tf("enc.err_passphrase_short", { n: MIN_PASSPHRASE_LENGTH }));
    }
    if (changed && passphrase !== confirmation) throw new FieldError("enc-passphrase-confirm", t("enc.err_passphrase_mismatch"));
  }
  return { enabled, mode, recipients, identity, passphrase };
}

async function saveSettingsGroup(group, submitter, overrides) {
  const cfg = SETTINGS_FORMS[group];
  hideFormError(cfg.error);
  clearInvalid(cfg.form);
  setText(cfg.status, "");
  let payload;
  try {
    if (group === "general") payload = collectGeneral();
    else if (group === "security") payload = collectSecurity();
    else payload = collectEncryption(overrides);
  } catch (err) {
    if (!(err instanceof FieldError)) throw err;
    showFormError(cfg.error, err.message);
    const field = document.getElementById(err.field);
    if (field) {
      field.setAttribute("aria-invalid", "true");
      field.focus();
    }
    return false;
  }

  if (group === "encryption" && !overrides) {
    const before = settingsGroup("encryption");
    if (before.enabled && !payload.enabled && !(await confirmDialog({ body: t("enc.confirm_disable"), danger: true, confirmLabel: t("dialog.confirm") }))) return false;
    if (payload.enabled && payload.mode === "x25519" && !payload.identity && !(await confirmDialog({ body: t("enc.confirm_no_identity"), danger: true, confirmLabel: t("dialog.confirm") }))) return false;
  }

  const btn = submitter || document.querySelector(`#${cfg.form} [type=submit]`);
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON("/api/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ [group]: payload })
    });
    if (!json.success) {
      showFormError(cfg.error, json.error || t("notify.toast_save_failed"));
      return false;
    }
    const data = json.data || {};
    if (data.general || data.security || data.encryption) {
      state.settings = { ...(state.settings || {}), ...data };
      state.loaded.settings = true;
    } else {
      await loadSettings(false);
    }
    dirtySettings.delete(group);
    fillSettingsForms(false);
    renderWarnings();
    showSaved(group, data.restart_required);
    // A lowered protection waits (protection.js).
    if (typeof protectionSettingsSaved === "function") protectionSettingsSaved(data);
    return true;
  } catch (err) {
    showFormError(cfg.error, err.message);
    return false;
  } finally {
    if (btn) btn.disabled = !state.loaded.settings;
  }
}

// "Saved" next to the button; settings that only apply after a restart are named.
function showSaved(group, restartRequired) {
  const cfg = SETTINGS_FORMS[group];
  const el = document.getElementById(cfg.status);
  if (!el) return;
  const restart = Array.isArray(restartRequired) ? restartRequired.filter(Boolean) : [];
  const text = restart.length > 0 ? tf("settings.saved_restart", { keys: restart.join(", ") }) : t("settings.saved");
  el.classList.toggle("is-warn", restart.length > 0);
  el.innerHTML = `${restart.length > 0 ? "" : icon("check")}<span>${escapeHtml(text)}</span>`;
  clearTimeout(saveStatusTimers[group]);
  if (restart.length === 0) {
    saveStatusTimers[group] = setTimeout(() => { el.textContent = ""; }, 4000);
  }
}

function renderRetiredKeys() {
  const tbody = document.getElementById("retired-keys-tbody");
  if (!tbody || !state.loaded.settings) return;
  const retired = settingsGroup("encryption").retired_keys || [];
  if (retired.length === 0) {
    setTbody(tbody, emptyRow(2, t("enc.retired_empty")));
    return;
  }
  setTbody(tbody, retired.map(k => `<tr>
      <td><span class="mono key-cell" title="${escapeHtml(k.recipient || "")}">${k.recipient ? escapeHtml(k.recipient) : mutedDash()}</span></td>
      <td>${timeCell(k.retired_at)}</td>
    </tr>`).join(""));
}

// Re-applies language-dependent text that is set from JavaScript.
function renderSettingsLanguage() {
  document.querySelectorAll("input[data-duration]").forEach(updateDurationPreview);
  renderRetiredKeys();
  updateProviderHint();
}

// "Verify integrity first" default for the restore dialog, from Settings > General.
function verifyDefault(isSafeClone) {
  const policy = settingsGroup("general").restore_verify_policy;
  if (policy === "always") return true;
  if (policy === "never") return false;
  return !isSafeClone;
}

// ---------------------------------------------------------------------------
// Encryption: key pair generation (identity shown once, never stored unless used)
// ---------------------------------------------------------------------------

async function generateKeyPair(btn) {
  if (generatedKey.identity && !(await confirmDialog({ body: t("enc.confirm_replace_generated"), danger: true, confirmLabel: t("dialog.confirm") }))) return;
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON("/api/v1/settings/encryption/generate-key", { method: "POST" });
    const data = json.data || {};
    if (!json.success || !data.identity || !data.recipient) {
      showToast(json.error || t("enc.generate_failed"), "error");
      return;
    }
    generatedKey.identity = data.identity;
    generatedKey.recipient = data.recipient;
    setValue("enc-gen-identity", data.identity);
    setText("enc-gen-recipient", data.recipient);
    setText("enc-copy-label", t("settings.copy"));
    document.getElementById("enc-gen-saved").checked = false;
    document.getElementById("enc-use-key").disabled = true;
    const panel = document.getElementById("enc-generated");
    panel.hidden = false;
    panel.scrollIntoView({ block: "nearest" });
    document.querySelector('[data-action="copy-identity"]').focus();
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    if (btn) btn.disabled = false;
  }
}

function downloadIdentity() {
  if (!generatedKey.identity) return;
  const created = new Date().toISOString();
  const text = `# created: ${created}\n# public key: ${generatedKey.recipient}\n${generatedKey.identity}\n`;
  const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = `mongorescue-age-key-${created.slice(0, 10)}.txt`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

// Stores the generated identity (encrypted server-side) and adds its public key
// to the recipients. The previous identity becomes a retired key.
async function useGeneratedKey(btn) {
  if (!generatedKey.identity || !document.getElementById("enc-gen-saved").checked) return;
  const saved = await saveSettingsGroup("encryption", btn, {
    mode: "x25519",
    identity: generatedKey.identity,
    recipient: generatedKey.recipient
  });
  if (!saved) return;
  discardGeneratedKey(false);
  fillSettingsForms(true);
  showToast(t("enc.key_in_use"), "success");
  const generate = document.querySelector('[data-action="generate-key"]');
  if (generate) generate.focus();
}

// With ask, it confirms first (asynchronously); without, it discards at once.
async function discardGeneratedKey(ask) {
  if (ask && generatedKey.identity && !(await confirmDialog({ body: t("enc.confirm_discard"), danger: true, confirmLabel: t("dialog.confirm") }))) return;
  generatedKey.identity = "";
  generatedKey.recipient = "";
  setValue("enc-gen-identity", "");
  setText("enc-gen-recipient", "");
  const saved = document.getElementById("enc-gen-saved");
  if (saved) saved.checked = false;
  const use = document.getElementById("enc-use-key");
  if (use) use.disabled = true;
  const panel = document.getElementById("enc-generated");
  if (panel) panel.hidden = true;
  if (ask) {
    const generate = document.querySelector('[data-action="generate-key"]');
    if (generate) generate.focus();
  }
}

// ---------------------------------------------------------------------------
// Storage targets
// ---------------------------------------------------------------------------

// Provider presets only pre-fill the S3 form; the server stores type "s3".
// {region} in an endpoint follows the region field while the endpoint is untouched.
const STORAGE_PRESETS = {
  local: { type: "local", label: "" },
  aws: { type: "s3", label: "AWS S3", endpoint: "", region: "us-east-1", pathStyle: false },
  r2: { type: "s3", label: "Cloudflare R2", endpoint: "https://<ACCOUNT_ID>.r2.cloudflarestorage.com", region: "auto", pathStyle: false },
  b2: { type: "s3", label: "Backblaze B2", endpoint: "https://s3.{region}.backblazeb2.com", region: "us-west-004", pathStyle: false },
  minio: { type: "s3", label: "MinIO", endpoint: "http://minio:9000", region: "us-east-1", pathStyle: true },
  do: { type: "s3", label: "DigitalOcean Spaces", endpoint: "https://{region}.digitaloceanspaces.com", region: "nyc3", pathStyle: false },
  wasabi: { type: "s3", label: "Wasabi", endpoint: "https://s3.{region}.wasabisys.com", region: "us-east-1", pathStyle: false },
  s3: { type: "s3", label: "" }
};

// Endpoint the form filled in itself, so region edits may keep it in sync.
const storageAuto = { endpoint: "" };

// Reloads the storage targets; the error of a failed reload is returned, or
// "" once the list is up to date.
async function loadStorageTargets() {
  try {
    const json = await apiJSON("/api/v1/storage-targets");
    if (!noteLoad("storage_targets", json)) {
      const msg = json.error || t("toasts.request_failed");
      console.error("Failed to load storage targets:", msg);
      return msg;
    }
    state.storageTargets = json.data || [];
    state.loaded.storageTargets = true;
    renderStorageTargets();
    return "";
  } catch (err) {
    console.error("Failed to load storage targets:", err);
    return err.message || t("toasts.request_failed");
  }
}

// Puts a saved target into the list when the reload after saving failed, so
// the dialog it was created from can still select it.
function rememberSavedTarget(target, makeDefault) {
  if (!target || !target.id) return;
  const list = state.storageTargets.filter(s => s.id !== target.id);
  const saved = { ...target };
  if (makeDefault) {
    list.forEach(s => { s.is_default = false; });
    saved.is_default = true;
  }
  const idx = state.storageTargets.findIndex(s => s.id === target.id);
  if (idx >= 0) list.splice(idx, 0, saved);
  else list.push(saved);
  state.storageTargets = list;
  state.loaded.storageTargets = true;
  renderStorageTargets();
}

function defaultStorageTarget() {
  return state.storageTargets.find(s => s.is_default) || null;
}

function storageTargetName(id) {
  const s = id ? state.storageTargets.find(x => x.id === id) : null;
  return s ? s.name : "";
}

// endpointHostname returns the host name of an S3 endpoint ("" when it cannot be
// parsed); an endpoint without a scheme is read as https.
function endpointHostname(endpoint) {
  try {
    return new URL(/^[a-z][a-z0-9+.-]*:\/\//.test(endpoint) ? endpoint : "https://" + endpoint).hostname;
  } catch {
    return "";
  }
}

function inferProvider(target) {
  if (!target || target.type === "local") return "local";
  const endpoint = String((target.s3 && target.s3.endpoint) || "").toLowerCase();
  if (!endpoint) return "aws";
  const host = endpointHostname(endpoint);
  const onDomain = d => host === d || host.endsWith("." + d);
  if (onDomain("amazonaws.com") || onDomain("amazonaws.com.cn")) return "aws";
  if (onDomain("r2.cloudflarestorage.com")) return "r2";
  if (onDomain("backblazeb2.com")) return "b2";
  if (onDomain("digitaloceanspaces.com")) return "do";
  if (onDomain("wasabisys.com")) return "wasabi";
  if (endpoint.includes("minio") || /:9000(\/|$)/.test(endpoint)) return "minio";
  return "s3";
}

function providerLabel(provider) {
  if (provider === "local") return t("storage.provider_local");
  if (provider === "s3") return t("storage.provider_s3");
  return (STORAGE_PRESETS[provider] && STORAGE_PRESETS[provider].label) || provider;
}

function endpointHost(endpoint) {
  try {
    return new URL(endpoint).host;
  } catch (err) {
    return String(endpoint || "");
  }
}

function storageLocation(target) {
  if (!target) return "";
  if (target.type === "local") return (target.local && target.local.path) || "";
  const s3 = target.s3 || {};
  const prefix = String(s3.prefix || "").replace(/^\/+/, "");
  return prefix ? `${s3.bucket}/${prefix}` : String(s3.bucket || "");
}

function storageTestBadge(target) {
  if (!parseDate(target.last_test_at)) return statusBadge("neutral", t("conn.untested"));
  return target.last_test_ok
    ? statusBadge("success", t("storage.writable"))
    : statusBadge("danger", t("status.failed"), target.last_test_error);
}

function renderStorageTargets() {
  const tbody = document.getElementById("storage-tbody");
  if (!tbody || !state.loaded.storageTargets) return;
  if (state.storageTargets.length === 0) {
    setTbody(tbody, emptyRow(5, t("storage.empty"), "new-storage", "plus", t("storage.add")));
    return;
  }
  setTbody(tbody, state.storageTargets.map(s => {
    const provider = inferProvider(s);
    const s3 = s.s3 || {};
    const where = s.type === "local"
      ? ""
      : `<div class="cell-sub">${escapeHtml(s3.endpoint ? endpointHost(s3.endpoint) : s3.region || "")}</div>`;
    const errorLine = parseDate(s.last_test_at) && !s.last_test_ok && s.last_test_error
      ? errorDetail(s.last_test_error, truncate(s.last_test_error, 70))
      : "";
    // The default carries a badge; every other target offers "Set default" in its place.
    const defaultCell = s.is_default
      ? `<span class="chip chip-accent">${escapeHtml(t("storage.default"))}</span>`
      : `<button type="button" class="link-btn" data-action="default-storage" data-id="${escapeHtml(s.id)}">${escapeHtml(t("storage.set_default"))}</button>`;
    const deleteAttrs = s.is_default ? ` disabled title="${escapeHtml(t("storage.cannot_delete_default"))}"` : "";
    const tested = parseDate(s.last_test_at) ? `<span class="cell-sub test-time">${timeCell(s.last_test_at)}</span>` : "";
    return `<tr>
      <td class="cell-primary">${ellipsis(s.name)}<div class="storage-default">${defaultCell}</div></td>
      <td>${escapeHtml(providerLabel(provider))}</td>
      <td><span class="mono host-cell storage-location" title="${escapeHtml(storageLocation(s))}">${escapeHtml(storageLocation(s))}</span>${where}</td>
      <td><div class="status-line">${storageTestBadge(s)}${tested}</div>${errorLine}</td>
      <td class="col-actions"><div class="row-actions">
        <button type="button" class="btn btn-secondary btn-sm" data-action="test-storage" data-id="${escapeHtml(s.id)}">${escapeHtml(t("conn.test_short"))}</button>
        <button type="button" class="btn btn-secondary btn-sm" data-action="edit-storage" data-id="${escapeHtml(s.id)}">${escapeHtml(t("notify.edit"))}</button>
        <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="delete-storage" data-id="${escapeHtml(s.id)}"${deleteAttrs}>${escapeHtml(t("actions.delete"))}</button>
      </div></td>
    </tr>`;
  }).join(""));
  // Storage scans of every target (trust.js).
  trustRenderScans();
  // Target of the metadata backups (recovery.js).
  if (typeof recoveryFillTargets === "function") recoveryFillTargets();
}

// Name of the target a backup was written to (snapshot first: the target may be gone).
function backupStorageName(b) {
  return b.storage_target_name || storageTargetName(b.storage_target_id) || "";
}

// Storage target line under a backup's size.
function backupStorageCell(b) {
  let name = backupStorageName(b);
  if (!name && b.storage_type) name = b.storage_type === "local" ? t("storage.provider_local") : String(b.storage_type).toUpperCase();
  if (!name) return "";
  const title = `${t("storage.select_label")}: ${name}`;
  return `<div class="cell-sub storage-sub" title="${escapeHtml(title)}">${escapeHtml(truncate(name, 24))}</div>`;
}

// Storage target select a storage form opened from its "New" button returns to.
let storageReturn = null;

// Storage target select of the job form and "Back up now"; the default is
// preselected unless selectedId names an existing target.
function fillStorageSelect(select, selectedId) {
  if (!select) return;
  select.textContent = "";
  const addOption = (value, label) => {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = label;
    select.appendChild(opt);
  };
  if (state.storageTargets.length === 0) {
    addOption("", t("storage.default_target"));
    return;
  }
  state.storageTargets.forEach(s => {
    addOption(s.id, s.is_default ? tf("storage.default_named", { name: s.name }) : s.name);
  });
  const def = defaultStorageTarget();
  if (selectedId && state.storageTargets.some(s => s.id === selectedId)) {
    select.value = selectedId;
  } else {
    select.value = def ? def.id : state.storageTargets[0].id;
  }
}

// Opens the storage form on top of a dialog; a target saved there is selected
// in selectId, a cancelled form leaves the select as it was.
function openStorageModalFor(selectId) {
  const select = document.getElementById(selectId);
  if (!select) return;
  openStorageModal("");
  storageReturn = { select };
}

function storageSelection(selectId) {
  const value = getValue(selectId);
  return value ? { storage_target_id: value } : {};
}

function setupStorageForm() {
  const provider = document.getElementById("storage-provider");
  provider.addEventListener("change", () => applyProvider(provider.value, true));
  document.getElementById("storage-region").addEventListener("input", () => {
    const preset = STORAGE_PRESETS[provider.value] || {};
    const endpoint = document.getElementById("storage-endpoint");
    if (preset.endpoint && preset.endpoint.includes("{region}") && endpoint.value === storageAuto.endpoint) {
      endpoint.value = presetEndpoint(preset, getValue("storage-region"));
      storageAuto.endpoint = endpoint.value;
    }
  });
  // Any edit of what gets tested invalidates the last test result.
  const form = document.getElementById("form-storage");
  const invalidate = (e) => {
    if (e.target.id !== "storage-default") resetStorageTest();
  };
  form.addEventListener("input", invalidate);
  form.addEventListener("change", invalidate);
  form.addEventListener("submit", saveStorageTarget);
}

function presetEndpoint(preset, region) {
  return String(preset.endpoint || "").replace("{region}", region || preset.region || "");
}

// Switches the form to a provider. fromUser pre-fills the provider-specific
// endpoint, region and path style; the generic S3 choice keeps what is there.
function applyProvider(provider, fromUser) {
  const preset = STORAGE_PRESETS[provider] || STORAGE_PRESETS.s3;
  document.querySelectorAll("#form-storage .storage-fields").forEach(el => {
    el.hidden = el.dataset.storageType !== preset.type;
  });
  const endpoint = document.getElementById("storage-endpoint");
  const region = document.getElementById("storage-region");
  endpoint.placeholder = preset.endpoint ? preset.endpoint.replace("{region}", "<region>") : provider === "aws" ? "" : "https://s3.example.com";
  region.placeholder = preset.region || "";
  if (fromUser && preset.type === "s3" && preset.region !== undefined) {
    region.value = preset.region;
    endpoint.value = presetEndpoint(preset, region.value);
    storageAuto.endpoint = endpoint.value;
    document.getElementById("storage-path-style").checked = preset.pathStyle;
  }
  updateProviderHint();
}

function updateProviderHint() {
  const select = document.getElementById("storage-provider");
  if (!select) return;
  setText("storage-provider-hint", t(`storage.hint_${select.value}`, ""));
}

function openStorageModal(id) {
  const target = id ? state.storageTargets.find(s => s.id === id) : null;
  if (id && !target) return;
  const form = document.getElementById("form-storage");
  if (form) form.reset();
  storageAuto.endpoint = "";
  const s3 = (target && target.s3) || {};
  setValue("storage-id", target ? target.id : "");
  setValue("storage-name", target ? target.name : "");
  setValue("storage-path", target && target.local ? target.local.path : "");
  setValue("storage-endpoint", s3.endpoint || "");
  setValue("storage-region", s3.region || "");
  setValue("storage-bucket", s3.bucket || "");
  setValue("storage-prefix", s3.prefix || "");
  setValue("storage-access-key", s3.access_key_id || "");
  setValue("storage-secret-key", s3.secret_access_key || "");
  document.getElementById("storage-path-style").checked = !!s3.use_path_style;
  const provider = target ? inferProvider(target) : state.storageTargets.some(s => s.type === "local") ? "aws" : "local";
  setValue("storage-provider", provider);
  applyProvider(provider, !target);
  // The default can only move to another target, never be switched off here.
  const isDefault = !!(target && target.is_default);
  const def = document.getElementById("storage-default");
  def.checked = isDefault || state.storageTargets.length === 0;
  def.disabled = isDefault;
  setText("storage-modal-title", target ? t("storage.modal_edit") : t("storage.modal_new"));
  resetStorageTest();
  openModal("modal-storage");
}

// Builds the create/update body, or throws FieldError for the first invalid field.
function storagePayload() {
  const name = getValue("storage-name");
  if (!name) throw new FieldError("storage-name", t("storage.err_name"));
  const provider = getValue("storage-provider");
  const preset = STORAGE_PRESETS[provider] || STORAGE_PRESETS.s3;
  if (preset.type === "local") {
    const path = getValue("storage-path");
    if (!path) throw new FieldError("storage-path", t("storage.err_path"));
    if (path.split(/[\\/]+/).includes("..")) throw new FieldError("storage-path", t("storage.err_path_traversal"));
    return { name, type: "local", local: { path } };
  }
  const endpoint = getValue("storage-endpoint").replace(/\/+$/, "");
  if (/[<>{}]/.test(endpoint)) throw new FieldError("storage-endpoint", t("storage.err_endpoint_placeholder"));
  if (endpoint) {
    let ok = false;
    try {
      const u = new URL(endpoint);
      ok = u.protocol === "https:" || u.protocol === "http:";
    } catch (err) {
      ok = false;
    }
    if (!ok) throw new FieldError("storage-endpoint", t("storage.err_endpoint"));
  } else if (provider !== "aws") {
    throw new FieldError("storage-endpoint", t("storage.err_endpoint_required"));
  }
  const bucket = getValue("storage-bucket");
  if (!bucket || /[\s/]/.test(bucket)) throw new FieldError("storage-bucket", t("storage.err_bucket"));
  const region = getValue("storage-region");
  if (provider === "aws" && !region) throw new FieldError("storage-region", t("storage.err_region"));
  const accessKey = getValue("storage-access-key");
  const secretKey = document.getElementById("storage-secret-key").value;
  if (!!accessKey !== !!secretKey) {
    throw new FieldError(accessKey ? "storage-secret-key" : "storage-access-key", t("storage.err_credentials"));
  }
  return {
    name,
    type: "s3",
    s3: {
      endpoint,
      region,
      bucket,
      prefix: getValue("storage-prefix").replace(/^\/+/, ""),
      access_key_id: accessKey,
      secret_access_key: secretKey,
      use_path_style: document.getElementById("storage-path-style").checked
    }
  };
}

// True when the form still holds exactly what is stored (masked secret included),
// so the saved target can be tested with its stored credentials.
function storageUnchanged(target, payload) {
  if (!target || target.name !== payload.name || target.type !== payload.type) return false;
  if (payload.type === "local") return ((target.local && target.local.path) || "") === payload.local.path;
  const a = target.s3 || {};
  const b = payload.s3;
  return ["endpoint", "region", "bucket", "prefix", "access_key_id", "secret_access_key"].every(k => String(a[k] || "") === String(b[k] || "")) &&
    !!a.use_path_style === !!b.use_path_style;
}

function resetStorageTest() {
  storageTest.sig = null;
  storageTest.ok = false;
  storageTest.saveAnyway = false;
  const result = document.getElementById("storage-test-result");
  result.textContent = "";
  result.className = "test-result";
  setText("storage-submit", t("notify.save"));
}

function showStorageTestResult(kind, text) {
  const result = document.getElementById("storage-test-result");
  result.className = `test-result test-${kind}`;
  result.innerHTML = kind === "pending" ? escapeHtml(text) : `${statusGlyph(kind === "ok" ? "success" : "danger")}<span>${escapeHtml(text)}</span>`;
}

function describeStorageTest(data) {
  if (data && data.ok) return tf("storage.test_ok", { ms: Math.round(Number(data.latency_ms) || 0) });
  return tf("storage.test_failed", { error: truncate(data && data.error ? data.error : "", 160) });
}

function storageFormPayload() {
  try {
    return storagePayload();
  } catch (err) {
    if (!(err instanceof FieldError)) throw err;
    showStorageTestResult("fail", err.message);
    const field = document.getElementById(err.field);
    if (field) {
      field.setAttribute("aria-invalid", "true");
      field.focus();
      field.addEventListener("input", () => field.removeAttribute("aria-invalid"), { once: true });
    }
    return null;
  }
}

// Tests the form. An unchanged saved target is tested with /{id}/test; edited
// values go to /storage-targets/test together with the id, so a masked secret
// can be resolved from the stored target.
async function testStorageForm() {
  const payload = storageFormPayload();
  if (!payload) return false;
  const id = getValue("storage-id");
  const existing = id ? state.storageTargets.find(s => s.id === id) : null;
  const useStored = storageUnchanged(existing, payload);
  const btn = document.getElementById("storage-test-btn");
  btn.disabled = true;
  showStorageTestResult("pending", t("storage.testing"));
  let data;
  try {
    const json = useStored
      ? await apiJSON(`/api/v1/storage-targets/${encodeURIComponent(id)}/test`, { method: "POST" })
      : await apiJSON("/api/v1/storage-targets/test", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(id ? { ...payload, id } : payload)
      });
    data = json.success ? (json.data || {}) : { ok: false, error: json.error || "" };
  } catch (err) {
    data = { ok: false, error: err.message };
  } finally {
    btn.disabled = false;
  }
  storageTest.sig = JSON.stringify(payload);
  storageTest.ok = !!data.ok;
  showStorageTestResult(data.ok ? "ok" : "fail", describeStorageTest(data));
  if (useStored) loadStorageTargets();
  return storageTest.ok;
}

async function saveStorageTarget(e) {
  e.preventDefault();
  const payload = storageFormPayload();
  if (!payload) return;
  const id = getValue("storage-id");
  const existing = id ? state.storageTargets.find(s => s.id === id) : null;
  const makeDefault = document.getElementById("storage-default").checked && !(existing && existing.is_default);

  // Test before saving; a failing test must be overridden explicitly.
  if (!storageTest.saveAnyway) {
    const ok = storageTest.sig === JSON.stringify(payload) ? storageTest.ok : await testStorageForm();
    if (!ok) {
      storageTest.saveAnyway = true;
      setText("storage-submit", t("conn.save_anyway"));
      showToast(t("storage.test_before_save"), "error");
      return;
    }
  }

  const url = id ? `/api/v1/storage-targets/${encodeURIComponent(id)}` : "/api/v1/storage-targets";
  const submit = document.getElementById("storage-submit");
  submit.disabled = true;
  try {
    const json = await apiJSON(url, {
      method: id ? "PUT" : "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    });
    if (!json.success) {
      showToast(json.error || t("notify.toast_save_failed"), "error");
      return;
    }
    const savedId = (json.data && json.data.id) || id;
    let defaultMoved = false;
    if (makeDefault && savedId) {
      const def = await apiJSON(`/api/v1/storage-targets/${encodeURIComponent(savedId)}/default`, { method: "POST" });
      if (!def.success) showToast(def.error || t("toasts.request_failed"), "error");
      defaultMoved = !!def.success;
    }
    showToast(t("storage.saved"), "success");
    // A target created from a dialog's "New" button is selected there once reloaded.
    const ret = storageReturn;
    storageReturn = null;
    closeModal("modal-storage");
    const loadError = await loadStorageTargets();
    if (loadError) {
      showToast(loadError, "error");
      rememberSavedTarget(json.data, defaultMoved);
    }
    renderStats();
    if (ret && ret.select) fillStorageSelect(ret.select, savedId);
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    submit.disabled = false;
  }
}

async function testStorageTarget(id, btn) {
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON(`/api/v1/storage-targets/${encodeURIComponent(id)}/test`, { method: "POST" });
    const data = json.success ? (json.data || {}) : { ok: false, error: json.error || "" };
    showToast(`${storageTargetName(id)}: ${describeStorageTest(data)}`, data.ok ? "success" : "error");
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    if (btn) btn.disabled = false;
    loadStorageTargets();
  }
}

async function setDefaultStorageTarget(id, btn) {
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON(`/api/v1/storage-targets/${encodeURIComponent(id)}/default`, { method: "POST" });
    if (json.success) {
      showToast(tf("storage.default_set", { name: storageTargetName(id) }), "success");
      await loadStorageTargets();
      renderStats();
    } else {
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    if (btn && document.contains(btn)) btn.disabled = false;
  }
}

async function deleteStorageTarget(id) {
  if (!(await confirmDialog({ title: t("dialog.delete_title"), body: tf("storage.confirm_delete", { name: storageTargetName(id) }), danger: true, confirmLabel: t("actions.delete") }))) return;
  try {
    const json = await apiJSON(`/api/v1/storage-targets/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (json.success) {
      showToast(t("storage.deleted"), "success");
      loadStorageTargets();
    } else {
      // 409: jobs or backup records still use the target; the server names them.
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

// ---------------------------------------------------------------------------
// Settings: users and API keys
// ---------------------------------------------------------------------------

async function loadUsers() {
  try {
    // Non-admins get only IDs and names, to show who created or pinned something.
    const json = await apiJSON(can("admin") ? "/api/v1/users" : "/api/v1/users/names");
    if (!noteLoad("users", json)) return;
    state.users = json.data || [];
    state.loaded.users = true;
    renderUsers();
  } catch (err) {
    console.error("Failed to load users:", err);
  }
}

async function loadApiKeys() {
  try {
    const json = await apiJSON("/api/v1/api-keys");
    if (!noteLoad("api_keys", json)) return;
    state.apikeys = json.data || [];
    state.loaded.apikeys = true;
    renderApiKeys();
  } catch (err) {
    console.error("Failed to load API keys:", err);
  }
}

let auditInFlight = false;

async function loadAudit() {
  if (auditInFlight || !auth.user || !can("admin")) return;
  auditInFlight = true;
  try {
    const json = await apiJSON("/api/v1/audit?limit=200");
    state.auditError = json.success ? "" : (json.error || t("toasts.request_failed"));
    if (json.success) state.audit = json.data || [];
    state.loaded.audit = true;
    renderAudit();
  } catch (err) {
    state.auditError = err.message;
    state.loaded.audit = true;
    renderAudit();
  } finally {
    auditInFlight = false;
  }
}

const AUDIT_RESULTS = {
  ok: "success",
  error: "danger",
  denied: "neutral",
  rate_limited: "neutral"
};

function auditResultCell(e) {
  const kind = AUDIT_RESULTS[e.result] || "neutral";
  const label = AUDIT_RESULTS[e.result] ? t(`settings.result_${e.result}`) : String(e.result || "");
  const detail = e.error ? errorDetail(e.error, truncate(e.error, 60)) : "";
  const count = Number(e.count) > 1 ? ` <span class="muted">×${Number(e.count)}</span>` : "";
  return `${statusBadge(kind, label)}${count}${detail}`;
}

function auditArguments(e) {
  let text = "";
  try {
    text = e.arguments && Object.keys(e.arguments).length ? JSON.stringify(e.arguments) : "";
  } catch (err) {
    text = "";
  }
  return text ? `<div class="cell-sub mono">${ellipsis(truncate(text, 120))}</div>` : "";
}

function renderAudit() {
  const tbody = document.getElementById("audit-tbody");
  if (!tbody || !state.loaded.audit) return;
  if (state.auditError) {
    setTbody(tbody, emptyRow(6, tf("settings.activity_load_failed", { error: state.auditError })));
    return;
  }
  if (state.audit.length === 0) {
    setTbody(tbody, emptyRow(6, t("settings.activity_empty")));
    return;
  }
  setTbody(tbody, state.audit.map(e => `<tr>
      <td>${timeCell(e.time)}</td>
      <td class="cell-primary"><span title="${escapeHtml(e.api_key_id || "")}">${escapeHtml(e.api_key_name || e.api_key_id || "")}</span></td>
      <td><span class="mono">${escapeHtml(e.tool || "")}</span>${auditArguments(e)}</td>
      <td>${auditResultCell(e)}</td>
      <td><span class="mono muted">${escapeHtml(e.transport || "")}</span></td>
      <td><span class="muted">${escapeHtml(Number(e.duration_ms) >= 1 ? formatDuration(Number(e.duration_ms) / 1000) : "<1 ms")}</span></td>
    </tr>`).join(""));
}

function userName(id) {
  const u = state.users.find(x => x.id === id || x.username === id);
  return u ? u.username : String(id || "");
}

function renderUsers() {
  const tbody = document.getElementById("users-tbody");
  if (!tbody || !state.loaded.users || !can("admin")) return;
  const onlyOne = state.users.length <= 1;
  const admins = state.users.filter(u => u.role === "admin").length;
  setTbody(tbody, state.users.map(u => {
    const self = auth.user && u.id === auth.user.id;
    const lastAdmin = u.role === "admin" && admins <= 1;
    const deleteTitle = self ? t("settings.cannot_delete_self") : onlyOne ? t("settings.cannot_delete_last")
      : lastAdmin ? t("settings.user_role_last_admin") : "";
    // Single sign-on users (sso.js) have no password, and their role follows the
    // group mappings while there are any.
    const external = u.auth_provider === "oidc";
    const badge = typeof ssoProviderBadge === "function" ? ssoProviderBadge(u) : "";
    const managed = typeof ssoRoleManaged === "function" && ssoRoleManaged(u);
    return `<tr>
      <td class="cell-primary"><span class="user-cell">${escapeHtml(u.username)}${self ? `<span class="chip">${escapeHtml(t("settings.you"))}</span>` : ""}${badge}</span></td>
      <td>${userRoleSelect(u, self, lastAdmin, managed)}</td>
      <td>${timeCell(u.created_at)}</td>
      <td>${parseDate(u.last_login_at) ? timeCell(u.last_login_at) : `<span class="muted">${escapeHtml(t("settings.never"))}</span>`}</td>
      <td class="col-actions"><div class="row-actions">
        ${external ? "" : `<button type="button" class="btn btn-secondary btn-sm" data-action="change-user-password" data-id="${escapeHtml(u.id)}">${escapeHtml(t("auth.change_password"))}</button>`}
        <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="delete-user" data-id="${escapeHtml(u.id)}"${deleteTitle ? ` disabled title="${escapeHtml(deleteTitle)}"` : ""}>${escapeHtml(t("actions.delete"))}</button>
      </div></td>
    </tr>`;
  }).join(""));
}

function apiKeyDisplay(k) {
  const prefix = String(k.prefix || "");
  return `${prefix.startsWith("mr_") ? prefix : `mr_${prefix}`}_…`;
}

const API_KEY_SCOPES = ["read", "operator", "admin"];

// Scope chip of an API key; unknown scopes (from a newer server) are shown as-is.
function scopeChip(scope) {
  const s = API_KEY_SCOPES.includes(scope) ? scope : "read";
  const label = API_KEY_SCOPES.includes(scope) ? t(`settings.scope_${s}`) : String(scope || "");
  const cls = s === "admin" ? "chip chip-accent" : "chip";
  return `<span class="${cls}" title="${escapeHtml(t("settings.key_scope_hint"))}">${escapeHtml(label)}</span>`;
}

function renderApiKeys() {
  const tbody = document.getElementById("apikeys-tbody");
  if (!tbody || !state.loaded.apikeys) return;
  if (state.apikeys.length === 0) {
    setTbody(tbody, emptyRow(7, t("settings.keys_empty"), "new-api-key", "plus", t("settings.create_key")));
    return;
  }
  setTbody(tbody, state.apikeys.map(k => `<tr>
      <td class="cell-primary">${escapeHtml(k.name)}</td>
      <td><span class="mono muted">${escapeHtml(apiKeyDisplay(k))}</span></td>
      <td>${scopeChip(k.scope)}${keyCapNote(k)}</td>
      <td>${k.created_by ? escapeHtml(userName(k.created_by)) : mutedDash()}</td>
      <td>${timeCell(k.created_at)}</td>
      <td>${parseDate(k.last_used_at) ? timeCell(k.last_used_at) : `<span class="muted">${escapeHtml(t("settings.never"))}</span>`}</td>
      <td class="col-actions"><div class="row-actions">
        <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="revoke-api-key" data-id="${escapeHtml(k.id)}">${escapeHtml(t("settings.revoke"))}</button>
      </div></td>
    </tr>`).join(""));
}

function openUserModal() {
  const form = document.getElementById("form-user");
  if (form) form.reset();
  // New users are viewers unless an administrator picks more.
  setValue("user-role", "viewer");
  hideFormError("user-error");
  openModal("modal-user");
}

async function saveUser(e) {
  e.preventDefault();
  const username = getValue("user-username");
  const password = document.getElementById("user-password").value;
  const error = !username
    ? t("auth.err_username_required")
    : validateNewPassword(password, document.getElementById("user-password-confirm").value);
  if (error) {
    showFormError("user-error", error);
    return;
  }
  const role = ROLE_SCOPES[getValue("user-role")] ? getValue("user-role") : "viewer";
  try {
    const json = await apiJSON("/api/v1/users", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password, role })
    });
    if (json.success) {
      showToast(t("settings.user_created"), "success");
      closeModal("modal-user");
      loadUsers();
    } else {
      showFormError("user-error", json.error || t("notify.toast_save_failed"));
    }
  } catch (err) {
    showFormError("user-error", err.message);
  }
}

async function deleteUser(id) {
  if (!(await confirmDialog({ title: t("dialog.delete_title"), body: tf("settings.confirm_delete_user", { name: userName(id) }), danger: true, confirmLabel: t("actions.delete") }))) return;
  try {
    const json = await apiJSON(`/api/v1/users/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (json.success) {
      showToast(t("settings.user_deleted"), "success");
      loadUsers();
    } else {
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

function openPasswordModal(userId) {
  toggleUserMenu(false);
  const id = userId || (auth.user && auth.user.id) || "";
  const self = auth.user && id === auth.user.id;
  const form = document.getElementById("form-password");
  if (form) form.reset();
  hideFormError("password-error");
  setValue("password-user-id", id);
  document.getElementById("password-current-group").hidden = !self;
  document.getElementById("password-current").required = !!self;
  setText("password-modal-title", self ? t("auth.change_password") : tf("settings.password_for", { name: userName(id) }));
  openModal("modal-password");
}

async function savePassword(e) {
  e.preventDefault();
  const id = getValue("password-user-id");
  const self = auth.user && id === auth.user.id;
  const current = document.getElementById("password-current").value;
  const next = document.getElementById("password-new").value;
  let error = self && !current ? t("auth.err_current_required") : "";
  if (!error) error = validateNewPassword(next, document.getElementById("password-confirm").value);
  if (error) {
    showFormError("password-error", error);
    return;
  }
  const body = { new_password: next };
  if (self) body.current_password = current;
  try {
    const json = await apiJSON(`/api/v1/users/${encodeURIComponent(id)}/password`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    });
    if (!json.success) {
      showFormError("password-error", json.error || t("notify.toast_save_failed"));
      return;
    }
    closeModal("modal-password");
    if (self && !(await loadMe())) {
      // Changing your own password may end every session, including this one.
      sessionExpiredShown = true;
      clearAuth();
      showLogin(t("auth.password_changed_relogin"));
      return;
    }
    showToast(t("auth.password_changed"), "success");
    loadUsers();
  } catch (err) {
    if (auth.user) showFormError("password-error", err.message);
  }
}

function openApiKeyModal() {
  const form = document.getElementById("form-api-key");
  if (form) form.reset();
  // A key never gets more than its creator may do: scopes above the caller's are
  // not offered (the server refuses them too).
  document.querySelectorAll("#api-key-scope option").forEach(opt => {
    const allowed = can(opt.value);
    opt.hidden = !allowed;
    opt.disabled = !allowed;
  });
  const hint = document.getElementById("api-key-scope-limit");
  if (hint) hint.hidden = can("admin");
  showApiKeyStep("name");
  openModal("modal-api-key");
}

function showApiKeyStep(step) {
  const secret = step === "secret";
  document.getElementById("api-key-step-name").hidden = secret;
  document.getElementById("api-key-footer-name").hidden = secret;
  document.getElementById("api-key-step-secret").hidden = !secret;
  document.getElementById("api-key-footer-secret").hidden = !secret;
  setText("api-key-modal-title", secret ? t("settings.key_created_title") : t("settings.create_key"));
  setText("api-key-copy-label", t("settings.copy"));
  if (!secret) setValue("api-key-secret", "");
}

async function createApiKey(e) {
  e.preventDefault();
  if (!document.getElementById("api-key-step-secret").hidden) return;
  const name = getValue("api-key-name");
  if (!name) {
    document.getElementById("api-key-name").focus();
    return;
  }
  const chosen = getValue("api-key-scope");
  const scope = API_KEY_SCOPES.includes(chosen) ? chosen : "read";
  try {
    const json = await apiJSON("/api/v1/api-keys", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, scope })
    });
    if (!json.success) {
      showToast(json.error || t("notify.toast_save_failed"), "error");
      return;
    }
    const data = json.data || {};
    setValue("api-key-secret", data.key || data.plaintext || data.token || "");
    showApiKeyStep("secret");
    // Focus the copy button rather than selecting the field, which would scroll the
    // key's prefix out of view.
    document.querySelector('[data-action="copy-api-key"]').focus();
    loadApiKeys();
  } catch (err) {
    showToast(err.message, "error");
  }
}

function copyApiKey() {
  return copyField("api-key-secret", "api-key-copy-label");
}

// Copies a read-only field (API key, generated private key) to the clipboard,
// falling back to select + execCommand outside secure contexts.
async function copyField(inputId, labelId) {
  const input = document.getElementById(inputId);
  const value = input ? input.value : "";
  if (!value) return;
  let copied = false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(value);
      copied = true;
    }
  } catch (err) {
    copied = false;
  }
  if (!copied) {
    input.focus();
    input.select();
    try {
      copied = document.execCommand("copy");
    } catch (err) {
      copied = false;
    }
  }
  if (copied) {
    setText(labelId, t("settings.copied"));
    setTimeout(() => setText(labelId, t("settings.copy")), 2000);
  } else {
    showToast(t("settings.copy_failed"), "error");
  }
}

async function revokeApiKey(id) {
  const key = state.apikeys.find(k => k.id === id);
  if (!(await confirmDialog({ body: tf("settings.confirm_revoke", { name: key ? key.name : id }), danger: true, confirmLabel: t("dialog.confirm") }))) return;
  try {
    const json = await apiJSON(`/api/v1/api-keys/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (json.success) {
      showToast(t("settings.key_revoked"), "success");
      loadApiKeys();
    } else {
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

// ---------------------------------------------------------------------------
// Modals: Escape / overlay click close, initial focus, focus trap, focus restore
// ---------------------------------------------------------------------------

const modalStack = [];

function setupModals() {
  document.querySelectorAll(".modal-backdrop").forEach(backdrop => {
    let pressedOnBackdrop = false;
    backdrop.addEventListener("mousedown", (e) => {
      pressedOnBackdrop = e.target === backdrop;
    });
    backdrop.addEventListener("click", (e) => {
      if (pressedOnBackdrop && e.target === backdrop) closeModal(backdrop.id);
      pressedOnBackdrop = false;
    });
  });

  document.addEventListener("keydown", (e) => {
    const top = modalStack[modalStack.length - 1];
    if (!top) return;
    if (e.key === "Escape") {
      e.preventDefault();
      closeModal(top.id);
    } else if (e.key === "Tab") {
      trapFocus(document.getElementById(top.id), e);
    }
  });
}

function focusableIn(root) {
  return Array.from(root.querySelectorAll(
    'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
  )).filter(el => el.offsetParent !== null);
}

function trapFocus(modal, e) {
  if (!modal) return;
  const items = focusableIn(modal);
  if (items.length === 0) return;
  const first = items[0];
  const last = items[items.length - 1];
  // Focus that escaped the dialog (a click on the backdrop, a removed element)
  // comes back to its edge.
  if (!modal.contains(document.activeElement)) {
    e.preventDefault();
    (e.shiftKey ? last : first).focus();
  } else if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
  }
}

function openModal(id) {
  const el = document.getElementById(id);
  if (!el || el.classList.contains("open")) return;
  modalStack.push({ id, opener: document.activeElement });
  el.classList.add("open");
  el.setAttribute("aria-hidden", "false");
  document.body.classList.add("modal-open");
  const body = el.querySelector(".modal-body") || el;
  const first = focusableIn(body)[0] || focusableIn(el)[0];
  if (first) first.focus();
}

function closeModal(id) {
  const el = document.getElementById(id);
  if (!el || !el.classList.contains("open") || modalLocked(id)) return;
  el.classList.remove("open");
  el.setAttribute("aria-hidden", "true");
  if (id === "modal-api-key") showApiKeyStep("name");
  if (id === "modal-storage") storageReturn = null;
  const idx = modalStack.findIndex(m => m.id === id);
  const entry = idx >= 0 ? modalStack.splice(idx, 1)[0] : null;
  if (modalStack.length === 0) document.body.classList.remove("modal-open");
  returnFocus(entry && entry.opener);
  // Settles confirmDialog() and ends bulk dialogs (bulk.js).
  onModalClosed(id);
}

// Gives the focus back to what opened a dialog. An opener that is gone or hidden
// (a closed menu's item, a re-rendered row) hands it to its menu button, the dialog
// below or the active tab, so keyboard users never land on <body>.
function returnFocus(opener) {
  const visible = (el) => el && typeof el.focus === "function" && document.contains(el) && el.offsetParent !== null;
  let target = visible(opener) ? opener : null;
  if (!target && opener && document.getElementById("user-menu-list") && document.getElementById("user-menu-list").contains(opener)) {
    target = document.getElementById("user-menu-btn");
  }
  if (!target && opener && document.getElementById("row-menu") && document.getElementById("row-menu").contains(opener)) {
    target = rowMenu.trigger && visible(rowMenu.trigger) ? rowMenu.trigger : null;
  }
  const top = modalStack[modalStack.length - 1];
  if (!target && top) {
    const below = document.getElementById(top.id);
    target = below ? focusableIn(below)[0] : null;
  }
  if (!target) target = document.querySelector(".tab-btn.active");
  if (target && visible(target)) target.focus();
}

// Shows a toast; opts.action ({label, run}) adds a button. The queue (stacking,
// dismiss, announcements) lives in nav.js; this fallback only runs without it.
function showToast(msg, type = "success", opts) {
  if (typeof toastShow === "function") {
    toastShow(msg, type, opts);
    return;
  }
  const container = document.getElementById("toast-container");
  if (!container) return;
  const toast = document.createElement("div");
  toast.className = `toast toast-${type}`;
  toast.setAttribute("role", type === "error" ? "alert" : "status");
  toast.textContent = msg;
  container.appendChild(toast);
  setTimeout(() => toast.remove(), type === "error" ? 6000 : 4000);
}

// ---------------------------------------------------------------------------
// Formatters
// ---------------------------------------------------------------------------

function uiLocale() {
  return typeof currentLang === "string" && currentLang ? currentLang : "en";
}

// Binary (IEC) units: 1 KiB = 1024 bytes. Every size in the dashboard goes through
// formatBytes, so tiles, tables and dialogs agree: three significant digits
// ("9.77 MiB", "97.7 MiB", "977 MiB"), whole bytes below 1 KiB, the number in the
// UI language's format.
const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

function formatBytes(value) {
  const bytes = Number(value);
  let i = 0;
  let n = Number.isFinite(bytes) && bytes > 0 ? bytes : 0;
  while (n >= 1024 && i < BYTE_UNITS.length - 1) {
    n /= 1024;
    i++;
  }
  let digits = i === 0 ? 0 : n < 10 ? 2 : n < 100 ? 1 : 0;
  // 1023.96 KiB would round to "1024 KiB": show "1.00 MiB" instead.
  if (i > 0 && i < BYTE_UNITS.length - 1 && Number(n.toFixed(digits)) >= 1024) {
    n /= 1024;
    i++;
    digits = 2;
  }
  let text;
  try {
    text = new Intl.NumberFormat(uiLocale(), { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(n);
  } catch (err) {
    text = n.toFixed(digits);
  }
  return `${text} ${BYTE_UNITS[i]}`;
}

function parseDate(value) {
  if (!value || String(value).startsWith("0001")) return null;
  const d = new Date(value);
  return isNaN(d.getTime()) ? null : d;
}

function formatAbsolute(d) {
  try {
    return new Intl.DateTimeFormat(uiLocale(), { dateStyle: "medium", timeStyle: "medium" }).format(d);
  } catch (err) {
    return d.toLocaleString();
  }
}

function formatRelative(d) {
  const diffSec = (d.getTime() - Date.now()) / 1000;
  const abs = Math.abs(diffSec);
  let rtf;
  try {
    rtf = new Intl.RelativeTimeFormat(uiLocale(), { numeric: "auto" });
  } catch (err) {
    return formatAbsolute(d);
  }
  if (abs < 10) return rtf.format(0, "second");
  if (abs < 60) return rtf.format(Math.round(diffSec), "second");
  if (abs < 3600) return rtf.format(Math.round(diffSec / 60), "minute");
  if (abs < 86400) return rtf.format(Math.round(diffSec / 3600), "hour");
  if (abs < 86400 * 30) return rtf.format(Math.round(diffSec / 86400), "day");
  if (abs < 86400 * 365) return rtf.format(Math.round(diffSec / (86400 * 30)), "month");
  return rtf.format(Math.round(diffSec / (86400 * 365)), "year");
}

// Compact span such as "2h 10m" (used for "next run in ...").
function formatSpan(ms) {
  const totalMin = Math.max(0, Math.round(ms / 60000));
  const days = Math.floor(totalMin / 1440);
  const hours = Math.floor((totalMin % 1440) / 60);
  const minutes = totalMin % 60;
  if (days > 0) return hours > 0 ? `${days}d ${hours}h` : `${days}d`;
  if (hours > 0) return minutes > 0 ? `${hours}h ${minutes}m` : `${hours}h`;
  return `${Math.max(minutes, 1)}m`;
}

function formatDuration(seconds) {
  const s = Number(seconds) || 0;
  if (s < 1) return `${Math.max(1, Math.round(s * 1000))} ms`;
  if (s < 60) return `${s.toFixed(1)} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

function escapeHtml(value) {
  if (value === null || value === undefined) return "";
  return String(value).replace(/[&<>'"]/g, tag => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    "'": "&#39;",
    '"': "&quot;"
  }[tag] || tag));
}
