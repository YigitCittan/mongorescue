/**
 * MongoRescue dashboard: table filters, sorting and table chrome.
 *
 * - Filter bars for the Jobs (server-side: GET /api/v1/jobs?..., the matcher of the
 *   bulk job actions), Connections and Notifications tables (client-side), in the
 *   URL hash next to the Backups / Restores filters (#jobs?status=paused&sort=-name).
 * - Sortable column headers on every table (aria-sort, keyboard: they are buttons).
 *   Backups and Restores sort on the server (sort=size, sort=-duration, ...); the
 *   Jobs table sorts its rows by their data (database by the selection summary,
 *   last run by the run status); the other tables sort their rendered rows.
 * - Saved views (named filter + sort presets per tab, one may open by default),
 *   column visibility and row density per table (localStorage), sticky headers,
 *   loading skeletons and "no match" rows with a Clear filters button.
 * - Inline validation of the job, storage target and notification channel forms
 *   (forms.js's liveValidate).
 *
 * Loaded last; uses the globals of app.js (state, lists, auth, t, tf, escapeHtml,
 * writeHash, loadList, applyListParams, ...). app.js calls tablesApplyHash(),
 * tablesHashParams(), tablesValidSort(), tablesRefresh(), tablesRows(),
 * tablesNoMatch(), tablesSkeleton(), tablesSkeletonFor(), tablesRendered() and
 * tablesReset(); bulk.js calls tablesFilterKey(), tablesTotal(), tablesActiveCount()
 * and tablesApiFilter(). Server values reach innerHTML only through escapeHtml() or
 * as textContent; table names from data attributes are checked against a fixed list.
 */

// ---------------------------------------------------------------------------
// Translations, merged into i18n.js's table (entries already there win)
// ---------------------------------------------------------------------------

