/**
 * MongoRescue dashboard: the experimental "Point-in-time recovery" panel of the
 * Connections tab.
 *
 * It lists the PITR streams (GET /api/v1/pitr/streams): per connection the
 * collector's state, the newest point-in-time window, the lag, the oplog headroom
 * and the chain breaks, with actions to enable or disable a stream (PATCH, admin),
 * take a base backup now (POST .../base, operator) and delete a disabled stream
 * (DELETE, admin). The form below creates the stream of a connection (POST, admin).
 * The restore wizard (admin) picks a time inside a window, runs the restore
 * preflight (POST /api/v1/restores/preflight with "pitr") to show the chosen base,
 * the chunks and the estimated duration, and starts the restore into safe clones
 * (POST /api/v1/restore) after a confirmation.
 *
 * Loaded after app.js and trust.js (mergeTranslations). Server values reach the
 * DOM only through escapeHtml, textContent and form values.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const PITR_TRANSLATIONS = {
  en: {
    pitr: {
      title: "Point-in-time recovery",
      experimental: "Experimental",
      desc: "Collects the oplog of a replica set connection into encrypted chunks and takes base backups of the whole instance, so it can be restored to any moment within its window, into new databases.",
      table_label: "Point-in-time recovery streams",
      col_connection: "Connection",
      col_status: "Collector",
      col_window: "Window",
      col_lag: "Lag",
      col_headroom: "Headroom",
      col_breaks: "Chain breaks",
      empty: "No connection collects its oplog yet.",
      status_running: "Running",
      status_failed: "Failing",
      status_stopped: "Stopped",
      status_disabled: "Disabled",
      no_window: "No window yet (waiting for a base backup)",
      enable_title: "Enable point-in-time recovery",
      enable_hint: "Needs a replica set whose user may read local.oplog.rs (the backup role, or read on local) and backup encryption with age keys: the oplog keeps deleted data.",
      connection: "Connection",
      base_cron: "Base backup schedule (cron)",
      chunk_seconds: "Chunk interval (seconds)",
      keep_count: "Keep base backups",
      keep_days: "or days",
      base_on_gap: "Take a base backup when a gap breaks the chain",
      chain_test_cron: "Chain test schedule (cron, optional)",
      chain_test_hint: "Restores one base to the consistent point of the next into temporary databases, compares them with that base's manifest and drops them. Empty turns chain tests off.",
      enable: "Enable",
      disable: "Disable",
      resume: "Enable",
      base_now: "Take base backup",
      delete: "Delete",
      delete_title: "Delete this PITR stream?",
      delete_body: "Its oplog chunks are deleted and purged once the delete grace period ends; delete the stream again then. Its base backups stay as backups.",
      enabled_toast: "Point-in-time recovery enabled for {name}.",
      updated_toast: "PITR stream of {name} updated.",
      base_started: "Base backup {id} started.",
      deleted: "PITR stream deleted.",
      no_connections: "Add a replica set connection first.",
      invalid: "Check the schedule, the interval (15-900 seconds) and the retention.",
      restore_action: "Restore to a time",
      restore_title: "Restore to a point in time",
      restore_hint: "Every database, or the ones you list, is restored into a new database named <db>_rescue_<timestamp>; existing data is never touched. Times are UTC on the primary's clock.",
      restore_window: "Window",
      restore_at: "Restore to (UTC)",
      restore_databases: "Databases (optional, comma-separated)",
      restore_all: "all databases",
      restore_cancel: "Cancel",
      restore_check: "Check",
      restore_start: "Restore",
      restore_open: "open",
      restore_range: "Between {start} and {end} (UTC).",
      restore_no_window: "This stream has no window to restore from yet.",
      restore_outside: "Choose a time inside the window.",
      restore_checks_failed: "A preflight check failed; fix it or choose another time.",
      restore_plan_base: "Base backup",
      restore_plan_chunks: "Oplog",
      restore_plan_chunks_value: "{n} chunk(s), {size}",
      restore_plan_size: "Base backup size",
      restore_plan_clones: "New databases",
      restore_plan_rto: "Estimated duration",
      restore_rto_default: "from default rates",
      restore_rto_chain_test: "from the last chain test",
      restore_confirm_title: "Start the point-in-time restore?",
      restore_confirm_body: "Restores to {at} into new databases ending in {suffix}; existing data is untouched. Estimated duration: {duration}.",
      restore_started: "Point-in-time restore {id} started."
    },
    notify: {
      events: {
        pitr_chain_broken: "PITR oplog chain broken",
        pitr_diverged: "PITR oplog diverged",
        pitr_lag_high: "PITR collector lagging",
        pitr_lag_recovered: "PITR collector caught up",
        pitr_window_low: "PITR oplog headroom low",
        pitr_collector_failed: "PITR collector failing",
        pitr_collector_recovered: "PITR collector recovered"
      }
    },
    readiness: {
      reason_pitr_chain_broken: "PITR chain broken",
      reason_pitr_collector_down: "PITR collector down",
      reason_pitr_lag_high: "PITR lag high",
      reason_pitr_window_low: "PITR headroom low",
      reason_pitr_no_window: "No PITR window yet",
      reason_pitr_chain_test_failed: "PITR chain test failed",
      rpo_pitr: "PITR: oplog captured",
      rpo_pitr_note: "The recovery point comes from the PITR stream: the oplog up to it is captured and can be restored to a point in time (experimental)."
    }
  },
  tr: {
    pitr: {
      title: "Zamana noktasal kurtarma",
      experimental: "Deneysel",
      desc: "Bir replica set bağlantısının oplog'unu şifreli parçalara toplar ve tüm sunucunun temel yedeklerini alır; böylece penceresi içindeki herhangi bir ana, yeni veritabanlarına geri yüklenebilir.",
      table_label: "Zamana noktasal kurtarma akışları",
      col_connection: "Bağlantı",
      col_status: "Toplayıcı",
      col_window: "Pencere",
      col_lag: "Gecikme",
      col_headroom: "Pay",
      col_breaks: "Zincir kopmaları",
      empty: "Henüz hiçbir bağlantının oplog'u toplanmıyor.",
      status_running: "Çalışıyor",
      status_failed: "Hata veriyor",
      status_stopped: "Durdu",
      status_disabled: "Kapalı",
      no_window: "Henüz pencere yok (temel yedek bekleniyor)",
      enable_title: "Zamana noktasal kurtarmayı aç",
      enable_hint: "Kullanıcısı local.oplog.rs'i okuyabilen (backup rolü ya da local üzerinde read) bir replica set ve age anahtarlı yedek şifrelemesi gerekir: oplog silinen verileri de saklar.",
      connection: "Bağlantı",
      base_cron: "Temel yedek zamanlaması (cron)",
      chunk_seconds: "Parça aralığı (saniye)",
      keep_count: "Saklanacak temel yedek",
      keep_days: "veya gün",
      base_on_gap: "Bir boşluk zinciri koparınca temel yedek al",
      chain_test_cron: "Zincir testi zamanlaması (cron, isteğe bağlı)",
      chain_test_hint: "Bir temel yedeği, sonrakinin tutarlı noktasına geçici veritabanlarında geri yükler, o yedeğin manifestiyle karşılaştırır ve siler. Boş bırakılırsa zincir testleri kapalıdır.",
      enable: "Aç",
      disable: "Kapat",
      resume: "Aç",
      base_now: "Temel yedek al",
      delete: "Sil",
      delete_title: "Bu PITR akışı silinsin mi?",
      delete_body: "Oplog parçaları silinir ve silme bekleme süresi bitince temizlenir; akışı o zaman yeniden silin. Temel yedekleri yedek olarak kalır.",
      enabled_toast: "{name} için zamana noktasal kurtarma açıldı.",
      updated_toast: "{name} PITR akışı güncellendi.",
      base_started: "{id} temel yedeği başladı.",
      deleted: "PITR akışı silindi.",
      no_connections: "Önce bir replica set bağlantısı ekleyin.",
      invalid: "Zamanlamayı, aralığı (15-900 saniye) ve saklama ayarlarını kontrol edin.",
      restore_action: "Bir zamana geri yükle",
      restore_title: "Zamana noktasal geri yükleme",
      restore_hint: "Tüm veritabanları ya da listelediğiniz veritabanları <db>_rescue_<timestamp> adlı yeni bir veritabanına geri yüklenir; mevcut verilere dokunulmaz. Saatler UTC'dir ve birincil sunucunun saatine göredir.",
      restore_window: "Pencere",
      restore_at: "Geri yüklenecek an (UTC)",
      restore_databases: "Veritabanları (isteğe bağlı, virgülle ayrılmış)",
      restore_all: "tüm veritabanları",
      restore_cancel: "İptal",
      restore_check: "Kontrol et",
      restore_start: "Geri yükle",
      restore_open: "açık",
      restore_range: "{start} ile {end} arası (UTC).",
      restore_no_window: "Bu akışın henüz geri yüklenebilecek bir penceresi yok.",
      restore_outside: "Pencere içinde bir zaman seçin.",
      restore_checks_failed: "Bir ön kontrol başarısız oldu; düzeltin ya da başka bir zaman seçin.",
      restore_plan_base: "Temel yedek",
      restore_plan_chunks: "Oplog",
      restore_plan_chunks_value: "{n} parça, {size}",
      restore_plan_size: "Temel yedek boyutu",
      restore_plan_clones: "Yeni veritabanları",
      restore_plan_rto: "Tahmini süre",
      restore_rto_default: "varsayılan hızlara göre",
      restore_rto_chain_test: "son zincir testine göre",
      restore_confirm_title: "Zamana noktasal geri yükleme başlatılsın mı?",
      restore_confirm_body: "{at} anına, adı {suffix} ile biten yeni veritabanlarına geri yükler; mevcut verilere dokunulmaz. Tahmini süre: {duration}.",
      restore_started: "Zamana noktasal geri yükleme {id} başladı."
    },
    notify: {
      events: {
        pitr_chain_broken: "PITR oplog zinciri koptu",
        pitr_diverged: "PITR oplog'u ayrıştı",
        pitr_lag_high: "PITR toplayıcısı geride",
        pitr_lag_recovered: "PITR toplayıcısı yetişti",
        pitr_window_low: "PITR oplog payı düşük",
        pitr_collector_failed: "PITR toplayıcısı hata veriyor",
        pitr_collector_recovered: "PITR toplayıcısı düzeldi"
      }
    },
    readiness: {
      reason_pitr_chain_broken: "PITR zinciri koptu",
      reason_pitr_collector_down: "PITR toplayıcısı çalışmıyor",
      reason_pitr_lag_high: "PITR gecikmesi yüksek",
      reason_pitr_window_low: "PITR payı düşük",
      reason_pitr_no_window: "Henüz PITR penceresi yok",
      reason_pitr_chain_test_failed: "PITR zincir testi başarısız",
      rpo_pitr: "PITR: oplog kaydediliyor",
      rpo_pitr_note: "Kurtarma noktası PITR akışından gelir: oplog o ana kadar kaydedildi ve zamana noktasal olarak geri yüklenebilir (deneysel)."
    }
  },
  de: {
    pitr: {
      title: "Point-in-Time-Wiederherstellung",
      experimental: "Experimentell",
      desc: "Sammelt das Oplog einer Replica-Set-Verbindung in verschlüsselten Blöcken und erstellt Basis-Backups der ganzen Instanz, damit sie zu jedem Zeitpunkt innerhalb ihres Fensters in neue Datenbanken wiederhergestellt werden kann.",
      table_label: "Point-in-Time-Streams",
      col_connection: "Verbindung",
      col_status: "Collector",
      col_window: "Fenster",
      col_lag: "Verzögerung",
      col_headroom: "Reserve",
      col_breaks: "Kettenbrüche",
      empty: "Noch keine Verbindung sammelt ihr Oplog.",
      status_running: "Läuft",
      status_failed: "Fehlerhaft",
      status_stopped: "Gestoppt",
      status_disabled: "Deaktiviert",
      no_window: "Noch kein Fenster (wartet auf ein Basis-Backup)",
      enable_title: "Point-in-Time-Wiederherstellung aktivieren",
      enable_hint: "Benötigt ein Replica Set, dessen Benutzer local.oplog.rs lesen darf (Rolle backup oder read auf local), und Backup-Verschlüsselung mit age-Schlüsseln: Das Oplog enthält auch gelöschte Daten.",
      connection: "Verbindung",
      base_cron: "Zeitplan der Basis-Backups (cron)",
      chunk_seconds: "Blockintervall (Sekunden)",
      keep_count: "Basis-Backups behalten",
      keep_days: "oder Tage",
      base_on_gap: "Basis-Backup erstellen, wenn eine Lücke die Kette bricht",
      chain_test_cron: "Zeitplan für Kettentests (Cron, optional)",
      chain_test_hint: "Stellt ein Basis-Backup auf den konsistenten Punkt des nächsten in temporäre Datenbanken wieder her, vergleicht sie mit dessen Manifest und löscht sie. Leer schaltet Kettentests aus.",
      enable: "Aktivieren",
      disable: "Deaktivieren",
      resume: "Aktivieren",
      base_now: "Basis-Backup erstellen",
      delete: "Löschen",
      delete_title: "Diesen PITR-Stream löschen?",
      delete_body: "Seine Oplog-Blöcke werden gelöscht und nach Ablauf der Löschfrist entfernt; löschen Sie den Stream dann erneut. Seine Basis-Backups bleiben als Backups erhalten.",
      enabled_toast: "Point-in-Time-Wiederherstellung für {name} aktiviert.",
      updated_toast: "PITR-Stream von {name} aktualisiert.",
      base_started: "Basis-Backup {id} gestartet.",
      deleted: "PITR-Stream gelöscht.",
      no_connections: "Fügen Sie zuerst eine Replica-Set-Verbindung hinzu.",
      invalid: "Prüfen Sie den Zeitplan, das Intervall (15-900 Sekunden) und die Aufbewahrung.",
      restore_action: "Auf Zeitpunkt wiederherstellen",
      restore_title: "Auf einen Zeitpunkt wiederherstellen",
      restore_hint: "Jede Datenbank, oder die angegebenen, wird in eine neue Datenbank namens <db>_rescue_<timestamp> wiederhergestellt; vorhandene Daten bleiben unberührt. Zeiten sind UTC nach der Uhr des Primary.",
      restore_window: "Fenster",
      restore_at: "Wiederherstellen auf (UTC)",
      restore_databases: "Datenbanken (optional, durch Kommas getrennt)",
      restore_all: "alle Datenbanken",
      restore_cancel: "Abbrechen",
      restore_check: "Prüfen",
      restore_start: "Wiederherstellen",
      restore_open: "offen",
      restore_range: "Zwischen {start} und {end} (UTC).",
      restore_no_window: "Dieser Stream hat noch kein Fenster zum Wiederherstellen.",
      restore_outside: "Wählen Sie einen Zeitpunkt innerhalb des Fensters.",
      restore_checks_failed: "Eine Vorabprüfung ist fehlgeschlagen; beheben Sie sie oder wählen Sie einen anderen Zeitpunkt.",
      restore_plan_base: "Basis-Backup",
      restore_plan_chunks: "Oplog",
      restore_plan_chunks_value: "{n} Block/Blöcke, {size}",
      restore_plan_size: "Größe des Basis-Backups",
      restore_plan_clones: "Neue Datenbanken",
      restore_plan_rto: "Geschätzte Dauer",
      restore_rto_default: "nach Standardraten",
      restore_rto_chain_test: "nach dem letzten Kettentest",
      restore_confirm_title: "Point-in-Time-Wiederherstellung starten?",
      restore_confirm_body: "Stellt auf {at} in neue Datenbanken mit der Endung {suffix} wieder her; vorhandene Daten bleiben unberührt. Geschätzte Dauer: {duration}.",
      restore_started: "Point-in-Time-Wiederherstellung {id} gestartet."
    },
    notify: {
      events: {
        pitr_chain_broken: "PITR-Oplog-Kette gebrochen",
        pitr_diverged: "PITR-Oplog divergiert",
        pitr_lag_high: "PITR-Collector im Rückstand",
        pitr_lag_recovered: "PITR-Collector aufgeholt",
        pitr_window_low: "PITR-Oplog-Reserve gering",
        pitr_collector_failed: "PITR-Collector fehlerhaft",
        pitr_collector_recovered: "PITR-Collector wiederhergestellt"
      }
    },
    readiness: {
      reason_pitr_chain_broken: "PITR-Kette gebrochen",
      reason_pitr_collector_down: "PITR-Collector ausgefallen",
      reason_pitr_lag_high: "PITR-Verzögerung hoch",
      reason_pitr_window_low: "PITR-Reserve gering",
      reason_pitr_no_window: "Noch kein PITR-Fenster",
      reason_pitr_chain_test_failed: "PITR-Kettentest fehlgeschlagen",
      rpo_pitr: "PITR: Oplog erfasst",
      rpo_pitr_note: "Der Wiederherstellungspunkt stammt aus dem PITR-Stream: Das Oplog ist bis dahin erfasst und kann auf einen Zeitpunkt wiederhergestellt werden (experimentell)."
    }
  },
  es: {
    pitr: {
      title: "Recuperación a un punto en el tiempo",
      experimental: "Experimental",
      desc: "Recoge el oplog de una conexión de replica set en fragmentos cifrados y toma copias base de toda la instancia, para poder restaurarla a cualquier momento dentro de su ventana, en bases de datos nuevas.",
      table_label: "Flujos de recuperación a un punto en el tiempo",
      col_connection: "Conexión",
      col_status: "Recolector",
      col_window: "Ventana",
      col_lag: "Retraso",
      col_headroom: "Margen",
      col_breaks: "Roturas de cadena",
      empty: "Ninguna conexión recoge todavía su oplog.",
      status_running: "En marcha",
      status_failed: "Fallando",
      status_stopped: "Detenido",
      status_disabled: "Desactivado",
      no_window: "Aún sin ventana (esperando una copia base)",
      enable_title: "Activar la recuperación a un punto en el tiempo",
      enable_hint: "Necesita un replica set cuyo usuario pueda leer local.oplog.rs (el rol backup, o read en local) y cifrado de copias con claves age: el oplog guarda también los datos borrados.",
      connection: "Conexión",
      base_cron: "Programación de copias base (cron)",
      chunk_seconds: "Intervalo de fragmento (segundos)",
      keep_count: "Conservar copias base",
      keep_days: "o días",
      base_on_gap: "Tomar una copia base cuando un hueco rompe la cadena",
      chain_test_cron: "Programación de pruebas de cadena (cron, opcional)",
      chain_test_hint: "Restaura una copia base al punto consistente de la siguiente en bases de datos temporales, las compara con el manifiesto de esa copia y las elimina. Vacío desactiva las pruebas de cadena.",
      enable: "Activar",
      disable: "Desactivar",
      resume: "Activar",
      base_now: "Tomar copia base",
      delete: "Eliminar",
      delete_title: "¿Eliminar este flujo PITR?",
      delete_body: "Sus fragmentos de oplog se eliminan y se purgan cuando termina el periodo de gracia; elimina el flujo de nuevo entonces. Sus copias base se conservan como copias.",
      enabled_toast: "Recuperación a un punto en el tiempo activada para {name}.",
      updated_toast: "Flujo PITR de {name} actualizado.",
      base_started: "Copia base {id} iniciada.",
      deleted: "Flujo PITR eliminado.",
      no_connections: "Añade primero una conexión de replica set.",
      invalid: "Revisa la programación, el intervalo (15-900 segundos) y la retención.",
      restore_action: "Restaurar a un momento",
      restore_title: "Restaurar a un punto en el tiempo",
      restore_hint: "Cada base de datos, o las que indiques, se restaura en una base de datos nueva llamada <db>_rescue_<timestamp>; los datos existentes nunca se tocan. Las horas son UTC según el reloj del primario.",
      restore_window: "Ventana",
      restore_at: "Restaurar a (UTC)",
      restore_databases: "Bases de datos (opcional, separadas por comas)",
      restore_all: "todas las bases de datos",
      restore_cancel: "Cancelar",
      restore_check: "Comprobar",
      restore_start: "Restaurar",
      restore_open: "abierta",
      restore_range: "Entre {start} y {end} (UTC).",
      restore_no_window: "Este flujo aún no tiene una ventana desde la que restaurar.",
      restore_outside: "Elige un momento dentro de la ventana.",
      restore_checks_failed: "Una comprobación previa falló; corrígela o elige otro momento.",
      restore_plan_base: "Copia base",
      restore_plan_chunks: "Oplog",
      restore_plan_chunks_value: "{n} fragmento(s), {size}",
      restore_plan_size: "Tamaño de la copia base",
      restore_plan_clones: "Bases de datos nuevas",
      restore_plan_rto: "Duración estimada",
      restore_rto_default: "según las tasas por defecto",
      restore_rto_chain_test: "según la última prueba de cadena",
      restore_confirm_title: "¿Iniciar la restauración a un punto en el tiempo?",
      restore_confirm_body: "Restaura a {at} en bases de datos nuevas que terminan en {suffix}; los datos existentes no se tocan. Duración estimada: {duration}.",
      restore_started: "Restauración a un punto en el tiempo {id} iniciada."
    },
    notify: {
      events: {
        pitr_chain_broken: "Cadena de oplog PITR rota",
        pitr_diverged: "Oplog PITR divergente",
        pitr_lag_high: "Recolector PITR retrasado",
        pitr_lag_recovered: "Recolector PITR al día",
        pitr_window_low: "Margen de oplog PITR bajo",
        pitr_collector_failed: "Recolector PITR fallando",
        pitr_collector_recovered: "Recolector PITR recuperado"
      }
    },
    readiness: {
      reason_pitr_chain_broken: "Cadena PITR rota",
      reason_pitr_collector_down: "Recolector PITR caído",
      reason_pitr_lag_high: "Retraso PITR alto",
      reason_pitr_window_low: "Margen PITR bajo",
      reason_pitr_no_window: "Aún sin ventana PITR",
      reason_pitr_chain_test_failed: "Prueba de cadena PITR fallida",
      rpo_pitr: "PITR: oplog capturado",
      rpo_pitr_note: "El punto de recuperación viene del flujo PITR: el oplog está capturado hasta él y se puede restaurar a un punto en el tiempo (experimental)."
    }
  },
  fr: {
    pitr: {
      title: "Restauration à un instant donné",
      experimental: "Expérimental",
      desc: "Collecte l'oplog d'une connexion replica set en blocs chiffrés et réalise des sauvegardes de base de toute l'instance, pour pouvoir la restaurer à n'importe quel instant de sa fenêtre, dans de nouvelles bases.",
      table_label: "Flux de restauration à un instant donné",
      col_connection: "Connexion",
      col_status: "Collecteur",
      col_window: "Fenêtre",
      col_lag: "Retard",
      col_headroom: "Marge",
      col_breaks: "Ruptures de chaîne",
      empty: "Aucune connexion ne collecte encore son oplog.",
      status_running: "En cours",
      status_failed: "En échec",
      status_stopped: "Arrêté",
      status_disabled: "Désactivé",
      no_window: "Pas encore de fenêtre (en attente d'une sauvegarde de base)",
      enable_title: "Activer la restauration à un instant donné",
      enable_hint: "Nécessite un replica set dont l'utilisateur peut lire local.oplog.rs (rôle backup, ou read sur local) et le chiffrement des sauvegardes avec des clés age : l'oplog conserve aussi les données supprimées.",
      connection: "Connexion",
      base_cron: "Planification des sauvegardes de base (cron)",
      chunk_seconds: "Intervalle des blocs (secondes)",
      keep_count: "Conserver les sauvegardes de base",
      keep_days: "ou jours",
      base_on_gap: "Faire une sauvegarde de base quand un trou rompt la chaîne",
      chain_test_cron: "Planification des tests de chaîne (cron, facultatif)",
      chain_test_hint: "Restaure une sauvegarde de base au point cohérent de la suivante dans des bases temporaires, les compare au manifeste de celle-ci puis les supprime. Vide désactive les tests de chaîne.",
      enable: "Activer",
      disable: "Désactiver",
      resume: "Activer",
      base_now: "Sauvegarde de base",
      delete: "Supprimer",
      delete_title: "Supprimer ce flux PITR ?",
      delete_body: "Ses blocs d'oplog sont supprimés et purgés à la fin du délai de grâce ; supprimez alors le flux à nouveau. Ses sauvegardes de base restent des sauvegardes.",
      enabled_toast: "Restauration à un instant donné activée pour {name}.",
      updated_toast: "Flux PITR de {name} mis à jour.",
      base_started: "Sauvegarde de base {id} démarrée.",
      deleted: "Flux PITR supprimé.",
      no_connections: "Ajoutez d'abord une connexion replica set.",
      invalid: "Vérifiez la planification, l'intervalle (15-900 secondes) et la rétention.",
      restore_action: "Restaurer à un instant",
      restore_title: "Restaurer à un instant donné",
      restore_hint: "Chaque base, ou celles que vous indiquez, est restaurée dans une nouvelle base nommée <db>_rescue_<timestamp> ; les données existantes ne sont jamais touchées. Les heures sont en UTC selon l'horloge du primaire.",
      restore_window: "Fenêtre",
      restore_at: "Restaurer à (UTC)",
      restore_databases: "Bases (facultatif, séparées par des virgules)",
      restore_all: "toutes les bases",
      restore_cancel: "Annuler",
      restore_check: "Vérifier",
      restore_start: "Restaurer",
      restore_open: "ouverte",
      restore_range: "Entre {start} et {end} (UTC).",
      restore_no_window: "Ce flux n'a pas encore de fenêtre à partir de laquelle restaurer.",
      restore_outside: "Choisissez un instant dans la fenêtre.",
      restore_checks_failed: "Une vérification préalable a échoué ; corrigez-la ou choisissez un autre instant.",
      restore_plan_base: "Sauvegarde de base",
      restore_plan_chunks: "Oplog",
      restore_plan_chunks_value: "{n} bloc(s), {size}",
      restore_plan_size: "Taille de la sauvegarde de base",
      restore_plan_clones: "Nouvelles bases",
      restore_plan_rto: "Durée estimée",
      restore_rto_default: "d'après les débits par défaut",
      restore_rto_chain_test: "d'après le dernier test de chaîne",
      restore_confirm_title: "Lancer la restauration à un instant donné ?",
      restore_confirm_body: "Restaure à {at} dans de nouvelles bases se terminant par {suffix} ; les données existantes ne sont pas touchées. Durée estimée : {duration}.",
      restore_started: "Restauration à un instant donné {id} lancée."
    },
    notify: {
      events: {
        pitr_chain_broken: "Chaîne d'oplog PITR rompue",
        pitr_diverged: "Oplog PITR divergent",
        pitr_lag_high: "Collecteur PITR en retard",
        pitr_lag_recovered: "Collecteur PITR à jour",
        pitr_window_low: "Marge d'oplog PITR faible",
        pitr_collector_failed: "Collecteur PITR en échec",
        pitr_collector_recovered: "Collecteur PITR rétabli"
      }
    },
    readiness: {
      reason_pitr_chain_broken: "Chaîne PITR rompue",
      reason_pitr_collector_down: "Collecteur PITR arrêté",
      reason_pitr_lag_high: "Retard PITR élevé",
      reason_pitr_window_low: "Marge PITR faible",
      reason_pitr_no_window: "Pas encore de fenêtre PITR",
      reason_pitr_chain_test_failed: "Test de chaîne PITR en échec",
      rpo_pitr: "PITR : oplog capturé",
      rpo_pitr_note: "Le point de récupération vient du flux PITR : l'oplog est capturé jusqu'à lui et peut être restauré à un instant donné (expérimental)."
    }
  },
  zh: {
    pitr: {
      title: "时间点恢复",
      experimental: "实验性",
      desc: "将副本集连接的 oplog 收集为加密的分块，并对整个实例进行基础备份，以便将其恢复到窗口内的任意时刻（恢复到新数据库）。",
      table_label: "时间点恢复流",
      col_connection: "连接",
      col_status: "收集器",
      col_window: "窗口",
      col_lag: "延迟",
      col_headroom: "余量",
      col_breaks: "链中断",
      empty: "还没有连接在收集其 oplog。",
      status_running: "运行中",
      status_failed: "失败中",
      status_stopped: "已停止",
      status_disabled: "已禁用",
      no_window: "暂无窗口（等待基础备份）",
      enable_title: "启用时间点恢复",
      enable_hint: "需要一个其用户可读取 local.oplog.rs（backup 角色或 local 上的 read）的副本集，以及使用 age 密钥的备份加密：oplog 也会保留已删除的数据。",
      connection: "连接",
      base_cron: "基础备份计划（cron）",
      chunk_seconds: "分块间隔（秒）",
      keep_count: "保留基础备份数",
      keep_days: "或天数",
      base_on_gap: "出现断档导致链中断时进行基础备份",
      chain_test_cron: "链测试计划（cron，可选）",
      chain_test_hint: "将一个基础备份恢复到下一个基础备份的一致点（写入临时数据库），与其清单比较后删除。留空则关闭链测试。",
      enable: "启用",
      disable: "禁用",
      resume: "启用",
      base_now: "立即基础备份",
      delete: "删除",
      delete_title: "删除此 PITR 流？",
      delete_body: "其 oplog 分块将被删除，并在删除宽限期结束后清除；届时请再次删除该流。其基础备份将作为备份保留。",
      enabled_toast: "已为 {name} 启用时间点恢复。",
      updated_toast: "已更新 {name} 的 PITR 流。",
      base_started: "基础备份 {id} 已开始。",
      deleted: "PITR 流已删除。",
      no_connections: "请先添加一个副本集连接。",
      invalid: "请检查计划、间隔（15-900 秒）和保留设置。",
      restore_action: "恢复到某个时间",
      restore_title: "时间点恢复",
      restore_hint: "所有数据库（或您列出的数据库）都会恢复到名为 <db>_rescue_<timestamp> 的新数据库中；现有数据不会被改动。时间为 UTC，以主节点时钟为准。",
      restore_window: "窗口",
      restore_at: "恢复到（UTC）",
      restore_databases: "数据库（可选，逗号分隔）",
      restore_all: "所有数据库",
      restore_cancel: "取消",
      restore_check: "检查",
      restore_start: "恢复",
      restore_open: "进行中",
      restore_range: "{start} 至 {end}（UTC）。",
      restore_no_window: "此流还没有可用于恢复的窗口。",
      restore_outside: "请在窗口内选择时间。",
      restore_checks_failed: "预检失败；请修复或选择其他时间。",
      restore_plan_base: "基础备份",
      restore_plan_chunks: "Oplog",
      restore_plan_chunks_value: "{n} 个分块，{size}",
      restore_plan_size: "基础备份大小",
      restore_plan_clones: "新数据库",
      restore_plan_rto: "预计时长",
      restore_rto_default: "按默认速率估算",
      restore_rto_chain_test: "按最近一次链测试估算",
      restore_confirm_title: "开始时间点恢复？",
      restore_confirm_body: "恢复到 {at}，写入以 {suffix} 结尾的新数据库；现有数据不受影响。预计时长：{duration}。",
      restore_started: "时间点恢复 {id} 已开始。"
    },
    notify: {
      events: {
        pitr_chain_broken: "PITR oplog 链中断",
        pitr_diverged: "PITR oplog 出现分歧",
        pitr_lag_high: "PITR 收集器延迟",
        pitr_lag_recovered: "PITR 收集器已追上",
        pitr_window_low: "PITR oplog 余量不足",
        pitr_collector_failed: "PITR 收集器失败",
        pitr_collector_recovered: "PITR 收集器已恢复"
      }
    },
    readiness: {
      reason_pitr_chain_broken: "PITR 链中断",
      reason_pitr_collector_down: "PITR 收集器停止",
      reason_pitr_lag_high: "PITR 延迟过高",
      reason_pitr_window_low: "PITR 余量不足",
      reason_pitr_no_window: "尚无 PITR 窗口",
      reason_pitr_chain_test_failed: "PITR 链测试失败",
      rpo_pitr: "PITR：oplog 已捕获",
      rpo_pitr_note: "恢复点来自 PITR 流：截至该点的 oplog 已捕获，可以进行时间点恢复（实验性）。"
    }
  },
  ja: {
    pitr: {
      title: "ポイントインタイムリカバリ",
      experimental: "実験的",
      desc: "レプリカセット接続の oplog を暗号化されたチャンクとして収集し、インスタンス全体のベースバックアップを取得します。これにより、ウィンドウ内の任意の時点に新しいデータベースとして復元できます。",
      table_label: "ポイントインタイムリカバリのストリーム",
      col_connection: "接続",
      col_status: "コレクター",
      col_window: "ウィンドウ",
      col_lag: "遅延",
      col_headroom: "余裕",
      col_breaks: "チェーンの断絶",
      empty: "oplog を収集している接続はまだありません。",
      status_running: "実行中",
      status_failed: "失敗中",
      status_stopped: "停止",
      status_disabled: "無効",
      no_window: "ウィンドウはまだありません（ベースバックアップ待ち）",
      enable_title: "ポイントインタイムリカバリを有効にする",
      enable_hint: "local.oplog.rs を読み取れるユーザー（backup ロール、または local の read）のレプリカセットと、age キーによるバックアップ暗号化が必要です。oplog には削除されたデータも残ります。",
      connection: "接続",
      base_cron: "ベースバックアップのスケジュール（cron）",
      chunk_seconds: "チャンク間隔（秒）",
      keep_count: "保持するベースバックアップ数",
      keep_days: "または日数",
      base_on_gap: "ギャップでチェーンが切れたらベースバックアップを取得する",
      chain_test_cron: "チェーンテストのスケジュール（cron、任意）",
      chain_test_hint: "ベースバックアップを次のベースバックアップの整合点まで一時データベースに復元し、そのマニフェストと比較してから削除します。空にするとチェーンテストは無効です。",
      enable: "有効にする",
      disable: "無効にする",
      resume: "有効にする",
      base_now: "ベースバックアップを取得",
      delete: "削除",
      delete_title: "この PITR ストリームを削除しますか？",
      delete_body: "oplog チャンクは削除され、削除猶予期間の終了後に完全に消去されます。その後、ストリームをもう一度削除してください。ベースバックアップはバックアップとして残ります。",
      enabled_toast: "{name} のポイントインタイムリカバリを有効にしました。",
      updated_toast: "{name} の PITR ストリームを更新しました。",
      base_started: "ベースバックアップ {id} を開始しました。",
      deleted: "PITR ストリームを削除しました。",
      no_connections: "まずレプリカセット接続を追加してください。",
      invalid: "スケジュール、間隔（15-900 秒）、保持設定を確認してください。",
      restore_action: "時点を指定して復元",
      restore_title: "ポイントインタイム復元",
      restore_hint: "すべてのデータベース（または指定したもの）が <db>_rescue_<timestamp> という新しいデータベースに復元されます。既存のデータには触れません。時刻はプライマリの時計による UTC です。",
      restore_window: "ウィンドウ",
      restore_at: "復元する時点（UTC）",
      restore_databases: "データベース（任意、カンマ区切り）",
      restore_all: "すべてのデータベース",
      restore_cancel: "キャンセル",
      restore_check: "確認",
      restore_start: "復元",
      restore_open: "継続中",
      restore_range: "{start} から {end} まで（UTC）。",
      restore_no_window: "このストリームには復元できるウィンドウがまだありません。",
      restore_outside: "ウィンドウ内の時刻を選んでください。",
      restore_checks_failed: "事前チェックに失敗しました。修正するか別の時刻を選んでください。",
      restore_plan_base: "ベースバックアップ",
      restore_plan_chunks: "Oplog",
      restore_plan_chunks_value: "{n} チャンク、{size}",
      restore_plan_size: "ベースバックアップのサイズ",
      restore_plan_clones: "新しいデータベース",
      restore_plan_rto: "推定所要時間",
      restore_rto_default: "既定の速度による",
      restore_rto_chain_test: "直近のチェーンテストによる",
      restore_confirm_title: "ポイントインタイム復元を開始しますか？",
      restore_confirm_body: "{at} の時点に、名前が {suffix} で終わる新しいデータベースへ復元します。既存のデータには触れません。推定所要時間: {duration}。",
      restore_started: "ポイントインタイム復元 {id} を開始しました。"
    },
    notify: {
      events: {
        pitr_chain_broken: "PITR oplog チェーンが切断",
        pitr_diverged: "PITR oplog が分岐",
        pitr_lag_high: "PITR コレクターが遅延",
        pitr_lag_recovered: "PITR コレクターが追いついた",
        pitr_window_low: "PITR oplog の余裕が少ない",
        pitr_collector_failed: "PITR コレクターが失敗",
        pitr_collector_recovered: "PITR コレクターが回復"
      }
    },
    readiness: {
      reason_pitr_chain_broken: "PITR チェーン切断",
      reason_pitr_collector_down: "PITR コレクター停止",
      reason_pitr_lag_high: "PITR 遅延大",
      reason_pitr_window_low: "PITR 余裕不足",
      reason_pitr_no_window: "PITR ウィンドウがまだありません",
      reason_pitr_chain_test_failed: "PITR チェーンテストが失敗",
      rpo_pitr: "PITR：oplog を取得済み",
      rpo_pitr_note: "復旧時点は PITR ストリームによるものです。その時点までの oplog は取得済みで、ポイントインタイム復元が可能です（実験的）。"
    }
  },
  ru: {
    pitr: {
      title: "Восстановление на момент времени",
      experimental: "Экспериментально",
      desc: "Собирает oplog подключения к набору реплик в зашифрованные фрагменты и делает базовые резервные копии всего экземпляра, чтобы его можно было восстановить на любой момент внутри окна в новые базы данных.",
      table_label: "Потоки восстановления на момент времени",
      col_connection: "Подключение",
      col_status: "Сборщик",
      col_window: "Окно",
      col_lag: "Отставание",
      col_headroom: "Запас",
      col_breaks: "Разрывы цепочки",
      empty: "Пока ни одно подключение не собирает свой oplog.",
      status_running: "Работает",
      status_failed: "Сбоит",
      status_stopped: "Остановлен",
      status_disabled: "Отключён",
      no_window: "Окна пока нет (ожидается базовая копия)",
      enable_title: "Включить восстановление на момент времени",
      enable_hint: "Нужен набор реплик, пользователь которого может читать local.oplog.rs (роль backup или read на local), и шифрование резервных копий ключами age: oplog хранит и удалённые данные.",
      connection: "Подключение",
      base_cron: "Расписание базовых копий (cron)",
      chunk_seconds: "Интервал фрагмента (секунды)",
      keep_count: "Хранить базовых копий",
      keep_days: "или дней",
      base_on_gap: "Делать базовую копию, когда разрыв обрывает цепочку",
      chain_test_cron: "Расписание теста цепочки (cron, необязательно)",
      chain_test_hint: "Восстанавливает базовую копию на согласованную точку следующей во временные базы данных, сравнивает их с её манифестом и удаляет. Пусто — тесты цепочки отключены.",
      enable: "Включить",
      disable: "Отключить",
      resume: "Включить",
      base_now: "Сделать базовую копию",
      delete: "Удалить",
      delete_title: "Удалить этот поток PITR?",
      delete_body: "Его фрагменты oplog удаляются и очищаются по окончании льготного периода удаления; тогда удалите поток снова. Его базовые копии остаются резервными копиями.",
      enabled_toast: "Восстановление на момент времени включено для {name}.",
      updated_toast: "Поток PITR подключения {name} обновлён.",
      base_started: "Базовая копия {id} запущена.",
      deleted: "Поток PITR удалён.",
      no_connections: "Сначала добавьте подключение к набору реплик.",
      invalid: "Проверьте расписание, интервал (15-900 секунд) и хранение.",
      restore_action: "Восстановить на момент",
      restore_title: "Восстановление на момент времени",
      restore_hint: "Каждая база данных (или указанные вами) восстанавливается в новую базу с именем <db>_rescue_<timestamp>; существующие данные не затрагиваются. Время указано в UTC по часам первичного узла.",
      restore_window: "Окно",
      restore_at: "Восстановить на (UTC)",
      restore_databases: "Базы данных (необязательно, через запятую)",
      restore_all: "все базы данных",
      restore_cancel: "Отмена",
      restore_check: "Проверить",
      restore_start: "Восстановить",
      restore_open: "открыто",
      restore_range: "С {start} по {end} (UTC).",
      restore_no_window: "У этого потока пока нет окна для восстановления.",
      restore_outside: "Выберите время внутри окна.",
      restore_checks_failed: "Предварительная проверка не пройдена; исправьте её или выберите другое время.",
      restore_plan_base: "Базовая копия",
      restore_plan_chunks: "Oplog",
      restore_plan_chunks_value: "фрагментов: {n}, {size}",
      restore_plan_size: "Размер базовой копии",
      restore_plan_clones: "Новые базы данных",
      restore_plan_rto: "Ожидаемая длительность",
      restore_rto_default: "по скоростям по умолчанию",
      restore_rto_chain_test: "по последнему тесту цепочки",
      restore_confirm_title: "Запустить восстановление на момент времени?",
      restore_confirm_body: "Восстанавливает на {at} в новые базы данных с окончанием {suffix}; существующие данные не затрагиваются. Ожидаемая длительность: {duration}.",
      restore_started: "Восстановление на момент времени {id} запущено."
    },
    notify: {
      events: {
        pitr_chain_broken: "Цепочка oplog PITR разорвана",
        pitr_diverged: "Oplog PITR разошёлся",
        pitr_lag_high: "Сборщик PITR отстаёт",
        pitr_lag_recovered: "Сборщик PITR догнал",
        pitr_window_low: "Мало запаса oplog PITR",
        pitr_collector_failed: "Сборщик PITR сбоит",
        pitr_collector_recovered: "Сборщик PITR восстановлен"
      }
    },
    readiness: {
      reason_pitr_chain_broken: "Цепочка PITR разорвана",
      reason_pitr_collector_down: "Сборщик PITR не работает",
      reason_pitr_lag_high: "Большое отставание PITR",
      reason_pitr_window_low: "Мало запаса PITR",
      reason_pitr_no_window: "Окна PITR пока нет",
      reason_pitr_chain_test_failed: "Тест цепочки PITR не пройден",
      rpo_pitr: "PITR: oplog сохраняется",
      rpo_pitr_note: "Точка восстановления взята из потока PITR: oplog до неё сохранён, и возможно восстановление на момент времени (экспериментально)."
    }
  }
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(PITR_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], PITR_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// State and rendering
// ---------------------------------------------------------------------------

const PITR_REFRESH_MS = 30000;
const pitr = { streams: [], loaded: false, timer: null };

// pitrConnectionName names a stream's connection.
function pitrConnectionName(id) {
  return typeof connectionName === "function" ? connectionName(id) : id;
}

// pitrStatusBadge renders the collector's state of a stream.
function pitrStatusBadge(s) {
  const st = s.stream || {};
  if (!st.enabled) return statusBadge("neutral", t("pitr.status_disabled"));
  const state = s.state || {};
  if (state.status === "failed") return statusBadge("danger", t("pitr.status_failed"), state.last_error || "");
  if (!s.running) return statusBadge("warn", t("pitr.status_stopped"));
  return statusBadge("success", t("pitr.status_running"));
}

// pitrWindow renders the newest point-in-time window of a stream.
function pitrWindow(s) {
  const list = s.windows || [];
  if (list.length === 0) return `<span class="muted">${escapeHtml(t("pitr.no_window"))}</span>`;
  const w = list[list.length - 1];
  const start = parseDate(w.start_time);
  const end = parseDate(w.end_time);
  if (!start || !end) return mutedDash();
  const abs = d => (typeof formatAbsolute === "function" ? formatAbsolute(d) : d.toLocaleString());
  return `<span>${escapeHtml(abs(start))} → ${escapeHtml(abs(end))}</span>`;
}

// pitrSeconds renders a duration in seconds, or a dash.
function pitrSeconds(value, warn) {
  if (value === null || value === undefined) return mutedDash();
  const text = escapeHtml(formatDuration(value));
  return warn ? `<span class="text-danger">${text}</span>` : text;
}

function pitrRender() {
  const tbody = document.getElementById("pitr-tbody");
  if (!tbody) return;
  if (pitr.streams.length === 0) {
    setTbody(tbody, `<tr class="empty-row"><td colspan="7"><div class="empty-state"><p>${escapeHtml(t("pitr.empty"))}</p></div></td></tr>`);
  } else {
    setTbody(tbody, pitr.streams.map(s => {
      const st = s.stream || {};
      const id = escapeHtml(st.id);
      const live = s.live || {};
      const toggle = st.enabled
        ? `<button type="button" class="btn btn-secondary btn-sm" data-action="pitr-toggle" data-id="${id}" data-enable="false">${escapeHtml(t("pitr.disable"))}</button>`
        : `<button type="button" class="btn btn-secondary btn-sm" data-action="pitr-toggle" data-id="${id}" data-enable="true">${escapeHtml(t("pitr.resume"))}</button>
           <button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="pitr-delete" data-id="${id}">${escapeHtml(t("pitr.delete"))}</button>`;
      return `<tr>
        <td class="cell-primary">${ellipsis(pitrConnectionName(st.connection_id))}<div class="cell-sub mono">${escapeHtml(st.replica_set || "")}</div></td>
        <td>${pitrStatusBadge(s)}</td>
        <td>${pitrWindow(s)}</td>
        <td>${pitrSeconds(s.lag_seconds, s.state && s.state.lag_since)}</td>
        <td>${pitrSeconds(s.headroom_seconds, live.window_low)}</td>
        <td>${s.chain_breaks ? `<span class="text-danger">${escapeHtml(String(s.chain_breaks))}</span>` : "0"}</td>
        <td class="col-actions"><div class="row-actions">
          ${(s.windows || []).length ? `<button type="button" class="btn btn-primary btn-sm" data-action="pitr-restore" data-id="${id}">${escapeHtml(t("pitr.restore_action"))}</button>` : ""}
          <button type="button" class="btn btn-secondary btn-sm" data-action="pitr-base" data-id="${id}">${escapeHtml(t("pitr.base_now"))}</button>
          ${toggle}
        </div></td>
      </tr>`;
    }).join(""));
  }
  pitrFillConnections();
}

// pitrFillConnections offers the connections without a stream in the form.
function pitrFillConnections() {
  const select = document.getElementById("pitr-connection");
  if (!select || typeof state !== "object" || !state.loaded || !state.loaded.connections) return;
  const taken = new Set(pitr.streams.map(s => (s.stream || {}).connection_id));
  const current = select.value;
  select.textContent = "";
  (state.connections || []).filter(c => !taken.has(c.id)).forEach(c => {
    const opt = document.createElement("option");
    opt.value = c.id;
    opt.textContent = c.name;
    select.appendChild(opt);
  });
  if (current) select.value = current;
}

async function pitrLoad() {
  try {
    const json = await apiJSON("/api/v1/pitr/streams");
    if (!json.success) return;
    pitr.streams = json.data || [];
    pitr.loaded = true;
    pitrRender();
  } catch (err) {
    console.error("Failed to load PITR streams:", err);
  }
}

// pitrVisible reports whether the Connections tab is shown.
function pitrVisible() {
  const tab = document.getElementById("tab-connections");
  return !!tab && !tab.hidden && document.visibilityState !== "hidden";
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

// pitrRequest sends a stream request and shows its error as a toast.
async function pitrRequest(url, method, body) {
  const opts = { method, headers: { "Content-Type": "application/json" } };
  if (body !== undefined) opts.body = JSON.stringify(body);
  const json = await apiJSON(url, opts);
  if (!json.success) {
    showToast(json.error || t("toasts.request_failed"), "error");
    return null;
  }
  return json;
}

async function pitrCreate(e) {
  e.preventDefault();
  const connection = getValue("pitr-connection");
  if (!connection) {
    showFormError("pitr-error", t("pitr.no_connections"));
    return;
  }
  const num = id => parseInt(getValue(id), 10);
  const body = {
    connection_id: connection,
    enabled: true,
    base_cron: getValue("pitr-base-cron").trim(),
    chunk_seconds: num("pitr-chunk-seconds"),
    base_keep_count: num("pitr-keep-count"),
    base_keep_days: num("pitr-keep-days"),
    base_on_gap: !!(document.getElementById("pitr-base-on-gap") || {}).checked,
    chain_test_cron: getValue("pitr-chain-test-cron").trim()
  };
  if (!body.base_cron || !(body.chunk_seconds >= 15 && body.chunk_seconds <= 900) || isNaN(body.base_keep_count) || isNaN(body.base_keep_days)) {
    showFormError("pitr-error", t("pitr.invalid"));
    return;
  }
  const submit = e.submitter || document.querySelector("#form-pitr [type=submit]");
  if (submit) submit.disabled = true;
  try {
    const json = await apiJSON("/api/v1/pitr/streams", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body)
    });
    if (!json.success) {
      showFormError("pitr-error", json.error || t("toasts.request_failed"));
      return;
    }
    hideFormError("pitr-error");
    showToast(tf("pitr.enabled_toast", { name: pitrConnectionName(connection) }));
    await pitrLoad();
  } catch (err) {
    showFormError("pitr-error", err.message);
  } finally {
    if (submit) submit.disabled = false;
  }
}

async function pitrAction(btn) {
  const id = btn.dataset.id;
  const s = pitr.streams.find(x => x.stream && x.stream.id === id);
  const name = s ? pitrConnectionName(s.stream.connection_id) : id;
  btn.disabled = true;
  try {
    switch (btn.dataset.action) {
      case "pitr-restore":
        pitrOpenRestore(id);
        return;
      case "pitr-toggle": {
        const json = await pitrRequest(`/api/v1/pitr/streams/${encodeURIComponent(id)}`, "PATCH", { enabled: btn.dataset.enable === "true" });
        if (json) showToast(tf("pitr.updated_toast", { name }));
        break;
      }
      case "pitr-base": {
        const json = await pitrRequest(`/api/v1/pitr/streams/${encodeURIComponent(id)}/base`, "POST");
        if (json && json.data) showToast(tf("pitr.base_started", { id: json.data.id }));
        break;
      }
      case "pitr-delete": {
        const ok = typeof confirmDialog === "function"
          ? await confirmDialog({ title: t("pitr.delete_title"), body: t("pitr.delete_body"), danger: true, confirmLabel: t("pitr.delete") })
          : false;
        if (!ok) break;
        const json = await pitrRequest(`/api/v1/pitr/streams/${encodeURIComponent(id)}`, "DELETE");
        if (json) showToast(t("pitr.deleted"));
        break;
      }
    }
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    btn.disabled = false;
    await pitrLoad();
  }
}

// ---------------------------------------------------------------------------
// Point-in-time restore wizard: window and time, preflight, confirm
// ---------------------------------------------------------------------------

const pitrRestore = { stream: null, windows: [], body: null, plan: null };

// pitrUTCInput renders a date as the value of a datetime-local input read as UTC.
function pitrUTCInput(d) {
  return d.toISOString().slice(0, 19);
}

// pitrInputDate reads a datetime-local value as a UTC date, or null.
function pitrInputDate(value) {
  if (!value) return null;
  const v = value.length === 16 ? value + ":00" : value;
  const d = new Date(v + "Z");
  return isNaN(d.getTime()) ? null : d;
}

// pitrUTC formats a date as RFC 3339 in UTC, without fractions.
function pitrUTC(d) {
  return d.toISOString().replace(/\.\d{3}Z$/, "Z");
}

// pitrRestoreBounds returns the first and last second of the selected window a
// restore can go to: from the base's consistent point to one second before the
// newest collected entry.
function pitrRestoreBounds() {
  const w = pitrRestore.windows[parseInt(getValue("pitr-restore-window"), 10) || 0];
  if (!w) return null;
  const start = parseDate(w.start_time);
  const end = parseDate(w.end_time);
  if (!start || !end) return null;
  const last = new Date(end.getTime() - 1000);
  return last < start ? null : { start, last };
}

// pitrRestoreReset forgets the checked plan: the time or databases changed.
function pitrRestoreReset() {
  pitrRestore.body = null;
  pitrRestore.plan = null;
  const plan = document.getElementById("pitr-restore-plan");
  if (plan) { plan.hidden = true; plan.textContent = ""; }
  const submit = document.getElementById("pitr-restore-submit");
  if (submit) submit.disabled = true;
  hideFormError("pitr-restore-error");
}

// pitrRestoreWindow constrains the time picker to the selected window.
function pitrRestoreWindow() {
  pitrRestoreReset();
  const input = document.getElementById("pitr-restore-at");
  const range = document.getElementById("pitr-restore-range");
  const b = pitrRestoreBounds();
  if (!input || !range) return;
  if (!b) {
    input.min = input.max = input.value = "";
    range.textContent = t("pitr.restore_no_window");
    return;
  }
  input.min = pitrUTCInput(b.start);
  input.max = pitrUTCInput(b.last);
  input.value = pitrUTCInput(b.last);
  range.textContent = tf("pitr.restore_range", { start: pitrUTC(b.start), end: pitrUTC(b.last) });
}

// pitrOpenRestore opens the wizard for stream id.
function pitrOpenRestore(id) {
  const s = pitr.streams.find(x => x.stream && x.stream.id === id);
  const form = document.getElementById("form-pitr-restore");
  if (!s || !form) return;
  pitrRestore.stream = s;
  pitrRestore.windows = s.windows || [];
  document.getElementById("pitr-restore-conn").textContent = pitrConnectionName(s.stream.connection_id);
  const select = document.getElementById("pitr-restore-window");
  select.textContent = "";
  pitrRestore.windows.forEach((w, i) => {
    const opt = document.createElement("option");
    opt.value = String(i);
    opt.textContent = `${w.start_time} → ${w.end_time}${w.open ? " (" + t("pitr.restore_open") + ")" : ""}`;
    select.appendChild(opt);
  });
  select.value = String(Math.max(0, pitrRestore.windows.length - 1));
  setValue("pitr-restore-databases", "");
  form.hidden = false;
  pitrRestoreWindow();
  form.scrollIntoView({ block: "nearest" });
}

// pitrRestoreClose hides the wizard.
function pitrRestoreClose() {
  const form = document.getElementById("form-pitr-restore");
  if (form) form.hidden = true;
  pitrRestore.stream = null;
  pitrRestoreReset();
}

// pitrRestoreRequest returns the body of the restore and its preflight, or null
// (with the error shown) for a time outside the window.
function pitrRestoreRequest() {
  const at = pitrInputDate(getValue("pitr-restore-at"));
  const b = pitrRestoreBounds();
  if (!at || !b || at < b.start || at > b.last) {
    showFormError("pitr-restore-error", t("pitr.restore_outside"));
    return null;
  }
  const body = { pitr: { stream_id: pitrRestore.stream.stream.id, at: pitrUTC(at) } };
  const dbs = getValue("pitr-restore-databases").split(",").map(x => x.trim()).filter(Boolean);
  if (dbs.length) body.databases = dbs;
  return body;
}

// pitrPlanHTML renders the plan and the checks of a preflight.
function pitrPlanHTML(res) {
  const p = res.pitr;
  const rows = [];
  if (p) {
    const consistent = parseDate(p.base_consistent_at);
    rows.push([t("pitr.restore_plan_base"), `${p.base_id} (${consistent ? pitrUTC(consistent) : ""})`]);
    rows.push([t("pitr.restore_plan_chunks"), tf("pitr.restore_plan_chunks_value", { n: p.chunks, size: formatBytes(p.oplog_bytes) })]);
    rows.push([t("pitr.restore_plan_size"), formatBytes(p.base_bytes)]);
    rows.push([t("pitr.restore_plan_clones"), `<db>${p.clone_suffix}`]);
    const note = p.estimate_from === "chain_test" ? t("pitr.restore_rto_chain_test") : t("pitr.restore_rto_default");
    rows.push([t("pitr.restore_plan_rto"), `${formatDuration(p.estimated_seconds)} (${note})`]);
  }
  const plan = rows.map(([k, v]) => `<dt>${escapeHtml(k)}</dt><dd>${escapeHtml(v)}</dd>`).join("");
  const checks = (res.checks || []).map(c => {
    const kind = c.status === "fail" ? "danger" : c.status === "warn" ? "warn" : "success";
    return `<li>${statusBadge(kind, c.status)} <span class="mono">${escapeHtml(c.id)}</span> ${escapeHtml(c.message)}</li>`;
  }).join("");
  return `<dl class="pitr-plan-list">${plan}</dl><ul class="pitr-checks">${checks}</ul>`;
}

// pitrRestoreCheck runs the preflight of the chosen time and shows the plan.
async function pitrRestoreCheck() {
  pitrRestoreReset();
  const body = pitrRestoreRequest();
  if (!body) return;
  const btn = document.getElementById("pitr-restore-check");
  btn.disabled = true;
  try {
    const json = await apiJSON("/api/v1/restores/preflight", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body)
    });
    if (!json.success) {
      showFormError("pitr-restore-error", json.error || t("toasts.request_failed"));
      return;
    }
    const res = json.data || {};
    const plan = document.getElementById("pitr-restore-plan");
    plan.innerHTML = pitrPlanHTML(res);
    plan.hidden = false;
    if (!res.ok) {
      showFormError("pitr-restore-error", t("pitr.restore_checks_failed"));
      return;
    }
    pitrRestore.body = body;
    pitrRestore.plan = res.pitr || null;
    document.getElementById("pitr-restore-submit").disabled = false;
  } catch (err) {
    showFormError("pitr-restore-error", err.message);
  } finally {
    btn.disabled = false;
  }
}

// pitrRestoreSubmit confirms and starts the checked restore.
async function pitrRestoreSubmit(e) {
  e.preventDefault();
  const body = pitrRestore.body;
  if (!body) return;
  const p = pitrRestore.plan || {};
  const ok = typeof confirmDialog === "function"
    ? await confirmDialog({
      title: t("pitr.restore_confirm_title"),
      body: tf("pitr.restore_confirm_body", { at: body.pitr.at, suffix: p.clone_suffix || "_rescue_…", duration: formatDuration(p.estimated_seconds || 0) }),
      confirmLabel: t("pitr.restore_start")
    })
    : false;
  if (!ok) return;
  const submit = document.getElementById("pitr-restore-submit");
  submit.disabled = true;
  try {
    const json = await apiJSON("/api/v1/restore", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body)
    });
    if (!json.success) {
      showFormError("pitr-restore-error", json.error || t("toasts.request_failed"));
      submit.disabled = false;
      return;
    }
    showToast(tf("pitr.restore_started", { id: (json.data || {}).id || "" }));
    pitrRestoreClose();
  } catch (err) {
    showFormError("pitr-restore-error", err.message);
    submit.disabled = false;
  }
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function pitrSetup() {
  if (typeof ROLE_ACTION_SCOPES === "object") {
    Object.assign(ROLE_ACTION_SCOPES, { "pitr-toggle": "admin", "pitr-delete": "admin", "pitr-base": "operator", "pitr-restore": "admin" });
  }
  const restoreForm = document.getElementById("form-pitr-restore");
  if (restoreForm) {
    restoreForm.addEventListener("submit", pitrRestoreSubmit);
    document.getElementById("pitr-restore-window").addEventListener("change", pitrRestoreWindow);
    document.getElementById("pitr-restore-at").addEventListener("input", pitrRestoreReset);
    document.getElementById("pitr-restore-databases").addEventListener("input", pitrRestoreReset);
    document.getElementById("pitr-restore-check").addEventListener("click", pitrRestoreCheck);
    document.getElementById("pitr-restore-cancel").addEventListener("click", pitrRestoreClose);
  }
  document.addEventListener("click", (e) => {
    const target = e.target instanceof Element ? e.target : null;
    const btn = target ? target.closest("[data-action^='pitr-']") : null;
    if (btn && !btn.disabled) {
      pitrAction(btn);
      return;
    }
    if (target && target.closest("[data-tab='tab-connections']")) {
      // The connections list may have changed: refresh the form's choices too.
      setTimeout(pitrLoad, 0);
    }
  });
  const form = document.getElementById("form-pitr");
  if (form) form.addEventListener("submit", pitrCreate);
  pitr.timer = setInterval(() => {
    if (pitrVisible()) pitrLoad();
  }, PITR_REFRESH_MS);
  if (pitrVisible()) pitrLoad();
}

document.addEventListener("DOMContentLoaded", pitrSetup);
