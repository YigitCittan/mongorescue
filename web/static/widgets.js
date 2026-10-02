/**
 * MongoRescue dashboard: job form widgets.
 *
 * - The cron builder under the job form's schedule field: presets, a custom
 *   editor (repeat, minute, hour, weekdays, day of month) that writes the cron
 *   expression, its human-readable reading (describeCron) and its next three
 *   runs as the scheduler computes them (GET /api/v1/schedule/preview). The raw
 *   expression stays editable; the builder follows what is typed.
 * - Retention presets above the "keep for" / "keep at most" fields. The job store
 *   has no grandfather-father-son rotation, so there is no "7 daily + 4 weekly".
 *
 * Self-contained: the widgets mount into the existing inputs (#job-cron,
 * #job-retention-days, #job-retention-count) and resync whenever the job dialog
 * opens, so the form code in app.js is untouched. Loaded after nav.js; uses
 * app.js's t, tf, apiJSON, escapeHtml, describeCron and uiLocale.
 */

// ---------------------------------------------------------------------------
// Translations
// ---------------------------------------------------------------------------

const WIDGETS_I18N = {
  en: {
    cronb: {
      presets: "Schedule presets",
      hourly: "Every hour",
      nightly: "Every night 02:00",
      sunday: "Every Sunday 03:00",
      weekdays: "Weekdays 22:00",
      custom: "Custom…",
      editor: "Custom schedule",
      repeat: "Repeat",
      f_hourly: "Every hour",
      f_daily: "Every day",
      f_weekly: "Every week",
      f_monthly: "Every month",
      minute: "Minute",
      hour: "Hour",
      days: "Days",
      day_of_month: "Day of month",
      next_runs: "Next runs: {runs}",
      no_runs: "No upcoming runs.",
      invalid: "The scheduler does not accept this expression.",
      checking: "Checking…",
      server_tz: "Hours are the server's ({zone}); next runs in your local time.",
    },
    retp: {
      presets: "Retention presets",
      days7: "7 days",
      days30: "30 days",
      last10: "Last 10 backups",
    },
  },
  tr: {
    cronb: {
      presets: "Zamanlama hazır ayarları",
      hourly: "Her saat",
      nightly: "Her gece 02:00",
      sunday: "Her Pazar 03:00",
      weekdays: "Hafta içi 22:00",
      custom: "Özel…",
      editor: "Özel zamanlama",
      repeat: "Tekrar",
      f_hourly: "Her saat",
      f_daily: "Her gün",
      f_weekly: "Her hafta",
      f_monthly: "Her ay",
      minute: "Dakika",
      hour: "Saat",
      days: "Günler",
      day_of_month: "Ayın günü",
      next_runs: "Sonraki çalıştırmalar: {runs}",
      no_runs: "Yaklaşan çalıştırma yok.",
      invalid: "Zamanlayıcı bu ifadeyi kabul etmiyor.",
      checking: "Denetleniyor…",
      server_tz: "Saatler sunucunundur ({zone}); sonraki çalıştırmalar yerel saatinizle.",
    },
    retp: {
      presets: "Saklama hazır ayarları",
      days7: "7 gün",
      days30: "30 gün",
      last10: "Son 10 yedek",
    },
  },
  de: {
    cronb: {
      presets: "Zeitplan-Vorlagen",
      hourly: "Jede Stunde",
      nightly: "Jede Nacht 02:00",
      sunday: "Jeden Sonntag 03:00",
      weekdays: "Werktags 22:00",
      custom: "Benutzerdefiniert…",
      editor: "Eigener Zeitplan",
      repeat: "Wiederholen",
      f_hourly: "Jede Stunde",
      f_daily: "Jeden Tag",
      f_weekly: "Jede Woche",
      f_monthly: "Jeden Monat",
      minute: "Minute",
      hour: "Stunde",
      days: "Tage",
      day_of_month: "Tag im Monat",
      next_runs: "Nächste Läufe: {runs}",
      no_runs: "Keine anstehenden Läufe.",
      invalid: "Der Scheduler akzeptiert diesen Ausdruck nicht.",
      checking: "Wird geprüft…",
      server_tz: "Stunden in der Zeit des Servers ({zone}); nächste Läufe in Ihrer Ortszeit.",
    },
    retp: {
      presets: "Aufbewahrungs-Vorlagen",
      days7: "7 Tage",
      days30: "30 Tage",
      last10: "Letzte 10 Backups",
    },
  },
  es: {
    cronb: {
      presets: "Programaciones predefinidas",
      hourly: "Cada hora",
      nightly: "Cada noche 02:00",
      sunday: "Cada domingo 03:00",
      weekdays: "Laborables 22:00",
      custom: "Personalizada…",
      editor: "Programación personalizada",
      repeat: "Repetir",
      f_hourly: "Cada hora",
      f_daily: "Cada día",
      f_weekly: "Cada semana",
      f_monthly: "Cada mes",
      minute: "Minuto",
      hour: "Hora",
      days: "Días",
      day_of_month: "Día del mes",
      next_runs: "Próximas ejecuciones: {runs}",
      no_runs: "No hay ejecuciones próximas.",
      invalid: "El planificador no acepta esta expresión.",
      checking: "Comprobando…",
      server_tz: "Las horas son las del servidor ({zone}); las próximas ejecuciones en su hora local.",
    },
    retp: {
      presets: "Retenciones predefinidas",
      days7: "7 días",
      days30: "30 días",
      last10: "Últimas 10 copias",
    },
  },
  fr: {
    cronb: {
      presets: "Planifications prédéfinies",
      hourly: "Toutes les heures",
      nightly: "Chaque nuit 02:00",
      sunday: "Chaque dimanche 03:00",
      weekdays: "En semaine 22:00",
      custom: "Personnalisée…",
      editor: "Planification personnalisée",
      repeat: "Répéter",
      f_hourly: "Toutes les heures",
      f_daily: "Tous les jours",
      f_weekly: "Toutes les semaines",
      f_monthly: "Tous les mois",
      minute: "Minute",
      hour: "Heure",
      days: "Jours",
      day_of_month: "Jour du mois",
      next_runs: "Prochaines exécutions : {runs}",
      no_runs: "Aucune exécution à venir.",
      invalid: "Le planificateur n'accepte pas cette expression.",
      checking: "Vérification…",
      server_tz: "Heures du serveur ({zone}) ; prochaines exécutions à votre heure locale.",
    },
    retp: {
      presets: "Rétentions prédéfinies",
      days7: "7 jours",
      days30: "30 jours",
      last10: "10 dernières sauvegardes",
    },
  },
  zh: {
    cronb: {
      presets: "计划预设",
      hourly: "每小时",
      nightly: "每晚 02:00",
      sunday: "每周日 03:00",
      weekdays: "工作日 22:00",
      custom: "自定义…",
      editor: "自定义计划",
      repeat: "重复",
      f_hourly: "每小时",
      f_daily: "每天",
      f_weekly: "每周",
      f_monthly: "每月",
      minute: "分钟",
      hour: "小时",
      days: "星期",
      day_of_month: "每月日期",
      next_runs: "接下来的运行：{runs}",
      no_runs: "没有即将进行的运行。",
      invalid: "调度器不接受此表达式。",
      checking: "正在检查…",
      server_tz: "小时为服务器时间（{zone}）；接下来的运行以您的本地时间显示。",
    },
    retp: {
      presets: "保留预设",
      days7: "7 天",
      days30: "30 天",
      last10: "最近 10 个备份",
    },
  },
  ja: {
    cronb: {
      presets: "スケジュールのプリセット",
      hourly: "毎時",
      nightly: "毎晩 02:00",
      sunday: "毎週日曜 03:00",
      weekdays: "平日 22:00",
      custom: "カスタム…",
      editor: "カスタムスケジュール",
      repeat: "繰り返し",
      f_hourly: "毎時",
      f_daily: "毎日",
      f_weekly: "毎週",
      f_monthly: "毎月",
      minute: "分",
      hour: "時",
      days: "曜日",
      day_of_month: "日",
      next_runs: "次回の実行: {runs}",
      no_runs: "予定された実行はありません。",
      invalid: "スケジューラはこの式を受け付けません。",
      checking: "確認中…",
      server_tz: "時刻はサーバーのもの（{zone}）、次回の実行はローカル時刻で表示しています。",
    },
    retp: {
      presets: "保持のプリセット",
      days7: "7 日",
      days30: "30 日",
      last10: "直近 10 件",
    },
  },
  ru: {
    cronb: {
      presets: "Готовые расписания",
      hourly: "Каждый час",
      nightly: "Каждую ночь 02:00",
      sunday: "Каждое воскресенье 03:00",
      weekdays: "По будням 22:00",
      custom: "Своё…",
      editor: "Своё расписание",
      repeat: "Повтор",
      f_hourly: "Каждый час",
      f_daily: "Каждый день",
      f_weekly: "Каждую неделю",
      f_monthly: "Каждый месяц",
      minute: "Минута",
      hour: "Час",
      days: "Дни",
      day_of_month: "День месяца",
      next_runs: "Следующие запуски: {runs}",
      no_runs: "Предстоящих запусков нет.",
      invalid: "Планировщик не принимает это выражение.",
      checking: "Проверка…",
      server_tz: "Часы — по времени сервера ({zone}); следующие запуски — по вашему местному времени.",
    },
    retp: {
      presets: "Готовые варианты хранения",
      days7: "7 дней",
      days30: "30 дней",
      last10: "Последние 10 копий",
    },
  },
};