const TABLES_I18N = {
  en: {
    tv: {
      col_backup: "Backup",
      col_restore: "Restore",
      jobs_label: "Filter jobs",
      connections_label: "Filter connections",
      channels_label: "Filter channels",
      search_jobs: "Search name, ID or database",
      search_connections: "Search name, host or ID",
      search_channels: "Search name or ID",
      state: "State",
      state_all: "All states",
      state_enabled: "Scheduled",
      state_paused: "Paused",
      connection_all: "All connections",
      schedule: "Frequency",
      schedule_all: "Any frequency",
      schedule_hourly: "Hourly or more often",
      schedule_daily: "Daily",
      schedule_weekly: "Weekly",
      schedule_monthly: "Monthly",
      schedule_other: "Other schedules",
      last_all: "Any last run",
      last_completed: "Last run succeeded",
      last_failed: "Last run failed",
      last_partial: "Last run partly failed",
      last_in_progress: "Running now",
      last_cancelled: "Last run cancelled",
      last_never: "Never ran",
      test_all: "Any test result",
      test_ok: "Reachable",
      test_failed: "Test failed",
      test_untested: "Not tested",
      type_all: "All types",
      enabled_all: "Enabled and disabled",
      no_jobs_match: "No jobs match these filters.",
      no_connections_match: "No connections match these filters.",
      no_channels_match: "No channels match these filters.",
      filter_failed: "The jobs could not be filtered: {error}",
      views: "Views",
      views_title: "Saved views",
      views_empty: "No saved views yet. Save the current filters and sort to come back to them.",
      views_unavailable: "Saved views need browser storage, which is turned off.",
      view_name: "View name",
      save_view: "Save current view",
      save: "Save",
      cancel: "Cancel",
      default_badge: "Default",
      make_default: "Make default",
      unset_default: "Remove default",
      rename: "Rename",
      delete: "Delete",
      view_saved: "View “{name}” saved.",
      view_updated: "View “{name}” updated.",
      view_deleted: "View “{name}” deleted.",
      view_applied: "View “{name}” applied.",
      view_renamed: "View renamed to “{name}”.",
      view_limit: "A tab keeps at most {n} views. Delete one first.",
      view_name_required: "Enter a name for the view.",
      default_set: "“{name}” now opens by default.",
      default_cleared: "No view opens by default any more.",
      default_hint: "The default view opens with the dashboard when the link names no filters.",
      columns: "Columns",
      columns_title: "Columns and density",
      visible_columns: "Visible columns",
      density: "Row density",
      density_comfortable: "Comfortable",
      density_compact: "Compact",
      show_all_columns: "Show all columns",
      err_control: "Must not contain control characters.",
      err_cron: "Use five fields (minute hour day month weekday) or a descriptor such as @daily.",
      err_cron_every: "@every needs a duration such as 30m or 6h.",
      err_integer: "Enter a whole number.",
      err_path_absolute: "Use an absolute path such as /backups.",
      err_url: "Enter an http:// or https:// URL.",
      err_header: "Each line must look like Header-Name: value.",
      err_telegram_token: "A bot token looks like 123456789:AAH… (digits, a colon, then at least 20 characters).",
      err_telegram_chat: "Use a numeric chat ID (such as -1001234567890) or @channelname.",
      err_host: "Enter a host name such as smtp.example.com, without http:// or a port.",
      err_email: "Enter an e-mail address such as alerts@example.com.",
      err_email_list: "Enter 1 to 50 e-mail addresses, separated by commas.",
      err_twilio_sid: "An Account SID is AC followed by 32 hexadecimal characters.",
      err_twilio_from: "Use an E.164 number such as +15551234567 or a Messaging Service SID (MG…).",
      err_phone_list: "Enter 1 to 20 numbers in E.164 format (such as +15551234567), separated by commas."
    }
  },
  tr: {
    tv: {
      col_backup: "Yedek",
      col_restore: "Geri yükleme",
      jobs_label: "Görevleri filtrele",
      connections_label: "Bağlantıları filtrele",
      channels_label: "Kanalları filtrele",
      search_jobs: "Ad, kimlik veya veritabanı ara",
      search_connections: "Ad, sunucu veya kimlik ara",
      search_channels: "Ad veya kimlik ara",
      state: "Durum",
      state_all: "Tüm durumlar",
      state_enabled: "Zamanlanmış",
      state_paused: "Duraklatılmış",
      connection_all: "Tüm bağlantılar",
      schedule: "Sıklık",
      schedule_all: "Her sıklık",
      schedule_hourly: "Saatlik veya daha sık",
      schedule_daily: "Günlük",
      schedule_weekly: "Haftalık",
      schedule_monthly: "Aylık",
      schedule_other: "Diğer zamanlamalar",
      last_all: "Her son çalışma",
      last_completed: "Son çalışma başarılı",
      last_failed: "Son çalışma başarısız",
      last_partial: "Son çalışma kısmen başarısız",
      last_in_progress: "Şu an çalışıyor",
      last_cancelled: "Son çalışma iptal edildi",
      last_never: "Hiç çalışmadı",
      test_all: "Her test sonucu",
      test_ok: "Erişilebilir",
      test_failed: "Test başarısız",
      test_untested: "Test edilmedi",
      type_all: "Tüm türler",
      enabled_all: "Etkin ve devre dışı",
      no_jobs_match: "Bu filtrelere uyan görev yok.",
      no_connections_match: "Bu filtrelere uyan bağlantı yok.",
      no_channels_match: "Bu filtrelere uyan kanal yok.",
      filter_failed: "Görevler filtrelenemedi: {error}",
      views: "Görünümler",
      views_title: "Kayıtlı görünümler",
      views_empty: "Henüz kayıtlı görünüm yok. Geçerli filtreleri ve sıralamayı kaydedip sonra geri dönün.",
      views_unavailable: "Kayıtlı görünümler tarayıcı depolaması gerektirir; depolama kapalı.",
      view_name: "Görünüm adı",
      save_view: "Geçerli görünümü kaydet",
      save: "Kaydet",
      cancel: "Vazgeç",
      default_badge: "Varsayılan",
      make_default: "Varsayılan yap",
      unset_default: "Varsayılanı kaldır",
      rename: "Yeniden adlandır",
      delete: "Sil",
      view_saved: "“{name}” görünümü kaydedildi.",
      view_updated: "“{name}” görünümü güncellendi.",
      view_deleted: "“{name}” görünümü silindi.",
      view_applied: "“{name}” görünümü uygulandı.",
      view_renamed: "Görünümün yeni adı: “{name}”.",
      view_limit: "Bir sekmede en fazla {n} görünüm tutulur. Önce birini silin.",
      view_name_required: "Görünüm için bir ad girin.",
      default_set: "Pano artık “{name}” ile açılır.",
      default_cleared: "Artık varsayılan görünüm yok.",
      default_hint: "Bağlantıda filtre yoksa pano varsayılan görünümle açılır.",
      columns: "Sütunlar",
      columns_title: "Sütunlar ve yoğunluk",
      visible_columns: "Görünen sütunlar",
      density: "Satır yoğunluğu",
      density_comfortable: "Rahat",
      density_compact: "Sıkı",
      show_all_columns: "Tüm sütunları göster",
      err_control: "Denetim karakteri içeremez.",
      err_cron: "Beş alan (dakika saat gün ay haftanın günü) veya @daily gibi bir tanımlayıcı kullanın.",
      err_cron_every: "@every için 30m veya 6h gibi bir süre gerekir.",
      err_integer: "Tam sayı girin.",
      err_path_absolute: "/backups gibi mutlak bir yol kullanın.",
      err_url: "http:// veya https:// ile başlayan bir adres girin.",
      err_header: "Her satır Başlık-Adı: değer biçiminde olmalı.",
      err_telegram_token: "Bot belirteci 123456789:AAH… gibidir (rakamlar, iki nokta, ardından en az 20 karakter).",
      err_telegram_chat: "Sayısal sohbet kimliği (-1001234567890 gibi) veya @kanaladi kullanın.",
      err_host: "http:// ve port olmadan smtp.example.com gibi bir sunucu adı girin.",
      err_email: "alerts@example.com gibi bir e-posta adresi girin.",
      err_email_list: "Virgülle ayrılmış 1 ile 50 arası e-posta adresi girin.",
      err_twilio_sid: "Hesap SID'si AC ve ardından 32 onaltılık karakterden oluşur.",
      err_twilio_from: "+15551234567 gibi bir E.164 numarası veya Messaging Service SID'si (MG…) kullanın.",
      err_phone_list: "Virgülle ayrılmış, E.164 biçiminde (+15551234567 gibi) 1 ile 20 arası numara girin."
    }
  },
  de: {
    tv: {
      col_backup: "Sicherung",
      col_restore: "Wiederherstellung",
      jobs_label: "Aufträge filtern",
      connections_label: "Verbindungen filtern",
      channels_label: "Kanäle filtern",
      search_jobs: "Name, ID oder Datenbank suchen",
      search_connections: "Name, Host oder ID suchen",
      search_channels: "Name oder ID suchen",
      state: "Zustand",
      state_all: "Alle Zustände",
      state_enabled: "Geplant",
      state_paused: "Pausiert",
      connection_all: "Alle Verbindungen",
      schedule: "Häufigkeit",
      schedule_all: "Jede Häufigkeit",
      schedule_hourly: "Stündlich oder öfter",
      schedule_daily: "Täglich",
      schedule_weekly: "Wöchentlich",
      schedule_monthly: "Monatlich",
      schedule_other: "Andere Zeitpläne",
      last_all: "Jeder letzte Lauf",
      last_completed: "Letzter Lauf erfolgreich",
      last_failed: "Letzter Lauf fehlgeschlagen",
      last_partial: "Letzter Lauf teilweise fehlgeschlagen",
      last_in_progress: "Läuft gerade",
      last_cancelled: "Letzter Lauf abgebrochen",
      last_never: "Nie gelaufen",
      test_all: "Jedes Testergebnis",
      test_ok: "Erreichbar",
      test_failed: "Test fehlgeschlagen",
      test_untested: "Nicht getestet",
      type_all: "Alle Typen",
      enabled_all: "Aktiviert und deaktiviert",
      no_jobs_match: "Keine Aufträge entsprechen diesen Filtern.",
      no_connections_match: "Keine Verbindungen entsprechen diesen Filtern.",
      no_channels_match: "Keine Kanäle entsprechen diesen Filtern.",
      filter_failed: "Die Aufträge konnten nicht gefiltert werden: {error}",
      views: "Ansichten",
      views_title: "Gespeicherte Ansichten",
      views_empty: "Noch keine gespeicherten Ansichten. Speichern Sie die aktuellen Filter und die Sortierung, um später darauf zurückzukommen.",
      views_unavailable: "Gespeicherte Ansichten brauchen den Browserspeicher, der ausgeschaltet ist.",
      view_name: "Name der Ansicht",
      save_view: "Aktuelle Ansicht speichern",
      save: "Speichern",
      cancel: "Abbrechen",
      default_badge: "Standard",
      make_default: "Als Standard",
      unset_default: "Standard entfernen",
      rename: "Umbenennen",
      delete: "Löschen",
      view_saved: "Ansicht „{name}“ gespeichert.",
      view_updated: "Ansicht „{name}“ aktualisiert.",
      view_deleted: "Ansicht „{name}“ gelöscht.",
      view_applied: "Ansicht „{name}“ angewendet.",
      view_renamed: "Ansicht umbenannt in „{name}“.",
      view_limit: "Ein Tab hält höchstens {n} Ansichten. Löschen Sie zuerst eine.",
      view_name_required: "Geben Sie einen Namen für die Ansicht ein.",
      default_set: "„{name}“ wird jetzt standardmäßig geöffnet.",
      default_cleared: "Keine Ansicht wird mehr standardmäßig geöffnet.",
      default_hint: "Die Standardansicht öffnet sich mit dem Dashboard, wenn der Link keine Filter nennt.",
      columns: "Spalten",
      columns_title: "Spalten und Dichte",
      visible_columns: "Sichtbare Spalten",
      density: "Zeilendichte",
      density_comfortable: "Bequem",
      density_compact: "Kompakt",
      show_all_columns: "Alle Spalten anzeigen",
      err_control: "Darf keine Steuerzeichen enthalten.",
      err_cron: "Verwenden Sie fünf Felder (Minute Stunde Tag Monat Wochentag) oder einen Deskriptor wie @daily.",
      err_cron_every: "@every braucht eine Dauer wie 30m oder 6h.",
      err_integer: "Geben Sie eine ganze Zahl ein.",
      err_path_absolute: "Verwenden Sie einen absoluten Pfad wie /backups.",
      err_url: "Geben Sie eine http://- oder https://-Adresse ein.",
      err_header: "Jede Zeile muss wie Header-Name: Wert aussehen.",
      err_telegram_token: "Ein Bot-Token sieht aus wie 123456789:AAH… (Ziffern, ein Doppelpunkt, dann mindestens 20 Zeichen).",
      err_telegram_chat: "Verwenden Sie eine numerische Chat-ID (wie -1001234567890) oder @kanalname.",
      err_host: "Geben Sie einen Hostnamen wie smtp.example.com ein, ohne http:// und Port.",
      err_email: "Geben Sie eine E-Mail-Adresse wie alerts@example.com ein.",
      err_email_list: "Geben Sie 1 bis 50 E-Mail-Adressen ein, durch Kommas getrennt.",
      err_twilio_sid: "Eine Account SID besteht aus AC und 32 Hexadezimalzeichen.",
      err_twilio_from: "Verwenden Sie eine E.164-Nummer wie +15551234567 oder eine Messaging Service SID (MG…).",
      err_phone_list: "Geben Sie 1 bis 20 Nummern im E.164-Format (wie +15551234567) ein, durch Kommas getrennt."
    }
  },
  es: {
    tv: {
      col_backup: "Copia",
      col_restore: "Restauración",
      jobs_label: "Filtrar tareas",
      connections_label: "Filtrar conexiones",
      channels_label: "Filtrar canales",
      search_jobs: "Buscar nombre, ID o base de datos",
      search_connections: "Buscar nombre, host o ID",
      search_channels: "Buscar nombre o ID",
      state: "Estado",
      state_all: "Todos los estados",
      state_enabled: "Programada",
      state_paused: "En pausa",
      connection_all: "Todas las conexiones",
      schedule: "Frecuencia",
      schedule_all: "Cualquier frecuencia",
      schedule_hourly: "Cada hora o más a menudo",
      schedule_daily: "Diaria",
      schedule_weekly: "Semanal",
      schedule_monthly: "Mensual",
      schedule_other: "Otros horarios",
      last_all: "Cualquier última ejecución",
      last_completed: "Última ejecución correcta",
      last_failed: "Última ejecución fallida",
      last_partial: "Última ejecución fallida en parte",
      last_in_progress: "En ejecución",
      last_cancelled: "Última ejecución cancelada",
      last_never: "Nunca se ejecutó",
      test_all: "Cualquier resultado de prueba",
      test_ok: "Accesible",
      test_failed: "Prueba fallida",
      test_untested: "Sin probar",
      type_all: "Todos los tipos",
      enabled_all: "Activados y desactivados",
      no_jobs_match: "Ninguna tarea coincide con estos filtros.",
      no_connections_match: "Ninguna conexión coincide con estos filtros.",
      no_channels_match: "Ningún canal coincide con estos filtros.",
      filter_failed: "No se pudieron filtrar las tareas: {error}",
      views: "Vistas",
      views_title: "Vistas guardadas",
      views_empty: "Aún no hay vistas guardadas. Guarde los filtros y el orden actuales para volver a ellos.",
      views_unavailable: "Las vistas guardadas necesitan el almacenamiento del navegador, que está desactivado.",
      view_name: "Nombre de la vista",
      save_view: "Guardar la vista actual",
      save: "Guardar",
      cancel: "Cancelar",
      default_badge: "Predeterminada",
      make_default: "Hacer predeterminada",
      unset_default: "Quitar predeterminada",
      rename: "Renombrar",
      delete: "Eliminar",
      view_saved: "Vista «{name}» guardada.",
      view_updated: "Vista «{name}» actualizada.",
      view_deleted: "Vista «{name}» eliminada.",
      view_applied: "Vista «{name}» aplicada.",
      view_renamed: "Vista renombrada a «{name}».",
      view_limit: "Una pestaña guarda como máximo {n} vistas. Elimine una primero.",
      view_name_required: "Escriba un nombre para la vista.",
      default_set: "«{name}» se abre ahora por defecto.",
      default_cleared: "Ninguna vista se abre ya por defecto.",
      default_hint: "La vista predeterminada se abre con el panel cuando el enlace no indica filtros.",
      columns: "Columnas",
      columns_title: "Columnas y densidad",
      visible_columns: "Columnas visibles",
      density: "Densidad de filas",
      density_comfortable: "Cómoda",
      density_compact: "Compacta",
      show_all_columns: "Mostrar todas las columnas",
      err_control: "No debe contener caracteres de control.",
      err_cron: "Use cinco campos (minuto hora día mes día de la semana) o un descriptor como @daily.",
      err_cron_every: "@every necesita una duración como 30m o 6h.",
      err_integer: "Escriba un número entero.",
      err_path_absolute: "Use una ruta absoluta como /backups.",
      err_url: "Escriba una URL http:// o https://.",
      err_header: "Cada línea debe tener la forma Nombre-Cabecera: valor.",
      err_telegram_token: "Un token de bot tiene la forma 123456789:AAH… (dígitos, dos puntos y al menos 20 caracteres).",
      err_telegram_chat: "Use un ID de chat numérico (como -1001234567890) o @nombrecanal.",
      err_host: "Escriba un nombre de host como smtp.example.com, sin http:// ni puerto.",
      err_email: "Escriba una dirección de correo como alerts@example.com.",
      err_email_list: "Escriba de 1 a 50 direcciones de correo separadas por comas.",
      err_twilio_sid: "Un Account SID es AC seguido de 32 caracteres hexadecimales.",
      err_twilio_from: "Use un número E.164 como +15551234567 o un Messaging Service SID (MG…).",
      err_phone_list: "Escriba de 1 a 20 números en formato E.164 (como +15551234567) separados por comas."
    }
  },
  fr: {
    tv: {
      col_backup: "Sauvegarde",
      col_restore: "Restauration",
      jobs_label: "Filtrer les tâches",
      connections_label: "Filtrer les connexions",
      channels_label: "Filtrer les canaux",
      search_jobs: "Rechercher un nom, un ID ou une base",
      search_connections: "Rechercher un nom, un hôte ou un ID",
      search_channels: "Rechercher un nom ou un ID",
      state: "État",
      state_all: "Tous les états",
      state_enabled: "Planifiée",
      state_paused: "En pause",
      connection_all: "Toutes les connexions",
      schedule: "Fréquence",
      schedule_all: "Toute fréquence",
      schedule_hourly: "Toutes les heures ou plus souvent",
      schedule_daily: "Quotidienne",
      schedule_weekly: "Hebdomadaire",
      schedule_monthly: "Mensuelle",
      schedule_other: "Autres planifications",
      last_all: "Toute dernière exécution",
      last_completed: "Dernière exécution réussie",
      last_failed: "Dernière exécution échouée",
      last_partial: "Dernière exécution en partie échouée",
      last_in_progress: "En cours",
      last_cancelled: "Dernière exécution annulée",
      last_never: "Jamais exécutée",
      test_all: "Tout résultat de test",
      test_ok: "Joignable",
      test_failed: "Test échoué",
      test_untested: "Non testée",
      type_all: "Tous les types",
      enabled_all: "Activés et désactivés",
      no_jobs_match: "Aucune tâche ne correspond à ces filtres.",
      no_connections_match: "Aucune connexion ne correspond à ces filtres.",
      no_channels_match: "Aucun canal ne correspond à ces filtres.",
      filter_failed: "Les tâches n’ont pas pu être filtrées : {error}",
      views: "Vues",
      views_title: "Vues enregistrées",
      views_empty: "Aucune vue enregistrée. Enregistrez les filtres et le tri actuels pour y revenir.",
      views_unavailable: "Les vues enregistrées ont besoin du stockage du navigateur, qui est désactivé.",
      view_name: "Nom de la vue",
      save_view: "Enregistrer la vue actuelle",
      save: "Enregistrer",
      cancel: "Annuler",
      default_badge: "Par défaut",
      make_default: "Définir par défaut",
      unset_default: "Retirer le défaut",
      rename: "Renommer",
      delete: "Supprimer",
      view_saved: "Vue « {name} » enregistrée.",
      view_updated: "Vue « {name} » mise à jour.",
      view_deleted: "Vue « {name} » supprimée.",
      view_applied: "Vue « {name} » appliquée.",
      view_renamed: "Vue renommée en « {name} ».",
      view_limit: "Un onglet garde au plus {n} vues. Supprimez-en une d’abord.",
      view_name_required: "Saisissez un nom pour la vue.",
      default_set: "« {name} » s’ouvre désormais par défaut.",
      default_cleared: "Plus aucune vue ne s’ouvre par défaut.",
      default_hint: "La vue par défaut s’ouvre avec le tableau de bord quand le lien n’indique aucun filtre.",
      columns: "Colonnes",
      columns_title: "Colonnes et densité",
      visible_columns: "Colonnes visibles",
      density: "Densité des lignes",
      density_comfortable: "Confortable",
      density_compact: "Compacte",
      show_all_columns: "Afficher toutes les colonnes",
      err_control: "Ne doit pas contenir de caractères de contrôle.",
      err_cron: "Utilisez cinq champs (minute heure jour mois jour de la semaine) ou un descripteur comme @daily.",
      err_cron_every: "@every demande une durée comme 30m ou 6h.",
      err_integer: "Saisissez un nombre entier.",
      err_path_absolute: "Utilisez un chemin absolu comme /backups.",
      err_url: "Saisissez une URL http:// ou https://.",
      err_header: "Chaque ligne doit avoir la forme Nom-En-Tête: valeur.",
      err_telegram_token: "Un jeton de bot ressemble à 123456789:AAH… (des chiffres, deux-points, puis au moins 20 caractères).",
      err_telegram_chat: "Utilisez un ID de discussion numérique (comme -1001234567890) ou @nomducanal.",
      err_host: "Saisissez un nom d’hôte comme smtp.example.com, sans http:// ni port.",
      err_email: "Saisissez une adresse e-mail comme alerts@example.com.",
      err_email_list: "Saisissez de 1 à 50 adresses e-mail, séparées par des virgules.",
      err_twilio_sid: "Un Account SID est AC suivi de 32 caractères hexadécimaux.",
      err_twilio_from: "Utilisez un numéro E.164 comme +15551234567 ou un Messaging Service SID (MG…).",
      err_phone_list: "Saisissez de 1 à 20 numéros au format E.164 (comme +15551234567), séparés par des virgules."
    }
  },
  zh: {
    tv: {
      col_backup: "备份",
      col_restore: "恢复",
      jobs_label: "筛选任务",
      connections_label: "筛选连接",
      channels_label: "筛选渠道",
      search_jobs: "搜索名称、ID 或数据库",
      search_connections: "搜索名称、主机或 ID",
      search_channels: "搜索名称或 ID",
      state: "状态",
      state_all: "全部状态",
      state_enabled: "已计划",
      state_paused: "已暂停",
      connection_all: "全部连接",
      schedule: "频率",
      schedule_all: "任意频率",
      schedule_hourly: "每小时或更频繁",
      schedule_daily: "每天",
      schedule_weekly: "每周",
      schedule_monthly: "每月",
      schedule_other: "其他计划",
      last_all: "任意上次运行",
      last_completed: "上次运行成功",
      last_failed: "上次运行失败",
      last_partial: "上次运行部分失败",
      last_in_progress: "正在运行",
      last_cancelled: "上次运行已取消",
      last_never: "从未运行",
      test_all: "任意测试结果",
      test_ok: "可连接",
      test_failed: "测试失败",
      test_untested: "未测试",
      type_all: "全部类型",
      enabled_all: "已启用和已停用",
      no_jobs_match: "没有符合这些筛选条件的任务。",
      no_connections_match: "没有符合这些筛选条件的连接。",
      no_channels_match: "没有符合这些筛选条件的渠道。",
      filter_failed: "无法筛选任务：{error}",
      views: "视图",
      views_title: "已保存的视图",
      views_empty: "还没有已保存的视图。保存当前的筛选和排序，以便之后回到这里。",
      views_unavailable: "已保存的视图需要浏览器存储，但存储已关闭。",
      view_name: "视图名称",
      save_view: "保存当前视图",
      save: "保存",
      cancel: "取消",
      default_badge: "默认",
      make_default: "设为默认",
      unset_default: "取消默认",
      rename: "重命名",
      delete: "删除",
      view_saved: "已保存视图“{name}”。",
      view_updated: "已更新视图“{name}”。",
      view_deleted: "已删除视图“{name}”。",
      view_applied: "已应用视图“{name}”。",
      view_renamed: "视图已重命名为“{name}”。",
      view_limit: "每个标签页最多保存 {n} 个视图。请先删除一个。",
      view_name_required: "请输入视图名称。",
      default_set: "现在默认打开“{name}”。",
      default_cleared: "不再默认打开任何视图。",
      default_hint: "当链接未指定筛选条件时，仪表板打开默认视图。",
      columns: "列",
      columns_title: "列和密度",
      visible_columns: "显示的列",
      density: "行密度",
      density_comfortable: "宽松",
      density_compact: "紧凑",
      show_all_columns: "显示所有列",
      err_control: "不能包含控制字符。",
      err_cron: "请使用五个字段（分 时 日 月 星期）或 @daily 等描述符。",
      err_cron_every: "@every 需要时长，例如 30m 或 6h。",
      err_integer: "请输入整数。",
      err_path_absolute: "请使用绝对路径，例如 /backups。",
      err_url: "请输入 http:// 或 https:// 网址。",
      err_header: "每行的格式必须为 Header-Name: value。",
      err_telegram_token: "机器人令牌形如 123456789:AAH…（数字、冒号，然后至少 20 个字符）。",
      err_telegram_chat: "请使用数字聊天 ID（例如 -1001234567890）或 @频道名。",
      err_host: "请输入主机名，例如 smtp.example.com，不要包含 http:// 或端口。",
      err_email: "请输入电子邮件地址，例如 alerts@example.com。",
      err_email_list: "请输入 1 到 50 个电子邮件地址，用逗号分隔。",
      err_twilio_sid: "Account SID 由 AC 加 32 个十六进制字符组成。",
      err_twilio_from: "请使用 E.164 号码（例如 +15551234567）或 Messaging Service SID（MG…）。",
      err_phone_list: "请输入 1 到 20 个 E.164 格式的号码（例如 +15551234567），用逗号分隔。"
    }
  },
  ja: {
    tv: {
      col_backup: "バックアップ",
      col_restore: "リストア",
      jobs_label: "ジョブを絞り込む",
      connections_label: "接続を絞り込む",
      channels_label: "チャネルを絞り込む",
      search_jobs: "名前、ID、データベースを検索",
      search_connections: "名前、ホスト、ID を検索",
      search_channels: "名前または ID を検索",
      state: "状態",
      state_all: "すべての状態",
      state_enabled: "スケジュール済み",
      state_paused: "一時停止中",
      connection_all: "すべての接続",
      schedule: "頻度",
      schedule_all: "すべての頻度",
      schedule_hourly: "毎時またはそれ以上",
      schedule_daily: "毎日",
      schedule_weekly: "毎週",
      schedule_monthly: "毎月",
      schedule_other: "その他のスケジュール",
      last_all: "すべての前回実行",
      last_completed: "前回の実行が成功",
      last_failed: "前回の実行が失敗",
      last_partial: "前回の実行が一部失敗",
      last_in_progress: "実行中",
      last_cancelled: "前回の実行がキャンセル",
      last_never: "未実行",
      test_all: "すべてのテスト結果",
      test_ok: "到達可能",
      test_failed: "テスト失敗",
      test_untested: "未テスト",
      type_all: "すべての種類",
      enabled_all: "有効と無効",
      no_jobs_match: "条件に一致するジョブはありません。",
      no_connections_match: "条件に一致する接続はありません。",
      no_channels_match: "条件に一致するチャネルはありません。",
      filter_failed: "ジョブを絞り込めませんでした: {error}",
      views: "ビュー",
      views_title: "保存したビュー",
      views_empty: "保存したビューはまだありません。現在の絞り込みと並べ替えを保存して、あとで戻れます。",
      views_unavailable: "保存したビューにはブラウザのストレージが必要ですが、無効になっています。",
      view_name: "ビュー名",
      save_view: "現在のビューを保存",
      save: "保存",
      cancel: "キャンセル",
      default_badge: "既定",
      make_default: "既定にする",
      unset_default: "既定を解除",
      rename: "名前を変更",
      delete: "削除",
      view_saved: "ビュー「{name}」を保存しました。",
      view_updated: "ビュー「{name}」を更新しました。",
      view_deleted: "ビュー「{name}」を削除しました。",
      view_applied: "ビュー「{name}」を適用しました。",
      view_renamed: "ビューの名前を「{name}」に変更しました。",
      view_limit: "1 つのタブに保存できるビューは最大 {n} 個です。先に 1 つ削除してください。",
      view_name_required: "ビューの名前を入力してください。",
      default_set: "「{name}」が既定で開くようになりました。",
      default_cleared: "既定で開くビューはなくなりました。",
      default_hint: "リンクに絞り込みが含まれないとき、ダッシュボードは既定のビューで開きます。",
      columns: "列",
      columns_title: "列と密度",
      visible_columns: "表示する列",
      density: "行の密度",
      density_comfortable: "ゆったり",
      density_compact: "コンパクト",
      show_all_columns: "すべての列を表示",
      err_control: "制御文字は使えません。",
      err_cron: "5 つのフィールド（分 時 日 月 曜日）または @daily などの記述子を使ってください。",
      err_cron_every: "@every には 30m や 6h などの期間が必要です。",
      err_integer: "整数を入力してください。",
      err_path_absolute: "/backups のような絶対パスを使ってください。",
      err_url: "http:// または https:// の URL を入力してください。",
      err_header: "各行は Header-Name: value の形式にしてください。",
      err_telegram_token: "ボットトークンは 123456789:AAH… の形式です（数字、コロン、20 文字以上）。",
      err_telegram_chat: "数値のチャット ID（-1001234567890 など）または @チャンネル名を使ってください。",
      err_host: "http:// やポートを付けずに smtp.example.com のようなホスト名を入力してください。",
      err_email: "alerts@example.com のようなメールアドレスを入力してください。",
      err_email_list: "メールアドレスを 1〜50 個、カンマ区切りで入力してください。",
      err_twilio_sid: "Account SID は AC と 32 文字の 16 進数です。",
      err_twilio_from: "+15551234567 のような E.164 番号または Messaging Service SID（MG…）を使ってください。",
      err_phone_list: "E.164 形式（+15551234567 など）の番号を 1〜20 個、カンマ区切りで入力してください。"
    }
  },
  ru: {
    tv: {
      col_backup: "Копия",
      col_restore: "Восстановление",
      jobs_label: "Фильтр заданий",
      connections_label: "Фильтр подключений",
      channels_label: "Фильтр каналов",
      search_jobs: "Поиск по имени, ID или базе",
      search_connections: "Поиск по имени, хосту или ID",
      search_channels: "Поиск по имени или ID",
      state: "Состояние",
      state_all: "Все состояния",
      state_enabled: "По расписанию",
      state_paused: "Приостановлено",
      connection_all: "Все подключения",
      schedule: "Частота",
      schedule_all: "Любая частота",
      schedule_hourly: "Ежечасно или чаще",
      schedule_daily: "Ежедневно",
      schedule_weekly: "Еженедельно",
      schedule_monthly: "Ежемесячно",
      schedule_other: "Другие расписания",
      last_all: "Любой последний запуск",
      last_completed: "Последний запуск успешен",
      last_failed: "Последний запуск с ошибкой",
      last_partial: "Последний запуск частично с ошибкой",
      last_in_progress: "Выполняется",
      last_cancelled: "Последний запуск отменён",
      last_never: "Ни разу не запускалось",
      test_all: "Любой результат проверки",
      test_ok: "Доступно",
      test_failed: "Проверка не пройдена",
      test_untested: "Не проверялось",
      type_all: "Все типы",
      enabled_all: "Включённые и выключенные",
      no_jobs_match: "Нет заданий, подходящих под фильтры.",
      no_connections_match: "Нет подключений, подходящих под фильтры.",
      no_channels_match: "Нет каналов, подходящих под фильтры.",
      filter_failed: "Не удалось отфильтровать задания: {error}",
      views: "Виды",
      views_title: "Сохранённые виды",
      views_empty: "Сохранённых видов пока нет. Сохраните текущие фильтры и сортировку, чтобы вернуться к ним.",
      views_unavailable: "Сохранённым видам нужно хранилище браузера, а оно отключено.",
      view_name: "Название вида",
      save_view: "Сохранить текущий вид",
      save: "Сохранить",
      cancel: "Отмена",
      default_badge: "По умолчанию",
      make_default: "Сделать по умолчанию",
      unset_default: "Снять умолчание",
      rename: "Переименовать",
      delete: "Удалить",
      view_saved: "Вид «{name}» сохранён.",
      view_updated: "Вид «{name}» обновлён.",
      view_deleted: "Вид «{name}» удалён.",
      view_applied: "Вид «{name}» применён.",
      view_renamed: "Вид переименован в «{name}».",
      view_limit: "Во вкладке хранится не больше {n} видов. Сначала удалите один.",
      view_name_required: "Введите название вида.",
      default_set: "Теперь по умолчанию открывается «{name}».",
      default_cleared: "Вид по умолчанию больше не задан.",
      default_hint: "Вид по умолчанию открывается вместе с панелью, если в ссылке нет фильтров.",
      columns: "Столбцы",
      columns_title: "Столбцы и плотность",
      visible_columns: "Видимые столбцы",
      density: "Плотность строк",
      density_comfortable: "Свободно",
      density_compact: "Компактно",
      show_all_columns: "Показать все столбцы",
      err_control: "Не должно содержать управляющих символов.",
      err_cron: "Используйте пять полей (минута час день месяц день недели) или дескриптор вроде @daily.",
      err_cron_every: "Для @every нужна длительность, например 30m или 6h.",
      err_integer: "Введите целое число.",
      err_path_absolute: "Укажите абсолютный путь, например /backups.",
      err_url: "Введите адрес http:// или https://.",
      err_header: "Каждая строка должна выглядеть как Имя-Заголовка: значение.",
      err_telegram_token: "Токен бота выглядит как 123456789:AAH… (цифры, двоеточие и не меньше 20 символов).",
      err_telegram_chat: "Укажите числовой ID чата (например -1001234567890) или @имяканала.",
      err_host: "Введите имя хоста, например smtp.example.com, без http:// и порта.",
      err_email: "Введите адрес почты, например alerts@example.com.",
      err_email_list: "Введите от 1 до 50 адресов почты через запятую.",
      err_twilio_sid: "Account SID — это AC и 32 шестнадцатеричных символа.",
      err_twilio_from: "Укажите номер E.164, например +15551234567, или Messaging Service SID (MG…).",
      err_phone_list: "Введите от 1 до 20 номеров в формате E.164 (например +15551234567) через запятую."
    }
  }
};

