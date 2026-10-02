/**
 * MongoRescue dashboard: Settings → Audit log.
 *
 * The hash-chained log of every action (GET /api/v1/audit): a filterable,
 * paged table, chain verification (GET /api/v1/audit/verify), the JSON Lines
 * export (GET /api/v1/audit/export) and the retention and webhook forwarding
 * settings (the "audit" group of PUT /api/v1/settings). Loaded after app.js and
 * i18n.js, whose helpers it uses; app.js calls auditlogRefresh() when the panel
 * opens and auditlogFillSettings() when the settings load.
 *
 * Same security invariants as app.js: every server value goes through
 * escapeHtml() or textContent, interaction goes through delegated data-action
 * listeners ("auditlog-*"). The webhook URL and secret come back masked; sending
 * the masks back keeps the stored values.
 */

const AUDITLOG_PAGE = 100;
const AUDITLOG_MIN_RETENTION = 30;
const AUDITLOG_MAX_RETENTION = 36500;
const AUDITLOG_RANGES = { "24h": 24, "7d": 24 * 7, "30d": 24 * 30 };
const AUDITLOG_OUTCOMES = { ok: "success", error: "danger", denied: "warn", rate_limited: "neutral" };

const auditlog = {
  events: [],
  nextBefore: 0,
  forwarding: null,
  error: "",
  loaded: false,
  loading: false,
  verification: null,
  dirty: false,
  filterTimer: null,
  exporting: false
};

// ---------------------------------------------------------------------------
// Table
// ---------------------------------------------------------------------------

// Query string of the current filters (no paging).
function auditlogQuery() {
  const q = new URLSearchParams();
  const actor = getValue("auditlog-filter-actor");
  const action = getValue("auditlog-filter-action");
  const kind = getValue("auditlog-filter-kind");
  const result = getValue("auditlog-filter-result");
  const range = getValue("auditlog-filter-range");
  if (actor) q.set("actor", actor);
  if (action) q.set("action", action);
  if (kind) q.set("actor_kind", kind);
  if (result) q.set("result", result);
  if (AUDITLOG_RANGES[range]) {
    q.set("since", new Date(Date.now() - AUDITLOG_RANGES[range] * 3600 * 1000).toISOString().replace(/\.\d{3}Z$/, "Z"));
  }
  return q;
}

async function auditlogRefresh(more) {
  if (!auth.user || auditlog.loading) return;
  auditlog.loading = true;
  const q = auditlogQuery();
  q.set("limit", String(AUDITLOG_PAGE));
  if (more && auditlog.nextBefore) q.set("before_id", String(auditlog.nextBefore));
  try {
    const json = await apiJSON(`/api/v1/audit?${q.toString()}`);
    if (!json.success) {
      auditlog.error = json.error || t("toasts.request_failed");
    } else {
      const data = json.data || {};
      const events = Array.isArray(data.events) ? data.events : [];
      auditlog.events = more ? auditlog.events.concat(events) : events;
      auditlog.nextBefore = Number(data.next_before_id) || 0;
      auditlog.forwarding = data.forwarding || null;
      auditlog.error = "";
    }
  } catch (err) {
    auditlog.error = err.message;
  } finally {
    auditlog.loaded = true;
    auditlog.loading = false;
  }
  auditlogRender();
  auditlogRenderForwarding();
}

function auditlogActorCell(e) {
  const kindLabel = t(`auditlog.kind_${e.actor_kind}`, String(e.actor_kind || ""));
  let name = "";
  let id = "";
  if (e.actor_kind === "api_key") {
    name = e.actor_key_name || e.actor_key_id || "";
    id = e.actor_key_id || "";
  } else {
    name = e.actor_name || e.actor_user_id || "";
    id = e.actor_user_id || "";
  }
  const sub = [kindLabel];
  if (e.actor_kind === "api_key" && e.actor_name) sub.push(e.actor_name);
  return `<span class="cell-primary"${id ? ` title="${escapeHtml(id)}"` : ""}>${ellipsis(name || "—")}</span>` +
    `<div class="cell-sub muted">${escapeHtml(sub.join(" · "))}</div>`;
}

function auditlogTargets(e) {
  const targets = e.targets && typeof e.targets === "object" ? Object.keys(e.targets).sort() : [];
  if (!targets.length) return mutedDash();
  return targets.map(k => `<div class="mono">${ellipsis(`${k}=${e.targets[k]}`)}</div>`).join("");
}

