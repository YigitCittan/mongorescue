/**
 * MongoRescue dashboard: navigation.
 *
 * - The command palette (Ctrl/Cmd+K): jump to tabs, settings sections, jobs,
 *   connections and backups (searched through the list API), and run actions.
 * - Keyboard shortcuts: "g" then a letter for the tabs, "/" for the search, "?" for
 *   the shortcut list, "n" for a new item in the current tab, "r" to refresh.
 * - Deep links (#/backups/<id>, #/jobs/<id>, #/restores/<id>) that open a details
 *   dialog, survive a refresh and follow back/forward, plus "Copy link".
 * - The toast queue (stacked, dismissable, with an optional action button).
 * - Relative or absolute times, the time zone label and the documentation link of
 *   the user menu.
 *
 * Loaded after i18n.js, app.js, runs.js, trust.js, forms.js and bulk.js, whose
 * globals it uses (state, auth, t, tf, apiJSON, escapeHtml, openModal, closeModal,
 * activateTab, ...). app.js calls toastShow(), timeDisplay(), navHoldsHash() and
 * navOpenDetail(). The same security invariants apply: server values reach the DOM
 * through textContent or escapeHtml(), IDs in URLs go through encodeURIComponent(),
 * and actions use delegated data-action listeners ("nav-*").
 */

// ---------------------------------------------------------------------------
// Translations, merged into i18n.js's table (entries already there win)
// ---------------------------------------------------------------------------