if (typeof navMergeI18n === "function") navMergeI18n(WIDGETS_I18N);

// ---------------------------------------------------------------------------
// Cron builder
// ---------------------------------------------------------------------------

const CRON_PRESETS = [
  { key: "hourly", expr: "0 * * * *" },
  { key: "nightly", expr: "0 2 * * *" },
  { key: "sunday", expr: "0 3 * * 0" },
  { key: "weekdays", expr: "0 22 * * 1-5" }
];
// Descriptors the scheduler accepts, as five fields.
const CRON_DESCRIPTORS = {
  "@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *",
  "@weekly": "0 0 * * 0", "@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *"
};
// Monday first; cron numbers Sunday 0.
const CRON_WEEK = [1, 2, 3, 4, 5, 6, 0];
const CRON_PREVIEW_DEBOUNCE_MS = 300;
const cronb = { timer: null, seq: 0, mounted: false };

function cronNormalize(expr) {
  const s = String(expr || "").trim().replace(/\s+/g, " ");
  return CRON_DESCRIPTORS[s.toLowerCase()] || s;
}

// The editor's reading of expr ({freq, minute, hour, days, dom}), or null when the
// expression is not one the editor can show.
function cronParseSimple(expr) {
  const f = cronNormalize(expr).split(" ");
  if (f.length !== 5 || f[3] !== "*") return null;
  const [min, hour, dom, , dow] = f;
  const num = (v, lo, hi) => (/^\d{1,2}$/.test(v) && Number(v) >= lo && Number(v) <= hi ? Number(v) : null);
  const minute = num(min, 0, 59);
  if (minute === null) return null;
  if (hour === "*" && dom === "*" && dow === "*") return { freq: "hourly", minute, hour: 2, days: [], dom: 1 };
  const h = num(hour, 0, 23);
  if (h === null) return null;
  if (dom === "*" && dow === "*") return { freq: "daily", minute, hour: h, days: [], dom: 1 };
  if (dow === "*") {
    const d = num(dom, 1, 31);
    return d === null ? null : { freq: "monthly", minute, hour: h, days: [], dom: d };
  }
  if (dom !== "*") return null;
  const days = new Set();
  for (const part of dow.split(",")) {
    const range = /^([0-7])-([0-7])$/.exec(part);
    if (range) {
      const a = Number(range[1]);
      const b = Number(range[2]);
      if (a > b) return null;
      for (let d = a; d <= b; d++) days.add(d % 7);
    } else if (/^[0-7]$/.test(part)) {
      days.add(Number(part) % 7);
    } else {
      return null;
    }
  }
  return { freq: "weekly", minute, hour: h, days: Array.from(days), dom: 1 };
}

