/**
 * MongoRescue dashboard: Settings → Monitoring and the job heartbeat field.
 *
 * Settings → Monitoring edits the "monitoring" group of PUT /api/v1/settings: the
 * global heartbeat URL (an external dead-man's switch such as healthchecks.io,
 * pinged every interval while the scheduler is healthy) and its interval, with a
 * "Send test ping" button (POST /api/v1/settings/monitoring/test, admin). The job
 * form gets an optional heartbeat URL of its own (heartbeat_url): pinged at
 * <url>/start when a run starts, <url> on success and <url>/fail on failure (nothing more for a cancelled run).
 *
 * Heartbeat URLs are secrets: the server returns them masked (origin + "******")
 * and keeps the stored URL when the masked value is sent back. Loaded after
 * app.js and trust.js (mergeTranslations). Server values reach the DOM only
 * through textContent and form values.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const MONITORING_TRANSLATIONS = {
  en: {
    monitoring: {
      nav: "Monitoring",
      title: "Monitoring",
      desc: "An external dead-man's switch alerts you when MongoRescue itself stops: a crash, a host that is down, a hung scheduler or a full disk. MongoRescue pings the URL while it is healthy; the external service alerts when the pings stop.",
      group: "Heartbeat",
      url: "Heartbeat URL",
      url_hint: "A healthchecks.io-compatible ping URL, such as https://hc-ping.com/<uuid>. Leave empty to turn the heartbeat off. The URL is stored encrypted and shown only up to its host.",
      interval: "Ping every (minutes)",
      interval_hint: "1 to 60 minutes. Give the external check this period and a grace time of a few minutes.",
      test: "Send test ping",
      testing: "Sending a test ping…",
      test_ok: "The test ping reached {host}.",
      test_failed: "Test ping failed: {error}",
      url_invalid: "Enter an http or https URL.",
      interval_invalid: "The interval must be between 1 and 60 minutes.",
      alerts_hint: "Prometheus alert rules for these signals ship in deploy/prometheus/alerts.yml; see the monitoring guide.",
      job_url: "Heartbeat URL",
      job_url_hint: "Optional. Pinged at <url>/start when a run starts, <url> when it succeeds and <url>/fail when it fails or is partial; a cancelled run sends nothing more. Stored encrypted."
    }
  },
  tr: {
    monitoring: {
      nav: "İzleme",
      title: "İzleme",
      desc: "Harici bir ölü adam anahtarı, MongoRescue'nun kendisi durduğunda sizi uyarır: çökme, kapalı sunucu, takılan zamanlayıcı veya dolu disk. MongoRescue sağlıklıyken URL'ye ping gönderir; ping'ler kesildiğinde harici servis alarm verir.",
      group: "Heartbeat",
      url: "Heartbeat URL'si",
      url_hint: "healthchecks.io uyumlu bir ping URL'si, örneğin https://hc-ping.com/<uuid>. Heartbeat'i kapatmak için boş bırakın. URL şifreli saklanır ve yalnızca sunucu adına kadar gösterilir.",
      interval: "Ping aralığı (dakika)",
      interval_hint: "1 ile 60 dakika arası. Harici kontrole bu periyodu ve birkaç dakikalık tolerans süresi verin.",
      test: "Test ping'i gönder",
      testing: "Test ping'i gönderiliyor…",
      test_ok: "Test ping'i {host} adresine ulaştı.",
      test_failed: "Test ping'i başarısız: {error}",
      url_invalid: "Bir http veya https URL'si girin.",
      interval_invalid: "Aralık 1 ile 60 dakika arasında olmalıdır.",
      alerts_hint: "Bu sinyaller için Prometheus alarm kuralları deploy/prometheus/alerts.yml dosyasında gelir; izleme kılavuzuna bakın.",
      job_url: "Heartbeat URL'si",
      job_url_hint: "İsteğe bağlı. Çalışma başladığında <url>/start, başarılı olduğunda <url>, başarısız veya kısmi olduğunda <url>/fail adresine ping gönderilir; iptal edilen çalışma başka ping göndermez. Şifreli saklanır."
    }
  },
  de: {
    monitoring: {
      nav: "Überwachung",
      title: "Überwachung",
      desc: "Ein externer Totmannschalter warnt Sie, wenn MongoRescue selbst ausfällt: ein Absturz, ein ausgefallener Host, ein hängender Scheduler oder eine volle Festplatte. MongoRescue pingt die URL, solange es gesund ist; der externe Dienst alarmiert, wenn die Pings ausbleiben.",
      group: "Heartbeat",
      url: "Heartbeat-URL",
      url_hint: "Eine mit healthchecks.io kompatible Ping-URL, etwa https://hc-ping.com/<uuid>. Leer lassen, um den Heartbeat auszuschalten. Die URL wird verschlüsselt gespeichert und nur bis zum Host angezeigt.",
      interval: "Ping alle (Minuten)",
      interval_hint: "1 bis 60 Minuten. Geben Sie der externen Prüfung diese Periode und eine Karenzzeit von einigen Minuten.",
      test: "Test-Ping senden",
      testing: "Test-Ping wird gesendet…",
      test_ok: "Der Test-Ping hat {host} erreicht.",
      test_failed: "Test-Ping fehlgeschlagen: {error}",
      url_invalid: "Geben Sie eine http- oder https-URL ein.",
      interval_invalid: "Das Intervall muss zwischen 1 und 60 Minuten liegen.",
      alerts_hint: "Prometheus-Alarmregeln für diese Signale liegen in deploy/prometheus/alerts.yml; siehe die Überwachungsanleitung.",
      job_url: "Heartbeat-URL",
      job_url_hint: "Optional. Gepingt wird <url>/start beim Start eines Laufs, <url> bei Erfolg und <url>/fail bei Fehlschlag oder Teilerfolg; ein abgebrochener Lauf sendet nichts mehr. Wird verschlüsselt gespeichert."
    }
  },
  es: {
    monitoring: {
      nav: "Monitorización",
      title: "Monitorización",
      desc: "Un interruptor de hombre muerto externo te avisa cuando MongoRescue deja de funcionar: un fallo, un host caído, un planificador bloqueado o un disco lleno. MongoRescue envía pings a la URL mientras está sano; el servicio externo avisa cuando dejan de llegar.",
      group: "Heartbeat",
      url: "URL de heartbeat",
      url_hint: "Una URL de ping compatible con healthchecks.io, como https://hc-ping.com/<uuid>. Déjala vacía para desactivar el heartbeat. La URL se guarda cifrada y solo se muestra hasta el host.",
      interval: "Ping cada (minutos)",
      interval_hint: "De 1 a 60 minutos. Da a la comprobación externa este periodo y un margen de unos minutos.",
      test: "Enviar ping de prueba",
      testing: "Enviando un ping de prueba…",
      test_ok: "El ping de prueba llegó a {host}.",
      test_failed: "El ping de prueba falló: {error}",
      url_invalid: "Introduce una URL http o https.",
      interval_invalid: "El intervalo debe estar entre 1 y 60 minutos.",
      alerts_hint: "Las reglas de alerta de Prometheus para estas señales se incluyen en deploy/prometheus/alerts.yml; consulta la guía de monitorización.",
      job_url: "URL de heartbeat",
      job_url_hint: "Opcional. Se envía un ping a <url>/start cuando empieza una ejecución, a <url> cuando termina bien y a <url>/fail cuando falla o es parcial; una ejecución cancelada no envía nada más. Se guarda cifrada."
    }
  },
  fr: {
    monitoring: {
      nav: "Surveillance",
      title: "Surveillance",
      desc: "Un dispositif d'homme mort externe vous alerte quand MongoRescue lui-même s'arrête : un plantage, un hôte hors service, un planificateur bloqué ou un disque plein. MongoRescue envoie un ping à l'URL tant qu'il est en bonne santé ; le service externe alerte quand les pings cessent.",
      group: "Heartbeat",
      url: "URL de heartbeat",
      url_hint: "Une URL de ping compatible healthchecks.io, par exemple https://hc-ping.com/<uuid>. Laissez vide pour désactiver le heartbeat. L'URL est stockée chiffrée et affichée seulement jusqu'à l'hôte.",
      interval: "Ping toutes les (minutes)",
      interval_hint: "De 1 à 60 minutes. Donnez à la vérification externe cette période et un délai de grâce de quelques minutes.",
      test: "Envoyer un ping de test",
      testing: "Envoi d'un ping de test…",
      test_ok: "Le ping de test a atteint {host}.",
      test_failed: "Échec du ping de test : {error}",
      url_invalid: "Saisissez une URL http ou https.",
      interval_invalid: "L'intervalle doit être compris entre 1 et 60 minutes.",
      alerts_hint: "Les règles d'alerte Prometheus pour ces signaux sont fournies dans deploy/prometheus/alerts.yml ; voir le guide de surveillance.",
      job_url: "URL de heartbeat",
      job_url_hint: "Facultatif. Ping de <url>/start au début d'une exécution, de <url> en cas de succès et de <url>/fail en cas d'échec ou de résultat partiel ; une exécution annulée n'envoie plus rien. Stockée chiffrée."
    }
  },
  zh: {
    monitoring: {
      nav: "监控",
      title: "监控",
      desc: "外部失效开关会在 MongoRescue 自身停止时提醒您：崩溃、主机宕机、调度器卡住或磁盘已满。MongoRescue 在健康时定期 ping 该 URL；ping 停止后，外部服务会发出告警。",
      group: "心跳",
      url: "心跳 URL",
      url_hint: "兼容 healthchecks.io 的 ping URL，例如 https://hc-ping.com/<uuid>。留空即关闭心跳。URL 加密存储，仅显示到主机名。",
      interval: "ping 间隔（分钟）",
      interval_hint: "1 到 60 分钟。为外部检查设置相同的周期，并留出几分钟的宽限时间。",
      test: "发送测试 ping",
      testing: "正在发送测试 ping…",
      test_ok: "测试 ping 已到达 {host}。",
      test_failed: "测试 ping 失败：{error}",
      url_invalid: "请输入 http 或 https URL。",
      interval_invalid: "间隔必须在 1 到 60 分钟之间。",
      alerts_hint: "这些信号的 Prometheus 告警规则位于 deploy/prometheus/alerts.yml；请参阅监控指南。",
      job_url: "心跳 URL",
      job_url_hint: "可选。运行开始时 ping <url>/start，成功时 ping <url>，失败或部分成功时 ping <url>/fail；已取消的运行不再发送 ping。加密存储。"
    }
  },
  ja: {
    monitoring: {
      nav: "監視",
      title: "監視",
      desc: "外部のデッドマンスイッチは、MongoRescue 自体が停止したとき（クラッシュ、ホストのダウン、スケジューラーの停止、ディスクフル）に通知します。MongoRescue は正常な間 URL に ping を送り、ping が途絶えると外部サービスがアラートを出します。",
      group: "ハートビート",
      url: "ハートビート URL",
      url_hint: "healthchecks.io 互換の ping URL（例: https://hc-ping.com/<uuid>）。空にするとハートビートは無効になります。URL は暗号化して保存され、ホスト名までしか表示されません。",
      interval: "ping 間隔（分）",
      interval_hint: "1〜60 分。外部チェックにはこの周期と数分の猶予時間を設定してください。",
      test: "テスト ping を送信",
      testing: "テスト ping を送信しています…",
      test_ok: "テスト ping は {host} に届きました。",
      test_failed: "テスト ping に失敗しました: {error}",
      url_invalid: "http または https の URL を入力してください。",
      interval_invalid: "間隔は 1〜60 分にしてください。",
      alerts_hint: "これらのシグナル用の Prometheus アラートルールは deploy/prometheus/alerts.yml に含まれています。監視ガイドを参照してください。",
      job_url: "ハートビート URL",
      job_url_hint: "任意。実行開始時に <url>/start、成功時に <url>、失敗・一部失敗時に <url>/fail へ ping します。キャンセルされた実行はそれ以上送信しません。暗号化して保存されます。"
    }
  },
  ru: {
    monitoring: {
      nav: "Мониторинг",
      title: "Мониторинг",
      desc: "Внешний «выключатель мертвеца» предупредит вас, когда остановится сам MongoRescue: сбой, недоступный хост, зависший планировщик или переполненный диск. Пока MongoRescue исправен, он отправляет пинги на URL; внешний сервис поднимает тревогу, когда пинги прекращаются.",
      group: "Heartbeat",
      url: "URL heartbeat",
      url_hint: "URL пинга, совместимый с healthchecks.io, например https://hc-ping.com/<uuid>. Оставьте пустым, чтобы выключить heartbeat. URL хранится в зашифрованном виде и показывается только до имени хоста.",
      interval: "Пинг каждые (минут)",
      interval_hint: "От 1 до 60 минут. Задайте внешней проверке этот период и запас в несколько минут.",
      test: "Отправить тестовый пинг",
      testing: "Отправка тестового пинга…",
      test_ok: "Тестовый пинг дошёл до {host}.",
      test_failed: "Тестовый пинг не удался: {error}",
      url_invalid: "Введите URL http или https.",
      interval_invalid: "Интервал должен быть от 1 до 60 минут.",
      alerts_hint: "Правила оповещений Prometheus для этих сигналов поставляются в deploy/prometheus/alerts.yml; см. руководство по мониторингу.",
      job_url: "URL heartbeat",
      job_url_hint: "Необязательно. Пинг <url>/start при запуске, <url> при успехе и <url>/fail при сбое или частичном результате; отменённый запуск больше ничего не отправляет. Хранится в зашифрованном виде."
    }
  }
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(MONITORING_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], MONITORING_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Settings → Monitoring
// ---------------------------------------------------------------------------

const MONITORING_MIN_INTERVAL = 1;
const MONITORING_MAX_INTERVAL = 60;
const MONITORING_DEFAULT_INTERVAL = 5;
const MONITORING_URL_RE = /^https?:\/\/[^\s/]+/i;

const monitoring = { dirty: false };

// monitoringIntervalMinutes reads a Go duration ("5m0s") as whole minutes.
function monitoringIntervalMinutes(value) {
  const ms = typeof parseGoDuration === "function" ? parseGoDuration(value) : NaN;
  if (!Number.isFinite(ms) || ms <= 0) return MONITORING_DEFAULT_INTERVAL;
  return Math.max(MONITORING_MIN_INTERVAL, Math.round(ms / 60000));
}

// monitoringFillSettings fills the form from the loaded settings (app.js calls it
// with the other settings forms) unless it has unsaved edits.
function monitoringFillSettings(force) {
  const form = document.getElementById("form-monitoring");
  if (!form) return;
  const submit = form.querySelector("[type=submit]");
  if (submit) submit.disabled = !state.loaded.settings;
  if (!state.loaded.settings || (monitoring.dirty && !force)) return;
  monitoring.dirty = false;
  const m = settingsGroup("monitoring");
  setValue("monitoring-heartbeat-url", m.heartbeat_url || "");
  setValue("monitoring-heartbeat-interval", monitoringIntervalMinutes(m.heartbeat_interval));
  hideFormError("monitoring-error");
}

// monitoringReadForm validates the form and returns the "monitoring" patch, or
// null after showing the problem.
function monitoringReadForm() {
  const url = getValue("monitoring-heartbeat-url");
  if (url && !MONITORING_URL_RE.test(url)) {
    showFormError("monitoring-error", t("monitoring.url_invalid"));
    return null;
  }
  const minutes = parseInt(getValue("monitoring-heartbeat-interval"), 10);
  if (isNaN(minutes) || minutes < MONITORING_MIN_INTERVAL || minutes > MONITORING_MAX_INTERVAL) {
    showFormError("monitoring-error", t("monitoring.interval_invalid"));
    return null;
  }
  return { heartbeat_url: url, heartbeat_interval: `${minutes}m` };
}

async function monitoringSaveSettings(e) {
  e.preventDefault();
  const patch = monitoringReadForm();
  if (!patch) return;
  const submit = e.submitter || document.querySelector("#form-monitoring [type=submit]");
  if (submit) submit.disabled = true;
  try {
    const json = await apiJSON("/api/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ monitoring: patch })
    });
    if (!json.success) {
      showFormError("monitoring-error", json.error || t("toasts.request_failed"));
      return;
    }
    hideFormError("monitoring-error");
    // Only this form is refilled, so unsaved edits in the other forms stay.
    if (json.data && json.data.monitoring) {
      state.settings = { ...(state.settings || {}), monitoring: json.data.monitoring, warnings: json.data.warnings || [] };
    }
    monitoring.dirty = false;
    monitoringFillSettings(true);
    setText("monitoring-save-status", t("settings.saved"));
    setTimeout(() => setText("monitoring-save-status", ""), 3000);
  } catch (err) {
    showFormError("monitoring-error", err.message);
  } finally {
    if (submit) submit.disabled = false;
  }
}

// monitoringTest pings the URL in the form (its masked value pings the stored URL).
async function monitoringTest(btn) {
  const out = document.getElementById("monitoring-test-result");
  const url = getValue("monitoring-heartbeat-url");
  if (url && !MONITORING_URL_RE.test(url)) {
    showFormError("monitoring-error", t("monitoring.url_invalid"));
    return;
  }
  hideFormError("monitoring-error");
  if (btn) btn.disabled = true;
  if (out) {
    out.textContent = t("monitoring.testing");
    out.classList.remove("text-danger");
  }
  try {
    const json = await apiJSON("/api/v1/settings/monitoring/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ heartbeat_url: url })
    });
    if (!out) return;
    if (json.success && json.data) {
      out.textContent = tf("monitoring.test_ok", { host: String(json.data.host || "") });
    } else {
      out.textContent = tf("monitoring.test_failed", { error: json.error || t("toasts.request_failed") });
      out.classList.add("text-danger");
    }
  } catch (err) {
    if (out) {
      out.textContent = tf("monitoring.test_failed", { error: err.message });
      out.classList.add("text-danger");
    }
  } finally {
    if (btn) btn.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// Job form
// ---------------------------------------------------------------------------

// monitoringFillJobForm sets the job's heartbeat URL (masked for a stored job).
function monitoringFillJobForm(job) {
  setValue("job-heartbeat-url", job && job.heartbeat_url ? job.heartbeat_url : "");
}

// monitoringJobPayload returns the job form's heartbeat_url ("" for none; the
// masked value keeps the stored URL).
function monitoringJobPayload() {
  if (!document.getElementById("job-heartbeat-url")) return {};
  return { heartbeat_url: getValue("job-heartbeat-url") };
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function monitoringSetup() {
  if (typeof ROLE_ACTION_SCOPES === "object") {
    Object.assign(ROLE_ACTION_SCOPES, { "monitoring-test": "admin" });
  }
  document.addEventListener("click", (e) => {
    const btn = e.target instanceof Element ? e.target.closest("[data-action='monitoring-test']") : null;
    if (!btn || btn.disabled) return;
    monitoringTest(btn);
  });
  const form = document.getElementById("form-monitoring");
  if (form) {
    form.addEventListener("submit", monitoringSaveSettings);
    form.addEventListener("input", () => { monitoring.dirty = true; });
    form.addEventListener("change", () => { monitoring.dirty = true; });
  }
}

document.addEventListener("DOMContentLoaded", monitoringSetup);