const NAV_I18N = {
  en: {
    tabs: { overview: "Overview" },
    palette: {
      title: "Command palette",
      placeholder: "Search or jump to…",
      label: "Search commands, backups, jobs and connections",
      hint: "↑ ↓ to move · Enter to open · Esc to close",
      group_actions: "Actions",
      group_goto: "Go to",
      group_settings: "Settings",
      group_jobs: "Jobs",
      group_connections: "Connections",
      group_backups: "Backups",
      go_tab: "Go to {name}",
      settings_section: "Settings › {name}",
      check_updates: "Check for updates",
      check_updates_sub: "Installed: {version} · opens the releases page",
      shortcuts: "Keyboard shortcuts",
      toggle_time: "Switch between relative and absolute times",
      searching: "Searching backups…",
      no_results: "No matches",
      results_n: "{n} results",
      search_failed: "The backup search failed.",
      needs_connection: "Add a MongoDB connection first.",
      installed: "Installed: {version}",
      checking_updates: "Checking for updates…",
      up_to_date: "Up to date ({version})",
      update_available: "{version} is available",
      update_now: "Update",
      check_failed: "Could not check for updates: {error}",
      update_failed: "The update could not start: {error}",
    },
    keys: {
      title: "Keyboard shortcuts",
      palette: "Open the command palette",
      search: "Focus the search of the current tab",
      help: "Show this list",
      new_item: "New item in the current tab (backup, job, connection, channel)",
      refresh: "Refresh",
      then: "then",
      note: "Single-key shortcuts are off while you type in a field or a dialog is open.",
    },
    links: {
      copy: "Copy link",
      not_found_backups: "Backup {id} was not found.",
      not_found_jobs: "Job {id} was not found.",
      not_found_restores: "Restore {id} was not found.",
    },
    toast: {
      dismiss: "Dismiss",
      view: "View",
      region: "Messages",
    },
    timefmt: {
      absolute: "Show absolute times",
      zone: "Your time zone: {zone}",
      server_zone: "Server time zone: {zone}",
      docs: "Help & documentation",
      docs_title: "Opens the documentation of {version} on GitHub",
    },
  },
  tr: {
    tabs: { overview: "Genel bakış" },
    palette: {
      title: "Komut paleti",
      placeholder: "Ara veya git…",
      label: "Komut, yedek, görev ve bağlantı ara",
      hint: "↑ ↓ ile gezin · Enter ile aç · Esc ile kapat",
      group_actions: "Eylemler",
      group_goto: "Git",
      group_settings: "Ayarlar",
      group_jobs: "Görevler",
      group_connections: "Bağlantılar",
      group_backups: "Yedekler",
      go_tab: "{name} sekmesine git",
      settings_section: "Ayarlar › {name}",
      check_updates: "Güncellemeleri denetle",
      check_updates_sub: "Kurulu: {version} · sürümler sayfasını açar",
      shortcuts: "Klavye kısayolları",
      toggle_time: "Göreli ve mutlak zaman arasında geçiş yap",
      searching: "Yedekler aranıyor…",
      no_results: "Eşleşme yok",
      results_n: "{n} sonuç",
      search_failed: "Yedek araması başarısız oldu.",
      needs_connection: "Önce bir MongoDB bağlantısı ekleyin.",
      installed: "Kurulu: {version}",
      checking_updates: "Güncellemeler denetleniyor…",
      up_to_date: "Güncel ({version})",
      update_available: "{version} mevcut",
      update_now: "Güncelle",
      check_failed: "Güncellemeler denetlenemedi: {error}",
      update_failed: "Güncelleme başlatılamadı: {error}",
    },
    keys: {
      title: "Klavye kısayolları",
      palette: "Komut paletini aç",
      search: "Geçerli sekmenin aramasına odaklan",
      help: "Bu listeyi göster",
      new_item: "Geçerli sekmede yeni öğe (yedek, görev, bağlantı, kanal)",
      refresh: "Yenile",
      then: "ardından",
      note: "Bir alana yazarken veya bir pencere açıkken tek tuşlu kısayollar kapalıdır.",
    },
    links: {
      copy: "Bağlantıyı kopyala",
      not_found_backups: "{id} yedeği bulunamadı.",
      not_found_jobs: "{id} görevi bulunamadı.",
      not_found_restores: "{id} geri yüklemesi bulunamadı.",
    },
    toast: {
      dismiss: "Kapat",
      view: "Görüntüle",
      region: "Mesajlar",
    },
    timefmt: {
      absolute: "Mutlak zamanları göster",
      zone: "Saat diliminiz: {zone}",
      server_zone: "Sunucu saat dilimi: {zone}",
      docs: "Yardım ve belgeler",
      docs_title: "{version} belgelerini GitHub'da açar",
    },
  },
  de: {
    tabs: { overview: "Übersicht" },
    palette: {
      title: "Befehlspalette",
      placeholder: "Suchen oder springen…",
      label: "Befehle, Backups, Aufträge und Verbindungen suchen",
      hint: "↑ ↓ zum Wählen · Enter zum Öffnen · Esc zum Schließen",
      group_actions: "Aktionen",
      group_goto: "Gehe zu",
      group_settings: "Einstellungen",
      group_jobs: "Aufträge",
      group_connections: "Verbindungen",
      group_backups: "Backups",
      go_tab: "Gehe zu {name}",
      settings_section: "Einstellungen › {name}",
      check_updates: "Nach Updates suchen",
      check_updates_sub: "Installiert: {version} · öffnet die Release-Seite",
      shortcuts: "Tastenkürzel",
      toggle_time: "Zwischen relativen und absoluten Zeiten wechseln",
      searching: "Backups werden gesucht…",
      no_results: "Keine Treffer",
      results_n: "{n} Ergebnisse",
      search_failed: "Die Backup-Suche ist fehlgeschlagen.",
      needs_connection: "Fügen Sie zuerst eine MongoDB-Verbindung hinzu.",
      installed: "Installiert: {version}",
      checking_updates: "Suche nach Updates…",
      up_to_date: "Aktuell ({version})",
      update_available: "{version} ist verfügbar",
      update_now: "Aktualisieren",
      check_failed: "Nach Updates suchen fehlgeschlagen: {error}",
      update_failed: "Das Update konnte nicht starten: {error}",
    },
    keys: {
      title: "Tastenkürzel",
      palette: "Befehlspalette öffnen",
      search: "Suche des aktuellen Tabs fokussieren",
      help: "Diese Liste anzeigen",
      new_item: "Neues Element im aktuellen Tab (Backup, Auftrag, Verbindung, Kanal)",
      refresh: "Aktualisieren",
      then: "dann",
      note: "Einzeltasten-Kürzel sind aus, während Sie in einem Feld tippen oder ein Dialog offen ist.",
    },
    links: {
      copy: "Link kopieren",
      not_found_backups: "Backup {id} wurde nicht gefunden.",
      not_found_jobs: "Auftrag {id} wurde nicht gefunden.",
      not_found_restores: "Wiederherstellung {id} wurde nicht gefunden.",
    },
    toast: {
      dismiss: "Schließen",
      view: "Anzeigen",
      region: "Meldungen",
    },
    timefmt: {
      absolute: "Absolute Zeiten anzeigen",
      zone: "Ihre Zeitzone: {zone}",
      server_zone: "Zeitzone des Servers: {zone}",
      docs: "Hilfe und Dokumentation",
      docs_title: "Öffnet die Dokumentation von {version} auf GitHub",
    },
  },
  es: {
    tabs: { overview: "Resumen" },
    palette: {
      title: "Paleta de comandos",
      placeholder: "Buscar o ir a…",
      label: "Buscar comandos, copias, tareas y conexiones",
      hint: "↑ ↓ para moverse · Intro para abrir · Esc para cerrar",
      group_actions: "Acciones",
      group_goto: "Ir a",
      group_settings: "Ajustes",
      group_jobs: "Tareas",
      group_connections: "Conexiones",
      group_backups: "Copias de seguridad",
      go_tab: "Ir a {name}",
      settings_section: "Ajustes › {name}",
      check_updates: "Buscar actualizaciones",
      check_updates_sub: "Instalada: {version} · abre la página de versiones",
      shortcuts: "Atajos de teclado",
      toggle_time: "Alternar entre horas relativas y absolutas",
      searching: "Buscando copias…",
      no_results: "Sin coincidencias",
      results_n: "{n} resultados",
      search_failed: "La búsqueda de copias ha fallado.",
      needs_connection: "Añada primero una conexión de MongoDB.",
      installed: "Instalada: {version}",
      checking_updates: "Buscando actualizaciones…",
      up_to_date: "Actualizado ({version})",
      update_available: "{version} está disponible",
      update_now: "Actualizar",
      check_failed: "No se pudieron buscar actualizaciones: {error}",
      update_failed: "No se pudo iniciar la actualización: {error}",
    },
    keys: {
      title: "Atajos de teclado",
      palette: "Abrir la paleta de comandos",
      search: "Ir a la búsqueda de la pestaña actual",
      help: "Mostrar esta lista",
      new_item: "Nuevo elemento en la pestaña actual (copia, tarea, conexión, canal)",
      refresh: "Actualizar",
      then: "luego",
      note: "Los atajos de una tecla están desactivados mientras escribe en un campo o hay un diálogo abierto.",
    },
    links: {
      copy: "Copiar enlace",
      not_found_backups: "No se encontró la copia {id}.",
      not_found_jobs: "No se encontró la tarea {id}.",
      not_found_restores: "No se encontró la restauración {id}.",
    },
    toast: {
      dismiss: "Descartar",
      view: "Ver",
      region: "Mensajes",
    },
    timefmt: {
      absolute: "Mostrar horas absolutas",
      zone: "Su zona horaria: {zone}",
      server_zone: "Zona horaria del servidor: {zone}",
      docs: "Ayuda y documentación",
      docs_title: "Abre la documentación de {version} en GitHub",
    },
  },
  fr: {
    tabs: { overview: "Vue d'ensemble" },
    palette: {
      title: "Palette de commandes",
      placeholder: "Rechercher ou aller à…",
      label: "Rechercher des commandes, sauvegardes, tâches et connexions",
      hint: "↑ ↓ pour naviguer · Entrée pour ouvrir · Échap pour fermer",
      group_actions: "Actions",
      group_goto: "Aller à",
      group_settings: "Paramètres",
      group_jobs: "Tâches",
      group_connections: "Connexions",
      group_backups: "Sauvegardes",
      go_tab: "Aller à {name}",
      settings_section: "Paramètres › {name}",
      check_updates: "Rechercher des mises à jour",
      check_updates_sub: "Installée : {version} · ouvre la page des versions",
      shortcuts: "Raccourcis clavier",
      toggle_time: "Basculer entre heures relatives et absolues",
      searching: "Recherche des sauvegardes…",
      no_results: "Aucun résultat",
      results_n: "{n} résultats",
      search_failed: "La recherche de sauvegardes a échoué.",
      needs_connection: "Ajoutez d'abord une connexion MongoDB.",
      installed: "Installée : {version}",
      checking_updates: "Recherche des mises à jour…",
      up_to_date: "À jour ({version})",
      update_available: "{version} est disponible",
      update_now: "Mettre à jour",
      check_failed: "Impossible de rechercher des mises à jour : {error}",
      update_failed: "La mise à jour n'a pas pu démarrer : {error}",
    },
    keys: {
      title: "Raccourcis clavier",
      palette: "Ouvrir la palette de commandes",
      search: "Aller à la recherche de l'onglet actuel",
      help: "Afficher cette liste",
      new_item: "Nouvel élément dans l'onglet actuel (sauvegarde, tâche, connexion, canal)",
      refresh: "Actualiser",
      then: "puis",
      note: "Les raccourcis à une touche sont désactivés pendant la saisie dans un champ ou lorsqu'une boîte de dialogue est ouverte.",
    },
    links: {
      copy: "Copier le lien",
      not_found_backups: "Sauvegarde {id} introuvable.",
      not_found_jobs: "Tâche {id} introuvable.",
      not_found_restores: "Restauration {id} introuvable.",
    },
    toast: {
      dismiss: "Fermer",
      view: "Voir",
      region: "Messages",
    },
    timefmt: {
      absolute: "Afficher les heures absolues",
      zone: "Votre fuseau horaire : {zone}",
      server_zone: "Fuseau horaire du serveur : {zone}",
      docs: "Aide et documentation",
      docs_title: "Ouvre la documentation de {version} sur GitHub",
    },
  },
  zh: {
    tabs: { overview: "概览" },
    palette: {
      title: "命令面板",
      placeholder: "搜索或跳转…",
      label: "搜索命令、备份、任务和连接",
      hint: "↑ ↓ 移动 · Enter 打开 · Esc 关闭",
      group_actions: "操作",
      group_goto: "跳转到",
      group_settings: "设置",
      group_jobs: "任务",
      group_connections: "连接",
      group_backups: "备份",
      go_tab: "跳转到{name}",
      settings_section: "设置 › {name}",
      check_updates: "检查更新",
      check_updates_sub: "已安装：{version} · 打开发布页面",
      shortcuts: "键盘快捷键",
      toggle_time: "在相对时间和绝对时间之间切换",
      searching: "正在搜索备份…",
      no_results: "没有匹配项",
      results_n: "{n} 个结果",
      search_failed: "备份搜索失败。",
      needs_connection: "请先添加一个 MongoDB 连接。",
      installed: "已安装：{version}",
      checking_updates: "正在检查更新…",
      up_to_date: "已是最新（{version}）",
      update_available: "{version} 可用",
      update_now: "更新",
      check_failed: "无法检查更新：{error}",
      update_failed: "无法开始更新：{error}",
    },
    keys: {
      title: "键盘快捷键",
      palette: "打开命令面板",
      search: "聚焦当前标签页的搜索框",
      help: "显示此列表",
      new_item: "在当前标签页新建（备份、任务、连接、渠道）",
      refresh: "刷新",
      then: "然后",
      note: "在输入框中输入或对话框打开时，单键快捷键不可用。",
    },
    links: {
      copy: "复制链接",
      not_found_backups: "未找到备份 {id}。",
      not_found_jobs: "未找到任务 {id}。",
      not_found_restores: "未找到恢复 {id}。",
    },
    toast: {
      dismiss: "关闭",
      view: "查看",
      region: "消息",
    },
    timefmt: {
      absolute: "显示绝对时间",
      zone: "您的时区：{zone}",
      server_zone: "服务器时区：{zone}",
      docs: "帮助与文档",
      docs_title: "在 GitHub 上打开 {version} 的文档",
    },
  },
  ja: {
    tabs: { overview: "概要" },
    palette: {
      title: "コマンドパレット",
      placeholder: "検索または移動…",
      label: "コマンド、バックアップ、ジョブ、接続を検索",
      hint: "↑ ↓ で移動 · Enter で開く · Esc で閉じる",
      group_actions: "操作",
      group_goto: "移動",
      group_settings: "設定",
      group_jobs: "ジョブ",
      group_connections: "接続",
      group_backups: "バックアップ",
      go_tab: "{name}へ移動",
      settings_section: "設定 › {name}",
      check_updates: "アップデートを確認",
      check_updates_sub: "インストール済み: {version} · リリースページを開きます",
      shortcuts: "キーボードショートカット",
      toggle_time: "相対時刻と絶対時刻を切り替え",
      searching: "バックアップを検索中…",
      no_results: "一致なし",
      results_n: "{n} 件",
      search_failed: "バックアップの検索に失敗しました。",
      needs_connection: "先に MongoDB 接続を追加してください。",
      installed: "インストール済み: {version}",
      checking_updates: "アップデートを確認中…",
      up_to_date: "最新です（{version}）",
      update_available: "{version} が利用可能です",
      update_now: "アップデート",
      check_failed: "アップデートを確認できませんでした: {error}",
      update_failed: "アップデートを開始できませんでした: {error}",
    },
    keys: {
      title: "キーボードショートカット",
      palette: "コマンドパレットを開く",
      search: "現在のタブの検索欄にフォーカス",
      help: "この一覧を表示",
      new_item: "現在のタブで新規作成（バックアップ、ジョブ、接続、チャネル）",
      refresh: "更新",
      then: "→",
      note: "入力欄で入力中やダイアログ表示中は、単一キーのショートカットは無効です。",
    },
    links: {
      copy: "リンクをコピー",
      not_found_backups: "バックアップ {id} が見つかりません。",
      not_found_jobs: "ジョブ {id} が見つかりません。",
      not_found_restores: "リストア {id} が見つかりません。",
    },
    toast: {
      dismiss: "閉じる",
      view: "表示",
      region: "メッセージ",
    },
    timefmt: {
      absolute: "絶対時刻を表示",
      zone: "あなたのタイムゾーン: {zone}",
      server_zone: "サーバーのタイムゾーン: {zone}",
      docs: "ヘルプとドキュメント",
      docs_title: "GitHub で {version} のドキュメントを開きます",
    },
  },
  ru: {
    tabs: { overview: "Обзор" },
    palette: {
      title: "Палитра команд",
      placeholder: "Поиск или переход…",
      label: "Поиск команд, резервных копий, заданий и подключений",
      hint: "↑ ↓ — выбор · Enter — открыть · Esc — закрыть",
      group_actions: "Действия",
      group_goto: "Перейти",
      group_settings: "Настройки",
      group_jobs: "Задания",
      group_connections: "Подключения",
      group_backups: "Резервные копии",
      go_tab: "Перейти: {name}",
      settings_section: "Настройки › {name}",
      check_updates: "Проверить обновления",
      check_updates_sub: "Установлено: {version} · открывает страницу релизов",
      shortcuts: "Сочетания клавиш",
      toggle_time: "Переключить относительное и абсолютное время",
      searching: "Поиск резервных копий…",
      no_results: "Ничего не найдено",
      results_n: "Результатов: {n}",
      search_failed: "Не удалось выполнить поиск резервных копий.",
      needs_connection: "Сначала добавьте подключение к MongoDB.",
      installed: "Установлено: {version}",
      checking_updates: "Проверка обновлений…",
      up_to_date: "Установлена последняя версия ({version})",
      update_available: "Доступна версия {version}",
      update_now: "Обновить",
      check_failed: "Не удалось проверить обновления: {error}",
      update_failed: "Не удалось запустить обновление: {error}",
    },
    keys: {
      title: "Сочетания клавиш",
      palette: "Открыть палитру команд",
      search: "Перейти к поиску текущей вкладки",
      help: "Показать этот список",
      new_item: "Новый элемент на текущей вкладке (копия, задание, подключение, канал)",
      refresh: "Обновить",
      then: "затем",
      note: "Одноклавишные сочетания не работают, пока вы вводите текст в поле или открыт диалог.",
    },
    links: {
      copy: "Копировать ссылку",
      not_found_backups: "Резервная копия {id} не найдена.",
      not_found_jobs: "Задание {id} не найдено.",
      not_found_restores: "Восстановление {id} не найдено.",
    },
    toast: {
      dismiss: "Закрыть",
      view: "Открыть",
      region: "Сообщения",
    },
    timefmt: {
      absolute: "Показывать абсолютное время",
      zone: "Ваш часовой пояс: {zone}",
      server_zone: "Часовой пояс сервера: {zone}",
      docs: "Справка и документация",
      docs_title: "Открывает документацию {version} на GitHub",
    },
  },
};

