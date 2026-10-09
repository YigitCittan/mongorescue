/**
 * MongoRescue dashboard: recovery readiness and recovery point objectives.
 *
 * The Overview's "Recovery readiness" panel (GET /api/v1/readiness: one row per
 * database a job backs up, with its last good, verified and restore-tested backups,
 * its RPO, the estimated RTO, whether the keys are escrowed and an ok / warn / fail
 * status), the RPO entries of the "Attention needed" list (the server's RPO status,
 * not a rule of its own), and the job form's RPO field with the default of the
 * schedule (GET /api/v1/schedule/preview: default_rpo_minutes).
 *
 * Loaded after app.js, trust.js (mergeTranslations), nav.js (navOpenDetail) and
 * overview.js (ovWhen), which calls readinessLoad(), readinessRender() and
 * readinessAttention(). Server values reach the DOM only through escapeHtml() or
 * textContent; statuses are always written as text, never as colour alone.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const READINESS_TRANSLATIONS = {
  en: {
    readiness: {
      title: "Recovery readiness",
      sub: "{ok} ready · {warn} with warnings · {fail} not ready",
      none: "No job backs up a database yet.",
      caption: "Recovery readiness of every database a job backs up",
      load_failed: "The readiness report could not be loaded.",
      col_database: "Database",
      col_jobs: "Jobs",
      col_last_good: "Last good backup",
      partial_excluded: "Covered partially: {n} collections excluded",
      partial_included: "Covered partially: only {n} collections",
      col_verified: "Last verified",
      col_restore_test: "Restore test",
      col_rpo: "RPO",
      col_rto: "Estimated RTO",
      col_keys: "Keys escrowed",
      col_status: "Status",
      status_ok: "Ready",
      status_warn: "Warning",
      status_fail: "Not ready",
      never: "Never",
      rt_ok: "Passed {when}",
      rt_mismatch: "Differences {when}",
      rt_error: "Failed {when}",
      rpo_age: "{age} of {target}",
      rpo_met: "met",
      rpo_missed: "missed",
      rpo_paused: "paused",
      rpo_no_backup: "no backup yet, target {target}",
      rto_restore_test: "{d}, from a restore test",
      rto_restore: "{d}, from a restore",
      rto_unknown: "Unknown",
      keys_yes: "Yes",
      keys_no: "No",
      keys_title: "Whether a recovery kit holds the current secret.key and encryption keys",
      reason_rpo_missed: "RPO missed",
      reason_restore_test_failed: "Restore test failed",
      reason_verification_failed: "Verification failed",
      reason_no_backup: "No backup yet",
      reason_paused: "Jobs paused",
      reason_not_verified: "Not verified",
      reason_no_restore_test: "No restore test",
      reason_keys_not_escrowed: "Keys not in a recovery kit",
      dur_d: "{n} d",
      dur_h: "{n} h",
      dur_m: "{n} min",
      att_rpo: "{job}: no successful backup for {age} (RPO {rpo})",
      att_rpo_db: "{job}: {db} has no successful backup for {age} (RPO {rpo})",
      att_rpo_never: "{job}: no successful backup yet (RPO {rpo})",
      att_rpo_never_db: "{job}: {db} has no successful backup yet (RPO {rpo})",
      rpo_label: "Recovery point objective (hours)",
      rpo_hint: "How old the newest successful backup may be. Leave empty for the default: two schedule intervals plus an hour, at least 6 hours. 0.25 (15 minutes) to 2160 (90 days).",
      rpo_hint_default: "How old the newest successful backup may be. Leave empty for the default of this schedule: {rpo}. 0.25 (15 minutes) to 2160 (90 days).",
      details_rpo: "RPO",
      details_rpo_default: "{rpo} (default)",
    },
    notify: { events: { job_rpo_missed: "Recovery point objective missed", job_rpo_recovered: "Recovery point objective met again" } },
  },
  tr: {
    readiness: {
      title: "Kurtarmaya hazırlık",
      sub: "{ok} hazır · {warn} uyarılı · {fail} hazır değil",
      none: "Henüz hiçbir görev bir veritabanını yedeklemiyor.",
      caption: "Görevlerin yedeklediği her veritabanının kurtarmaya hazırlığı",
      load_failed: "Hazırlık raporu yüklenemedi.",
      col_database: "Veritabanı",
      col_jobs: "Görevler",
      col_last_good: "Son başarılı yedek",
      partial_excluded: "Kısmen kapsanıyor: {n} koleksiyon hariç",
      partial_included: "Kısmen kapsanıyor: yalnızca {n} koleksiyon",
      col_verified: "Son doğrulanan",
      col_restore_test: "Geri yükleme testi",
      col_rpo: "RPO",
      col_rto: "Tahmini RTO",
      col_keys: "Anahtarlar emanette",
      col_status: "Durum",
      status_ok: "Hazır",
      status_warn: "Uyarı",
      status_fail: "Hazır değil",
      never: "Hiç",
      rt_ok: "Geçti {when}",
      rt_mismatch: "Farklar var {when}",
      rt_error: "Başarısız {when}",
      rpo_age: "{age} / {target}",
      rpo_met: "karşılanıyor",
      rpo_missed: "kaçırıldı",
      rpo_paused: "duraklatıldı",
      rpo_no_backup: "henüz yedek yok, hedef {target}",
      rto_restore_test: "{d}, geri yükleme testinden",
      rto_restore: "{d}, bir geri yüklemeden",
      rto_unknown: "Bilinmiyor",
      keys_yes: "Evet",
      keys_no: "Hayır",
      keys_title: "Güncel secret.key ve şifreleme anahtarlarının bir kurtarma kitinde olup olmadığı",
      reason_rpo_missed: "RPO kaçırıldı",
      reason_restore_test_failed: "Geri yükleme testi başarısız",
      reason_verification_failed: "Doğrulama başarısız",
      reason_no_backup: "Henüz yedek yok",
      reason_paused: "Görevler duraklatıldı",
      reason_not_verified: "Doğrulanmadı",
      reason_no_restore_test: "Geri yükleme testi yok",
      reason_keys_not_escrowed: "Anahtarlar kurtarma kitinde değil",
      dur_d: "{n} g",
      dur_h: "{n} sa",
      dur_m: "{n} dk",
      att_rpo: "{job}: {age} süredir başarılı yedek yok (RPO {rpo})",
      att_rpo_db: "{job}: {db} için {age} süredir başarılı yedek yok (RPO {rpo})",
      att_rpo_never: "{job}: henüz başarılı yedek yok (RPO {rpo})",
      att_rpo_never_db: "{job}: {db} için henüz başarılı yedek yok (RPO {rpo})",
      rpo_label: "Kurtarma noktası hedefi (saat)",
      rpo_hint: "En yeni başarılı yedeğin en fazla ne kadar eski olabileceği. Varsayılan için boş bırakın: iki zamanlama aralığı artı bir saat, en az 6 saat. 0,25 (15 dakika) ile 2160 (90 gün) arası.",
      rpo_hint_default: "En yeni başarılı yedeğin en fazla ne kadar eski olabileceği. Bu zamanlamanın varsayılanı için boş bırakın: {rpo}. 0,25 (15 dakika) ile 2160 (90 gün) arası.",
      details_rpo: "RPO",
      details_rpo_default: "{rpo} (varsayılan)",
    },
    notify: { events: { job_rpo_missed: "Kurtarma noktası hedefi kaçırıldı", job_rpo_recovered: "Kurtarma noktası hedefi yeniden karşılanıyor" } },
  },
  de: {
    readiness: {
      title: "Wiederherstellungsbereitschaft",
      sub: "{ok} bereit · {warn} mit Warnungen · {fail} nicht bereit",
      none: "Noch sichert kein Job eine Datenbank.",
      caption: "Wiederherstellungsbereitschaft jeder Datenbank, die ein Job sichert",
      load_failed: "Der Bereitschaftsbericht konnte nicht geladen werden.",
      col_database: "Datenbank",
      col_jobs: "Jobs",
      col_last_good: "Letzte gute Sicherung",
      partial_excluded: "Teilweise abgedeckt: {n} Collections ausgeschlossen",
      partial_included: "Teilweise abgedeckt: nur {n} Collections",
      col_verified: "Zuletzt geprüft",
      col_restore_test: "Wiederherstellungstest",
      col_rpo: "RPO",
      col_rto: "Geschätzte RTO",
      col_keys: "Schlüssel hinterlegt",
      col_status: "Status",
      status_ok: "Bereit",
      status_warn: "Warnung",
      status_fail: "Nicht bereit",
      never: "Nie",
      rt_ok: "Bestanden {when}",
      rt_mismatch: "Abweichungen {when}",
      rt_error: "Fehlgeschlagen {when}",
      rpo_age: "{age} von {target}",
      rpo_met: "eingehalten",
      rpo_missed: "verfehlt",
      rpo_paused: "pausiert",
      rpo_no_backup: "noch keine Sicherung, Ziel {target}",
      rto_restore_test: "{d}, aus einem Wiederherstellungstest",
      rto_restore: "{d}, aus einer Wiederherstellung",
      rto_unknown: "Unbekannt",
      keys_yes: "Ja",
      keys_no: "Nein",
      keys_title: "Ob ein Wiederherstellungspaket den aktuellen secret.key und die Verschlüsselungsschlüssel enthält",
      reason_rpo_missed: "RPO verfehlt",
      reason_restore_test_failed: "Wiederherstellungstest fehlgeschlagen",
      reason_verification_failed: "Prüfung fehlgeschlagen",
      reason_no_backup: "Noch keine Sicherung",
      reason_paused: "Jobs pausiert",
      reason_not_verified: "Nicht geprüft",
      reason_no_restore_test: "Kein Wiederherstellungstest",
      reason_keys_not_escrowed: "Schlüssel nicht in einem Wiederherstellungspaket",
      dur_d: "{n} T",
      dur_h: "{n} Std",
      dur_m: "{n} Min",
      att_rpo: "{job}: seit {age} keine erfolgreiche Sicherung (RPO {rpo})",
      att_rpo_db: "{job}: {db} seit {age} ohne erfolgreiche Sicherung (RPO {rpo})",
      att_rpo_never: "{job}: noch keine erfolgreiche Sicherung (RPO {rpo})",
      att_rpo_never_db: "{job}: {db} hat noch keine erfolgreiche Sicherung (RPO {rpo})",
      rpo_label: "Recovery Point Objective (Stunden)",
      rpo_hint: "Wie alt die neueste erfolgreiche Sicherung sein darf. Leer lassen für den Standard: zwei Zeitplanintervalle plus eine Stunde, mindestens 6 Stunden. 0,25 (15 Minuten) bis 2160 (90 Tage).",
      rpo_hint_default: "Wie alt die neueste erfolgreiche Sicherung sein darf. Leer lassen für den Standard dieses Zeitplans: {rpo}. 0,25 (15 Minuten) bis 2160 (90 Tage).",
      details_rpo: "RPO",
      details_rpo_default: "{rpo} (Standard)",
    },
    notify: { events: { job_rpo_missed: "Recovery Point Objective verfehlt", job_rpo_recovered: "Recovery Point Objective wieder eingehalten" } },
  },
  es: {
    readiness: {
      title: "Preparación para la recuperación",
      sub: "{ok} listas · {warn} con avisos · {fail} no listas",
      none: "Todavía ningún trabajo respalda una base de datos.",
      caption: "Preparación para la recuperación de cada base de datos que respalda un trabajo",
      load_failed: "No se pudo cargar el informe de preparación.",
      col_database: "Base de datos",
      col_jobs: "Trabajos",
      col_last_good: "Última copia correcta",
      partial_excluded: "Cubierta en parte: {n} colecciones excluidas",
      partial_included: "Cubierta en parte: solo {n} colecciones",
      col_verified: "Última verificada",
      col_restore_test: "Prueba de restauración",
      col_rpo: "RPO",
      col_rto: "RTO estimado",
      col_keys: "Claves custodiadas",
      col_status: "Estado",
      status_ok: "Lista",
      status_warn: "Aviso",
      status_fail: "No lista",
      never: "Nunca",
      rt_ok: "Superada {when}",
      rt_mismatch: "Con diferencias {when}",
      rt_error: "Fallida {when}",
      rpo_age: "{age} de {target}",
      rpo_met: "cumplido",
      rpo_missed: "incumplido",
      rpo_paused: "en pausa",
      rpo_no_backup: "aún sin copia, objetivo {target}",
      rto_restore_test: "{d}, de una prueba de restauración",
      rto_restore: "{d}, de una restauración",
      rto_unknown: "Desconocido",
      keys_yes: "Sí",
      keys_no: "No",
      keys_title: "Si un kit de recuperación contiene el secret.key y las claves de cifrado actuales",
      reason_rpo_missed: "RPO incumplido",
      reason_restore_test_failed: "Prueba de restauración fallida",
      reason_verification_failed: "Verificación fallida",
      reason_no_backup: "Aún sin copia",
      reason_paused: "Trabajos en pausa",
      reason_not_verified: "Sin verificar",
      reason_no_restore_test: "Sin prueba de restauración",
      reason_keys_not_escrowed: "Claves fuera de un kit de recuperación",
      dur_d: "{n} d",
      dur_h: "{n} h",
      dur_m: "{n} min",
      att_rpo: "{job}: sin copia correcta desde hace {age} (RPO {rpo})",
      att_rpo_db: "{job}: {db} sin copia correcta desde hace {age} (RPO {rpo})",
      att_rpo_never: "{job}: aún sin copia correcta (RPO {rpo})",
      att_rpo_never_db: "{job}: {db} aún no tiene copia correcta (RPO {rpo})",
      rpo_label: "Objetivo de punto de recuperación (horas)",
      rpo_hint: "Antigüedad máxima de la copia correcta más reciente. Déjelo vacío para el valor predeterminado: dos intervalos de la programación más una hora, al menos 6 horas. De 0,25 (15 minutos) a 2160 (90 días).",
      rpo_hint_default: "Antigüedad máxima de la copia correcta más reciente. Déjelo vacío para el valor predeterminado de esta programación: {rpo}. De 0,25 (15 minutos) a 2160 (90 días).",
      details_rpo: "RPO",
      details_rpo_default: "{rpo} (predeterminado)",
    },
    notify: { events: { job_rpo_missed: "Objetivo de punto de recuperación incumplido", job_rpo_recovered: "Objetivo de punto de recuperación cumplido de nuevo" } },
  },
  fr: {
    readiness: {
      title: "Capacité de restauration",
      sub: "{ok} prêtes · {warn} avec avertissements · {fail} non prêtes",
      none: "Aucune tâche ne sauvegarde encore de base.",
      caption: "Capacité de restauration de chaque base sauvegardée par une tâche",
      load_failed: "Le rapport de capacité de restauration n'a pas pu être chargé.",
      col_database: "Base",
      col_jobs: "Tâches",
      col_last_good: "Dernière sauvegarde réussie",
      partial_excluded: "Couverte en partie : {n} collections exclues",
      partial_included: "Couverte en partie : seulement {n} collections",
      col_verified: "Dernière vérifiée",
      col_restore_test: "Test de restauration",
      col_rpo: "RPO",
      col_rto: "RTO estimé",
      col_keys: "Clés déposées",
      col_status: "État",
      status_ok: "Prête",
      status_warn: "Avertissement",
      status_fail: "Non prête",
      never: "Jamais",
      rt_ok: "Réussi {when}",
      rt_mismatch: "Écarts {when}",
      rt_error: "Échoué {when}",
      rpo_age: "{age} sur {target}",
      rpo_met: "respecté",
      rpo_missed: "manqué",
      rpo_paused: "en pause",
      rpo_no_backup: "pas encore de sauvegarde, objectif {target}",
      rto_restore_test: "{d}, d'après un test de restauration",
      rto_restore: "{d}, d'après une restauration",
      rto_unknown: "Inconnu",
      keys_yes: "Oui",
      keys_no: "Non",
      keys_title: "Si un kit de récupération contient le secret.key et les clés de chiffrement actuels",
      reason_rpo_missed: "RPO manqué",
      reason_restore_test_failed: "Test de restauration échoué",
      reason_verification_failed: "Vérification échouée",
      reason_no_backup: "Pas encore de sauvegarde",
      reason_paused: "Tâches en pause",
      reason_not_verified: "Non vérifiée",
      reason_no_restore_test: "Aucun test de restauration",
      reason_keys_not_escrowed: "Clés absentes d'un kit de récupération",
      dur_d: "{n} j",
      dur_h: "{n} h",
      dur_m: "{n} min",
      att_rpo: "{job} : aucune sauvegarde réussie depuis {age} (RPO {rpo})",
      att_rpo_db: "{job} : {db} sans sauvegarde réussie depuis {age} (RPO {rpo})",
      att_rpo_never: "{job} : pas encore de sauvegarde réussie (RPO {rpo})",
      att_rpo_never_db: "{job} : {db} n'a pas encore de sauvegarde réussie (RPO {rpo})",
      rpo_label: "Objectif de point de récupération (heures)",
      rpo_hint: "Âge maximal de la sauvegarde réussie la plus récente. Laissez vide pour la valeur par défaut : deux intervalles de la planification plus une heure, au moins 6 heures. De 0,25 (15 minutes) à 2160 (90 jours).",
      rpo_hint_default: "Âge maximal de la sauvegarde réussie la plus récente. Laissez vide pour la valeur par défaut de cette planification : {rpo}. De 0,25 (15 minutes) à 2160 (90 jours).",
      details_rpo: "RPO",
      details_rpo_default: "{rpo} (par défaut)",
    },
    notify: { events: { job_rpo_missed: "Objectif de point de récupération manqué", job_rpo_recovered: "Objectif de point de récupération de nouveau respecté" } },
  },
  zh: {
    readiness: {
      title: "恢复就绪状态",
      sub: "{ok} 个就绪 · {warn} 个有警告 · {fail} 个未就绪",
      none: "还没有任务备份任何数据库。",
      caption: "每个由任务备份的数据库的恢复就绪状态",
      load_failed: "无法加载就绪报告。",
      col_database: "数据库",
      col_jobs: "任务",
      col_last_good: "最近成功备份",
      partial_excluded: "部分覆盖：排除了 {n} 个集合",
      partial_included: "部分覆盖：仅 {n} 个集合",
      col_verified: "最近验证",
      col_restore_test: "恢复测试",
      col_rpo: "RPO",
      col_rto: "预计 RTO",
      col_keys: "密钥已托管",
      col_status: "状态",
      status_ok: "就绪",
      status_warn: "警告",
      status_fail: "未就绪",
      never: "从未",
      rt_ok: "通过 {when}",
      rt_mismatch: "存在差异 {when}",
      rt_error: "失败 {when}",
      rpo_age: "{age} / {target}",
      rpo_met: "达标",
      rpo_missed: "未达标",
      rpo_paused: "已暂停",
      rpo_no_backup: "尚无备份，目标 {target}",
      rto_restore_test: "{d}，来自恢复测试",
      rto_restore: "{d}，来自一次恢复",
      rto_unknown: "未知",
      keys_yes: "是",
      keys_no: "否",
      keys_title: "恢复包是否包含当前的 secret.key 和加密密钥",
      reason_rpo_missed: "RPO 未达标",
      reason_restore_test_failed: "恢复测试失败",
      reason_verification_failed: "验证失败",
      reason_no_backup: "尚无备份",
      reason_paused: "任务已暂停",
      reason_not_verified: "未验证",
      reason_no_restore_test: "无恢复测试",
      reason_keys_not_escrowed: "密钥不在恢复包中",
      dur_d: "{n} 天",
      dur_h: "{n} 小时",
      dur_m: "{n} 分钟",
      att_rpo: "{job}：已 {age} 没有成功的备份（RPO {rpo}）",
      att_rpo_db: "{job}：{db} 已 {age} 没有成功的备份（RPO {rpo}）",
      att_rpo_never: "{job}：尚无成功的备份（RPO {rpo}）",
      att_rpo_never_db: "{job}：{db} 尚无成功的备份（RPO {rpo}）",
      rpo_label: "恢复点目标（小时）",
      rpo_hint: "最新成功备份允许的最大时长。留空则使用默认值：两个计划间隔加一小时，至少 6 小时。范围 0.25（15 分钟）到 2160（90 天）。",
      rpo_hint_default: "最新成功备份允许的最大时长。留空则使用此计划的默认值：{rpo}。范围 0.25（15 分钟）到 2160（90 天）。",
      details_rpo: "RPO",
      details_rpo_default: "{rpo}（默认）",
    },
    notify: { events: { job_rpo_missed: "未达到恢复点目标", job_rpo_recovered: "恢复点目标再次达标" } },
  },
  ja: {
    readiness: {
      title: "復旧準備状況",
      sub: "準備完了 {ok} · 警告あり {warn} · 未準備 {fail}",
      none: "データベースをバックアップするジョブはまだありません。",
      caption: "ジョブがバックアップする各データベースの復旧準備状況",
      load_failed: "準備状況レポートを読み込めませんでした。",
      col_database: "データベース",
      col_jobs: "ジョブ",
      col_last_good: "最新の成功バックアップ",
      partial_excluded: "一部のみ対象: {n} 個のコレクションを除外",
      partial_included: "一部のみ対象: {n} 個のコレクションのみ",
      col_verified: "最新の検証済み",
      col_restore_test: "復元テスト",
      col_rpo: "RPO",
      col_rto: "推定 RTO",
      col_keys: "鍵の預託",
      col_status: "状態",
      status_ok: "準備完了",
      status_warn: "警告",
      status_fail: "未準備",
      never: "なし",
      rt_ok: "成功 {when}",
      rt_mismatch: "差異あり {when}",
      rt_error: "失敗 {when}",
      rpo_age: "{age} / {target}",
      rpo_met: "達成",
      rpo_missed: "未達",
      rpo_paused: "一時停止中",
      rpo_no_backup: "バックアップなし、目標 {target}",
      rto_restore_test: "{d}（復元テストより）",
      rto_restore: "{d}（復元より）",
      rto_unknown: "不明",
      keys_yes: "はい",
      keys_no: "いいえ",
      keys_title: "リカバリキットに現在の secret.key と暗号鍵が含まれているか",
      reason_rpo_missed: "RPO 未達",
      reason_restore_test_failed: "復元テスト失敗",
      reason_verification_failed: "検証失敗",
      reason_no_backup: "バックアップなし",
      reason_paused: "ジョブ一時停止中",
      reason_not_verified: "未検証",
      reason_no_restore_test: "復元テストなし",
      reason_keys_not_escrowed: "鍵がリカバリキットにありません",
      dur_d: "{n} 日",
      dur_h: "{n} 時間",
      dur_m: "{n} 分",
      att_rpo: "{job}: {age} 成功したバックアップがありません（RPO {rpo}）",
      att_rpo_db: "{job}: {db} は {age} 成功したバックアップがありません（RPO {rpo}）",
      att_rpo_never: "{job}: 成功したバックアップがまだありません（RPO {rpo}）",
      att_rpo_never_db: "{job}: {db} には成功したバックアップがまだありません（RPO {rpo}）",
      rpo_label: "目標復旧時点（時間）",
      rpo_hint: "最新の成功バックアップが許容される古さ。空欄で既定値：スケジュール間隔の 2 倍に 1 時間を加えた値（最低 6 時間）。0.25（15 分）〜 2160（90 日）。",
      rpo_hint_default: "最新の成功バックアップが許容される古さ。空欄でこのスケジュールの既定値：{rpo}。0.25（15 分）〜 2160（90 日）。",
      details_rpo: "RPO",
      details_rpo_default: "{rpo}（既定）",
    },
    notify: { events: { job_rpo_missed: "目標復旧時点の未達", job_rpo_recovered: "目標復旧時点を再び達成" } },
  },
  ru: {
    readiness: {
      title: "Готовность к восстановлению",
      sub: "{ok} готовы · {warn} с предупреждениями · {fail} не готовы",
      none: "Пока ни одно задание не создаёт копии базы данных.",
      caption: "Готовность к восстановлению каждой базы данных, копируемой заданием",
      load_failed: "Не удалось загрузить отчёт о готовности.",
      col_database: "База данных",
      col_jobs: "Задания",
      col_last_good: "Последняя успешная копия",
      partial_excluded: "Покрыта частично: исключено коллекций: {n}",
      partial_included: "Покрыта частично: только коллекций: {n}",
      col_verified: "Последняя проверенная",
      col_restore_test: "Тест восстановления",
      col_rpo: "RPO",
      col_rto: "Оценка RTO",
      col_keys: "Ключи сохранены",
      col_status: "Статус",
      status_ok: "Готова",
      status_warn: "Предупреждение",
      status_fail: "Не готова",
      never: "Никогда",
      rt_ok: "Пройден {when}",
      rt_mismatch: "Есть расхождения {when}",
      rt_error: "Не пройден {when}",
      rpo_age: "{age} из {target}",
      rpo_met: "соблюдён",
      rpo_missed: "нарушен",
      rpo_paused: "приостановлено",
      rpo_no_backup: "копий ещё нет, цель {target}",
      rto_restore_test: "{d}, по тесту восстановления",
      rto_restore: "{d}, по восстановлению",
      rto_unknown: "Неизвестно",
      keys_yes: "Да",
      keys_no: "Нет",
      keys_title: "Содержит ли комплект восстановления текущий secret.key и ключи шифрования",
      reason_rpo_missed: "RPO нарушен",
      reason_restore_test_failed: "Тест восстановления не пройден",
      reason_verification_failed: "Проверка не пройдена",
      reason_no_backup: "Копий ещё нет",
      reason_paused: "Задания приостановлены",
      reason_not_verified: "Не проверена",
      reason_no_restore_test: "Нет теста восстановления",
      reason_keys_not_escrowed: "Ключей нет в комплекте восстановления",
      dur_d: "{n} д",
      dur_h: "{n} ч",
      dur_m: "{n} мин",
      att_rpo: "{job}: нет успешной копии уже {age} (RPO {rpo})",
      att_rpo_db: "{job}: у {db} нет успешной копии уже {age} (RPO {rpo})",
      att_rpo_never: "{job}: успешных копий ещё нет (RPO {rpo})",
      att_rpo_never_db: "{job}: у {db} ещё нет успешной копии (RPO {rpo})",
      rpo_label: "Целевая точка восстановления (часы)",
      rpo_hint: "Насколько старой может быть последняя успешная копия. Оставьте пустым для значения по умолчанию: два интервала расписания плюс час, не меньше 6 часов. От 0,25 (15 минут) до 2160 (90 дней).",
      rpo_hint_default: "Насколько старой может быть последняя успешная копия. Оставьте пустым для значения по умолчанию для этого расписания: {rpo}. От 0,25 (15 минут) до 2160 (90 дней).",
      details_rpo: "RPO",
      details_rpo_default: "{rpo} (по умолчанию)",
    },
    notify: { events: { job_rpo_missed: "Целевая точка восстановления нарушена", job_rpo_recovered: "Целевая точка восстановления снова соблюдается" } },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(READINESS_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], READINESS_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// State and loading
// ---------------------------------------------------------------------------

// The RPO bounds of the job form, in minutes (models.MinRPOMinutes, MaxRPOMinutes).
const READINESS_RPO_MIN = 15;
const READINESS_RPO_MAX = 90 * 24 * 60;

const readiness = { data: null, error: "", defaultRpoMinutes: 0 };

// readinessLoad fetches GET /api/v1/readiness; overviewRefresh calls it with the
// history. Failures are kept as an error message, never thrown.
async function readinessLoad() {
  try {
    const json = await apiJSON("/api/v1/readiness");
    if (json.success && json.data) {
      readiness.data = json.data;
      readiness.error = "";
    } else {
      readiness.error = json.error || t("readiness.load_failed");
    }
  } catch (err) {
    readiness.error = t("readiness.load_failed");
  }
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

// A duration in seconds as days and hours, or hours and minutes ("2 d 3 h").
function readinessDuration(seconds) {
  const total = Math.max(0, Math.floor((Number(seconds) || 0) / 60));
  const d = Math.floor(total / 1440);
  const h = Math.floor((total % 1440) / 60);
  const m = total % 60;
  const parts = [];
  if (d > 0) parts.push(tf("readiness.dur_d", { n: d }));
  if (h > 0) parts.push(tf("readiness.dur_h", { n: h }));
  if (m > 0 && d === 0) parts.push(tf("readiness.dur_m", { n: m }));
  return parts.length ? parts.join(" ") : tf("readiness.dur_m", { n: 0 });
}

function readinessWhen(value) {
  return value ? ovWhen(value) : t("readiness.never");
}

const READINESS_STATUS = { ok: ["success", "readiness.status_ok"], warn: ["warn", "readiness.status_warn"], fail: ["danger", "readiness.status_fail"] };

// The status chip: an icon and the status as text, with the reasons spelled out.
function readinessStatusCell(row) {
  const [kind, key] = READINESS_STATUS[row.status] || READINESS_STATUS.warn;
  const reasons = (row.reasons || []).map(r => t(`readiness.reason_${r}`, r));
  return statusBadge(kind, t(key)) +
    (reasons.length ? `<span class="rd-reasons">${escapeHtml(reasons.join(" · "))}</span>` : "");
}

function readinessRpoCell(row) {
  const rpo = row.rpo || {};
  const target = readinessDuration(rpo.target_seconds);
  if (rpo.met === undefined || rpo.met === null) {
    return escapeHtml(t("readiness.rpo_paused"));
  }
  const verdict = rpo.met ? t("readiness.rpo_met") : t("readiness.rpo_missed");
  const text = rpo.age_seconds === undefined || rpo.age_seconds === null
    ? tf("readiness.rpo_no_backup", { target })
    : tf("readiness.rpo_age", { age: readinessDuration(rpo.age_seconds), target });
  // A PITR RPO comes from the oplog the stream captured, restorable to a point in
  // time (experimental).
  const pitrNote = rpo.source === "pitr"
    ? `<div class="cell-sub" title="${escapeHtml(t("readiness.rpo_pitr_note"))}">${escapeHtml(t("readiness.rpo_pitr"))}</div>`
    : "";
  return `${escapeHtml(text)} <span class="${rpo.met ? "rd-met" : "rd-missed"}">${escapeHtml(verdict)}</span>${pitrNote}`;
}

function readinessRtoCell(row) {
  const rto = row.rto;
  // The PITR estimate of the row's connection (pitr.js), under the database's.
  const pitr = typeof pitrReadinessRto === "function" ? pitrReadinessRto(row) : "";
  if (!rto) return escapeHtml(t("readiness.rto_unknown")) + pitr;
  const key = rto.source === "restore_test" ? "readiness.rto_restore_test" : "readiness.rto_restore";
  return escapeHtml(tf(key, { d: readinessDuration(Math.max(Number(rto.seconds) || 0, 60)) })) + pitr;
}

function readinessRestoreTestCell(row) {
  const rt = row.last_restore_test;
  if (!rt) return escapeHtml(t("readiness.never"));
  const key = rt.status === "ok" ? "readiness.rt_ok" : rt.status === "mismatch" ? "readiness.rt_mismatch" : "readiness.rt_error";
  return escapeHtml(tf(key, { when: ovWhen(rt.at) }));
}

// The note of a last good backup that holds only some collections of its database
// ("Covered partially: 2 collections excluded"), or "". It still counts towards the
// RPO; the collections it left out are not covered.
function readinessPartialCell(ref) {
  if (!ref || !ref.filtered) return "";
  const inc = ref.collections || [];
  const exc = ref.exclude_collections || [];
  const text = inc.length > 0
    ? tf("readiness.partial_included", { n: inc.length })
    : tf("readiness.partial_excluded", { n: exc.length });
  const title = typeof backupFilterText === "function" ? backupFilterText(ref) : "";
  return `<span class="rd-sub" title="${escapeHtml(title)}">${escapeHtml(text)}</span>`;
}

function readinessJobsCell(row) {
  return (row.jobs || []).map(j => `<button type="button" class="link-btn" data-action="rd-open-job" data-id="${escapeHtml(j.id)}">${escapeHtml(j.name || j.id)}</button>`).join(", ");
}

// ---------------------------------------------------------------------------
// Overview panel
// ---------------------------------------------------------------------------

let readinessRendered = "";

// readinessRender draws the "Recovery readiness" panel from the last report.
function readinessRender() {
  const box = document.getElementById("ov-readiness-table");
  const sub = document.getElementById("ov-readiness-sub");
  if (!box || !sub) return;
  const data = readiness.data;
  if (!data) {
    sub.textContent = readiness.error;
    sub.classList.toggle("text-danger", !!readiness.error);
    return;
  }
  sub.classList.toggle("text-danger", !!readiness.error);
  const rows = Array.isArray(data.rows) ? data.rows : [];
  const s = data.summary || {};
  sub.textContent = readiness.error || (rows.length ? tf("readiness.sub", { ok: s.ok || 0, warn: s.warn || 0, fail: s.fail || 0 }) : "");
  const card = document.getElementById("ov-readiness-card");
  if (card) card.classList.toggle("ov-card-alert", (s.fail || 0) > 0);
  let html;
  if (rows.length === 0) {
    html = `<p class="ov-empty-text">${escapeHtml(t("readiness.none"))}</p>`;
  } else {
    const head = ["col_database", "col_jobs", "col_last_good", "col_verified", "col_restore_test", "col_rpo", "col_rto", "col_keys", "col_status"]
      .map(k => `<th scope="col">${escapeHtml(t(`readiness.${k}`))}</th>`).join("");
    const body = rows.map(r => {
      const where = r.connection_name || r.connection_id || "";
      const keys = r.keys_escrowed ? t("readiness.keys_yes") : t("readiness.keys_no");
      return `<tr>
        <th scope="row"><span class="mono">${escapeHtml(r.database)}</span>${where ? `<span class="rd-sub">${escapeHtml(where)}</span>` : ""}</th>
        <td>${readinessJobsCell(r)}</td>
        <td>${escapeHtml(readinessWhen(r.last_good_backup && r.last_good_backup.at))}${readinessPartialCell(r.last_good_backup)}${typeof copiesReadinessCell === "function" ? copiesReadinessCell(r) : ""}</td>
        <td>${escapeHtml(readinessWhen(r.last_verified_backup && r.last_verified_backup.at))}</td>
        <td>${readinessRestoreTestCell(r)}</td>
        <td>${readinessRpoCell(r)}</td>
        <td>${readinessRtoCell(r)}</td>
        <td title="${escapeHtml(t("readiness.keys_title"))}">${escapeHtml(keys)}</td>
        <td>${readinessStatusCell(r)}</td>
      </tr>`;
    }).join("");
    html = `<div class="table-wrap rd-table-wrap"><table class="table rd-table">
      <caption class="sr-only">${escapeHtml(t("readiness.caption"))}</caption>
      <thead><tr>${head}</tr></thead><tbody>${body}</tbody></table></div>`;
  }
  // How the storage targets protect backups against deletion (objectlock.js).
  if (typeof objectLockReadinessHints === "function") html += objectLockReadinessHints(data);
  // Unchanged markup is not rewritten, so focus stays put across refreshes.
  if (html !== readinessRendered) {
    readinessRendered = html;
    box.innerHTML = html;
  }
}

// readinessAttention returns the "Attention needed" items of the jobs whose RPO the
// server reports as missed, one per job (its stalest database), skipping the jobs in
// skip (already listed with a failed run).
function readinessAttention(skip) {
  const data = readiness.data;
  if (!data || !Array.isArray(data.rows)) return [];
  const worst = new Map();
  data.rows.forEach(row => {
    (row.jobs || []).forEach(j => {
      if (!j.enabled || j.met || (skip && skip.has(j.id))) return;
      const cur = worst.get(j.id);
      if (!cur || Number(j.age_seconds) > Number(cur.j.age_seconds)) worst.set(j.id, { j, db: row.database });
    });
  });
  const items = [];
  worst.forEach(({ j, db }) => {
    const job = state.jobs.find(x => x.id === j.id);
    const name = job ? (job.name || job.id) : (j.name || j.id);
    const multi = job && typeof jobIsMulti === "function" && jobIsMulti(job);
    const rpo = readinessDuration(j.target_seconds);
    const vars = { job: name, db, rpo, age: readinessDuration(j.age_seconds) };
    const key = j.no_backup ? (multi ? "readiness.att_rpo_never_db" : "readiness.att_rpo_never") : (multi ? "readiness.att_rpo_db" : "readiness.att_rpo");
    items.push({ sev: "warn", text: tf(key, vars), open: () => navOpenDetail("jobs", j.id) });
  });
  return items;
}

// ---------------------------------------------------------------------------
// Job form
// ---------------------------------------------------------------------------

// readinessRpoHint shows the default RPO of the schedule being edited (from the
// schedule preview), or the general rule when it is unknown.
function readinessRpoHint(defaultMinutes) {
  readiness.defaultRpoMinutes = Number(defaultMinutes) || 0;
  const hint = document.getElementById("job-rpo-hint");
  if (!hint) return;
  hint.textContent = readiness.defaultRpoMinutes > 0
    ? tf("readiness.rpo_hint_default", { rpo: readinessDuration(readiness.defaultRpoMinutes * 60) })
    : t("readiness.rpo_hint");
}

// readinessFillJobForm sets the RPO field from job (empty for a new job or the
// default).
function readinessFillJobForm(job) {
  const input = document.getElementById("job-rpo");
  if (!input) return;
  const minutes = job ? Number(job.rpo_minutes) || 0 : 0;
  input.value = minutes > 0 ? String(Math.round((minutes / 60) * 100) / 100) : "";
  readinessRpoHint(0);
}

// readinessJobPayload returns the job form's rpo_minutes (0 for the default).
function readinessJobPayload() {
  const input = document.getElementById("job-rpo");
  if (!input) return {};
  const hours = parseFloat(String(input.value).replace(",", "."));
  if (!Number.isFinite(hours) || hours <= 0) return { rpo_minutes: 0 };
  return { rpo_minutes: Math.round(hours * 60) };
}

// Live check of the RPO field against the server's bounds, so the browser refuses
// the form before it is sent.
function readinessCheckRpo() {
  const input = document.getElementById("job-rpo");
  if (!input) return;
  const { rpo_minutes: m } = readinessJobPayload();
  input.setCustomValidity(m === 0 || (m >= READINESS_RPO_MIN && m <= READINESS_RPO_MAX) ? "" : t("readiness.rpo_hint"));
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

function setupReadiness() {
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-action='rd-open-job']");
    if (!btn || btn.disabled) return;
    navOpenDetail("jobs", btn.dataset.id || "");
  });
  const input = document.getElementById("job-rpo");
  if (input) input.addEventListener("input", readinessCheckRpo);
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      readinessRendered = "";
      readinessRender();
      readinessRpoHint(readiness.defaultRpoMinutes);
    });
  }
  readinessRpoHint(0);
}

document.addEventListener("DOMContentLoaded", setupReadiness);