// Compact day-of-week field: "1-5", "0,6" or "1,3,5".
function cronDaysField(days) {
  const sorted = Array.from(new Set(days)).sort((a, b) => a - b);
  if (sorted.length === 0 || sorted.length === 7) return "*";
  const parts = [];
  let start = sorted[0];
  let prev = sorted[0];
  for (let i = 1; i <= sorted.length; i++) {
    const d = sorted[i];
    if (d === prev + 1) {
      prev = d;
      continue;
    }
    parts.push(prev - start >= 2 ? `${start}-${prev}` : start === prev ? `${start}` : `${start},${prev}`);
    start = d;
    prev = d;
  }
  return parts.join(",");
}

function cronFromEditor(v) {
  const m = Math.min(Math.max(Number(v.minute) || 0, 0), 59);
  const h = Math.min(Math.max(Number(v.hour) || 0, 0), 23);
  switch (v.freq) {
    case "hourly":
      return `${m} * * * *`;
    case "weekly":
      return `${m} ${h} * * ${cronDaysField(v.days)}`;
    case "monthly":
      return `${m} ${h} ${Math.min(Math.max(Number(v.dom) || 1, 1), 31)} * *`;
    default:
      return `${m} ${h} * * *`;
  }
}

function cronWeekdayName(day, style) {
  try {
    // 4 January 2026 is a Sunday.
    return new Intl.DateTimeFormat(uiLocale(), { weekday: style || "short", timeZone: "UTC" }).format(new Date(Date.UTC(2026, 0, 4 + day)));
  } catch (err) {
    return String(day);
  }
}