// navMergeI18n adds the namespaces of table to i18n.js's translations; keys that are
// already defined there win.
function navMergeI18n(table) {
  if (typeof translations !== "object" || !translations) return;
  Object.keys(table).forEach(lang => {
    if (!translations[lang]) translations[lang] = {};
    const target = translations[lang];
    Object.keys(table[lang]).forEach(ns => {
      target[ns] = Object.assign({}, table[lang][ns], target[ns] || {});
    });
  });
}

navMergeI18n(NAV_I18N);

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

const NAV_REPO = "https://github.com/YigitCittan/mongorescue";
// Tabs in order, with the key of their "g" shortcut.
const NAV_TABS = [
  { id: "tab-overview", key: "o", label: "tabs.overview" },
  { id: "tab-backups", key: "b", label: "tabs.backups" },
  { id: "tab-jobs", key: "j", label: "tabs.jobs" },
  { id: "tab-restores", key: "r", label: "tabs.restores" },
  { id: "tab-connections", key: "c", label: "tabs.connections" },
  { id: "tab-notifications", key: "n", label: "tabs.notifications" },
  { id: "tab-settings", key: "s", label: "tabs.settings" }
];
const NAV_CLOSE_ICON = '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M4 4l8 8M12 4l-8 8"/></svg>';

