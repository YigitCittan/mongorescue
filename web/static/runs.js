/**
 * MongoRescue dashboard: run control.
 *
 * Cancelling running backups and restores, their live progress, per-run logs with a
 * phase timeline, the restore details dialog and pausing jobs. Loaded after app.js,
 * whose helpers (t, tf, apiJSON, apiFetch, escapeHtml, showToast, ...) it uses; app.js
 * calls the hooks below. The same security invariants apply: server values reach
 * innerHTML only through escapeHtml(), logs are set through textContent, IDs in URLs
 * are encoded and actions go through delegated data-action handlers.
 */

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// Live progress of the active runs (GET /api/v1/runs/active), keyed by record ID.
const runProgressById = new Map();
// Runs whose cancellation was requested from this browser.
const cancellingRuns = new Set();
// Restore shown in the restore details dialog.
let detailsRestoreId = "";
// Log viewer of the open details dialog.
const runLog = { prefix: "", kind: "", id: "", timer: null, seq: 0, loaded: false };
// Job shown in the pause dialog.
let pausingJobId = "";

// How often the log of a running run is re-read while "Follow" is on.
const RUN_LOG_POLL_MS = 2000;
// Lines the log viewer shows (the whole file is available as a download).
const RUN_LOG_TAIL_LINES = 1000;

// ---------------------------------------------------------------------------
// Live progress
// ---------------------------------------------------------------------------

// Loads the progress of every active run; app.js calls it while runs are active.
async function loadActiveRuns() {
  try {
    const json = await apiJSON("/api/v1/runs/active");
    if (!json.success) return;
    runProgressById.clear();
    (json.data || []).forEach(p => {
      if (p && p.id) runProgressById.set(p.id, p);
    });
  } catch (err) {
    console.error("Failed to load active runs:", err);
  }
}

// Progress of record rec while it runs: embedded in the list item, or from the last
// GET /api/v1/runs/active.
function runProgressOf(rec) {
  if (!rec || rec.status !== "in_progress") return null;
  return rec.progress || runProgressById.get(rec.id) || null;
}

function runPhaseLabel(phase) {
  const known = ["queued", "dumping", "verifying", "restoring", "finishing", "cancelling"];
  return known.includes(phase) ? t(`run.phase_${phase}`) : String(phase || "");
}

// "45% · 12.3 MB · 1,234 docs · shop.orders · 3/10 collections · 5.2 MB/s".
function runProgressText(p) {
  const parts = [];
  if (p.percent !== undefined && p.percent !== null) parts.push(`${Number(p.percent).toFixed(1)}%`);
  if (Number(p.bytes) > 0) {
    parts.push(Number(p.total_bytes) > 0 ? `${formatBytes(p.bytes)} / ${formatBytes(p.total_bytes)}` : formatBytes(p.bytes));
  }
  if (Number(p.documents) > 0) parts.push(tf("run.docs_n", { n: Number(p.documents).toLocaleString(uiLocale()) }));
  if (p.current_collection) parts.push(p.current_collection);
  if (Number(p.collections_total) > 0) {
    parts.push(tf("run.collections_n", { done: p.collections_done || 0, total: p.collections_total }));
  }
  if (Number(p.bytes_per_second) > 0) parts.push(`${formatBytes(p.bytes_per_second)}/s`);
  return parts.join(" · ");
}

