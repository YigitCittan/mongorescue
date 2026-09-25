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

// Signed-in user, CSRF token for unsafe requests and how the session was authenticated.
const auth = { user: null, csrf: "", mode: "" };

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
const ACTIVE_POLL_MS = 3000;
const ACTIVE_STATUSES = ["pending", "in_progress"];

// Operations started from this browser, keyed by record ID, so their outcome can be
// reported once the background run finishes (the API answers 202 Accepted).
const trackedOps = { backups: new Map(), restores: new Map() };
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

  setupTabs();
  setupActions();
  setupModals();
  setupForms();
  setupUserMenu();

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

  const fromHash = `tab-${String(window.location.hash || "").replace(/^#/, "")}`;
  if (document.getElementById(fromHash) && fromHash !== "tab-") {
    activateTab(fromHash, false);
  }
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
  try {
    window.history.replaceState(null, "", `#${id.replace(/^tab-/, "")}`);
  } catch (err) {
    // history API unavailable (e.g. sandboxed frame): ignore
  }
}

// ---------------------------------------------------------------------------
// Delegated actions
// ---------------------------------------------------------------------------

function setupActions() {
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-action]");
    if (!btn || btn.disabled) return;
    const id = btn.dataset.id || "";
    switch (btn.dataset.action) {
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
      case "restore-backup":
        openRestoreModal(id, btn.dataset.db || "");
        break;
      case "delete-backup":
        deleteBackup(id);
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
  if (!dot) return;
  dot.dataset.status = status;
  let label = t("nav.conn_checking");
  if (status === "ok") label = version ? `${t("nav.conn_ok")} (v${version})` : t("nav.conn_ok");
  if (status === "down") label = t("nav.conn_down");
  dot.title = label;
  dot.setAttribute("aria-label", label);
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
      loadBackups(),
      loadRestores(),
      loadNotifications()
    ]);
  } finally {
    refreshInFlight = false;
  }
  if (!auth.user) return;
  renderStats();
  renderJobs();
  renderBackups();
  // Restores name their source connection via the backup records loaded alongside.
  renderRestores();
  renderRules();
  scheduleActivePoll();
}

// While any backup or restore is still running, refresh those lists every few
// seconds (only while the tab is visible) so completion shows up promptly.
function hasActiveOperations() {
  return state.backups.some(b => ACTIVE_STATUSES.includes(b.status)) ||
    state.restores.some(r => ACTIVE_STATUSES.includes(r.status)) ||
    trackedOps.backups.size > 0 || trackedOps.restores.size > 0;
}

function scheduleActivePoll() {
  if (activePollTimer || document.hidden || !auth.user || !hasActiveOperations()) return;
  activePollTimer = setTimeout(async () => {
    activePollTimer = null;
    if (document.hidden || !auth.user) return;
    await Promise.all([loadBackups(), loadRestores()]);
    loadStats().then(renderStats);
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
    const db = (kind === "backups" ? rec.database : rec.target_database) || info.database;
    if (rec.status === "failed") {
      const key = kind === "backups" ? "toasts.backup_failed_detail" : "toasts.restore_failed_detail";
      showToast(tf(key, { db, error: truncate(errorSummary(rec.error_message), 160) }), "error");
    } else {
      const key = kind === "backups" ? "toasts.backup_succeeded" : "toasts.restore_succeeded";
      showToast(tf(key, { db }), "success");
    }
  });
}

async function loadStats() {
  try {
    const json = await apiJSON("/api/v1/stats");
    if (json.success) state.stats = json.data || null;
  } catch (err) {
    console.error("Failed to load stats:", err);
  }
}

async function loadJobs() {
  try {
    const json = await apiJSON("/api/v1/jobs");
    if (!json.success) return;
    state.jobs = json.data || [];
    state.loaded.jobs = true;
    renderJobs();
  } catch (err) {
    console.error("Failed to load jobs:", err);
  }
}

async function loadBackups() {
  try {
    const json = await apiJSON("/api/v1/backups");
    if (!json.success) return;
    state.backups = sortByStartedDesc(json.data || []);
    state.loaded.backups = true;
    reportFinished("backups", state.backups);
    renderBackups();
    renderJobs();
  } catch (err) {
    console.error("Failed to load backups:", err);
  }
}

