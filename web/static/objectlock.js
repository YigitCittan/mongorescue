/**
 * MongoRescue dashboard: immutable backups (S3 Object Lock).
 *
 * An S3 storage target can lock every uploaded object in governance or compliance
 * mode for a number of days, and set an S3 legal hold on the archive of a pinned
 * backup. The server checks the bucket (Object Lock and versioning enabled) when the
 * target is saved or tested and keeps a deleted backup until its lock ends; the
 * dashboard shows the settings, the "locked until" date and the target hints.
 *
 * Loaded after app.js and trust.js (mergeTranslations). Server values reach the DOM
 * only through escapeHtml() or textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const OBJECT_LOCK_TRANSLATIONS = {
  en: {
    objectlock: {
      label: "Object Lock (immutable backups)",
      mode_none: "None",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "Needs a bucket created with Object Lock enabled; MongoRescue checks it on save and never enables it. Compliance locks cannot be shortened or removed by anyone, including the bucket owner.",
      retention_label: "Lock each upload for (days)",
      legal_hold_label: "Set an S3 legal hold on pinned backups",
      err_retention: "Enter a number of days from {min} to {max}.",
      locked_until: "Locked until {date}",
      locked_until_title: "S3 Object Lock ({mode}): the archive cannot be deleted before {date}",
      lock_expired: "Lock ended {date}",
      legal_hold: "Legal hold",
      legal_hold_on: "On (S3 legal hold while pinned)",
      version: "S3 version",
      chip: "{mode} lock · {days} d",
      deleted_locked: "Deleted, waiting for its lock to end on {date}; then purged",
      hints_title: "Storage protection",
      job_warning: "Job saved. {warning}"
    }
  },
  tr: {
    objectlock: {
      label: "Object Lock (değiştirilemez yedekler)",
      mode_none: "Yok",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "Object Lock etkin oluşturulmuş bir kova gerekir; MongoRescue kaydederken denetler, kendisi asla etkinleştirmez. Compliance kilitlerini kova sahibi dahil kimse kısaltamaz veya kaldıramaz.",
      retention_label: "Her yüklemeyi kilitle (gün)",
      legal_hold_label: "Sabitlenen yedeklere S3 legal hold koy",
      err_retention: "{min} ile {max} arasında bir gün sayısı girin.",
      locked_until: "{date} tarihine kadar kilitli",
      locked_until_title: "S3 Object Lock ({mode}): arşiv {date} tarihinden önce silinemez",
      lock_expired: "Kilit {date} tarihinde bitti",
      legal_hold: "Legal hold",
      legal_hold_on: "Açık (sabitliyken S3 legal hold)",
      version: "S3 sürümü",
      chip: "{mode} kilidi · {days} g",
      deleted_locked: "Silindi, kilidinin {date} tarihinde bitmesi bekleniyor; ardından temizlenir",
      hints_title: "Depolama koruması",
      job_warning: "Görev kaydedildi. {warning}"
    }
  }
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(OBJECT_LOCK_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], OBJECT_LOCK_TRANSLATIONS[lang]);
  });
}

const OBJECT_LOCK_MIN_DAYS = 1;
const OBJECT_LOCK_MAX_DAYS = 3650;
const OBJECT_LOCK_DEFAULT_DAYS = 30;

// objectLockModeLabel names an object lock mode.
function objectLockModeLabel(mode) {
  return t(`objectlock.mode_${mode === "governance" || mode === "compliance" ? mode : "none"}`);
}

// ---------------------------------------------------------------------------
// Storage target form and list
// ---------------------------------------------------------------------------

// objectLockSyncForm enables the retention and legal hold fields only with a mode.
function objectLockSyncForm() {
  const mode = document.getElementById("storage-object-lock");
  if (!mode) return;
  const locked = mode.value === "governance" || mode.value === "compliance";
  ["storage-retention-days", "storage-legal-hold"].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.disabled = !locked;
  });
  const row = document.getElementById("storage-lock-fields");
  if (row) row.hidden = !locked;
}

// objectLockFillForm fills the object lock fields from a target's S3 settings.
function objectLockFillForm(s3) {
  const mode = document.getElementById("storage-object-lock");
  if (!mode) return;
  mode.value = s3 && (s3.object_lock === "governance" || s3.object_lock === "compliance") ? s3.object_lock : "none";
  setValue("storage-retention-days", String((s3 && s3.retention_days) || OBJECT_LOCK_DEFAULT_DAYS));
  document.getElementById("storage-legal-hold").checked = !!(s3 && s3.legal_hold_on_pin);
  objectLockSyncForm();
}

// objectLockPayload returns the object lock fields of the S3 settings, or throws
// FieldError for an invalid retention.
function objectLockPayload() {
  const mode = getValue("storage-object-lock") || "none";
  if (mode !== "governance" && mode !== "compliance") return { object_lock: "none", retention_days: 0, legal_hold_on_pin: false };
  const days = Number(getValue("storage-retention-days"));
  if (!Number.isInteger(days) || days < OBJECT_LOCK_MIN_DAYS || days > OBJECT_LOCK_MAX_DAYS) {
    throw new FieldError("storage-retention-days", tf("objectlock.err_retention", { min: OBJECT_LOCK_MIN_DAYS, max: OBJECT_LOCK_MAX_DAYS }));
  }
  return { object_lock: mode, retention_days: days, legal_hold_on_pin: document.getElementById("storage-legal-hold").checked };
}

// objectLockTargetCell shows a target's lock and its hints under its location.
function objectLockTargetCell(s) {
  const s3 = (s && s.s3) || {};
  const parts = [];
  if (s3.object_lock === "governance" || s3.object_lock === "compliance") {
    parts.push(`<span class="trust-chip trust-ok">${escapeHtml(tf("objectlock.chip", { mode: objectLockModeLabel(s3.object_lock), days: s3.retention_days }))}</span>`);
  }
  (Array.isArray(s && s.hints) ? s.hints : []).forEach(h => {
    const cls = h.level === "warn" ? "trust-chip trust-warn" : "trust-chip";
    parts.push(`<span class="${cls}" title="${escapeHtml(h.message)}">${escapeHtml(truncate(String(h.message || ""), 60))}</span>`);
  });
  return parts.length ? `<div class="trust-chips">${parts.join("")}</div>` : "";
}

// ---------------------------------------------------------------------------
// Backups
// ---------------------------------------------------------------------------

// objectLockActive reports whether b's archive is still under its lock.
function objectLockActive(b) {
  const until = b && parseDate(b.retain_until);
  return !!until && until.getTime() > Date.now();
}

// objectLockBackupBadge is the "Locked until" chip under a backup's status.
function objectLockBackupBadge(b) {
  if (!objectLockActive(b)) return "";
  const date = formatAbsolute(parseDate(b.retain_until));
  const title = tf("objectlock.locked_until_title", { mode: objectLockModeLabel(b.object_lock_mode), date });
  return `<div class="trust-chips"><span class="trust-chip trust-ok" title="${escapeHtml(title)}">${escapeHtml(tf("objectlock.locked_until", { date }))}</span></div>`;
}

// objectLockBackupDetailRows adds the lock, the legal hold and the version to the
// backup details dialog.
function objectLockBackupDetailRows(b, row) {
  if (!b) return;
  if (b.retain_until) {
    const date = absoluteWithRelative(b.retain_until);
    let text = objectLockActive(b) ? tf("objectlock.locked_until_title", { mode: objectLockModeLabel(b.object_lock_mode), date }) : tf("objectlock.lock_expired", { date });
    if (b.status === "deleted" && objectLockActive(b)) text = tf("objectlock.deleted_locked", { date });
    row(t("objectlock.label"), text);
  }
  if (b.legal_hold) row(t("objectlock.legal_hold"), t("objectlock.legal_hold_on"));
  if (b.storage_version_id) row(t("objectlock.version"), String(b.storage_version_id), true);
}

// ---------------------------------------------------------------------------
// Readiness and job saves
// ---------------------------------------------------------------------------

// objectLockReadinessHints lists the storage hints of a readiness report.
function objectLockReadinessHints(data) {
  const hints = data && Array.isArray(data.storage_hints) ? data.storage_hints : [];
  if (hints.length === 0) return "";
  const items = hints.map(h => {
    const cls = h.level === "warn" ? "rd-hint rd-hint-warn" : "rd-hint";
    return `<li class="${cls}"><strong>${escapeHtml(h.target_name || h.target_id)}</strong>: ${escapeHtml(h.message)}</li>`;
  }).join("");
  return `<div class="rd-hints"><h4 class="rd-hints-title">${escapeHtml(t("objectlock.hints_title"))}</h4><ul>${items}</ul></div>`;
}

// objectLockJobSaved shows the warnings of a job save (a retention shorter than the
// lock of its storage target).
function objectLockJobSaved(data) {
  const warnings = data && Array.isArray(data.warnings) ? data.warnings : [];
  warnings.forEach(w => showToast(tf("objectlock.job_warning", { warning: w }), "info"));
}

document.addEventListener("change", e => {
  if (e.target && e.target.id === "storage-object-lock") objectLockSyncForm();
});