(function mergeTablesTranslations() {
  if (typeof translations !== "object" || !translations) return;
  Object.keys(TABLES_I18N).forEach(lang => {
    if (!translations[lang]) translations[lang] = {};
    const target = translations[lang];
    Object.keys(TABLES_I18N[lang]).forEach(ns => {
      target[ns] = Object.assign({}, TABLES_I18N[lang][ns], target[ns] || {});
    });
  });
})();

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Every managed table. tbody is its body's id; tab names its pane in the URL hash
// (main tables only: their filters and sort live in the hash and in saved views);
// server marks Backups and Restores (sorted by the API, filtered by app.js); data
// marks the Jobs table, sorted from its data before rendering; tools adds the
// views, columns and density controls (into bar, the filter bar). cols maps header
// cells (by their data-i18n key) to columns: key names the column (sort value,
// column toggle, hash), sort makes it sortable, dir is the first direction, fixed
// columns cannot be hidden. defaultSort is the order rows arrive in.
const TV_TABLES = {
  backups: {
    tbody: "backups-tbody", tab: "backups", server: true, tools: true, bar: "#tab-backups .filter-bar", defaultSort: "-started_at",
    cols: [
      { i18n: "tv.col_backup", key: "database", sort: true, dir: "asc", fixed: true },
      { i18n: "tables.status", key: "status", sort: true, dir: "asc" },
      { i18n: "tables.started_at", key: "started_at", sort: true, dir: "desc" },
      { i18n: "tables.duration", key: "duration", sort: true, dir: "desc" },
      { i18n: "tables.size", key: "size", sort: true, dir: "desc" },
      { i18n: "tables.sha256", key: "sha256" }
    ]
  },
  restores: {
    tbody: "restores-tbody", tab: "restores", server: true, tools: true, bar: "#tab-restores .filter-bar", defaultSort: "-started_at",
    cols: [
      { i18n: "tv.col_restore", key: "database", sort: true, dir: "asc", fixed: true },
      { i18n: "tables.source_db", key: "source" },
      { i18n: "tables.status", key: "status", sort: true, dir: "asc" },
      { i18n: "tables.started_at", key: "started_at", sort: true, dir: "desc" },
      { i18n: "tables.duration", key: "duration", sort: true, dir: "desc" }
    ]
  },
  jobs: {
    tbody: "jobs-tbody", tab: "jobs", data: true, tools: true, bar: '[data-tv-filters="jobs"]', defaultSort: "name",
    cols: [
      { i18n: "tables.job_name", key: "name", sort: true, dir: "asc", fixed: true },
      { i18n: "tables.connection", key: "connection", sort: true, dir: "asc" },
      { i18n: "tables.database", key: "database", sort: true, dir: "asc" },
      { i18n: "tables.schedule", key: "schedule", sort: true, dir: "asc" },
      { i18n: "tables.retention_policy", key: "retention" },
      { i18n: "tables.status", key: "status", sort: true, dir: "asc" },
      { i18n: "tables.last_run", key: "last_run", sort: true, dir: "asc" },
      { i18n: "tables.next_run", key: "next_run", sort: true, dir: "asc" }
    ]
  },
  connections: {
    tbody: "connections-tbody", tab: "connections", tools: true, bar: '[data-tv-filters="connections"]', defaultSort: "name",
    cols: [
      { i18n: "conn.col_name", key: "name", sort: true, dir: "asc", fixed: true },
      { i18n: "conn.col_host", key: "host", sort: true, dir: "asc" },
      { i18n: "conn.col_status", key: "test", sort: true, dir: "asc" },
      { i18n: "conn.col_version", key: "version", sort: true, dir: "desc" },
      { i18n: "conn.col_tested", key: "tested", sort: true, dir: "desc" }
    ]
  },
  channels: {
    tbody: "channels-tbody", tab: "notifications", tools: true, bar: '[data-tv-filters="channels"]', defaultSort: "name",
    cols: [
      { i18n: "notify.col_name", key: "name", sort: true, dir: "asc", fixed: true },
      { i18n: "notify.col_type", key: "type", sort: true, dir: "asc" },
      { i18n: "notify.col_enabled", key: "enabled", sort: true, dir: "asc" },
      { i18n: "notify.col_last_delivery", key: "delivery", sort: true, dir: "desc" }
    ]
  },
  rules: {
    tbody: "rules-tbody", defaultSort: "name",
    cols: [
      { i18n: "notify.col_name", key: "name", sort: true, dir: "asc" },
      { i18n: "notify.col_jobs", key: "jobs", sort: true, dir: "asc" },
      { i18n: "notify.col_channels", key: "channels", sort: true, dir: "asc" },
      { i18n: "notify.col_enabled", key: "enabled", sort: true, dir: "asc" }
    ]
  },
  storage: {
    tbody: "storage-tbody", defaultSort: "",
    cols: [
      { i18n: "storage.col_name", key: "name", sort: true, dir: "asc" },
      { i18n: "storage.col_type", key: "type", sort: true, dir: "asc" },
      { i18n: "storage.col_location", key: "location", sort: true, dir: "asc" },
      { i18n: "storage.col_status", key: "test", sort: true, dir: "asc" }
    ]
  },
  users: {
    tbody: "users-tbody", defaultSort: "username",
    cols: [
      { i18n: "settings.col_username", key: "username", sort: true, dir: "asc" },
      { i18n: "settings.col_created", key: "created", sort: true, dir: "desc" },
      { i18n: "settings.col_last_login", key: "last_login", sort: true, dir: "desc" }
    ]
  },
  apikeys: {
    tbody: "apikeys-tbody", defaultSort: "",
    cols: [
      { i18n: "settings.col_key_name", key: "name", sort: true, dir: "asc" },
      { i18n: "settings.col_scope", key: "scope", sort: true, dir: "asc" },
      { i18n: "settings.col_created_by", key: "created_by", sort: true, dir: "asc" },
      { i18n: "settings.col_created", key: "created", sort: true, dir: "desc" },
      { i18n: "settings.col_last_used", key: "last_used", sort: true, dir: "desc" }
    ]
  },
  sessions: {
    tbody: "sessions-tbody", defaultSort: "",
    cols: [
      { i18n: "settings.col_username", key: "username", sort: true, dir: "asc" },
      { i18n: "sessions.col_signed_in", key: "signed_in", sort: true, dir: "desc" },
      { i18n: "sessions.col_last_active", key: "last_active", sort: true, dir: "desc" },
      { i18n: "sessions.col_expires", key: "expires", sort: true, dir: "asc" }
    ]
  },
  audit: {
    tbody: "audit-tbody", defaultSort: "",
    cols: [
      { i18n: "settings.col_time", key: "time", sort: true, dir: "desc" },
      { i18n: "settings.col_api_key", key: "api_key", sort: true, dir: "asc" },
      { i18n: "settings.col_tool", key: "tool", sort: true, dir: "asc" },
      { i18n: "settings.col_result", key: "result", sort: true, dir: "asc" },
      { i18n: "settings.col_transport", key: "transport", sort: true, dir: "asc" }
    ]
  },
  retired: {
    tbody: "retired-keys-tbody", defaultSort: "",
    cols: [
      { i18n: "enc.col_recipient", key: "recipient", sort: true, dir: "asc" },
      { i18n: "enc.col_retired", key: "retired", sort: true, dir: "desc" }
    ]
  }
};