// The running version as shown in the header ("v0.11.0", "dev" or "").
function navVersion() {
  const el = document.getElementById("app-version");
  return el ? String(el.textContent || "").trim() : "";
}

// The Git ref the documentation link points at: the release tag of a release build,
// main for development builds and git-describe versions between tags.
function navDocsRef(version) {
  const v = String(version || "").trim();
  return /^v\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)(?:\.?\d+)*)?$/.test(v) ? v : "main";
}

function navDocsUrl() {
  return `${NAV_REPO}/tree/${encodeURIComponent(navDocsRef(navVersion()))}/docs`;
}

function navOpenExternal(url) {
  const w = window.open(url, "_blank", "noopener,noreferrer");
  if (w) w.opener = null;
}

// True while the focus is in a field where typing must not trigger shortcuts.
function navTyping(target) {
  const el = target instanceof Element ? target : null;
  if (!el) return false;
  if (el.isContentEditable) return true;
  const tag = el.tagName;
  if (tag === "TEXTAREA" || tag === "SELECT") return true;
  if (tag !== "INPUT") return false;
  const type = String(el.getAttribute("type") || "text").toLowerCase();
  return !["checkbox", "radio", "button", "submit", "reset", "range", "color", "file"].includes(type);
}

function navAnyModalOpen() {
  return document.querySelector(".modal-backdrop.open") !== null;
}

// ---------------------------------------------------------------------------
// Toast queue
// ---------------------------------------------------------------------------

const TOAST_TYPES = ["success", "error", "info"];
const TOAST_MAX_VISIBLE = 4;
const toastWaiting = [];

// Shows msg as a toast: type is "success", "error" or "info"; opts.action
// ({label, run}) adds a button, opts.duration overrides how long it stays (ms).
// Toasts stack up to TOAST_MAX_VISIBLE; further ones wait their turn. Screen
// readers hear them through the shared live regions (forms.js), not the toast.
// It returns a handle for toastDismiss (null for an empty message).
function toastShow(msg, type, opts) {
  const text = String(msg || "");
  if (!text) return null;
  const item = { text, type: TOAST_TYPES.includes(type) ? type : "success", opts: opts || {} };
  if (typeof window.mrAnnounce === "function") window.mrAnnounce(text, item.type === "error");
  const container = document.getElementById("toast-container");
  if (!container) return null;
  // The same message already showing: keep it longer instead of stacking a copy.
  const same = Array.from(container.children).find(el => el.dataset.text === text && el.dataset.type === item.type);
  if (same && same.mrRestart) {
    same.mrRestart();
    return same;
  }
  if (container.children.length >= TOAST_MAX_VISIBLE) {
    toastWaiting.push(item);
    return item;
  }
  return toastRender(container, item);
}

function toastRender(container, item) {
  const el = document.createElement("div");
  el.className = `toast toast-${item.type}`;
  el.dataset.text = item.text;
  el.dataset.type = item.type;
  const body = document.createElement("span");
  body.className = "toast-text";
  body.textContent = item.text;
  el.appendChild(body);
  const action = item.opts.action;
  if (action && action.label && typeof action.run === "function") {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn btn-secondary btn-sm toast-action";
    btn.textContent = String(action.label);
    btn.addEventListener("click", () => {
      toastDismiss(el);
      action.run();
    });
    el.appendChild(btn);
  }
  const close = document.createElement("button");
  close.type = "button";
  close.className = "toast-close";
  close.setAttribute("aria-label", t("toast.dismiss"));
  close.title = t("toast.dismiss");
  close.innerHTML = NAV_CLOSE_ICON;
  close.addEventListener("click", () => toastDismiss(el));
  el.appendChild(close);

  const duration = Number(item.opts.duration) > 0 ? Number(item.opts.duration)
    : item.type === "error" || action ? 8000 : 5000;
  let timer = null;
  const start = () => {
    clearTimeout(timer);
    timer = setTimeout(() => toastDismiss(el), duration);
  };
  const pause = () => clearTimeout(timer);
  el.mrRestart = start;
  el.mrStop = pause;
  // Hovering or focusing a toast keeps it, so its button stays reachable.
  el.addEventListener("mouseenter", pause);
  el.addEventListener("mouseleave", () => { if (!el.contains(document.activeElement)) start(); });
  el.addEventListener("focusin", pause);
  el.addEventListener("focusout", (e) => { if (!el.contains(e.relatedTarget)) start(); });
  el.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      toastDismiss(el);
    }
  });
  container.appendChild(el);
  item.el = el;
  start();
  return el;
}

// toastDismiss removes a toast (a handle from toastShow: its element, or a queued
// entry, which leaves the queue or is removed once it was shown).
function toastDismiss(handle) {
  if (handle && !(handle instanceof Element)) {
    const i = toastWaiting.indexOf(handle);
    if (i >= 0) {
      toastWaiting.splice(i, 1);
      return;
    }
    handle = handle.el;
  }
  const el = handle;
  if (!el || !el.parentNode) return;
  if (el.mrStop) el.mrStop();
  const container = el.parentNode;
  const hadFocus = el.contains(document.activeElement);
  el.remove();
  if (hadFocus) {
    const next = container.querySelector(".toast-close");
    if (next) next.focus();
    else if (typeof returnFocus === "function") returnFocus(null);
  }
  if (toastWaiting.length > 0 && container.children.length < TOAST_MAX_VISIBLE) {
    toastRender(container, toastWaiting.shift());
  }
}

// ---------------------------------------------------------------------------
// Time display (relative / absolute), time zone label, documentation link
// ---------------------------------------------------------------------------

const TIME_PREF_KEY = "mongorescue_time_display";
const navTime = { absolute: false, serverZone: null };

try {
  navTime.absolute = window.localStorage.getItem(TIME_PREF_KEY) === "absolute";
} catch (err) {
  navTime.absolute = false;
}

function timeDisplayAbsolute() {
  return navTime.absolute;
}

function navAbsoluteShort(d) {
  try {
    return new Intl.DateTimeFormat(uiLocale(), { dateStyle: "short", timeStyle: "short" }).format(d);
  } catch (err) {
    return formatAbsolute(d);
  }
}

// timeDisplay returns [shown text, tooltip] of a timestamp in the chosen style.
function timeDisplay(d) {
  return navTime.absolute ? [navAbsoluteShort(d), formatRelative(d)] : [formatRelative(d), formatAbsolute(d)];
}

function setTimeDisplay(absolute) {
  navTime.absolute = !!absolute;
  try {
    if (navTime.absolute) {
      window.localStorage.setItem(TIME_PREF_KEY, "absolute");
    } else {
      window.localStorage.removeItem(TIME_PREF_KEY);
    }
  } catch (err) {
    // storage unavailable: the choice lasts for this page only
  }
  navRenderUserMenu();
  if (typeof renderAll === "function") renderAll();
  if (typeof overviewRender === "function") overviewRender();
}

// "Europe/Istanbul (UTC+03:00)" for the browser's zone.
function navZoneLabel(name, offsetMinutes) {
  const sign = offsetMinutes < 0 ? "−" : "+";
  const abs = Math.abs(offsetMinutes);
  const off = `UTC${sign}${String(Math.floor(abs / 60)).padStart(2, "0")}:${String(abs % 60).padStart(2, "0")}`;
  // Abbreviations that are only an offset ("+03") add nothing to it.
  return name && name !== off && !/^[+−-]\d/.test(name) ? `${name} (${off})` : off;
}

