/**
 * MongoRescue dashboard: backup copies on further storage targets (3-2-1).
 *
 * The job form's copy targets (at most three, never the primary) and copy mode,
 * the copies in the backup details, the restore dialog's source target, the
 * source of a restore in its details and the copy count of a readiness row.
 *
 * Loaded after app.js, trust.js (mergeTranslations), runs.js and readiness.js;
 * app.js calls copiesFillJobForm(), copiesJobPayload(), copiesBackupDetailRows(),
 * copiesFillRestore() and copiesRestorePayload(), runs.js calls
 * copiesRestoreDetailRows() and readiness.js copiesReadinessCell(). Server values
 * reach the DOM only through textContent or escapeHtml().
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const COPIES_TRANSLATIONS = {
  en: {
    copies: {
      targets: "Copy targets",
      targets_hint: "Every backup is also copied, byte for byte and checked against its SHA-256, to these storage targets (at most 3): 3-2-1 backups.",
      none_available: "Add another storage target to copy backups to it.",
      max_reached: "At most 3 copy targets.",
      mode: "Copy mode",
      mode_async: "Async: copy after the backup completed",
      mode_sync: "Sync: the backup completes only when every copy succeeded",
      count_one: "1 copy",
      count_many: "{n} copies",
      count_of: "{done} of {total} copies",
      details_label: "Copies",
      status_pending: "Pending",
      status_done: "Copied",
      status_failed: "Failed",
      status_purged: "Purged",
      verified_ok: "verified",
      verified_mismatch: "damaged",
      next_attempt: "next attempt {time}",
      source_label: "Read the archive from",
      source_auto: "Automatic: the primary, or a healthy copy if it is missing or damaged",
      source_primary: "Primary: {name}",
      source_copy: "Copy: {name}",
      read_from: "Read from",
      fallback: "Fallback",
    },
    readiness: { reason_copy_missing: "Copies missing" },
    restore_checks: { check_source: "Source archive" },
    notify: { events: { backup_copy_failed: "Backup copy failing", backup_copy_recovered: "Backup copy recovered" } },
  },
  tr: {
    copies: {
      targets: "Kopya hedefleri",
      targets_hint: "Her yedek bu depolama hedeflerine de (en fazla 3) bayt bayt kopyalanır ve SHA-256 ile denetlenir: 3-2-1 yedekleme.",
      none_available: "Yedekleri kopyalamak için başka bir depolama hedefi ekleyin.",
      max_reached: "En fazla 3 kopya hedefi.",
      mode: "Kopyalama kipi",
      mode_async: "Eşzamansız: yedek tamamlandıktan sonra kopyala",
      mode_sync: "Eşzamanlı: yedek ancak tüm kopyalar başarılı olunca tamamlanır",
      count_one: "1 kopya",
      count_many: "{n} kopya",
      count_of: "{total} kopyadan {done}",
      details_label: "Kopyalar",
      status_pending: "Bekliyor",
      status_done: "Kopyalandı",
      status_failed: "Başarısız",
      status_purged: "Kalıcı silindi",
      verified_ok: "doğrulandı",
      verified_mismatch: "bozuk",
      next_attempt: "sonraki deneme {time}",
      source_label: "Arşivin okunacağı yer",
      source_auto: "Otomatik: birincil, eksik ya da bozuksa sağlam bir kopya",
      source_primary: "Birincil: {name}",
      source_copy: "Kopya: {name}",
      read_from: "Okunan yer",
      fallback: "Yedeğe geçiş",
    },
    readiness: { reason_copy_missing: "Kopyalar eksik" },
    restore_checks: { check_source: "Kaynak arşiv" },
    notify: { events: { backup_copy_failed: "Yedek kopyalanamıyor", backup_copy_recovered: "Yedek kopyası düzeldi" } },
  },
  de: {
    copies: {
      targets: "Kopieziele",
      targets_hint: "Jedes Backup wird zusätzlich Byte für Byte auf diese Speicherziele kopiert (höchstens 3) und mit seinem SHA-256 geprüft: 3-2-1-Backups.",
      none_available: "Legen Sie ein weiteres Speicherziel an, um Backups dorthin zu kopieren.",
      max_reached: "Höchstens 3 Kopieziele.",
      mode: "Kopiermodus",
      mode_async: "Asynchron: nach Abschluss des Backups kopieren",
      mode_sync: "Synchron: das Backup ist erst fertig, wenn jede Kopie gelungen ist",
      count_one: "1 Kopie",
      count_many: "{n} Kopien",
      count_of: "{done} von {total} Kopien",
      details_label: "Kopien",
      status_pending: "Ausstehend",
      status_done: "Kopiert",
      status_failed: "Fehlgeschlagen",
      status_purged: "Endgültig gelöscht",
      verified_ok: "geprüft",
      verified_mismatch: "beschädigt",
      next_attempt: "nächster Versuch {time}",
      source_label: "Archiv lesen von",
      source_auto: "Automatisch: das Primärziel, oder eine intakte Kopie, wenn es fehlt oder beschädigt ist",
      source_primary: "Primär: {name}",
      source_copy: "Kopie: {name}",
      read_from: "Gelesen von",
      fallback: "Ausweichen",
    },
    readiness: { reason_copy_missing: "Kopien fehlen" },
    restore_checks: { check_source: "Quellarchiv" },
    notify: { events: { backup_copy_failed: "Backup-Kopie schlägt fehl", backup_copy_recovered: "Backup-Kopie wieder in Ordnung" } },
  },
  es: {
    copies: {
      targets: "Destinos de copia",
      targets_hint: "Cada copia de seguridad se copia también, byte a byte y comprobada con su SHA-256, en estos destinos (3 como máximo): copias 3-2-1.",
      none_available: "Añada otro destino de almacenamiento para copiar ahí las copias de seguridad.",
      max_reached: "Como máximo 3 destinos de copia.",
      mode: "Modo de copia",
      mode_async: "Asíncrono: copiar cuando la copia de seguridad haya terminado",
      mode_sync: "Síncrono: la copia de seguridad termina solo cuando todas las copias salen bien",
      count_one: "1 copia",
      count_many: "{n} copias",
      count_of: "{done} de {total} copias",
      details_label: "Copias",
      status_pending: "Pendiente",
      status_done: "Copiada",
      status_failed: "Fallida",
      status_purged: "Purgada",
      verified_ok: "verificada",
      verified_mismatch: "dañada",
      next_attempt: "próximo intento {time}",
      source_label: "Leer el archivo de",
      source_auto: "Automático: el principal, o una copia sana si falta o está dañado",
      source_primary: "Principal: {name}",
      source_copy: "Copia: {name}",
      read_from: "Leído de",
      fallback: "Alternativa",
    },
    readiness: { reason_copy_missing: "Faltan copias" },
    restore_checks: { check_source: "Archivo de origen" },
    notify: { events: { backup_copy_failed: "La copia de la copia de seguridad falla", backup_copy_recovered: "La copia de la copia de seguridad se recuperó" } },
  },
  fr: {
    copies: {
      targets: "Cibles de copie",
      targets_hint: "Chaque sauvegarde est aussi copiée octet par octet, et contrôlée par son SHA-256, vers ces cibles de stockage (3 au plus) : sauvegardes 3-2-1.",
      none_available: "Ajoutez une autre cible de stockage pour y copier les sauvegardes.",
      max_reached: "3 cibles de copie au plus.",
      mode: "Mode de copie",
      mode_async: "Asynchrone : copier une fois la sauvegarde terminée",
      mode_sync: "Synchrone : la sauvegarde n'est terminée que lorsque chaque copie a réussi",
      count_one: "1 copie",
      count_many: "{n} copies",
      count_of: "{done} copies sur {total}",
      details_label: "Copies",
      status_pending: "En attente",
      status_done: "Copiée",
      status_failed: "Échec",
      status_purged: "Purgée",
      verified_ok: "vérifiée",
      verified_mismatch: "endommagée",
      next_attempt: "prochain essai {time}",
      source_label: "Lire l'archive depuis",
      source_auto: "Automatique : la cible principale, ou une copie saine si elle manque ou est endommagée",
      source_primary: "Principale : {name}",
      source_copy: "Copie : {name}",
      read_from: "Lue depuis",
      fallback: "Repli",
    },
    readiness: { reason_copy_missing: "Copies manquantes" },
    restore_checks: { check_source: "Archive source" },
    notify: { events: { backup_copy_failed: "Copie de sauvegarde en échec", backup_copy_recovered: "Copie de sauvegarde rétablie" } },
  },
  zh: {
    copies: {
      targets: "副本目标",
      targets_hint: "每个备份还会逐字节复制到这些存储目标（最多 3 个），并按其 SHA-256 校验：3-2-1 备份。",
      none_available: "添加另一个存储目标即可将备份复制到那里。",
      max_reached: "最多 3 个副本目标。",
      mode: "复制模式",
      mode_async: "异步：备份完成后再复制",
      mode_sync: "同步：所有副本都成功后备份才算完成",
      count_one: "1 个副本",
      count_many: "{n} 个副本",
      count_of: "{done}/{total} 个副本",
      details_label: "副本",
      status_pending: "等待中",
      status_done: "已复制",
      status_failed: "失败",
      status_purged: "已清除",
      verified_ok: "已校验",
      verified_mismatch: "已损坏",
      next_attempt: "下次尝试 {time}",
      source_label: "从以下位置读取归档",
      source_auto: "自动：主目标；若其缺失或损坏，则使用完好的副本",
      source_primary: "主目标：{name}",
      source_copy: "副本：{name}",
      read_from: "读取自",
      fallback: "回退",
    },
    readiness: { reason_copy_missing: "副本缺失" },
    restore_checks: { check_source: "源归档" },
    notify: { events: { backup_copy_failed: "备份副本复制失败", backup_copy_recovered: "备份副本已恢复" } },
  },
  ja: {
    copies: {
      targets: "コピー先",
      targets_hint: "各バックアップはこれらのストレージ先（最大 3 つ）にもバイト単位でコピーされ、SHA-256 で検査されます：3-2-1 バックアップ。",
      none_available: "バックアップをコピーするには、別のストレージ先を追加してください。",
      max_reached: "コピー先は最大 3 つです。",
      mode: "コピーモード",
      mode_async: "非同期：バックアップ完了後にコピー",
      mode_sync: "同期：すべてのコピーが成功して初めてバックアップが完了",
      count_one: "コピー 1 件",
      count_many: "コピー {n} 件",
      count_of: "コピー {total} 件中 {done} 件",
      details_label: "コピー",
      status_pending: "待機中",
      status_done: "コピー済み",
      status_failed: "失敗",
      status_purged: "完全削除済み",
      verified_ok: "検証済み",
      verified_mismatch: "破損",
      next_attempt: "次の試行 {time}",
      source_label: "アーカイブの読み取り元",
      source_auto: "自動：プライマリ。欠落または破損している場合は健全なコピー",
      source_primary: "プライマリ：{name}",
      source_copy: "コピー：{name}",
      read_from: "読み取り元",
      fallback: "フォールバック",
    },
    readiness: { reason_copy_missing: "コピーが不足" },
    restore_checks: { check_source: "ソースアーカイブ" },
    notify: { events: { backup_copy_failed: "バックアップのコピーが失敗", backup_copy_recovered: "バックアップのコピーが回復" } },
  },
  ru: {
    copies: {
      targets: "Цели копирования",
      targets_hint: "Каждая резервная копия также копируется побайтно, с проверкой по SHA-256, в эти хранилища (не более 3): схема 3-2-1.",
      none_available: "Добавьте ещё одно хранилище, чтобы копировать туда резервные копии.",
      max_reached: "Не более 3 целей копирования.",
      mode: "Режим копирования",
      mode_async: "Асинхронно: копировать после завершения резервной копии",
      mode_sync: "Синхронно: резервная копия завершается только после успеха всех копий",
      count_one: "1 копия",
      count_many: "Копий: {n}",
      count_of: "{done} из {total} копий",
      details_label: "Копии",
      status_pending: "Ожидает",
      status_done: "Скопирована",
      status_failed: "Ошибка",
      status_purged: "Удалена окончательно",
      verified_ok: "проверена",
      verified_mismatch: "повреждена",
      next_attempt: "следующая попытка {time}",
      source_label: "Читать архив из",
      source_auto: "Автоматически: основное хранилище, или исправная копия, если архив отсутствует или повреждён",
      source_primary: "Основное: {name}",
      source_copy: "Копия: {name}",
      read_from: "Прочитано из",
      fallback: "Переключение",
    },
    readiness: { reason_copy_missing: "Нет копий" },
    restore_checks: { check_source: "Исходный архив" },
    notify: { events: { backup_copy_failed: "Копирование резервной копии не удаётся", backup_copy_recovered: "Копирование резервной копии восстановлено" } },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(COPIES_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], COPIES_TRANSLATIONS[lang]);
  });
}

const COPIES_MAX = 3;

// The copy targets and mode the job form shows (kept across re-renders).
const copiesForm = { selected: [], mode: "async" };

// copiesCount returns "1 copy", "2 copies" or "1 of 2 copies".
function copiesCount(done, total) {
  if (total > 0 && done !== total) return tf("copies.count_of", { done, total });
  return done === 1 ? t("copies.count_one") : tf("copies.count_many", { n: done });
}

// copiesTargetName returns the name of storage target id, else id.
function copiesTargetName(id, fallback) {
  const s = (state.storageTargets || []).find(x => x.id === id);
  return (s && s.name) || fallback || id || "";
}

// ---------------------------------------------------------------------------
// Job form
// ---------------------------------------------------------------------------

// copiesJobSection returns the job form's copy section, created on first use
// below the storage target.
function copiesJobSection() {
  let group = document.getElementById("job-copies-group");
  if (group) return group;
  const storage = document.getElementById("job-storage");
  const anchor = storage && storage.closest(".form-group");
  if (!anchor) return null;
  group = document.createElement("div");
  group.className = "form-group";
  group.id = "job-copies-group";
  const label = document.createElement("span");
  label.className = "form-label";
  label.id = "job-copies-label";
  const list = document.createElement("div");
  list.id = "job-copies-list";
  list.setAttribute("role", "group");
  list.setAttribute("aria-labelledby", "job-copies-label");
  const hint = document.createElement("p");
  hint.className = "form-hint";
  hint.id = "job-copies-hint";
  const modeLabel = document.createElement("label");
  modeLabel.className = "form-label";
  modeLabel.htmlFor = "job-copy-mode";
  modeLabel.id = "job-copy-mode-label";
  const mode = document.createElement("select");
  mode.id = "job-copy-mode";
  mode.className = "form-select";
  ["async", "sync"].forEach(v => {
    const opt = document.createElement("option");
    opt.value = v;
    mode.appendChild(opt);
  });
  mode.addEventListener("change", () => { copiesForm.mode = mode.value; });
  const modeWrap = document.createElement("div");
  modeWrap.id = "job-copy-mode-wrap";
  modeWrap.append(modeLabel, mode);
  group.append(label, list, hint, modeWrap);
  anchor.after(group);
  storage.addEventListener("change", copiesRenderJob);
  return group;
}

// copiesRenderJob renders the copy target checkboxes: every target but the
// selected primary, at most COPIES_MAX checked.
function copiesRenderJob() {
  const group = copiesJobSection();
  if (!group) return;
  const primary = getValue("job-storage");
  copiesForm.selected = copiesForm.selected.filter(id => id && id !== primary);
  document.getElementById("job-copies-label").textContent = t("copies.targets");
  document.getElementById("job-copy-mode-label").textContent = t("copies.mode");
  const mode = document.getElementById("job-copy-mode");
  mode.options[0].textContent = t("copies.mode_async");
  mode.options[1].textContent = t("copies.mode_sync");
  mode.value = copiesForm.mode === "sync" ? "sync" : "async";
  const list = document.getElementById("job-copies-list");
  list.textContent = "";
  const candidates = (state.storageTargets || []).filter(s => s.id !== primary);
  const full = copiesForm.selected.length >= COPIES_MAX;
  candidates.forEach(s => {
    const wrap = document.createElement("label");
    wrap.className = "check";
    const box = document.createElement("input");
    box.type = "checkbox";
    box.value = s.id;
    box.checked = copiesForm.selected.includes(s.id);
    box.disabled = full && !box.checked;
    box.addEventListener("change", () => {
      copiesForm.selected = box.checked
        ? [...copiesForm.selected, s.id].slice(0, COPIES_MAX)
        : copiesForm.selected.filter(id => id !== s.id);
      copiesRenderJob();
    });
    const name = document.createElement("span");
    name.className = "check-title";
    name.textContent = s.name;
    wrap.append(box, name);
    list.appendChild(wrap);
  });
  const hint = document.getElementById("job-copies-hint");
  hint.textContent = candidates.length === 0 ? t("copies.none_available")
    : full ? `${t("copies.targets_hint")} ${t("copies.max_reached")}` : t("copies.targets_hint");
  document.getElementById("job-copy-mode-wrap").hidden = copiesForm.selected.length === 0;
}

// copiesFillJobForm shows job's copy targets (none for a new job).
function copiesFillJobForm(job) {
  const j = job || {};
  copiesForm.selected = Array.isArray(j.copy_targets) ? j.copy_targets.slice(0, COPIES_MAX) : [];
  copiesForm.mode = j.copy_mode === "sync" ? "sync" : "async";
  copiesRenderJob();
}

// copiesJobPayload returns the copy fields of a job save.
function copiesJobPayload() {
  const primary = getValue("job-storage");
  const targets = copiesForm.selected.filter(id => id && id !== primary);
  return { copy_targets: targets, copy_mode: targets.length > 0 ? copiesForm.mode : "" };
}

// ---------------------------------------------------------------------------
// Backup details
// ---------------------------------------------------------------------------

// copiesStatusText describes copy c in one line.
function copiesStatusText(c) {
  const parts = [t(`copies.status_${c.status}`, String(c.status || ""))];
  if (c.status === "done" && c.copied_at) parts.push(absoluteWithRelative(c.copied_at));
  if (c.verification === "ok") parts.push(t("copies.verified_ok"));
  if (c.verification === "mismatch") parts.push(t("copies.verified_mismatch"));
  if (c.next_attempt_at && (c.status === "failed" || c.status === "pending")) {
    parts.push(tf("copies.next_attempt", { time: absoluteWithRelative(c.next_attempt_at) }));
  }
  if (c.error && c.status !== "done") parts.push(c.error);
  return parts.filter(Boolean).join(" · ");
}

// copiesBackupDetailRows adds the copies of backup b to its details.
function copiesBackupDetailRows(b, row) {
  const list = Array.isArray(b.copies) ? b.copies : [];
  if (list.length === 0) return;
  const done = list.filter(c => c.status === "done").length;
  row(t("copies.details_label"), copiesCount(done, list.length));
  list.forEach(c => row(copiesTargetName(c.target_id, c.target_name), copiesStatusText(c)));
}

// ---------------------------------------------------------------------------
// Restore dialog and restore details
// ---------------------------------------------------------------------------

// copiesRestoreSelect returns the restore dialog's source select, created on first
// use above the target connection.
function copiesRestoreSelect() {
  let select = document.getElementById("restore-source-target");
  if (select) return select;
  const conn = document.getElementById("restore-target-connection");
  const anchor = conn && conn.closest(".form-group");
  if (!anchor) return null;
  const group = document.createElement("div");
  group.className = "form-group";
  group.id = "restore-source-group";
  const label = document.createElement("label");
  label.className = "form-label";
  label.htmlFor = "restore-source-target";
  label.id = "restore-source-label";
  select = document.createElement("select");
  select.id = "restore-source-target";
  select.className = "form-select";
  group.append(label, select);
  anchor.before(group);
  return select;
}

// copiesFillRestore offers the primary and every completed copy of backup b as the
// restore's source; the group is hidden for a backup without copies.
function copiesFillRestore(b) {
  const select = copiesRestoreSelect();
  if (!select) return;
  const copies = (Array.isArray(b && b.copies) ? b.copies : []).filter(c => c.status === "done");
  document.getElementById("restore-source-group").hidden = copies.length === 0;
  document.getElementById("restore-source-label").textContent = t("copies.source_label");
  select.textContent = "";
  const add = (value, label) => {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = label;
    select.appendChild(opt);
  };
  add("", t("copies.source_auto"));
  if (copies.length === 0) return;
  add(b.storage_target_id || "", tf("copies.source_primary", { name: backupStorageName(b) || b.storage_target_id || "" }));
  copies.forEach(c => add(c.target_id, tf("copies.source_copy", { name: copiesTargetName(c.target_id, c.target_name) })));
  // The primary's own option has its ID; "" (automatic) stays the default.
  if (!b.storage_target_id) select.options[1].remove();
  select.value = "";
}

// copiesRestorePayload returns the source target of the restore dialog, if chosen.
function copiesRestorePayload() {
  const select = document.getElementById("restore-source-target");
  const group = document.getElementById("restore-source-group");
  if (!select || !group || group.hidden || !select.value) return {};
  return { source_target_id: select.value };
}

// copiesRestoreDetailRows adds where restore r read its archive from.
function copiesRestoreDetailRows(r, row) {
  if (!r.source_target_id) return;
  row(t("copies.read_from"), copiesTargetName(r.source_target_id, r.source_target_name));
  if (r.source_fallback) row(t("copies.fallback"), r.source_fallback);
}

// ---------------------------------------------------------------------------
// Readiness
// ---------------------------------------------------------------------------

// copiesReadinessCell returns the copy count of readiness row r as HTML ("" when
// its newest backup has no copy targets).
function copiesReadinessCell(r) {
  if (!r || !(r.copy_targets > 0)) return "";
  return ` <span class="muted rd-copies">· ${escapeHtml(copiesCount(r.copies || 0, r.copy_targets))}</span>`;
}

document.addEventListener("DOMContentLoaded", () => {
  if (typeof onLanguageChange === "function") onLanguageChange(() => copiesRenderJob());
});