// tvName returns value when it names a managed table (a literal), "" otherwise.
// Table names come from data attributes and the URL; this keeps them to the list.
function tvName(value) {
  switch (value) {
    case "backups": return "backups";
    case "restores": return "restores";
    case "jobs": return "jobs";
    case "connections": return "connections";
    case "channels": return "channels";
    case "rules": return "rules";
    case "storage": return "storage";
    case "users": return "users";
    case "apikeys": return "apikeys";
    case "sessions": return "sessions";
    case "audit": return "audit";
    case "retired": return "retired";
    default: return "";
  }
}

// The main table of a tab of the URL hash (#jobs, #notifications, ...), or "".
function tvTabTable(tab) {
  switch (tab) {
    case "backups": return "backups";
    case "restores": return "restores";
    case "jobs": return "jobs";
    case "connections": return "connections";
    case "notifications": return "channels";
    default: return "";
  }
}

// Client-side filters of the Jobs, Connections and Channels tables, in hash order:
// "text" values are free (capped), arrays list the accepted values.
const TV_FILTERS = {
  jobs: {
    q: "text", status: ["enabled", "paused"], connection: "text", database: "text",
    schedule: ["hourly", "daily", "weekly", "monthly", "other"],
    last: ["completed", "failed", "partial", "in_progress", "cancelled", "never"]
  },
  connections: { q: "text", test: ["ok", "failed", "untested"] },
  channels: { q: "text", type: ["webhook", "telegram", "email", "twilio"], enabled: ["enabled", "disabled"] }
};
const TV_TEXT_MAX = 256;
const TV_DEBOUNCE_MS = 300;
const TV_VIEWS_MAX = 20;
const TV_VIEW_NAME_MAX = 60;
const TV_VIEW_QS_MAX = 2000;
const TV_SKELETON_ROWS = 6;
const TV_SORT_ICON = '<svg class="th-sort-icon" viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path class="th-up" d="M5 6.5 8 3.5l3 3"/><path class="th-down" d="M5 9.5 8 12.5l3-3"/></svg>';

const tv = {
  // Filter values of the client-filtered tables.
  filters: {},
  // Sort of the client-sorted tables ("" = defaultSort; "-key" = descending).
  sort: {},
  // Column visibility and density per table.
  prefs: {},
  // Header cell index (1-based) of every column key, per table.
  index: {},
  // Debounce timers of the search fields.
  timers: {},
  // The open popover: { name, kind } or null; the view being renamed.
  pop: null,
  renaming: null
};
Object.keys(TV_FILTERS).forEach(name => { tv.filters[name] = tvEmptyFilters(name); });

// The server-filtered jobs list: the IDs GET /api/v1/jobs returned for key.
const tvJobs = { key: null, ids: new Set(), error: "", seq: 0, inflight: "", source: null };

function tvEmptyFilters(name) {
  const f = {};
  Object.keys(TV_FILTERS[name] || {}).forEach(k => { f[k] = ""; });
  return f;
}

function tvTable(name) {
  const tbody = document.getElementById(TV_TABLES[name].tbody);
  return tbody ? tbody.closest("table") : null;
}

// ---------------------------------------------------------------------------
// URL hash, sort values and filter state
// ---------------------------------------------------------------------------

// tablesValidSort returns v when it names a sortable column of table name ("key" or
// "-key"), normalized to "" for the table's default order, and "" otherwise.
function tablesValidSort(name, v) {
  const T = TV_TABLES[tvName(name)];
  if (!T) return "";
  const s = String(v || "");
  const desc = s.startsWith("-");
  const key = desc ? s.slice(1) : s;
  if (!T.cols.some(c => c.key === key && c.sort)) return "";
  const out = (desc ? "-" : "") + key;
  return out === T.defaultSort ? "" : out;
}

function tvSortOf(name) {
  const T = TV_TABLES[name];
  if (T.server) return lists[name].sort || "";
  return tv.sort[name] || "";
}

// Reads the filters and sort of table name from params; reports a change. Unknown
// or malformed values are dropped.
function tvApplyParams(name, params) {
  const spec = TV_FILTERS[name] || {};
  const next = tvEmptyFilters(name);
  Object.keys(spec).forEach(k => {
    const v = String(params.get(k) || "").slice(0, TV_TEXT_MAX);
    if (spec[k] === "text" || spec[k].includes(v)) next[k] = v;
  });
  const sort = tablesValidSort(name, params.get("sort"));
  const cur = tv.filters[name] || {};
  const changed = sort !== (tv.sort[name] || "") || Object.keys(spec).some(k => next[k] !== cur[k]);
  tv.filters[name] = next;
  tv.sort[name] = sort;
  return changed;
}

// app.js hook: hash parameters of tab (other than Backups and Restores).
function tablesApplyHash(tab, params) {
  const name = tvTabTable(tab);
  if (!name || TV_TABLES[name].server) return false;
  return tvApplyParams(name, params);
}