function navLocalZone() {
  let name = "";
  try {
    name = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch (err) {
    name = "";
  }
  return { name, offset: -new Date().getTimezoneOffset() };
}

// Remembers the server's time zone (from /api/v1/stats/history or the schedule
// preview), shown in the user menu when it differs from the browser's.
function navSetServerZone(zone) {
  if (!zone || typeof zone.offset_minutes !== "number") return;
  navTime.serverZone = { name: String(zone.name || ""), offset: zone.offset_minutes };
  navRenderUserMenu();
}

function navRenderUserMenu() {
  const toggle = document.getElementById("user-menu-time");
  if (toggle) toggle.setAttribute("aria-checked", String(navTime.absolute));
  const local = navLocalZone();
  const zone = document.getElementById("user-menu-tz");
  if (zone) {
    let text = tf("timefmt.zone", { zone: navZoneLabel(local.name, local.offset) });
    const server = navTime.serverZone;
    if (server && (server.offset !== local.offset || (server.name && local.name && server.name !== local.name && server.name.includes("/")))) {
      text += ` · ${tf("timefmt.server_zone", { zone: navZoneLabel(server.name, server.offset) })}`;
    }
    zone.textContent = text;
  }
  const docs = document.getElementById("user-menu-docs");
  if (docs) {
    docs.href = navDocsUrl();
    const version = navVersion() || navDocsRef("");
    docs.title = tf("timefmt.docs_title", { version });
  }
}

// ---------------------------------------------------------------------------
// Deep links: #/backups/<id>, #/jobs/<id>, #/restores/<id>
// ---------------------------------------------------------------------------

const DETAIL_KINDS = {
  backups: { modal: "modal-backup-details", tab: "tab-backups", current: () => detailsBackupId },
  jobs: { modal: "modal-job-details", tab: "tab-jobs", current: () => detailsJobId },
  restores: { modal: "modal-restore-details", tab: "tab-restores", current: () => detailsRestoreId }
};
const DETAIL_ID_MAX = 256;
const DETAIL_WAIT_MS = 15000;

// The detail link of the page as it was loaded (app.js rewrites the hash once the
// lists load, so it is captured before that).
const deepLink = { initial: null, applying: null, busy: false, seq: 0 };

function parseDetailHash(hash) {
  const m = /^#\/(backups|jobs|restores)\/([^/?#]+)$/.exec(String(hash || ""));
  if (!m) return null;
  let id;
  try {
    id = decodeURIComponent(m[2]);
  } catch (err) {
    return null;
  }
  if (!id || id.length > DETAIL_ID_MAX) return null;
  return { kind: m[1], id };
}

function detailHash(kind, id) {
  return `#/${kind}/${encodeURIComponent(id)}`;
}

function detailUrl(kind, id) {
  return `${window.location.origin}${window.location.pathname}${detailHash(kind, id)}`;
}

deepLink.initial = parseDetailHash(window.location.hash);

// The details dialog that is open (the topmost when several are), or null.
function openDetail() {
  const stack = typeof modalStack !== "undefined" ? modalStack : [];
  for (let i = stack.length - 1; i >= 0; i--) {
    const kind = Object.keys(DETAIL_KINDS).find(k => DETAIL_KINDS[k].modal === stack[i].id);
    if (kind) {
      const id = DETAIL_KINDS[kind].current();
      return id ? { kind, id } : null;
    }
  }
  return null;
}

// navHoldsHash reports that the URL names the open details dialog; app.js then
// leaves the hash alone (list refreshes would otherwise replace it).
function navHoldsHash() {
  const open = openDetail();
  const cur = parseDetailHash(window.location.hash);
  return !!(open && cur && open.kind === cur.kind && open.id === cur.id);
}

function navHistory(method, data, hash) {
  try {
    window.history[method](data, "", hash);
  } catch (err) {
    // history API unavailable (sandboxed frame): the URL simply does not follow
  }
}

// Keeps the URL in step with the details dialogs: opening one pushes its link (so
// Back closes it), switching to another replaces it, closing one goes back to the
// list it was opened from.
function syncDetailHash() {
  // navOpenDetail closes other dialogs and syncs once the target is open.
  if (deepLink.busy) return;
  const open = openDetail();
  const cur = parseDetailHash(window.location.hash);
  const st = window.history.state;
  if (open) {
    const want = detailHash(open.kind, open.id);
    if (window.location.hash === want) return;
    const applying = deepLink.applying;
    if (cur || (applying && applying.kind === open.kind && applying.id === open.id && applying.replace)) {
      navHistory("replaceState", { mrDetail: cur && st && st.mrDetail === "pushed" ? "pushed" : "replaced" }, want);
    } else {
      navHistory("pushState", { mrDetail: "pushed" }, want);
    }
    return;
  }
  if (!cur) return;
  if (st && st.mrDetail === "pushed") {
    window.history.back();
  } else {
    navHistory("replaceState", null, tabHash(activeTabId()));
  }
}

function navNotFound(kind, id) {
  showToast(tf(`links.not_found_${kind}`, { id }), "error");
}

// Resolves true once test() holds, or false after timeoutMs (DETAIL_WAIT_MS by
// default; Infinity waits as long as it takes, e.g. for a sign-in).
function navWait(test, timeoutMs) {
  const limit = timeoutMs === undefined ? DETAIL_WAIT_MS : timeoutMs;
  return new Promise(resolve => {
    const started = Date.now();
    const tick = () => {
      if (test()) return resolve(true);
      if (Date.now() - started > limit) return resolve(false);
      setTimeout(tick, 150);
    };
    tick();
  });
}

// navOpenDetail opens the details dialog of record id ("backups", "jobs" or
// "restores"), fetching the record when it is not loaded. replace makes the URL
// replace the current history entry instead of adding one.
async function navOpenDetail(kind, id, replace) {
  const spec = DETAIL_KINDS[kind];
  if (!spec || !id) return false;
  const seq = ++deepLink.seq;
  await navWait(() => !!auth.user, Infinity);
  if (seq !== deepLink.seq) return false;
  deepLink.busy = true;
  try {
    return await navShowDetail(kind, id, replace, seq);
  } finally {
    deepLink.busy = false;
    deepLink.applying = null;
  }
}

async function navShowDetail(kind, id, replace, seq) {
  const spec = DETAIL_KINDS[kind];
  // Other details dialogs make way; the dialog shown is the one the link names.
  Object.keys(DETAIL_KINDS).forEach(k => {
    if (k !== kind) closeModal(DETAIL_KINDS[k].modal);
  });
  let found = false;
  if (kind === "jobs") {
    await navWait(() => state.loaded.jobs);
    found = state.jobs.some(j => j.id === id);
  } else if (kind === "backups") {
    if (!backupCache.has(id)) {
      try {
        const json = await apiJSON(`/api/v1/backups?id=${encodeURIComponent(id)}&limit=1`);
        if (json.success) rememberBackups(json.data || []);
      } catch (err) {
        // reported as not found below
      }
    }
    found = backupCache.has(id);
  } else {
    await navWait(() => state.loaded.restores);
    if (!state.restores.some(r => r.id === id)) {
      // Restores outside the current page: filter the list to the one asked for.
      const L = lists.restores;
      L.filters = emptyFilters("restores");
      L.filters.q = id.slice(0, MAX_FILTER_TEXT);
      L.page = 1;
      renderListControls("restores", true);
      await loadList("restores");
    }
    found = state.restores.some(r => r.id === id);
  }
  if (seq !== deepLink.seq) return false;
  if (!found) {
    navNotFound(kind, id);
    if (parseDetailHash(window.location.hash)) navHistory("replaceState", null, tabHash(activeTabId()));
    return false;
  }
  deepLink.applying = { kind, id, replace: !!replace };
  if (activeTabId() !== spec.tab) activateTab(spec.tab, false);
  if (kind === "jobs") openJobDetails(id);
  else if (kind === "backups") openBackupDetails(id);
  else openRestoreDetails(id);
  deepLink.busy = false;
  syncDetailHash();
  return true;
}

// Back/forward and edited URLs: open the dialog the hash names, or close the
// details dialogs when it names none.
function applyDetailHash() {
  const link = parseDetailHash(window.location.hash);
  const open = openDetail();
  if (link) {
    if (open && open.kind === link.kind && open.id === link.id) return;
    navOpenDetail(link.kind, link.id, true);
    return;
  }
  if (open) {
    deepLink.seq++;
    Object.keys(DETAIL_KINDS).forEach(k => closeModal(DETAIL_KINDS[k].modal));
  }
}

// Copies the link of the open details dialog (copyText confirms with a toast; the
// button keeps its label).
function copyDetailLink() {
  const open = openDetail();
  if (open) copyText(detailUrl(open.kind, open.id), null);
}

function setupDeepLinks() {
  const observer = new MutationObserver(() => syncDetailHash());
  Object.keys(DETAIL_KINDS).forEach(kind => {
    const el = document.getElementById(DETAIL_KINDS[kind].modal);
    if (el) observer.observe(el, { attributes: true, attributeFilter: ["class"] });
  });
  window.addEventListener("popstate", applyDetailHash);
  window.addEventListener("hashchange", applyDetailHash);
  if (deepLink.initial) {
    const { kind, id } = deepLink.initial;
    deepLink.initial = null;
    navOpenDetail(kind, id, true);
  }
}

// ---------------------------------------------------------------------------
// Check for updates: the desktop app's updater, or the releases page
// ---------------------------------------------------------------------------

// The desktop app's update endpoints (internal/desktop/updater.go). They exist only
// in the app's webview; POSTs need the header, which a cross-origin page cannot send.
const DESKTOP_UPDATE = {
  check: "/desktop/update/check",
  install: "/desktop/update/install",
  release: "/desktop/update/release-page",
  header: "X-MongoRescue-Desktop",
  // Created by the update script while an optional update is available; its click
  // runs the script's own install flow (progress bar, release page fallback).
  button: "mr-update-header"
};

// The desktop app injects its update script, which sets this flag; the web and
// Docker builds never have it.
function navIsDesktop() {
  return window.__mongorescueUpdate === true;
}

function desktopPost(path) {
  return fetch(path, { method: "POST", credentials: "same-origin", cache: "no-store", headers: { [DESKTOP_UPDATE.header]: "1" } });
}

async function desktopJSON(path) {
  const res = await desktopPost(path);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body && body.error ? String(body.error) : `HTTP ${res.status}`);
  return body || {};
}