async function loadRestores() {
  try {
    const json = await apiJSON("/api/v1/restores");
    if (!json.success) return;
    state.restores = sortByStartedDesc(json.data || []);
    state.loaded.restores = true;
    reportFinished("restores", state.restores);
    renderRestores();
  } catch (err) {
    console.error("Failed to load restores:", err);
  }
}

async function loadNotifications() {
  try {
    const [cJson, rJson] = await Promise.all([
      apiJSON("/api/v1/notifications/channels"),
      apiJSON("/api/v1/notifications/rules")
    ]);
    if (cJson.success) {
      state.channels = cJson.data || [];
      state.loaded.channels = true;
      renderChannels();
    }
    if (rJson.success) {
      state.rules = rJson.data || [];
      state.loaded.rules = true;
      renderRules();
    }
  } catch (err) {
    console.error("Failed to load notifications:", err);
  }
}

function sortByStartedDesc(items) {
  return items.slice().sort((a, b) => {
    const da = parseDate(a.started_at);
    const db = parseDate(b.started_at);
    return (db ? db.getTime() : 0) - (da ? da.getTime() : 0);
  });
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

  // Backups (completed archives) + failures in the last 24h
  setText("stat-backups", state.stats ? String(stats.completed_backups || 0) : "—");
  const dayAgo = Date.now() - 24 * 3600 * 1000;
  const failed24h = state.backups.filter(b => {
    const d = parseDate(b.started_at);
    return b.status === "failed" && d && d.getTime() >= dayAgo;
  }).length;
  const backupsSub = document.getElementById("stat-backups-sub");
  if (backupsSub) {
    backupsSub.classList.toggle("text-danger", failed24h > 0);
    backupsSub.textContent = !state.loaded.backups
      ? ""
      : failed24h > 0
        ? tf("metrics.failed_24h", { n: failed24h })
        : state.backups.length > 0 ? t("metrics.no_failures_24h") : "";
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
  const last = state.backups[0];
  if (valueEl) {
    if (!last) {
      valueEl.textContent = "—";
      valueEl.title = "";
    } else {
      const d = parseDate(last.started_at);
      const [kind, label] = backupStatus(last.status);
      valueEl.innerHTML = `<span class="stat-time">${escapeHtml(d ? formatRelative(d) : "—")}</span>${statusBadge(kind, label, last.error_message)}`;
      valueEl.title = d ? formatAbsolute(d) : "";
    }
  }
  if (lastSub) {
    lastSub.textContent = !state.loaded.backups
      ? ""
      : last ? `${jobLabel(last.job_id)} · ${last.database}` : t("metrics.no_backups");
  }

  setText("count-backups", state.loaded.backups ? String(state.backups.length) : "");
  setText("count-jobs", state.loaded.jobs ? String(state.jobs.length) : "");
  setText("count-restores", state.loaded.restores ? String(state.restores.length) : "");
  setText("count-notifications", state.loaded.channels ? String(state.channels.length) : "");
}

function storageDescription(type) {
  const def = defaultStorageTarget();
  if (def) return `${def.name} · ${storageLocation(def)}`;
  return String(type || "");
}

// "connection · job" line under a backup's database name.
function backupOrigin(b) {
  const conn = b.connection_name || (b.connection_id ? connectionName(b.connection_id) : "");
  return conn ? `${conn} · ${jobLabel(b.job_id)}` : jobLabel(b.job_id);
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
      ? emptyRow(9, t("conn.jobs_need_connection"), "new-connection", "plus", t("conn.add"))
      : emptyRow(9, t("tables.empty_jobs"), "new-job", "plus", t("nav.new_job")));
    return;
  }

  setTbody(tbody, state.jobs.map(job => {
    const lastBackup = state.backups.find(b => b.job_id === job.id);
    let lastDot = "";
    if (lastBackup) {
      const [kind, label] = backupStatus(lastBackup.status);
      lastDot = statusMark(kind, label);
    }
    const enabled = job.enabled !== false;
    return `<tr>
      <td class="cell-primary">${ellipsis(job.name, "ell-md")}<div class="cell-sub mono">${ellipsis(job.id, "ell-md")}</div></td>
      <td>${job.connection_id ? ellipsis(connectionName(job.connection_id), "ell-sm") : mutedDash()}</td>
      <td>${ellipsis(job.database, "mono ell-sm")}${collectionScope(job)}</td>
      <td><span class="mono">${escapeHtml(job.cron_expression)}</span></td>
      <td>${escapeHtml(retentionText(job))}</td>
      <td>${enabled ? statusBadge("success", t("status.enabled")) : statusBadge("neutral", t("status.disabled"))}</td>
      <td>${lastDot}${timeCell(job.last_run)}</td>
      <td>${enabled ? timeCell(job.next_run) : mutedDash()}</td>
      <td class="col-actions"><div class="row-actions">
        <button type="button" class="btn btn-secondary btn-sm" data-action="trigger-job" data-id="${escapeHtml(job.id)}">${escapeHtml(t("actions.run_now"))}</button>
        <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="delete-job" data-id="${escapeHtml(job.id)}">${escapeHtml(t("actions.delete"))}</button>
      </div></td>
    </tr>`;
  }).join(""));
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
    default:
      return ["neutral", String(status || "")];
  }
}

