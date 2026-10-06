/**
 * MongoRescue dashboard: immutable backups (S3 Object Lock).
 *
 * An S3 storage target can lock every uploaded object in governance or compliance
 * mode for a number of days, and set an S3 legal hold on the archive of a pinned
 * backup. The server checks the bucket (Object Lock and versioning enabled) when the
 * target is saved or tested and keeps a deleted backup until its lock ends; the
 * dashboard shows the settings, the "locked until" date and the target hints.
 *
 * Loaded after app.js, trust.js (mergeTranslations) and protection.js. Server values reach the DOM
 * only through escapeHtml() or textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js), in every dashboard language
// ---------------------------------------------------------------------------

const OBJECT_LOCK_TRANSLATIONS = {
  en: {
    objectlock: {
      label: "Object Lock (immutable backups)",
      mode_none: "None",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "Needs a bucket created with Object Lock enabled; MongoRescue checks it on save and never enables it. Compliance locks cannot be shortened or removed by anyone, including the bucket owner. Lowering the lock takes effect only after the delete grace period.",
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
      job_warning: "Job saved. {warning}",
      no_lock: "no object lock",
      lock_text: "{mode}, {days} days",
      pending: "Object lock of storage target {target} lowered to {value}, effective {when}",
      lowered_pending: "Saved. The lower object lock takes effect {when}, after the delete grace period.",
      lowered_approval: "Saved. Lowering the object lock waits for a second administrator's approval.",
      hint_no_object_lock: "Backups can be deleted with the bucket credentials; enable S3 Object Lock to make them immutable.",
      hint_governance_bypass: "Governance mode: principals with s3:BypassGovernanceRetention can still delete locked backups; use compliance mode to stop everyone.",
      hint_local_not_immutable: "Local storage has no immutability option; use an S3 target with Object Lock for immutable backups."
    }
  },
  tr: {
    objectlock: {
      label: "Object Lock (değiştirilemez yedekler)",
      mode_none: "Yok",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "Object Lock etkin oluşturulmuş bir kova gerekir; MongoRescue kaydederken denetler, kendisi asla etkinleştirmez. Compliance kilitlerini kova sahibi dahil kimse kısaltamaz veya kaldıramaz. Kilidi düşürmek ancak silme bekleme süresinden sonra geçerli olur.",
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
      job_warning: "Görev kaydedildi. {warning}",
      no_lock: "nesne kilidi yok",
      lock_text: "{mode}, {days} gün",
      pending: "{target} depolama hedefinin nesne kilidi {value} olarak düşürüldü, geçerlilik {when}",
      lowered_pending: "Kaydedildi. Daha düşük nesne kilidi silme bekleme süresinden sonra, {when} geçerli olur.",
      lowered_approval: "Kaydedildi. Nesne kilidini düşürmek ikinci bir yöneticinin onayını bekliyor.",
      hint_no_object_lock: "Yedekler kova kimlik bilgileriyle silinebilir; değiştirilemez yapmak için S3 Object Lock'u etkinleştirin.",
      hint_governance_bypass: "Governance modu: s3:BypassGovernanceRetention yetkisi olanlar kilitli yedekleri yine silebilir; herkesi durdurmak için compliance modunu kullanın.",
      hint_local_not_immutable: "Yerel depolamanın değiştirilemezlik seçeneği yok; değiştirilemez yedekler için Object Lock'lu bir S3 hedefi kullanın."
    }
  },
  de: {
    objectlock: {
      label: "Object Lock (unveränderliche Backups)",
      mode_none: "Keiner",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "Erfordert einen Bucket, der mit aktiviertem Object Lock erstellt wurde; MongoRescue prüft das beim Speichern und aktiviert es nie selbst. Compliance-Sperren kann niemand verkürzen oder entfernen, auch nicht der Bucket-Eigentümer. Eine schwächere Sperre gilt erst nach der Lösch-Karenzzeit.",
      retention_label: "Jeden Upload sperren für (Tage)",
      legal_hold_label: "S3 Legal Hold auf angeheftete Backups setzen",
      err_retention: "Geben Sie eine Anzahl Tage von {min} bis {max} ein.",
      locked_until: "Gesperrt bis {date}",
      locked_until_title: "S3 Object Lock ({mode}): das Archiv kann vor {date} nicht gelöscht werden",
      lock_expired: "Sperre endete {date}",
      legal_hold: "Legal Hold",
      legal_hold_on: "Aktiv (S3 Legal Hold, solange angeheftet)",
      version: "S3-Version",
      chip: "{mode}-Sperre · {days} T",
      deleted_locked: "Gelöscht, wartet auf das Ende der Sperre am {date}; danach bereinigt",
      hints_title: "Speicherschutz",
      job_warning: "Job gespeichert. {warning}",
      no_lock: "keine Objektsperre",
      lock_text: "{mode}, {days} Tage",
      pending: "Objektsperre des Speicherziels {target} auf {value} gesenkt, wirksam {when}",
      lowered_pending: "Gespeichert. Die schwächere Objektsperre gilt ab {when}, nach der Lösch-Karenzzeit.",
      lowered_approval: "Gespeichert. Das Senken der Objektsperre wartet auf die Freigabe eines zweiten Administrators.",
      hint_no_object_lock: "Backups können mit den Bucket-Zugangsdaten gelöscht werden; aktivieren Sie S3 Object Lock, um sie unveränderlich zu machen.",
      hint_governance_bypass: "Governance-Modus: Prinzipale mit s3:BypassGovernanceRetention können gesperrte Backups trotzdem löschen; nutzen Sie den Compliance-Modus, um alle zu stoppen.",
      hint_local_not_immutable: "Lokaler Speicher kennt keine Unveränderlichkeit; nutzen Sie ein S3-Ziel mit Object Lock für unveränderliche Backups."
    }
  },
  es: {
    objectlock: {
      label: "Object Lock (copias inmutables)",
      mode_none: "Ninguno",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "Requiere un bucket creado con Object Lock activado; MongoRescue lo comprueba al guardar y nunca lo activa. Nadie puede acortar ni quitar un bloqueo compliance, ni siquiera el propietario del bucket. Rebajar el bloqueo solo surte efecto tras el periodo de gracia de borrado.",
      retention_label: "Bloquear cada subida durante (días)",
      legal_hold_label: "Poner una retención legal S3 a las copias fijadas",
      err_retention: "Introduzca un número de días de {min} a {max}.",
      locked_until: "Bloqueada hasta {date}",
      locked_until_title: "S3 Object Lock ({mode}): el archivo no se puede borrar antes de {date}",
      lock_expired: "Bloqueo terminado el {date}",
      legal_hold: "Retención legal",
      legal_hold_on: "Activa (retención legal S3 mientras esté fijada)",
      version: "Versión S3",
      chip: "Bloqueo {mode} · {days} d",
      deleted_locked: "Borrada, a la espera de que termine su bloqueo el {date}; luego se purga",
      hints_title: "Protección del almacenamiento",
      job_warning: "Tarea guardada. {warning}",
      no_lock: "sin bloqueo de objetos",
      lock_text: "{mode}, {days} días",
      pending: "Bloqueo de objetos del destino {target} rebajado a {value}, efectivo {when}",
      lowered_pending: "Guardado. El bloqueo de objetos menor surte efecto {when}, tras el periodo de gracia de borrado.",
      lowered_approval: "Guardado. Rebajar el bloqueo de objetos espera la aprobación de un segundo administrador.",
      hint_no_object_lock: "Las copias se pueden borrar con las credenciales del bucket; active S3 Object Lock para hacerlas inmutables.",
      hint_governance_bypass: "Modo governance: quien tenga s3:BypassGovernanceRetention aún puede borrar copias bloqueadas; use el modo compliance para impedírselo a todos.",
      hint_local_not_immutable: "El almacenamiento local no ofrece inmutabilidad; use un destino S3 con Object Lock para copias inmutables."
    }
  },
  fr: {
    objectlock: {
      label: "Object Lock (sauvegardes immuables)",
      mode_none: "Aucun",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "Nécessite un bucket créé avec Object Lock activé ; MongoRescue le vérifie à l'enregistrement et ne l'active jamais. Personne ne peut raccourcir ni retirer un verrou compliance, pas même le propriétaire du bucket. Abaisser le verrou ne prend effet qu'après le délai de grâce de suppression.",
      retention_label: "Verrouiller chaque envoi pendant (jours)",
      legal_hold_label: "Poser une conservation légale S3 sur les sauvegardes épinglées",
      err_retention: "Saisissez un nombre de jours de {min} à {max}.",
      locked_until: "Verrouillée jusqu'au {date}",
      locked_until_title: "S3 Object Lock ({mode}) : l'archive ne peut pas être supprimée avant le {date}",
      lock_expired: "Verrou terminé le {date}",
      legal_hold: "Conservation légale",
      legal_hold_on: "Active (conservation légale S3 tant qu'elle est épinglée)",
      version: "Version S3",
      chip: "Verrou {mode} · {days} j",
      deleted_locked: "Supprimée, en attente de la fin de son verrou le {date} ; purgée ensuite",
      hints_title: "Protection du stockage",
      job_warning: "Tâche enregistrée. {warning}",
      no_lock: "aucun verrou d'objet",
      lock_text: "{mode}, {days} jours",
      pending: "Verrou d'objet de la cible {target} abaissé à {value}, effectif {when}",
      lowered_pending: "Enregistré. Le verrou d'objet plus faible prend effet {when}, après le délai de grâce de suppression.",
      lowered_approval: "Enregistré. L'abaissement du verrou d'objet attend l'approbation d'un second administrateur.",
      hint_no_object_lock: "Les sauvegardes peuvent être supprimées avec les identifiants du bucket ; activez S3 Object Lock pour les rendre immuables.",
      hint_governance_bypass: "Mode governance : les principaux disposant de s3:BypassGovernanceRetention peuvent toujours supprimer les sauvegardes verrouillées ; utilisez le mode compliance pour l'empêcher à tous.",
      hint_local_not_immutable: "Le stockage local n'offre pas d'immuabilité ; utilisez une cible S3 avec Object Lock pour des sauvegardes immuables."
    }
  },
  zh: {
    objectlock: {
      label: "对象锁定（不可变备份）",
      mode_none: "无",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "需要创建时已启用对象锁定的存储桶；MongoRescue 保存时会检查，但绝不会自行启用。Compliance 锁定任何人都无法缩短或解除，包括存储桶所有者。降低锁定仅在删除宽限期后生效。",
      retention_label: "每次上传的锁定天数",
      legal_hold_label: "对固定的备份设置 S3 法律保留",
      err_retention: "请输入 {min} 到 {max} 之间的天数。",
      locked_until: "锁定至 {date}",
      locked_until_title: "S3 对象锁定（{mode}）：{date} 之前无法删除该归档",
      lock_expired: "锁定已于 {date} 结束",
      legal_hold: "法律保留",
      legal_hold_on: "开启（固定期间的 S3 法律保留）",
      version: "S3 版本",
      chip: "{mode} 锁定 · {days} 天",
      deleted_locked: "已删除，等待锁定于 {date} 结束后清除",
      hints_title: "存储保护",
      job_warning: "任务已保存。{warning}",
      no_lock: "无对象锁定",
      lock_text: "{mode}，{days} 天",
      pending: "存储目标 {target} 的对象锁定降为 {value}，于 {when} 生效",
      lowered_pending: "已保存。较低的对象锁定将在删除宽限期后于 {when} 生效。",
      lowered_approval: "已保存。降低对象锁定需等待第二位管理员批准。",
      hint_no_object_lock: "使用存储桶凭据即可删除备份；启用 S3 对象锁定可使其不可变。",
      hint_governance_bypass: "Governance 模式：拥有 s3:BypassGovernanceRetention 的主体仍可删除已锁定的备份；请使用 compliance 模式阻止所有人。",
      hint_local_not_immutable: "本地存储没有不可变选项；如需不可变备份，请使用启用对象锁定的 S3 目标。"
    }
  },
  ja: {
    objectlock: {
      label: "Object Lock（変更不可のバックアップ）",
      mode_none: "なし",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "Object Lock を有効にして作成したバケットが必要です。MongoRescue は保存時に確認しますが、自分で有効にすることはありません。Compliance のロックはバケット所有者を含め誰も短縮・解除できません。ロックの引き下げは削除猶予期間の後に有効になります。",
      retention_label: "各アップロードのロック日数",
      legal_hold_label: "ピン留めしたバックアップに S3 リーガルホールドを設定",
      err_retention: "{min} から {max} までの日数を入力してください。",
      locked_until: "{date} までロック",
      locked_until_title: "S3 Object Lock（{mode}）：{date} より前にアーカイブを削除できません",
      lock_expired: "ロックは {date} に終了",
      legal_hold: "リーガルホールド",
      legal_hold_on: "オン（ピン留め中は S3 リーガルホールド）",
      version: "S3 バージョン",
      chip: "{mode} ロック · {days} 日",
      deleted_locked: "削除済み。{date} のロック終了を待ってから完全削除されます",
      hints_title: "ストレージの保護",
      job_warning: "ジョブを保存しました。{warning}",
      no_lock: "オブジェクトロックなし",
      lock_text: "{mode}、{days} 日",
      pending: "ストレージ先 {target} のオブジェクトロックを {value} に引き下げ、{when} に有効",
      lowered_pending: "保存しました。弱いオブジェクトロックは削除猶予期間の後、{when} に有効になります。",
      lowered_approval: "保存しました。オブジェクトロックの引き下げは 2 人目の管理者の承認待ちです。",
      hint_no_object_lock: "バケットの認証情報でバックアップを削除できます。変更不可にするには S3 Object Lock を有効にしてください。",
      hint_governance_bypass: "Governance モード：s3:BypassGovernanceRetention を持つプリンシパルはロック中のバックアップも削除できます。全員を止めるには compliance モードを使ってください。",
      hint_local_not_immutable: "ローカルストレージには変更不可のオプションがありません。変更不可のバックアップには Object Lock 付きの S3 先を使ってください。"
    }
  },
  ru: {
    objectlock: {
      label: "Object Lock (неизменяемые резервные копии)",
      mode_none: "Нет",
      mode_governance: "Governance",
      mode_compliance: "Compliance",
      form_hint: "Нужен бакет, созданный с включённым Object Lock; MongoRescue проверяет это при сохранении и никогда не включает его сам. Блокировку compliance никто не может сократить или снять, даже владелец бакета. Ослабление блокировки вступает в силу только после периода отсрочки удаления.",
      retention_label: "Блокировать каждую загрузку на (дней)",
      legal_hold_label: "Ставить юридическое удержание S3 на закреплённые копии",
      err_retention: "Введите число дней от {min} до {max}.",
      locked_until: "Заблокирована до {date}",
      locked_until_title: "S3 Object Lock ({mode}): архив нельзя удалить до {date}",
      lock_expired: "Блокировка закончилась {date}",
      legal_hold: "Юридическое удержание",
      legal_hold_on: "Включено (юридическое удержание S3, пока копия закреплена)",
      version: "Версия S3",
      chip: "Блокировка {mode} · {days} дн.",
      deleted_locked: "Удалена, ожидает окончания блокировки {date}; затем будет очищена",
      hints_title: "Защита хранилища",
      job_warning: "Задание сохранено. {warning}",
      no_lock: "без блокировки объектов",
      lock_text: "{mode}, {days} дн.",
      pending: "Блокировка объектов хранилища {target} ослаблена до {value}, вступает в силу {when}",
      lowered_pending: "Сохранено. Более слабая блокировка объектов вступит в силу {when}, после периода отсрочки удаления.",
      lowered_approval: "Сохранено. Ослабление блокировки объектов ждёт одобрения второго администратора.",
      hint_no_object_lock: "Копии можно удалить с учётными данными бакета; включите S3 Object Lock, чтобы сделать их неизменяемыми.",
      hint_governance_bypass: "Режим governance: субъекты с s3:BypassGovernanceRetention всё ещё могут удалять заблокированные копии; используйте режим compliance, чтобы запретить это всем.",
      hint_local_not_immutable: "Локальное хранилище не поддерживает неизменяемость; для неизменяемых копий используйте цель S3 с Object Lock."
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
    const text = t(`objectlock.hint_${h.code}`, String(h.message || ""));
    parts.push(`<span class="${cls}" title="${escapeHtml(text)}">${escapeHtml(truncate(text, 60))}</span>`);
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
    const text = t(`objectlock.hint_${h.code}`, String(h.message || ""));
    return `<li class="${cls}"><strong>${escapeHtml(h.target_name || h.target_id)}</strong>: ${escapeHtml(text)}</li>`;
  }).join("");
  return `<div class="rd-hints"><h4 class="rd-hints-title">${escapeHtml(t("objectlock.hints_title"))}</h4><ul>${items}</ul></div>`;
}

// objectLockJobSaved shows the warnings of a job save (a retention shorter than the
// lock of its storage target).
function objectLockJobSaved(data) {
  const warnings = data && Array.isArray(data.warnings) ? data.warnings : [];
  warnings.forEach(w => showToast(tf("objectlock.job_warning", { warning: w }), "info"));
}

// objectLockText describes an object lock ({mode, retention_days}).
function objectLockText(lock) {
  if (!lock || (lock.mode !== "governance" && lock.mode !== "compliance")) return t("objectlock.no_lock");
  return tf("objectlock.lock_text", { mode: objectLockModeLabel(lock.mode), days: Number(lock.retention_days) || 0 });
}

// objectLockPendingText describes a pending lowering of a target's object lock
// (protection.js renderPendingChanges).
function objectLockPendingText(c, when) {
  const target = (typeof storageTargetName === "function" && storageTargetName(c.target_id)) || String(c.target_id || "");
  return tf("objectlock.pending", { target, value: objectLockText(c.object_lock), when });
}

// objectLockTargetSaved explains a lowered object lock after a target save (app.js)
// and reports whether it showed a message.
function objectLockTargetSaved(data) {
  if (!data) return false;
  if (data.approval) {
    showToast(t("objectlock.lowered_approval"), "info");
    if (typeof loadApprovals === "function") loadApprovals();
    return true;
  }
  const c = data.pending_object_lock;
  if (c && Date.parse(c.created_at) > Date.now() - 60000) {
    const when = typeof protectionWhen === "function" ? protectionWhen(c.effective_at) : String(c.effective_at);
    showToast(tf("objectlock.lowered_pending", { when }), "info");
    if (typeof loadPendingChanges === "function") loadPendingChanges();
    return true;
  }
  return false;
}

document.addEventListener("change", e => {
  if (e.target && e.target.id === "storage-object-lock") objectLockSyncForm();
});
