/**
 * MongoRescue dashboard: the REST API rate limit.
 *
 * The security settings api_rate_limit_per_minute, api_rate_limit_burst,
 * api_work_rate_limit_per_minute and api_work_rate_limit_burst (Settings →
 * Security → API access), and the strings of the back-off after a 429 (app.js
 * apiFetch waits for the server's Retry-After and sends the request again once).
 *
 * Loaded after app.js and trust.js (mergeTranslations); app.js calls
 * rateLimitFillSecurity() and rateLimitCollectSecurity().
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const RATELIMIT_TRANSLATIONS = {
  en: {
    ratelimit: {
      toast: "Too many requests: the server asked the dashboard to wait {seconds} s. It continues on its own.",
      per_minute: "Requests per minute (per API key or session)",
      per_minute_hint: "REST API requests each API key and each session may make, sustained. 0 turns the limit off. Over it, requests get 429 with Retry-After.",
      burst: "Burst",
      burst_hint: "Requests allowed at once before the per-minute rate applies.",
      work_per_minute: "Work requests per minute",
      work_per_minute_hint: "A tighter budget for requests that start work: backups, restores, job runs, verifications, scans, bulk actions and tests. 0 leaves them to the general limit.",
      work_burst: "Work burst",
      work_burst_hint: "Work requests allowed at once.",
      err_per_minute: "Enter a number of requests per minute from 0 (off) to 100000.",
      err_burst: "Enter a burst from 1 to 100000.",
    },
  },
  tr: {
    ratelimit: {
      toast: "Çok fazla istek: sunucu panelin {seconds} sn beklemesini istedi. Kendiliğinden devam eder.",
      per_minute: "Dakikadaki istek (API anahtarı veya oturum başına)",
      per_minute_hint: "Her API anahtarının ve her oturumun sürekli yapabileceği REST API isteği. 0 sınırı kapatır. Aşan istekler Retry-After ile 429 alır.",
      burst: "Ani yük",
      burst_hint: "Dakikalık oran devreye girmeden önce bir anda izin verilen istek.",
      work_per_minute: "Dakikadaki iş isteği",
      work_per_minute_hint: "İş başlatan istekler için daha sıkı bir bütçe: yedeklemeler, geri yüklemeler, iş çalıştırmaları, doğrulamalar, taramalar, toplu işlemler ve testler. 0 bunları genel sınıra bırakır.",
      work_burst: "İş ani yükü",
      work_burst_hint: "Bir anda izin verilen iş isteği.",
      err_per_minute: "Dakikadaki istek sayısını 0 (kapalı) ile 100000 arasında girin.",
      err_burst: "Ani yükü 1 ile 100000 arasında girin.",
    },
  },
  de: {
    ratelimit: {
      toast: "Zu viele Anfragen: Der Server hat das Dashboard gebeten, {seconds} s zu warten. Es macht von selbst weiter.",
      per_minute: "Anfragen pro Minute (pro API-Schlüssel oder Sitzung)",
      per_minute_hint: "REST-API-Anfragen, die jeder API-Schlüssel und jede Sitzung dauerhaft stellen darf. 0 schaltet das Limit aus. Darüber erhalten Anfragen 429 mit Retry-After.",
      burst: "Burst",
      burst_hint: "Anfragen, die auf einmal erlaubt sind, bevor die Rate pro Minute greift.",
      work_per_minute: "Arbeitsanfragen pro Minute",
      work_per_minute_hint: "Ein engeres Budget für Anfragen, die Arbeit starten: Backups, Wiederherstellungen, Jobläufe, Prüfungen, Scans, Massenaktionen und Tests. 0 überlässt sie dem allgemeinen Limit.",
      work_burst: "Arbeits-Burst",
      work_burst_hint: "Arbeitsanfragen, die auf einmal erlaubt sind.",
      err_per_minute: "Geben Sie Anfragen pro Minute von 0 (aus) bis 100000 ein.",
      err_burst: "Geben Sie einen Burst von 1 bis 100000 ein.",
    },
  },
  es: {
    ratelimit: {
      toast: "Demasiadas solicitudes: el servidor pidió al panel esperar {seconds} s. Continúa por sí solo.",
      per_minute: "Solicitudes por minuto (por clave de API o sesión)",
      per_minute_hint: "Solicitudes a la API REST que cada clave de API y cada sesión pueden hacer de forma sostenida. 0 desactiva el límite. Por encima, las solicitudes reciben 429 con Retry-After.",
      burst: "Ráfaga",
      burst_hint: "Solicitudes permitidas de golpe antes de que se aplique la tasa por minuto.",
      work_per_minute: "Solicitudes de trabajo por minuto",
      work_per_minute_hint: "Un presupuesto más estricto para las solicitudes que inician trabajo: copias de seguridad, restauraciones, ejecuciones de trabajos, verificaciones, escaneos, acciones masivas y pruebas. 0 las deja al límite general.",
      work_burst: "Ráfaga de trabajo",
      work_burst_hint: "Solicitudes de trabajo permitidas de golpe.",
      err_per_minute: "Introduzca solicitudes por minuto de 0 (desactivado) a 100000.",
      err_burst: "Introduzca una ráfaga de 1 a 100000.",
    },
  },
  fr: {
    ratelimit: {
      toast: "Trop de requêtes : le serveur a demandé au tableau de bord d'attendre {seconds} s. Il reprend de lui-même.",
      per_minute: "Requêtes par minute (par clé d'API ou session)",
      per_minute_hint: "Requêtes à l'API REST que chaque clé d'API et chaque session peut faire en continu. 0 désactive la limite. Au-delà, les requêtes reçoivent 429 avec Retry-After.",
      burst: "Rafale",
      burst_hint: "Requêtes permises d'un coup avant que le débit par minute s'applique.",
      work_per_minute: "Requêtes de travail par minute",
      work_per_minute_hint: "Un budget plus strict pour les requêtes qui lancent du travail : sauvegardes, restaurations, exécutions de tâches, vérifications, analyses, actions groupées et tests. 0 les laisse à la limite générale.",
      work_burst: "Rafale de travail",
      work_burst_hint: "Requêtes de travail permises d'un coup.",
      err_per_minute: "Saisissez un nombre de requêtes par minute de 0 (désactivé) à 100000.",
      err_burst: "Saisissez une rafale de 1 à 100000.",
    },
  },
  zh: {
    ratelimit: {
      toast: "请求过多：服务器要求控制台等待 {seconds} 秒，之后会自动继续。",
      per_minute: "每分钟请求数（每个 API 密钥或会话）",
      per_minute_hint: "每个 API 密钥和每个会话可持续发出的 REST API 请求数。0 表示关闭限制。超出后请求会收到带 Retry-After 的 429。",
      burst: "突发",
      burst_hint: "在每分钟速率生效前一次允许的请求数。",
      work_per_minute: "每分钟工作请求数",
      work_per_minute_hint: "为启动工作的请求设置更严格的配额：备份、恢复、作业运行、校验、扫描、批量操作和测试。0 表示只受通用限制约束。",
      work_burst: "工作突发",
      work_burst_hint: "一次允许的工作请求数。",
      err_per_minute: "请输入每分钟请求数，0（关闭）到 100000。",
      err_burst: "请输入突发数，1 到 100000。",
    },
  },
  ja: {
    ratelimit: {
      toast: "リクエストが多すぎます：サーバーがダッシュボードに {seconds} 秒待つよう求めました。自動的に再開します。",
      per_minute: "1 分あたりのリクエスト数（API キーまたはセッションごと）",
      per_minute_hint: "各 API キーと各セッションが継続して送れる REST API リクエスト数。0 で制限を無効にします。超えたリクエストには Retry-After 付きの 429 が返ります。",
      burst: "バースト",
      burst_hint: "1 分あたりのレートが適用される前に一度に許可されるリクエスト数。",
      work_per_minute: "1 分あたりの作業リクエスト数",
      work_per_minute_hint: "作業を開始するリクエスト（バックアップ、リストア、ジョブ実行、検証、スキャン、一括操作、テスト）用のより厳しい予算。0 にすると全体の制限のみが適用されます。",
      work_burst: "作業バースト",
      work_burst_hint: "一度に許可される作業リクエスト数。",
      err_per_minute: "1 分あたりのリクエスト数を 0（無効）〜100000 で入力してください。",
      err_burst: "バーストを 1〜100000 で入力してください。",
    },
  },
  ru: {
    ratelimit: {
      toast: "Слишком много запросов: сервер попросил панель подождать {seconds} с. Она продолжит сама.",
      per_minute: "Запросов в минуту (на API-ключ или сеанс)",
      per_minute_hint: "Запросы к REST API, которые каждый API-ключ и каждый сеанс может делать постоянно. 0 отключает ограничение. Сверх него запросы получают 429 с Retry-After.",
      burst: "Всплеск",
      burst_hint: "Запросов, разрешённых сразу, до того как действует ограничение в минуту.",
      work_per_minute: "Рабочих запросов в минуту",
      work_per_minute_hint: "Более строгий бюджет для запросов, запускающих работу: резервные копии, восстановления, запуски заданий, проверки, сканирования, массовые действия и тесты. 0 оставляет их общему ограничению.",
      work_burst: "Рабочий всплеск",
      work_burst_hint: "Рабочих запросов, разрешённых сразу.",
      err_per_minute: "Введите число запросов в минуту от 0 (выключено) до 100000.",
      err_burst: "Введите всплеск от 1 до 100000.",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(RATELIMIT_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], RATELIMIT_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Settings → Security
// ---------------------------------------------------------------------------

const RATELIMIT_MAX = 100000;
// The defaults of a fresh installation (settings.Defaults).
const RATELIMIT_DEFAULTS = {
  api_rate_limit_per_minute: 1200,
  api_rate_limit_burst: 300,
  api_work_rate_limit_per_minute: 60,
  api_work_rate_limit_burst: 20,
};
// The fields of the rate limit settings: [element ID, setting, minimum].
const RATELIMIT_FIELDS = [
  ["set-api-rate-limit", "api_rate_limit_per_minute", 0],
  ["set-api-rate-burst", "api_rate_limit_burst", 1],
  ["set-api-work-rate-limit", "api_work_rate_limit_per_minute", 0],
  ["set-api-work-rate-burst", "api_work_rate_limit_burst", 1],
];

// rateLimitFillSecurity fills the rate limit fields (app.js fillSecurity).
function rateLimitFillSecurity(sec) {
  RATELIMIT_FIELDS.forEach(([id, key]) => {
    const n = Number(sec[key]);
    setValue(id, String(Number.isInteger(n) && n >= 0 ? n : RATELIMIT_DEFAULTS[key]));
  });
}

// rateLimitCollectSecurity validates the rate limit fields (app.js
// collectSecurity); a FieldError marks the field.
function rateLimitCollectSecurity() {
  const out = {};
  RATELIMIT_FIELDS.forEach(([id, key, min]) => {
    const el = document.getElementById(id);
    if (!el) return;
    const raw = String(el.value || "").trim();
    const n = Number(raw);
    if (!/^\d+$/.test(raw) || n < min || n > RATELIMIT_MAX) {
      throw new FieldError(id, t(min === 0 ? "ratelimit.err_per_minute" : "ratelimit.err_burst"));
    }
    out[key] = n;
  });
  return out;
}