function auditlogResultCell(e) {
  const kind = AUDITLOG_OUTCOMES[e.outcome] || "neutral";
  const label = AUDITLOG_OUTCOMES[e.outcome] ? t(`settings.result_${e.outcome}`) : String(e.outcome || "");
  const status = Number(e.status) > 0 ? ` <span class="mono muted">${Number(e.status)}</span>` : "";
  const count = Number(e.count) > 1 ? ` <span class="muted">×${Number(e.count)}</span>` : "";
  return `${statusBadge(kind, label)}${status}${count}`;
}

function auditlogClientCell(e) {
  if (!e.client_ip && !e.user_agent) return mutedDash();
  const ua = e.user_agent ? `<div class="cell-sub muted">${ellipsis(truncate(e.user_agent, 60))}</div>` : "";
  return `<span class="mono">${escapeHtml(e.client_ip || "")}</span>${ua}`;
}

function auditlogRender() {
  const tbody = document.getElementById("auditlog-tbody");
  if (!tbody || !auditlog.loaded) return;
  const more = document.getElementById("auditlog-more");
  if (more) more.hidden = !auditlog.nextBefore || !!auditlog.error;
  if (auditlog.error) {
    setTbody(tbody, emptyRow(6, tf("auditlog.load_failed", { error: auditlog.error })));
    return;
  }
  if (auditlog.events.length === 0) {
    setTbody(tbody, emptyRow(6, t("auditlog.empty")));
    return;
  }
  setTbody(tbody, auditlog.events.map(e => `<tr>
      <td>${timeCell(e.time)}<div class="cell-sub mono muted">#${Number(e.id) || 0}</div></td>
      <td>${auditlogActorCell(e)}</td>
      <td><span class="mono">${ellipsis(e.action || "")}</span></td>
      <td>${auditlogTargets(e)}</td>
      <td>${auditlogResultCell(e)}</td>
      <td>${auditlogClientCell(e)}</td>
    </tr>`).join(""));
}

// Reloads the first page shortly after the filters stop changing.
function auditlogFilterChanged() {
  clearTimeout(auditlog.filterTimer);
  auditlog.filterTimer = setTimeout(() => auditlogRefresh(false), 300);
}

// ---------------------------------------------------------------------------
// Verification and export
// ---------------------------------------------------------------------------

async function auditlogVerify(btn) {
  if (btn) btn.disabled = true;
  setText("auditlog-verify-status", t("auditlog.verifying"));
  try {
    const json = await apiJSON("/api/v1/audit/verify");
    if (!json.success) {
      auditlog.verification = null;
      setText("auditlog-verify-status", json.error || t("toasts.request_failed"));
      return;
    }
    auditlog.verification = json.data || null;
    auditlogRenderVerification();
    const v = auditlog.verification;
    showToast(v && v.ok ? t("auditlog.verify_ok_toast") : t("auditlog.verify_broken_toast"), v && v.ok ? "success" : "error");
  } catch (err) {
    setText("auditlog-verify-status", err.message);
  } finally {
    if (btn) btn.disabled = false;
  }
}

function auditlogRenderVerification() {
  const el = document.getElementById("auditlog-verify-status");
  const v = auditlog.verification;
  if (!el || !v) return;
  const head = String(v.head_hash || "").slice(0, 16);
  el.textContent = v.ok
    ? tf("auditlog.verify_ok", { n: Number(v.checked) || 0, id: Number(v.head_id) || 0, hash: head })
    : tf("auditlog.verify_broken", { id: Number(v.broken_id) || 0, reason: String(v.reason || "") });
  el.classList.toggle("text-danger", !v.ok);
}

// Download name from Content-Disposition, restricted to safe characters.
function auditlogFileName(res) {
  const header = res.headers.get("content-disposition") || "";
  const m = /filename="?([A-Za-z0-9._-]+)"?/.exec(header);
  return m ? m[1] : "mongorescue-audit.jsonl";
}