function cronbEl(id) {
  return document.getElementById(id);
}

function cronbOptions(lo, hi, step) {
  let html = "";
  for (let v = lo; v <= hi; v += step || 1) html += `<option value="${v}">${String(v).padStart(2, "0")}</option>`;
  return html;
}

// Builds the widget below the schedule field (once).
function mountCronBuilder() {
  const input = cronbEl("job-cron");
  if (!input || cronb.mounted) return;
  cronb.mounted = true;
  const box = document.createElement("div");
  box.className = "cronb";
  box.id = "job-cron-builder";
  box.innerHTML = `
    <div class="preset-row" role="group" id="cronb-presets"></div>
    <fieldset class="cronb-editor" id="cronb-editor" hidden>
      <legend class="sr-only" id="cronb-editor-legend"></legend>
      <div class="cronb-fields">
        <label class="cronb-field"><span class="form-label" id="cronb-l-repeat"></span>
          <select id="cronb-freq" class="form-select"></select></label>
        <label class="cronb-field" id="cronb-hour-field"><span class="form-label" id="cronb-l-hour"></span>
          <select id="cronb-hour" class="form-select">${cronbOptions(0, 23)}</select></label>
        <label class="cronb-field"><span class="form-label" id="cronb-l-minute"></span>
          <select id="cronb-minute" class="form-select">${cronbOptions(0, 59)}</select></label>
        <label class="cronb-field" id="cronb-dom-field"><span class="form-label" id="cronb-l-dom"></span>
          <select id="cronb-dom" class="form-select">${cronbOptions(1, 31)}</select></label>
      </div>
      <div class="cronb-days" id="cronb-days-field" role="group" aria-labelledby="cronb-l-days">
        <span class="form-label" id="cronb-l-days"></span>
        <div class="cronb-day-list" id="cronb-days"></div>
      </div>
    </fieldset>
    <p class="form-hint cronb-preview" id="job-cron-preview" aria-live="polite">
      <span class="cronb-reading" id="cronb-reading"></span>
      <span class="cronb-next" id="cronb-next"></span>
      <span class="cronb-tz" id="cronb-tz"></span>
    </p>`;
  input.insertAdjacentElement("afterend", box);
  const described = String(input.getAttribute("aria-describedby") || "").split(/\s+/).filter(Boolean);
  if (!described.includes("job-cron-preview")) described.push("job-cron-preview");
  input.setAttribute("aria-describedby", described.join(" "));
  cronbLabels();

  input.addEventListener("input", () => cronbSync(false));
  box.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-cron-preset]");
    if (!btn) return;
    if (btn.dataset.cronPreset === "custom") {
      const editor = cronbEl("cronb-editor");
      const open = editor.hidden;
      editor.hidden = !open;
      btn.setAttribute("aria-expanded", String(open));
      if (open) {
        cronbFillEditor(cronParseSimple(input.value) || { freq: "daily", minute: 0, hour: 2, days: [], dom: 1 });
        cronbEl("cronb-freq").focus();
      }
      return;
    }
    const preset = CRON_PRESETS.find(p => p.key === btn.dataset.cronPreset);
    if (preset) cronbWrite(preset.expr);
  });
  ["cronb-freq", "cronb-hour", "cronb-minute", "cronb-dom"].forEach(id => {
    cronbEl(id).addEventListener("change", cronbFromEditor);
  });
  cronbEl("cronb-days").addEventListener("change", cronbFromEditor);
}

