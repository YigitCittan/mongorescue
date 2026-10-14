/**
 * MongoRescue dashboard: API key expiry.
 *
 * The expiry column of Settings → API keys, the expiry choice of the "Create API
 * key" dialog (Never, 30/90/180/365 days or a date; "Never" is not offered and
 * the choices stop at the maximum while security.api_key_max_lifetime_days is
 * set), the security settings api_key_max_lifetime_days and
 * api_key_expiry_warning_days, the lines of the api_keys_expiring and
 * api_key_never_expires warnings (runtimewarnings.js) and the labels of the
 * api_key.expiring and api_key.deadline_set events.
 *
 * Loaded after app.js and trust.js (mergeTranslations); app.js calls
 * apiKeyExpiryCell(), apiKeyExpiryOpenModal(), apiKeyExpiryPayload(),
 * apiKeyExpiryFillSecurity() and apiKeyExpiryCollectSecurity(); runtimewarnings.js
 * calls apiKeyWarningLine(). Server values reach the DOM only through textContent
 * or escapeHtml().
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const APIKEY_TRANSLATIONS = {
  en: {
    apikeys: {
      col_expires: "Expires",
      expiry_label: "Expires",
      expiry_never: "Never",
      expiry_days: "In {days} days",
      expiry_date: "On a date…",
      expiry_date_label: "Expiry date",
      expiry_hint: "The key stops working at this time. Create a replacement before then; you are warned in advance.",
      expiry_hint_max: "At most {days} days: the maximum API key lifetime (Settings → Security). You are warned before the key expires.",
      err_date: "Choose a date in the future, at most {days} days from now.",
      expired: "Expired",
      expires_soon: "Expires soon",
      max_lifetime: "Maximum API key lifetime (days)",
      max_lifetime_hint: "0 is unlimited. When set, new keys need an expiry within it, and existing keys without one (or with a later one) expire this many days from now; this is announced as api_key.deadline_set.",
      warning_days: "Expiry warning (days)",
      warning_days_hint: "Warn this many days before a key expires (1–365): a banner, the api_key.expiring event and a metric.",
      err_max_lifetime: "Enter the maximum lifetime in days, from 0 (unlimited) to 3650.",
      err_warning_days: "Enter the expiry warning in days, from 1 to 365.",
      line_expires: "{name} ({prefix}) expires {time}",
      line_expired: "{name} ({prefix}) expired {time}",
      line_never: "{name} ({prefix}) never expires",
    },
    warnings: {
      api_keys_expiring_title: "API keys expire soon",
      api_keys_expiring_desc: "These API keys expire soon or have expired. Create replacements, update the clients that use them and revoke the old keys (Settings → API keys).",
      api_key_never_expires_title: "An imported API key never expires",
      api_key_never_expires_desc: "The key imported from MONGORESCUE_API_KEY has no expiry although a maximum API key lifetime is set. Replace it with a key created in Settings → API keys and revoke it.",
    },
    notify: { events: { api_key_expiring: "API key expires soon", api_key_deadline_set: "API keys given an expiry" } },
  },
  tr: {
    apikeys: {
      col_expires: "Bitiş",
      expiry_label: "Geçerlilik bitişi",
      expiry_never: "Hiçbir zaman",
      expiry_days: "{days} gün sonra",
      expiry_date: "Bir tarihte…",
      expiry_date_label: "Bitiş tarihi",
      expiry_hint: "Anahtar bu zamanda çalışmayı bırakır. Öncesinde yerine yenisini oluşturun; önceden uyarılırsınız.",
      expiry_hint_max: "En fazla {days} gün: en uzun API anahtarı ömrü (Ayarlar → Güvenlik). Anahtarın süresi dolmadan uyarılırsınız.",
      err_date: "Gelecekte, en fazla {days} gün sonrasına ait bir tarih seçin.",
      expired: "Süresi doldu",
      expires_soon: "Yakında doluyor",
      max_lifetime: "En uzun API anahtarı ömrü (gün)",
      max_lifetime_hint: "0 sınırsızdır. Ayarlandığında yeni anahtarlar bu süre içinde bir bitiş tarihi ister; bitişi olmayan (ya da daha geç olan) mevcut anahtarlar şu andan itibaren bu kadar gün sonra sona erer; bu, api_key.deadline_set olarak duyurulur.",
      warning_days: "Bitiş uyarısı (gün)",
      warning_days_hint: "Bir anahtarın süresi dolmadan bu kadar gün önce uyar (1–365): bir başlık, api_key.expiring olayı ve bir metrik.",
      err_max_lifetime: "En uzun ömrü gün olarak girin, 0 (sınırsız) ile 3650 arasında.",
      err_warning_days: "Bitiş uyarısını gün olarak girin, 1 ile 365 arasında.",
      line_expires: "{name} ({prefix}) {time} tarihinde sona eriyor",
      line_expired: "{name} ({prefix}) {time} tarihinde sona erdi",
      line_never: "{name} ({prefix}) hiç sona ermiyor",
    },
    warnings: {
      api_keys_expiring_title: "API anahtarlarının süresi yakında doluyor",
      api_keys_expiring_desc: "Bu API anahtarlarının süresi yakında doluyor ya da doldu. Yerlerine yenilerini oluşturun, onları kullanan istemcileri güncelleyin ve eski anahtarları iptal edin (Ayarlar → API anahtarları).",
      api_key_never_expires_title: "İçe aktarılan bir API anahtarının süresi hiç dolmuyor",
      api_key_never_expires_desc: "MONGORESCUE_API_KEY'den içe aktarılan anahtarın, en uzun API anahtarı ömrü ayarlı olmasına rağmen bitiş tarihi yok. Onu Ayarlar → API anahtarları'nda oluşturulan bir anahtarla değiştirin ve iptal edin.",
    },
    notify: { events: { api_key_expiring: "API anahtarının süresi yakında doluyor", api_key_deadline_set: "API anahtarlarına bitiş tarihi verildi" } },
  },
  de: {
    apikeys: {
      col_expires: "Läuft ab",
      expiry_label: "Läuft ab",
      expiry_never: "Nie",
      expiry_days: "In {days} Tagen",
      expiry_date: "An einem Datum…",
      expiry_date_label: "Ablaufdatum",
      expiry_hint: "Der Schlüssel funktioniert ab diesem Zeitpunkt nicht mehr. Erstellen Sie vorher einen Ersatz; Sie werden rechtzeitig gewarnt.",
      expiry_hint_max: "Höchstens {days} Tage: die maximale Lebensdauer von API-Schlüsseln (Einstellungen → Sicherheit). Sie werden vor dem Ablauf gewarnt.",
      err_date: "Wählen Sie ein Datum in der Zukunft, höchstens {days} Tage ab heute.",
      expired: "Abgelaufen",
      expires_soon: "Läuft bald ab",
      max_lifetime: "Maximale Lebensdauer von API-Schlüsseln (Tage)",
      max_lifetime_hint: "0 ist unbegrenzt. Wenn gesetzt, brauchen neue Schlüssel ein Ablaufdatum innerhalb dieser Frist, und bestehende Schlüssel ohne (oder mit späterem) Ablaufdatum laufen so viele Tage ab jetzt ab; das wird als api_key.deadline_set angekündigt.",
      warning_days: "Ablaufwarnung (Tage)",
      warning_days_hint: "So viele Tage vor dem Ablauf eines Schlüssels warnen (1–365): ein Banner, das Ereignis api_key.expiring und eine Metrik.",
      err_max_lifetime: "Geben Sie die maximale Lebensdauer in Tagen ein, von 0 (unbegrenzt) bis 3650.",
      err_warning_days: "Geben Sie die Ablaufwarnung in Tagen ein, von 1 bis 365.",
      line_expires: "{name} ({prefix}) läuft ab am {time}",
      line_expired: "{name} ({prefix}) ist abgelaufen am {time}",
      line_never: "{name} ({prefix}) läuft nie ab",
    },
    warnings: {
      api_keys_expiring_title: "API-Schlüssel laufen bald ab",
      api_keys_expiring_desc: "Diese API-Schlüssel laufen bald ab oder sind abgelaufen. Erstellen Sie Ersatzschlüssel, aktualisieren Sie die Clients, die sie verwenden, und widerrufen Sie die alten Schlüssel (Einstellungen → API-Schlüssel).",
      api_key_never_expires_title: "Ein importierter API-Schlüssel läuft nie ab",
      api_key_never_expires_desc: "Der aus MONGORESCUE_API_KEY importierte Schlüssel hat kein Ablaufdatum, obwohl eine maximale Lebensdauer gesetzt ist. Ersetzen Sie ihn durch einen in Einstellungen → API-Schlüssel erstellten Schlüssel und widerrufen Sie ihn.",
    },
    notify: { events: { api_key_expiring: "API-Schlüssel läuft bald ab", api_key_deadline_set: "API-Schlüssel erhielten ein Ablaufdatum" } },
  },
  es: {
    apikeys: {
      col_expires: "Caduca",
      expiry_label: "Caduca",
      expiry_never: "Nunca",
      expiry_days: "En {days} días",
      expiry_date: "En una fecha…",
      expiry_date_label: "Fecha de caducidad",
      expiry_hint: "La clave deja de funcionar en ese momento. Cree una sustituta antes; se le avisará con antelación.",
      expiry_hint_max: "Como máximo {days} días: la vida máxima de las claves de API (Configuración → Seguridad). Se le avisará antes de que caduque.",
      err_date: "Elija una fecha futura, como máximo dentro de {days} días.",
      expired: "Caducada",
      expires_soon: "Caduca pronto",
      max_lifetime: "Vida máxima de las claves de API (días)",
      max_lifetime_hint: "0 es ilimitado. Si se establece, las claves nuevas necesitan una caducidad dentro de ese plazo, y las existentes sin caducidad (o con una posterior) caducan dentro de esos días a partir de ahora; se anuncia como api_key.deadline_set.",
      warning_days: "Aviso de caducidad (días)",
      warning_days_hint: "Avisar estos días antes de que caduque una clave (1–365): un aviso, el evento api_key.expiring y una métrica.",
      err_max_lifetime: "Introduzca la vida máxima en días, de 0 (ilimitado) a 3650.",
      err_warning_days: "Introduzca el aviso de caducidad en días, de 1 a 365.",
      line_expires: "{name} ({prefix}) caduca el {time}",
      line_expired: "{name} ({prefix}) caducó el {time}",
      line_never: "{name} ({prefix}) nunca caduca",
    },
    warnings: {
      api_keys_expiring_title: "Claves de API que caducan pronto",
      api_keys_expiring_desc: "Estas claves de API caducan pronto o ya caducaron. Cree sustitutas, actualice los clientes que las usan y revoque las claves antiguas (Configuración → Claves de API).",
      api_key_never_expires_title: "Una clave de API importada nunca caduca",
      api_key_never_expires_desc: "La clave importada de MONGORESCUE_API_KEY no tiene caducidad aunque hay una vida máxima establecida. Sustitúyala por una clave creada en Configuración → Claves de API y revóquela.",
    },
    notify: { events: { api_key_expiring: "Clave de API caduca pronto", api_key_deadline_set: "Claves de API con caducidad asignada" } },
  },
  fr: {
    apikeys: {
      col_expires: "Expire",
      expiry_label: "Expire",
      expiry_never: "Jamais",
      expiry_days: "Dans {days} jours",
      expiry_date: "À une date…",
      expiry_date_label: "Date d'expiration",
      expiry_hint: "La clé cesse de fonctionner à ce moment. Créez une remplaçante avant ; vous êtes prévenu à l'avance.",
      expiry_hint_max: "Au plus {days} jours : la durée de vie maximale des clés d'API (Paramètres → Sécurité). Vous êtes prévenu avant l'expiration.",
      err_date: "Choisissez une date future, au plus dans {days} jours.",
      expired: "Expirée",
      expires_soon: "Expire bientôt",
      max_lifetime: "Durée de vie maximale des clés d'API (jours)",
      max_lifetime_hint: "0 est illimité. Une fois définie, les nouvelles clés doivent expirer dans ce délai, et les clés existantes sans expiration (ou avec une plus tardive) expirent dans ce nombre de jours à partir de maintenant ; c'est annoncé par api_key.deadline_set.",
      warning_days: "Alerte d'expiration (jours)",
      warning_days_hint: "Prévenir ce nombre de jours avant l'expiration d'une clé (1–365) : une bannière, l'événement api_key.expiring et une métrique.",
      err_max_lifetime: "Saisissez la durée de vie maximale en jours, de 0 (illimitée) à 3650.",
      err_warning_days: "Saisissez l'alerte d'expiration en jours, de 1 à 365.",
      line_expires: "{name} ({prefix}) expire le {time}",
      line_expired: "{name} ({prefix}) a expiré le {time}",
      line_never: "{name} ({prefix}) n'expire jamais",
    },
    warnings: {
      api_keys_expiring_title: "Des clés d'API expirent bientôt",
      api_keys_expiring_desc: "Ces clés d'API expirent bientôt ou ont expiré. Créez des remplaçantes, mettez à jour les clients qui les utilisent et révoquez les anciennes clés (Paramètres → Clés d'API).",
      api_key_never_expires_title: "Une clé d'API importée n'expire jamais",
      api_key_never_expires_desc: "La clé importée depuis MONGORESCUE_API_KEY n'a pas d'expiration alors qu'une durée de vie maximale est définie. Remplacez-la par une clé créée dans Paramètres → Clés d'API et révoquez-la.",
    },
    notify: { events: { api_key_expiring: "Clé d'API expirant bientôt", api_key_deadline_set: "Expiration donnée à des clés d'API" } },
  },
  zh: {
    apikeys: {
      col_expires: "过期时间",
      expiry_label: "过期时间",
      expiry_never: "永不",
      expiry_days: "{days} 天后",
      expiry_date: "指定日期…",
      expiry_date_label: "过期日期",
      expiry_hint: "密钥在此时间停止工作。请在此之前创建替代密钥；系统会提前提醒你。",
      expiry_hint_max: "最多 {days} 天：API 密钥的最长有效期（设置 → 安全）。密钥过期前你会收到提醒。",
      err_date: "请选择一个将来的日期，最多为 {days} 天后。",
      expired: "已过期",
      expires_soon: "即将过期",
      max_lifetime: "API 密钥最长有效期（天）",
      max_lifetime_hint: "0 表示不限。设置后，新密钥必须在此期限内过期；没有过期时间（或过期时间更晚）的现有密钥将从现在起在这么多天后过期，并以 api_key.deadline_set 事件通知。",
      warning_days: "过期提醒（天）",
      warning_days_hint: "在密钥过期前这么多天发出提醒（1–365）：横幅、api_key.expiring 事件和指标。",
      err_max_lifetime: "请输入以天为单位的最长有效期，0（不限）到 3650。",
      err_warning_days: "请输入以天为单位的过期提醒，1 到 365。",
      line_expires: "{name}（{prefix}）将于 {time} 过期",
      line_expired: "{name}（{prefix}）已于 {time} 过期",
      line_never: "{name}（{prefix}）永不过期",
    },
    warnings: {
      api_keys_expiring_title: "API 密钥即将过期",
      api_keys_expiring_desc: "这些 API 密钥即将过期或已过期。请创建替代密钥，更新使用它们的客户端，并撤销旧密钥（设置 → API 密钥）。",
      api_key_never_expires_title: "导入的 API 密钥永不过期",
      api_key_never_expires_desc: "虽然已设置 API 密钥最长有效期，但从 MONGORESCUE_API_KEY 导入的密钥没有过期时间。请用在“设置 → API 密钥”中创建的密钥替换它，并将其撤销。",
    },
    notify: { events: { api_key_expiring: "API 密钥即将过期", api_key_deadline_set: "已为 API 密钥设置过期时间" } },
  },
  ja: {
    apikeys: {
      col_expires: "有効期限",
      expiry_label: "有効期限",
      expiry_never: "なし",
      expiry_days: "{days} 日後",
      expiry_date: "日付を指定…",
      expiry_date_label: "有効期限の日付",
      expiry_hint: "この時点でキーは使えなくなります。それまでに代わりのキーを作成してください。事前に警告されます。",
      expiry_hint_max: "最大 {days} 日：API キーの最長有効期間（設定 → セキュリティ）。キーの期限切れ前に警告されます。",
      err_date: "今日から最大 {days} 日後までの将来の日付を選んでください。",
      expired: "期限切れ",
      expires_soon: "まもなく期限切れ",
      max_lifetime: "API キーの最長有効期間（日）",
      max_lifetime_hint: "0 は無制限です。設定すると、新しいキーにはこの期間内の有効期限が必要になり、有効期限のない（またはより遅い）既存のキーは今からこの日数後に期限切れになります。これは api_key.deadline_set として通知されます。",
      warning_days: "期限切れの警告（日）",
      warning_days_hint: "キーの期限切れのこの日数前に警告します（1〜365）：バナー、api_key.expiring イベント、メトリクス。",
      err_max_lifetime: "最長有効期間を日数で入力してください（0（無制限）〜3650）。",
      err_warning_days: "期限切れの警告を日数で入力してください（1〜365）。",
      line_expires: "{name}（{prefix}）は {time} に期限切れになります",
      line_expired: "{name}（{prefix}）は {time} に期限切れになりました",
      line_never: "{name}（{prefix}）は期限切れになりません",
    },
    warnings: {
      api_keys_expiring_title: "API キーがまもなく期限切れになります",
      api_keys_expiring_desc: "これらの API キーはまもなく期限切れになるか、すでに期限切れです。代わりのキーを作成し、それを使うクライアントを更新して、古いキーを取り消してください（設定 → API キー）。",
      api_key_never_expires_title: "インポートされた API キーが期限切れになりません",
      api_key_never_expires_desc: "API キーの最長有効期間が設定されていますが、MONGORESCUE_API_KEY からインポートされたキーには有効期限がありません。設定 → API キーで作成したキーに置き換え、取り消してください。",
    },
    notify: { events: { api_key_expiring: "API キーがまもなく期限切れ", api_key_deadline_set: "API キーに有効期限を設定" } },
  },
  ru: {
    apikeys: {
      col_expires: "Истекает",
      expiry_label: "Срок действия",
      expiry_never: "Бессрочно",
      expiry_days: "Через {days} дн.",
      expiry_date: "В указанную дату…",
      expiry_date_label: "Дата истечения",
      expiry_hint: "В это время ключ перестанет работать. Заранее создайте замену; вы получите предупреждение.",
      expiry_hint_max: "Не более {days} дн.: максимальный срок действия API-ключей (Настройки → Безопасность). Вы получите предупреждение до истечения ключа.",
      err_date: "Выберите дату в будущем, не позднее чем через {days} дн.",
      expired: "Истёк",
      expires_soon: "Скоро истекает",
      max_lifetime: "Максимальный срок действия API-ключей (дни)",
      max_lifetime_hint: "0 — без ограничения. Если задан, новым ключам нужен срок действия в его пределах, а существующие ключи без срока (или с более поздним) истекут через столько дней с этого момента; об этом сообщает событие api_key.deadline_set.",
      warning_days: "Предупреждение об истечении (дни)",
      warning_days_hint: "Предупреждать за столько дней до истечения ключа (1–365): баннер, событие api_key.expiring и метрика.",
      err_max_lifetime: "Введите максимальный срок в днях, от 0 (без ограничения) до 3650.",
      err_warning_days: "Введите срок предупреждения в днях, от 1 до 365.",
      line_expires: "{name} ({prefix}) истекает {time}",
      line_expired: "{name} ({prefix}) истёк {time}",
      line_never: "{name} ({prefix}) бессрочный",
    },
    warnings: {
      api_keys_expiring_title: "Скоро истекают API-ключи",
      api_keys_expiring_desc: "Эти API-ключи скоро истекут или уже истекли. Создайте замену, обновите клиентов, которые их используют, и отзовите старые ключи (Настройки → API-ключи).",
      api_key_never_expires_title: "Импортированный API-ключ бессрочный",
      api_key_never_expires_desc: "У ключа, импортированного из MONGORESCUE_API_KEY, нет срока действия, хотя задан максимальный срок. Замените его ключом, созданным в Настройки → API-ключи, и отзовите.",
    },
    notify: { events: { api_key_expiring: "API-ключ скоро истекает", api_key_deadline_set: "API-ключам назначен срок действия" } },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(APIKEY_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], APIKEY_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Policy (from GET /api/v1/settings)
// ---------------------------------------------------------------------------

const APIKEY_DAY_MS = 24 * 60 * 60 * 1000;
// The longest expiry the server accepts without a maximum lifetime (auth.MaxAPIKeyLifetime).
const APIKEY_ABSOLUTE_MAX_DAYS = 3650;
const APIKEY_DEFAULT_WARNING_DAYS = 14;

function apiKeySecurity() {
  return (typeof state === "object" && state.settings && state.settings.security) || {};
}

// apiKeyMaxLifetimeDays returns security.api_key_max_lifetime_days (0: unlimited).
function apiKeyMaxLifetimeDays() {
  const n = Number(apiKeySecurity().api_key_max_lifetime_days);
  return Number.isInteger(n) && n > 0 ? n : 0;
}

// apiKeyWarningDays returns security.api_key_expiry_warning_days.
function apiKeyWarningDays() {
  const n = Number(apiKeySecurity().api_key_expiry_warning_days);
  return Number.isInteger(n) && n > 0 ? n : APIKEY_DEFAULT_WARNING_DAYS;
}

// apiKeyPrefixText is the public display form of a key's prefix.
function apiKeyPrefixText(prefix) {
  const p = String(prefix || "");
  return `${p.startsWith("mr_") ? p : `mr_${p}`}_…`;
}

// ---------------------------------------------------------------------------
// Keys list and warnings
// ---------------------------------------------------------------------------

// apiKeyExpiryCell renders the expiry of key k: never, or the time with a chip
// once it is within the warning window or past.
function apiKeyExpiryCell(k) {
  const d = parseDate(k && k.expires_at);
  if (!d) return `<span class="muted">${escapeHtml(t("apikeys.expiry_never"))}</span>`;
  const left = d.getTime() - Date.now();
  let chip = "";
  if (left <= 0) chip = ` <span class="chip chip-warn">${escapeHtml(t("apikeys.expired"))}</span>`;
  else if (left <= apiKeyWarningDays() * APIKEY_DAY_MS) chip = ` <span class="chip chip-warn">${escapeHtml(t("apikeys.expires_soon"))}</span>`;
  return timeCell(k.expires_at) + chip;
}

// apiKeyWarningLine describes a key of a warning as plain text.
function apiKeyWarningLine(k) {
  const vars = { name: String((k && k.name) || ""), prefix: apiKeyPrefixText(k && k.prefix) };
  const d = parseDate(k && k.expires_at);
  if (!d) return tf("apikeys.line_never", vars);
  vars.time = typeof formatAbsolute === "function" ? formatAbsolute(d) : d.toISOString();
  return tf(d.getTime() <= Date.now() ? "apikeys.line_expired" : "apikeys.line_expires", vars);
}

// ---------------------------------------------------------------------------
// The "Create API key" dialog
// ---------------------------------------------------------------------------

// isoDay returns the local calendar day of d as YYYY-MM-DD.
function apiKeyIsoDay(d) {
  const pad = n => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

// apiKeyExpiryOpenModal offers the expiries the policy allows and preselects one:
// never without a maximum lifetime, else the longest preset within it.
function apiKeyExpiryOpenModal() {
  const select = document.getElementById("api-key-expiry");
  if (!select) return;
  const max = apiKeyMaxLifetimeDays();
  const presets = [];
  select.querySelectorAll("option").forEach(opt => {
    let allowed = true;
    if (opt.value === "never") {
      allowed = max === 0;
    } else if (/^\d+$/.test(opt.value)) {
      const days = Number(opt.value);
      opt.textContent = tf("apikeys.expiry_days", { days });
      allowed = max === 0 || days <= max;
      if (allowed) presets.push(opt.value);
    }
    opt.hidden = !allowed;
    opt.disabled = !allowed;
  });
  select.value = max === 0 ? "never" : (presets.length ? presets[presets.length - 1] : "date");
  const date = document.getElementById("api-key-expiry-date");
  if (date) {
    const now = new Date();
    date.min = apiKeyIsoDay(new Date(now.getTime() + APIKEY_DAY_MS));
    date.max = apiKeyIsoDay(new Date(now.getTime() + (max || APIKEY_ABSOLUTE_MAX_DAYS) * APIKEY_DAY_MS));
    date.value = apiKeyIsoDay(new Date(now.getTime() + Math.min(max || 90, 90) * APIKEY_DAY_MS));
  }
  setText("api-key-expiry-hint", max > 0 ? tf("apikeys.expiry_hint_max", { days: max }) : t("apikeys.expiry_hint"));
  apiKeyExpiryToggleDate();
}

// apiKeyExpiryToggleDate shows the date field for "On a date…".
function apiKeyExpiryToggleDate() {
  const date = document.getElementById("api-key-expiry-date");
  if (date) date.hidden = getValue("api-key-expiry") !== "date";
}

document.addEventListener("change", e => {
  if (e.target && e.target.id === "api-key-expiry") apiKeyExpiryToggleDate();
});

// apiKeyExpiryPayload returns the expiry fields of POST /api/v1/api-keys ({} for
// never), or null after reporting an invalid date.
function apiKeyExpiryPayload() {
  const choice = getValue("api-key-expiry");
  if (!choice || choice === "never") return {};
  if (/^\d+$/.test(choice)) return { expires_in_days: Number(choice) };
  const max = apiKeyMaxLifetimeDays() || APIKEY_ABSOLUTE_MAX_DAYS;
  const raw = getValue("api-key-expiry-date");
  // The end of the chosen day, but never beyond the maximum lifetime.
  const end = /^\d{4}-\d{2}-\d{2}$/.test(raw) ? new Date(`${raw}T23:59:59`) : null;
  const now = Date.now();
  if (!end || isNaN(end.getTime()) || end.getTime() <= now || end.getTime() - now > (max + 1) * APIKEY_DAY_MS) {
    showToast(tf("apikeys.err_date", { days: max }), "error");
    const field = document.getElementById("api-key-expiry-date");
    if (field) field.focus();
    return null;
  }
  const at = Math.min(end.getTime(), now + max * APIKEY_DAY_MS - 60000);
  return { expires_at: new Date(at).toISOString() };
}

// ---------------------------------------------------------------------------
// Settings → Security
// ---------------------------------------------------------------------------

// apiKeyExpiryFillSecurity fills the API key lifetime fields (app.js fillSecurity).
function apiKeyExpiryFillSecurity(sec) {
  setValue("set-api-key-max-lifetime", String(Number(sec.api_key_max_lifetime_days) || 0));
  setValue("set-api-key-warning", String(Number(sec.api_key_expiry_warning_days) || APIKEY_DEFAULT_WARNING_DAYS));
}

// apiKeyExpiryCollectSecurity validates the API key lifetime fields (app.js
// collectSecurity); a FieldError marks the field.
function apiKeyExpiryCollectSecurity() {
  const out = {};
  const max = document.getElementById("set-api-key-max-lifetime");
  if (max) {
    const raw = String(max.value || "0").trim();
    const n = Number(raw);
    if (!/^\d+$/.test(raw) || n > APIKEY_ABSOLUTE_MAX_DAYS) throw new FieldError("set-api-key-max-lifetime", t("apikeys.err_max_lifetime"));
    out.api_key_max_lifetime_days = n;
  }
  const warn = document.getElementById("set-api-key-warning");
  if (warn) {
    const raw = String(warn.value || "").trim();
    const n = Number(raw);
    if (!/^\d+$/.test(raw) || n < 1 || n > 365) throw new FieldError("set-api-key-warning", t("apikeys.err_warning_days"));
    out.api_key_expiry_warning_days = n;
  }
  return out;
}