async function auditlogExport(btn) {
  if (auditlog.exporting) return;
  auditlog.exporting = true;
  if (btn) btn.disabled = true;
  try {
    const res = await apiFetch(`/api/v1/audit/export?${auditlogQuery().toString()}`);
    const type = res.headers.get("content-type") || "";
    if (!res.ok || !type.includes("application/x-ndjson")) {
      let msg = tf("toasts.unexpected_response", { status: res.status });
      if (type.includes("application/json")) {
        try {
          const json = await res.json();
          if (json && json.error) msg = json.error;
        } catch (err) {
          // keep the generic message
        }
      }
      showToast(msg, "error");
      return;
    }
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = auditlogFileName(res);
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    showToast(t("auditlog.exported"), "success");
    // The export is an audited action itself.
    auditlogRefresh(false);
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    auditlog.exporting = false;
    if (btn) btn.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// Retention and forwarding settings
// ---------------------------------------------------------------------------

function auditlogFillSettings(force) {
  const form = document.getElementById("form-auditlog");
  if (!form) return;
  const submit = form.querySelector("[type=submit]");
  if (submit) submit.disabled = !state.loaded.settings;
  if (!state.loaded.settings || (auditlog.dirty && !force)) return;
  auditlog.dirty = false;
  const a = settingsGroup("audit");
  setValue("auditlog-retention", Number(a.retention_days) || 365);
  setValue("auditlog-webhook-url", a.webhook_url || "");
  setValue("auditlog-webhook-secret", a.webhook_secret || "");
  hideFormError("auditlog-error");
  auditlogRenderForwarding();
}

function auditlogRenderForwarding() {
  const el = document.getElementById("auditlog-forward-status");
  if (!el) return;
  const f = auditlog.forwarding;
  if (!f || !f.enabled) {
    el.textContent = t("auditlog.forward_off");
    el.classList.remove("text-danger");
    return;
  }
  let text = tf("auditlog.forward_status", {
    sent: Number(f.sent) || 0, failed: Number(f.failed) || 0, dropped: Number(f.dropped) || 0, queued: Number(f.queued) || 0
  });
  if (f.last_error) text += " " + tf("auditlog.forward_last_error", { error: f.last_error });
  el.textContent = text;
  el.classList.toggle("text-danger", Number(f.failed) > 0 || Number(f.dropped) > 0);
}

async function auditlogSaveSettings(e) {
  e.preventDefault();
  const days = parseInt(getValue("auditlog-retention"), 10);
  if (isNaN(days) || days < AUDITLOG_MIN_RETENTION || days > AUDITLOG_MAX_RETENTION) {
    showFormError("auditlog-error", tf("auditlog.retention_invalid", { min: AUDITLOG_MIN_RETENTION, max: AUDITLOG_MAX_RETENTION }));
    return;
  }
  const url = getValue("auditlog-webhook-url");
  if (url && !/^https?:\/\/[^\s/]+/i.test(url)) {
    showFormError("auditlog-error", t("auditlog.webhook_url_invalid"));
    return;
  }
  const secretEl = document.getElementById("auditlog-webhook-secret");
  const payload = {
    audit: {
      retention_days: days,
      webhook_url: url,
      webhook_secret: secretEl ? secretEl.value : ""
    }
  };
  const submit = e.submitter || document.querySelector("#form-auditlog [type=submit]");
  if (submit) submit.disabled = true;
  try {
    const json = await apiJSON("/api/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    });
    if (!json.success) {
      showFormError("auditlog-error", json.error || t("toasts.request_failed"));
      return;
    }
    hideFormError("auditlog-error");
    // Only this form is refilled, so unsaved edits in the other forms stay.
    if (json.data && json.data.audit) {
      state.settings = { ...(state.settings || {}), audit: json.data.audit, warnings: json.data.warnings || [] };
    }
    auditlog.dirty = false;
    auditlogFillSettings(true);
    setText("auditlog-save-status", t("settings.saved"));
    setTimeout(() => setText("auditlog-save-status", ""), 3000);
    auditlogRefresh(false);
  } catch (err) {
    showFormError("auditlog-error", err.message);
  } finally {
    if (submit) submit.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function auditlogSetup() {
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-action^='auditlog-']");
    if (!btn || btn.disabled) return;
    switch (btn.dataset.action) {
      case "auditlog-refresh":
        auditlogRefresh(false);
        break;
      case "auditlog-more":
        auditlogRefresh(true);
        break;
      case "auditlog-verify":
        auditlogVerify(btn);
        break;
      case "auditlog-export":
        auditlogExport(btn);
        break;
    }
  });
  ["auditlog-filter-actor", "auditlog-filter-action"].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.addEventListener("input", auditlogFilterChanged);
  });
  ["auditlog-filter-kind", "auditlog-filter-result", "auditlog-filter-range"].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.addEventListener("change", () => auditlogRefresh(false));
  });
  const form = document.getElementById("form-auditlog");
  if (form) {
    form.addEventListener("submit", auditlogSaveSettings);
    form.addEventListener("input", () => { auditlog.dirty = true; });
    form.addEventListener("change", () => { auditlog.dirty = true; });
  }
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      auditlogRender();
      auditlogRenderVerification();
      auditlogRenderForwarding();
    });
  }
}

document.addEventListener("DOMContentLoaded", auditlogSetup);
