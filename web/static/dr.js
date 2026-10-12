/**
 * MongoRescue dashboard: cross-region disaster recovery.
 *
 * The job form's locked copies policy and DR drill source (the copy target its
 * restore tests read), the region label of a storage target, the locked copies
 * default under Settings → Security, a PITR stream's copy targets and the copy
 * chain a point-in-time restore reads, and the DR status of a readiness row.
 *
 * Loaded after app.js, trust.js (mergeTranslations), readiness.js, pitr.js,
 * protection.js and copies.js; those call the dr* hooks below when they exist.
 * Server values reach the DOM only through textContent or escapeHtml().
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const DR_TRANSLATIONS = {
  en: {
    dr: {
      require_locked: "Require locked copies",
      require_locked_hint: "Refuse to save the job unless every copy target has S3 Object Lock, so no copy is left deletable.",
      settings_locked: "Require locked copies for new jobs",
      settings_locked_hint: "The default of new jobs: every copy target needs S3 Object Lock. Turning it off waits for the delete grace period, and for a second administrator under the two-person rule.",
      pending_locked: "Locked copies no longer required from {when}",
      pending_job_locked: "Job {job} stops requiring locked copies from {when}",
      require_locked_forced: "Locked by the security setting: every job requires locked copies.",
      region: "Region label (disaster recovery)",
      region_hint: "Where this target's data lives, for the cross-region checks. Optional on AWS S3 (the bucket's region is used); set it for MinIO, other S3 providers, a local disk or a NAS, for example dc-frankfurt: their region is not known otherwise.",
      drill_source: "Read the archive from (DR drill)",
      drill_primary: "The primary storage target",
      drill_copy: "Copy target: {name}",
      drill_hint: "A restore test that reads the copy in another region is a DR drill: it proves a restore works with the primary's region gone.",
      level_cross_region: "DR: cross-region ✓",
      level_cross_region_unproven: "DR: cross-region, unproven",
      level_same_region: "DR: same region ✗",
      drill_ok: "last drill passed {when}",
      drill_failed: "last drill failed {when}",
      drill_none: "no drill yet",
      stream_copies: "Copy targets",
      stream_copies_hint: "Oplog chunks and base backups are also copied to these targets (at most 3), for a restore in another region.",
      pitr_source: "Read from",
      pitr_source_auto: "Automatic: the primary, or a complete copy chain if it cannot be read",
      pitr_source_copy: "Copy chain on {name}",
    },
    readiness: {
      reason_dr_same_region: "No copy in another region",
      reason_dr_same_credentials: "Copies share the primary's credentials",
      reason_dr_drill_stale: "No recent DR drill",
      reason_dr_unlocked_copy: "A required locked copy has no Object Lock",
    },
  },
  tr: {
    dr: {
      require_locked: "Kilitli kopya zorunlu",
      require_locked_hint: "Her kopya hedefinde S3 Object Lock yoksa işi kaydetme; hiçbir kopya silinebilir kalmaz.",
      settings_locked: "Yeni işlerde kilitli kopya zorunlu",
      settings_locked_hint: "Yeni işlerin varsayılanı: her kopya hedefinde S3 Object Lock gerekir. Kapatmak silme bekleme süresini, iki kişi kuralında ikinci bir yöneticiyi de bekler.",
      pending_locked: "Kilitli kopya zorunluluğu {when} itibarıyla kalkıyor",
      pending_job_locked: "{job} işinde kilitli kopya zorunluluğu {when} itibarıyla kalkıyor",
      require_locked_forced: "Güvenlik ayarı gereği: her iş kilitli kopya gerektirir.",
      region: "Bölge etiketi (felaket kurtarma)",
      region_hint: "Bu hedefin verisinin bulunduğu yer, bölgeler arası denetimler için. AWS S3'te isteğe bağlı (kovanın bölgesi kullanılır); MinIO, diğer S3 sağlayıcıları, yerel disk ya da NAS için girin, örneğin dc-frankfurt: aksi hâlde bölgeleri bilinmez.",
      drill_source: "Arşivin okunacağı yer (DR tatbikatı)",
      drill_primary: "Birincil depolama hedefi",
      drill_copy: "Kopya hedefi: {name}",
      drill_hint: "Başka bölgedeki kopyadan okuyan geri yükleme testi bir DR tatbikatıdır: birincil bölge yokken geri yüklemenin çalıştığını kanıtlar.",
      level_cross_region: "DR: bölgeler arası ✓",
      level_cross_region_unproven: "DR: bölgeler arası, kanıtlanmadı",
      level_same_region: "DR: aynı bölge ✗",
      drill_ok: "son tatbikat başarılı {when}",
      drill_failed: "son tatbikat başarısız {when}",
      drill_none: "henüz tatbikat yok",
      stream_copies: "Kopya hedefleri",
      stream_copies_hint: "Oplog parçaları ve temel yedekler bu hedeflere de (en fazla 3) kopyalanır; başka bir bölgede geri yükleme için.",
      pitr_source: "Okunacak yer",
      pitr_source_auto: "Otomatik: birincil, okunamazsa eksiksiz bir kopya zinciri",
      pitr_source_copy: "{name} üzerindeki kopya zinciri",
    },
    readiness: {
      reason_dr_same_region: "Başka bölgede kopya yok",
      reason_dr_same_credentials: "Kopyalar birincilin kimlik bilgilerini paylaşıyor",
      reason_dr_drill_stale: "Yakın tarihli DR tatbikatı yok",
      reason_dr_unlocked_copy: "Zorunlu kilitli kopyada Object Lock yok",
    },
  },
  de: {
    dr: {
      require_locked: "Gesperrte Kopien verlangen",
      require_locked_hint: "Den Job nur speichern, wenn jedes Kopieziel S3 Object Lock hat, damit keine Kopie löschbar bleibt.",
      settings_locked: "Gesperrte Kopien für neue Jobs verlangen",
      settings_locked_hint: "Vorgabe für neue Jobs: jedes Kopieziel braucht S3 Object Lock. Das Abschalten wartet die Löschfrist ab, mit der Zwei-Personen-Regel auch auf einen zweiten Administrator.",
      pending_locked: "Gesperrte Kopien ab {when} nicht mehr verlangt",
      pending_job_locked: "Job {job} verlangt ab {when} keine gesperrten Kopien mehr",
      require_locked_forced: "Durch die Sicherheitseinstellung festgelegt: jeder Job verlangt gesperrte Kopien.",
      region: "Regionsbezeichnung (Disaster Recovery)",
      region_hint: "Wo die Daten dieses Ziels liegen, für die regionsübergreifenden Prüfungen. Bei AWS S3 optional (die Region des Buckets gilt); für MinIO, andere S3-Anbieter, eine lokale Platte oder ein NAS setzen, zum Beispiel dc-frankfurt: sonst ist ihre Region unbekannt.",
      drill_source: "Archiv lesen von (DR-Übung)",
      drill_primary: "Das primäre Speicherziel",
      drill_copy: "Kopieziel: {name}",
      drill_hint: "Ein Wiederherstellungstest, der die Kopie in einer anderen Region liest, ist eine DR-Übung: er beweist, dass eine Wiederherstellung ohne die Region des Primärziels gelingt.",
      level_cross_region: "DR: regionsübergreifend ✓",
      level_cross_region_unproven: "DR: regionsübergreifend, unbewiesen",
      level_same_region: "DR: gleiche Region ✗",
      drill_ok: "letzte Übung bestanden {when}",
      drill_failed: "letzte Übung fehlgeschlagen {when}",
      drill_none: "noch keine Übung",
      stream_copies: "Kopieziele",
      stream_copies_hint: "Oplog-Chunks und Basis-Backups werden zusätzlich auf diese Ziele kopiert (höchstens 3), für eine Wiederherstellung in einer anderen Region.",
      pitr_source: "Lesen von",
      pitr_source_auto: "Automatisch: das Primärziel, oder eine vollständige Kopiekette, wenn es nicht lesbar ist",
      pitr_source_copy: "Kopiekette auf {name}",
    },
    readiness: {
      reason_dr_same_region: "Keine Kopie in einer anderen Region",
      reason_dr_same_credentials: "Kopien teilen die Zugangsdaten des Primärziels",
      reason_dr_drill_stale: "Keine aktuelle DR-Übung",
      reason_dr_unlocked_copy: "Einer verlangten gesperrten Kopie fehlt Object Lock",
    },
  },
  es: {
    dr: {
      require_locked: "Exigir copias bloqueadas",
      require_locked_hint: "No guardar la tarea salvo que cada destino de copia tenga S3 Object Lock, para que ninguna copia quede borrable.",
      settings_locked: "Exigir copias bloqueadas en tareas nuevas",
      settings_locked_hint: "Valor por defecto de las tareas nuevas: cada destino de copia necesita S3 Object Lock. Desactivarlo espera el periodo de gracia de borrado y, con la regla de dos personas, a un segundo administrador.",
      pending_locked: "Las copias bloqueadas dejan de exigirse el {when}",
      pending_job_locked: "La tarea {job} deja de exigir copias bloqueadas el {when}",
      require_locked_forced: "Fijado por el ajuste de seguridad: toda tarea exige copias bloqueadas.",
      region: "Etiqueta de región (recuperación ante desastres)",
      region_hint: "Dónde están los datos de este destino, para las comprobaciones entre regiones. Opcional en AWS S3 (se usa la región del bucket); indíquela para MinIO, otros proveedores S3, un disco local o un NAS, por ejemplo dc-frankfurt: si no, su región es desconocida.",
      drill_source: "Leer el archivo de (simulacro DR)",
      drill_primary: "El destino de almacenamiento principal",
      drill_copy: "Destino de copia: {name}",
      drill_hint: "Una prueba de restauración que lee la copia de otra región es un simulacro DR: demuestra que la restauración funciona sin la región del principal.",
      level_cross_region: "DR: entre regiones ✓",
      level_cross_region_unproven: "DR: entre regiones, sin demostrar",
      level_same_region: "DR: misma región ✗",
      drill_ok: "último simulacro superado {when}",
      drill_failed: "último simulacro fallido {when}",
      drill_none: "sin simulacros aún",
      stream_copies: "Destinos de copia",
      stream_copies_hint: "Los fragmentos del oplog y las copias base también se copian en estos destinos (3 como máximo), para restaurar en otra región.",
      pitr_source: "Leer de",
      pitr_source_auto: "Automático: el principal, o una cadena de copias completa si no se puede leer",
      pitr_source_copy: "Cadena de copias en {name}",
    },
    readiness: {
      reason_dr_same_region: "Ninguna copia en otra región",
      reason_dr_same_credentials: "Las copias comparten las credenciales del principal",
      reason_dr_drill_stale: "Ningún simulacro DR reciente",
      reason_dr_unlocked_copy: "Una copia que debe estar bloqueada no tiene Object Lock",
    },
  },
  fr: {
    dr: {
      require_locked: "Exiger des copies verrouillées",
      require_locked_hint: "Refuser d'enregistrer la tâche si une cible de copie n'a pas S3 Object Lock, pour qu'aucune copie ne reste supprimable.",
      settings_locked: "Exiger des copies verrouillées pour les nouvelles tâches",
      settings_locked_hint: "Valeur par défaut des nouvelles tâches : chaque cible de copie doit avoir S3 Object Lock. La désactivation attend le délai de grâce de suppression et, avec la règle des deux personnes, un second administrateur.",
      pending_locked: "Copies verrouillées plus exigées à partir du {when}",
      pending_job_locked: "La tâche {job} n'exige plus de copies verrouillées à partir du {when}",
      require_locked_forced: "Imposé par le réglage de sécurité : chaque tâche exige des copies verrouillées.",
      region: "Libellé de région (reprise après sinistre)",
      region_hint: "L'endroit où se trouvent les données de cette cible, pour les contrôles inter-régions. Facultatif sur AWS S3 (la région du bucket est utilisée) ; à renseigner pour MinIO, les autres fournisseurs S3, un disque local ou un NAS, par exemple dc-frankfurt : sinon leur région est inconnue.",
      drill_source: "Lire l'archive depuis (exercice DR)",
      drill_primary: "La cible de stockage principale",
      drill_copy: "Cible de copie : {name}",
      drill_hint: "Un test de restauration qui lit la copie d'une autre région est un exercice DR : il prouve qu'une restauration fonctionne sans la région de la cible principale.",
      level_cross_region: "DR : inter-régions ✓",
      level_cross_region_unproven: "DR : inter-régions, non prouvé",
      level_same_region: "DR : même région ✗",
      drill_ok: "dernier exercice réussi {when}",
      drill_failed: "dernier exercice échoué {when}",
      drill_none: "aucun exercice pour l'instant",
      stream_copies: "Cibles de copie",
      stream_copies_hint: "Les fragments d'oplog et les sauvegardes de base sont aussi copiés vers ces cibles (3 au plus), pour une restauration dans une autre région.",
      pitr_source: "Lire depuis",
      pitr_source_auto: "Automatique : la cible principale, ou une chaîne de copies complète si elle est illisible",
      pitr_source_copy: "Chaîne de copies sur {name}",
    },
    readiness: {
      reason_dr_same_region: "Aucune copie dans une autre région",
      reason_dr_same_credentials: "Les copies partagent les identifiants de la cible principale",
      reason_dr_drill_stale: "Aucun exercice DR récent",
      reason_dr_unlocked_copy: "Une copie qui doit être verrouillée n'a pas d'Object Lock",
    },
  },
  zh: {
    dr: {
      require_locked: "要求副本加锁",
      require_locked_hint: "除非每个副本目标都启用了 S3 Object Lock，否则拒绝保存任务，确保没有副本可被删除。",
      settings_locked: "新任务要求副本加锁",
      settings_locked_hint: "新任务的默认值：每个副本目标都需要 S3 Object Lock。关闭它需等待删除宽限期，启用双人规则时还需第二位管理员批准。",
      pending_locked: "自 {when} 起不再要求副本加锁",
      pending_job_locked: "任务 {job} 自 {when} 起不再要求副本加锁",
      require_locked_forced: "由安全设置锁定：每个任务都要求副本加锁。",
      region: "区域标签（灾难恢复）",
      region_hint: "该目标数据所在的位置，用于跨区域检查。AWS S3 可选（使用存储桶的区域）；MinIO、其他 S3 服务商、本地磁盘或 NAS 请填写，例如 dc-frankfurt，否则其区域未知。",
      drill_source: "读取归档的位置（DR 演练）",
      drill_primary: "主存储目标",
      drill_copy: "副本目标：{name}",
      drill_hint: "从另一区域的副本读取的恢复测试即为 DR 演练：证明在主目标所在区域不可用时恢复依然可行。",
      level_cross_region: "DR：跨区域 ✓",
      level_cross_region_unproven: "DR：跨区域，未经验证",
      level_same_region: "DR：同一区域 ✗",
      drill_ok: "上次演练通过 {when}",
      drill_failed: "上次演练失败 {when}",
      drill_none: "尚无演练",
      stream_copies: "副本目标",
      stream_copies_hint: "Oplog 分块和基础备份也会复制到这些目标（最多 3 个），以便在其他区域恢复。",
      pitr_source: "读取位置",
      pitr_source_auto: "自动：主目标，无法读取时使用完整的副本链",
      pitr_source_copy: "{name} 上的副本链",
    },
    readiness: {
      reason_dr_same_region: "其他区域没有副本",
      reason_dr_same_credentials: "副本与主目标共用凭据",
      reason_dr_drill_stale: "近期没有 DR 演练",
      reason_dr_unlocked_copy: "要求加锁的副本未启用 Object Lock",
    },
  },
  ja: {
    dr: {
      require_locked: "ロックされたコピーを必須にする",
      require_locked_hint: "すべてのコピー先に S3 Object Lock がない限りジョブを保存しません。削除可能なコピーを残しません。",
      settings_locked: "新しいジョブでロックされたコピーを必須にする",
      settings_locked_hint: "新しいジョブの既定値: すべてのコピー先に S3 Object Lock が必要です。オフにするには削除猶予期間を待ち、二人承認ルールでは 2 人目の管理者の承認も必要です。",
      pending_locked: "{when} からロックされたコピーは必須でなくなります",
      pending_job_locked: "ジョブ {job} は {when} からロックされたコピーを必須にしなくなります",
      require_locked_forced: "セキュリティ設定により固定: すべてのジョブでロックされたコピーが必須です。",
      region: "リージョンラベル（災害復旧）",
      region_hint: "このターゲットのデータがある場所。クロスリージョンのチェックに使います。AWS S3 では任意（バケットのリージョンを使用）。MinIO、他の S3 プロバイダー、ローカルディスク、NAS では dc-frankfurt のように設定してください。設定しないとリージョンは不明です。",
      drill_source: "アーカイブの読み取り元（DR 訓練）",
      drill_primary: "プライマリのストレージターゲット",
      drill_copy: "コピー先: {name}",
      drill_hint: "別リージョンのコピーから読み取るリストアテストは DR 訓練です。プライマリのリージョンがなくてもリストアできることを証明します。",
      level_cross_region: "DR: クロスリージョン ✓",
      level_cross_region_unproven: "DR: クロスリージョン（未検証）",
      level_same_region: "DR: 同一リージョン ✗",
      drill_ok: "前回の訓練は成功 {when}",
      drill_failed: "前回の訓練は失敗 {when}",
      drill_none: "訓練はまだありません",
      stream_copies: "コピー先",
      stream_copies_hint: "oplog チャンクとベースバックアップもこれらのターゲット（最大 3）にコピーされ、別リージョンでリストアできます。",
      pitr_source: "読み取り元",
      pitr_source_auto: "自動: プライマリ、読めない場合は完全なコピーチェーン",
      pitr_source_copy: "{name} 上のコピーチェーン",
    },
    readiness: {
      reason_dr_same_region: "別リージョンにコピーがありません",
      reason_dr_same_credentials: "コピーがプライマリの認証情報を共有しています",
      reason_dr_drill_stale: "最近の DR 訓練がありません",
      reason_dr_unlocked_copy: "ロック必須のコピーに Object Lock がありません",
    },
  },
  ru: {
    dr: {
      require_locked: "Требовать заблокированные копии",
      require_locked_hint: "Не сохранять задание, пока у каждой цели копирования нет S3 Object Lock, чтобы ни одна копия не оставалась удаляемой.",
      settings_locked: "Требовать заблокированные копии для новых заданий",
      settings_locked_hint: "Значение по умолчанию для новых заданий: каждой цели копирования нужен S3 Object Lock. Отключение ждёт окончания льготного периода удаления, а при правиле двух лиц — и второго администратора.",
      pending_locked: "Заблокированные копии перестают требоваться с {when}",
      pending_job_locked: "Задание {job} перестаёт требовать заблокированные копии с {when}",
      require_locked_forced: "Задано настройкой безопасности: каждое задание требует заблокированные копии.",
      region: "Метка региона (аварийное восстановление)",
      region_hint: "Где хранятся данные этой цели, для межрегиональных проверок. Для AWS S3 необязательно (берётся регион бакета); укажите для MinIO, других S3-провайдеров, локального диска или NAS, например dc-frankfurt: иначе их регион неизвестен.",
      drill_source: "Читать архив из (учения DR)",
      drill_primary: "Основная цель хранения",
      drill_copy: "Цель копирования: {name}",
      drill_hint: "Проверка восстановления, читающая копию в другом регионе, — это учения DR: она доказывает, что восстановление работает без региона основной цели.",
      level_cross_region: "DR: межрегионально ✓",
      level_cross_region_unproven: "DR: межрегионально, не подтверждено",
      level_same_region: "DR: тот же регион ✗",
      drill_ok: "последние учения пройдены {when}",
      drill_failed: "последние учения провалены {when}",
      drill_none: "учений ещё не было",
      stream_copies: "Цели копирования",
      stream_copies_hint: "Фрагменты oplog и базовые бэкапы также копируются на эти цели (не более 3) для восстановления в другом регионе.",
      pitr_source: "Читать из",
      pitr_source_auto: "Автоматически: основная цель или полная цепочка копий, если она нечитаема",
      pitr_source_copy: "Цепочка копий на {name}",
    },
    readiness: {
      reason_dr_same_region: "Нет копии в другом регионе",
      reason_dr_same_credentials: "Копии используют учётные данные основной цели",
      reason_dr_drill_stale: "Нет недавних учений DR",
      reason_dr_unlocked_copy: "У копии, которая должна быть заблокирована, нет Object Lock",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(DR_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], DR_TRANSLATIONS[lang]);
  });
}

const DR_MAX_COPIES = 3;

// The DR fields of the job form (kept across re-renders).
const drForm = { locked: false, drill: "" };

// drTargetLabel returns the name of storage target id, else id.
function drTargetLabel(id) {
  const s = (state.storageTargets || []).find(x => x.id === id);
  return (s && s.name) || id || "";
}

// drCheck returns a checkbox row: <label class="check"> with a title and a hint.
function drCheck(id, onChange) {
  const wrap = document.createElement("label");
  wrap.className = "check";
  wrap.htmlFor = id;
  const box = document.createElement("input");
  box.type = "checkbox";
  box.id = id;
  if (onChange) box.addEventListener("change", onChange);
  const text = document.createElement("span");
  text.className = "check-text";
  const title = document.createElement("span");
  title.className = "check-title";
  title.id = `${id}-title`;
  const hint = document.createElement("span");
  hint.className = "form-hint";
  hint.id = `${id}-hint`;
  text.append(title, hint);
  wrap.append(box, text);
  return wrap;
}

// ---------------------------------------------------------------------------
// Job form: locked copies and the DR drill source
// ---------------------------------------------------------------------------

// drJobFields creates, on first use, the locked copies checkbox (in the copy
// section) and the drill source select (in the restore test fields).
function drJobFields() {
  const copies = document.getElementById("job-copies-group");
  if (copies && !document.getElementById("job-require-locked")) {
    copies.appendChild(drCheck("job-require-locked", e => { drForm.locked = e.target.checked; }));
  }
  const conn = document.getElementById("job-rt-connection");
  const anchor = conn && conn.closest(".form-group");
  if (anchor && !document.getElementById("job-rt-source")) {
    const group = document.createElement("div");
    group.className = "form-group";
    group.id = "job-rt-source-group";
    const label = document.createElement("label");
    label.className = "form-label";
    label.htmlFor = "job-rt-source";
    label.id = "job-rt-source-label";
    const select = document.createElement("select");
    select.id = "job-rt-source";
    select.className = "form-select";
    select.addEventListener("change", () => { drForm.drill = select.value; });
    const hint = document.createElement("p");
    hint.className = "form-hint";
    hint.id = "job-rt-source-hint";
    group.append(label, select, hint);
    anchor.after(group);
  }
}

// drRenderJob renders the DR fields of the job form; the drill source offers the
// job's selected copy targets.
function drRenderJob() {
  drJobFields();
  const box = document.getElementById("job-require-locked");
  if (box) {
    // With security.require_locked_copies on, no job can opt out.
    const forced = !!(state.settings && state.settings.security && state.settings.security.require_locked_copies);
    box.checked = drForm.locked || forced;
    box.disabled = forced;
    setText("job-require-locked-title", t("dr.require_locked"));
    setText("job-require-locked-hint", forced ? t("dr.require_locked_forced") : t("dr.require_locked_hint"));
  }
  const select = document.getElementById("job-rt-source");
  if (!select) return;
  const copies = (typeof copiesForm === "object" ? copiesForm.selected : []).slice(0, DR_MAX_COPIES);
  if (drForm.drill && !copies.includes(drForm.drill)) drForm.drill = "";
  select.textContent = "";
  const add = (value, text) => {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = text;
    select.appendChild(opt);
  };
  add("", t("dr.drill_primary"));
  copies.forEach(id => add(id, tf("dr.drill_copy", { name: drTargetLabel(id) })));
  select.value = drForm.drill;
  setText("job-rt-source-label", t("dr.drill_source"));
  setText("job-rt-source-hint", t("dr.drill_hint"));
  document.getElementById("job-rt-source-group").hidden = copies.length === 0;
}

// drFillJobForm shows job's DR fields; a new job takes the locked copies default
// from Settings → Security (app.js).
function drFillJobForm(job) {
  const sec = (state.settings && state.settings.security) || {};
  drForm.locked = job ? !!job.require_locked_copies : !!sec.require_locked_copies;
  drForm.drill = (job && job.restore_test && job.restore_test.source_target_id) || "";
  drRenderJob();
}

// drJobCopiesChanged re-renders the drill source when the copy targets change
// (copies.js).
function drJobCopiesChanged() {
  if (document.getElementById("job-rt-source")) drRenderJob();
}

// drJobPayload adds the DR fields to a job save payload (app.js).
function drJobPayload(payload) {
  const sec = (state.settings && state.settings.security) || {};
  payload.require_locked_copies = drForm.locked || !!sec.require_locked_copies;
  const copies = Array.isArray(payload.copy_targets) ? payload.copy_targets : [];
  if (payload.restore_test) payload.restore_test.source_target_id = copies.includes(drForm.drill) ? drForm.drill : "";
}

// ---------------------------------------------------------------------------
// Storage target form: the region label
// ---------------------------------------------------------------------------

// drRegionInput returns the region label input, created on first use below the
// target's name.
function drRegionInput() {
  let input = document.getElementById("storage-dr-region");
  if (input) return input;
  const name = document.getElementById("storage-name");
  const anchor = name && name.closest(".form-group");
  if (!anchor) return null;
  const group = document.createElement("div");
  group.className = "form-group";
  const label = document.createElement("label");
  label.className = "form-label";
  label.htmlFor = "storage-dr-region";
  label.id = "storage-dr-region-label";
  input = document.createElement("input");
  input.type = "text";
  input.id = "storage-dr-region";
  input.className = "form-input mono";
  input.maxLength = 64;
  input.autocomplete = "off";
  input.spellcheck = false;
  input.setAttribute("aria-describedby", "storage-dr-region-hint");
  const hint = document.createElement("p");
  hint.className = "form-hint";
  hint.id = "storage-dr-region-hint";
  group.append(label, input, hint);
  anchor.after(group);
  return input;
}

// drFillTargetForm shows the region label of target (none for a new one).
function drFillTargetForm(target) {
  const input = drRegionInput();
  if (!input) return;
  // A detected region (AWS bucket location) is shown as a hint, not as a label:
  // saving the form must not turn it into one.
  const detected = !!(target && target.region_detected);
  input.value = detected ? "" : (target && target.region) || "";
  input.placeholder = detected ? String(target.region || "") : "";
  setText("storage-dr-region-label", t("dr.region"));
  setText("storage-dr-region-hint", t("dr.region_hint"));
}

// drTargetPayload returns the region label of a target save (app.js).
function drTargetPayload() {
  const input = document.getElementById("storage-dr-region");
  return input ? { region: input.value.trim() } : {};
}

// ---------------------------------------------------------------------------
// Settings → Security: the locked copies default
// ---------------------------------------------------------------------------

// drSecurityBox returns the locked copies checkbox, created on first use below
// the two-person rule.
function drSecurityBox() {
  let box = document.getElementById("set-require-locked-copies");
  if (box) return box;
  const second = document.getElementById("set-require-second-approver");
  const anchor = second && second.closest("label");
  if (!anchor) return null;
  const row = drCheck("set-require-locked-copies");
  anchor.after(row);
  return document.getElementById("set-require-locked-copies");
}

// drFillSecurity shows security.require_locked_copies (protection.js).
function drFillSecurity(sec) {
  const box = drSecurityBox();
  if (!box) return;
  box.checked = !!(sec && sec.require_locked_copies);
  setText("set-require-locked-copies-title", t("dr.settings_locked"));
  setText("set-require-locked-copies-hint", t("dr.settings_locked_hint"));
}

// drCollectSecurity returns security.require_locked_copies (protection.js).
function drCollectSecurity() {
  const box = document.getElementById("set-require-locked-copies");
  return box ? { require_locked_copies: box.checked } : {};
}

// drPendingText describes a pending change that turns locked copies off, or
// returns "" for another kind (protection.js).
function drPendingText(c, when) {
  if (c && c.kind === "job_locked_copies") return tf("dr.pending_job_locked", { job: String(c.job_id || ""), when });
  return c && c.kind === "disable_locked_copies" ? tf("dr.pending_locked", { when }) : "";
}

// ---------------------------------------------------------------------------
// PITR: stream copy targets and the copy chain a restore reads
// ---------------------------------------------------------------------------

// drStreamFields creates, on first use, the copy target list of the PITR form.
function drStreamFields() {
  let list = document.getElementById("pitr-copy-targets");
  if (list) return list;
  const cron = document.getElementById("pitr-chain-test-cron");
  const anchor = cron && cron.closest(".form-group");
  if (!anchor) return null;
  const group = document.createElement("div");
  group.className = "form-group";
  const label = document.createElement("span");
  label.className = "form-label";
  label.id = "pitr-copy-targets-label";
  list = document.createElement("div");
  list.id = "pitr-copy-targets";
  list.setAttribute("role", "group");
  list.setAttribute("aria-labelledby", "pitr-copy-targets-label");
  const hint = document.createElement("p");
  hint.className = "form-hint";
  hint.id = "pitr-copy-targets-hint";
  group.append(label, list, hint);
  anchor.after(group);
  return list;
}

// drRenderStreamForm lists every storage target but the default (the stream's
// primary) as a copy target of a new PITR stream, at most three checked.
function drRenderStreamForm() {
  const list = drStreamFields();
  if (!list) return;
  const checked = Array.from(list.querySelectorAll("input:checked")).map(b => b.value);
  list.textContent = "";
  const primary = typeof defaultStorageTarget === "function" ? defaultStorageTarget() : null;
  (state.storageTargets || []).filter(s => !primary || s.id !== primary.id).forEach(s => {
    const wrap = document.createElement("label");
    wrap.className = "check";
    const box = document.createElement("input");
    box.type = "checkbox";
    box.value = s.id;
    box.checked = checked.includes(s.id);
    box.addEventListener("change", () => {
      const all = Array.from(list.querySelectorAll("input"));
      const n = all.filter(b => b.checked).length;
      all.forEach(b => { b.disabled = !b.checked && n >= DR_MAX_COPIES; });
    });
    const name = document.createElement("span");
    name.className = "check-title";
    name.textContent = s.name;
    wrap.append(box, name);
    list.appendChild(wrap);
  });
  setText("pitr-copy-targets-label", t("dr.stream_copies"));
  setText("pitr-copy-targets-hint", t("dr.stream_copies_hint"));
}

// drStreamPayload returns the copy targets of a new PITR stream (pitr.js).
function drStreamPayload() {
  const list = document.getElementById("pitr-copy-targets");
  if (!list) return {};
  return { copy_targets: Array.from(list.querySelectorAll("input:checked")).map(b => b.value).slice(0, DR_MAX_COPIES) };
}

// drFillPITRRestore offers the copy chains of stream s as the source of a
// point-in-time restore (pitr.js).
function drFillPITRRestore(s) {
  let select = document.getElementById("pitr-restore-source");
  if (!select) {
    const dbs = document.getElementById("pitr-restore-databases");
    const anchor = dbs && dbs.closest(".form-group");
    if (!anchor) return;
    const group = document.createElement("div");
    group.className = "form-group";
    group.id = "pitr-restore-source-group";
    const label = document.createElement("label");
    label.className = "form-label";
    label.htmlFor = "pitr-restore-source";
    label.id = "pitr-restore-source-label";
    select = document.createElement("select");
    select.id = "pitr-restore-source";
    select.className = "form-select";
    group.append(label, select);
    anchor.after(group);
  }
  const copies = (s && s.stream && Array.isArray(s.stream.copy_targets)) ? s.stream.copy_targets : [];
  select.textContent = "";
  const add = (value, text) => {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = text;
    select.appendChild(opt);
  };
  add("", t("dr.pitr_source_auto"));
  copies.forEach(id => add(id, tf("dr.pitr_source_copy", { name: drTargetLabel(id) })));
  select.value = "";
  setText("pitr-restore-source-label", t("dr.pitr_source"));
  document.getElementById("pitr-restore-source-group").hidden = copies.length === 0;
}

// drPITRRestorePayload returns the source target of a point-in-time restore.
function drPITRRestorePayload() {
  const select = document.getElementById("pitr-restore-source");
  const group = document.getElementById("pitr-restore-source-group");
  if (!select || !group || group.hidden || !select.value) return {};
  return { source_target_id: select.value };
}

// ---------------------------------------------------------------------------
// Readiness
// ---------------------------------------------------------------------------

// drReadinessCell returns the DR status of readiness row r as HTML ("" without
// copy targets): "DR: cross-region ✓" or what is missing, with the last drill.
function drReadinessCell(r) {
  const dr = r && r.dr;
  if (!dr || !dr.level) return "";
  const kind = dr.level === "cross_region" ? "success" : dr.level === "same_region" ? "danger" : "warn";
  const parts = [];
  const g = dr.last_good_drill;
  const last = dr.last_drill;
  if (last && last.status !== "ok") parts.push(tf("dr.drill_failed", { when: readinessWhen(last.at) }));
  else if (g) parts.push(tf("dr.drill_ok", { when: readinessWhen(g.at) }));
  else parts.push(t("dr.drill_none"));
  const title = parts.join(" · ");
  return ` <span class="badge badge-${kind} rd-dr" title="${escapeHtml(title)}">${escapeHtml(t(`dr.level_${dr.level}`, dr.level))}</span>`;
}

document.addEventListener("DOMContentLoaded", () => {
  drRenderStreamForm();
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      if (document.getElementById("job-rt-source")) drRenderJob();
      if (document.getElementById("storage-dr-region")) drFillTargetForm({ region: getValue("storage-dr-region") });
      const sec = document.getElementById("set-require-locked-copies");
      if (sec) drFillSecurity({ require_locked_copies: sec.checked });
      drRenderStreamForm();
    });
  }
});
