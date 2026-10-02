/**
 * MongoRescue dashboard: the Overview tab and the jobs' sparklines.
 *
 * The success rate and storage growth of the last 30 days, the scheduled runs of
 * the next 24 hours and the "attention needed" list, drawn as inline SVG (no chart
 * library) from GET /api/v1/stats/history, plus the per-job sparkline of recent
 * outcomes and durations in the Jobs table. Every chart has a text equivalent: a
 * role="img" label and a data table.
 *
 * Loaded after nav.js (navMergeI18n, navOpenDetail, navSetServerZone) and app.js
 * (state, t, tf, apiJSON, escapeHtml, formatBytes, ...); app.js calls
 * overviewRefresh() and jobSparkline(). Server values reach the DOM only through
 * escapeHtml(); colours come from the theme tokens in style.css via classes.
 */

// ---------------------------------------------------------------------------
// Translations
// ---------------------------------------------------------------------------

const OVERVIEW_I18N = {
  en: {
    overview: {
      desc: "Backup health over the last 30 days, the next 24 hours of scheduled runs and what needs attention.",
      success_title: "Success rate",
      success_sub: "{ok} completed · {failed} failed · {cancelled} cancelled in 30 days",
      success_none: "No finished backups in the last 30 days",
      success_chart: "Backups per day over the last 30 days: {ok} completed, {failed} failed, {cancelled} cancelled.",
      storage_title: "Storage growth",
      storage_sub: "Completed backups on record · {delta} added in 30 days",
      storage_chart: "Stored size at the end of each day over the last 30 days, from {from} to {to}.",
      upcoming_title: "Next 24 hours",
      upcoming_sub: "{n} scheduled runs",
      upcoming_none: "No runs scheduled in the next 24 hours.",
      upcoming_more: "+{n} more jobs",
      upcoming_truncated: "Very frequent schedules are cut short.",
      upcoming_chart: "Scheduled runs of {jobs} jobs in the next 24 hours.",
      attention_title: "Attention needed",
      attention_none: "Nothing needs attention.",
      att_failed_last: "{job}: the last run failed {when}",
      att_verify_mismatch: "{db}: checksum mismatch in the backup of {when}",
      att_verify_error: "{db}: the backup of {when} could not be verified",
      att_verify_more: "{n} more backups failed verification",
      att_restore_test: "{job}: the last restore test failed {when}",
      att_restore_mismatch: "{job}: the last restore test found differences {when}",
      att_stale: "{job}: no successful backup for {hours} hours",
      att_never: "{job}: no successful backup yet",
      att_corrupt: "{n} stored records cannot be read",
      open: "Open",
      show_data: "Show the data",
      col_day: "Day",
      col_completed: "Completed",
      col_failed: "Failed",
      col_cancelled: "Cancelled",
      col_added: "Added",
      col_stored: "Stored",
      load_failed: "The overview could not be loaded.",
      loading: "Loading…",
      now: "now",
      spark_label: "Last {n} runs: {ok} completed, {failed} failed",
      spark_longest: "longest {d}",
      spark_run: "{when} · {status} · {d}",
    },
  },
  tr: {
    overview: {
      desc: "Son 30 günün yedek sağlığı, önümüzdeki 24 saatin zamanlanmış çalıştırmaları ve ilgilenilmesi gerekenler.",
      success_title: "Başarı oranı",
      success_sub: "30 günde {ok} tamamlandı · {failed} başarısız · {cancelled} iptal",
      success_none: "Son 30 günde biten yedek yok",
      success_chart: "Son 30 günde günlük yedekler: {ok} tamamlandı, {failed} başarısız, {cancelled} iptal.",
      storage_title: "Depolama büyümesi",
      storage_sub: "Kayıttaki tamamlanmış yedekler · 30 günde {delta} eklendi",
      storage_chart: "Son 30 günde her gün sonundaki depolanan boyut: {from} ile {to} arası.",
      upcoming_title: "Önümüzdeki 24 saat",
      upcoming_sub: "{n} zamanlanmış çalıştırma",
      upcoming_none: "Önümüzdeki 24 saatte zamanlanmış çalıştırma yok.",
      upcoming_more: "+{n} görev daha",
      upcoming_truncated: "Çok sık çalışan zamanlamalar kısaltıldı.",
      upcoming_chart: "{jobs} görevin önümüzdeki 24 saatteki zamanlanmış çalıştırmaları.",
      attention_title: "İlgilenilmesi gerekenler",
      attention_none: "İlgilenilmesi gereken bir şey yok.",
      att_failed_last: "{job}: son çalıştırma başarısız oldu ({when})",
      att_verify_mismatch: "{db}: {when} tarihli yedekte sağlama toplamı uyuşmuyor",
      att_verify_error: "{db}: {when} tarihli yedek doğrulanamadı",
      att_verify_more: "{n} yedek daha doğrulamadan geçemedi",
      att_restore_test: "{job}: son geri yükleme testi başarısız oldu ({when})",
      att_restore_mismatch: "{job}: son geri yükleme testi farklılık buldu ({when})",
      att_stale: "{job}: {hours} saattir başarılı yedek yok",
      att_never: "{job}: henüz başarılı yedek yok",
      att_corrupt: "{n} kayıt okunamıyor",
      open: "Aç",
      show_data: "Verileri göster",
      col_day: "Gün",
      col_completed: "Tamamlandı",
      col_failed: "Başarısız",
      col_cancelled: "İptal",
      col_added: "Eklenen",
      col_stored: "Depolanan",
      load_failed: "Genel bakış yüklenemedi.",
      loading: "Yükleniyor…",
      now: "şimdi",
      spark_label: "Son {n} çalıştırma: {ok} tamamlandı, {failed} başarısız",
      spark_longest: "en uzun {d}",
      spark_run: "{when} · {status} · {d}",
    },
  },
  de: {
    overview: {
      desc: "Zustand der Backups der letzten 30 Tage, die geplanten Läufe der nächsten 24 Stunden und was Aufmerksamkeit braucht.",
      success_title: "Erfolgsquote",
      success_sub: "{ok} abgeschlossen · {failed} fehlgeschlagen · {cancelled} abgebrochen in 30 Tagen",
      success_none: "Keine beendeten Backups in den letzten 30 Tagen",
      success_chart: "Backups pro Tag in den letzten 30 Tagen: {ok} abgeschlossen, {failed} fehlgeschlagen, {cancelled} abgebrochen.",
      storage_title: "Speicherwachstum",
      storage_sub: "Abgeschlossene Backups im Bestand · {delta} in 30 Tagen hinzugekommen",
      storage_chart: "Gespeicherte Größe am Ende jedes Tages der letzten 30 Tage, von {from} bis {to}.",
      upcoming_title: "Nächste 24 Stunden",
      upcoming_sub: "{n} geplante Läufe",
      upcoming_none: "In den nächsten 24 Stunden sind keine Läufe geplant.",
      upcoming_more: "+{n} weitere Aufträge",
      upcoming_truncated: "Sehr häufige Zeitpläne werden gekürzt.",
      upcoming_chart: "Geplante Läufe von {jobs} Aufträgen in den nächsten 24 Stunden.",
      attention_title: "Aufmerksamkeit nötig",
      attention_none: "Nichts braucht Aufmerksamkeit.",
      att_failed_last: "{job}: Der letzte Lauf ist fehlgeschlagen ({when})",
      att_verify_mismatch: "{db}: Prüfsummenabweichung im Backup von {when}",
      att_verify_error: "{db}: Das Backup von {when} konnte nicht geprüft werden",
      att_verify_more: "{n} weitere Backups haben die Prüfung nicht bestanden",
      att_restore_test: "{job}: Der letzte Wiederherstellungstest ist fehlgeschlagen ({when})",
      att_restore_mismatch: "{job}: Der letzte Wiederherstellungstest hat Abweichungen gefunden ({when})",
      att_stale: "{job}: seit {hours} Stunden kein erfolgreiches Backup",
      att_never: "{job}: noch kein erfolgreiches Backup",
      att_corrupt: "{n} gespeicherte Datensätze können nicht gelesen werden",
      open: "Öffnen",
      show_data: "Daten anzeigen",
      col_day: "Tag",
      col_completed: "Abgeschlossen",
      col_failed: "Fehlgeschlagen",
      col_cancelled: "Abgebrochen",
      col_added: "Hinzugekommen",
      col_stored: "Gespeichert",
      load_failed: "Die Übersicht konnte nicht geladen werden.",
      loading: "Wird geladen…",
      now: "jetzt",
      spark_label: "Letzte {n} Läufe: {ok} abgeschlossen, {failed} fehlgeschlagen",
      spark_longest: "längster {d}",
      spark_run: "{when} · {status} · {d}",
    },
  },
  es: {
    overview: {
      desc: "Estado de las copias de los últimos 30 días, las ejecuciones programadas de las próximas 24 horas y lo que requiere atención.",
      success_title: "Tasa de éxito",
      success_sub: "{ok} completadas · {failed} fallidas · {cancelled} canceladas en 30 días",
      success_none: "No hay copias finalizadas en los últimos 30 días",
      success_chart: "Copias por día en los últimos 30 días: {ok} completadas, {failed} fallidas, {cancelled} canceladas.",
      storage_title: "Crecimiento del almacenamiento",
      storage_sub: "Copias completadas registradas · {delta} añadidos en 30 días",
      storage_chart: "Tamaño almacenado al final de cada día de los últimos 30 días, de {from} a {to}.",
      upcoming_title: "Próximas 24 horas",
      upcoming_sub: "{n} ejecuciones programadas",
      upcoming_none: "No hay ejecuciones programadas en las próximas 24 horas.",
      upcoming_more: "+{n} tareas más",
      upcoming_truncated: "Las programaciones muy frecuentes se recortan.",
      upcoming_chart: "Ejecuciones programadas de {jobs} tareas en las próximas 24 horas.",
      attention_title: "Requiere atención",
      attention_none: "Nada requiere atención.",
      att_failed_last: "{job}: la última ejecución falló ({when})",
      att_verify_mismatch: "{db}: suma de comprobación distinta en la copia de {when}",
      att_verify_error: "{db}: no se pudo verificar la copia de {when}",
      att_verify_more: "{n} copias más no superaron la verificación",
      att_restore_test: "{job}: la última prueba de restauración falló ({when})",
      att_restore_mismatch: "{job}: la última prueba de restauración encontró diferencias ({when})",
      att_stale: "{job}: ninguna copia correcta desde hace {hours} horas",
      att_never: "{job}: todavía no hay ninguna copia correcta",
      att_corrupt: "No se pueden leer {n} registros almacenados",
      open: "Abrir",
      show_data: "Mostrar los datos",
      col_day: "Día",
      col_completed: "Completadas",
      col_failed: "Fallidas",
      col_cancelled: "Canceladas",
      col_added: "Añadido",
      col_stored: "Almacenado",
      load_failed: "No se pudo cargar el resumen.",
      loading: "Cargando…",
      now: "ahora",
      spark_label: "Últimas {n} ejecuciones: {ok} completadas, {failed} fallidas",
      spark_longest: "la más larga {d}",
      spark_run: "{when} · {status} · {d}",
    },
  },
  fr: {
    overview: {
      desc: "État des sauvegardes des 30 derniers jours, exécutions planifiées des prochaines 24 heures et points d'attention.",
      success_title: "Taux de réussite",
      success_sub: "{ok} terminées · {failed} en échec · {cancelled} annulées en 30 jours",
      success_none: "Aucune sauvegarde terminée ces 30 derniers jours",
      success_chart: "Sauvegardes par jour sur les 30 derniers jours : {ok} terminées, {failed} en échec, {cancelled} annulées.",
      storage_title: "Croissance du stockage",
      storage_sub: "Sauvegardes terminées enregistrées · {delta} ajoutés en 30 jours",
      storage_chart: "Taille stockée à la fin de chaque jour sur les 30 derniers jours, de {from} à {to}.",
      upcoming_title: "Prochaines 24 heures",
      upcoming_sub: "{n} exécutions planifiées",
      upcoming_none: "Aucune exécution planifiée dans les prochaines 24 heures.",
      upcoming_more: "+{n} autres tâches",
      upcoming_truncated: "Les planifications très fréquentes sont tronquées.",
      upcoming_chart: "Exécutions planifiées de {jobs} tâches dans les prochaines 24 heures.",
      attention_title: "Points d'attention",
      attention_none: "Rien ne demande votre attention.",
      att_failed_last: "{job} : la dernière exécution a échoué ({when})",
      att_verify_mismatch: "{db} : somme de contrôle différente dans la sauvegarde du {when}",
      att_verify_error: "{db} : la sauvegarde du {when} n'a pas pu être vérifiée",
      att_verify_more: "{n} autres sauvegardes ont échoué à la vérification",
      att_restore_test: "{job} : le dernier test de restauration a échoué ({when})",
      att_restore_mismatch: "{job} : le dernier test de restauration a trouvé des différences ({when})",
      att_stale: "{job} : aucune sauvegarde réussie depuis {hours} heures",
      att_never: "{job} : aucune sauvegarde réussie pour l'instant",
      att_corrupt: "{n} enregistrements stockés sont illisibles",
      open: "Ouvrir",
      show_data: "Afficher les données",
      col_day: "Jour",
      col_completed: "Terminées",
      col_failed: "En échec",
      col_cancelled: "Annulées",
      col_added: "Ajouté",
      col_stored: "Stocké",
      load_failed: "La vue d'ensemble n'a pas pu être chargée.",
      loading: "Chargement…",
      now: "maintenant",
      spark_label: "{n} dernières exécutions : {ok} terminées, {failed} en échec",
      spark_longest: "la plus longue {d}",
      spark_run: "{when} · {status} · {d}",
    },
  },
  zh: {
    overview: {
      desc: "过去 30 天的备份状况、未来 24 小时的计划运行以及需要关注的事项。",
      success_title: "成功率",
      success_sub: "30 天内 {ok} 个完成 · {failed} 个失败 · {cancelled} 个已取消",
      success_none: "过去 30 天没有已结束的备份",
      success_chart: "过去 30 天每天的备份：{ok} 个完成，{failed} 个失败，{cancelled} 个已取消。",
      storage_title: "存储增长",
      storage_sub: "记录中的已完成备份 · 30 天内新增 {delta}",
      storage_chart: "过去 30 天每天结束时的存储大小，从 {from} 到 {to}。",
      upcoming_title: "未来 24 小时",
      upcoming_sub: "{n} 次计划运行",
      upcoming_none: "未来 24 小时没有计划运行。",
      upcoming_more: "另有 {n} 个任务",
      upcoming_truncated: "过于频繁的计划已被截断。",
      upcoming_chart: "{jobs} 个任务在未来 24 小时内的计划运行。",
      attention_title: "需要关注",
      attention_none: "没有需要关注的事项。",
      att_failed_last: "{job}：最近一次运行失败（{when}）",
      att_verify_mismatch: "{db}：{when} 的备份校验和不匹配",
      att_verify_error: "{db}：{when} 的备份无法验证",
      att_verify_more: "另有 {n} 个备份验证失败",
      att_restore_test: "{job}：最近一次恢复测试失败（{when}）",
      att_restore_mismatch: "{job}：最近一次恢复测试发现差异（{when}）",
      att_stale: "{job}：已有 {hours} 小时没有成功的备份",
      att_never: "{job}：尚无成功的备份",
      att_corrupt: "{n} 条存储记录无法读取",
      open: "打开",
      show_data: "显示数据",
      col_day: "日期",
      col_completed: "完成",
      col_failed: "失败",
      col_cancelled: "已取消",
      col_added: "新增",
      col_stored: "已存储",
      load_failed: "无法加载概览。",
      loading: "正在加载…",
      now: "现在",
      spark_label: "最近 {n} 次运行：{ok} 次完成，{failed} 次失败",
      spark_longest: "最长 {d}",
      spark_run: "{when} · {status} · {d}",
    },
  },
  ja: {
    overview: {
      desc: "過去 30 日間のバックアップの状態、今後 24 時間の予定実行、対応が必要な項目。",
      success_title: "成功率",
      success_sub: "30 日間で完了 {ok} · 失敗 {failed} · キャンセル {cancelled}",
      success_none: "過去 30 日間に終了したバックアップはありません",
      success_chart: "過去 30 日間の 1 日ごとのバックアップ: 完了 {ok}、失敗 {failed}、キャンセル {cancelled}。",
      storage_title: "ストレージの増加",
      storage_sub: "記録上の完了済みバックアップ · 30 日間で {delta} 増加",
      storage_chart: "過去 30 日間の各日の終わりの保存サイズ（{from} から {to}）。",
      upcoming_title: "今後 24 時間",
      upcoming_sub: "予定実行 {n} 件",
      upcoming_none: "今後 24 時間に予定された実行はありません。",
      upcoming_more: "ほか {n} ジョブ",
      upcoming_truncated: "非常に頻繁なスケジュールは省略されています。",
      upcoming_chart: "{jobs} ジョブの今後 24 時間の予定実行。",
      attention_title: "要対応",
      attention_none: "対応が必要な項目はありません。",
      att_failed_last: "{job}: 前回の実行が失敗しました（{when}）",
      att_verify_mismatch: "{db}: {when} のバックアップでチェックサムが一致しません",
      att_verify_error: "{db}: {when} のバックアップを検証できませんでした",
      att_verify_more: "ほかに {n} 件のバックアップが検証に失敗しました",
      att_restore_test: "{job}: 前回のリストアテストが失敗しました（{when}）",
      att_restore_mismatch: "{job}: 前回のリストアテストで差異が見つかりました（{when}）",
      att_stale: "{job}: {hours} 時間成功したバックアップがありません",
      att_never: "{job}: まだ成功したバックアップがありません",
      att_corrupt: "{n} 件の保存レコードを読み取れません",
      open: "開く",
      show_data: "データを表示",
      col_day: "日付",
      col_completed: "完了",
      col_failed: "失敗",
      col_cancelled: "キャンセル",
      col_added: "追加",
      col_stored: "保存済み",
      load_failed: "概要を読み込めませんでした。",
      loading: "読み込み中…",
      now: "現在",
      spark_label: "直近 {n} 回の実行: 完了 {ok}、失敗 {failed}",
      spark_longest: "最長 {d}",
      spark_run: "{when} · {status} · {d}",
    },
  },
  ru: {
    overview: {
      desc: "Состояние резервных копий за 30 дней, запланированные запуски на ближайшие 24 часа и то, что требует внимания.",
      success_title: "Доля успешных",
      success_sub: "За 30 дней: завершено {ok} · с ошибкой {failed} · отменено {cancelled}",
      success_none: "За последние 30 дней нет завершённых копий",
      success_chart: "Резервные копии по дням за 30 дней: завершено {ok}, с ошибкой {failed}, отменено {cancelled}.",
      storage_title: "Рост хранилища",
      storage_sub: "Завершённые копии в учёте · добавлено {delta} за 30 дней",
      storage_chart: "Объём хранения на конец каждого дня за 30 дней: от {from} до {to}.",
      upcoming_title: "Ближайшие 24 часа",
      upcoming_sub: "Запланированных запусков: {n}",
      upcoming_none: "На ближайшие 24 часа запусков не запланировано.",
      upcoming_more: "Ещё заданий: {n}",
      upcoming_truncated: "Очень частые расписания сокращены.",
      upcoming_chart: "Запланированные запуски заданий ({jobs}) на ближайшие 24 часа.",
      attention_title: "Требует внимания",
      attention_none: "Ничего не требует внимания.",
      att_failed_last: "{job}: последний запуск завершился ошибкой ({when})",
      att_verify_mismatch: "{db}: несовпадение контрольной суммы в копии от {when}",
      att_verify_error: "{db}: копию от {when} не удалось проверить",
      att_verify_more: "Ещё копий не прошли проверку: {n}",
      att_restore_test: "{job}: последняя проверка восстановления не удалась ({when})",
      att_restore_mismatch: "{job}: последняя проверка восстановления нашла различия ({when})",
      att_stale: "{job}: нет успешной копии уже {hours} ч",
      att_never: "{job}: успешных копий пока нет",
      att_corrupt: "Не удаётся прочитать сохранённых записей: {n}",
      open: "Открыть",
      show_data: "Показать данные",
      col_day: "День",
      col_completed: "Завершено",
      col_failed: "С ошибкой",
      col_cancelled: "Отменено",
      col_added: "Добавлено",
      col_stored: "Хранится",
      load_failed: "Не удалось загрузить обзор.",
      loading: "Загрузка…",
      now: "сейчас",
      spark_label: "Последние запуски ({n}): завершено {ok}, с ошибкой {failed}",
      spark_longest: "самый долгий {d}",
      spark_run: "{when} · {status} · {d}",
    },
  },
};