// The update script polls slowly while idle; a visibility event makes it look at
// the status again soon (it then shows the update bar or the install progress).
function wakeUpdateScript() {
  document.dispatchEvent(new Event("visibilitychange"));
}

// checkForUpdates asks the desktop app's updater for a release now and reports the
// result as a toast ("Up to date (vX)", or "vY is available" with Update); in the
// browser it opens the releases page.
async function checkForUpdates() {
  if (!navIsDesktop()) {
    navOpenExternal(`${NAV_REPO}/releases/latest`);
    return;
  }
  const checking = toastShow(t("palette.checking_updates"), "info", { duration: 60000 });
  let status;
  try {
    status = await desktopJSON(DESKTOP_UPDATE.check);
  } catch (err) {
    toastDismiss(checking);
    showToast(tf("palette.check_failed", { error: err.message }), "error");
    return;
  }
  toastDismiss(checking);
  if (status.available) {
    showToast(tf("palette.update_available", { version: formatVersion(status.latest) }), "info", {
      duration: 20000,
      action: { label: t("palette.update_now"), run: () => startDesktopUpdate(status) }
    });
  } else {
    showToast(tf("palette.up_to_date", { version: formatVersion(status.current) }), "success");
  }
  wakeUpdateScript();
}

// startDesktopUpdate runs the update through the update script's header button
// when it is there; otherwise it asks the updater directly (the release page when
// there is no installable file) and lets the script show the progress.
async function startDesktopUpdate(status) {
  const btn = document.getElementById(DESKTOP_UPDATE.button);
  if (btn && !btn.disabled) {
    btn.click();
    return;
  }
  try {
    await desktopJSON(status && status.installable === false ? DESKTOP_UPDATE.release : DESKTOP_UPDATE.install);
  } catch (err) {
    showToast(tf("palette.update_failed", { error: err.message }), "error");
    return;
  }
  wakeUpdateScript();
}

// ---------------------------------------------------------------------------
// Command palette
// ---------------------------------------------------------------------------

const PALETTE_MAX_PER_GROUP = 6;
const PALETTE_SEARCH_DEBOUNCE_MS = 250;
const palette = { items: [], active: 0, query: "", timer: null, seq: 0, backups: [], searching: false, failed: false };

// Lower-case, without diacritics (so "guncel" finds "Güncellemeleri"), with the
// Turkish dotted and dotless i folded to "i".
function paletteNorm(text) {
  return String(text || "")
    .toLocaleLowerCase()
    .replace(/[ıİ]/g, "i")
    .normalize("NFD")
    .replace(/[̀-ͯ]/g, "");
}

// Fuzzy score of query in text: -1 for no match; whole-substring and word-start
// matches score higher than scattered letters.
function paletteScore(query, text) {
  const q = paletteNorm(query).trim();
  const s = paletteNorm(text);
  if (!q) return 0;
  const at = s.indexOf(q);
  if (at >= 0) return 1000 - at + (at === 0 || /[\s\-_/·›]/.test(s[at - 1]) ? 200 : 0);
  let score = 0;
  let pos = 0;
  let prev = -2;
  for (const ch of q) {
    if (ch === " ") continue;
    const i = s.indexOf(ch, pos);
    if (i < 0) return -1;
    score += i === prev + 1 ? 8 : 1;
    if (i === 0 || /[\s\-_/·›]/.test(s[i - 1])) score += 5;
    prev = i;
    pos = i + 1;
  }
  return score;
}

function paletteNeedsConnection(run) {
  return () => {
    if (state.loaded.connections && state.connections.length === 0) {
      showToast(t("palette.needs_connection"), "info");
      activateTab("tab-connections", false);
      return;
    }
    run();
  };
}

