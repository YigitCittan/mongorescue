/**
 * MongoRescue dashboard: storage usage and cost.
 *
 * The "Storage usage and cost" card of the Overview (stored size per storage target
 * with its growth, the estimated monthly cost from the price entered on the target
 * and the estimated egress, a trend chart drawn as inline SVG with a text
 * equivalent and a data table, the largest jobs and databases) from GET
 * /api/v1/storage/usage, and the price fields of the storage target form. Every
 * figure is an estimate from MongoRescue's own records, never a bill, and the card
 * says so.
 *
 * Loaded after app.js, trust.js (mergeTranslations) and overview.js (the ov* chart
 * helpers); overview.js calls costLoad() and costRender(), app.js calls
 * costFillTargetForm() and costTargetPayload(). Server values reach the DOM only
 * through escapeHtml() or form values.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const COST_TRANSLATIONS = {
  en: {
    cost: {
      title: "Storage usage and cost",
      sub: "{stored} stored on {n} storage target(s) · an estimate, not a bill",
      none: "No storage targets yet.",
      chart: "Total stored size per day over the last {days} days, from {from} to {to}.",
      col_target: "Target",
      col_stored: "Stored",
      col_growth: "Change",
      col_month: "Per month",
      col_egress: "Egress",
      col_egress_cost: "Egress cost",
      no_price: "No price set",
      kinds: "Backups {backup} · Copies {copy} · Retained copies {retained} · PITR chunks {pitr}",
      egress_detail: "Verification {verification} · Copies {copy} · Restore tests {restore_test}",
      growth_title: "Change over the last {days} days",
      month_title: "Stored size now × the storage price",
      top_jobs: "Largest jobs",
      top_dbs: "Largest databases",
      manual: "Manual backups",
      basis: "An estimate from MongoRescue's own records, not a bill: stored sizes, the last verification of each archive, restore tests and copies. No provider billing data is used.",
      load_failed: "Could not load the storage usage.",
      price_title: "Estimated cost (optional)",
      price_currency: "Currency (ISO code)",
      price_storage: "Storage price per GiB per month",
      price_egress: "Egress price per GiB",
      price_hint: "Used for the cost estimate on the Overview only; MongoRescue never asks the provider for prices. Leave empty for no estimate.",
      err_currency: "Enter a three-letter currency code such as USD or EUR.",
      err_price: "Prices must be numbers of 0 or more."
    }
  },
  tr: {
    cost: {
      title: "Depolama kullanımı ve maliyet",
      sub: "{n} depolama hedefinde {stored} saklanıyor · tahmindir, fatura değildir",
      none: "Henüz depolama hedefi yok.",
      chart: "Son {days} gün için günlük toplam saklanan boyut, {from} - {to} arası.",
      col_target: "Hedef",
      col_stored: "Saklanan",
      col_growth: "Değişim",
      col_month: "Aylık",
      col_egress: "Çıkış trafiği",
      col_egress_cost: "Çıkış maliyeti",
      no_price: "Fiyat girilmemiş",
      kinds: "Yedekler {backup} · Kopyalar {copy} · Tutulan kopyalar {retained} · PITR parçaları {pitr}",
      egress_detail: "Doğrulama {verification} · Kopyalar {copy} · Geri yükleme testleri {restore_test}",
      growth_title: "Son {days} gündeki değişim",
      month_title: "Şu anki boyut × depolama fiyatı",
      top_jobs: "En büyük işler",
      top_dbs: "En büyük veritabanları",
      manual: "Elle alınan yedekler",
      basis: "MongoRescue'un kendi kayıtlarından yapılan bir tahmindir, fatura değildir: saklanan boyutlar, her arşivin son doğrulaması, geri yükleme testleri ve kopyalar. Sağlayıcının fatura verisi kullanılmaz.",
      load_failed: "Depolama kullanımı yüklenemedi.",
      price_title: "Tahmini maliyet (isteğe bağlı)",
      price_currency: "Para birimi (ISO kodu)",
      price_storage: "GiB başına aylık depolama fiyatı",
      price_egress: "GiB başına çıkış trafiği fiyatı",
      price_hint: "Yalnızca Genel Bakış'taki maliyet tahmininde kullanılır; MongoRescue fiyatları sağlayıcıdan hiçbir zaman istemez. Tahmin istemiyorsanız boş bırakın.",
      err_currency: "USD veya EUR gibi üç harfli bir para birimi kodu girin.",
      err_price: "Fiyatlar 0 veya daha büyük sayılar olmalıdır."
    }
  },
  de: {
    cost: {
      title: "Speichernutzung und Kosten",
      sub: "{stored} auf {n} Speicherziel(en) · eine Schätzung, keine Rechnung",
      none: "Noch keine Speicherziele.",
      chart: "Gesamte gespeicherte Größe pro Tag in den letzten {days} Tagen, von {from} bis {to}.",
      col_target: "Ziel",
      col_stored: "Gespeichert",
      col_growth: "Änderung",
      col_month: "Pro Monat",
      col_egress: "Egress",
      col_egress_cost: "Egress-Kosten",
      no_price: "Kein Preis hinterlegt",
      kinds: "Backups {backup} · Kopien {copy} · Aufbewahrte Kopien {retained} · PITR-Chunks {pitr}",
      egress_detail: "Verifizierung {verification} · Kopien {copy} · Wiederherstellungstests {restore_test}",
      growth_title: "Änderung in den letzten {days} Tagen",
      month_title: "Aktuelle Größe × Speicherpreis",
      top_jobs: "Größte Jobs",
      top_dbs: "Größte Datenbanken",
      manual: "Manuelle Backups",
      basis: "Eine Schätzung aus den eigenen Aufzeichnungen von MongoRescue, keine Rechnung: gespeicherte Größen, die letzte Verifizierung jedes Archivs, Wiederherstellungstests und Kopien. Es werden keine Abrechnungsdaten des Anbieters verwendet.",
      load_failed: "Die Speichernutzung konnte nicht geladen werden.",
      price_title: "Geschätzte Kosten (optional)",
      price_currency: "Währung (ISO-Code)",
      price_storage: "Speicherpreis pro GiB und Monat",
      price_egress: "Egress-Preis pro GiB",
      price_hint: "Wird nur für die Kostenschätzung in der Übersicht verwendet; MongoRescue fragt Preise nie beim Anbieter ab. Leer lassen für keine Schätzung.",
      err_currency: "Geben Sie einen dreistelligen Währungscode wie USD oder EUR ein.",
      err_price: "Preise müssen Zahlen ab 0 sein."
    }
  },
  es: {
    cost: {
      title: "Uso de almacenamiento y coste",
      sub: "{stored} almacenados en {n} destino(s) de almacenamiento · una estimación, no una factura",
      none: "Aún no hay destinos de almacenamiento.",
      chart: "Tamaño total almacenado por día en los últimos {days} días, del {from} al {to}.",
      col_target: "Destino",
      col_stored: "Almacenado",
      col_growth: "Cambio",
      col_month: "Por mes",
      col_egress: "Salida de datos",
      col_egress_cost: "Coste de salida",
      no_price: "Sin precio",
      kinds: "Copias de seguridad {backup} · Réplicas {copy} · Réplicas retenidas {retained} · Fragmentos PITR {pitr}",
      egress_detail: "Verificación {verification} · Réplicas {copy} · Pruebas de restauración {restore_test}",
      growth_title: "Cambio en los últimos {days} días",
      month_title: "Tamaño actual × precio de almacenamiento",
      top_jobs: "Trabajos más grandes",
      top_dbs: "Bases de datos más grandes",
      manual: "Copias manuales",
      basis: "Una estimación a partir de los registros de MongoRescue, no una factura: tamaños almacenados, la última verificación de cada archivo, pruebas de restauración y réplicas. No se usan datos de facturación del proveedor.",
      load_failed: "No se pudo cargar el uso de almacenamiento.",
      price_title: "Coste estimado (opcional)",
      price_currency: "Moneda (código ISO)",
      price_storage: "Precio de almacenamiento por GiB al mes",
      price_egress: "Precio de salida de datos por GiB",
      price_hint: "Solo se usa para la estimación de coste del Resumen; MongoRescue nunca pide precios al proveedor. Déjelo vacío para no tener estimación.",
      err_currency: "Introduzca un código de moneda de tres letras, como USD o EUR.",
      err_price: "Los precios deben ser números de 0 o más."
    }
  },
  fr: {
    cost: {
      title: "Utilisation du stockage et coût",
      sub: "{stored} stockés sur {n} cible(s) de stockage · une estimation, pas une facture",
      none: "Aucune cible de stockage pour l'instant.",
      chart: "Taille totale stockée par jour sur les {days} derniers jours, du {from} au {to}.",
      col_target: "Cible",
      col_stored: "Stocké",
      col_growth: "Variation",
      col_month: "Par mois",
      col_egress: "Sortie de données",
      col_egress_cost: "Coût de sortie",
      no_price: "Aucun prix saisi",
      kinds: "Sauvegardes {backup} · Copies {copy} · Copies conservées {retained} · Segments PITR {pitr}",
      egress_detail: "Vérification {verification} · Copies {copy} · Tests de restauration {restore_test}",
      growth_title: "Variation sur les {days} derniers jours",
      month_title: "Taille actuelle × prix du stockage",
      top_jobs: "Plus gros jobs",
      top_dbs: "Plus grosses bases de données",
      manual: "Sauvegardes manuelles",
      basis: "Une estimation à partir des propres enregistrements de MongoRescue, pas une facture : tailles stockées, dernière vérification de chaque archive, tests de restauration et copies. Aucune donnée de facturation du fournisseur n'est utilisée.",
      load_failed: "Impossible de charger l'utilisation du stockage.",
      price_title: "Coût estimé (facultatif)",
      price_currency: "Devise (code ISO)",
      price_storage: "Prix du stockage par Gio et par mois",
      price_egress: "Prix de la sortie de données par Gio",
      price_hint: "Utilisé uniquement pour l'estimation du coût dans la Vue d'ensemble ; MongoRescue ne demande jamais les prix au fournisseur. Laissez vide pour ne pas avoir d'estimation.",
      err_currency: "Saisissez un code de devise à trois lettres, comme USD ou EUR.",
      err_price: "Les prix doivent être des nombres supérieurs ou égaux à 0."
    }
  },
  zh: {
    cost: {
      title: "存储用量与成本",
      sub: "{n} 个存储目标共存储 {stored} · 仅为估算，并非账单",
      none: "尚无存储目标。",
      chart: "最近 {days} 天每日存储总量，从 {from} 到 {to}。",
      col_target: "目标",
      col_stored: "已存储",
      col_growth: "变化",
      col_month: "每月",
      col_egress: "出站流量",
      col_egress_cost: "出站费用",
      no_price: "未设置价格",
      kinds: "备份 {backup} · 副本 {copy} · 保留的副本 {retained} · PITR 分块 {pitr}",
      egress_detail: "校验 {verification} · 副本 {copy} · 恢复测试 {restore_test}",
      growth_title: "最近 {days} 天的变化",
      month_title: "当前存储量 × 存储单价",
      top_jobs: "最大的任务",
      top_dbs: "最大的数据库",
      manual: "手动备份",
      basis: "根据 MongoRescue 自身记录得出的估算，并非账单：已存储大小、每个归档的最近一次校验、恢复测试和副本。不使用服务商的计费数据。",
      load_failed: "无法加载存储用量。",
      price_title: "估算成本（可选）",
      price_currency: "货币（ISO 代码）",
      price_storage: "每 GiB 每月存储价格",
      price_egress: "每 GiB 出站流量价格",
      price_hint: "仅用于概览中的成本估算；MongoRescue 从不向服务商查询价格。留空则不估算。",
      err_currency: "请输入三个字母的货币代码，例如 USD 或 EUR。",
      err_price: "价格必须是不小于 0 的数字。"
    }
  },
  ja: {
    cost: {
      title: "ストレージ使用量とコスト",
      sub: "{n} 件のストレージターゲットに {stored} を保存 · 見積もりであり請求額ではありません",
      none: "ストレージターゲットはまだありません。",
      chart: "過去 {days} 日間の 1 日ごとの保存サイズの合計（{from} から {to}）。",
      col_target: "ターゲット",
      col_stored: "保存量",
      col_growth: "増減",
      col_month: "月額",
      col_egress: "エグレス",
      col_egress_cost: "エグレス費用",
      no_price: "価格未設定",
      kinds: "バックアップ {backup} · コピー {copy} · 保持コピー {retained} · PITR チャンク {pitr}",
      egress_detail: "検証 {verification} · コピー {copy} · リストアテスト {restore_test}",
      growth_title: "過去 {days} 日間の増減",
      month_title: "現在のサイズ × ストレージ単価",
      top_jobs: "サイズの大きいジョブ",
      top_dbs: "サイズの大きいデータベース",
      manual: "手動バックアップ",
      basis: "MongoRescue 自身の記録に基づく見積もりであり、請求額ではありません。保存サイズ、各アーカイブの最後の検証、リストアテスト、コピーが対象です。プロバイダーの請求データは使用しません。",
      load_failed: "ストレージ使用量を読み込めませんでした。",
      price_title: "見積もりコスト（任意）",
      price_currency: "通貨（ISO コード）",
      price_storage: "GiB あたりの月額ストレージ価格",
      price_egress: "GiB あたりのエグレス価格",
      price_hint: "概要のコスト見積もりにのみ使用します。MongoRescue がプロバイダーに価格を問い合わせることはありません。見積もりが不要な場合は空のままにしてください。",
      err_currency: "USD や EUR のような 3 文字の通貨コードを入力してください。",
      err_price: "価格は 0 以上の数値にしてください。"
    }
  },
  ru: {
    cost: {
      title: "Использование хранилища и стоимость",
      sub: "{stored} хранится на {n} хранилищ(ах) · оценка, а не счёт",
      none: "Хранилищ пока нет.",
      chart: "Общий объём хранимых данных по дням за последние {days} дн., с {from} по {to}.",
      col_target: "Хранилище",
      col_stored: "Хранится",
      col_growth: "Изменение",
      col_month: "В месяц",
      col_egress: "Исходящий трафик",
      col_egress_cost: "Стоимость трафика",
      no_price: "Цена не задана",
      kinds: "Резервные копии {backup} · Дубликаты {copy} · Сохранённые дубликаты {retained} · Фрагменты PITR {pitr}",
      egress_detail: "Проверка {verification} · Дубликаты {copy} · Тесты восстановления {restore_test}",
      growth_title: "Изменение за последние {days} дн.",
      month_title: "Текущий объём × цена хранения",
      top_jobs: "Самые большие задания",
      top_dbs: "Самые большие базы данных",
      manual: "Ручные резервные копии",
      basis: "Оценка по собственным записям MongoRescue, а не счёт: объёмы хранимых данных, последняя проверка каждого архива, тесты восстановления и дубликаты. Данные биллинга провайдера не используются.",
      load_failed: "Не удалось загрузить данные об использовании хранилища.",
      price_title: "Ориентировочная стоимость (необязательно)",
      price_currency: "Валюта (код ISO)",
      price_storage: "Цена хранения за ГиБ в месяц",
      price_egress: "Цена исходящего трафика за ГиБ",
      price_hint: "Используется только для оценки стоимости на странице «Обзор»; MongoRescue никогда не запрашивает цены у провайдера. Оставьте пустым, чтобы не получать оценку.",
      err_currency: "Введите трёхбуквенный код валюты, например USD или EUR.",
      err_price: "Цены должны быть числами не меньше 0."
    }
  }
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(COST_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], COST_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// The range of the report in days.
const COST_DAYS = 30;
// The report is refetched at most this often by the periodic refresh.
const COST_TTL_MS = 60000;

const cost = { data: null, loadedAt: 0, loading: false, error: "", hidden: false };

// costLoad fetches GET /api/v1/storage/usage when the cached copy is older than
// COST_TTL_MS (or force is set) and redraws the card. It never throws. A key
// limited to some connections is refused (403), and a server without the report (503) answers 503: the card stays hidden then.
async function costLoad(force) {
  if (typeof auth !== "undefined" && !auth.user) return;
  if (cost.loading) return;
  if (!force && cost.data && Date.now() - cost.loadedAt < COST_TTL_MS) {
    costRender();
    return;
  }
  cost.loading = true;
  try {
    const json = await apiJSON(`/api/v1/storage/usage?days=${COST_DAYS}`);
    if (json.success && json.data) {
      cost.data = json.data;
      cost.error = "";
      cost.hidden = false;
      cost.loadedAt = Date.now();
    } else if (json.httpStatus === 403 || json.httpStatus === 503) {
      cost.data = null;
      cost.hidden = true;
    } else {
      cost.error = json.error || t("cost.load_failed");
    }
  } catch (err) {
    cost.error = t("cost.load_failed");
  } finally {
    cost.loading = false;
  }
  costRender();
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

// costMoney formats amount in the currency code ("USD", "EUR"); an unknown code
// falls back to "<amount> <code>".
function costMoney(amount, code) {
  const n = Number(amount) || 0;
  try {
    return new Intl.NumberFormat(uiLocale(), { style: "currency", currency: code, maximumFractionDigits: n !== 0 && Math.abs(n) < 1 ? 4 : 2 }).format(n);
  } catch (err) {
    return `${n.toFixed(2)} ${code}`;
  }
}

// costSigned formats a change of bytes with its sign.
function costSigned(bytes) {
  const n = Number(bytes) || 0;
  if (n === 0) return formatBytes(0);
  return `${n > 0 ? "+" : "−"}${formatBytes(Math.abs(n))}`;
}

// costTotalSeries sums the daily history of every target: [{date, bytes}].
function costTotalSeries(targets) {
  const sums = new Map();
  (targets || []).forEach(tg => (tg.history || []).forEach(p => {
    sums.set(p.date, (sums.get(p.date) || 0) + (Number(p.bytes) || 0));
  }));
  return [...sums.keys()].sort().map(date => ({ date, bytes: sums.get(date) }));
}

// costChart draws the total stored size per day as an area under a line.
function costChart(points) {
  const plotW = OV_W - OV_PAD.left - OV_PAD.right;
  const plotH = OV_H - OV_PAD.top - OV_PAD.bottom;
  const max = ovNiceMax(Math.max(0, ...points.map(p => p.bytes)));
  const step = plotW / Math.max(points.length, 1);
  const x = i => OV_PAD.left + i * step + step / 2;
  const y = v => OV_PAD.top + plotH - (v / max) * plotH;
  let body = "";
  [0, max / 2, max].forEach(v => {
    body += `<line class="ov-gridline" x1="${OV_PAD.left}" x2="${OV_W - OV_PAD.right}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"/>`;
    body += ovText(OV_PAD.left - 6, y(v) + 4, v === 0 ? "0" : formatBytes(v), "ov-axis-label", "end");
  });
  if (points.length > 0) {
    const pts = points.map((p, i) => `${x(i).toFixed(1)},${y(p.bytes).toFixed(1)}`);
    body += `<path class="ov-area" d="M${x(0).toFixed(1)},${y(0).toFixed(1)} L${pts.join(" L")} L${x(points.length - 1).toFixed(1)},${y(0).toFixed(1)} Z"/>`;
    body += `<path class="ov-line" d="M${pts.join(" L")}"/>`;
    points.forEach((p, i) => {
      body += `<rect class="ov-hit" x="${(x(i) - step / 2).toFixed(1)}" y="${OV_PAD.top}" width="${step.toFixed(1)}" height="${plotH}"><title>${escapeHtml(`${ovDayLabel(p.date)}: ${formatBytes(p.bytes)}`)}</title></rect>`;
    });
    body += `<circle class="ov-dot" cx="${x(points.length - 1).toFixed(1)}" cy="${y(points[points.length - 1].bytes).toFixed(1)}" r="3.5"/>`;
  }
  return body + ovXLabels(points, step);
}

// costTargetRow returns the table row of one target.
function costTargetRow(tg) {
  const k = tg.by_kind || {};
  const kinds = tf("cost.kinds", { backup: formatBytes(k.backup), copy: formatBytes(k.copy), retained: formatBytes(k.retained), pitr: formatBytes(k.pitr) });
  const e = tg.egress || {};
  const src = e.by_source || {};
  const egress = tf("cost.egress_detail", { verification: formatBytes(src.verification), copy: formatBytes(src.copy), restore_test: formatBytes(src.restore_test) });
  const code = tg.pricing && tg.pricing.currency;
  const month = tg.monthly_cost != null && code ? costMoney(tg.monthly_cost, code) : t("cost.no_price");
  const egressCost = e.cost != null && code ? costMoney(e.cost, code) : "—";
  const growth = tf("cost.growth_title", { days: cost.data.days });
  return `<tr>
    <th scope="row">${escapeHtml(tg.name || tg.target_id)}</th>
    <td class="num" title="${escapeHtml(kinds)}">${escapeHtml(formatBytes(tg.stored_bytes))}</td>
    <td class="num" title="${escapeHtml(growth)}">${escapeHtml(costSigned(tg.growth_bytes))}</td>
    <td class="num" title="${escapeHtml(t("cost.month_title"))}">${escapeHtml(month)}</td>
    <td class="num" title="${escapeHtml(egress)}">${escapeHtml(formatBytes(e.bytes))}</td>
    <td class="num">${escapeHtml(egressCost)}</td>
  </tr>`;
}

// costTopList returns the list of the largest consumers: jobs (by name) or databases.
function costTopList(title, items, label) {
  if (!items || items.length === 0) return "";
  const rows = items.slice(0, 5).map(it => `<li><span>${escapeHtml(label(it))}</span> <span class="muted">${escapeHtml(formatBytes(it.bytes))}</span></li>`).join("");
  return `<div class="cost-top"><h4 class="cost-top-title">${escapeHtml(title)}</h4><ul class="cost-top-list">${rows}</ul></div>`;
}

// costRender draws the card from the cached report.
function costRender() {
  const card = document.getElementById("ov-cost-card");
  if (!card) return;
  card.hidden = cost.hidden || (!cost.data && !cost.error);
  if (cost.hidden) return;
  const sub = document.getElementById("ov-cost-sub");
  const box = document.getElementById("ov-cost-body");
  const d = cost.data;
  if (!d) {
    if (sub) {
      sub.textContent = cost.error;
      sub.classList.toggle("text-danger", !!cost.error);
    }
    return;
  }
  const targets = Array.isArray(d.targets) ? d.targets : [];
  const stored = targets.reduce((n, tg) => n + (Number(tg.stored_bytes) || 0), 0);
  ovSetText("ov-cost-figure", targets.length ? formatBytes(stored) : "—");
  if (sub) {
    sub.classList.remove("text-danger");
    sub.textContent = targets.length ? tf("cost.sub", { stored: formatBytes(stored), n: targets.length }) : "";
  }
  if (!box) return;
  if (targets.length === 0) {
    ovSet("ov-cost-body", `<p class="ov-empty-text">${escapeHtml(t("cost.none"))}</p>`);
    return;
  }
  const series = costTotalSeries(targets);
  const from = series.length ? ovDayLabel(series[0].date) : "";
  const to = series.length ? ovDayLabel(series[series.length - 1].date) : "";
  const head = ["col_target", "col_stored", "col_growth", "col_month", "col_egress", "col_egress_cost"]
    .map(k => `<th scope="col">${escapeHtml(t(`cost.${k}`))}</th>`).join("");
  const totals = (Array.isArray(d.totals) ? d.totals : []).map(c => `${costMoney(c.monthly_cost, c.code)}`).join(" + ");
  const jobs = costTopList(t("cost.top_jobs"), d.jobs, it => ovJobName(it.job_id));
  const dbs = costTopList(t("cost.top_dbs"), d.databases, it => it.database);
  ovSet("ov-cost-body",
    `<p class="sr-only" id="ov-cost-desc">${escapeHtml(tf("cost.chart", { days: d.days, from, to }))}</p>` +
    (series.length ? ovSvg(OV_W, OV_H, "ov-cost-desc", costChart(series)) : "") +
    `<div class="table-wrap"><table class="table cost-table"><thead><tr>${head}</tr></thead><tbody>${targets.map(costTargetRow).join("")}</tbody></table></div>` +
    (totals ? `<p class="ov-card-sub cost-total">${escapeHtml(totals)}</p>` : "") +
    (jobs || dbs ? `<div class="cost-tops">${jobs}${dbs}</div>` : "") +
    `<p class="ov-card-sub cost-basis">${escapeHtml(t("cost.basis"))}</p>`);
}

// ---------------------------------------------------------------------------
// Storage target form: the price
// ---------------------------------------------------------------------------

// costPriceGroup returns the price fields group of the target form, creating it on
// first use below the target's name.
function costPriceGroup() {
  let group = document.getElementById("storage-cost-group");
  if (group) return group;
  const name = document.getElementById("storage-name");
  const anchor = name && name.closest(".form-group");
  if (!anchor) return null;
  group = document.createElement("fieldset");
  group.id = "storage-cost-group";
  group.className = "form-group";
  const legend = document.createElement("legend");
  legend.className = "form-label";
  legend.id = "storage-cost-legend";
  group.append(legend);
  [["currency", "text"], ["storage", "number"], ["egress", "number"]].forEach(([key, type]) => {
    const row = document.createElement("div");
    row.className = "form-group";
    const label = document.createElement("label");
    label.className = "form-label";
    label.htmlFor = `storage-price-${key}`;
    label.id = `storage-price-${key}-label`;
    const input = document.createElement("input");
    input.type = type;
    input.id = `storage-price-${key}`;
    input.className = "form-input mono";
    input.autocomplete = "off";
    input.spellcheck = false;
    if (type === "number") {
      input.min = "0";
      input.step = "any";
      input.inputMode = "decimal";
    } else {
      input.maxLength = 3;
      input.placeholder = "USD";
    }
    row.append(label, input);
    group.append(row);
  });
  const hint = document.createElement("p");
  hint.className = "form-hint";
  hint.id = "storage-price-hint";
  group.append(hint);
  anchor.after(group);
  return group;
}

// costLabelForm sets the (translated) labels of the price fields.
function costLabelForm() {
  setText("storage-cost-legend", t("cost.price_title"));
  setText("storage-price-currency-label", t("cost.price_currency"));
  setText("storage-price-storage-label", t("cost.price_storage"));
  setText("storage-price-egress-label", t("cost.price_egress"));
  setText("storage-price-hint", t("cost.price_hint"));
}

// costFillTargetForm shows the price of target (none for a new one).
function costFillTargetForm(target) {
  if (!costPriceGroup()) return;
  const p = (target && target.pricing) || {};
  setValue("storage-price-currency", p.currency || "");
  setValue("storage-price-storage", p.currency ? String(p.storage_per_gib_month ?? "") : "");
  setValue("storage-price-egress", p.currency ? String(p.egress_per_gib ?? "") : "");
  costLabelForm();
}

// costParsePrice reads a price field: "" is 0, anything but a number of 0 or more
// is a FieldError.
function costParsePrice(id) {
  const raw = getValue(id).trim().replace(",", ".");
  if (raw === "") return 0;
  const v = Number(raw);
  if (!Number.isFinite(v) || v < 0) throw new FieldError(id, t("cost.err_price"));
  return v;
}

// costTargetPayload returns the price of a target save (app.js): an empty object
// (removing the price) when nothing is entered; a FieldError for an invalid entry.
function costTargetPayload() {
  if (!document.getElementById("storage-price-currency")) return {};
  const currency = getValue("storage-price-currency").trim().toUpperCase();
  const storageRaw = getValue("storage-price-storage").trim();
  const egressRaw = getValue("storage-price-egress").trim();
  if (!currency && !storageRaw && !egressRaw) return { pricing: {} };
  if (!/^[A-Z]{3}$/.test(currency)) throw new FieldError("storage-price-currency", t("cost.err_currency"));
  return { pricing: { currency, storage_per_gib_month: costParsePrice("storage-price-storage"), egress_per_gib: costParsePrice("storage-price-egress") } };
}

document.addEventListener("DOMContentLoaded", () => {
  costPriceGroup();
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      if (document.getElementById("storage-cost-group")) costLabelForm();
      costRender();
    });
  }
});