// Texts of the widget (also after a language change).
function cronbLabels() {
  const presets = cronbEl("cronb-presets");
  if (!presets) return;
  presets.setAttribute("aria-label", t("cronb.presets"));
  presets.innerHTML = CRON_PRESETS.map(p =>
    `<button type="button" class="preset-btn" data-cron-preset="${p.key}" aria-pressed="false">${escapeHtml(t(`cronb.${p.key}`))}</button>`).join("") +
    `<button type="button" class="preset-btn preset-more" data-cron-preset="custom" aria-expanded="${!cronbEl("cronb-editor").hidden}" aria-controls="cronb-editor">${escapeHtml(t("cronb.custom"))}</button>`;
  cronbEl("cronb-editor-legend").textContent = t("cronb.editor");
  cronbEl("cronb-l-repeat").textContent = t("cronb.repeat");
  cronbEl("cronb-l-hour").textContent = t("cronb.hour");
  cronbEl("cronb-l-minute").textContent = t("cronb.minute");
  cronbEl("cronb-l-dom").textContent = t("cronb.day_of_month");
  cronbEl("cronb-l-days").textContent = t("cronb.days");
  const freq = cronbEl("cronb-freq");
  const current = freq.value || "daily";
  freq.innerHTML = ["hourly", "daily", "weekly", "monthly"].map(f => `<option value="${f}">${escapeHtml(t(`cronb.f_${f}`))}</option>`).join("");
  freq.value = current;
  const checked = new Set(Array.from(document.querySelectorAll("#cronb-days input:checked")).map(i => Number(i.value)));
  cronbEl("cronb-days").innerHTML = CRON_WEEK.map(d => `<label class="cronb-day"><input type="checkbox" value="${d}"${checked.has(d) ? " checked" : ""}>
    <span aria-hidden="true">${escapeHtml(cronWeekdayName(d, "short"))}</span><span class="sr-only">${escapeHtml(cronWeekdayName(d, "long"))}</span></label>`).join("");
  cronbSync(true);
}

function cronbFillEditor(v) {
  cronbEl("cronb-freq").value = v.freq;
  cronbEl("cronb-hour").value = String(v.hour);
  cronbEl("cronb-minute").value = String(v.minute);
  cronbEl("cronb-dom").value = String(v.dom);
  document.querySelectorAll("#cronb-days input").forEach(box => {
    box.checked = v.days.includes(Number(box.value));
  });
  cronbEditorFields(v.freq);
}