function renderBackups() {
  const tbody = document.getElementById("backups-tbody");
  if (!tbody) return;
  if (!state.loaded.backups) return;

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
    return `<tr>
      <td><div class="id-cell">${ellipsis(b.id, "mono muted cell-id")}${lock}</div></td>
      <td>${ellipsis(b.database)}<div class="cell-sub">${ellipsis(backupOrigin(b))}</div></td>
      <td>${statusBadge(kind, label, b.error_message)}${errorLine}</td>
      <td>${timeCell(b.started_at)}</td>
      <td class="num">${durationCell(b)}</td>
      <td class="num">${size}${backupStorageCell(b)}</td>
      <td>${sha}</td>
      <td class="col-actions"><div class="row-actions">
        ${restoreBtn}
        <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="delete-backup" data-id="${escapeHtml(b.id)}">${escapeHtml(t("actions.delete"))}</button>
      </div></td>
    </tr>`;
  }).join(""));
}

function durationCell(item) {
  const secs = Number(item.duration_seconds);
  if (!secs && item.status !== "completed") return mutedDash();
  return escapeHtml(formatDuration(secs || 0));
}

function renderRestores() {
  const tbody = document.getElementById("restores-tbody");
  if (!tbody) return;
  if (!state.loaded.restores) return;

  if (state.restores.length === 0) {
    setTbody(tbody, emptyRow(6, t("tables.empty_restores"), "goto-tab", "", t("tables.go_backups"), "tab-backups", "btn-secondary"));
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
    const { source, target } = restoreConnections(r);
    // A restore into another server is marked so it cannot pass for a local one.
    const cross = target && target !== source
      ? `<div class="cell-sub cross-server" title="${escapeHtml(`${source || "?"} → ${target}`)}">${ellipsis(source || "?")}<span aria-hidden="true">→</span><span class="sr-only">${escapeHtml(t("tables.cross_server"))}:</span>${ellipsis(target)}</div>`
      : "";
    return `<tr>
      <td>${ellipsis(r.id, "mono muted cell-id")}</td>
      <td>${ellipsis(r.source_database)}${source ? `<div class="cell-sub">${ellipsis(source)}</div>` : ""}</td>
      <td>${ellipsis(r.target_database, "mono")}${cross}</td>
      <td><div class="status-line">${statusBadge(kind, label, r.error_message)}${chips.join("")}</div>${errorLine}</td>
      <td>${timeCell(r.started_at)}</td>
      <td class="num">${durationCell(r)}</td>
    </tr>`;
  }).join(""));
}

// Source and target connection names of a restore record. The source comes from
// the record's snapshot or its backup; the target defaults to the source.
function restoreConnections(r) {
  const backup = state.backups.find(b => b.id === r.backup_id) || {};
  const sourceId = r.source_connection_id || backup.connection_id || "";
  const source = r.source_connection_name || backup.connection_name || connectionName(sourceId);
  const targetId = r.target_connection_id || "";
  const target = r.target_connection_name || (targetId ? connectionName(targetId) : "");
  if (targetId && sourceId && targetId === sourceId) return { source, target: source };
  return { source, target: target || source };
}

function channelTypeLabel(type) {
  return CHANNEL_TYPES.includes(type) ? t(`notify.type_${type}`) : String(type || "");
}