// The query string (filters and sort, no page) of table name.
function tvQuery(name) {
  const T = TV_TABLES[name];
  if (T.server) {
    const p = new URLSearchParams(listHashParams(name));
    p.delete("page");
    return p.toString();
  }
  const p = new URLSearchParams();
  const f = tv.filters[name] || {};
  Object.keys(TV_FILTERS[name] || {}).forEach(k => { if (f[k]) p.set(k, f[k]); });
  if (tv.sort[name]) p.set("sort", tv.sort[name]);
  return p.toString();
}

// app.js hook: the hash query of tab (other than Backups and Restores).
function tablesHashParams(tab) {
  const name = tvTabTable(tab);
  return name && !TV_TABLES[name].server ? tvQuery(name) : "";
}

// app.js hook: shows the state of tab after a hash change.
function tablesRefresh(tab) {
  const name = tvTabTable(tab);
  if (!name) return;
  tvRenderControls(name, true);
  tvRenderHeaders(name);
  tvRerender(name);
}

function tablesActiveCount(name) {
  const f = tv.filters[tvName(name)];
  if (!f) return 0;
  return Object.keys(f).filter(k => (k === "q" ? f[k].trim() : f[k])).length;
}

// The GET /api/v1/jobs query of the Jobs filters ("" without filters).
function tvJobsQuery() {
  const f = tv.filters.jobs;
  const p = new URLSearchParams();
  if (f.q.trim()) p.set("q", f.q.trim());
  if (f.status) p.set("enabled", f.status === "enabled" ? "true" : "false");
  if (f.connection) p.set("connection_id", f.connection);
  if (f.database) p.set("database", f.database);
  if (f.schedule) p.set("schedule", f.schedule);
  if (f.last) p.set("last_status", f.last);
  return p.toString();
}

// bulk.js hooks: the filter state a jobs selection is made under, the number of
// matching jobs and the bulk API filter (the same fields as the list query).
function tablesFilterKey(name) {
  return tvName(name) === "jobs" ? tvJobsQuery() : "";
}

function tablesTotal(name) {
  if (tvName(name) !== "jobs") return 0;
  const key = tvJobsQuery();
  if (!key) return state.jobs.length;
  if (tvJobs.key !== key || tvJobs.error) return 0;
  return state.jobs.filter(j => tvJobs.ids.has(j.id)).length;
}

function tablesApiFilter(name) {
  if (tvName(name) !== "jobs") return {};
  const out = {};
  new URLSearchParams(tvJobsQuery()).forEach((v, k) => {
    out[k] = k === "enabled" ? v === "true" : v;
  });
  return out;
}

// ---------------------------------------------------------------------------
// Rows: filtering and data sorting (app.js render hooks)
// ---------------------------------------------------------------------------

function tvLower(v) {
  return String(v === null || v === undefined ? "" : v).toLowerCase();
}

function tvConnectionTest(c) {
  if (!parseDate(c.last_test_at)) return "untested";
  return c.last_test_ok ? "ok" : "failed";
}

// app.js hook: the rows of table name to render, filtered and (Jobs) sorted. The
// Jobs table returns null while its server-side filter is loading.
function tablesRows(name, items) {
  const list = Array.isArray(items) ? items : [];
  if (name === "jobs") {
    tvJobOptions();
    const key = tvJobsQuery();
    let rows = list;
    if (key) {
      if (tvJobs.key !== key) {
        if (tvJobs.inflight !== key) tvFetchJobs(key);
        return null;
      }
      // The jobs were reloaded: ask again (last runs change), keep showing these.
      if (tvJobs.source !== list && tvJobs.inflight !== key) tvFetchJobs(key);
      rows = tvJobs.error ? [] : list.filter(j => tvJobs.ids.has(j.id));
    }
    return tvSortJobs(rows);
  }
  if (name === "connections") {
    const f = tv.filters.connections;
    const q = f.q.trim().toLowerCase();
    return list.filter(c => (!q || [c.name, c.id, c.description, maskedHost(c.uri)].some(v => tvLower(v).includes(q))) &&
      (!f.test || tvConnectionTest(c) === f.test));
  }
  if (name === "channels") {
    const f = tv.filters.channels;
    const q = f.q.trim().toLowerCase();
    return list.filter(ch => (!q || [ch.name, ch.id, channelTypeLabel(ch.type)].some(v => tvLower(v).includes(q))) &&
      (!f.type || ch.type === f.type) &&
      (!f.enabled || (ch.enabled ? "enabled" : "disabled") === f.enabled));
  }
  return list;
}

function tvFetchJobs(key) {
  if (!auth.user) return;
  const seq = ++tvJobs.seq;
  const source = state.jobs;
  tvJobs.inflight = key;
  const done = (ids, error) => {
    if (seq !== tvJobs.seq) return;
    tvJobs.inflight = "";
    tvJobs.key = key;
    tvJobs.source = source;
    tvJobs.ids = ids;
    tvJobs.error = error;
    renderJobs();
  };
  apiJSON(`/api/v1/jobs?${key}`)
    .then(json => {
      if (json && json.success) {
        done(new Set((Array.isArray(json.data) ? json.data : []).map(j => j && j.id).filter(Boolean)), "");
      } else {
        done(new Set(), (json && json.error) || t("filters.load_failed"));
      }
    })
    .catch(() => done(new Set(), t("filters.load_failed")));
}

// Rank of a job's last outcome for sorting by last run: failures first.
const TV_RUN_RANK = { failed: 0, partial: 1, cancelled: 2, running: 3, ok: 4 };

// The last outcome of job: its last run's for jobs with several databases
// (jobdbs.js), its newest backup's otherwise; "" when it never ran.
function tvJobLastStatus(job) {
  if (typeof jobLastRun === "function" && typeof jobIsMulti === "function" && jobIsMulti(job)) {
    const run = jobLastRun(job);
    return run ? String(run.status || "") : "";
  }
  const b = state.stats && state.stats.job_last_backups ? state.stats.job_last_backups[job.id] : null;
  if (!b) return "";
  switch (b.status) {
    case "failed": return "failed";
    case "cancelled": return "cancelled";
    case "pending":
    case "in_progress": return "running";
    default: return "ok";
  }
}

function tvTime(value) {
  const d = parseDate(value);
  return d ? d.getTime() : null;
}

// Sort values of the Jobs columns; null sorts last in both directions.
const TV_JOB_SORT = {
  name: j => j.name || j.id,
  connection: j => (j.connection_id ? connectionName(j.connection_id) : null),
  // The selection summary of jobs with several databases (jobdbs.js).
  database: j => (typeof jobDatabaseSummary === "function" ? jobDatabaseSummary(j) : j.database) || null,
  schedule: j => j.cron_expression || null,
  status: j => (j.enabled !== false ? 0 : 1),
  // The run status first, then the newest run first.
  last_run: j => {
    const s = tvJobLastStatus(j);
    if (!s) return null;
    const rank = Object.prototype.hasOwnProperty.call(TV_RUN_RANK, s) ? TV_RUN_RANK[s] : 5;
    return [rank, -(tvTime(j.last_run) || 0)];
  },
  next_run: j => (j.enabled !== false ? tvTime(j.next_run) : null)
};

function tvSortJobs(rows) {
  const sort = tvSortOf("jobs");
  if (!sort) return rows;
  const desc = sort.startsWith("-");
  const fn = TV_JOB_SORT[desc ? sort.slice(1) : sort];
  if (!fn) return rows;
  const collator = tvCollator();
  return rows.map((j, i) => ({ j, i, v: fn(j) }))
    .sort((a, b) => tvCompare(a.v, b.v, desc, collator) || a.i - b.i)
    .map(x => x.j);
}

function tvCollator() {
  try {
    return new Intl.Collator(uiLocale(), { numeric: true, sensitivity: "base" });
  } catch (err) {
    return new Intl.Collator(undefined, { numeric: true });
  }
}

// Compares two sort values (numbers, strings or arrays of them); null is last.
function tvCompare(a, b, desc, collator) {
  if (a === null && b === null) return 0;
  if (a === null) return 1;
  if (b === null) return -1;
  let c;
  if (Array.isArray(a) && Array.isArray(b)) {
    c = 0;
    for (let i = 0; i < Math.max(a.length, b.length) && c === 0; i++) c = tvCompare(a[i] ?? null, b[i] ?? null, false, collator);
  } else if (typeof a === "number" && typeof b === "number") {
    c = a - b;
  } else {
    c = collator.compare(String(a), String(b));
  }
  return desc ? -c : c;
}

// app.js hook: the row shown when filters leave table name empty.
function tablesNoMatch(name) {
  const key = tvName(name);
  const table = key ? tvTable(key) : null;
  const cols = table && table.tHead ? table.tHead.rows[0].cells.length : 1;
  const msg = key === "jobs" && tvJobs.error ? tf("tv.filter_failed", { error: tvJobs.error }) : t(`tv.no_${key}_match`);
  return `<tr class="empty-row"><td colspan="${Number(cols) || 1}"><div class="empty-state"><p>${escapeHtml(msg)}</p>`
    + `<button type="button" class="btn btn-secondary btn-sm" data-tv="clear" data-tv-table="${escapeHtml(key)}">${escapeHtml(t("filters.clear"))}</button></div></td></tr>`;
}

function tvRerender(name) {
  switch (name) {
    case "jobs":
      renderJobs();
      break;
    case "connections":
      renderConnections();
      break;
    case "channels":
      renderChannels();
      break;
    case "backups":
    case "restores":
      if (auth.user) loadList(name);
      break;
    default:
      tvDomSort(name);
  }
}

// ---------------------------------------------------------------------------
// Skeletons and render hooks
// ---------------------------------------------------------------------------

// Placeholder rows shaped like table's columns, with a screen reader "Loading…".
function tvSkeletonHtml(table, rows) {
  const ths = table && table.tHead && table.tHead.rows[0] ? Array.from(table.tHead.rows[0].cells) : [];
  if (ths.length === 0) return "";
  const firstData = Math.max(0, ths.findIndex(th => !th.classList.contains("col-select")));
  let html = "";
  for (let r = 0; r < rows; r++) {
    const cells = ths.map((th, i) => {
      const cls = ["col-select", "col-actions", "num"].filter(c => th.classList.contains(c)).join(" ");
      const sr = r === 0 && i === firstData ? `<span class="sr-only">${escapeHtml(t("tables.loading"))}</span>` : "";
      const bar = th.classList.contains("col-select") ? "" : `<span class="skel skel-w${((r + i) % 4) + 1}" aria-hidden="true"></span>`;
      return `<td${cls ? ` class="${cls}"` : ""}>${sr}${bar}</td>`;
    });
    html += `<tr class="tv-skel">${cells.join("")}</tr>`;
  }
  return html;
}

// app.js hooks: skeleton rows of table name, or of the table of tbody.
function tablesSkeleton(name) {
  const key = tvName(name);
  return key ? tvSkeletonHtml(tvTable(key), TV_SKELETON_ROWS) : "";
}

function tablesSkeletonFor(tbody) {
  const table = tbody.closest("table");
  const name = tvNameOfTbody(tbody);
  return tvSkeletonHtml(table, name && TV_TABLES[name].tab ? TV_SKELETON_ROWS : 3);
}

function tvNameOfTbody(tbody) {
  const id = tbody ? tbody.id : "";
  return Object.keys(TV_TABLES).find(n => TV_TABLES[n].tbody === id) || "";
}

function tvMarkBusy(tbody) {
  const table = tbody ? tbody.closest("table") : null;
  if (table) table.setAttribute("aria-busy", String(!!tbody.querySelector("tr.tv-skel")));
}

// app.js hook, after every re-render of a table body.
function tablesRendered(tbody) {
  tvMarkBusy(tbody);
  const name = tvNameOfTbody(tbody);
  if (name) tvDomSort(name);
}

// app.js hook: signing out forgets the server-side jobs filter result.
function tablesReset() {
  tvJobs.seq++;
  tvJobs.key = null;
  tvJobs.ids = new Set();
  tvJobs.error = "";
  tvJobs.inflight = "";
  tvJobs.source = null;
  document.querySelectorAll("#app-main tbody").forEach(tvMarkBusy);
}

// ---------------------------------------------------------------------------
// Sorting: headers and rendered rows
// ---------------------------------------------------------------------------