if (typeof navMergeI18n === "function") navMergeI18n(OVERVIEW_I18N);

// ---------------------------------------------------------------------------
// State and loading
// ---------------------------------------------------------------------------

const OVERVIEW_DAYS = 30;
// The history is refetched at most this often by the periodic refresh.
const OVERVIEW_TTL_MS = 30000;
const OVERVIEW_TIMELINE_ROWS = 6;
const OVERVIEW_NEXT_LIST = 5;
const SPARK_RUNS = 12;
const HOUR_MS = 3600000;

const overview = { data: null, loadedAt: 0, loading: false, again: false, error: "", attention: [] };

// overviewRefresh fetches GET /api/v1/stats/history when the cached copy is older
// than OVERVIEW_TTL_MS (or force is set), then redraws the overview and the jobs'
// sparklines.
async function overviewRefresh(force) {
  if (!auth.user) return;
  if (overview.loading) {
    if (force) overview.again = true;
    return;
  }
  if (!force && overview.data && Date.now() - overview.loadedAt < OVERVIEW_TTL_MS) {
    overviewRender();
    return;
  }
  overview.loading = true;
  const offset = -new Date().getTimezoneOffset();
  try {
    // Days follow the browser's zone (also across daylight saving changes); the
    // offset is the fallback for zones the server does not know.
    let zone = "";
    try {
      zone = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
    } catch (err) {
      zone = "";
    }
    const tz = zone ? `&tz=${encodeURIComponent(zone)}` : "";
    const json = await apiJSON(`/api/v1/stats/history?days=${OVERVIEW_DAYS}${tz}&tz_offset=${offset}`);
    if (json.success && json.data) {
      overview.data = json.data;
      overview.error = "";
      overview.loadedAt = Date.now();
      if (typeof navSetServerZone === "function") navSetServerZone(json.data.server_time_zone);
    } else {
      overview.error = json.error || t("overview.load_failed");
    }
  } catch (err) {
    overview.error = t("overview.load_failed");
  } finally {
    overview.loading = false;
  }
  overviewRender();
  renderJobs();
  if (overview.again) {
    overview.again = false;
    overviewRefresh(true);
  }
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

// Local Date of a "YYYY-MM-DD" day.
function ovDay(date) {
  const [y, m, d] = String(date).split("-").map(Number);
  return new Date(y, (m || 1) - 1, d || 1);
}

function ovFormat(d, options) {
  try {
    return new Intl.DateTimeFormat(uiLocale(), options).format(d);
  } catch (err) {
    return d.toLocaleString();
  }
}

function ovDayLabel(date) {
  return ovFormat(ovDay(date), { month: "short", day: "numeric" });
}

function ovPercent(ratio) {
  try {
    return new Intl.NumberFormat(uiLocale(), { style: "percent", maximumFractionDigits: 1 }).format(ratio);
  } catch (err) {
    return `${Math.round(ratio * 1000) / 10}%`;
  }
}

function ovWhen(value) {
  const d = parseDate(value);
  if (!d) return "—";
  return typeof timeDisplay === "function" ? timeDisplay(d)[0] : formatRelative(d);
}

// Truncates text to max characters with an ellipsis (SVG text cannot ellipsize).
function ovClip(text, max) {
  const s = String(text || "");
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

function ovJobName(id) {
  const job = state.jobs.find(j => j.id === id);
  return job ? (job.name || job.id) : id;
}

// A rounded-up axis maximum: 1, 2 or 5 times a power of ten.
function ovNiceMax(v) {
  if (!(v > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 5, 10]) {
    if (v <= m * p) return m * p;
  }
  return 10 * p;
}

function ovSvg(width, height, labelId, body) {
  return `<svg class="ov-svg" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img" aria-labelledby="${labelId}" focusable="false">${body}</svg>`;
}

function ovText(x, y, text, cls, anchor) {
  return `<text class="${cls || "ov-axis-label"}" x="${x.toFixed(1)}" y="${y.toFixed(1)}"${anchor ? ` text-anchor="${anchor}"` : ""}>${escapeHtml(text)}</text>`;
}

function ovTable(headers, rows) {
  return `<details class="ov-data"><summary>${escapeHtml(t("overview.show_data"))}</summary>
    <div class="table-wrap ov-table-wrap"><table class="table ov-table">
      <thead><tr>${headers.map(h => `<th scope="col">${escapeHtml(h)}</th>`).join("")}</tr></thead>
      <tbody>${rows.map(r => `<tr>${r.map((c, i) => (i === 0 ? `<th scope="row">${escapeHtml(c)}</th>` : `<td class="num">${escapeHtml(c)}</td>`)).join("")}</tr>`).join("")}</tbody>
    </table></div></details>`;
}

// ---------------------------------------------------------------------------
// Charts
// ---------------------------------------------------------------------------

const OV_W = 560;
const OV_H = 150;
const OV_PAD = { left: 44, right: 6, top: 8, bottom: 20 };

// Daily outcomes as stacked bars (completed, failed, cancelled).
function ovSuccessChart(daily) {
  const plotW = OV_W - OV_PAD.left - OV_PAD.right;
  const plotH = OV_H - OV_PAD.top - OV_PAD.bottom;
  const max = ovNiceMax(Math.max(0, ...daily.map(d => d.completed + d.failed + d.cancelled)));
  const step = plotW / Math.max(daily.length, 1);
  const barW = Math.max(step * 0.68, 1);
  const y = v => OV_PAD.top + plotH - (v / max) * plotH;
  let body = "";
  [0, max / 2, max].forEach(v => {
    body += `<line class="ov-gridline" x1="${OV_PAD.left}" x2="${OV_W - OV_PAD.right}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"/>`;
    body += ovText(OV_PAD.left - 6, y(v) + 4, Number.isInteger(v) ? formatCount(v) : "", "ov-axis-label", "end");
  });
  daily.forEach((d, i) => {
    const x = OV_PAD.left + i * step + (step - barW) / 2;
    const title = `${ovDayLabel(d.date)}: ${t("overview.col_completed")} ${d.completed}, ${t("overview.col_failed")} ${d.failed}, ${t("overview.col_cancelled")} ${d.cancelled}`;
    let base = 0;
    let rects = "";
    [["completed", "ov-ok"], ["failed", "ov-bad"], ["cancelled", "ov-neutral"]].forEach(([k, cls]) => {
      const v = d[k];
      if (!v) return;
      rects += `<rect class="${cls}" x="${x.toFixed(1)}" y="${y(base + v).toFixed(1)}" width="${barW.toFixed(1)}" height="${(y(base) - y(base + v)).toFixed(1)}"/>`;
      base += v;
    });
    if (!rects) rects = `<rect class="ov-empty" x="${x.toFixed(1)}" y="${(y(0) - 1).toFixed(1)}" width="${barW.toFixed(1)}" height="1"/>`;
    body += `<g><title>${escapeHtml(title)}</title>${rects}</g>`;
  });
  body += ovXLabels(daily, step);
  return body;
}

// The first, middle and last day under the plot.
function ovXLabels(daily, step) {
  if (daily.length === 0) return "";
  const at = i => OV_PAD.left + i * step + step / 2;
  const last = daily.length - 1;
  const mid = Math.floor(last / 2);
  return ovText(at(0), OV_H - 5, ovDayLabel(daily[0].date), "ov-axis-label", "start") +
    (last > 1 ? ovText(at(mid), OV_H - 5, ovDayLabel(daily[mid].date), "ov-axis-label", "middle") : "") +
    (last > 0 ? ovText(at(last), OV_H - 5, ovDayLabel(daily[last].date), "ov-axis-label", "end") : "");
}

// Stored size at the end of each day, as an area under a line.
function ovStorageChart(daily) {
  const plotW = OV_W - OV_PAD.left - OV_PAD.right;
  const plotH = OV_H - OV_PAD.top - OV_PAD.bottom;
  const max = ovNiceMax(Math.max(0, ...daily.map(d => d.stored_bytes)));
  const step = plotW / Math.max(daily.length, 1);
  const x = i => OV_PAD.left + i * step + step / 2;
  const y = v => OV_PAD.top + plotH - (v / max) * plotH;
  let body = "";
  [0, max / 2, max].forEach(v => {
    body += `<line class="ov-gridline" x1="${OV_PAD.left}" x2="${OV_W - OV_PAD.right}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"/>`;
    body += ovText(OV_PAD.left - 6, y(v) + 4, v === 0 ? "0" : formatBytes(v), "ov-axis-label", "end");
  });
  if (daily.length > 0) {
    const pts = daily.map((d, i) => `${x(i).toFixed(1)},${y(d.stored_bytes).toFixed(1)}`);
    body += `<path class="ov-area" d="M${x(0).toFixed(1)},${y(0).toFixed(1)} L${pts.join(" L")} L${x(daily.length - 1).toFixed(1)},${y(0).toFixed(1)} Z"/>`;
    body += `<path class="ov-line" d="M${pts.join(" L")}"/>`;
    daily.forEach((d, i) => {
      body += `<rect class="ov-hit" x="${(x(i) - step / 2).toFixed(1)}" y="${OV_PAD.top}" width="${step.toFixed(1)}" height="${plotH}"><title>${escapeHtml(`${ovDayLabel(d.date)}: ${formatBytes(d.stored_bytes)} (+${formatBytes(d.bytes)})`)}</title></rect>`;
    });
    const last = daily[daily.length - 1];
    body += `<circle class="ov-dot" cx="${x(daily.length - 1).toFixed(1)}" cy="${y(last.stored_bytes).toFixed(1)}" r="3.5"/>`;
  }
  body += ovXLabels(daily, step);
  return body;
}

// The next 24 hours: one row per job, a dot per scheduled run, ticks every 3 hours.
function ovTimeline(upcoming, now) {
  const LABEL_W = 150;
  const ROW_H = 22;
  const TOP = 20;
  const end = now + 24 * HOUR_MS;
  const byJob = new Map();
  upcoming.forEach(u => {
    const at = Date.parse(u.at);
    if (!Number.isFinite(at) || at < now || at > end) return;
    if (!byJob.has(u.job_id)) byJob.set(u.job_id, []);
    byJob.get(u.job_id).push(at);
  });
  const jobs = Array.from(byJob.keys()).sort((a, b) => byJob.get(a)[0] - byJob.get(b)[0]);
  const shown = jobs.slice(0, OVERVIEW_TIMELINE_ROWS);
  const height = TOP + Math.max(shown.length, 1) * ROW_H + 4;
  const x = ms => LABEL_W + ((ms - now) / (end - now)) * (OV_W - LABEL_W - 8);
  let body = "";
  // Ticks on the local clock's hours divisible by three.
  const first = new Date(now);
  first.setMinutes(0, 0, 0);
  first.setHours(first.getHours() + 1);
  while (first.getHours() % 3 !== 0) first.setHours(first.getHours() + 1);
  for (let ms = first.getTime(); ms < end; ms += 3 * HOUR_MS) {
    body += `<line class="ov-gridline" x1="${x(ms).toFixed(1)}" x2="${x(ms).toFixed(1)}" y1="${TOP - 4}" y2="${height - 2}"/>`;
    // Labels too close to "now" (or the right edge) would overlap it.
    if (x(ms) - x(now) > 36 && OV_W - x(ms) > 16) body += ovText(x(ms), 12, ovFormat(new Date(ms), { hour: "numeric" }), "ov-axis-label", "middle");
  }
  body += `<line class="ov-now" x1="${x(now).toFixed(1)}" x2="${x(now).toFixed(1)}" y1="${TOP - 4}" y2="${height - 2}"/>`;
  body += ovText(x(now) + 2, 12, t("overview.now"), "ov-axis-label ov-now-label", "start");
  shown.forEach((jobID, row) => {
    const cy = TOP + row * ROW_H + ROW_H / 2;
    const name = ovJobName(jobID);
    body += `<text class="ov-row-label" x="0" y="${(cy + 4).toFixed(1)}"><title>${escapeHtml(name)}</title>${escapeHtml(ovClip(name, 20))}</text>`;
    body += `<line class="ov-track" x1="${LABEL_W}" x2="${OV_W - 8}" y1="${cy}" y2="${cy}"/>`;
    byJob.get(jobID).forEach(at => {
      body += `<circle class="ov-run" cx="${x(at).toFixed(1)}" cy="${cy}" r="4"><title>${escapeHtml(`${name} · ${formatAbsolute(new Date(at))}`)}</title></circle>`;
    });
  });
  return { body, height, jobs: jobs.length, more: jobs.length - shown.length };
}

// ---------------------------------------------------------------------------
// Attention needed
// ---------------------------------------------------------------------------

// The interval between a job's runs: the server's reading of its schedule, else its
// next two runs today, else the gap between its last and next run, else a day.
function ovJobInterval(job, upcoming, hj) {
  if (hj && Number(hj.interval_seconds) > 0) return Number(hj.interval_seconds) * 1000;
  const runs = upcoming.filter(u => u.job_id === job.id).map(u => Date.parse(u.at)).filter(Number.isFinite);
  if (runs.length >= 2) return runs[1] - runs[0];
  const next = parseDate(job.next_run);
  const last = parseDate(job.last_run);
  if (next && last && next > last) return next - last;
  return 24 * HOUR_MS;
}

function overviewAttention() {
  const items = [];
  const h = overview.data || {};
  const stats = state.stats || {};
  const now = Date.now();
  const upcoming = Array.isArray(h.upcoming) ? h.upcoming : [];
  const failedJobs = new Set();
  state.jobs.forEach(job => {
    const last = stats.job_last_backups ? stats.job_last_backups[job.id] : null;
    if (last && last.status === "failed") {
      failedJobs.add(job.id);
      items.push({ sev: "danger", text: tf("overview.att_failed_last", { job: job.name || job.id, when: ovWhen(last.started_at) }),
        open: () => navOpenDetail("backups", last.id) });
    }
  });
  (h.verification_issues || []).forEach(v => {
    const key = v.verification === "mismatch" ? "overview.att_verify_mismatch" : "overview.att_verify_error";
    items.push({ sev: v.verification === "mismatch" ? "danger" : "warn", text: tf(key, { db: v.database, when: ovWhen(v.started_at) }),
      open: () => navOpenDetail("backups", v.id) });
  });
  const more = (Number(h.verification_issues_total) || 0) - (h.verification_issues || []).length;
  if (more > 0) items.push({ sev: "warn", text: tf("overview.att_verify_more", { n: more }) });
  state.jobs.forEach(job => {
    const rt = job.last_restore_test;
    if (!rt || rt.status === "ok") return;
    const key = rt.status === "mismatch" ? "overview.att_restore_mismatch" : "overview.att_restore_test";
    items.push({ sev: "danger", text: tf(key, { job: job.name || job.id, when: ovWhen(rt.at) }), open: () => navOpenDetail("jobs", job.id) });
  });
  if (overview.data) {
    state.jobs.forEach(job => {
      if (job.enabled === false || failedJobs.has(job.id)) return;
      const hj = h.jobs ? h.jobs[job.id] : null;
      // Two missed runs (plus an hour of slack), and never less than six hours.
      const threshold = Math.max(2 * ovJobInterval(job, upcoming, hj) + HOUR_MS, 6 * HOUR_MS);
      const success = hj ? parseDate(hj.last_success_at) : null;
      if (success) {
        if (now - success.getTime() > threshold) {
          items.push({ sev: "warn", text: tf("overview.att_stale", { job: job.name || job.id, hours: Math.floor((now - success.getTime()) / HOUR_MS) }),
            open: () => navOpenDetail("jobs", job.id) });
        }
        return;
      }
      const created = parseDate(job.created_at);
      if (created && now - created.getTime() > threshold) {
        items.push({ sev: "warn", text: tf("overview.att_never", { job: job.name || job.id }), open: () => navOpenDetail("jobs", job.id) });
      }
    });
  }
  const corrupt = Array.isArray(stats.corrupt_records) ? stats.corrupt_records.length : 0;
  if (corrupt > 0) {
    items.push({ sev: "danger", text: tf("overview.att_corrupt", { n: corrupt }),
      open: () => navOpenExternal(`${NAV_REPO}/blob/${encodeURIComponent(navDocsRef(navVersion()))}/docs/troubleshooting.md#unreadable-records`) });
  }
  const rank = { danger: 0, warn: 1 };
  return items.map((it, i) => ({ it, i })).sort((a, b) => rank[a.it.sev] - rank[b.it.sev] || a.i - b.i).map(x => x.it);
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// Markup last written per element: unchanged markup is not rewritten, so an open
// "Show the data" stays open and focus stays put across refreshes.
const ovRendered = new Map();

function ovSet(id, html) {
  const el = document.getElementById(id);
  if (!el || ovRendered.get(el) === html) return;
  ovRendered.set(el, html);
  el.innerHTML = html;
}

function ovSetText(id, text) {
  const el = document.getElementById(id);
  if (el && el.textContent !== text) el.textContent = text;
}

const OV_ICON_ALERT = '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M8 2.5 14 13H2z"/><path d="M8 6.5v3M8 11.2v.1"/></svg>';
const OV_ICON_OK = '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M3.5 8.5 6.5 11.5 12.5 4.5"/></svg>';

// overviewRender draws the Overview tab from the cached history.
function overviewRender() {
  if (!document.getElementById("tab-overview")) return;
  const h = overview.data;
  ovSetText("overview-status", !h && overview.loading ? t("overview.loading") : overview.error);
  const status = document.getElementById("overview-status");
  if (status) status.classList.toggle("text-danger", !!overview.error);
  if (!h) return;
  const daily = Array.isArray(h.daily) ? h.daily : [];

  // Success rate
  const sum = k => daily.reduce((n, d) => n + (Number(d[k]) || 0), 0);
  const ok = sum("completed");
  const failed = sum("failed");
  const cancelled = sum("cancelled");
  ovSetText("ov-success-figure", ok + failed > 0 ? ovPercent(ok / (ok + failed)) : "—");
  const fig = document.getElementById("ov-success-figure");
  if (fig) fig.classList.toggle("text-danger", ok + failed > 0 && failed / (ok + failed) > 0.1);
  ovSetText("ov-success-sub", ok + failed + cancelled > 0 ? tf("overview.success_sub", { ok, failed, cancelled }) : t("overview.success_none"));
  ovSetText("ov-success-desc", tf("overview.success_chart", { ok, failed, cancelled }));
  ovSet("ov-success-chart", ovSvg(OV_W, OV_H, "ov-success-desc", ovSuccessChart(daily)) +
    ovTable([t("overview.col_day"), t("overview.col_completed"), t("overview.col_failed"), t("overview.col_cancelled")],
      daily.map(d => [ovDayLabel(d.date), formatCount(d.completed), formatCount(d.failed), formatCount(d.cancelled)])));

  // Storage growth
  const lastDay = daily[daily.length - 1];
  const stored = lastDay ? lastDay.stored_bytes : 0;
  const firstStored = daily.length ? daily[0].stored_bytes - daily[0].bytes : 0;
  ovSetText("ov-storage-figure", formatBytes(stored));
  ovSetText("ov-storage-sub", tf("overview.storage_sub", { delta: formatBytes(sum("bytes")) }));
  ovSetText("ov-storage-desc", tf("overview.storage_chart", { from: formatBytes(firstStored), to: formatBytes(stored) }));
  ovSet("ov-storage-chart", ovSvg(OV_W, OV_H, "ov-storage-desc", ovStorageChart(daily)) +
    ovTable([t("overview.col_day"), t("overview.col_added"), t("overview.col_stored")],
      daily.map(d => [ovDayLabel(d.date), formatBytes(d.bytes), formatBytes(d.stored_bytes)])));

  // Next 24 hours
  const upcoming = Array.isArray(h.upcoming) ? h.upcoming : [];
  const now = Date.now();
  const future = upcoming.filter(u => Date.parse(u.at) >= now);
  ovSetText("ov-upcoming-figure", formatCount(future.length));
  ovSetText("ov-upcoming-sub", future.length ? tf("overview.upcoming_sub", { n: future.length }) +
    (h.upcoming_truncated ? ` · ${t("overview.upcoming_truncated")}` : "") : t("overview.upcoming_none"));
  if (future.length) {
    const tl = ovTimeline(future, now);
    ovSetText("ov-upcoming-desc", tf("overview.upcoming_chart", { jobs: tl.jobs }));
    const list = future.slice(0, OVERVIEW_NEXT_LIST).map(u => {
      const at = new Date(u.at);
      return `<li><span class="ov-next-time">${escapeHtml(ovFormat(at, { weekday: "short", hour: "2-digit", minute: "2-digit" }))}</span>
        <button type="button" class="link-btn" data-action="ov-open-job" data-id="${escapeHtml(u.job_id)}">${escapeHtml(ovJobName(u.job_id))}</button>
        <span class="muted">${escapeHtml(formatRelative(at))}</span></li>`;
    }).join("");
    ovSet("ov-upcoming-chart", ovSvg(OV_W, tl.height, "ov-upcoming-desc", tl.body) +
      (tl.more > 0 ? `<p class="ov-more">${escapeHtml(tf("overview.upcoming_more", { n: tl.more }))}</p>` : "") +
      `<ol class="ov-next">${list}</ol>`);
  } else {
    ovSet("ov-upcoming-chart", `<p class="ov-empty-text">${escapeHtml(t("overview.upcoming_none"))}</p>`);
  }

  // Attention needed
  overview.attention = overviewAttention();
  ovSetText("ov-attention-figure", overview.attention.length ? formatCount(overview.attention.length) : "");
  const att = document.getElementById("ov-attention-card");
  if (att) att.classList.toggle("ov-card-alert", overview.attention.some(a => a.sev === "danger"));
  ovSet("ov-attention-list", overview.attention.length
    ? `<ul class="ov-attention">${overview.attention.map((a, i) => `<li class="ov-att ov-att-${a.sev}">
        <span class="ov-att-icon">${OV_ICON_ALERT}</span><span class="ov-att-text">${escapeHtml(a.text)}</span>
        ${a.open ? `<button type="button" class="btn btn-secondary btn-sm" data-action="ov-attention" data-index="${i}">${escapeHtml(t("overview.open"))}</button>` : ""}
      </li>`).join("")}</ul>`
    : `<p class="ov-all-good">${OV_ICON_OK}<span>${escapeHtml(t("overview.attention_none"))}</span></p>`);
}

// ---------------------------------------------------------------------------
// Jobs table sparkline
// ---------------------------------------------------------------------------

const SPARK_CLASS = { completed: "ov-ok", failed: "ov-bad", cancelled: "ov-neutral", in_progress: "ov-running", pending: "ov-running" };

// jobSparkline returns the recent outcomes and durations of job jobID as a small
// inline SVG (newest on the right, bar height by duration), or "" without history.
function jobSparkline(jobID) {
  const h = overview.data;
  const hj = h && h.jobs ? h.jobs[jobID] : null;
  const runs = hj && Array.isArray(hj.runs) ? hj.runs.slice(-SPARK_RUNS) : [];
  if (runs.length === 0) return "";
  const W = 60;
  const H = 14;
  const slot = W / SPARK_RUNS;
  const maxDur = Math.max(0, ...runs.map(r => Number(r.duration_seconds) || 0));
  let bars = "";
  runs.forEach((r, i) => {
    const dur = Number(r.duration_seconds) || 0;
    const height = maxDur > 0 && dur > 0 ? 3 + (H - 3) * (dur / maxDur) : r.status === "failed" ? H : 4;
    const x = (SPARK_RUNS - runs.length + i) * slot;
    const [, label] = backupStatus(r.status);
    const d = parseDate(r.started_at);
    const title = tf("overview.spark_run", { when: d ? navAbsoluteShort(d) : "—", status: label, d: dur > 0 ? formatDuration(dur) : "—" });
    bars += `<rect class="${SPARK_CLASS[r.status] || "ov-neutral"}" x="${(x + 0.5).toFixed(1)}" y="${(H - height).toFixed(1)}" width="${(slot - 1.5).toFixed(1)}" height="${height.toFixed(1)}" rx="0.5"><title>${escapeHtml(title)}</title></rect>`;
  });
  const ok = runs.filter(r => r.status === "completed").length;
  const failed = runs.filter(r => r.status === "failed").length;
  let label = tf("overview.spark_label", { n: runs.length, ok, failed });
  if (maxDur > 0) label += ` · ${tf("overview.spark_longest", { d: formatDuration(maxDur) })}`;
  return `<div class="spark" role="img" aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}"><svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" aria-hidden="true" focusable="false">${bars}</svg></div>`;
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

function setupOverview() {
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-action^='ov-']");
    if (!btn || btn.disabled) return;
    if (btn.dataset.action === "ov-open-job") {
      navOpenDetail("jobs", btn.dataset.id || "");
    } else if (btn.dataset.action === "ov-attention") {
      const item = overview.attention[Number(btn.dataset.index)];
      if (item && item.open) item.open();
    }
  });
  if (typeof onLanguageChange === "function") onLanguageChange(overviewRender);
}

document.addEventListener("DOMContentLoaded", setupOverview);
