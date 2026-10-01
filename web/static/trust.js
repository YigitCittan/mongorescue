/**
 * MongoRescue dashboard: evidence that backups are restorable.
 *
 * Archive verification badges and "Verify now", pins (legal hold), automated
 * restore tests, retention previews and the retention history, the integrity
 * sweep and storage scans (orphan and missing archives). Loaded after app.js and
 * i18n.js, whose helpers it uses; app.js calls the trust* hooks below.
 *
 * Same security invariants as app.js: every server value goes through
 * escapeHtml() or textContent, interaction goes through delegated data-action
 * listeners ("trust-*"), and IDs in URLs are wrapped in encodeURIComponent().
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const TRUST_TRANSLATIONS = {
  en: {
    status: { missing: "Missing" },
    notify: {
      events: {
        verification_failed: "Backup verification failed",
        restore_test_succeeded: "Restore test passed",
        restore_test_failed: "Restore test failed",
        storage_drift_detected: "Storage drift detected",
        retention_deleted: "Backup deleted by retention",
      },
    },
    settings: { nav_integrity: "Integrity" },
    trust: {
      verified: "Verified · {when}",
      verify_mismatch: "Checksum mismatch",
      verify_error: "Not verified",
      verify_error_detail: "Verification could not run: {error}",
      never_verified: "Never verified",
      imported: "Imported",
      pinned: "Pinned",
      pinned_title: "Pinned by {by}: {note}",
      pinned_title_no_note: "Pinned by {by}",
      restore_test_ok: "Restore test: passed · {when}",
      restore_test_mismatch: "Restore test: differences · {when}",
      restore_test_error: "Restore test: failed · {when}",
      last_restore_test: "Last restore test",
      verification: "Verification",
      verified_at: "Last verified",
      pin: "Pin",
      verify_now: "Verify now",
      pin_action: "Pin (legal hold)",
      unpin_action: "Unpin",
      pin_prompt: "Pin this backup. Retention will never delete it and it cannot be deleted until it is unpinned.\n\nNote (optional):",
      unpin_confirm: "Unpin this backup? Retention and deletion apply to it again.",
      pinned_toast: "Backup pinned",
      unpinned_toast: "Backup unpinned",
      verify_started: "Verification started",
      verify_done_ok: "Backup {id} verified: the archive matches its checksum",
      verify_done_bad: "Backup {id} failed verification: {error}",
      job_section: "Integrity",
      job_verify: "Verify after upload",
      job_verify_inherit: "Use the setting ({value})",
      job_verify_on: "Always",
      job_verify_off: "Never",
      job_verify_hint: "Re-reads the stored archive after every backup and fails it when the bytes differ from what was written.",
      rt_enabled: "Automated restore test",
      rt_enabled_hint: "After a scheduled backup, restore it into a temporary <db>_rescue_verify_<time> database, compare counts and indexes, then drop it. Needs create and drop privileges.",
      rt_frequency: "Frequency",
      rt_daily: "Daily",
      rt_weekly: "Weekly",
      rt_monthly: "Monthly",
      rt_every_n: "Every N backups",
      rt_every_n_label: "N",
      rt_connection: "Test server",
      rt_connection_same: "The backup's own server",
      rt_policy: "Restore test",
      rt_off: "Off",
      rt_policy_text: "{freq} · {server}",
      rt_policy_every: "Every {n} backups",
      retention_preview_n: "With these settings {n} backup(s) would be deleted now.",
      retention_preview_none: "With these settings no backup would be deleted now.",
      retention_preview_protected: "{n} kept: pinned or the newest verified backup.",
      retention_history: "Retention history",
      retention_empty: "Retention has not deleted any backup of this job.",
      retention_entry: "{id} · {detail}",
      retention_entry_error: "the archive could not be deleted from storage: {error}",
      restore_tests_title: "Restore tests",
      restore_tests_empty: "No restore test yet.",
      run_restore_test: "Run restore test now",
      restore_test_started: "Restore test started for backup {id}",
      rt_status_ok: "Passed",
      rt_status_mismatch: "Differences",
      rt_status_error: "Failed",
      rt_dropped_failed: "the temporary database {db} could not be dropped: {error}",
      integrity_title: "Integrity",
      integrity_desc: "Evidence that backups are restorable: archives are re-read and compared with the checksum recorded when they were written, and storage is compared with the backup records.",
      verify_after_backup: "Verify every backup after upload",
      verify_after_backup_hint: "A backup whose stored archive differs from what was written fails. Jobs can override this.",
      verify_decrypt: "Also decrypt encrypted archives when verifying",
      verify_decrypt_hint: "Proves the configured keys still open them. Needs the identity or passphrase under Encryption.",
      sweep_schedule: "Integrity sweep",
      sweep_schedule_hint: "Re-verifies every completed backup, least recently verified first, one at a time. Reading archives back costs egress on cloud storage.",
      sweep_off: "Off",
      sweep_daily: "Daily",
      sweep_weekly: "Weekly",
      sweep_monthly: "Monthly",
      sweep_bandwidth: "Sweep bandwidth limit (MiB/s)",
      sweep_bandwidth_hint: "0 means unlimited.",
      storage_scan: "Scan storage targets weekly",
      storage_scan_hint: "Lists every target and reports orphan archives (no record) and missing ones (record without archive). Nothing is ever deleted.",
      sweep_status: "Sweep status",
      sweep_never: "No sweep has run yet.",
      sweep_running: "Running: {done} of {total} verified",
      sweep_finished: "Last sweep {when}: {ok} ok, {mismatch} mismatch, {errors} not verified ({total} backups)",
      sweep_interrupted: "Interrupted: {reason}",
      sweep_next: "Next sweep {when}",
      run_sweep: "Run sweep now",
      sweep_started: "Integrity sweep started",
      scans_title: "Storage scans",
      scans_desc: "Compare a target's archives with the backup records. Orphans can be imported; missing archives are marked and never deleted.",
      scan_now: "Scan now",
      scan_never: "Not scanned yet",
      scan_result: "Scanned {when}: {objects} archive(s), {orphans} orphan(s), {missing} missing",
      scan_failed: "Scan failed: {error}",
      scan_done: "Scan of {name} finished: {orphans} orphan(s), {missing} missing",
      orphans: "Orphan archives",
      missing: "Missing archives",
      import: "Import",
      import_confirm: "Create a backup record for {key}? It is hashed and marked imported and unverified.",
      import_started: "Import started",
      next_scan: "Next storage scan {when}",
      drift_title: "Storage and backup records disagree",
      drift_text: "{targets}: orphan archives or missing archives were found. Review them under Settings → Storage.",
      drift_open: "Review storage",
      busy: "Already running",
    },
  },
  tr: {
    status: { missing: "Kayıp" },
    notify: {
      events: {
        verification_failed: "Yedek doğrulaması başarısız",
        restore_test_succeeded: "Geri yükleme testi başarılı",
        restore_test_failed: "Geri yükleme testi başarısız",
        storage_drift_detected: "Depolama tutarsızlığı bulundu",
        retention_deleted: "Yedek saklama kuralıyla silindi",
      },
    },
    settings: { nav_integrity: "Bütünlük" },
    trust: {
      verified: "Doğrulandı · {when}",
      verify_mismatch: "Sağlama toplamı uyuşmuyor",
      verify_error: "Doğrulanamadı",
      verify_error_detail: "Doğrulama çalışamadı: {error}",
      never_verified: "Hiç doğrulanmadı",
      imported: "İçe aktarıldı",
      pinned: "Sabitlendi",
      pinned_title: "{by} sabitledi: {note}",
      pinned_title_no_note: "{by} sabitledi",
      restore_test_ok: "Son restore testi: başarılı · {when}",
      restore_test_mismatch: "Son restore testi: farklar var · {when}",
      restore_test_error: "Son restore testi: başarısız · {when}",
      last_restore_test: "Son restore testi",
      verification: "Doğrulama",
      verified_at: "Son doğrulama",
      pin: "Sabitleme",
      verify_now: "Şimdi doğrula",
      pin_action: "Sabitle (yasal saklama)",
      unpin_action: "Sabitlemeyi kaldır",
      pin_prompt: "Bu yedeği sabitleyin. Saklama kuralı onu asla silmez ve sabitleme kaldırılana kadar silinemez.\n\nNot (isteğe bağlı):",
      unpin_confirm: "Sabitleme kaldırılsın mı? Saklama ve silme bu yedeğe yeniden uygulanır.",
      pinned_toast: "Yedek sabitlendi",
      unpinned_toast: "Sabitleme kaldırıldı",
      verify_started: "Doğrulama başladı",
      verify_done_ok: "{id} doğrulandı: arşiv sağlama toplamıyla eşleşiyor",
      verify_done_bad: "{id} doğrulanamadı: {error}",
      job_section: "Bütünlük",
      job_verify: "Yüklemeden sonra doğrula",
      job_verify_inherit: "Ayarı kullan ({value})",
      job_verify_on: "Her zaman",
      job_verify_off: "Asla",
      job_verify_hint: "Her yedekten sonra depolanan arşivi yeniden okur; baytlar yazılandan farklıysa yedeği başarısız sayar.",
      rt_enabled: "Otomatik geri yükleme testi",
      rt_enabled_hint: "Zamanlanmış bir yedekten sonra onu geçici bir <db>_rescue_verify_<zaman> veritabanına geri yükler, sayıları ve indeksleri karşılaştırır, sonra siler. Oluşturma ve silme yetkisi gerekir.",
      rt_frequency: "Sıklık",
      rt_daily: "Günlük",
      rt_weekly: "Haftalık",
      rt_monthly: "Aylık",
      rt_every_n: "Her N yedekte bir",
      rt_every_n_label: "N",
      rt_connection: "Test sunucusu",
      rt_connection_same: "Yedeğin kendi sunucusu",
      rt_policy: "Geri yükleme testi",
      rt_off: "Kapalı",
      rt_policy_text: "{freq} · {server}",
      rt_policy_every: "Her {n} yedekte bir",
      retention_preview_n: "Bu ayarla {n} yedek silinecek.",
      retention_preview_none: "Bu ayarla hiçbir yedek silinmeyecek.",
      retention_preview_protected: "{n} yedek korunuyor: sabitlenmiş veya en son doğrulanmış.",
      retention_history: "Saklama geçmişi",
      retention_empty: "Saklama kuralı bu işin hiçbir yedeğini silmedi.",
      retention_entry: "{id} · {detail}",
      retention_entry_error: "arşiv depolamadan silinemedi: {error}",
      restore_tests_title: "Geri yükleme testleri",
      restore_tests_empty: "Henüz geri yükleme testi yok.",
      run_restore_test: "Şimdi geri yükleme testi yap",
      restore_test_started: "{id} için geri yükleme testi başladı",
      rt_status_ok: "Başarılı",
      rt_status_mismatch: "Farklar var",
      rt_status_error: "Başarısız",
      rt_dropped_failed: "geçici veritabanı {db} silinemedi: {error}",
      integrity_title: "Bütünlük",
      integrity_desc: "Yedeklerin geri yüklenebildiğinin kanıtı: arşivler yeniden okunup yazıldıkları andaki sağlama toplamıyla karşılaştırılır, depolama da yedek kayıtlarıyla karşılaştırılır.",
      verify_after_backup: "Her yedeği yüklemeden sonra doğrula",
      verify_after_backup_hint: "Depolanan arşivi yazılandan farklı olan yedek başarısız sayılır. İşler bunu değiştirebilir.",
      verify_decrypt: "Doğrularken şifreli arşivlerin şifresini de çöz",
      verify_decrypt_hint: "Yapılandırılmış anahtarların arşivleri hâlâ açtığını kanıtlar. Şifreleme altında kimlik veya parola gerekir.",
      sweep_schedule: "Bütünlük taraması",
      sweep_schedule_hint: "Tamamlanmış her yedeği, en uzun süredir doğrulanmamış olandan başlayarak tek tek yeniden doğrular. Bulut depolamada arşivleri okumak çıkış trafiği ücreti doğurur.",
      sweep_off: "Kapalı",
      sweep_daily: "Günlük",
      sweep_weekly: "Haftalık",
      sweep_monthly: "Aylık",
      sweep_bandwidth: "Tarama bant genişliği sınırı (MiB/s)",
      sweep_bandwidth_hint: "0 sınırsız demektir.",
      storage_scan: "Depolama hedeflerini haftalık tara",
      storage_scan_hint: "Her hedefi listeler; kaydı olmayan (yetim) ve kaydı olup arşivi olmayan (kayıp) arşivleri raporlar. Hiçbir şey silinmez.",
      sweep_status: "Bütünlük taraması",
      sweep_never: "Henüz bütünlük taraması yapılmadı.",
      sweep_running: "Çalışıyor: {total} yedekten {done} doğrulandı",
      sweep_finished: "Son bütünlük taraması {when}: {ok} sağlam, {mismatch} uyuşmayan, {errors} doğrulanamayan ({total} yedek)",
      sweep_interrupted: "Yarıda kaldı: {reason}",
      sweep_next: "Sonraki bütünlük taraması {when}",
      run_sweep: "Bütünlük taramasını başlat",
      sweep_started: "Bütünlük taraması başladı",
      scans_title: "Depolama taramaları",
      scans_desc: "Bir hedefin arşivlerini yedek kayıtlarıyla karşılaştırın. Yetim arşivler içe aktarılabilir; kayıp arşivler işaretlenir, asla silinmez.",
      scan_now: "Şimdi tara",
      scan_never: "Henüz taranmadı",
      scan_result: "{when} tarandı: {objects} arşiv, {orphans} yetim, {missing} kayıp",
      scan_failed: "Tarama başarısız: {error}",
      scan_done: "{name} taraması bitti: {orphans} yetim, {missing} kayıp",
      orphans: "Yetim arşivler",
      missing: "Kayıp arşivler",
      import: "İçe aktar",
      import_confirm: "{key} için yedek kaydı oluşturulsun mu? Arşivin özeti alınır, içe aktarılmış ve doğrulanmamış olarak işaretlenir.",
      import_started: "İçe aktarma başladı",
      next_scan: "Sonraki depolama taraması {when}",
      drift_title: "Depolama ile yedek kayıtları uyuşmuyor",
      drift_text: "{targets}: yetim veya kayıp arşivler bulundu. Ayarlar → Depolama altında inceleyin.",
      drift_open: "Depolamayı incele",
      busy: "Zaten çalışıyor",
    },
  },
};

// mergeTranslations deep-merges src into dst without replacing existing strings.
function mergeTranslations(dst, src) {
  Object.keys(src).forEach(k => {
    if (typeof src[k] === "object" && src[k] !== null) {
      if (typeof dst[k] !== "object" || dst[k] === null) dst[k] = {};
      mergeTranslations(dst[k], src[k]);
    } else if (dst[k] === undefined) {
      dst[k] = src[k];
    }
  });
}

if (typeof translations === "object") {
  Object.keys(TRUST_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], TRUST_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const trust = {
  // GET /api/v1/integrity: sweep status and the latest scan of every target.
  status: null,
  // Per-job lists fetched for the job details dialog.
  retentionLog: { jobId: "", entries: null, at: 0, seq: 0 },
  restoreTests: { jobId: "", entries: null, at: 0, seq: 0 },
  // Retention preview of the job form.
  preview: { timer: null, seq: 0 },
  // Backups whose on-demand verification this browser started: id -> verified_at before.
  verifying: new Map(),
  verifyTimer: null,
  // Storage scans in progress from this browser.
  scanning: new Set(),
  integrityDirty: false
};

const TRUST_LIST_TTL_MS = 30000;
const TRUST_PIN_PATH = '<path d="M9.5 2.5l4 4-2 1-2.5 2.5.5 3-1 1-2.5-3.5L3 13.5l-.5-.5L6 9.5 2.5 7l1-1 3 .5L9 4l.5-1.5z"/>';

function trustIcon(paths, extraClass) {
  const cls = extraClass ? `icon ${extraClass}` : "icon";
  return `<svg class="${cls}" viewBox="0 0 16 16" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">${paths}</svg>`;
}

function trustWhen(value) {
  const d = parseDate(value);
  return d ? formatRelative(d) : "";
}

// ---------------------------------------------------------------------------
// Backups: badges, menu items, details
// ---------------------------------------------------------------------------

// Pin icon shown next to a pinned backup's ID.
function trustPinIcon(b) {
  if (!b || !b.pinned) return "";
  const by = b.pinned_by || "?";
  const title = b.pin_note ? tf("trust.pinned_title", { by, note: b.pin_note }) : tf("trust.pinned_title_no_note", { by });
  return `<span class="pin-icon" title="${escapeHtml(title)}">${trustIcon(TRUST_PIN_PATH)}<span class="sr-only">${escapeHtml(t("trust.pinned"))}</span></span>`;
}

// Verification, restore test and import chips under a backup's status.
function trustBackupBadges(b) {
  if (!b) return "";
  const chips = [];
  if (b.verification === "ok") {
    chips.push(`<span class="trust-chip trust-ok" title="${escapeHtml(absoluteWithRelative(b.verified_at))}">${icon("check", "badge-icon")}${escapeHtml(tf("trust.verified", { when: trustWhen(b.verified_at) }))}</span>`);
  } else if (b.verification === "mismatch") {
    chips.push(`<span class="trust-chip trust-bad" title="${escapeHtml(b.verification_error || "")}">${icon("x", "badge-icon")}${escapeHtml(t("trust.verify_mismatch"))}</span>`);
  } else if (b.verification === "error") {
    chips.push(`<span class="trust-chip trust-warn" title="${escapeHtml(tf("trust.verify_error_detail", { error: b.verification_error || "" }))}">${escapeHtml(t("trust.verify_error"))}</span>`);
  }
  const rt = b.last_restore_test;
  if (rt && rt.status) {
    const cls = rt.status === "ok" ? "trust-ok" : "trust-bad";
    const key = rt.status === "ok" ? "trust.restore_test_ok" : rt.status === "mismatch" ? "trust.restore_test_mismatch" : "trust.restore_test_error";
    chips.push(`<span class="trust-chip ${cls}" title="${escapeHtml(rt.detail || absoluteWithRelative(rt.at))}">${escapeHtml(tf(key, { when: trustWhen(rt.at) }))}</span>`);
  }
  if (b.imported) chips.push(`<span class="trust-chip">${escapeHtml(t("trust.imported"))}</span>`);
  return chips.length > 0 ? `<div class="trust-chips">${chips.join("")}</div>` : "";
}

// Row menu items of a backup: verify, pin or unpin.
function trustBackupMenuItems(id) {
  const b = state.backups.find(x => x.id === id);
  if (!b) return [];
  const items = [];
  if (b.status === "completed" && b.sha256) items.push(["trust-verify", t("trust.verify_now")]);
  items.push(b.pinned ? ["trust-unpin", t("trust.unpin_action")] : ["trust-pin", t("trust.pin_action")]);
  return items;
}

// Extra rows of the backup details dialog.
function trustBackupDetailRows(b, row) {
  if (!b) return;
  if (b.status === "completed" || b.verification) {
    let verification = t("trust.never_verified");
    if (b.verification === "ok") verification = tf("trust.verified", { when: trustWhen(b.verified_at) });
    if (b.verification === "mismatch") verification = `${t("trust.verify_mismatch")}: ${b.verification_error || ""}`;
    if (b.verification === "error") verification = tf("trust.verify_error_detail", { error: b.verification_error || "" });
    row(t("trust.verification"), verification);
  }
  if (b.pinned) {
    row(t("trust.pin"), b.pin_note ? tf("trust.pinned_title", { by: b.pinned_by || "?", note: b.pin_note }) : tf("trust.pinned_title_no_note", { by: b.pinned_by || "?" }));
  }
  const rt = b.last_restore_test;
  if (rt && rt.status) row(t("trust.last_restore_test"), trustRestoreTestText(rt));
  if (b.imported) row(t("trust.imported"), absoluteWithRelative(b.imported_at));
}

function trustRestoreTestText(rt) {
  const key = rt.status === "ok" ? "trust.restore_test_ok" : rt.status === "mismatch" ? "trust.restore_test_mismatch" : "trust.restore_test_error";
  const text = tf(key, { when: trustWhen(rt.at) });
  return rt.detail ? `${text} — ${rt.detail}` : text;
}

async function trustVerify(id) {
  const b = state.backups.find(x => x.id === id);
  try {
    const json = await apiJSON(`/api/v1/backups/${encodeURIComponent(id)}/verify`, { method: "POST" });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    trust.verifying.set(id, b ? b.verified_at || "" : "");
    showToast(t("trust.verify_started"), "info");
    trustPollVerifications();
  } catch (err) {
    showToast(err.message, "error");
  }
}

// Polls the backups until every verification started here has a new verified_at.
function trustPollVerifications() {
  if (trust.verifying.size === 0 || trust.verifyTimer) return;
  trust.verifyTimer = setTimeout(async () => {
    trust.verifyTimer = null;
    await loadBackups();
    trust.verifying.forEach((before, id) => {
      const b = state.backups.find(x => x.id === id);
      if (!b) {
        trust.verifying.delete(id);
        return;
      }
      if ((b.verified_at || "") === before) return;
      trust.verifying.delete(id);
      if (b.verification === "ok") showToast(tf("trust.verify_done_ok", { id: shortBackupId(id) }), "success");
      else showToast(tf("trust.verify_done_bad", { id: shortBackupId(id), error: truncate(b.verification_error || b.verification || "", 160) }), "error");
    });
    trustPollVerifications();
  }, ACTIVE_POLL_MS);
}

async function trustPin(id) {
  const note = window.prompt(t("trust.pin_prompt"), "");
  if (note === null) return;
  await trustPinRequest(id, "pin", { note }, "trust.pinned_toast");
}

async function trustUnpin(id) {
  if (!(await confirmDialog({ body: t("trust.unpin_confirm"), confirmLabel: t("dialog.confirm") }))) return;
  await trustPinRequest(id, "unpin", null, "trust.unpinned_toast");
}

async function trustPinRequest(id, action, body, toastKey) {
  try {
    const json = await apiJSON(`/api/v1/backups/${encodeURIComponent(id)}/${action}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: body ? JSON.stringify(body) : undefined
    });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    showToast(t(toastKey), "success");
    await loadBackups();
  } catch (err) {
    showToast(err.message, "error");
  }
}

// ---------------------------------------------------------------------------
// Jobs: details, form, retention preview
// ---------------------------------------------------------------------------

function trustFrequencyText(policy) {
  if (!policy) return "";
  if (policy.frequency === "every_n") return tf("trust.rt_policy_every", { n: policy.every_n || 1 });
  return t(`trust.rt_${policy.frequency || "weekly"}`);
}

function trustVerifyDefault() {
  const integrity = settingsGroup("integrity");
  return integrity.verify_after_backup === false ? t("trust.job_verify_off") : t("trust.job_verify_on");
}

// Latest restore test chip under a job's last run in the jobs table.
function trustJobChip(job) {
  const rt = job && job.last_restore_test;
  if (!rt || !rt.status) return "";
  const cls = rt.status === "ok" ? "trust-ok" : "trust-bad";
  return `<div class="trust-chips"><span class="trust-chip ${cls}" title="${escapeHtml(rt.detail || absoluteWithRelative(rt.at))}">${escapeHtml(trustRestoreTestText({ ...rt, detail: "" }))}</span></div>`;
}

// Integrity rows and sections of the job details dialog.
function trustJobDetails(job, options) {
  const v = job.verify_after_backup || "";
  appendKv(options, t("trust.job_verify"), v === "on" ? t("trust.job_verify_on") : v === "off" ? t("trust.job_verify_off") : tf("trust.job_verify_inherit", { value: trustVerifyDefault() }));
  const rt = job.restore_test;
  appendKv(options, t("trust.rt_policy"), rt && rt.enabled
    ? tf("trust.rt_policy_text", { freq: trustFrequencyText(rt), server: rt.connection_id ? connectionName(rt.connection_id) : t("trust.rt_connection_same") })
    : t("trust.rt_off"));
  appendKv(options, t("trust.last_restore_test"), job.last_restore_test ? trustRestoreTestText(job.last_restore_test) : "");

  const run = document.getElementById("job-details-restore-test");
  if (run) run.dataset.id = job.id;
  trustLoadJobList(trust.restoreTests, job, `/api/v1/jobs/${encodeURIComponent(job.id)}/restore-tests?limit=10`, trustRenderRestoreTests);
  trustLoadJobList(trust.retentionLog, job, `/api/v1/jobs/${encodeURIComponent(job.id)}/retention/log?limit=50`, trustRenderRetentionLog);
}

// Fetches a per-job list once per TRUST_LIST_TTL_MS (or when the job or its last
// restore test changed) and renders it.
function trustLoadJobList(list, job, url, render) {
  const key = `${job.id}|${job.last_restore_test ? job.last_restore_test.id : ""}|${job.last_run || ""}`;
  if (list.jobId === key && Date.now() - list.at < TRUST_LIST_TTL_MS) {
    render(list.entries);
    return;
  }
  list.jobId = key;
  list.at = Date.now();
  const seq = ++list.seq;
  if (list.entries === null) render(null);
  apiJSON(url).then(json => {
    if (seq !== list.seq) return;
    list.entries = json.success ? (json.data || []) : [];
    render(list.entries);
  }).catch(() => {
    if (seq === list.seq) render([]);
  });
}

function trustRenderRestoreTests(entries) {
  const box = document.getElementById("job-details-restore-tests");
  if (!box) return;
  if (entries === null) {
    box.innerHTML = `<p class="muted">${escapeHtml(t("tables.loading"))}</p>`;
    return;
  }
  if (entries.length === 0) {
    box.innerHTML = `<p class="muted">${escapeHtml(t("trust.restore_tests_empty"))}</p>`;
    return;
  }
  box.innerHTML = `<ul class="trust-list">${entries.map(r => {
    const kind = r.status === "ok" ? "success" : "danger";
    const label = t(`trust.rt_status_${r.status}`);
    const lines = [];
    if (r.error) lines.push(r.error);
    (r.mismatches || []).forEach(m => lines.push(m));
    if (r.drop_error) lines.push(tf("trust.rt_dropped_failed", { db: r.temp_database || "", error: r.drop_error }));
    const detail = lines.length > 0 ? `<div class="cell-sub cell-error">${lines.map(escapeHtml).join("<br>")}</div>` : "";
    return `<li>${statusBadge(kind, label)} <span>${escapeHtml(absoluteWithRelative(r.completed_at || r.started_at))}</span>
      <span class="muted"> · ${escapeHtml(shortBackupId(r.backup_id))} · ${escapeHtml(formatDuration(r.duration_seconds))}</span>${detail}</li>`;
  }).join("")}</ul>`;
}

function trustRenderRetentionLog(entries) {
  const box = document.getElementById("job-details-retention-log");
  if (!box) return;
  if (entries === null) {
    box.innerHTML = `<p class="muted">${escapeHtml(t("tables.loading"))}</p>`;
    return;
  }
  if (entries.length === 0) {
    box.innerHTML = `<p class="muted">${escapeHtml(t("trust.retention_empty"))}</p>`;
    return;
  }
  box.innerHTML = `<ul class="trust-list">${entries.map(e => {
    const err = e.error ? `<div class="cell-sub cell-error">${escapeHtml(tf("trust.retention_entry_error", { error: e.error }))}</div>` : "";
    return `<li><time title="${escapeHtml(absoluteWithRelative(e.time))}">${escapeHtml(trustWhen(e.time))}</time>
      <span class="mono" title="${escapeHtml(e.backup_id)}"> ${escapeHtml(shortBackupId(e.backup_id))}</span>
      <span class="muted"> · ${escapeHtml(e.detail || e.reason || "")} · ${escapeHtml(formatBytes(e.size_bytes))}</span>${err}</li>`;
  }).join("")}</ul>`;
}

async function trustStartRestoreTest(jobID, btn) {
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}/restore-test`, { method: "POST" });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    showToast(tf("trust.restore_test_started", { id: shortBackupId((json.data && json.data.backup_id) || "") }), "info");
    trust.restoreTests.at = 0;
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    if (btn) btn.disabled = false;
  }
}

// Prefills the integrity fields of the job form (job is null for a new job).
function trustFillJobForm(job) {
  const verify = document.getElementById("job-verify");
  if (verify) {
    const inherit = verify.querySelector('option[value=""]');
    if (inherit) inherit.textContent = tf("trust.job_verify_inherit", { value: trustVerifyDefault() });
    verify.value = job && job.verify_after_backup ? job.verify_after_backup : "";
  }
  const rt = (job && job.restore_test) || {};
  const enabled = document.getElementById("job-rt-enabled");
  if (enabled) enabled.checked = !!rt.enabled;
  setValue("job-rt-frequency", rt.frequency || "weekly");
  setValue("job-rt-every-n", rt.every_n || 7);
  const conn = document.getElementById("job-rt-connection");
  if (conn) {
    conn.textContent = "";
    const same = document.createElement("option");
    same.value = "";
    same.textContent = t("trust.rt_connection_same");
    conn.appendChild(same);
    state.connections.forEach(c => {
      const opt = document.createElement("option");
      opt.value = c.id;
      opt.textContent = c.name || c.id;
      conn.appendChild(opt);
    });
    conn.value = rt.connection_id || "";
  }
  trustUpdateRestoreTestFields();
  const hint = document.getElementById("job-retention-preview");
  if (hint) {
    hint.hidden = true;
    hint.textContent = "";
  }
  if (job) trustScheduleRetentionPreview();
}

function trustUpdateRestoreTestFields() {
  const enabled = document.getElementById("job-rt-enabled");
  const on = !!(enabled && enabled.checked);
  const group = document.getElementById("job-rt-fields");
  if (group) group.hidden = !on;
  const everyN = document.getElementById("job-rt-every-n-group");
  if (everyN) everyN.hidden = getValue("job-rt-frequency") !== "every_n";
}

// The integrity fields of the job form, for the create/update payload.
function trustJobPayload() {
  const enabled = document.getElementById("job-rt-enabled");
  const frequency = getValue("job-rt-frequency") || "weekly";
  const policy = {
    enabled: !!(enabled && enabled.checked),
    frequency,
    connection_id: getValue("job-rt-connection")
  };
  if (frequency === "every_n") policy.every_n = parseInt(getValue("job-rt-every-n"), 10) || 1;
  return { verify_after_backup: getValue("job-verify"), restore_test: policy };
}

// Previews, while an existing job is edited, how many backups its retention
// would delete with the values in the form.
function trustScheduleRetentionPreview() {
  clearTimeout(trust.preview.timer);
  trust.preview.timer = setTimeout(trustRetentionPreview, 350);
}

async function trustRetentionPreview() {
  const hint = document.getElementById("job-retention-preview");
  if (!hint || !editingJobId) return;
  const days = parseInt(getValue("job-retention-days"), 10) || 0;
  const count = parseInt(getValue("job-retention-count"), 10) || 0;
  const seq = ++trust.preview.seq;
  try {
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(editingJobId)}/retention/preview?retention_days=${days}&retention_count=${count}`);
    if (seq !== trust.preview.seq || !json.success || !json.data) return;
    const n = (json.data.delete || []).length;
    const kept = (json.data.protected || []).length;
    let text = n > 0 ? tf("trust.retention_preview_n", { n }) : t("trust.retention_preview_none");
    if (kept > 0) text += ` ${tf("trust.retention_preview_protected", { n: kept })}`;
    hint.textContent = text;
    hint.classList.toggle("text-danger", n > 0);
    hint.hidden = false;
  } catch (err) {
    hint.hidden = true;
  }
}

// ---------------------------------------------------------------------------
// Integrity status: sweep, storage scans, dashboard warning
// ---------------------------------------------------------------------------

async function trustRefresh() {
  if (!auth.user) return;
  try {
    const json = await apiJSON("/api/v1/integrity");
    if (json.success) trust.status = json.data || null;
  } catch (err) {
    console.error("Failed to load integrity status:", err);
  }
  trustRenderDriftWarning();
  trustRenderSweepStatus();
  trustRenderScans();
}

function trustScan(targetID) {
  const scans = trust.status && Array.isArray(trust.status.scans) ? trust.status.scans : [];
  return scans.find(s => s.target_id === targetID) || null;
}

function trustRenderDriftWarning() {
  const banner = document.getElementById("drift-warning");
  if (!banner) return;
  const scans = trust.status && Array.isArray(trust.status.scans) ? trust.status.scans : [];
  const drifted = scans.filter(s => !s.error && (s.orphan_count > 0 || s.missing_count > 0));
  banner.hidden = drifted.length === 0;
  setText("drift-warning-text", tf("trust.drift_text", { targets: drifted.map(s => s.target_name || s.target_id).join(", ") }));
}

function trustRenderSweepStatus() {
  const box = document.getElementById("integrity-sweep-status");
  if (!box) return;
  const st = trust.status && trust.status.sweep ? trust.status.sweep : null;
  const lines = [];
  if (!st || !st.started_at) {
    lines.push(t("trust.sweep_never"));
  } else if (st.running) {
    lines.push(tf("trust.sweep_running", { done: st.done || 0, total: st.total || 0 }));
  } else {
    lines.push(tf("trust.sweep_finished", { when: trustWhen(st.finished_at || st.started_at), ok: st.ok || 0, mismatch: st.mismatch || 0, errors: st.errors || 0, total: st.total || 0 }));
    if (st.interrupted) lines.push(tf("trust.sweep_interrupted", { reason: st.interrupted }));
  }
  if (st && st.next_run_at) lines.push(tf("trust.sweep_next", { when: trustWhen(st.next_run_at) }));
  if (trust.status && trust.status.next_scan_at) lines.push(tf("trust.next_scan", { when: trustWhen(trust.status.next_scan_at) }));
  const alarming = !!(st && !st.running && st.mismatch > 0);
  box.innerHTML = lines.map((l, i) => `<p${i === 0 && alarming ? ' class="text-danger"' : ""}>${escapeHtml(l)}</p>`).join("");
  const run = document.getElementById("integrity-run-sweep");
  if (run) run.disabled = !!(st && st.running);
}

// Storage scan panel of Settings → Storage.
function trustRenderScans() {
  const box = document.getElementById("storage-scans");
  if (!box || !state.loaded.storageTargets) return;
  if (state.storageTargets.length === 0) {
    box.innerHTML = "";
    return;
  }
  box.innerHTML = state.storageTargets.map(target => {
    const scan = trustScan(target.id);
    const busy = trust.scanning.has(target.id);
    let summary = t("trust.scan_never");
    if (scan && scan.error) summary = tf("trust.scan_failed", { error: scan.error });
    else if (scan) summary = tf("trust.scan_result", { when: trustWhen(scan.scanned_at), objects: scan.objects || 0, orphans: scan.orphan_count || 0, missing: scan.missing_count || 0 });
    const orphans = scan && (scan.orphans || []).length > 0
      ? `<h5 class="trust-subheading">${escapeHtml(t("trust.orphans"))}</h5><ul class="trust-list">${scan.orphans.map(o => `<li>
          <span class="mono ellipsis" title="${escapeHtml(o.key)}">${escapeHtml(o.key)}</span>
          <span class="muted"> · ${escapeHtml(formatBytes(o.size_bytes))}${o.record_id ? ` · ${escapeHtml(o.record_id)} (${escapeHtml(o.record_status)})` : ""}</span>
          <button type="button" class="btn btn-secondary btn-sm" data-action="trust-import" data-id="${escapeHtml(target.id)}" data-key="${escapeHtml(o.key)}">${escapeHtml(t("trust.import"))}</button>
        </li>`).join("")}</ul>`
      : "";
    const missing = scan && (scan.missing || []).length > 0
      ? `<h5 class="trust-subheading">${escapeHtml(t("trust.missing"))}</h5><ul class="trust-list">${scan.missing.map(m => `<li>
          <button type="button" class="link-btn mono" data-action="backup-details" data-id="${escapeHtml(m.backup_id)}">${escapeHtml(shortBackupId(m.backup_id))}</button>
          <span class="muted"> · ${escapeHtml(m.database)} · ${escapeHtml(m.storage_key)}</span></li>`).join("")}</ul>`
      : "";
    const cls = scan && !scan.error && (scan.orphan_count > 0 || scan.missing_count > 0) ? " text-danger" : "";
    return `<div class="trust-scan">
      <div class="trust-scan-head">
        <div><strong>${escapeHtml(target.name)}</strong><div class="cell-sub${cls}">${escapeHtml(summary)}</div></div>
        <button type="button" class="btn btn-secondary btn-sm" data-action="trust-scan" data-id="${escapeHtml(target.id)}"${busy ? " disabled" : ""}>${escapeHtml(t("trust.scan_now"))}</button>
      </div>${orphans}${missing}
    </div>`;
  }).join("");
}

async function trustScanTarget(id) {
  trust.scanning.add(id);
  trustRenderScans();
  try {
    const json = await apiJSON(`/api/v1/storage-targets/${encodeURIComponent(id)}/scan`, { method: "POST" });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    const r = json.data || {};
    showToast(tf("trust.scan_done", { name: r.target_name || id, orphans: r.orphan_count || 0, missing: r.missing_count || 0 }),
      r.orphan_count > 0 || r.missing_count > 0 ? "error" : "success");
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    trust.scanning.delete(id);
    await trustRefresh();
    loadBackups();
  }
}

async function trustImport(targetID, key) {
  if (!key || !(await confirmDialog({ body: tf("trust.import_confirm", { key }), confirmLabel: t("dialog.confirm") }))) return;
  try {
    const json = await apiJSON(`/api/v1/storage-targets/${encodeURIComponent(targetID)}/import`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ key })
    });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    showToast(t("trust.import_started"), "info");
    trackBackup(json.data);
    loadBackups();
  } catch (err) {
    showToast(err.message, "error");
  }
}

async function trustRunSweep(btn) {
  if (btn) btn.disabled = true;
  try {
    const json = await apiJSON("/api/v1/integrity/sweep", { method: "POST" });
    if (!json.success) {
      showToast(json.httpStatus === 409 ? t("trust.busy") : json.error || t("toasts.request_failed"), "error");
      return;
    }
    showToast(t("trust.sweep_started"), "info");
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    if (btn) btn.disabled = false;
    setTimeout(trustRefresh, 1500);
  }
}

// ---------------------------------------------------------------------------
// Settings → Integrity
// ---------------------------------------------------------------------------

// Fills the integrity settings form unless the operator is editing it.
function trustFillSettings(force) {
  const form = document.getElementById("form-integrity");
  if (!form) return;
  const submit = form.querySelector("[type=submit]");
  if (submit) submit.disabled = !state.loaded.settings;
  if (!state.loaded.settings || (trust.integrityDirty && !force)) return;
  trust.integrityDirty = false;
  const s = settingsGroup("integrity");
  document.getElementById("integrity-verify-after-backup").checked = s.verify_after_backup !== false;
  document.getElementById("integrity-verify-decrypt").checked = !!s.verify_decrypt;
  setValue("integrity-sweep-schedule", s.sweep_schedule || "off");
  setValue("integrity-sweep-bandwidth", Number(s.sweep_bandwidth_limit) || 0);
  document.getElementById("integrity-storage-scan").checked = s.storage_scan !== false;
  hideFormError("integrity-error");
}

async function trustSaveSettings(e) {
  e.preventDefault();
  const bandwidth = parseInt(getValue("integrity-sweep-bandwidth"), 10);
  if (isNaN(bandwidth) || bandwidth < 0) {
    showFormError("integrity-error", t("trust.sweep_bandwidth_hint"));
    return;
  }
  const payload = {
    integrity: {
      verify_after_backup: document.getElementById("integrity-verify-after-backup").checked,
      verify_decrypt: document.getElementById("integrity-verify-decrypt").checked,
      sweep_schedule: getValue("integrity-sweep-schedule"),
      sweep_bandwidth_limit: bandwidth,
      storage_scan: document.getElementById("integrity-storage-scan").checked
    }
  };
  const submit = e.submitter || document.querySelector("#form-integrity [type=submit]");
  if (submit) submit.disabled = true;
  try {
    const json = await apiJSON("/api/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    });
    if (!json.success) {
      showFormError("integrity-error", json.error || t("toasts.request_failed"));
      return;
    }
    hideFormError("integrity-error");
    // The response carries every settings group; only this form is refilled, so
    // unsaved edits in the other settings forms stay.
    if (json.data && json.data.integrity) state.settings = { ...(state.settings || {}), ...json.data };
    trust.integrityDirty = false;
    trustFillSettings(true);
    setText("integrity-status", t("settings.saved"));
    setTimeout(() => setText("integrity-status", ""), 3000);
    trustRefresh();
  } catch (err) {
    showFormError("integrity-error", err.message);
  } finally {
    if (submit) submit.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function trustSetup() {
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-action^='trust-']");
    if (!btn || btn.disabled) return;
    const id = btn.dataset.id || "";
    switch (btn.dataset.action) {
      case "trust-verify":
        trustVerify(id);
        break;
      case "trust-pin":
        trustPin(id);
        break;
      case "trust-unpin":
        trustUnpin(id);
        break;
      case "trust-restore-test":
        trustStartRestoreTest(id, btn);
        break;
      case "trust-run-sweep":
        trustRunSweep(btn);
        break;
      case "trust-scan":
        trustScanTarget(id);
        break;
      case "trust-import":
        trustImport(id, btn.dataset.key || "");
        break;
      case "trust-open-storage":
        activateTab("tab-settings", false);
        showSettingsSection("storage", true);
        break;
    }
  });
  const form = document.getElementById("form-integrity");
  if (form) {
    form.addEventListener("submit", trustSaveSettings);
    form.addEventListener("input", () => { trust.integrityDirty = true; });
    form.addEventListener("change", () => { trust.integrityDirty = true; });
  }
  ["job-retention-days", "job-retention-count"].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.addEventListener("input", trustScheduleRetentionPreview);
  });
  ["job-rt-enabled", "job-rt-frequency"].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.addEventListener("change", trustUpdateRestoreTestFields);
  });
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      trustRenderDriftWarning();
      trustRenderSweepStatus();
      trustRenderScans();
    });
  }
}

document.addEventListener("DOMContentLoaded", trustSetup);