// Turns the sortable header cells of table name into sort buttons and records the
// column indices.
function tvSetupHeaders(name) {
  const T = TV_TABLES[name];
  const table = tvTable(name);
  if (!table || !table.tHead) return;
  table.id = table.id || `tv-${name}`;
  const row = table.tHead.rows[0];
  const ths = Array.from(row.cells);
  tv.index[name] = {};
  T.cols.forEach(col => {
    const th = ths.find(x => x.getAttribute("data-i18n") === col.i18n);
    if (!th) return;
    tv.index[name][col.key] = ths.indexOf(th) + 1;
    th.dataset.col = col.key;
    if (!col.sort) return;
    const label = document.createElement("span");
    label.setAttribute("data-i18n", col.i18n);
    label.textContent = th.textContent;
    th.removeAttribute("data-i18n");
    th.textContent = "";
    th.classList.add("th-sortable");
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "th-sort";
    btn.dataset.tv = "sort";
    btn.dataset.tvTable = name;
    btn.dataset.col = col.key;
    btn.appendChild(label);
    btn.insertAdjacentHTML("beforeend", TV_SORT_ICON);
    th.appendChild(btn);
  });
}

// Shows the current sort of table name on its headers (aria-sort on one of them).
function tvRenderHeaders(name) {
  const T = TV_TABLES[name];
  const table = tvTable(name);
  if (!table || !table.tHead) return;
  const sort = tvSortOf(name) || T.defaultSort;
  const desc = sort.startsWith("-");
  const key = desc ? sort.slice(1) : sort;
  table.tHead.querySelectorAll("th.th-sortable").forEach(th => {
    if (th.dataset.col === key) {
      th.setAttribute("aria-sort", desc ? "descending" : "ascending");
    } else {
      th.removeAttribute("aria-sort");
    }
  });
}

// The next sort of table name when the header of key is activated: the other
// direction of the current column, or the column's first direction.
function tvNextSort(name, key) {
  const T = TV_TABLES[name];
  const col = T.cols.find(c => c.key === key && c.sort);
  if (!col) return null;
  const cur = tvSortOf(name) || T.defaultSort;
  if (cur === key) return `-${key}`;
  if (cur === `-${key}`) return key;
  return col.dir === "desc" ? `-${key}` : key;
}

function tvSetSort(name, key) {
  const next = tvNextSort(name, key);
  if (next === null) return;
  const sort = tablesValidSort(name, next);
  const T = TV_TABLES[name];
  if (T.server) {
    const L = lists[name];
    L.sort = sort;
    L.page = 1;
    writeHash(true);
    tvRenderHeaders(name);
    if (auth.user) loadList(name);
    return;
  }
  tv.sort[name] = sort;
  if (T.tab) {
    writeHash(true);
  } else {
    tvSavePrefs(name);
  }
  tvRenderHeaders(name);
  if (name === "jobs") {
    renderJobs();
  } else {
    tvDomSort(name);
  }
}

// The sort value of a rendered cell: its data-sort-value, its time, or its text.
function tvCellValue(td) {
  if (!td) return null;
  const holder = td.hasAttribute("data-sort-value") ? td : td.querySelector("[data-sort-value]");
  if (holder) {
    const v = holder.getAttribute("data-sort-value") || "";
    const n = Number(v);
    return v !== "" && Number.isFinite(n) ? n : v;
  }
  const time = td.querySelector("time[datetime]");
  if (time) {
    const ms = Date.parse(time.getAttribute("datetime") || "");
    if (Number.isFinite(ms)) return ms;
  }
  const text = (td.textContent || "").replace(/\s+/g, " ").trim();
  return text && text !== "—" ? text : null;
}

// Orders the rendered rows of a client-sorted table; the default sort restores the
// order they were rendered in.
function tvDomSort(name) {
  const T = TV_TABLES[name];
  if (!T || T.server || T.data) return;
  const tbody = document.getElementById(T.tbody);
  if (!tbody) return;
  const rows = Array.from(tbody.rows).filter(r => !r.classList.contains("empty-row") && !r.classList.contains("tv-skel"));
  rows.forEach((r, i) => { if (r.dataset.tvI === undefined) r.dataset.tvI = String(i); });
  if (rows.length < 2) return;
  const sort = tvSortOf(name);
  const desc = sort.startsWith("-");
  const idx = sort ? (tv.index[name] || {})[desc ? sort.slice(1) : sort] || 0 : 0;
  const collator = tvCollator();
  const sorted = rows.map(r => ({ r, i: Number(r.dataset.tvI), v: idx ? tvCellValue(r.cells[idx - 1]) : null }))
    .sort((a, b) => (idx ? tvCompare(a.v, b.v, desc, collator) : 0) || a.i - b.i);
  if (sorted.every((x, k) => x.r === rows[k])) return;
  const focused = tbody.contains(document.activeElement) ? document.activeElement : null;
  sorted.forEach(x => tbody.appendChild(x.r));
  if (focused && document.contains(focused)) focused.focus();
}

// ---------------------------------------------------------------------------
// Column visibility and density (localStorage)
// ---------------------------------------------------------------------------

function tvPrefKey(name) {
  return `mongorescue_table_${name}`;
}

function tvLoadPrefs(name) {
  const T = TV_TABLES[name];
  const prefs = { hidden: [], density: "comfortable" };
  try {
    const raw = storageGet(tvPrefKey(name));
    const o = raw ? JSON.parse(raw) : null;
    if (o && typeof o === "object") {
      if (Array.isArray(o.hidden)) prefs.hidden = o.hidden.filter(k => T.cols.some(c => c.key === k && !c.fixed));
      if (o.density === "compact") prefs.density = "compact";
      // Tables outside the URL hash keep their sort here.
      if (!T.tab && typeof o.sort === "string") tv.sort[name] = tablesValidSort(name, o.sort);
    }
  } catch (err) {
    // unreadable preferences: defaults
  }
  tv.prefs[name] = prefs;
}

function tvSavePrefs(name) {
  const p = tv.prefs[name] || { hidden: [], density: "comfortable" };
  const data = { hidden: p.hidden, density: p.density };
  if (!TV_TABLES[name].tab && tv.sort[name]) data.sort = tv.sort[name];
  try {
    storageSet(tvPrefKey(name), JSON.stringify(data));
  } catch (err) {
    // not persisted
  }
}

// Applies the column visibility (tv-hide-N classes, see tables.css) and density.
function tvApplyPrefs(name) {
  const table = tvTable(name);
  const p = tv.prefs[name];
  if (!table || !p) return;
  Array.from(table.classList).filter(c => c.startsWith("tv-hide-")).forEach(c => table.classList.remove(c));
  p.hidden.forEach(key => {
    const n = (tv.index[name] || {})[key];
    if (n) table.classList.add(`tv-hide-${Number(n)}`);
  });
  table.classList.toggle("table-compact", p.density === "compact");
}

// ---------------------------------------------------------------------------
// Saved views (localStorage, per tab)
// ---------------------------------------------------------------------------

function tvViewsKey(name) {
  return `mongorescue_views_${TV_TABLES[name].tab}`;
}

function tvStorageAvailable() {
  try {
    const k = "mongorescue_storage_probe";
    window.localStorage.setItem(k, "1");
    window.localStorage.removeItem(k);
    return true;
  } catch (err) {
    return false;
  }
}

// The saved views of table name: { views: [{ id, name, qs }], def: id or "" }.
function tvViews(name) {
  const out = { views: [], def: "" };
  try {
    const raw = storageGet(tvViewsKey(name));
    const o = raw ? JSON.parse(raw) : null;
    if (o && Array.isArray(o.views)) {
      o.views.forEach(v => {
        if (!v || typeof v !== "object" || out.views.length >= TV_VIEWS_MAX) return;
        const id = String(v.id || "");
        const nm = String(v.name || "").trim().slice(0, TV_VIEW_NAME_MAX);
        const qs = String(v.qs || "");
        if (!/^[a-z0-9]{1,24}$/.test(id) || !nm || qs.length > TV_VIEW_QS_MAX || out.views.some(x => x.id === id)) return;
        out.views.push({ id, name: nm, qs });
      });
      if (out.views.some(v => v.id === o.def)) out.def = String(o.def);
    }
  } catch (err) {
    // unreadable views: none
  }
  return out;
}

function tvSaveViews(name, data) {
  try {
    storageSet(tvViewsKey(name), JSON.stringify({ views: data.views, def: data.def }));
  } catch (err) {
    // not persisted
  }
}

function tvNewViewId() {
  return (Date.now().toString(36) + Math.random().toString(36).slice(2, 8)).slice(0, 24);
}

// Applies a filter + sort query (a saved view) to table name.
function tvApplyQuery(name, qs, push) {
  const T = TV_TABLES[name];
  const params = new URLSearchParams(qs);
  if (T.server) {
    clearTimeout(lists[name].timer);
    applyListParams(name, params);
    renderListControls(name, true);
  } else {
    tvApplyParams(name, params);
    tvRenderControls(name, true);
  }
  tvRenderHeaders(name);
  writeHash(push);
  tvRerender(name);
}

function tvIsDefaultState(name) {
  if (TV_TABLES[name].server) return activeFilterCount(name) === 0 && !lists[name].sort;
  return tablesActiveCount(name) === 0 && !tv.sort[name];
}

function tvSaveView(name, input) {
  const label = String(input.value || "").trim().slice(0, TV_VIEW_NAME_MAX);
  if (!label) {
    showToast(t("tv.view_name_required"), "error");
    input.focus();
    return;
  }
  const data = tvViews(name);
  const qs = tvQuery(name);
  const same = data.views.find(v => v.name.toLocaleLowerCase() === label.toLocaleLowerCase());
  if (same) {
    same.qs = qs;
    showToast(tf("tv.view_updated", { name: same.name }), "success");
  } else {
    if (data.views.length >= TV_VIEWS_MAX) {
      showToast(tf("tv.view_limit", { n: TV_VIEWS_MAX }), "error");
      return;
    }
    data.views.push({ id: tvNewViewId(), name: label, qs });
    showToast(tf("tv.view_saved", { name: label }), "success");
  }
  tvSaveViews(name, data);
  tvRenderPop(name, "views");
  const again = document.getElementById(`tv-view-name-${name}`);
  if (again) again.focus();
}

function tvViewAction(name, action, id, btn) {
  const data = tvViews(name);
  const view = data.views.find(v => v.id === id);
  if (!view) return;
  switch (action) {
    case "view-apply":
      tvClosePop(true);
      tvApplyQuery(name, view.qs, true);
      showToast(tf("tv.view_applied", { name: view.name }), "info");
      return;
    case "view-default":
      data.def = data.def === id ? "" : id;
      tvSaveViews(name, data);
      showToast(data.def ? tf("tv.default_set", { name: view.name }) : t("tv.default_cleared"), "success");
      break;
    case "view-rename":
      tv.renaming = { name, id };
      break;
    case "view-delete":
      data.views = data.views.filter(v => v.id !== id);
      if (data.def === id) data.def = "";
      tvSaveViews(name, data);
      showToast(tf("tv.view_deleted", { name: view.name }), "success");
      break;
    default:
      return;
  }
  tvRenderPop(name, "views");
  const pop = document.getElementById(`tv-pop-views-${name}`);
  const focus = action === "view-rename"
    ? pop && pop.querySelector(".tv-rename-input")
    : pop && (pop.querySelector(`[data-tv="${action}"][data-view="${CSS.escape(id)}"]`) || pop.querySelector("input"));
  if (focus) focus.focus();
  else if (btn && document.contains(btn)) btn.focus();
}

function tvRenameView(name, id, input) {
  const label = String(input.value || "").trim().slice(0, TV_VIEW_NAME_MAX);
  if (!label) {
    showToast(t("tv.view_name_required"), "error");
    input.focus();
    return;
  }
  const data = tvViews(name);
  const view = data.views.find(v => v.id === id);
  tv.renaming = null;
  if (view) {
    view.name = label;
    tvSaveViews(name, data);
    showToast(tf("tv.view_renamed", { name: label }), "success");
  }
  tvRenderPop(name, "views");
  const pop = document.getElementById(`tv-pop-views-${name}`);
  const again = pop && pop.querySelector(`[data-tv="view-apply"][data-view="${CSS.escape(id)}"]`);
  if (again) again.focus();
}

// ---------------------------------------------------------------------------
// Views and columns popovers
// ---------------------------------------------------------------------------