function eventLabel(ev) {
  return t(`notify.events.${String(ev).replace(".", "_")}`, String(ev));
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

  setTbody(tbody, state.channels.map(ch => {
    const ld = ch.last_delivery;
    let delivery = `<span class="muted">${escapeHtml(t("notify.never"))}</span>`;
    if (ld) {
      const d = parseDate(ld.time);
      const kind = ld.success ? "success" : "danger";
      const label = ld.success ? t("notify.delivered") : t("notify.delivery_failed");
      const title = ld.success ? (d ? formatAbsolute(d) : "") : String(ld.error || "");
      delivery = `<span class="delivery${ld.success ? "" : " text-danger"}" title="${escapeHtml(title)}">${statusMark(kind, "")}${escapeHtml(label)}${d ? ` <span class="muted">· ${escapeHtml(formatRelative(d))}</span>` : ""}</span>`;
    }
    return `<tr>
      <td class="cell-primary">${escapeHtml(ch.name)}<div class="cell-sub mono">${escapeHtml(ch.id)}</div></td>
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
  lock: '<rect x="3.5" y="7" width="9" height="6.5" rx="1.5"/><path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2"/>'
};

function icon(name, extraClass) {
  const paths = ICON_PATHS[name];
  if (!paths) return "";
  const cls = extraClass ? `icon ${extraClass}` : "icon";
  const weight = name === "check" || name === "x" ? "2" : "1.5";
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
  return `<time datetime="${escapeHtml(d.toISOString())}" title="${escapeHtml(formatAbsolute(d))}">${escapeHtml(formatRelative(d))}</time>`;
}

// Replaces a table body only when its markup changed, so the periodic refresh does
// not reset scroll, selection or keyboard focus inside unchanged tables.
const renderedTbodies = new WeakMap();

function setTbody(tbody, html) {
  if (renderedTbodies.get(tbody) === html) return;
  tbody.innerHTML = html;
  renderedTbodies.set(tbody, html);
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
    const source = pickerValue("job");
    if (!source) return;
    const payload = {
      name: getValue("job-name"),
      ...source,
      cron_expression: getValue("job-cron"),
      retention_days: parseInt(getValue("job-retention-days"), 10) || 0,
      retention_count: parseInt(getValue("job-retention-count"), 10) || 0,
      ...storageSelection("job-storage"),
      enabled: true
    };
    try {
      const json = await apiJSON("/api/v1/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload)
      });
      if (json.success) {
        showToast(t("toasts.job_created"), "success");
        closeModal("modal-new-job");
        refreshAll();
      } else {
        showToast(json.error || t("toasts.job_failed"), "error");
      }
    } catch (err) {
      showToast(err.message, "error");
    }
  });

  document.getElementById("form-instant-backup").addEventListener("submit", async (e) => {
    e.preventDefault();
    const source = pickerValue("instant");
    if (!source) return;
    try {
      closeModal("modal-instant-backup");
      const json = await apiJSON("/api/v1/backups", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...source, ...storageSelection("instant-storage") })
      });
      if (json.success) {
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

  document.getElementById("form-restore").addEventListener("submit", async (e) => {
    e.preventDefault();
    const backupID = getValue("restore-backup-id");
    const isSafeClone = document.getElementById("restore-safe-clone").checked;
    const targetDB = getValue("restore-target-db");
    const dryRun = document.getElementById("restore-dry-run").checked;
    const dropTarget = document.getElementById("restore-drop-target").checked;
    const verify = document.getElementById("restore-verify").checked;
    const targetConnection = document.getElementById("restore-target-connection").value;

    // The API defaults to a safe clone; writing into a named database needs explicit consent.
    const confirmInPlace = !isSafeClone && document.getElementById("restore-confirm-in-place").checked;
    if (!isSafeClone && !confirmInPlace) {
      document.getElementById("restore-confirm-in-place").focus();
      return;
    }
    if (!isSafeClone && dropTarget && !window.confirm(t("modal_restore.drop_confirm"))) {
      return;
    }

    try {
      closeModal("modal-restore");
      const json = await apiJSON("/api/v1/restore", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          backup_id: backupID,
          safe_clone: isSafeClone,
          confirm_in_place: confirmInPlace,
          target_database: isSafeClone ? "" : targetDB,
          dry_run: dryRun,
          drop_target: dropTarget,
          verify: verify,
          ...(targetConnection ? { target_connection_id: targetConnection } : {})
        })
      });
      if (json.success) {
        const target = json.data && json.data.target_database ? json.data.target_database : "";
        showToast(tf("toasts.restore_started", { db: target }), "info");
        trackRestore(json.data);
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

function openJobModal() {
  const form = document.getElementById("form-new-job");
  if (form) form.reset();
  // New jobs start from the retention defaults configured under Settings > General.
  const general = settingsGroup("general");
  if (general.default_retention_days !== undefined) setValue("job-retention-days", general.default_retention_days);
  if (general.default_retention_count !== undefined) setValue("job-retention-count", general.default_retention_count);
  resetPicker("job", "");
  fillStorageSelect(document.getElementById("job-storage"));
  openModal("modal-new-job");
}

function openBackupNowModal() {
  const form = document.getElementById("form-instant-backup");
  if (form) form.reset();
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
  // Safe clone is on by default; the verify policy decides the initial checkbox.
  document.getElementById("restore-verify").checked = verifyDefault(true);
  // Target server defaults to the one the backup came from.
  const backup = state.backups.find(b => b.id === backupID) || {};
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
  updateRestoreMode(false);
  openModal("modal-restore");
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
  const acked = document.getElementById("restore-confirm-in-place").checked;
  document.getElementById("restore-submit").disabled = !isSafeClone && !acked;
}

async function triggerJob(jobID, btn) {
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}/run`, { method: "POST" });
    if (json.success) {
      showToast(t("toasts.job_started"), "info");
      trackBackup(json.data);
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
  if (!window.confirm(t("toasts.confirm_delete_job"))) return;
  try {
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}`, { method: "DELETE" });
    if (json.success) {
      showToast(t("toasts.job_deleted"), "success");
      refreshAll();
    } else {
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
}

async function deleteBackup(backupID) {
  if (!window.confirm(t("toasts.confirm_delete_backup"))) return;
  try {
    const json = await apiJSON(`/api/v1/backups/${encodeURIComponent(backupID)}`, { method: "DELETE" });
    if (json.success) {
      showToast(t("toasts.backup_deleted"), "success");
      refreshAll();
    } else {
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
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
  if (!window.confirm(t("notify.confirm_delete_channel"))) return;
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
  if (!window.confirm(t("notify.confirm_delete_rule"))) return;
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
  renderUserMenu();
}

function clearAuth() {
  auth.user = null;
  auth.csrf = "";
  auth.mode = "";
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
  resetSettingsForms();
  trackedOps.backups.clear();
  trackedOps.restores.clear();
  document.querySelectorAll("#app-main tbody").forEach(tbody => {
    const cols = tbody.closest("table").querySelectorAll("thead th").length || 1;
    renderedTbodies.delete(tbody);
    tbody.innerHTML = `<tr class="empty-row"><td colspan="${cols}"><div class="empty-state"><p>${escapeHtml(t("tables.loading"))}</p></div></td></tr>`;
  });
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
  try {
    await apiFetch("/api/v1/auth/logout", { method: "POST" });
  } catch (err) {
    // The session is discarded locally either way.
  }
  sessionExpiredShown = true;
  clearAuth();
  showLogin(t("auth.signed_out"));
}

function renderUserMenu() {
  const name = auth.user ? auth.user.username || "" : "";
  setText("user-menu-name", name);
  setText("user-menu-fullname", name);
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
    if (!json.success) return;
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

  setTbody(tbody, state.connections.map(c => {
    const errorLine = parseDate(c.last_test_at) && !c.last_test_ok && c.last_test_error
      ? errorDetail(c.last_test_error, truncate(errorSummary(c.last_test_error), 70))
      : "";
    const desc = c.description ? `<div class="cell-sub">${ellipsis(c.description)}</div>` : "";
    return `<tr>
      <td class="cell-primary">${ellipsis(c.name)}${desc}</td>
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
    document.getElementById("connection-uri").focus();
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
  if (!window.confirm(tf("conn.confirm_delete", { name: connectionName(id) }))) return;
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
  const p = { prefix, manual: false, dbs: null, dbSeq: 0, collSeq: 0, dbError: "" };
  pickers[prefix] = p;
  pickerEl(p, "connection").addEventListener("change", () => loadPickerDatabases(p));
  pickerEl(p, "database-select").addEventListener("change", (e) => {
    if (e.target.value === PICKER_MANUAL) {
      setPickerManual(p, true);
      pickerEl(p, "database").focus();
    } else {
      loadPickerCollections(p);
    }
  });
  pickerEl(p, "database").addEventListener("change", () => loadPickerCollections(p));
  document.querySelectorAll(`input[name="${prefix}-coll-mode"]`).forEach(radio => {
    radio.addEventListener("change", () => updatePickerCollectionsBox(p));
  });
}

function resetPicker(prefix, connectionId) {
  const p = pickers[prefix];
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
  } catch (err) {
    if (seq !== p.dbSeq) return;
    select.disabled = false;
    p.dbError = tf("picker.dbs_failed", { error: truncate(errorSummary(err.message), 120) || "?" });
    setPickerManual(p, true);
  }
}

function pickerDatabase(p) {
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
  } catch (err) {
    if (seq !== p.collSeq) return;
    pickerEl(p, "collections").hidden = true;
    pickerEl(p, "collections-manual").hidden = false;
    setText(`${p.prefix}-collections-hint`, t("picker.colls_failed"));
  }
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
  const names = manualInput.hidden
    ? selectedValues(`${prefix}-collections`)
    : parseList(manualInput.value);
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

const SETTINGS_SECTIONS = ["general", "storage", "encryption", "security", "users", "apikeys"];

function showSettingsSection(name, focus) {
  if (!SETTINGS_SECTIONS.includes(name)) return;
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
    if (!json.success) {
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
}

function fillGeneral(g) {
  setValue("set-retention-days", g.default_retention_days ?? 0);
  setValue("set-retention-count", g.default_retention_count ?? 0);
  document.getElementById("set-gzip").checked = g.default_gzip !== false;
  setValue("set-backup-timeout", g.backup_timeout || "");
  setValue("set-backup-stall-timeout", g.backup_stall_timeout || "");
  setValue("set-restore-timeout", g.restore_timeout || "");
  setValue("set-verify-policy", ["always", "auto", "never"].includes(g.restore_verify_policy) ? g.restore_verify_policy : "auto");
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
    restore_verify_policy: getValue("set-verify-policy")
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
    mcp_enabled: document.getElementById("set-mcp-enabled").checked
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
    if (before.enabled && !payload.enabled && !window.confirm(t("enc.confirm_disable"))) return false;
    if (payload.enabled && payload.mode === "x25519" && !payload.identity && !window.confirm(t("enc.confirm_no_identity"))) return false;
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
    showSaved(group, data.restart_required);
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
  if (generatedKey.identity && !window.confirm(t("enc.confirm_replace_generated"))) return;
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

function discardGeneratedKey(ask) {
  if (ask && generatedKey.identity && !window.confirm(t("enc.confirm_discard"))) return;
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

async function loadStorageTargets() {
  try {
    const json = await apiJSON("/api/v1/storage-targets");
    if (!json.success) return;
    state.storageTargets = json.data || [];
    state.loaded.storageTargets = true;
    renderStorageTargets();
  } catch (err) {
    console.error("Failed to load storage targets:", err);
  }
}

function defaultStorageTarget() {
  return state.storageTargets.find(s => s.is_default) || null;
}

function storageTargetName(id) {
  const s = id ? state.storageTargets.find(x => x.id === id) : null;
  return s ? s.name : "";
}

function inferProvider(target) {
  if (!target || target.type === "local") return "local";
  const endpoint = String((target.s3 && target.s3.endpoint) || "").toLowerCase();
  if (!endpoint || endpoint.includes("amazonaws.com")) return "aws";
  if (endpoint.includes("r2.cloudflarestorage.com")) return "r2";
  if (endpoint.includes("backblazeb2.com")) return "b2";
  if (endpoint.includes("digitaloceanspaces.com")) return "do";
  if (endpoint.includes("wasabisys.com")) return "wasabi";
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

// Storage target select of the job form and "Back up now"; the default is preselected.
function fillStorageSelect(select) {
  if (!select) return;
  select.textContent = "";
  if (state.storageTargets.length === 0) {
    const opt = document.createElement("option");
    opt.value = "";
    opt.textContent = t("storage.default_target");
    select.appendChild(opt);
    return;
  }
  state.storageTargets.forEach(s => {
    const opt = document.createElement("option");
    opt.value = s.id;
    opt.textContent = s.is_default ? tf("storage.default_named", { name: s.name }) : s.name;
    select.appendChild(opt);
  });
  const def = defaultStorageTarget();
  select.value = def ? def.id : state.storageTargets[0].id;
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
    if (makeDefault && savedId) {
      const def = await apiJSON(`/api/v1/storage-targets/${encodeURIComponent(savedId)}/default`, { method: "POST" });
      if (!def.success) showToast(def.error || t("toasts.request_failed"), "error");
    }
    showToast(t("storage.saved"), "success");
    closeModal("modal-storage");
    await loadStorageTargets();
    renderStats();
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
  if (!window.confirm(tf("storage.confirm_delete", { name: storageTargetName(id) }))) return;
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
    const json = await apiJSON("/api/v1/users");
    if (!json.success) return;
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
    if (!json.success) return;
    state.apikeys = json.data || [];
    state.loaded.apikeys = true;
    renderApiKeys();
  } catch (err) {
    console.error("Failed to load API keys:", err);
  }
}

let auditInFlight = false;

async function loadAudit() {
  if (auditInFlight || !auth.user) return;
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
  return `${statusBadge(kind, label)}${detail}`;
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
  if (!tbody || !state.loaded.users) return;
  const onlyOne = state.users.length <= 1;
  setTbody(tbody, state.users.map(u => {
    const self = auth.user && u.id === auth.user.id;
    const deleteTitle = self ? t("settings.cannot_delete_self") : onlyOne ? t("settings.cannot_delete_last") : "";
    return `<tr>
      <td class="cell-primary"><span class="user-cell">${escapeHtml(u.username)}${self ? `<span class="chip">${escapeHtml(t("settings.you"))}</span>` : ""}</span></td>
      <td>${timeCell(u.created_at)}</td>
      <td>${parseDate(u.last_login_at) ? timeCell(u.last_login_at) : `<span class="muted">${escapeHtml(t("settings.never"))}</span>`}</td>
      <td class="col-actions"><div class="row-actions">
        <button type="button" class="btn btn-secondary btn-sm" data-action="change-user-password" data-id="${escapeHtml(u.id)}">${escapeHtml(t("auth.change_password"))}</button>
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
    setTbody(tbody, emptyRow(7, t("settings.keys_empty"), "new-api-key", "plus", t("settings.create_key"), "", "btn-secondary"));
    return;
  }
  setTbody(tbody, state.apikeys.map(k => `<tr>
      <td class="cell-primary">${escapeHtml(k.name)}</td>
      <td><span class="mono muted">${escapeHtml(apiKeyDisplay(k))}</span></td>
      <td>${scopeChip(k.scope)}</td>
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
  try {
    const json = await apiJSON("/api/v1/users", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password })
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
  if (!window.confirm(tf("settings.confirm_delete_user", { name: userName(id) }))) return;
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
  if (!window.confirm(tf("settings.confirm_revoke", { name: key ? key.name : id }))) return;
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
  if (e.shiftKey && document.activeElement === first) {
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
  if (!el || !el.classList.contains("open")) return;
  el.classList.remove("open");
  el.setAttribute("aria-hidden", "true");
  if (id === "modal-api-key") showApiKeyStep("name");
  const idx = modalStack.findIndex(m => m.id === id);
  const entry = idx >= 0 ? modalStack.splice(idx, 1)[0] : null;
  if (modalStack.length === 0) document.body.classList.remove("modal-open");
  if (entry && entry.opener && typeof entry.opener.focus === "function" && document.contains(entry.opener)) {
    entry.opener.focus();
  }
}

function showToast(msg, type = "success") {
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

function formatBytes(value) {
  const bytes = Number(value);
  if (!bytes || bytes <= 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(k)), sizes.length - 1);
  const n = bytes / Math.pow(k, i);
  return `${n.toFixed(i === 0 ? 0 : n < 10 ? 2 : 1)} ${sizes[i]}`;
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