// Progress bar and summary line of a running record, for a table cell or a dialog.
function runProgressHtml(rec) {
  const p = runProgressOf(rec);
  if (!p) return "";
  const pct = p.percent !== undefined && p.percent !== null ? Math.max(0, Math.min(100, Number(p.percent))) : null;
  const phase = p.cancelling || cancellingRuns.has(rec.id) ? t("run.phase_cancelling") : runPhaseLabel(p.phase);
  const label = pct === null ? phase : `${phase} ${pct.toFixed(1)}%`;
  // A native <progress> needs no inline style (the CSP forbids it); without a value
  // it is indeterminate.
  const bar = pct === null
    ? `<progress class="run-progress" max="100" aria-label="${escapeHtml(label)}"></progress>`
    : `<progress class="run-progress" max="100" value="${pct.toFixed(1)}" aria-label="${escapeHtml(label)}">${escapeHtml(label)}</progress>`;
  const text = runProgressText(p);
  return `<div class="run-progress-wrap">${bar}<div class="cell-sub run-progress-text" title="${escapeHtml(text)}">${escapeHtml(phase)}${text ? ` · ${escapeHtml(text)}` : ""}</div></div>`;
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

// "Cancel" button of a running backup or restore ("" for other records).
function cancelRunButton(kind, rec) {
  if (!rec || rec.status !== "in_progress") return "";
  const busy = cancellingRuns.has(rec.id) || (runProgressOf(rec) || {}).cancelling;
  const title = escapeHtml(t("run.cancel"));
  return `<button type="button" class="btn btn-secondary btn-sm btn-danger-text" data-action="cancel-run" data-kind="${escapeHtml(kind)}" data-id="${escapeHtml(rec.id)}" title="${title}"${busy ? " disabled" : ""}>${escapeHtml(busy ? t("run.cancelling") : t("run.cancel_short"))}</button>`;
}

// Cancels a running backup or restore after confirmation. A cancelled in-place
// restore may leave its target partially restored, which the confirmation says.
async function cancelRun(kind, id) {
  if (!id || cancellingRuns.has(id)) return;
  let question = t("run.confirm_cancel_backup");
  if (kind === "restore") {
    const r = state.restores.find(x => x.id === id) || {};
    question = r.in_place && !r.dry_run
      ? tf("run.confirm_cancel_in_place", { db: r.target_database || "" })
      : t("run.confirm_cancel_restore");
  }
  if (!window.confirm(question)) return;
  cancellingRuns.add(id);
  rerenderRuns();
  const path = kind === "restore" ? "restores" : "backups";
  try {
    const json = await apiJSON(`/api/v1/${path}/${encodeURIComponent(id)}/cancel`, { method: "POST" });
    if (json.success) {
      showToast(t("run.cancel_requested"), "info");
    } else {
      cancellingRuns.delete(id);
      showToast(json.error || t("run.cancel_failed"), "error");
    }
  } catch (err) {
    cancellingRuns.delete(id);
    showToast(err.message, "error");
  } finally {
    refreshAll();
  }
}

// The backup id from the loaded page or the backup cache of app.js, or undefined.
function findBackupRecord(id) {
  return state.backups.find(b => b.id === id) || backupCache.get(id);
}

// Forgets cancellations of runs that are no longer running (records off the loaded
// page are kept until they show up finished or their run is no longer active).
function pruneCancellingRuns() {
  cancellingRuns.forEach(id => {
    const rec = findBackupRecord(id) || state.restores.find(r => r.id === id);
    if (rec ? rec.status !== "in_progress" : !runProgressById.has(id)) cancellingRuns.delete(id);
  });
}

// Re-renders everything that shows run state.
function rerenderRuns() {
  renderBackups();
  renderRestores();
  renderJobs();
}

// ---------------------------------------------------------------------------
// Jobs: pause and resume
// ---------------------------------------------------------------------------

// The running backup of job jobID, if any: from the active runs (the backup may not
// be on the loaded page), else from the page.
function jobActiveRun(jobID) {
  for (const p of runProgressById.values()) {
    if (p.kind === "backup" && p.job_id === jobID) return { id: p.id, job_id: jobID, status: "in_progress" };
  }
  return state.backups.find(b => b.job_id === jobID && b.status === "in_progress") || null;
}

// State badge of a job: enabled, or paused (until a time, when set).
function jobStateBadge(job) {
  if (job.enabled !== false) return statusBadge("success", t("status.enabled"));
  const until = parseDate(job.paused_until);
  const title = until ? tf("run.paused_until", { time: formatAbsolute(until) }) : t("run.paused_indefinitely");
  const label = until ? tf("run.paused_until_short", { time: formatRelative(until) }) : t("run.paused");
  return statusBadge("neutral", label, title);
}

// Extra row menu items of a job: pause or resume, and stopping its current run.
function jobRunMenuItems(jobID) {
  const job = state.jobs.find(j => j.id === jobID);
  if (!job) return [];
  const items = [job.enabled === false ? ["resume-job", t("run.resume")] : ["pause-job", t("run.pause")]];
  if (jobActiveRun(jobID)) items.push(["stop-job-run", t("run.stop_current"), true]);
  return items;
}

// Row menu item of a running backup.
function backupRunMenuItems(backupID) {
  const b = findBackupRecord(backupID);
  return b && b.status === "in_progress" ? [["cancel-run-backup", t("run.cancel"), true]] : [];
}

function openPauseJob(jobID) {
  const job = state.jobs.find(j => j.id === jobID);
  if (!job) return;
  pausingJobId = jobID;
  setText("pause-job-name", job.name || job.id);
  const indefinite = document.getElementById("pause-job-indefinite");
  if (indefinite) indefinite.checked = true;
  const input = document.getElementById("pause-job-until");
  if (input) {
    const next = new Date(Date.now() + 24 * 3600 * 1000);
    next.setMinutes(0, 0, 0);
    input.value = toLocalInputValue(next);
    input.min = toLocalInputValue(new Date());
  }
  hideFormError("pause-job-error");
  openModal("modal-pause-job");
}

// Value of a datetime-local input for d, in local time.
function toLocalInputValue(d) {
  const pad = v => String(v).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

async function submitPauseJob(e) {
  e.preventDefault();
  const timed = document.getElementById("pause-job-timed");
  let until = null;
  if (timed && timed.checked) {
    const raw = getValue("pause-job-until");
    const d = raw ? new Date(raw) : null;
    if (!d || Number.isNaN(d.getTime()) || d.getTime() <= Date.now()) {
      showFormError("pause-job-error", t("run.pause_until_future"));
      return;
    }
    until = d.toISOString();
  }
  const ok = await setJobPaused(pausingJobId, true, until);
  if (ok) closeModal("modal-pause-job");
}

// Pauses (optionally until a time) or resumes job jobID. Every other field is sent
// back as stored, with its updated_at as precondition (like the enable toggle).
async function setJobPaused(jobID, paused, until) {
  if (!jobID || togglingJobs.has(jobID)) return false;
  togglingJobs.add(jobID);
  renderJobDetails();
  try {
    const fresh = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}`);
    if (!fresh.success || !fresh.data) {
      showToast(fresh.error || t("job_edit.update_failed"), "error");
      return false;
    }
    const job = fresh.data;
    const body = {
      name: job.name || "",
      cron_expression: job.cron_expression || "",
      database: job.database || "",
      collections: job.collections || [],
      exclude_collections: job.exclude_collections || [],
      connection_id: job.connection_id || "",
      storage_target_id: job.storage_target_id || "",
      enabled: !paused,
      updated_at: job.updated_at
    };
    if (paused && until) body.paused_until = until;
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    });
    if (json.success) {
      showToast(paused ? t("run.paused_toast") : t("run.resumed_toast"), "success");
      return true;
    }
    if (json.httpStatus === 409) {
      showToast(t("job_edit.conflict"), "error");
    } else if (paused) {
      showFormError("pause-job-error", json.error || t("job_edit.update_failed"));
    } else {
      showToast(json.error || t("job_edit.update_failed"), "error");
    }
    return false;
  } catch (err) {
    showToast(err.message, "error");
    return false;
  } finally {
    togglingJobs.delete(jobID);
    refreshAll();
  }
}

// Cancels the running backup of job jobID.
function stopJobRun(jobID) {
  const run = jobActiveRun(jobID);
  if (run) cancelRun("backup", run.id);
}

// ---------------------------------------------------------------------------
// Delegated actions (called by app.js for actions it does not handle)
// ---------------------------------------------------------------------------

function handleRunAction(action, id, btn) {
  switch (action) {
    case "cancel-run":
      cancelRun(btn.dataset.kind || "backup", id);
      return true;
    case "cancel-run-backup":
      cancelRun("backup", id);
      return true;
    case "pause-job":
      openPauseJob(id);
      return true;
    case "resume-job":
      setJobPaused(id, false, null);
      return true;
    case "stop-job-run":
      stopJobRun(id);
      return true;
    case "restore-details":
      openRestoreDetails(id);
      return true;
    case "run-tab":
      showRunTab(btn.dataset.prefix || "", btn.dataset.tab || "info");
      return true;
    case "run-log-refresh":
      loadRunLog(true);
      return true;
  }
  return false;
}

function setupRunControls() {
  const form = document.getElementById("form-pause-job");
  if (form) form.addEventListener("submit", submitPauseJob);
  const until = document.getElementById("pause-job-until");
  if (until) {
    until.addEventListener("focus", () => {
      const timed = document.getElementById("pause-job-timed");
      if (timed) timed.checked = true;
    });
  }
  ["backup", "restore"].forEach(prefix => {
    const follow = document.getElementById(`${prefix}-log-follow`);
    if (follow) follow.addEventListener("change", () => scheduleRunLog());
  });
}

// ---------------------------------------------------------------------------
// Details dialogs: tabs, phase timeline and log viewer
// ---------------------------------------------------------------------------

// Shows the "info" or "log" tab of the details dialog prefix ("backup", "restore").
function showRunTab(prefix, tab) {
  ["info", "log"].forEach(name => {
    const btn = document.getElementById(`${prefix}-tab-${name}`);
    const panel = document.getElementById(`${prefix}-panel-${name}`);
    const active = name === tab;
    if (btn) {
      btn.classList.toggle("active", active);
      btn.setAttribute("aria-selected", String(active));
      btn.tabIndex = active ? 0 : -1;
    }
    if (panel) panel.hidden = !active;
  });
  if (tab === "log") {
    loadRunLog(true);
  } else {
    stopRunLog();
  }
}

// Phase rows: [field, label key].
const RUN_PHASES = [
  ["queued", "run.ph_queued"],
  ["started", "run.ph_started"],
  ["dump_done", "run.ph_dump_done"],
  ["upload_done", "run.ph_upload_done"],
  ["verify_done", "run.ph_verify_done"],
  ["restore_done", "run.ph_restore_done"],
  ["finished", "run.ph_finished"]
];

// Fills the phase timeline element listId from the record's phases (live while it
// runs), with the time each phase took after the previous one.
function renderRunTimeline(listId, rec) {
  const list = document.getElementById(listId);
  if (!list) return;
  list.textContent = "";
  const p = runProgressOf(rec);
  const phases = (p && p.phases) || rec.phases || {};
  let prev = null;
  RUN_PHASES.forEach(([field, key]) => {
    const d = parseDate(phases[field]);
    if (!d) return;
    const li = document.createElement("li");
    const label = document.createElement("span");
    label.className = "timeline-label";
    label.textContent = t(key);
    const time = document.createElement("time");
    time.dateTime = d.toISOString();
    time.title = formatRelative(d);
    time.textContent = formatAbsolute(d);
    li.append(label, time);
    if (prev) {
      const delta = document.createElement("span");
      delta.className = "muted timeline-delta";
      delta.textContent = `+${formatDuration(Math.max(0, (d.getTime() - prev.getTime()) / 1000))}`;
      li.appendChild(delta);
    }
    prev = d;
    list.appendChild(li);
  });
  const section = list.closest(".detail-section");
  if (section) section.hidden = list.children.length === 0;
}

// Updates the run parts of a details dialog: progress, cancel button, timeline and
// the log viewer's follow toggle and download link.
function renderRunPanel(prefix, kind, rec) {
  const progress = document.getElementById(`${prefix}-details-progress`);
  if (progress) {
    const html = runProgressHtml(rec);
    progress.hidden = html === "";
    if (progress.dataset.html !== html) {
      progress.innerHTML = html;
      progress.dataset.html = html;
    }
  }
  const cancel = document.getElementById(`${prefix}-details-cancel`);
  if (cancel) {
    const running = rec.status === "in_progress";
    const busy = cancellingRuns.has(rec.id) || (runProgressOf(rec) || {}).cancelling;
    cancel.hidden = !running;
    cancel.disabled = Boolean(busy);
    cancel.dataset.id = rec.id;
    cancel.dataset.kind = kind;
    cancel.textContent = busy ? t("run.cancelling") : t("run.cancel");
  }
  renderRunTimeline(`${prefix}-details-timeline`, rec);

  const path = kind === "restore" ? "restores" : "backups";
  const download = document.getElementById(`${prefix}-log-download`);
  if (download) download.href = `/api/v1/${path}/${encodeURIComponent(rec.id)}/log`;
  const followWrap = document.getElementById(`${prefix}-log-follow-wrap`);
  if (followWrap) followWrap.hidden = rec.status !== "in_progress";

  if (runLog.prefix !== prefix || runLog.id !== rec.id) {
    stopRunLog();
    runLog.prefix = prefix;
    runLog.kind = kind;
    runLog.id = rec.id;
    runLog.loaded = false;
    setText(`${prefix}-log-text`, "");
    setText(`${prefix}-log-status`, "");
    showRunTab(prefix, "info");
  } else {
    scheduleRunLog();
  }
}

// Reads the end of the run log into the viewer of the open dialog. force re-reads
// even while a read is in flight (the newer answer wins).
async function loadRunLog(force) {
  const { prefix, kind, id } = runLog;
  if (!prefix || !id) return;
  const panel = document.getElementById(`${prefix}-panel-log`);
  if (!panel || panel.hidden || !runDialogOpen(prefix)) {
    stopRunLog();
    return;
  }
  if (!force && runLog.timer) return;
  const seq = ++runLog.seq;
  const path = kind === "restore" ? "restores" : "backups";
  const view = document.getElementById(`${prefix}-log-text`);
  const status = document.getElementById(`${prefix}-log-status`);
  if (!runLog.loaded && status) status.textContent = t("tables.loading");
  try {
    const res = await apiFetch(`/api/v1/${path}/${encodeURIComponent(id)}/log?tail=${RUN_LOG_TAIL_LINES}`);
    if (seq !== runLog.seq) return;
    if (res.status === 404) {
      if (view) view.textContent = "";
      if (status) status.textContent = t("run.log_none");
    } else if (!res.ok) {
      if (status) status.textContent = tf("toasts.unexpected_response", { status: res.status });
    } else {
      const text = await res.text();
      if (seq !== runLog.seq || !view) return;
      // Keep the view at the bottom while following, unless the user scrolled up.
      const atBottom = view.scrollHeight - view.scrollTop - view.clientHeight < 24;
      if (view.textContent !== text) view.textContent = text;
      if (status) status.textContent = text === "" ? t("run.log_empty") : "";
      if (atBottom || !runLog.loaded) view.scrollTop = view.scrollHeight;
      runLog.loaded = true;
    }
  } catch (err) {
    if (status && seq === runLog.seq) status.textContent = err.message;
  }
  scheduleRunLog();
}

// Polls the log while its run is active and "Follow" is on. A pending poll is kept
// (the list refreshes call this too, and must not keep postponing it).
function scheduleRunLog() {
  const { prefix, id } = runLog;
  const panel = prefix ? document.getElementById(`${prefix}-panel-log`) : null;
  const follow = prefix ? document.getElementById(`${prefix}-log-follow`) : null;
  const rec = prefix === "restore" ? state.restores.find(r => r.id === id) : findBackupRecord(id);
  const poll = Boolean(id && !document.hidden && runDialogOpen(prefix) && panel && !panel.hidden &&
    follow && follow.checked && rec && rec.status === "in_progress");
  if (!poll) {
    if (runLog.timer) clearTimeout(runLog.timer);
    runLog.timer = null;
    return;
  }
  if (runLog.timer) return;
  runLog.timer = setTimeout(() => {
    runLog.timer = null;
    loadRunLog(true);
  }, RUN_LOG_POLL_MS);
}

function stopRunLog() {
  if (runLog.timer) clearTimeout(runLog.timer);
  runLog.timer = null;
  runLog.seq++;
}

function runDialogOpen(prefix) {
  const modal = document.getElementById(`modal-${prefix}-details`);
  return Boolean(modal && modal.classList.contains("open"));
}

// ---------------------------------------------------------------------------
// Restore details dialog
// ---------------------------------------------------------------------------

function openRestoreDetails(restoreID) {
  if (!state.restores.some(r => r.id === restoreID)) return;
  detailsRestoreId = restoreID;
  openModal("modal-restore-details");
  renderRestoreDetails();
}

// Fills the restore details dialog of detailsRestoreId. Server values are set through
// textContent; messages are redacted by the server.
function renderRestoreDetails() {
  const modal = document.getElementById("modal-restore-details");
  if (!modal || !modal.classList.contains("open") || !detailsRestoreId) return;
  const r = state.restores.find(x => x.id === detailsRestoreId);
  if (!r) {
    closeModal("modal-restore-details");
    return;
  }
  const list = document.getElementById("restore-details-list");
  list.textContent = "";
  const row = (label, value, mono) => appendKv(list, label, value, mono);
  const [kind, label] = backupStatus(r.status);
  const { source, target } = restoreConnections(r);
  const secs = Number(r.duration_seconds);
  row(t("backup_details.id"), htmlNode(idCopy(r.id), "div").firstElementChild || r.id);
  row(t("backup_details.status"), htmlNode(statusBadge(kind, label)));
  row(t("run.source_backup"), r.backup_id, true);
  row(t("run.source"), `${r.source_database || ""}${source ? ` · ${source}` : ""}`);
  row(t("run.target"), `${r.target_database || ""}${target ? ` · ${target}` : ""}`);
  row(t("run.mode"), r.dry_run ? t("status.dry_run") : r.in_place ? t("run.mode_in_place") : t("run.mode_clone"));
  if (r.verified) row(t("status.verified"), t("run.yes"));
  row(t("backup_details.started"), absoluteWithRelative(r.started_at));
  row(r.status === "failed" ? t("backup_details.failed_at") : t("backup_details.finished"), absoluteWithRelative(r.completed_at));
  row(t("backup_details.duration"), secs > 0 ? formatDuration(secs) : "");
  if (r.status === "cancelled") {
    row(t("run.cancelled_by"), [r.cancelled_by || "", absoluteWithRelative(r.cancelled_at)].filter(Boolean).join(" · "));
  }

  const warning = document.getElementById("restore-details-warning");
  warning.hidden = !r.warning;
  setText("restore-details-warning-text", r.warning || "");
  const errorBox = document.getElementById("restore-details-error");
  setI18nText("restore-details-h-error", r.status === "cancelled" ? "run.cancel_note" : "backup_details.error");
  errorBox.hidden = !r.error_message;
  setText("restore-details-error-text", r.error_message || "");

  renderRunPanel("restore", "restore", r);
}

// Extra rows of the backup details dialog for a cancelled backup; its message is
// headed as a cancellation, not an error.
function backupCancelRows(b, row) {
  setI18nText("backup-details-h-error", b.status === "cancelled" ? "run.cancel_note" : "backup_details.error");
  if (b.status !== "cancelled") return;
  row(t("run.cancelled_by"), [b.cancelled_by || "", absoluteWithRelative(b.cancelled_at)].filter(Boolean).join(" · "));
}

// Outcome toast of a run started from this browser that was cancelled.
function reportCancelled(kind, rec, db) {
  showToast(tf(kind === "backups" ? "run.backup_cancelled_toast" : "run.restore_cancelled_toast", { db }), "info");
}

document.addEventListener("DOMContentLoaded", setupRunControls);