const TV_ICONS = {
  views: '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M4.5 2.5h7v11L8 11l-3.5 2.5z"/></svg>',
  columns: '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><rect x="2.5" y="3" width="11" height="10" rx="1.5"/><path d="M6.2 3v10M9.8 3v10"/></svg>'
};

// Adds the Views and Columns buttons to the filter bar of table name.
function tvSetupTools(name) {
  const T = TV_TABLES[name];
  const bar = document.querySelector(T.bar);
  if (!bar) return;
  let meta = bar.querySelector(".filter-meta");
  if (!meta) {
    meta = document.createElement("span");
    meta.className = "filter-meta";
    bar.appendChild(meta);
  }
  const tools = document.createElement("span");
  tools.className = "tv-tools";
  tools.innerHTML = ["views", "columns"].map(kind => {
    const titleKey = kind === "views" ? "tv.views_title" : "tv.columns_title";
    return `<span class="tv-drop">
      <button type="button" class="btn btn-secondary btn-sm tv-drop-btn" id="tv-btn-${kind}-${name}" data-tv="toggle" data-tv-pop="${kind}" data-tv-table="${name}" aria-expanded="false" aria-controls="tv-pop-${kind}-${name}" aria-haspopup="dialog">${TV_ICONS[kind]}<span data-i18n="tv.${kind}">${escapeHtml(t(`tv.${kind}`))}</span></button>
      <div class="tv-pop" id="tv-pop-${kind}-${name}" role="dialog" aria-label="${escapeHtml(t(titleKey))}" data-i18n-aria-label="${titleKey}" hidden></div>
    </span>`;
  }).join("");
  meta.appendChild(tools);
}

function tvRenderPop(name, kind) {
  const pop = document.getElementById(`tv-pop-${kind}-${name}`);
  if (!pop) return;
  pop.innerHTML = kind === "views" ? tvViewsHtml(name) : tvColumnsHtml(name);
}

function tvViewsHtml(name) {
  const n = escapeHtml(name);
  let html = `<p class="tv-pop-title">${escapeHtml(t("tv.views_title"))}</p>`;
  if (!tvStorageAvailable()) return `${html}<p class="tv-pop-note">${escapeHtml(t("tv.views_unavailable"))}</p>`;
  const data = tvViews(name);
  if (data.views.length === 0) {
    html += `<p class="tv-pop-note">${escapeHtml(t("tv.views_empty"))}</p>`;
  } else {
    html += `<ul class="tv-views">${data.views.map(v => {
      const id = escapeHtml(v.id);
      const isDef = data.def === v.id;
      if (tv.renaming && tv.renaming.name === name && tv.renaming.id === v.id) {
        return `<li class="tv-view"><form class="tv-view-form" data-tv-form="rename" data-tv-table="${n}" data-view="${id}">
          <input type="text" class="form-input tv-rename-input" maxlength="${TV_VIEW_NAME_MAX}" value="${escapeHtml(v.name)}" aria-label="${escapeHtml(t("tv.view_name"))}" autocomplete="off">
          <button type="submit" class="btn btn-primary btn-sm">${escapeHtml(t("tv.save"))}</button>
          <button type="button" class="btn btn-ghost btn-sm" data-tv="view-rename-cancel" data-tv-table="${n}" data-view="${id}">${escapeHtml(t("tv.cancel"))}</button>
        </form></li>`;
      }
      return `<li class="tv-view">
        <button type="button" class="tv-view-apply" data-tv="view-apply" data-tv-table="${n}" data-view="${id}"><span class="tv-view-name">${escapeHtml(v.name)}</span>${isDef ? `<span class="chip tv-default-chip">${escapeHtml(t("tv.default_badge"))}</span>` : ""}</button>
        <span class="tv-view-actions">
          <button type="button" class="link-btn" data-tv="view-default" data-tv-table="${n}" data-view="${id}">${escapeHtml(t(isDef ? "tv.unset_default" : "tv.make_default"))}</button>
          <button type="button" class="link-btn" data-tv="view-rename" data-tv-table="${n}" data-view="${id}">${escapeHtml(t("tv.rename"))}</button>
          <button type="button" class="link-btn btn-danger-text" data-tv="view-delete" data-tv-table="${n}" data-view="${id}">${escapeHtml(t("tv.delete"))}</button>
        </span>
      </li>`;
    }).join("")}</ul>`;
  }
  html += `<form class="tv-view-form tv-view-save" data-tv-form="save" data-tv-table="${n}">
      <label class="sr-only" for="tv-view-name-${n}">${escapeHtml(t("tv.view_name"))}</label>
      <input type="text" class="form-input" id="tv-view-name-${n}" maxlength="${TV_VIEW_NAME_MAX}" placeholder="${escapeHtml(t("tv.view_name"))}" autocomplete="off">
      <button type="submit" class="btn btn-primary btn-sm">${escapeHtml(t("tv.save_view"))}</button>
    </form>
    <p class="tv-pop-note">${escapeHtml(t("tv.default_hint"))}</p>`;
  return html;
}

function tvColumnsHtml(name) {
  const n = escapeHtml(name);
  const p = tv.prefs[name] || { hidden: [], density: "comfortable" };
  const cols = TV_TABLES[name].cols.filter(c => !c.fixed && (tv.index[name] || {})[c.key]);
  const boxes = cols.map(c => `<label class="check-inline"><input type="checkbox" data-tv="col" data-tv-table="${n}" data-col="${escapeHtml(c.key)}"${p.hidden.includes(c.key) ? "" : " checked"}><span>${escapeHtml(t(c.i18n))}</span></label>`).join("");
  const radio = d => `<label class="check-inline"><input type="radio" name="tv-density-${n}" value="${d}" data-tv="density" data-tv-table="${n}"${p.density === d ? " checked" : ""}><span>${escapeHtml(t(`tv.density_${d}`))}</span></label>`;
  return `<p class="tv-pop-title">${escapeHtml(t("tv.columns_title"))}</p>
    <fieldset class="tv-fieldset"><legend>${escapeHtml(t("tv.visible_columns"))}</legend>${boxes}</fieldset>
    <fieldset class="tv-fieldset"><legend>${escapeHtml(t("tv.density"))}</legend>${radio("comfortable")}${radio("compact")}</fieldset>
    <button type="button" class="link-btn" data-tv="cols-reset" data-tv-table="${n}">${escapeHtml(t("tv.show_all_columns"))}</button>`;
}

function tvOpenPop(name, kind) {
  tvClosePop(false);
  tv.renaming = null;
  const pop = document.getElementById(`tv-pop-${kind}-${name}`);
  const btn = document.getElementById(`tv-btn-${kind}-${name}`);
  if (!pop || !btn) return;
  tvRenderPop(name, kind);
  pop.hidden = false;
  btn.setAttribute("aria-expanded", "true");
  tv.pop = { name, kind };
  const first = pop.querySelector("input, button");
  if (first) first.focus();
}

function tvClosePop(focusToggle) {
  if (!tv.pop) return;
  const { name, kind } = tv.pop;
  tv.pop = null;
  tv.renaming = null;
  const pop = document.getElementById(`tv-pop-${kind}-${name}`);
  const btn = document.getElementById(`tv-btn-${kind}-${name}`);
  if (pop) pop.hidden = true;
  if (btn) {
    btn.setAttribute("aria-expanded", "false");
    if (focusToggle) btn.focus();
  }
}

// ---------------------------------------------------------------------------
// Filter bars of the Jobs, Connections and Channels tables
// ---------------------------------------------------------------------------

function tvFillSelect(sel, options, value, allKey) {
  const key = JSON.stringify(options);
  if (sel.dataset.key !== key) {
    sel.textContent = "";
    const all = document.createElement("option");
    all.value = "";
    all.setAttribute("data-i18n", allKey);
    all.textContent = t(allKey);
    sel.appendChild(all);
    options.forEach(([v, label]) => {
      const opt = document.createElement("option");
      opt.value = v;
      opt.textContent = label;
      sel.appendChild(opt);
    });
    sel.dataset.key = key;
  }
  if (sel.value !== value) sel.value = value;
}

// Connection and database options of the Jobs filters. Databases come from the
// jobs (with the databases multi-database jobs list or know) and the backups.
function tvJobOptions() {
  const f = tv.filters.jobs;
  const conn = document.getElementById("jobs-tfilter-connection");
  if (conn) {
    const opts = state.connections.map(c => [String(c.id), String(c.name || c.id)]);
    if (f.connection && !opts.some(o => o[0] === f.connection)) opts.unshift([f.connection, f.connection]);
    tvFillSelect(conn, opts, f.connection, "tv.connection_all");
  }
  const db = document.getElementById("jobs-tfilter-database");
  if (db) {
    const names = new Set();
    const add = v => { if (typeof v === "string" && v) names.add(v); };
    state.jobs.forEach(j => {
      add(j.database);
      (Array.isArray(j.known_databases) ? j.known_databases : []).forEach(add);
      const sel = j.database_selection || {};
      (Array.isArray(sel.databases) ? sel.databases : []).forEach(add);
    });
    ((lists.backups && lists.backups.databases) || []).forEach(add);
    add(f.database);
    const sorted = Array.from(names).sort((a, b) => a.localeCompare(b));
    tvFillSelect(db, sorted.map(n => [n, n]), f.database, "filters.database_all");
  }
}

// Shows the filter state of table name in its bar. The search box is only
// overwritten when it is not being typed in (or force is set).
function tvRenderControls(name, force) {
  const T = TV_TABLES[name];
  if (T.server) {
    renderListControls(name, force);
    return;
  }
  const bar = document.querySelector(`[data-tv-filters="${name}"]`);
  if (!bar) return;
  const f = tv.filters[name];
  if (name === "jobs") tvJobOptions();
  bar.querySelectorAll("[data-tfilter]").forEach(el => {
    const k = el.dataset.tfilter;
    if (!(k in f)) return;
    if (k === "q" && !force && document.activeElement === el) return;
    if (el.value !== f[k]) el.value = f[k];
  });
  const n = tablesActiveCount(name);
  const badge = document.getElementById(`${name}-tfilter-count`);
  if (badge) {
    badge.hidden = n === 0;
    badge.textContent = n > 0 ? tf("filters.active_n", { n }) : "";
  }
  const clear = document.getElementById(`${name}-tfilter-clear`);
  if (clear) clear.hidden = n === 0;
}

function tvSetFilter(name, key, value, push) {
  const spec = TV_FILTERS[name] || {};
  if (!spec[key]) return;
  const v = String(value || "").slice(0, TV_TEXT_MAX);
  if (spec[key] !== "text" && v && !spec[key].includes(v)) return;
  if (tv.filters[name][key] === v) return;
  tv.filters[name][key] = v;
  writeHash(push);
  tvRenderControls(name);
  tvRerender(name);
}

function tvClear(name) {
  if (TV_TABLES[name].server) {
    clearFilters(name);
    return;
  }
  clearTimeout(tv.timers[name]);
  tv.filters[name] = tvEmptyFilters(name);
  writeHash(true);
  tvRenderControls(name, true);
  tvRerender(name);
  const q = document.getElementById(`${name}-tfilter-q`);
  if (q) q.focus();
}

function tvFilterTable(el) {
  const bar = el instanceof Element ? el.closest("[data-tv-filters]") : null;
  const name = bar ? tvName(bar.dataset.tvFilters) : "";
  return name && TV_FILTERS[name] ? name : "";
}

// ---------------------------------------------------------------------------
// Inline validation of the job, storage target and channel forms
// ---------------------------------------------------------------------------

function tvHasControl(s) {
  for (const ch of String(s)) {
    const c = ch.codePointAt(0);
    if (c < 0x20 || c === 0x7f) return true;
  }
  return false;
}

function tvNameRule(el) {
  if (el.value && !el.value.trim()) return t("validate.required");
  return tvHasControl(el.value) ? t("tv.err_control") : "";
}

function tvIntRule(el) {
  return el.value === "" || /^\d+$/.test(el.value) ? "" : t("tv.err_integer");
}

const TV_CRON_DESCRIPTORS = ["@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly"];