function cronbEditorFields(freq) {
  cronbEl("cronb-hour-field").hidden = freq === "hourly";
  cronbEl("cronb-dom-field").hidden = freq !== "monthly";
  cronbEl("cronb-days-field").hidden = freq !== "weekly";
}

function cronbFromEditor() {
  const freq = cronbEl("cronb-freq").value;
  const days = Array.from(document.querySelectorAll("#cronb-days input:checked")).map(i => Number(i.value));
  // A new weekly schedule starts on Monday rather than on no day at all.
  if (freq === "weekly" && days.length === 0) {
    const monday = document.querySelector('#cronb-days input[value="1"]');
    if (monday) monday.checked = true;
    days.push(1);
  }
  cronbEditorFields(freq);
  cronbWrite(cronFromEditor({
    freq, days, minute: cronbEl("cronb-minute").value, hour: cronbEl("cronb-hour").value, dom: cronbEl("cronb-dom").value
  }), true);
}

// Writes expr into the schedule field, as if typed.
function cronbWrite(expr, fromEditor) {
  const input = cronbEl("job-cron");
  if (!input) return;
  input.value = expr;
  input.dispatchEvent(new Event("input", { bubbles: true }));
  if (fromEditor) return;
  // A preset also resets an open editor to its values.
  const editor = cronbEl("cronb-editor");
  const simple = cronParseSimple(expr);
  if (editor && !editor.hidden && simple) cronbFillEditor(simple);
}

// Follows the schedule field: pressed preset, editor values, reading, next runs.
function cronbSync(keepEditor) {
  const input = cronbEl("job-cron");
  if (!input || !cronbEl("cronb-presets")) return;
  const expr = input.value.trim();
  const norm = cronNormalize(expr);
  document.querySelectorAll("#cronb-presets [data-cron-preset]").forEach(btn => {
    const preset = CRON_PRESETS.find(p => p.key === btn.dataset.cronPreset);
    if (preset) btn.setAttribute("aria-pressed", String(preset.expr === norm));
  });
  const editor = cronbEl("cronb-editor");
  const simple = cronParseSimple(expr);
  if (!keepEditor && editor && !editor.hidden && simple && document.activeElement && !editor.contains(document.activeElement)) {
    cronbFillEditor(simple);
  }
  const reading = typeof describeCron === "function" ? describeCron(expr) : "";
  cronbEl("cronb-reading").textContent = reading;
  cronbPreview(expr);
}

function cronbRunText(value) {
  const d = parseDate(value);
  if (!d) return "";
  try {
    return new Intl.DateTimeFormat(uiLocale(), { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }).format(d);
  } catch (err) {
    return formatAbsolute(d);
  }
}

// Asks the server for the next three runs of expr (debounced; stale answers dropped).
function cronbPreview(expr) {
  clearTimeout(cronb.timer);
  const next = cronbEl("cronb-next");
  const tz = cronbEl("cronb-tz");
  const seq = ++cronb.seq;
  if (!expr || !auth.user) {
    next.textContent = "";
    tz.textContent = "";
    return;
  }
  next.textContent = t("cronb.checking");
  next.classList.remove("text-danger");
  cronb.timer = setTimeout(async () => {
    try {
      const json = await apiJSON(`/api/v1/schedule/preview?cron=${encodeURIComponent(expr.slice(0, 256))}&n=3`);
      if (seq !== cronb.seq) return;
      const p = json.success ? json.data : null;
      if (!p) {
        next.textContent = "";
        return;
      }
      // The job form's RPO hint shows the default of this schedule (readiness.js).
      if (typeof readinessRpoHint === "function") readinessRpoHint(p.valid ? p.default_rpo_minutes : 0);
      if (!p.valid) {
        next.textContent = t("cronb.invalid");
        next.classList.add("text-danger");
        tz.textContent = "";
        return;
      }
      const runs = (p.next_runs || []).map(cronbRunText).filter(Boolean);
      next.textContent = runs.length ? tf("cronb.next_runs", { runs: runs.join(" · ") }) : t("cronb.no_runs");
      const zone = p.server_time_zone;
      if (zone && typeof navSetServerZone === "function") navSetServerZone(zone);
      tz.textContent = zone && zone.offset_minutes !== -new Date().getTimezoneOffset() && typeof navZoneLabel === "function"
        ? tf("cronb.server_tz", { zone: navZoneLabel(zone.name, zone.offset_minutes) }) : "";
    } catch (err) {
      if (seq === cronb.seq) next.textContent = "";
    }
  }, CRON_PREVIEW_DEBOUNCE_MS);
}