// The static entries: actions, tabs and settings sections.
function paletteStaticItems() {
  const items = [];
  const version = navVersion() || "—";
  items.push(
    { group: "actions", label: t("nav.instant_backup"), icon: "play", run: paletteNeedsConnection(openBackupNowModal) },
    { group: "actions", label: t("nav.new_job"), icon: "plus", run: paletteNeedsConnection(() => openJobModal()) },
    { group: "actions", label: t("conn.add"), icon: "plus", run: () => openConnectionModal("") },
    { group: "actions", label: t("palette.check_updates"),
      sub: tf(navIsDesktop() ? "palette.installed" : "palette.check_updates_sub", { version }),
      icon: navIsDesktop() ? "arrow" : "external", run: checkForUpdates },
    { group: "actions", label: t("timefmt.docs"), icon: "external", run: () => navOpenExternal(navDocsUrl()) },
    { group: "actions", label: t("palette.toggle_time"), icon: "clock", run: () => setTimeDisplay(!navTime.absolute) },
    { group: "actions", label: t("palette.shortcuts"), sub: "?", icon: "key", run: openShortcutHelp }
  );
  NAV_TABS.forEach(tab => {
    if (!document.getElementById(tab.id)) return;
    items.push({ group: "goto", label: tf("palette.go_tab", { name: t(tab.label) }), sub: `g ${tab.key}`, icon: "arrow",
      run: () => activateTab(tab.id, true) });
  });
  document.querySelectorAll(".settings-nav-btn[data-section]").forEach(btn => {
    const section = btn.dataset.section;
    items.push({ group: "settings", label: tf("palette.settings_section", { name: btn.textContent.trim() }), icon: "arrow",
      run: () => {
        activateTab("tab-settings", false);
        showSettingsSection(section, true);
      } });
  });
  return items;
}

function paletteRecordItems() {
  const items = [];
  state.jobs.forEach(job => {
    items.push({ group: "jobs", label: job.name || job.id, sub: `${job.database || ""} · ${job.cron_expression || ""}`,
      keywords: `${job.id} ${job.database || ""}`, icon: "clock", run: () => navOpenDetail("jobs", job.id) });
  });
  state.connections.forEach(c => {
    items.push({ group: "connections", label: c.name || c.id, sub: maskedHost(c.uri), keywords: c.id, icon: "db",
      run: () => {
        activateTab("tab-connections", false);
        openConnectionModal(c.id);
      } });
  });
  palette.backups.forEach(b => {
    const d = parseDate(b.started_at);
    const [, statusLabel] = backupStatus(b.status);
    items.push({ group: "backups", label: b.database || b.id, sub: `${shortBackupId(b.id)} · ${d ? formatRelative(d) : ""} · ${statusLabel}`,
      keywords: b.id, icon: "archive", run: () => navOpenDetail("backups", b.id), remote: true });
  });
  return items;
}

const PALETTE_GROUPS = ["actions", "goto", "jobs", "backups", "connections", "settings"];

function paletteFilter() {
  const q = palette.query.trim();
  const all = paletteStaticItems().concat(paletteRecordItems());
  const scored = [];
  all.forEach(item => {
    if (!q && !["actions", "goto"].includes(item.group)) return;
    // Backups were searched by the server: they match by definition.
    const score = item.remote ? 500 : Math.max(paletteScore(q, item.label), paletteScore(q, item.keywords || "") - 50,
      paletteScore(q, item.sub || "") - 100);
    if (score < 0) return;
    scored.push({ item, score });
  });
  // Groups in their fixed order; with a query, the group of the best match leads.
  let order = PALETTE_GROUPS;
  if (q && scored.length > 0) {
    const best = scored.reduce((a, b) => (b.score > a.score ? b : a));
    order = [best.item.group].concat(PALETTE_GROUPS.filter(g => g !== best.item.group));
  }
  const out = [];
  order.forEach(group => {
    scored.filter(s => s.item.group === group)
      .sort((a, b) => b.score - a.score)
      .slice(0, q ? PALETTE_MAX_PER_GROUP : 20)
      .forEach(s => out.push(s.item));
  });
  return out;
}

const PALETTE_ICONS = {
  play: '<path d="M5 3.5v9l7-4.5z"/>',
  plus: '<path d="M8 3.5v9M3.5 8h9"/>',
  external: '<path d="M9 3h4v4M13 3 7.5 8.5M11 9.5V13H3V5h3.5"/>',
  clock: '<circle cx="8" cy="8" r="5.5"/><path d="M8 5v3l2 1.5"/>',
  key: '<rect x="1.5" y="4.5" width="13" height="7" rx="1.5"/><path d="M4 7h1M7 7h1M10 7h2M4.5 9.5h7"/>',
  arrow: '<path d="M3 8h10M9 4l4 4-4 4"/>',
  db: '<ellipse cx="8" cy="4" rx="5" ry="1.8"/><path d="M3 4v8c0 1 2.2 1.8 5 1.8s5-.8 5-1.8V4M3 8c0 1 2.2 1.8 5 1.8s5-.8 5-1.8"/>',
  archive: '<rect x="2" y="3" width="12" height="3" rx="1"/><path d="M3 6v7h10V6M6.5 8.5h3"/>'
};

function paletteIcon(name) {
  return `<svg class="icon palette-icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">${PALETTE_ICONS[name] || PALETTE_ICONS.arrow}</svg>`;
}

function paletteRender() {
  const list = document.getElementById("palette-list");
  const input = document.getElementById("palette-input");
  const status = document.getElementById("palette-status");
  if (!list || !input) return;
  palette.items = paletteFilter();
  palette.active = Math.min(Math.max(palette.active, 0), Math.max(palette.items.length - 1, 0));
  let html = "";
  let group = "";
  palette.items.forEach((item, i) => {
    if (item.group !== group) {
      group = item.group;
      html += `<li class="palette-group" role="presentation">${escapeHtml(t(`palette.group_${group}`))}</li>`;
    }
    const selected = i === palette.active;
    html += `<li class="palette-option${selected ? " active" : ""}" role="option" id="palette-opt-${i}" data-index="${i}" aria-selected="${selected}">
      ${paletteIcon(item.icon)}<span class="palette-label">${escapeHtml(item.label)}</span>${item.sub ? `<span class="palette-sub">${escapeHtml(item.sub)}</span>` : ""}
    </li>`;
  });
  list.innerHTML = html;
  input.setAttribute("aria-activedescendant", palette.items.length ? `palette-opt-${palette.active}` : "");
  if (!palette.items.length) input.removeAttribute("aria-activedescendant");
  const activeEl = document.getElementById(`palette-opt-${palette.active}`);
  if (activeEl) activeEl.scrollIntoView({ block: "nearest" });
  if (status) {
    let text = palette.items.length ? tf("palette.results_n", { n: palette.items.length }) : t("palette.no_results");
    if (palette.searching) text = `${text} · ${t("palette.searching")}`;
    else if (palette.failed) text = `${text} · ${t("palette.search_failed")}`;
    status.textContent = text;
  }
}

function paletteSearchBackups() {
  clearTimeout(palette.timer);
  const q = palette.query.trim();
  const seq = ++palette.seq;
  palette.failed = false;
  if (q.length < 2) {
    palette.backups = [];
    palette.searching = false;
    return;
  }
  palette.searching = true;
  palette.timer = setTimeout(async () => {
    try {
      const json = await apiJSON(`/api/v1/backups?q=${encodeURIComponent(q.slice(0, MAX_FILTER_TEXT))}&limit=${PALETTE_MAX_PER_GROUP}`);
      if (seq !== palette.seq) return;
      palette.backups = json.success && Array.isArray(json.data) ? json.data : [];
      palette.failed = !json.success;
      if (json.success) rememberBackups(palette.backups);
    } catch (err) {
      if (seq !== palette.seq) return;
      palette.backups = [];
      palette.failed = true;
    }
    palette.searching = false;
    paletteRender();
  }, PALETTE_SEARCH_DEBOUNCE_MS);
}