function tvCronRule(el) {
  let v = el.value.trim();
  if (!v) return "";
  if (v.startsWith("TZ=") || v.startsWith("CRON_TZ=")) {
    const i = v.indexOf(" ");
    if (i < 0) return t("tv.err_cron");
    v = v.slice(i + 1).trim();
  }
  if (v.startsWith("@every ")) {
    const ms = parseGoDuration(v.slice(7).trim());
    return Number.isFinite(ms) && ms > 0 ? "" : t("tv.err_cron_every");
  }
  if (v.startsWith("@")) return TV_CRON_DESCRIPTORS.includes(v) ? "" : t("tv.err_cron");
  const fields = v.split(/\s+/);
  if (fields.length !== 5 || !fields.every(f => /^[0-9A-Za-z*?/,-]+$/.test(f))) return t("tv.err_cron");
  return "";
}

function tvStorageProvider() {
  const preset = STORAGE_PRESETS[getValue("storage-provider")] || STORAGE_PRESETS.s3;
  return { provider: getValue("storage-provider"), local: preset.type === "local" };
}

const TV_STORAGE_RULES = {
  "storage-name": tvNameRule,
  "storage-path": el => {
    const v = el.value.trim();
    if (!tvStorageProvider().local) return "";
    if (!v) return t("storage.err_path");
    if (v.split(/[\\/]+/).includes("..")) return t("storage.err_path_traversal");
    return v.startsWith("/") || v.startsWith("\\") || /^[A-Za-z]:[\\/]/.test(v) ? "" : t("tv.err_path_absolute");
  },
  "storage-endpoint": el => {
    const v = el.value.trim().replace(/\/+$/, "");
    if (tvStorageProvider().local) return "";
    if (/[<>{}]/.test(v)) return t("storage.err_endpoint_placeholder");
    if (!v) return tvStorageProvider().provider === "aws" ? "" : t("storage.err_endpoint_required");
    return tvUrlOk(v) ? "" : t("storage.err_endpoint");
  },
  "storage-bucket": el => {
    const v = el.value.trim();
    if (tvStorageProvider().local) return "";
    return !v || /[\s/]/.test(v) ? t("storage.err_bucket") : "";
  },
  "storage-region": el => (tvStorageProvider().provider === "aws" && !el.value.trim() ? t("storage.err_region") : ""),
  "storage-access-key": el => (!el.value.trim() && document.getElementById("storage-secret-key").value ? t("storage.err_credentials") : ""),
  "storage-secret-key": el => (!el.value && getValue("storage-access-key") ? t("storage.err_credentials") : "")
};

function tvUrlOk(v) {
  try {
    const u = new URL(v);
    return (u.protocol === "https:" || u.protocol === "http:") && !!u.hostname;
  } catch (err) {
    return false;
  }
}

// An RFC 5322-ish address, plain or "Name <addr>": one @ with text on both sides.
function tvEmailOk(s) {
  let addr = String(s || "").trim();
  const lt = addr.lastIndexOf("<");
  if (lt >= 0) {
    if (!addr.endsWith(">")) return false;
    addr = addr.slice(lt + 1, -1).trim();
  }
  const at = addr.lastIndexOf("@");
  return at > 0 && at < addr.length - 1 && !/[\s<>,;]/.test(addr);
}

const TV_E164_RE = /^\+[1-9][0-9]{6,14}$/;
const TV_HEADER_NAME_RE = /^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,128}$/;

function tvRequired(el) {
  return el.value.trim() ? "" : t("validate.required");
}

const TV_CHANNEL_RULES = {
  "channel-name": tvNameRule,
  "webhook-url": el => tvRequired(el) || (tvUrlOk(el.value.trim()) ? "" : t("tv.err_url")),
  "webhook-headers": el => (String(el.value || "").split("\n").map(l => l.trim()).filter(Boolean).every(l => {
    const i = l.indexOf(":");
    return i > 0 && TV_HEADER_NAME_RE.test(l.slice(0, i).trim());
  }) ? "" : t("tv.err_header")),
  "telegram-token": el => tvRequired(el) || (el.value === MASKED_SECRET || /^[0-9]{1,20}:[A-Za-z0-9_-]{20,100}$/.test(el.value.trim()) ? "" : t("tv.err_telegram_token")),
  "telegram-chat": el => tvRequired(el) || (/^(-?[0-9]{1,20}|@[A-Za-z0-9_]{4,64})$/.test(el.value.trim()) ? "" : t("tv.err_telegram_chat")),
  "email-host": el => tvRequired(el) || (/^[A-Za-z0-9._-]{1,253}$/.test(el.value.trim()) || /^\[[0-9A-Fa-f:.]{2,45}\]$/.test(el.value.trim()) ? "" : t("tv.err_host")),
  "email-port": tvIntRule,
  "email-from": el => tvRequired(el) || (tvEmailOk(el.value) ? "" : t("tv.err_email")),
  "email-to": el => {
    const list = parseList(el.value);
    return list.length >= 1 && list.length <= 50 && list.every(tvEmailOk) ? "" : t("tv.err_email_list");
  },
  "twilio-sid": el => tvRequired(el) || (/^AC[0-9a-fA-F]{32}$/.test(el.value.trim()) ? "" : t("tv.err_twilio_sid")),
  "twilio-token": tvRequired,
  "twilio-from": el => tvRequired(el) || (TV_E164_RE.test(el.value.trim()) || /^MG[0-9a-fA-F]{32}$/.test(el.value.trim()) ? "" : t("tv.err_twilio_from")),
  "twilio-to": el => {
    const list = parseList(el.value);
    return list.length >= 1 && list.length <= 20 && list.every(n => TV_E164_RE.test(n)) ? "" : t("tv.err_phone_list");
  }
};

function tvSetupValidation() {
  if (typeof window.mrLiveValidate !== "function") return;
  window.mrLiveValidate("form-new-job", {
    "job-name": tvNameRule,
    "job-cron": tvCronRule,
    "job-retention-days": tvIntRule,
    "job-retention-count": tvIntRule,
    "job-rt-every-n": tvIntRule
  });
  window.mrLiveValidate("form-storage", TV_STORAGE_RULES);
  window.mrLiveValidate("form-channel", TV_CHANNEL_RULES);
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

document.addEventListener("click", (e) => {
  const target = e.target instanceof Element ? e.target : null;
  if (!target) return;
  // A click outside the open popover closes it.
  if (tv.pop) {
    const drop = document.getElementById(`tv-pop-${tv.pop.kind}-${tv.pop.name}`);
    const holder = drop ? drop.closest(".tv-drop") : null;
    if (holder && !holder.contains(target)) tvClosePop(false);
  }
  const el = target.closest("[data-tv]");
  if (!el || el.disabled) return;
  const name = tvName(el.dataset.tvTable || "");
  if (!name) return;
  switch (el.dataset.tv) {
    case "sort":
      tvSetSort(name, el.dataset.col || "");
      break;
    case "toggle": {
      const kind = el.dataset.tvPop === "columns" ? "columns" : "views";
      if (tv.pop && tv.pop.name === name && tv.pop.kind === kind) {
        tvClosePop(true);
      } else {
        tvOpenPop(name, kind);
      }
      break;
    }
    case "clear":
      tvClear(name);
      break;
    case "view-apply":
    case "view-default":
    case "view-rename":
    case "view-delete":
      tvViewAction(name, el.dataset.tv, el.dataset.view || "", el);
      break;
    case "view-rename-cancel": {
      const id = el.dataset.view || "";
      tv.renaming = null;
      tvRenderPop(name, "views");
      const pop = document.getElementById(`tv-pop-views-${name}`);
      const again = pop && pop.querySelector(`[data-tv="view-rename"][data-view="${CSS.escape(id)}"]`);
      if (again) again.focus();
      break;
    }
    case "cols-reset":
      tv.prefs[name].hidden = [];
      tvSavePrefs(name);
      tvApplyPrefs(name);
      tvRenderPop(name, "columns");
      break;
  }
});

document.addEventListener("change", (e) => {
  const el = e.target instanceof HTMLElement ? e.target : null;
  if (!el) return;
  const name = tvName(el.dataset.tvTable || "");
  if (name && el.dataset.tv === "col" && el instanceof HTMLInputElement) {
    const key = el.dataset.col || "";
    const p = tv.prefs[name];
    p.hidden = p.hidden.filter(k => k !== key);
    if (!el.checked && TV_TABLES[name].cols.some(c => c.key === key && !c.fixed)) p.hidden.push(key);
    tvSavePrefs(name);
    tvApplyPrefs(name);
    return;
  }
  if (name && el.dataset.tv === "density" && el instanceof HTMLInputElement && el.checked) {
    tv.prefs[name].density = el.value === "compact" ? "compact" : "comfortable";
    tvSavePrefs(name);
    tvApplyPrefs(name);
    return;
  }
  const table = tvFilterTable(el);
  if (table && el.dataset.tfilter && el.dataset.tfilter !== "q") tvSetFilter(table, el.dataset.tfilter, el.value, true);
});

document.addEventListener("input", (e) => {
  const el = e.target;
  if (!(el instanceof HTMLInputElement) || el.dataset.tfilter !== "q") return;
  const name = tvFilterTable(el);
  if (!name) return;
  clearTimeout(tv.timers[name]);
  tv.timers[name] = setTimeout(() => tvSetFilter(name, "q", el.value, false), TV_DEBOUNCE_MS);
});

document.addEventListener("keydown", (e) => {
  const el = e.target;
  if (e.key === "Escape" && tv.pop) {
    const pop = document.getElementById(`tv-pop-${tv.pop.kind}-${tv.pop.name}`);
    if (pop && el instanceof Node && pop.closest(".tv-drop").contains(el)) {
      e.preventDefault();
      e.stopPropagation();
      if (tv.renaming) {
        tv.renaming = null;
        tvRenderPop(tv.pop.name, "views");
        const first = pop.querySelector("input, button");
        if (first) first.focus();
      } else {
        tvClosePop(true);
      }
    }
    return;
  }
  if (e.key !== "Enter" || !(el instanceof HTMLInputElement) || el.dataset.tfilter !== "q") return;
  const name = tvFilterTable(el);
  if (!name) return;
  e.preventDefault();
  clearTimeout(tv.timers[name]);
  tvSetFilter(name, "q", el.value, false);
});

document.addEventListener("submit", (e) => {
  const form = e.target instanceof HTMLFormElement ? e.target : null;
  if (!form || !form.dataset.tvForm) return;
  e.preventDefault();
  const name = tvName(form.dataset.tvTable || "");
  const input = form.querySelector("input");
  if (!name || !input) return;
  if (form.dataset.tvForm === "save") tvSaveView(name, input);
  else if (form.dataset.tvForm === "rename") tvRenameView(name, form.dataset.view || "", input);
});

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

function setupTables() {
  Object.keys(TV_TABLES).forEach(name => {
    const T = TV_TABLES[name];
    const table = tvTable(name);
    if (!table) return;
    tvSetupHeaders(name);
    tvLoadPrefs(name);
    tvApplyPrefs(name);
    if (T.tools) {
      tvSetupTools(name);
      const wrap = table.closest(".table-wrap");
      if (wrap) wrap.classList.add("tv-scroll");
    }
    tvRenderHeaders(name);
  });
  // A default view opens with the dashboard unless the link names a state.
  Object.keys(TV_TABLES).filter(n => TV_TABLES[n].tools).forEach(name => {
    const data = tvViews(name);
    const view = data.views.find(v => v.id === data.def);
    if (!view || !tvIsDefaultState(name)) return;
    const params = new URLSearchParams(view.qs);
    if (TV_TABLES[name].server) {
      applyListParams(name, params);
    } else {
      tvApplyParams(name, params);
    }
    tvRenderHeaders(name);
  });
  Object.keys(TV_FILTERS).forEach(name => tvRenderControls(name, true));
  LIST_KINDS.forEach(kind => renderListControls(kind, true));
  writeHash(false);
  // Skeleton rows replace the static "Loading…" rows.
  document.querySelectorAll("#app-main tbody").forEach(tbody => {
    if (tbody.rows.length === 1 && tbody.querySelector('tr.empty-row [data-i18n="tables.loading"]')) {
      const html = tablesSkeletonFor(tbody);
      if (html) tbody.innerHTML = html;
    }
    tvMarkBusy(tbody);
  });
  tvSetupValidation();
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      if (tv.pop) tvRenderPop(tv.pop.name, tv.pop.kind);
      Object.keys(TV_FILTERS).forEach(name => tvRenderControls(name, true));
    });
  }
}

document.addEventListener("DOMContentLoaded", setupTables);

// For the headless check and the browser console.
window.mrTables = { tv, tvJobs, tablesValidSort, tvQuery };