// ---------------------------------------------------------------------------
// Retention presets
// ---------------------------------------------------------------------------

// No GFS rotation exists (only days and count), so no "7 daily + 4 weekly".
const RETENTION_PRESETS = [
  { key: "days7", days: 7, count: 0 },
  { key: "days30", days: 30, count: 0 },
  { key: "last10", days: 0, count: 10 }
];
let retentionMounted = false;

function mountRetentionPresets() {
  const days = cronbEl("job-retention-days");
  const count = cronbEl("job-retention-count");
  if (!days || !count || retentionMounted) return;
  const row = days.closest(".form-row");
  if (!row) return;
  retentionMounted = true;
  const group = document.createElement("div");
  group.className = "preset-row retention-presets";
  group.id = "retention-presets";
  group.setAttribute("role", "group");
  row.insertAdjacentElement("beforebegin", group);
  retentionLabels();
  group.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-retention-preset]");
    const preset = btn ? RETENTION_PRESETS.find(p => p.key === btn.dataset.retentionPreset) : null;
    if (!preset) return;
    days.value = String(preset.days);
    count.value = String(preset.count);
    // The retention preview (trust.js) listens for input.
    days.dispatchEvent(new Event("input", { bubbles: true }));
    count.dispatchEvent(new Event("input", { bubbles: true }));
    retentionSync();
  });
  [days, count].forEach(el => el.addEventListener("input", retentionSync));
}

function retentionLabels() {
  const group = cronbEl("retention-presets");
  if (!group) return;
  group.setAttribute("aria-label", t("retp.presets"));
  group.innerHTML = RETENTION_PRESETS.map(p =>
    `<button type="button" class="preset-btn" data-retention-preset="${p.key}" aria-pressed="false">${escapeHtml(t(`retp.${p.key}`))}</button>`).join("");
  retentionSync();
}

function retentionSync() {
  const days = parseInt(getValue("job-retention-days"), 10) || 0;
  const count = parseInt(getValue("job-retention-count"), 10) || 0;
  document.querySelectorAll("#retention-presets [data-retention-preset]").forEach(btn => {
    const p = RETENTION_PRESETS.find(x => x.key === btn.dataset.retentionPreset);
    btn.setAttribute("aria-pressed", String(!!p && p.days === days && p.count === count));
  });
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

function setupWidgets() {
  mountCronBuilder();
  mountRetentionPresets();
  // The job dialog fills its fields, then opens: resync the widgets on open.
  const modal = cronbEl("modal-new-job");
  if (modal) {
    let wasOpen = false;
    new MutationObserver(() => {
      const open = modal.classList.contains("open");
      if (open && !wasOpen) {
        const editor = cronbEl("cronb-editor");
        if (editor) editor.hidden = true;
        const more = document.querySelector('#cronb-presets [data-cron-preset="custom"]');
        if (more) more.setAttribute("aria-expanded", "false");
        cronbSync(true);
        retentionSync();
      }
      wasOpen = open;
    }).observe(modal, { attributes: true, attributeFilter: ["class"] });
  }
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      cronbLabels();
      retentionLabels();
    });
  }
}

document.addEventListener("DOMContentLoaded", setupWidgets);