function openPalette() {
  if (!auth.user) return;
  const modal = document.getElementById("modal-palette");
  if (!modal) return;
  toggleUserMenu(false);
  if (modal.classList.contains("open")) {
    closeModal("modal-palette");
    return;
  }
  const input = document.getElementById("palette-input");
  palette.query = "";
  palette.active = 0;
  palette.backups = [];
  palette.searching = false;
  palette.failed = false;
  if (input) input.value = "";
  paletteRender();
  openModal("modal-palette");
  if (input) input.focus();
}

function paletteRun(index) {
  const item = palette.items[index];
  if (!item) return;
  closeModal("modal-palette");
  // The dialog's focus return happens first; the command may then move the focus.
  setTimeout(() => item.run(), 0);
}

function setupPalette() {
  const input = document.getElementById("palette-input");
  const list = document.getElementById("palette-list");
  if (!input || !list) return;
  input.addEventListener("input", () => {
    palette.query = input.value.slice(0, MAX_FILTER_TEXT);
    palette.active = 0;
    paletteSearchBackups();
    paletteRender();
  });
  input.addEventListener("keydown", (e) => {
    const n = palette.items.length;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (n === 0) return;
      palette.active = (palette.active + (e.key === "ArrowDown" ? 1 : n - 1)) % n;
      paletteRender();
    } else if (e.key === "Home" && e.ctrlKey) {
      palette.active = 0;
      paletteRender();
    } else if (e.key === "End" && e.ctrlKey) {
      palette.active = Math.max(n - 1, 0);
      paletteRender();
    } else if (e.key === "Enter") {
      e.preventDefault();
      paletteRun(palette.active);
    }
  });
  list.addEventListener("mousemove", (e) => {
    const opt = e.target.closest(".palette-option");
    if (!opt) return;
    const i = Number(opt.dataset.index);
    if (i !== palette.active) {
      palette.active = i;
      paletteRender();
    }
  });
  list.addEventListener("click", (e) => {
    const opt = e.target.closest(".palette-option");
    if (opt) paletteRun(Number(opt.dataset.index));
  });
  // Clicks on the list must not take the focus away from the input.
  list.addEventListener("mousedown", (e) => e.preventDefault());
}

// ---------------------------------------------------------------------------
// Keyboard shortcuts
// ---------------------------------------------------------------------------

const SHORTCUT_CHORD_MS = 1500;
const shortcuts = { pendingG: 0 };

function openShortcutHelp() {
  const list = document.getElementById("shortcuts-list");
  if (list) {
    const isMac = /Mac|iPhone|iPad/.test(navigator.platform || "");
    const rows = [
      [[isMac ? "⌘" : "Ctrl", "K"], t("keys.palette")],
      [["/"], t("keys.search")],
      [["?"], t("keys.help")],
      [["n"], t("keys.new_item")],
      [["r"], t("keys.refresh")]
    ];
    NAV_TABS.forEach(tab => rows.push([["g", tab.key], tf("palette.go_tab", { name: t(tab.label) }), true]));
    list.innerHTML = rows.map(([keys, label, chord]) => {
      const sep = chord ? ` <span class="kbd-sep">${escapeHtml(t("keys.then"))}</span> ` : " + ";
      return `<dt>${keys.map(k => `<kbd>${escapeHtml(k)}</kbd>`).join(sep)}</dt><dd>${escapeHtml(label)}</dd>`;
    }).join("");
  }
  openModal("modal-shortcuts");
}

// The search field of the active tab, if it has one.
function activeSearchInput() {
  const pane = document.querySelector(".tab-pane.active");
  return pane ? pane.querySelector(".filter-search input") : null;
}

// "n": the primary "new" button of the active tab, if it is enabled.
function newInActiveTab() {
  const pane = document.querySelector(".tab-pane.active");
  if (!pane) return;
  if (pane.id === "tab-overview") {
    const btn = pane.querySelector('[data-action="backup-now"]');
    if (btn && !btn.disabled) btn.click();
    return;
  }
  const btn = pane.querySelector('.section-actions .btn-primary:not([disabled]), .section-actions [data-action^="new-"]:not([disabled])');
  if (btn) btn.click();
}

function onShortcutKey(e) {
  if (e.defaultPrevented || e.isComposing) return;
  // Ctrl/Cmd+K works everywhere, also in fields and to close the palette.
  if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && String(e.key).toLowerCase() === "k") {
    const paletteOpen = document.getElementById("modal-palette")?.classList.contains("open");
    if (navAnyModalOpen() && !paletteOpen) return;
    e.preventDefault();
    openPalette();
    return;
  }
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  if (!auth.user || navAnyModalOpen() || navTyping(e.target)) return;
  const userMenu = document.getElementById("user-menu-list");
  if (userMenu && !userMenu.hidden) return;
  const key = e.key;
  if (shortcuts.pendingG && Date.now() - shortcuts.pendingG < SHORTCUT_CHORD_MS) {
    shortcuts.pendingG = 0;
    const tab = NAV_TABS.find(x => x.key === String(key).toLowerCase());
    if (tab && document.getElementById(tab.id)) {
      e.preventDefault();
      activateTab(tab.id, true);
    }
    return;
  }
  shortcuts.pendingG = 0;
  switch (key) {
    case "g":
      shortcuts.pendingG = Date.now();
      break;
    case "/": {
      e.preventDefault();
      const input = activeSearchInput();
      if (input) {
        input.focus();
        input.select();
      } else {
        openPalette();
      }
      break;
    }
    case "?":
      e.preventDefault();
      openShortcutHelp();
      break;
    case "n":
      e.preventDefault();
      newInActiveTab();
      break;
    case "r":
      e.preventDefault();
      refreshAll();
      break;
  }
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

function setupNav() {
  const container = document.getElementById("toast-container");
  if (container) {
    // Toasts are announced through the shared live regions; the stack itself is a
    // labelled region keyboard users can Tab into.
    container.removeAttribute("aria-live");
    container.setAttribute("role", "region");
    container.setAttribute("aria-label", t("toast.region"));
  }
  setupPalette();
  setupDeepLinks();
  document.addEventListener("keydown", onShortcutKey);
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-action^='nav-']");
    if (!btn || btn.disabled) return;
    switch (btn.dataset.action) {
      case "nav-copy-link":
        copyDetailLink();
        break;
      case "nav-toggle-time":
        setTimeDisplay(!navTime.absolute);
        break;
      case "nav-palette":
        openPalette();
        break;
      case "nav-shortcuts":
        toggleUserMenu(false);
        openShortcutHelp();
        break;
    }
  });
  const docs = document.getElementById("user-menu-docs");
  if (docs) docs.addEventListener("click", () => toggleUserMenu(false));
  const menuBtn = document.getElementById("user-menu-btn");
  if (menuBtn) menuBtn.addEventListener("click", navRenderUserMenu, true);
  navRenderUserMenu();
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      if (container) container.setAttribute("aria-label", t("toast.region"));
      navRenderUserMenu();
      if (document.getElementById("modal-palette")?.classList.contains("open")) paletteRender();
    });
  }
}

document.addEventListener("DOMContentLoaded", setupNav);
