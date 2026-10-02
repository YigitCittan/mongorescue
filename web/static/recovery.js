/**
 * MongoRescue dashboard: Settings → Recovery.
 *
 * Scheduled metadata backups (snapshots of MongoRescue's own database), the
 * recovery kit download dialog and the two warnings that go with them (no kit
 * since the material changed, metadata backups without encryption). Loaded after
 * app.js and i18n.js, whose helpers it uses; app.js calls the recovery* hooks.
 *
 * Same security invariants as app.js: every server value goes through
 * escapeHtml() or textContent, interaction goes through delegated data-action
 * listeners ("recovery-*"). The kit passphrase and the password are only sent in
 * the request body and cleared from the form afterwards.
 */

// Warning IDs of GET /api/v1/settings (settings.WarningRecoveryKit and
// settings.WarningMetadataBackupUnencrypted).
const WARNING_RECOVERY_KIT = "recovery_kit_missing";
const WARNING_METADATA_UNENCRYPTED = "metadata_backup_unencrypted";
const MIN_KIT_PASSPHRASE_LENGTH = 12;
const RECOVERY_POLL_MS = 2000;
const RECOVERY_POLL_LIMIT = 30;

const recovery = {
  // GET /api/v1/metadata-backup and GET /api/v1/recovery-kit.
  status: null,
  kit: null,
  dirty: false,
  pollTimer: null,
  downloading: false
};

// ---------------------------------------------------------------------------
// Warnings
// ---------------------------------------------------------------------------

function recoveryRenderWarnings(list) {
  const warnings = Array.isArray(list) ? list : [];
  const kit = document.getElementById("recovery-kit-warning");
  if (kit) kit.hidden = !warnings.some(w => w && w.id === WARNING_RECOVERY_KIT);
  const meta = document.getElementById("metabackup-warning");
  if (meta) meta.hidden = !warnings.some(w => w && w.id === WARNING_METADATA_UNENCRYPTED);
  const hint = document.getElementById("metabackup-unencrypted-hint");
  if (hint) hint.hidden = !!settingsGroup("encryption").enabled;
}

async function recoveryDismissKitWarning() {
  if (!(await confirmDialog({ body: t("recovery.kit_dismiss_confirm") }))) return;
  try {
    const json = await apiJSON(`/api/v1/settings/warnings/${WARNING_RECOVERY_KIT}/dismiss`, { method: "POST" });
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

// ---------------------------------------------------------------------------
// Settings → Recovery: metadata backups
// ---------------------------------------------------------------------------

// Fills the target select: "" is the default target.
function recoveryFillTargets() {
  const select = document.getElementById("metabackup-target");
  if (!select) return;
  const current = recovery.dirty ? select.value : (settingsGroup("metadata_backup").target_id || "");
  select.textContent = "";
  const add = (value, label) => {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = label;
    select.appendChild(opt);
  };
  add("", t("storage.default_target"));
  (state.storageTargets || []).forEach(s => add(s.id, s.name));
  if (current && !(state.storageTargets || []).some(s => s.id === current)) add(current, current);
  select.value = current;
}

// Fills the metadata backup form unless the operator is editing it.
function recoveryFillSettings(force) {
  const form = document.getElementById("form-metabackup");
  if (!form) return;
  const submit = form.querySelector("[type=submit]");
  if (submit) submit.disabled = !state.loaded.settings;
  if (!state.loaded.settings || (recovery.dirty && !force)) return;
  recovery.dirty = false;
  const s = settingsGroup("metadata_backup");
  document.getElementById("metabackup-enabled").checked = !!s.enabled;
  const interval = document.getElementById("metabackup-interval");
  const value = s.interval || "24h0m0s";
  if (!Array.from(interval.options).some(o => o.value === value)) {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = value;
    interval.appendChild(opt);
  }
  interval.value = value;
  setValue("metabackup-retention", Number(s.retention_count) || 14);
  recoveryFillTargets();
  hideFormError("metabackup-error");
  recoveryRenderWarnings(state.settings && state.settings.warnings);
}

async function recoverySaveSettings(e) {
  e.preventDefault();
  const keep = parseInt(getValue("metabackup-retention"), 10);
  if (isNaN(keep) || keep < 1 || keep > 1000) {
    showFormError("metabackup-error", t("recovery.metabackup_retention_invalid"));
    return;
  }
  const payload = {
    metadata_backup: {
      enabled: document.getElementById("metabackup-enabled").checked,
      interval: getValue("metabackup-interval"),
      target_id: getValue("metabackup-target"),
      retention_count: keep
    }
  };
  const submit = e.submitter || document.querySelector("#form-metabackup [type=submit]");
  if (submit) submit.disabled = true;
  try {
    const json = await apiJSON("/api/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    });
    if (!json.success) {
      showFormError("metabackup-error", json.error || t("toasts.request_failed"));
      return;
    }
    hideFormError("metabackup-error");
    // Only this form is refilled, so unsaved edits in the other forms stay.
    if (json.data && json.data.metadata_backup) {
      state.settings = { ...(state.settings || {}), metadata_backup: json.data.metadata_backup, warnings: json.data.warnings || [] };
    }
    recovery.dirty = false;
    recoveryFillSettings(true);
    renderWarnings();
    setText("metabackup-save-status", t("settings.saved"));
    setTimeout(() => setText("metabackup-save-status", ""), 3000);
    recoveryRefresh();
  } catch (err) {
    showFormError("metabackup-error", err.message);
  } finally {
    if (submit) submit.disabled = false;
  }
}

async function recoveryRefresh() {
  if (!auth.user) return;
  try {
    const [meta, kit] = await Promise.all([apiJSON("/api/v1/metadata-backup"), apiJSON("/api/v1/recovery-kit")]);
    recovery.status = meta.success ? meta.data || null : null;
    recovery.kit = kit.success ? kit.data || null : null;
  } catch (err) {
    console.error("Failed to load the recovery status:", err);
  }
  recoveryRenderStatus();
}

function recoveryWhen(value) {
  const d = parseDate(value);
  return d ? formatRelative(d) : "";
}

function recoveryRenderStatus() {
  const box = document.getElementById("metabackup-status");
  if (box) {
    const st = recovery.status;
    const lines = [];
    if (st && st.running) {
      lines.push({ text: t("recovery.metabackup_running") });
    }
    if (st && st.last) {
      lines.push({ text: tf("recovery.metabackup_last", { when: recoveryWhen(st.last.created_at), target: st.last.target_name || st.last.target_id, size: formatBytes(st.last.size_bytes) }) });
      lines.push({ text: st.last.key, mono: true });
      if (!st.last.encrypted) lines.push({ text: t("recovery.unencrypted_hint"), danger: true });
    } else if (st) {
      lines.push({ text: t("recovery.metabackup_never") });
    }
    if (st && st.last_error) lines.push({ text: tf("recovery.metabackup_failed", { when: recoveryWhen(st.last_run_at), error: st.last_error }), danger: true });
    if (st && st.retention_error) lines.push({ text: tf("recovery.metabackup_retention_failed", { error: st.retention_error }), danger: true });
    if (st && st.next_run_at) lines.push({ text: tf("recovery.metabackup_next", { when: recoveryWhen(st.next_run_at) }) });
    else if (st && !st.enabled) lines.push({ text: t("recovery.metabackup_off") });
    box.innerHTML = lines.map(l => `<p class="${l.mono ? "mono ellipsis" : ""}${l.danger ? " text-danger" : ""}">${escapeHtml(l.text)}</p>`).join("");
    const run = document.getElementById("metabackup-run");
    if (run) run.disabled = !!(st && st.running);
  }
  const kit = document.getElementById("recovery-kit-status");
  if (kit) {
    const k = recovery.kit;
    let text = "";
    if (k && k.downloaded_at) {
      text = tf("recovery.kit_last", { when: recoveryWhen(k.downloaded_at) });
      if (!k.up_to_date) text += " " + t("recovery.kit_outdated");
    } else if (k) {
      text = t("recovery.kit_never");
    }
    kit.textContent = text;
    kit.classList.toggle("text-danger", !!(k && !k.up_to_date));
  }
}

async function recoveryRunNow(btn) {
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON("/api/v1/metadata-backup/run", { method: "POST" });
    if (!json.success) {
      showToast(json.httpStatus === 409 ? t("recovery.metabackup_busy") : json.error || t("toasts.request_failed"), "error");
      if (btn) btn.disabled = false;
      return;
    }
    recovery.status = json.data || recovery.status;
    recoveryRenderStatus();
    showToast(t("recovery.metabackup_started"), "info");
    recoveryPoll(0);
  } catch (err) {
    showToast(err.message, "error");
    if (btn) btn.disabled = false;
  }
}

// Polls the status until the snapshot started by "Back up now" finished.
function recoveryPoll(round) {
  clearTimeout(recovery.pollTimer);
  recovery.pollTimer = setTimeout(async () => {
    await recoveryRefresh();
    if (recovery.status && recovery.status.running && round < RECOVERY_POLL_LIMIT) {
      recoveryPoll(round + 1);
      return;
    }
    const st = recovery.status;
    if (st && st.last_error) showToast(tf("recovery.metabackup_failed", { when: recoveryWhen(st.last_run_at), error: st.last_error }), "error");
    else if (st && st.last) showToast(t("recovery.metabackup_done"), "success");
    // The recovery kit names the latest snapshot.
    loadSettings();
  }, RECOVERY_POLL_MS);
}

// ---------------------------------------------------------------------------
// Recovery kit dialog
// ---------------------------------------------------------------------------

function recoveryClearKitForm() {
  ["kit-passphrase", "kit-passphrase-confirm", "kit-current-password"].forEach(id => setValue(id, ""));
  hideFormError("kit-error");
}

function recoveryOpenKit() {
  recoveryClearKitForm();
  openModal("modal-recovery-kit");
}

// Download name from Content-Disposition, restricted to safe characters.
function recoveryKitFileName(res) {
  const header = res.headers.get("content-disposition") || "";
  const m = /filename="?([A-Za-z0-9._-]+)"?/.exec(header);
  return m ? m[1] : "mongorescue-recovery-kit.tar.age";
}

async function recoveryDownloadKit(e) {
  e.preventDefault();
  if (recovery.downloading) return;
  const passphrase = getValue("kit-passphrase");
  const confirmValue = getValue("kit-passphrase-confirm");
  const password = getValue("kit-current-password");
  if (Array.from(passphrase).length < MIN_KIT_PASSPHRASE_LENGTH || !passphrase.trim()) {
    showFormError("kit-error", tf("recovery.kit_err_short", { n: MIN_KIT_PASSPHRASE_LENGTH }));
    return;
  }
  if (passphrase !== confirmValue) {
    showFormError("kit-error", t("recovery.kit_err_mismatch"));
    return;
  }
  if (!password) {
    showFormError("kit-error", t("recovery.kit_err_password_required"));
    return;
  }
  hideFormError("kit-error");
  recovery.downloading = true;
  const submit = document.getElementById("kit-submit");
  if (submit) submit.disabled = true;
  try {
    // Not apiFetch: a wrong password answers 403, which apiFetch would retry and so
    // count twice against the password throttle.
    const res = await fetch("/api/v1/recovery-kit", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": auth.csrf },
      body: JSON.stringify({ passphrase, current_password: password })
    });
    if (res.status === 401) {
      handleUnauthorized();
      return;
    }
    const type = res.headers.get("content-type") || "";
    if (!res.ok || !type.includes("application/octet-stream")) {
      let msg = t("toasts.request_failed");
      if (res.status === 403) msg = t("recovery.kit_err_wrong_password");
      else if (res.status === 429) msg = t("auth.err_rate_limited");
      else if (type.includes("application/json")) {
        try {
          const json = await res.json();
          if (json && json.error) msg = json.error;
        } catch (err) {
          // keep the generic message
        }
      }
      setValue("kit-current-password", "");
      showFormError("kit-error", msg);
      return;
    }
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = recoveryKitFileName(res);
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    recoveryClearKitForm();
    closeModal("modal-recovery-kit");
    showToast(t("recovery.kit_downloaded"), "success");
    loadSettings();
    recoveryRefresh();
  } catch (err) {
    showFormError("kit-error", err.message);
  } finally {
    recovery.downloading = false;
    if (submit) submit.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function recoverySetup() {
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-action^='recovery-']");
    if (!btn || btn.disabled) return;
    switch (btn.dataset.action) {
      case "recovery-open-kit":
        recoveryOpenKit();
        break;
      case "recovery-dismiss-kit":
        recoveryDismissKitWarning();
        break;
      case "recovery-run-metabackup":
        recoveryRunNow(btn);
        break;
    }
  });
  const form = document.getElementById("form-metabackup");
  if (form) {
    form.addEventListener("submit", recoverySaveSettings);
    form.addEventListener("input", () => { recovery.dirty = true; });
    form.addEventListener("change", () => { recovery.dirty = true; });
  }
  const kitForm = document.getElementById("form-recovery-kit");
  if (kitForm) kitForm.addEventListener("submit", recoveryDownloadKit);
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      recoveryFillTargets();
      recoveryRenderStatus();
    });
  }
}

document.addEventListener("DOMContentLoaded", recoverySetup);
