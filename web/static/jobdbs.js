/**
 * MongoRescue dashboard: jobs that back up several databases.
 *
 * The job form's database selector (Single / Selected / All / Pattern) with its
 * live preview, the same selector (Single / Selected / All) in the Backup now
 * dialog, the Database column of the jobs table, and the job details' run history
 * grouped by run with the newly discovered databases. Loaded after app.js, runs.js
 * and trust.js, whose helpers it uses; app.js calls the jobDbs* and instantDbs*
 * hooks.
 *
 * Same security invariants as app.js: every server value goes through
 * escapeHtml() or textContent, interaction goes through listeners bound here or
 * delegated data-action buttons ("jobdb-*"), and IDs in URLs are wrapped in
 * encodeURIComponent().
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const JOBDB_TRANSLATIONS = {
  en: {
    instantdb: {
      list_hint: "Backs up each checked database into its own backup, all in one run.",
      all_hint: "Backs up every database the connection lists except admin, config and local and the ones checked here.",
      list_failed: "The databases could not be listed; choose Single to enter a name.",
      summary_n: "{n} databases selected · about {size} in total",
      summary_n_nosize: "{n} databases selected",
      too_many: "At most {max} databases can be backed up at once.",
      started_n: "Backup of {n} databases started.",
      busy_list: "Skipped, already being backed up: {list}.",
      view_run: "View run",
    },
    filters: { run_filter: "Run {id}" },
    jobdb: {
      n_of_m: "{ok}/{n} databases",
      last_run_result: "Last run result",
      mode_label: "Databases",
      mode_single: "Single",
      mode_list: "Selected",
      mode_all: "All",
      mode_pattern: "Pattern",
      search_placeholder: "Search databases",
      list_hint: "Backs up the checked databases, each into its own backup. A database that no longer exists is reported as failed in the run; the others still run.",
      list_none: "This connection lists no databases to choose from.",
      list_failed: "The databases could not be listed; enter them as patterns instead.",
      all_hint: "Backs up every database of the connection except admin, config and local and the ones checked here.",
      exclude_label: "Exclude",
      include_label: "Include patterns",
      include_placeholder: "prod_*, sales_??",
      exclude_patterns_label: "Exclude patterns",
      exclude_placeholder: "*_tmp",
      pattern_hint: "* matches any characters, ? exactly one; case-sensitive. Separate patterns with commas. admin, config and local are never included.",
      auto_label: "Add new databases automatically",
      auto_hint: "On: databases created later that the selection matches are backed up from the next run on, and a job.databases_added notification is sent. Off: the job keeps backing up the databases it has now; new ones are listed in the run summary, where you can add them.",
      parallelism_label: "Databases at a time",
      parallelism_hint: "Every database keeps its own run lock. 1 backs them up one after another.",
      preview_n: "With these settings these {n} databases will be backed up:",
      preview_one: "With these settings 1 database will be backed up:",
      preview_none: "With these settings no database would be backed up.",
      preview_loading: "Checking the connection's databases…",
      preview_failed: "The databases could not be checked: {error}",
      preview_new_excluded: "{n} new database(s) not included: {list}",
      preview_new_included: "{n} new database(s) will be added: {list}",
      preview_missing: "Not found on the server: {list}",
      preview_more: "+{n} more",
      need_selection: "Check at least one database.",
      all_excluded: "All databases are excluded. Uncheck at least one to back it up.",
      colls_toggle: "Collections of {db}",
      colls_loading: "Loading collections…",
      colls_failed: "The collections could not be listed; enter their names, separated by commas.",
      colls_none: "This database has no collections.",
      colls_manual_include: "Only these collections of {db} (empty: all)",
      colls_manual_exclude: "Collections of {db} to skip",
      colls_all_except: "all except {list}",
      tree_hint_list: "Expand a database to back up only some of its collections: checked collections are backed up.",
      tree_hint_all: "Expand a database to skip some of its collections: checked collections are excluded. Databases found later are backed up whole.",
      summary_colls: "{db}: {colls}",
      filtered_badge: "Filtered",
      filtered_only: "Only these collections: {list}",
      filtered_without: "Without these collections: {list}",
      need_pattern: "Enter at least one include pattern.",
      cell_n: "{n} databases",
      cell_all: "All",
      cell_all_n: "All ({n})",
      cell_pattern_n: "{pattern} ({n})",
      summary_list: "Selected: {list}",
      summary_all: "All databases",
      summary_all_except: "All databases except {list}",
      summary_pattern: "Matching {list}",
      summary_pattern_except: "Matching {include}, except {exclude}",
      summary_plus: "plus {list}",
      summary_auto: "new databases are added automatically",
      summary_frozen: "new databases are reported, not added",
      summary_parallel: "{n} at a time",
      runs_title: "Runs",
      runs_empty: "No runs yet.",
      runs_failed: "The runs could not be loaded.",
      run_line: "Run of {time} — {ok}/{n} databases ok",
      run_new_n: "{n} new databases not included",
      run_added_n: "{n} databases added",
      run_status_ok: "OK",
      run_status_partial: "Partial",
      run_status_failed: "Failed",
      run_status_cancelled: "Cancelled",
      run_status_running: "Running",
      new_title: "Newly discovered databases",
      new_hint: "These databases appeared since the job was saved and are not backed up yet.",
      add_to_job: "Add to job",
      added_toast: "{db} is now backed up by this job",
      add_failed: "The database could not be added to the job.",
      not_found: "Database not found",
      rt_databases: "Databases to test",
      rt_rotate: "One per run, taking turns",
      rt_all: "Every database of the run",
      stop_confirm: "Stop the current run of this job? The running database's backup is cancelled and the databases still waiting are skipped. Databases already backed up keep their backups.",
      stop_requested: "Stopping the job's run.",
      retention_per_db: "Per database: {list}",
      reason_system: "system database",
      reason_excluded: "excluded",
      reason_not_matched: "no pattern matches",
      reason_not_selected: "not selected",
      reason_new: "new",
      reason_not_found: "not found",
    },
    notify: { events: { job_databases_added: "New databases added to a job" } },
    overview: {
      spark_run_multi: "{when} — {status}: {ok}/{n}",
      spark_failed_dbs: "failed: {list}",
      spark_label_multi: "Last {n} runs: {ok} ok, {partial} partial, {failed} failed",
      att_partial_last: "{job}: the last run backed up {ok} of {n} databases {when}",
      spark_partial: "{ok} of {n} databases",
    },
  },
  tr: {
    instantdb: {
      list_hint: "İşaretli her veritabanını ayrı bir yedeğe, hepsini tek bir çalıştırmada yedekler.",
      all_hint: "Bağlantının listelediği admin, config, local ve burada işaretlenenler dışındaki tüm veritabanlarını yedekler.",
      list_failed: "Veritabanları listelenemedi; bir ad girmek için Tek'i seçin.",
      summary_n: "{n} veritabanı seçildi · toplam yaklaşık {size}",
      summary_n_nosize: "{n} veritabanı seçildi",
      too_many: "Aynı anda en fazla {max} veritabanı yedeklenebilir.",
      started_n: "{n} veritabanının yedeklemesi başladı.",
      busy_list: "Zaten yedeklendiği için atlandı: {list}.",
      view_run: "Çalıştırmayı göster",
    },
    filters: { run_filter: "Çalıştırma {id}" },
    jobdb: {
      n_of_m: "{ok}/{n} veritabanı",
      last_run_result: "Son çalıştırma sonucu",
      mode_label: "Veritabanı",
      mode_single: "Tek",
      mode_list: "Seçili",
      mode_all: "Tümü",
      mode_pattern: "Desen",
      search_placeholder: "Veritabanı ara",
      list_hint: "İşaretli veritabanlarını, her birini ayrı bir yedeğe yedekler. Artık var olmayan bir veritabanı çalıştırmada başarısız olarak bildirilir; diğerleri yine yedeklenir.",
      list_none: "Bu bağlantıda seçilebilecek veritabanı yok.",
      list_failed: "Veritabanları listelenemedi; bunun yerine desen girin.",
      all_hint: "Bağlantının admin, config, local ve burada işaretlenenler dışındaki tüm veritabanlarını yedekler.",
      exclude_label: "Hariç tut",
      include_label: "Dahil desenleri",
      include_placeholder: "prod_*, sales_??",
      exclude_patterns_label: "Hariç desenleri",
      exclude_placeholder: "*_tmp",
      pattern_hint: "* herhangi karakterlerle, ? tam bir karakterle eşleşir; büyük/küçük harfe duyarlıdır. Desenleri virgülle ayırın. admin, config ve local hiçbir zaman dahil edilmez.",
      auto_label: "Yeni veritabanlarını otomatik ekle",
      auto_hint: "Açık: sonradan oluşturulan ve seçime uyan veritabanları bir sonraki çalıştırmadan itibaren yedeklenir ve job.databases_added bildirimi gönderilir. Kapalı: iş şu anki veritabanlarını yedeklemeye devam eder; yenileri çalıştırma özetinde listelenir, oradan ekleyebilirsiniz.",
      parallelism_label: "Aynı anda veritabanı",
      parallelism_hint: "Her veritabanı kendi çalıştırma kilidini korur. 1 değeri onları sırayla yedekler.",
      preview_n: "Bu ayarla şu {n} veritabanı yedeklenecek:",
      preview_one: "Bu ayarla şu 1 veritabanı yedeklenecek:",
      preview_none: "Bu ayarla hiçbir veritabanı yedeklenmeyecek.",
      preview_loading: "Bağlantının veritabanları kontrol ediliyor…",
      preview_failed: "Veritabanları kontrol edilemedi: {error}",
      preview_new_excluded: "{n} yeni veritabanı dahil değil: {list}",
      preview_new_included: "{n} yeni veritabanı eklenecek: {list}",
      preview_missing: "Sunucuda bulunamadı: {list}",
      preview_more: "+{n} daha",
      need_selection: "En az bir veritabanı işaretleyin.",
      all_excluded: "Tüm veritabanları hariç tutuldu. Yedeklemek için en az birinin işaretini kaldırın.",
      colls_toggle: "{db} koleksiyonları",
      colls_loading: "Koleksiyonlar yükleniyor…",
      colls_failed: "Koleksiyonlar listelenemedi; adlarını virgülle ayırarak girin.",
      colls_none: "Bu veritabanında koleksiyon yok.",
      colls_manual_include: "{db} veritabanının yalnızca bu koleksiyonları (boş: tümü)",
      colls_manual_exclude: "{db} veritabanında atlanacak koleksiyonlar",
      colls_all_except: "{list} dışındaki tümü",
      tree_hint_list: "Bir veritabanının yalnızca bazı koleksiyonlarını yedeklemek için onu genişletin: işaretli koleksiyonlar yedeklenir.",
      tree_hint_all: "Bir veritabanının bazı koleksiyonlarını atlamak için onu genişletin: işaretli koleksiyonlar hariç tutulur. Sonradan bulunan veritabanları tümüyle yedeklenir.",
      summary_colls: "{db}: {colls}",
      filtered_badge: "Filtreli",
      filtered_only: "Yalnızca bu koleksiyonlar: {list}",
      filtered_without: "Bu koleksiyonlar hariç: {list}",
      need_pattern: "En az bir dahil deseni girin.",
      cell_n: "{n} veritabanı",
      cell_all: "Tümü",
      cell_all_n: "Tümü ({n})",
      cell_pattern_n: "{pattern} ({n})",
      summary_list: "Seçili: {list}",
      summary_all: "Tüm veritabanları",
      summary_all_except: "{list} dışındaki tüm veritabanları",
      summary_pattern: "{list} ile eşleşenler",
      summary_pattern_except: "{include} ile eşleşenler, {exclude} hariç",
      summary_plus: "ayrıca {list}",
      summary_auto: "yeni veritabanları otomatik eklenir",
      summary_frozen: "yeni veritabanları bildirilir, eklenmez",
      summary_parallel: "aynı anda {n}",
      runs_title: "Çalıştırmalar",
      runs_empty: "Henüz çalıştırma yok.",
      runs_failed: "Çalıştırmalar yüklenemedi.",
      run_line: "{time} çalıştırması — {ok}/{n} veritabanı tamam",
      run_new_n: "{n} yeni veritabanı dahil değil",
      run_added_n: "{n} veritabanı eklendi",
      run_status_ok: "Tamam",
      run_status_partial: "Kısmen",
      run_status_failed: "Başarısız",
      run_status_cancelled: "İptal edildi",
      run_status_running: "Çalışıyor",
      new_title: "Yeni keşfedilen veritabanları",
      new_hint: "Bu veritabanları iş kaydedildikten sonra ortaya çıktı ve henüz yedeklenmiyor.",
      add_to_job: "İşe ekle",
      added_toast: "{db} artık bu işle yedekleniyor",
      add_failed: "Veritabanı işe eklenemedi.",
      not_found: "Veritabanı bulunamadı",
      rt_databases: "Test edilecek veritabanları",
      rt_rotate: "Her çalıştırmada biri, sırayla",
      rt_all: "Çalıştırmanın tüm veritabanları",
      stop_confirm: "Bu işin geçerli çalıştırması durdurulsun mu? Çalışan veritabanının yedeği iptal edilir, bekleyen veritabanları atlanır. Önceden yedeklenen veritabanları yedeklerini korur.",
      stop_requested: "İşin çalıştırması durduruluyor.",
      retention_per_db: "Veritabanı başına: {list}",
      reason_system: "sistem veritabanı",
      reason_excluded: "hariç tutuldu",
      reason_not_matched: "desen eşleşmiyor",
      reason_not_selected: "seçili değil",
      reason_new: "yeni",
      reason_not_found: "bulunamadı",
    },
    notify: { events: { job_databases_added: "İşe yeni veritabanları eklendi" } },
    overview: {
      spark_run_multi: "{when} — {status}: {ok}/{n}",
      spark_failed_dbs: "başarısız: {list}",
      spark_label_multi: "Son {n} çalıştırma: {ok} tamam, {partial} kısmen, {failed} başarısız",
      att_partial_last: "{job}: son çalıştırma {when} {n} veritabanından {ok} tanesini yedekledi",
      spark_partial: "{n} veritabanından {ok}",
    },
  },
  de: {
    instantdb: {
      list_hint: "Sichert jede markierte Datenbank in eine eigene Sicherung, alle in einem Lauf.",
      all_hint: "Sichert jede Datenbank, die die Verbindung auflistet, außer admin, config und local und den hier markierten.",
      list_failed: "Die Datenbanken konnten nicht aufgelistet werden; wählen Sie „Eine“, um einen Namen einzugeben.",
      summary_n: "{n} Datenbanken ausgewählt · insgesamt etwa {size}",
      summary_n_nosize: "{n} Datenbanken ausgewählt",
      too_many: "Es können höchstens {max} Datenbanken auf einmal gesichert werden.",
      started_n: "Sicherung von {n} Datenbanken gestartet.",
      busy_list: "Übersprungen, da bereits gesichert: {list}.",
      view_run: "Lauf anzeigen",
    },
    filters: { run_filter: "Lauf {id}" },
    jobdb: {
      n_of_m: "{ok}/{n} Datenbanken",
      last_run_result: "Ergebnis des letzten Laufs",
      mode_label: "Datenbanken",
      mode_single: "Eine",
      mode_list: "Ausgewählt",
      mode_all: "Alle",
      mode_pattern: "Muster",
      search_placeholder: "Datenbanken suchen",
      list_hint: "Sichert die markierten Datenbanken, jede in ein eigenes Backup. Eine Datenbank, die nicht mehr existiert, wird im Lauf als fehlgeschlagen gemeldet; die anderen laufen trotzdem.",
      list_none: "Diese Verbindung listet keine Datenbanken zur Auswahl.",
      list_failed: "Die Datenbanken konnten nicht aufgelistet werden; geben Sie stattdessen Muster ein.",
      all_hint: "Sichert alle Datenbanken der Verbindung außer admin, config und local und den hier markierten.",
      exclude_label: "Ausschließen",
      include_label: "Einschlussmuster",
      include_placeholder: "prod_*, sales_??",
      exclude_patterns_label: "Ausschlussmuster",
      exclude_placeholder: "*_tmp",
      pattern_hint: "* passt auf beliebige Zeichen, ? auf genau eines; Groß-/Kleinschreibung zählt. Muster durch Kommas trennen. admin, config und local werden nie eingeschlossen.",
      auto_label: "Neue Datenbanken automatisch hinzufügen",
      auto_hint: "Ein: später angelegte passende Datenbanken werden ab dem nächsten Lauf gesichert, und eine Benachrichtigung job.databases_added wird gesendet. Aus: der Job sichert weiter die jetzigen Datenbanken; neue werden in der Laufzusammenfassung gelistet, wo Sie sie hinzufügen können.",
      parallelism_label: "Datenbanken gleichzeitig",
      parallelism_hint: "Jede Datenbank behält ihre eigene Laufsperre. 1 sichert sie nacheinander.",
      preview_n: "Mit diesen Einstellungen werden diese {n} Datenbanken gesichert:",
      preview_one: "Mit diesen Einstellungen wird 1 Datenbank gesichert:",
      preview_none: "Mit diesen Einstellungen würde keine Datenbank gesichert.",
      preview_loading: "Die Datenbanken der Verbindung werden geprüft…",
      preview_failed: "Die Datenbanken konnten nicht geprüft werden: {error}",
      preview_new_excluded: "{n} neue Datenbank(en) nicht enthalten: {list}",
      preview_new_included: "{n} neue Datenbank(en) werden hinzugefügt: {list}",
      preview_missing: "Auf dem Server nicht gefunden: {list}",
      preview_more: "+{n} weitere",
      need_selection: "Markieren Sie mindestens eine Datenbank.",
      all_excluded: "Alle Datenbanken sind ausgeschlossen. Entfernen Sie bei mindestens einer das Häkchen, um sie zu sichern.",
      colls_toggle: "Collections von {db}",
      colls_loading: "Collections werden geladen…",
      colls_failed: "Die Collections konnten nicht aufgelistet werden; geben Sie ihre Namen durch Kommas getrennt ein.",
      colls_none: "Diese Datenbank hat keine Collections.",
      colls_manual_include: "Nur diese Collections von {db} (leer: alle)",
      colls_manual_exclude: "Zu überspringende Collections von {db}",
      colls_all_except: "alle außer {list}",
      tree_hint_list: "Klappen Sie eine Datenbank auf, um nur einige ihrer Collections zu sichern: markierte Collections werden gesichert.",
      tree_hint_all: "Klappen Sie eine Datenbank auf, um einige ihrer Collections zu überspringen: markierte Collections werden ausgeschlossen. Später gefundene Datenbanken werden vollständig gesichert.",
      summary_colls: "{db}: {colls}",
      filtered_badge: "Gefiltert",
      filtered_only: "Nur diese Collections: {list}",
      filtered_without: "Ohne diese Collections: {list}",
      need_pattern: "Geben Sie mindestens ein Einschlussmuster ein.",
      cell_n: "{n} Datenbanken",
      cell_all: "Alle",
      cell_all_n: "Alle ({n})",
      cell_pattern_n: "{pattern} ({n})",
      summary_list: "Ausgewählt: {list}",
      summary_all: "Alle Datenbanken",
      summary_all_except: "Alle Datenbanken außer {list}",
      summary_pattern: "Passend zu {list}",
      summary_pattern_except: "Passend zu {include}, außer {exclude}",
      summary_plus: "zusätzlich {list}",
      summary_auto: "neue Datenbanken werden automatisch hinzugefügt",
      summary_frozen: "neue Datenbanken werden gemeldet, nicht hinzugefügt",
      summary_parallel: "{n} gleichzeitig",
      runs_title: "Läufe",
      runs_empty: "Noch keine Läufe.",
      runs_failed: "Die Läufe konnten nicht geladen werden.",
      run_line: "Lauf von {time} — {ok}/{n} Datenbanken ok",
      run_new_n: "{n} neue Datenbanken nicht enthalten",
      run_added_n: "{n} Datenbanken hinzugefügt",
      run_status_ok: "OK",
      run_status_partial: "Teilweise",
      run_status_failed: "Fehlgeschlagen",
      run_status_cancelled: "Abgebrochen",
      run_status_running: "Läuft",
      new_title: "Neu entdeckte Datenbanken",
      new_hint: "Diese Datenbanken sind seit dem Speichern des Jobs hinzugekommen und werden noch nicht gesichert.",
      add_to_job: "Zum Job hinzufügen",
      added_toast: "{db} wird jetzt von diesem Job gesichert",
      add_failed: "Die Datenbank konnte nicht zum Job hinzugefügt werden.",
      not_found: "Datenbank nicht gefunden",
      rt_databases: "Zu testende Datenbanken",
      rt_rotate: "Eine pro Lauf, abwechselnd",
      rt_all: "Alle Datenbanken des Laufs",
      stop_confirm: "Den aktuellen Lauf dieses Jobs stoppen? Das Backup der laufenden Datenbank wird abgebrochen, wartende Datenbanken werden übersprungen. Bereits gesicherte Datenbanken behalten ihre Backups.",
      stop_requested: "Der Lauf des Jobs wird gestoppt.",
      retention_per_db: "Pro Datenbank: {list}",
      reason_system: "Systemdatenbank",
      reason_excluded: "ausgeschlossen",
      reason_not_matched: "kein Muster passt",
      reason_not_selected: "nicht ausgewählt",
      reason_new: "neu",
      reason_not_found: "nicht gefunden",
    },
    notify: { events: { job_databases_added: "Neue Datenbanken zu einem Job hinzugefügt" } },
    overview: {
      spark_run_multi: "{when} — {status}: {ok}/{n}",
      spark_failed_dbs: "fehlgeschlagen: {list}",
      spark_label_multi: "Letzte {n} Läufe: {ok} ok, {partial} teilweise, {failed} fehlgeschlagen",
      att_partial_last: "{job}: der letzte Lauf {when} hat {ok} von {n} Datenbanken gesichert",
      spark_partial: "{ok} von {n} Datenbanken",
    },
  },
  es: {
    instantdb: {
      list_hint: "Respalda cada base de datos marcada en su propia copia, todas en una sola ejecución.",
      all_hint: "Respalda todas las bases de datos que lista la conexión excepto admin, config y local y las marcadas aquí.",
      list_failed: "No se pudieron listar las bases de datos; elija Una para escribir un nombre.",
      summary_n: "{n} bases de datos seleccionadas · unos {size} en total",
      summary_n_nosize: "{n} bases de datos seleccionadas",
      too_many: "Se pueden respaldar como máximo {max} bases de datos a la vez.",
      started_n: "Copia de seguridad de {n} bases de datos iniciada.",
      busy_list: "Omitidas, ya se están respaldando: {list}.",
      view_run: "Ver ejecución",
    },
    filters: { run_filter: "Ejecución {id}" },
    jobdb: {
      n_of_m: "{ok}/{n} bases de datos",
      last_run_result: "Resultado de la última ejecución",
      mode_label: "Bases de datos",
      mode_single: "Una",
      mode_list: "Seleccionadas",
      mode_all: "Todas",
      mode_pattern: "Patrón",
      search_placeholder: "Buscar bases de datos",
      list_hint: "Respalda las bases de datos marcadas, cada una en su propia copia. Una base de datos que ya no existe se informa como fallida en la ejecución; las demás se respaldan igualmente.",
      list_none: "Esta conexión no lista bases de datos para elegir.",
      list_failed: "No se pudieron listar las bases de datos; introduzca patrones en su lugar.",
      all_hint: "Respalda todas las bases de datos de la conexión excepto admin, config, local y las marcadas aquí.",
      exclude_label: "Excluir",
      include_label: "Patrones de inclusión",
      include_placeholder: "prod_*, sales_??",
      exclude_patterns_label: "Patrones de exclusión",
      exclude_placeholder: "*_tmp",
      pattern_hint: "* coincide con cualquier carácter, ? con exactamente uno; distingue mayúsculas. Separe los patrones con comas. admin, config y local nunca se incluyen.",
      auto_label: "Añadir nuevas bases de datos automáticamente",
      auto_hint: "Activado: las bases de datos creadas después que coincidan se respaldan desde la siguiente ejecución y se envía una notificación job.databases_added. Desactivado: el trabajo sigue respaldando las bases de datos actuales; las nuevas aparecen en el resumen de la ejecución, donde puede añadirlas.",
      parallelism_label: "Bases de datos a la vez",
      parallelism_hint: "Cada base de datos conserva su propio bloqueo. 1 las respalda una tras otra.",
      preview_n: "Con esta configuración se respaldarán estas {n} bases de datos:",
      preview_one: "Con esta configuración se respaldará 1 base de datos:",
      preview_none: "Con esta configuración no se respaldaría ninguna base de datos.",
      preview_loading: "Comprobando las bases de datos de la conexión…",
      preview_failed: "No se pudieron comprobar las bases de datos: {error}",
      preview_new_excluded: "{n} base(s) de datos nueva(s) no incluida(s): {list}",
      preview_new_included: "Se añadirán {n} base(s) de datos nueva(s): {list}",
      preview_missing: "No encontradas en el servidor: {list}",
      preview_more: "+{n} más",
      need_selection: "Marque al menos una base de datos.",
      all_excluded: "Todas las bases de datos están excluidas. Desmarque al menos una para respaldarla.",
      colls_toggle: "Colecciones de {db}",
      colls_loading: "Cargando colecciones…",
      colls_failed: "No se pudieron listar las colecciones; escriba sus nombres separados por comas.",
      colls_none: "Esta base de datos no tiene colecciones.",
      colls_manual_include: "Solo estas colecciones de {db} (vacío: todas)",
      colls_manual_exclude: "Colecciones de {db} que se omiten",
      colls_all_except: "todas excepto {list}",
      tree_hint_list: "Despliegue una base de datos para respaldar solo algunas de sus colecciones: se respaldan las colecciones marcadas.",
      tree_hint_all: "Despliegue una base de datos para omitir algunas de sus colecciones: las colecciones marcadas se excluyen. Las bases de datos que aparezcan después se respaldan enteras.",
      summary_colls: "{db}: {colls}",
      filtered_badge: "Filtrada",
      filtered_only: "Solo estas colecciones: {list}",
      filtered_without: "Sin estas colecciones: {list}",
      need_pattern: "Introduzca al menos un patrón de inclusión.",
      cell_n: "{n} bases de datos",
      cell_all: "Todas",
      cell_all_n: "Todas ({n})",
      cell_pattern_n: "{pattern} ({n})",
      summary_list: "Seleccionadas: {list}",
      summary_all: "Todas las bases de datos",
      summary_all_except: "Todas las bases de datos excepto {list}",
      summary_pattern: "Coincidentes con {list}",
      summary_pattern_except: "Coincidentes con {include}, excepto {exclude}",
      summary_plus: "además {list}",
      summary_auto: "las nuevas bases de datos se añaden automáticamente",
      summary_frozen: "las nuevas bases de datos se informan, no se añaden",
      summary_parallel: "{n} a la vez",
      runs_title: "Ejecuciones",
      runs_empty: "Aún no hay ejecuciones.",
      runs_failed: "No se pudieron cargar las ejecuciones.",
      run_line: "Ejecución de {time} — {ok}/{n} bases de datos correctas",
      run_new_n: "{n} bases de datos nuevas no incluidas",
      run_added_n: "{n} bases de datos añadidas",
      run_status_ok: "Correcta",
      run_status_partial: "Parcial",
      run_status_failed: "Fallida",
      run_status_cancelled: "Cancelada",
      run_status_running: "En curso",
      new_title: "Bases de datos descubiertas",
      new_hint: "Estas bases de datos aparecieron después de guardar el trabajo y aún no se respaldan.",
      add_to_job: "Añadir al trabajo",
      added_toast: "{db} ahora se respalda con este trabajo",
      add_failed: "No se pudo añadir la base de datos al trabajo.",
      not_found: "Base de datos no encontrada",
      rt_databases: "Bases de datos a probar",
      rt_rotate: "Una por ejecución, por turnos",
      rt_all: "Todas las bases de datos de la ejecución",
      stop_confirm: "¿Detener la ejecución actual de este trabajo? Se cancela la copia de la base de datos en curso y se omiten las que esperan. Las bases de datos ya respaldadas conservan sus copias.",
      stop_requested: "Deteniendo la ejecución del trabajo.",
      retention_per_db: "Por base de datos: {list}",
      reason_system: "base de datos del sistema",
      reason_excluded: "excluida",
      reason_not_matched: "ningún patrón coincide",
      reason_not_selected: "no seleccionada",
      reason_new: "nueva",
      reason_not_found: "no encontrada",
    },
    notify: { events: { job_databases_added: "Nuevas bases de datos añadidas a un trabajo" } },
    overview: {
      spark_run_multi: "{when} — {status}: {ok}/{n}",
      spark_failed_dbs: "fallidas: {list}",
      spark_label_multi: "Últimas {n} ejecuciones: {ok} correctas, {partial} parciales, {failed} fallidas",
      att_partial_last: "{job}: la última ejecución {when} respaldó {ok} de {n} bases de datos",
      spark_partial: "{ok} de {n} bases de datos",
    },
  },
  fr: {
    instantdb: {
      list_hint: "Sauvegarde chaque base cochée dans sa propre sauvegarde, toutes en une seule exécution.",
      all_hint: "Sauvegarde toutes les bases que la connexion liste, sauf admin, config et local et celles cochées ici.",
      list_failed: "Les bases de données n'ont pas pu être listées ; choisissez « Une » pour saisir un nom.",
      summary_n: "{n} bases sélectionnées · environ {size} au total",
      summary_n_nosize: "{n} bases sélectionnées",
      too_many: "Au plus {max} bases de données peuvent être sauvegardées à la fois.",
      started_n: "Sauvegarde de {n} bases de données lancée.",
      busy_list: "Ignorées, déjà en cours de sauvegarde : {list}.",
      view_run: "Voir l'exécution",
    },
    filters: { run_filter: "Exécution {id}" },
    jobdb: {
      n_of_m: "{ok}/{n} bases",
      last_run_result: "Résultat de la dernière exécution",
      mode_label: "Bases de données",
      mode_single: "Une",
      mode_list: "Sélection",
      mode_all: "Toutes",
      mode_pattern: "Motif",
      search_placeholder: "Rechercher des bases",
      list_hint: "Sauvegarde les bases cochées, chacune dans sa propre sauvegarde. Une base qui n'existe plus est signalée en échec dans l'exécution ; les autres sont tout de même sauvegardées.",
      list_none: "Cette connexion ne liste aucune base à choisir.",
      list_failed: "Les bases n'ont pas pu être listées ; saisissez plutôt des motifs.",
      all_hint: "Sauvegarde toutes les bases de la connexion sauf admin, config, local et celles cochées ici.",
      exclude_label: "Exclure",
      include_label: "Motifs d'inclusion",
      include_placeholder: "prod_*, sales_??",
      exclude_patterns_label: "Motifs d'exclusion",
      exclude_placeholder: "*_tmp",
      pattern_hint: "* correspond à n'importe quels caractères, ? à exactement un ; sensible à la casse. Séparez les motifs par des virgules. admin, config et local ne sont jamais inclus.",
      auto_label: "Ajouter automatiquement les nouvelles bases",
      auto_hint: "Activé : les bases créées plus tard qui correspondent sont sauvegardées dès l'exécution suivante et une notification job.databases_added est envoyée. Désactivé : la tâche continue de sauvegarder les bases actuelles ; les nouvelles sont listées dans le résumé de l'exécution, où vous pouvez les ajouter.",
      parallelism_label: "Bases en même temps",
      parallelism_hint: "Chaque base garde son propre verrou. 1 les sauvegarde l'une après l'autre.",
      preview_n: "Avec ces réglages, ces {n} bases seront sauvegardées :",
      preview_one: "Avec ces réglages, 1 base sera sauvegardée :",
      preview_none: "Avec ces réglages, aucune base ne serait sauvegardée.",
      preview_loading: "Vérification des bases de la connexion…",
      preview_failed: "Les bases n'ont pas pu être vérifiées : {error}",
      preview_new_excluded: "{n} nouvelle(s) base(s) non incluse(s) : {list}",
      preview_new_included: "{n} nouvelle(s) base(s) seront ajoutée(s) : {list}",
      preview_missing: "Introuvable(s) sur le serveur : {list}",
      preview_more: "+{n} de plus",
      need_selection: "Cochez au moins une base.",
      all_excluded: "Toutes les bases sont exclues. Décochez-en au moins une pour la sauvegarder.",
      colls_toggle: "Collections de {db}",
      colls_loading: "Chargement des collections…",
      colls_failed: "Les collections n'ont pas pu être listées ; saisissez leurs noms séparés par des virgules.",
      colls_none: "Cette base n'a aucune collection.",
      colls_manual_include: "Seulement ces collections de {db} (vide : toutes)",
      colls_manual_exclude: "Collections de {db} à ignorer",
      colls_all_except: "toutes sauf {list}",
      tree_hint_list: "Dépliez une base pour ne sauvegarder que certaines de ses collections : les collections cochées sont sauvegardées.",
      tree_hint_all: "Dépliez une base pour ignorer certaines de ses collections : les collections cochées sont exclues. Les bases trouvées plus tard sont sauvegardées entières.",
      summary_colls: "{db} : {colls}",
      filtered_badge: "Filtrée",
      filtered_only: "Seulement ces collections : {list}",
      filtered_without: "Sans ces collections : {list}",
      need_pattern: "Saisissez au moins un motif d'inclusion.",
      cell_n: "{n} bases",
      cell_all: "Toutes",
      cell_all_n: "Toutes ({n})",
      cell_pattern_n: "{pattern} ({n})",
      summary_list: "Sélection : {list}",
      summary_all: "Toutes les bases",
      summary_all_except: "Toutes les bases sauf {list}",
      summary_pattern: "Correspondant à {list}",
      summary_pattern_except: "Correspondant à {include}, sauf {exclude}",
      summary_plus: "plus {list}",
      summary_auto: "les nouvelles bases sont ajoutées automatiquement",
      summary_frozen: "les nouvelles bases sont signalées, pas ajoutées",
      summary_parallel: "{n} à la fois",
      runs_title: "Exécutions",
      runs_empty: "Aucune exécution pour l'instant.",
      runs_failed: "Les exécutions n'ont pas pu être chargées.",
      run_line: "Exécution de {time} — {ok}/{n} bases OK",
      run_new_n: "{n} nouvelles bases non incluses",
      run_added_n: "{n} bases ajoutées",
      run_status_ok: "OK",
      run_status_partial: "Partielle",
      run_status_failed: "Échec",
      run_status_cancelled: "Annulée",
      run_status_running: "En cours",
      new_title: "Bases nouvellement découvertes",
      new_hint: "Ces bases sont apparues depuis l'enregistrement de la tâche et ne sont pas encore sauvegardées.",
      add_to_job: "Ajouter à la tâche",
      added_toast: "{db} est maintenant sauvegardée par cette tâche",
      add_failed: "La base n'a pas pu être ajoutée à la tâche.",
      not_found: "Base introuvable",
      rt_databases: "Bases à tester",
      rt_rotate: "Une par exécution, à tour de rôle",
      rt_all: "Toutes les bases de l'exécution",
      stop_confirm: "Arrêter l'exécution en cours de cette tâche ? La sauvegarde de la base en cours est annulée et les bases en attente sont ignorées. Les bases déjà sauvegardées gardent leurs sauvegardes.",
      stop_requested: "Arrêt de l'exécution de la tâche.",
      retention_per_db: "Par base : {list}",
      reason_system: "base système",
      reason_excluded: "exclue",
      reason_not_matched: "aucun motif ne correspond",
      reason_not_selected: "non sélectionnée",
      reason_new: "nouvelle",
      reason_not_found: "introuvable",
    },
    notify: { events: { job_databases_added: "Nouvelles bases ajoutées à une tâche" } },
    overview: {
      spark_run_multi: "{when} — {status} : {ok}/{n}",
      spark_failed_dbs: "en échec : {list}",
      spark_label_multi: "{n} dernières exécutions : {ok} OK, {partial} partielles, {failed} en échec",
      att_partial_last: "{job} : la dernière exécution {when} a sauvegardé {ok} bases sur {n}",
      spark_partial: "{ok} bases sur {n}",
    },
  },
  zh: {
    instantdb: {
      list_hint: "将每个勾选的数据库分别备份为独立的备份，全部在一次运行中完成。",
      all_hint: "备份该连接列出的所有数据库，admin、config、local 以及此处勾选的除外。",
      list_failed: "无法列出数据库；请选择“单个”以输入名称。",
      summary_n: "已选择 {n} 个数据库 · 总计约 {size}",
      summary_n_nosize: "已选择 {n} 个数据库",
      too_many: "一次最多可备份 {max} 个数据库。",
      started_n: "已开始备份 {n} 个数据库。",
      busy_list: "已跳过（正在备份中）：{list}。",
      view_run: "查看运行",
    },
    filters: { run_filter: "运行 {id}" },
    jobdb: {
      n_of_m: "{ok}/{n} 个数据库",
      last_run_result: "最近一次运行结果",
      mode_label: "数据库",
      mode_single: "单个",
      mode_list: "选定",
      mode_all: "全部",
      mode_pattern: "模式",
      search_placeholder: "搜索数据库",
      list_hint: "备份勾选的数据库，每个数据库单独生成一个备份。已不存在的数据库会在本次运行中记为失败，其余数据库照常备份。",
      list_none: "此连接没有可选择的数据库。",
      list_failed: "无法列出数据库；请改为输入模式。",
      all_hint: "备份此连接中除 admin、config、local 及此处勾选之外的所有数据库。",
      exclude_label: "排除",
      include_label: "包含模式",
      include_placeholder: "prod_*, sales_??",
      exclude_patterns_label: "排除模式",
      exclude_placeholder: "*_tmp",
      pattern_hint: "* 匹配任意字符，? 匹配恰好一个字符；区分大小写。多个模式用逗号分隔。admin、config 和 local 永远不会被包含。",
      auto_label: "自动添加新数据库",
      auto_hint: "开启：之后创建且符合选择的数据库从下一次运行起被备份，并发送 job.databases_added 通知。关闭：作业继续备份当前的数据库；新数据库会列在运行摘要中，您可以在那里添加。",
      parallelism_label: "同时备份的数据库数",
      parallelism_hint: "每个数据库保留各自的运行锁。1 表示逐个备份。",
      preview_n: "按此设置将备份以下 {n} 个数据库：",
      preview_one: "按此设置将备份 1 个数据库：",
      preview_none: "按此设置不会备份任何数据库。",
      preview_loading: "正在检查连接的数据库…",
      preview_failed: "无法检查数据库：{error}",
      preview_new_excluded: "{n} 个新数据库未包含：{list}",
      preview_new_included: "将添加 {n} 个新数据库：{list}",
      preview_missing: "服务器上未找到：{list}",
      preview_more: "另外 {n} 个",
      need_selection: "请至少勾选一个数据库。",
      all_excluded: "所有数据库均已排除。请至少取消勾选一个以进行备份。",
      colls_toggle: "{db} 的集合",
      colls_loading: "正在加载集合…",
      colls_failed: "无法列出集合；请输入集合名称，用逗号分隔。",
      colls_none: "此数据库没有集合。",
      colls_manual_include: "仅 {db} 的这些集合（留空：全部）",
      colls_manual_exclude: "{db} 中要跳过的集合",
      colls_all_except: "除 {list} 外的全部",
      tree_hint_list: "展开数据库可只备份其中部分集合：勾选的集合会被备份。",
      tree_hint_all: "展开数据库可跳过其中部分集合：勾选的集合会被排除。之后发现的数据库将整体备份。",
      summary_colls: "{db}：{colls}",
      filtered_badge: "已筛选",
      filtered_only: "仅这些集合：{list}",
      filtered_without: "不含这些集合：{list}",
      need_pattern: "请至少输入一个包含模式。",
      cell_n: "{n} 个数据库",
      cell_all: "全部",
      cell_all_n: "全部（{n}）",
      cell_pattern_n: "{pattern}（{n}）",
      summary_list: "选定：{list}",
      summary_all: "所有数据库",
      summary_all_except: "除 {list} 外的所有数据库",
      summary_pattern: "匹配 {list}",
      summary_pattern_except: "匹配 {include}，排除 {exclude}",
      summary_plus: "另加 {list}",
      summary_auto: "新数据库自动添加",
      summary_frozen: "新数据库仅报告，不添加",
      summary_parallel: "同时 {n} 个",
      runs_title: "运行",
      runs_empty: "尚无运行。",
      runs_failed: "无法加载运行记录。",
      run_line: "{time} 的运行 — {ok}/{n} 个数据库成功",
      run_new_n: "{n} 个新数据库未包含",
      run_added_n: "已添加 {n} 个数据库",
      run_status_ok: "成功",
      run_status_partial: "部分成功",
      run_status_failed: "失败",
      run_status_cancelled: "已取消",
      run_status_running: "运行中",
      new_title: "新发现的数据库",
      new_hint: "这些数据库是在作业保存后出现的，尚未被备份。",
      add_to_job: "添加到作业",
      added_toast: "{db} 现在由此作业备份",
      add_failed: "无法将数据库添加到作业。",
      not_found: "未找到数据库",
      rt_databases: "要测试的数据库",
      rt_rotate: "每次运行测试一个，轮流进行",
      rt_all: "运行中的所有数据库",
      stop_confirm: "停止此作业的当前运行？正在运行的数据库备份将被取消，等待中的数据库将被跳过。已备份的数据库保留其备份。",
      stop_requested: "正在停止作业的运行。",
      retention_per_db: "按数据库：{list}",
      reason_system: "系统数据库",
      reason_excluded: "已排除",
      reason_not_matched: "无模式匹配",
      reason_not_selected: "未选定",
      reason_new: "新",
      reason_not_found: "未找到",
    },
    notify: { events: { job_databases_added: "作业新增了数据库" } },
    overview: {
      spark_run_multi: "{when} — {status}：{ok}/{n}",
      spark_failed_dbs: "失败：{list}",
      spark_label_multi: "最近 {n} 次运行：{ok} 次成功，{partial} 次部分成功，{failed} 次失败",
      att_partial_last: "{job}：最近一次运行（{when}）备份了 {n} 个数据库中的 {ok} 个",
      spark_partial: "{n} 个数据库中的 {ok} 个",
    },
  },
  ja: {
    instantdb: {
      list_hint: "チェックした各データベースを個別のバックアップとして、1 回の実行でまとめてバックアップします。",
      all_hint: "接続が一覧表示するすべてのデータベースをバックアップします（admin、config、local とここでチェックしたものを除く）。",
      list_failed: "データベースを一覧表示できませんでした。名前を入力するには「単一」を選択してください。",
      summary_n: "{n} 個のデータベースを選択 · 合計約 {size}",
      summary_n_nosize: "{n} 個のデータベースを選択",
      too_many: "一度にバックアップできるデータベースは最大 {max} 個です。",
      started_n: "{n} 個のデータベースのバックアップを開始しました。",
      busy_list: "バックアップ中のためスキップ: {list}。",
      view_run: "実行を表示",
    },
    filters: { run_filter: "実行 {id}" },
    jobdb: {
      n_of_m: "{ok}/{n} データベース",
      last_run_result: "前回の実行結果",
      mode_label: "データベース",
      mode_single: "単一",
      mode_list: "選択",
      mode_all: "すべて",
      mode_pattern: "パターン",
      search_placeholder: "データベースを検索",
      list_hint: "チェックしたデータベースを、それぞれ個別のバックアップにバックアップします。存在しなくなったデータベースは実行で失敗として報告され、ほかのデータベースはそのままバックアップされます。",
      list_none: "この接続には選択できるデータベースがありません。",
      list_failed: "データベースを一覧できませんでした。代わりにパターンを入力してください。",
      all_hint: "admin、config、local とここでチェックしたものを除く、接続のすべてのデータベースをバックアップします。",
      exclude_label: "除外",
      include_label: "含めるパターン",
      include_placeholder: "prod_*, sales_??",
      exclude_patterns_label: "除外するパターン",
      exclude_placeholder: "*_tmp",
      pattern_hint: "* は任意の文字列、? はちょうど 1 文字に一致します。大文字と小文字は区別されます。パターンはカンマで区切ります。admin、config、local は常に含まれません。",
      auto_label: "新しいデータベースを自動で追加",
      auto_hint: "オン: 後から作成され選択に一致するデータベースは次の実行からバックアップされ、job.databases_added 通知が送られます。オフ: ジョブは現在のデータベースのバックアップを続け、新しいものは実行の概要に表示されるので、そこから追加できます。",
      parallelism_label: "同時に処理するデータベース",
      parallelism_hint: "各データベースは独自の実行ロックを保持します。1 は順番にバックアップします。",
      preview_n: "この設定では次の {n} 個のデータベースがバックアップされます:",
      preview_one: "この設定では 1 個のデータベースがバックアップされます:",
      preview_none: "この設定ではデータベースはバックアップされません。",
      preview_loading: "接続のデータベースを確認しています…",
      preview_failed: "データベースを確認できませんでした: {error}",
      preview_new_excluded: "含まれていない新しいデータベース {n} 個: {list}",
      preview_new_included: "追加される新しいデータベース {n} 個: {list}",
      preview_missing: "サーバーに見つかりません: {list}",
      preview_more: "ほか {n} 個",
      need_selection: "少なくとも 1 つのデータベースをチェックしてください。",
      all_excluded: "すべてのデータベースが除外されています。バックアップするには少なくとも 1 つのチェックを外してください。",
      colls_toggle: "{db} のコレクション",
      colls_loading: "コレクションを読み込んでいます…",
      colls_failed: "コレクションを一覧表示できませんでした。名前をカンマ区切りで入力してください。",
      colls_none: "このデータベースにはコレクションがありません。",
      colls_manual_include: "{db} のこれらのコレクションのみ (空: すべて)",
      colls_manual_exclude: "{db} でスキップするコレクション",
      colls_all_except: "{list} 以外のすべて",
      tree_hint_list: "データベースを展開すると、一部のコレクションだけをバックアップできます。チェックしたコレクションがバックアップされます。",
      tree_hint_all: "データベースを展開すると、一部のコレクションをスキップできます。チェックしたコレクションは除外されます。後から見つかったデータベースは丸ごとバックアップされます。",
      summary_colls: "{db}: {colls}",
      filtered_badge: "フィルター済み",
      filtered_only: "これらのコレクションのみ: {list}",
      filtered_without: "これらのコレクションを除く: {list}",
      need_pattern: "含めるパターンを少なくとも 1 つ入力してください。",
      cell_n: "{n} 個のデータベース",
      cell_all: "すべて",
      cell_all_n: "すべて ({n})",
      cell_pattern_n: "{pattern} ({n})",
      summary_list: "選択: {list}",
      summary_all: "すべてのデータベース",
      summary_all_except: "{list} を除くすべてのデータベース",
      summary_pattern: "{list} に一致",
      summary_pattern_except: "{include} に一致、{exclude} を除く",
      summary_plus: "さらに {list}",
      summary_auto: "新しいデータベースは自動で追加",
      summary_frozen: "新しいデータベースは報告のみ、追加しない",
      summary_parallel: "同時に {n} 個",
      runs_title: "実行",
      runs_empty: "まだ実行はありません。",
      runs_failed: "実行を読み込めませんでした。",
      run_line: "{time} の実行 — {ok}/{n} データベース成功",
      run_new_n: "含まれていない新しいデータベース {n} 個",
      run_added_n: "{n} 個のデータベースを追加",
      run_status_ok: "成功",
      run_status_partial: "一部失敗",
      run_status_failed: "失敗",
      run_status_cancelled: "キャンセル",
      run_status_running: "実行中",
      new_title: "新しく見つかったデータベース",
      new_hint: "これらのデータベースはジョブの保存後に現れたもので、まだバックアップされていません。",
      add_to_job: "ジョブに追加",
      added_toast: "{db} はこのジョブでバックアップされるようになりました",
      add_failed: "データベースをジョブに追加できませんでした。",
      not_found: "データベースが見つかりません",
      rt_databases: "テストするデータベース",
      rt_rotate: "実行ごとに 1 つ、順番に",
      rt_all: "実行のすべてのデータベース",
      stop_confirm: "このジョブの現在の実行を停止しますか？実行中のデータベースのバックアップはキャンセルされ、待機中のデータベースはスキップされます。すでにバックアップ済みのデータベースはバックアップを保持します。",
      stop_requested: "ジョブの実行を停止しています。",
      retention_per_db: "データベース別: {list}",
      reason_system: "システムデータベース",
      reason_excluded: "除外",
      reason_not_matched: "一致するパターンなし",
      reason_not_selected: "未選択",
      reason_new: "新規",
      reason_not_found: "見つかりません",
    },
    notify: { events: { job_databases_added: "ジョブに新しいデータベースを追加" } },
    overview: {
      spark_run_multi: "{when} — {status}: {ok}/{n}",
      spark_failed_dbs: "失敗: {list}",
      spark_label_multi: "直近 {n} 回の実行: 成功 {ok}、一部失敗 {partial}、失敗 {failed}",
      att_partial_last: "{job}: 前回の実行（{when}）は {n} 個中 {ok} 個のデータベースをバックアップしました",
      spark_partial: "{n} 個中 {ok} 個のデータベース",
    },
  },
  ru: {
    instantdb: {
      list_hint: "Копирует каждую отмеченную базу данных в отдельную резервную копию, все за один запуск.",
      all_hint: "Копирует все базы данных, которые перечисляет подключение, кроме admin, config, local и отмеченных здесь.",
      list_failed: "Не удалось получить список баз данных; выберите «Одна», чтобы ввести имя.",
      summary_n: "Выбрано баз данных: {n} · всего около {size}",
      summary_n_nosize: "Выбрано баз данных: {n}",
      too_many: "За один раз можно скопировать не более {max} баз данных.",
      started_n: "Запущено резервное копирование баз данных: {n}.",
      busy_list: "Пропущены, уже копируются: {list}.",
      view_run: "Показать запуск",
    },
    filters: { run_filter: "Запуск {id}" },
    jobdb: {
      n_of_m: "{ok}/{n} баз данных",
      last_run_result: "Результат последнего запуска",
      mode_label: "Базы данных",
      mode_single: "Одна",
      mode_list: "Выбранные",
      mode_all: "Все",
      mode_pattern: "Шаблон",
      search_placeholder: "Поиск баз данных",
      list_hint: "Создаёт резервные копии отмеченных баз данных, каждой в отдельную копию. База данных, которой больше нет, отмечается в запуске как неудачная; остальные копируются как обычно.",
      list_none: "У этого подключения нет баз данных для выбора.",
      list_failed: "Не удалось получить список баз данных; введите вместо этого шаблоны.",
      all_hint: "Копирует все базы данных подключения, кроме admin, config, local и отмеченных здесь.",
      exclude_label: "Исключить",
      include_label: "Шаблоны включения",
      include_placeholder: "prod_*, sales_??",
      exclude_patterns_label: "Шаблоны исключения",
      exclude_placeholder: "*_tmp",
      pattern_hint: "* соответствует любым символам, ? ровно одному; регистр учитывается. Разделяйте шаблоны запятыми. admin, config и local никогда не включаются.",
      auto_label: "Автоматически добавлять новые базы данных",
      auto_hint: "Вкл.: созданные позже подходящие базы данных копируются со следующего запуска, и отправляется уведомление job.databases_added. Выкл.: задание продолжает копировать текущие базы данных; новые показываются в сводке запуска, где их можно добавить.",
      parallelism_label: "Баз данных одновременно",
      parallelism_hint: "Каждая база данных сохраняет собственную блокировку. 1 — копировать по очереди.",
      preview_n: "С этими настройками будут скопированы эти {n} баз(ы) данных:",
      preview_one: "С этими настройками будет скопирована 1 база данных:",
      preview_none: "С этими настройками ни одна база данных не будет скопирована.",
      preview_loading: "Проверка баз данных подключения…",
      preview_failed: "Не удалось проверить базы данных: {error}",
      preview_new_excluded: "Новые базы данных не включены ({n}): {list}",
      preview_new_included: "Будут добавлены новые базы данных ({n}): {list}",
      preview_missing: "Не найдены на сервере: {list}",
      preview_more: "ещё {n}",
      need_selection: "Отметьте хотя бы одну базу данных.",
      all_excluded: "Все базы данных исключены. Снимите отметку хотя бы с одной, чтобы создать её резервную копию.",
      colls_toggle: "Коллекции {db}",
      colls_loading: "Загрузка коллекций…",
      colls_failed: "Не удалось получить список коллекций; введите их имена через запятую.",
      colls_none: "В этой базе данных нет коллекций.",
      colls_manual_include: "Только эти коллекции {db} (пусто: все)",
      colls_manual_exclude: "Пропускаемые коллекции {db}",
      colls_all_except: "все, кроме {list}",
      tree_hint_list: "Раскройте базу данных, чтобы копировать лишь некоторые её коллекции: отмеченные коллекции копируются.",
      tree_hint_all: "Раскройте базу данных, чтобы пропустить некоторые её коллекции: отмеченные коллекции исключаются. Базы данных, найденные позже, копируются целиком.",
      summary_colls: "{db}: {colls}",
      filtered_badge: "С фильтром",
      filtered_only: "Только эти коллекции: {list}",
      filtered_without: "Без этих коллекций: {list}",
      need_pattern: "Введите хотя бы один шаблон включения.",
      cell_n: "Баз данных: {n}",
      cell_all: "Все",
      cell_all_n: "Все ({n})",
      cell_pattern_n: "{pattern} ({n})",
      summary_list: "Выбранные: {list}",
      summary_all: "Все базы данных",
      summary_all_except: "Все базы данных, кроме {list}",
      summary_pattern: "Соответствующие {list}",
      summary_pattern_except: "Соответствующие {include}, кроме {exclude}",
      summary_plus: "а также {list}",
      summary_auto: "новые базы данных добавляются автоматически",
      summary_frozen: "о новых базах данных сообщается, они не добавляются",
      summary_parallel: "по {n} одновременно",
      runs_title: "Запуски",
      runs_empty: "Запусков пока нет.",
      runs_failed: "Не удалось загрузить запуски.",
      run_line: "Запуск {time} — {ok}/{n} баз данных успешно",
      run_new_n: "Новые базы данных не включены: {n}",
      run_added_n: "Добавлено баз данных: {n}",
      run_status_ok: "Успешно",
      run_status_partial: "Частично",
      run_status_failed: "Ошибка",
      run_status_cancelled: "Отменён",
      run_status_running: "Выполняется",
      new_title: "Новые обнаруженные базы данных",
      new_hint: "Эти базы данных появились после сохранения задания и пока не копируются.",
      add_to_job: "Добавить в задание",
      added_toast: "{db} теперь копируется этим заданием",
      add_failed: "Не удалось добавить базу данных в задание.",
      not_found: "База данных не найдена",
      rt_databases: "Проверяемые базы данных",
      rt_rotate: "По одной за запуск, по очереди",
      rt_all: "Все базы данных запуска",
      stop_confirm: "Остановить текущий запуск этого задания? Резервное копирование выполняемой базы данных будет отменено, ожидающие базы данных будут пропущены. Уже скопированные базы данных сохраняют свои копии.",
      stop_requested: "Запуск задания останавливается.",
      retention_per_db: "По базам данных: {list}",
      reason_system: "системная база данных",
      reason_excluded: "исключена",
      reason_not_matched: "ни один шаблон не подходит",
      reason_not_selected: "не выбрана",
      reason_new: "новая",
      reason_not_found: "не найдена",
    },
    notify: { events: { job_databases_added: "В задание добавлены новые базы данных" } },
    overview: {
      spark_run_multi: "{when} — {status}: {ok}/{n}",
      spark_failed_dbs: "с ошибкой: {list}",
      spark_label_multi: "Последние запуски ({n}): успешно {ok}, частично {partial}, с ошибкой {failed}",
      att_partial_last: "{job}: последний запуск {when} скопировал {ok} из {n} баз данных",
      spark_partial: "{ok} из {n} баз данных",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(JOBDB_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], JOBDB_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const JOBDB_MODES = ["single", "list", "all", "pattern"];
// Names shown in a preview or tooltip before "+N more".
const JOBDB_PREVIEW_NAMES = 12;
// Runs shown in the job details.
const JOBDB_RUNS_LIMIT = 10;

const jobDbs = {
  mode: "single",
  // Databases of the connection (names, system databases hidden), or null.
  names: null,
  listFailed: false,
  selected: new Set(),
  excluded: new Set(),
  // Exclude patterns of an edited "all" job that are not plain database names.
  extraExcludes: [],
  // Databases an edited all or pattern job names explicitly ("Add to job").
  extraDatabases: [],
  search: "",
  // Collection filters per database (see dbSelFilterState).
  ...dbSelFilterState(),
  preview: { timer: null, seq: 0 },
  runs: { jobId: "", list: null, failed: false, seq: 0 },
  adding: new Set(),
};

// ---------------------------------------------------------------------------
// Job form: the database selector
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Shared selector parts (the job form and the Backup now dialog)
// ---------------------------------------------------------------------------

// A selector keeps one set of checked names per mode, and a check means the
// opposite in each: in Selected (mode "list") state.selected holds the databases
// to back up, in All state.excluded holds the ones to skip. The sets never feed
// each other, so switching modes never turns an included database into an
// excluded one; All starts with nothing excluded.

// The databases a multi-database selection backs up out of listed (the names the
// connection lists): the checked ones in Selected, the unchecked ones in All;
// sorted, empty for any other mode.
function dbSelIncluded(mode, listed, selected, excluded) {
  if (mode === "list") return Array.from(selected).sort();
  if (mode === "all") return (listed || []).filter(n => !excluded.has(n)).sort();
  return [];
}

// The translation key of what keeps a multi-database selection from being used, or
// "": Selected needs a checked database; All only fails when its exclusions leave
// none of the listed databases (never because nothing is checked).
function dbSelProblem(mode, listed, selected, excluded) {
  if (mode === "list") return selected.size === 0 ? "jobdb.need_selection" : "";
  if (mode === "all") {
    const names = listed || [];
    return names.length > 0 && names.every(n => excluded.has(n)) ? "jobdb.all_excluded" : "";
  }
  return "";
}

// Binds the segmented mode control, the search field and the checkbox lists of
// the selector whose elements start with prefix ("job", "instant"). state holds
// mode, search, selected (Selected's list) and excluded (All's list); setMode(mode)
// switches modes, render() redraws the lists and changed() runs after a box is
// (un)checked.
function dbSelBind(prefix, modes, state, hooks) {
  const group = document.getElementById(`${prefix}-db-mode`);
  if (!group) return false;
  group.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-db-mode]");
    if (btn) hooks.setMode(btn.dataset.dbMode);
  });
  group.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
    e.preventDefault();
    const idx = modes.indexOf(state.mode);
    const next = modes[(idx + (e.key === "ArrowRight" ? 1 : modes.length - 1)) % modes.length];
    hooks.setMode(next);
    const btn = group.querySelector(`[data-db-mode="${next}"]`);
    if (btn) btn.focus();
  });
  const search = document.getElementById(`${prefix}-dbs-search`);
  if (search) {
    search.addEventListener("input", () => {
      state.search = search.value.trim().toLowerCase();
      hooks.render();
    });
  }
  [`${prefix}-dbs-list`, `${prefix}-dbs-exclude-list`].forEach(id => {
    const list = document.getElementById(id);
    if (!list) return;
    // Each list writes only its own mode's sets (see dbSelIncluded).
    const mode = id === `${prefix}-dbs-list` ? "list" : "all";
    list.addEventListener("change", (e) => {
      const box = e.target;
      if (!(box instanceof HTMLInputElement) || box.type !== "checkbox") return;
      if (box.dataset.collDb !== undefined) {
        dbSelToggleColl(state, mode, box.dataset.collDb, box.value, box.checked);
      } else {
        const set = dbSelDbSet(state, mode);
        if (box.checked) set.add(box.value); else set.delete(box.value);
        // Checking or unchecking a database covers all of its collections.
        dbSelFilters(state, mode).delete(box.value);
      }
      hooks.render();
      hooks.changed();
    });
    list.addEventListener("input", (e) => {
      const input = e.target;
      if (!(input instanceof HTMLInputElement) || input.dataset.manualDb === undefined) return;
      const db = input.dataset.manualDb;
      dbSelSetManual(state, mode, db, parseList(input.value));
      dbSelRefreshItem(list, state, mode, db);
      hooks.changed();
    });
    list.addEventListener("click", (e) => {
      const btn = e.target instanceof Element ? e.target.closest("[data-db-toggle]") : null;
      if (btn) dbSelToggleExpanded(prefix, state, btn.dataset.dbToggle, hooks.render);
    });
    list.addEventListener("keydown", (e) => dbSelTreeKey(e, list, prefix, state, hooks.render));
  });
  return true;
}

// ---------------------------------------------------------------------------
// Shared selector parts: databases as a tree with their collections
// ---------------------------------------------------------------------------

// Like the database checks, the collection filters are kept per mode: listColls
// (Selected) and allColls (All) map a database to its filter {kind, names}, kind
// "include" (names are the collections backed up) or "exclude" (the ones skipped).
// A database without an entry is backed up whole. A check means the mode's meaning:
// in Selected a checked collection is backed up, in All a checked one is excluded.
// The editor writes entries of its mode's kind ("include" in Selected, "exclude" in
// All); an entry of the other kind (a job saved through the API) is shown as it is
// until a collection of it is (un)checked. colls caches the collections of each
// database of connection collsConn ({loading, items, failed}); expanded holds the
// databases shown open.
function dbSelFilterState() {
  return { listColls: new Map(), allColls: new Map(), colls: new Map(), expanded: new Set(), collsConn: "" };
}

// The filter kind the editor writes in mode.
function dbSelModeKind(mode) {
  return mode === "all" ? "exclude" : "include";
}

// The database set of mode: the selected databases (Selected) or the excluded ones (All).
function dbSelDbSet(state, mode) {
  return mode === "all" ? state.excluded : state.selected;
}

// The collection filters of mode.
function dbSelFilters(state, mode) {
  return mode === "all" ? state.allColls : state.listColls;
}

// The filter of db that applies in mode, or null: Selected only filters selected
// databases, All only those it does not exclude.
function dbSelActiveFilter(state, mode, db) {
  const e = dbSelFilters(state, mode).get(db);
  if (!e || e.names.size === 0) return null;
  const counts = mode === "all" ? !state.excluded.has(db) : state.selected.has(db);
  return counts ? e : null;
}

// Whether the box of collection coll of db is checked in mode.
function dbSelCollChecked(state, mode, db, coll) {
  const whole = dbSelDbSet(state, mode).has(db);
  if (mode === "all" && whole) return true;
  if (mode === "list" && !whole) return false;
  const e = dbSelActiveFilter(state, mode, db);
  if (!e) return mode === "list";
  return e.kind === dbSelModeKind(mode) ? e.names.has(coll) : !e.names.has(coll);
}

// The state of the box of db in mode: "on", "partial" (a collection filter) or "off".
function dbSelDbCheck(state, mode, db) {
  if (dbSelActiveFilter(state, mode, db)) return "partial";
  return dbSelDbSet(state, mode).has(db) ? "on" : "off";
}

// Makes marked the checked collections of db in mode (all: every collection of db,
// when known): none clears the database, all of them checks it whole, and anything
// in between becomes a filter of the mode's kind.
function dbSelSetMarked(state, mode, db, marked, all) {
  const set = dbSelDbSet(state, mode);
  const filters = dbSelFilters(state, mode);
  filters.delete(db);
  if (marked.size === 0) {
    set.delete(db);
    return;
  }
  if (all && all.length > 0 && all.every(c => marked.has(c))) {
    set.add(db);
    return;
  }
  if (mode === "list") set.add(db); else set.delete(db);
  filters.set(db, { kind: dbSelModeKind(mode), names: marked });
}

// (Un)checks collection coll of db in mode; needs the database's collection list.
function dbSelToggleColl(state, mode, db, coll, checked) {
  const info = state.colls.get(db);
  const all = info && Array.isArray(info.items) ? info.items.map(c => c.name) : null;
  if (!all) return;
  const marked = new Set(all.filter(c => dbSelCollChecked(state, mode, db, c)));
  if (checked) marked.add(coll); else marked.delete(coll);
  dbSelSetMarked(state, mode, db, marked, all);
}

// Sets the collections typed for db in mode when its collections could not be
// listed: the ones backed up (Selected; none means all) or skipped (All).
function dbSelSetManual(state, mode, db, names) {
  const filters = dbSelFilters(state, mode);
  filters.delete(db);
  if (names.length === 0) return;
  if (mode === "list") state.selected.add(db); else state.excluded.delete(db);
  filters.set(db, { kind: dbSelModeKind(mode), names: new Set(names) });
}

// The collections of db that mode backs up, in words ("orders, customers" or "all
// except logs"), or "" for the whole database.
function dbSelFilterSummary(state, mode, db) {
  const e = dbSelActiveFilter(state, mode, db);
  if (!e) return "";
  const names = jobDbsNameList(Array.from(e.names).sort(), 4);
  return e.kind === "include" ? names : tf("jobdb.colls_all_except", { list: names });
}

// The filter of db in mode as the API takes it ({collections} or
// {exclude_collections}), or null.
function dbSelFilterOf(state, mode, db) {
  const e = dbSelActiveFilter(state, mode, db);
  if (!e) return null;
  const names = Array.from(e.names).sort();
  return e.kind === "include" ? { collections: names } : { exclude_collections: names };
}

// The databases entry of db for POST /api/v1/backups: its name, or an object with
// its collection filter.
function dbSelEntry(state, mode, db) {
  const f = dbSelFilterOf(state, mode, db);
  return f ? { name: db, ...f } : db;
}

// The collection filters of the databases of a job selection in mode, sorted
// (database_selection.collection_filters).
function dbSelJobFilters(state, mode) {
  return Array.from(dbSelFilters(state, mode).keys()).sort()
    .map(db => { const f = dbSelFilterOf(state, mode, db); return f ? { name: db, ...f } : null; })
    .filter(Boolean);
}

// Loads filters (database_selection.collection_filters of a saved job) into mode.
function dbSelLoadFilters(state, mode, filters) {
  const map = dbSelFilters(state, mode);
  map.clear();
  (filters || []).forEach(f => {
    if (!f || !f.name) return;
    const include = f.collections || [];
    const exclude = f.exclude_collections || [];
    // The server refuses a filter with both lists, and the editor only ever writes
    // one of them (dbSelFilterOf), so an entry is either kind.
    const entry = include.length > 0
      ? { kind: "include", names: new Set(include) }
      : { kind: "exclude", names: new Set(exclude) };
    if (entry.names.size > 0) map.set(f.name, entry);
  });
}

// Forgets the collections, open rows and filters of databases that are not in
// names, and the collection cache when the connection changed.
function dbSelKeepFilters(state, names, connId) {
  if (state.collsConn !== connId) {
    state.colls = new Map();
    state.expanded = new Set();
    state.collsConn = connId;
  }
  [state.listColls, state.allColls].forEach(map => {
    Array.from(map.keys()).forEach(db => { if (!names.includes(db)) map.delete(db); });
  });
}

// Opens or closes the collections of db, loading them the first time.
function dbSelToggleExpanded(prefix, state, db, render, open) {
  const want = open === undefined ? !state.expanded.has(db) : open;
  if (want) state.expanded.add(db); else state.expanded.delete(db);
  render();
  if (want) dbSelLoadColls(prefix, state, db, render);
}

// Loads the collections of db from the selector's connection
// (GET /api/v1/connections/{id}/databases/{db}/collections) unless they are known.
async function dbSelLoadColls(prefix, state, db, render) {
  const conn = document.getElementById(`${prefix}-connection`);
  const connId = conn ? conn.value : "";
  if (state.collsConn !== connId) {
    state.colls = new Map();
    state.collsConn = connId;
  }
  const known = state.colls.get(db);
  if (known && (known.loading || known.items)) return;
  const info = { loading: true, items: null, failed: false };
  state.colls.set(db, info);
  render();
  try {
    if (!connId) throw new Error("no connection");
    const json = await apiJSON(`/api/v1/connections/${encodeURIComponent(connId)}/databases/${encodeURIComponent(db)}/collections`);
    if (state.colls.get(db) !== info) return;
    if (!json.success) throw new Error(json.error || "");
    info.items = (json.data || []).map(c => ({ name: c.name, type: c.type }));
  } catch (err) {
    if (state.colls.get(db) !== info) return;
    info.failed = true;
  }
  info.loading = false;
  render();
}

// Updates the box and the summary of db's row in list after its typed collections
// changed (the text field keeps its focus and caret).
function dbSelRefreshItem(list, state, mode, db) {
  const item = Array.from(list.querySelectorAll(".db-tree-item")).find(el => el.dataset.db === db);
  if (!item) return;
  const box = item.querySelector("input[data-db-box]");
  if (box) dbSelApplyCheck(box, dbSelDbCheck(state, mode, db));
  const sum = item.querySelector(".db-tree-summary");
  if (sum) sum.textContent = dbSelFilterSummary(state, mode, db);
}

// Shows check ("on", "partial", "off") in box.
function dbSelApplyCheck(box, check) {
  box.checked = check === "on";
  // A native checkbox reports an indeterminate box as "mixed" by itself.
  box.indeterminate = check === "partial";
}

// Keyboard of a database tree: Up and Down (Home, End) move between the visible
// boxes, Right opens a database or moves to its first collection, Left closes it or
// moves from a collection to its database. Space (un)checks, as for any checkbox.
function dbSelTreeKey(e, list, prefix, state, render) {
  if (!["ArrowDown", "ArrowUp", "ArrowRight", "ArrowLeft", "Home", "End"].includes(e.key)) return;
  const box = e.target;
  if (!(box instanceof HTMLInputElement) || box.type !== "checkbox") return;
  const boxes = Array.from(list.querySelectorAll('input[type="checkbox"]'));
  const i = boxes.indexOf(box);
  const item = box.closest(".db-tree-item");
  const db = item ? item.dataset.db : "";
  const isColl = box.dataset.collDb !== undefined;
  const dbBox = item ? item.querySelector("input[data-db-box]") : null;
  let next = null;
  switch (e.key) {
    case "ArrowDown": next = boxes[i + 1]; break;
    case "ArrowUp": next = boxes[i - 1]; break;
    case "Home": next = boxes[0]; break;
    case "End": next = boxes[boxes.length - 1]; break;
    case "ArrowRight":
      if (isColl || !db) break;
      e.preventDefault();
      if (!state.expanded.has(db)) {
        dbSelToggleExpanded(prefix, state, db, render, true);
        dbSelFocus(list, db, "");
        return;
      }
      next = item.querySelector("input[data-coll-db]");
      break;
    case "ArrowLeft":
      if (isColl) { next = dbBox; break; }
      if (db && state.expanded.has(db)) {
        e.preventDefault();
        dbSelToggleExpanded(prefix, state, db, render, false);
        dbSelFocus(list, db, "");
        return;
      }
      break;
  }
  if (next) {
    e.preventDefault();
    next.focus();
  }
}

// Focuses the box of db (coll "") or of its collection coll in list.
function dbSelFocus(list, db, coll) {
  const boxes = Array.from(list.querySelectorAll('input[type="checkbox"]'));
  const box = boxes.find(b => coll ? b.dataset.collDb === db && b.value === coll : b.dataset.dbBox !== undefined && b.value === db);
  if (box) box.focus();
}

// Shows mode in the segmented control and the panels of the selector with prefix,
// and moves the search field above the list of the visible panel (Selected or All).
function dbSelShowMode(prefix, modes, mode) {
  document.querySelectorAll(`#${prefix}-db-mode [data-db-mode]`).forEach(btn => {
    btn.setAttribute("aria-pressed", String(btn.dataset.dbMode === mode));
    btn.tabIndex = btn.dataset.dbMode === mode ? 0 : -1;
  });
  modes.forEach(m => {
    const panel = document.getElementById(`${prefix}-db-panel-${m}`);
    if (panel) panel.hidden = m !== mode;
  });
  const search = document.getElementById(`${prefix}-dbs-search`);
  const host = mode === "all" ? document.getElementById(`${prefix}-dbs-exclude-list`) : document.getElementById(`${prefix}-dbs-list`);
  if (search && host && search.nextElementSibling !== host) host.parentNode.insertBefore(search, host);
}

// Renders the database tree listId of mode ("list" for Selected, "all" for All)
// from names (plus checked names the server does not list, so nothing is dropped
// silently), filtered by state.search: a row per database with its box (tri-state:
// a collection filter shows as mixed), a caret that opens its collections and the
// summary of its filter. detail(name) returns an optional note shown after a listed
// name (such as its size). The focused box keeps the focus.
function dbSelRenderTree(listId, emptyId, mode, state, names, listFailed, detail) {
  const list = document.getElementById(listId);
  if (!list) return;
  const active = document.activeElement;
  const focus = active instanceof HTMLInputElement && active.type === "checkbox" && list.contains(active)
    ? { db: active.dataset.collDb !== undefined ? active.dataset.collDb : active.value, coll: active.dataset.collDb !== undefined ? active.value : "" }
    : null;
  list.textContent = "";
  const set = dbSelDbSet(state, mode);
  const all = names.slice();
  set.forEach(n => { if (!all.includes(n)) all.push(n); });
  dbSelFilters(state, mode).forEach((_, n) => { if (!all.includes(n) && set.has(n)) all.push(n); });
  all.sort();
  const search = state.search;
  const shown = all.filter(n => !search || n.toLowerCase().includes(search));
  shown.forEach((name, i) => {
    const open = state.expanded.has(name);
    const item = document.createElement("div");
    item.className = "db-tree-item";
    item.dataset.db = name;
    item.setAttribute("role", "treeitem");
    item.setAttribute("aria-level", "1");
    item.setAttribute("aria-expanded", String(open));
    const row = document.createElement("div");
    row.className = "db-tree-row";
    const groupId = `${listId}-colls-${i}`;
    const caret = document.createElement("button");
    caret.type = "button";
    caret.className = "db-tree-caret";
    caret.tabIndex = -1;
    caret.dataset.dbToggle = name;
    caret.setAttribute("aria-expanded", String(open));
    caret.setAttribute("aria-controls", groupId);
    caret.setAttribute("aria-label", tf("jobdb.colls_toggle", { db: name }));
    caret.title = tf("jobdb.colls_toggle", { db: name });
    const label = document.createElement("label");
    label.className = "check-inline db-check";
    label.title = name;
    const box = document.createElement("input");
    box.type = "checkbox";
    box.value = name;
    box.dataset.dbBox = "";
    dbSelApplyCheck(box, dbSelDbCheck(state, mode, name));
    const text = document.createElement("span");
    text.className = "mono";
    text.textContent = name;
    label.append(box, text);
    const note = names.includes(name) ? (detail ? detail(name) : "") : t("jobdb.reason_not_found");
    if (note) {
      const sub = document.createElement("span");
      sub.className = "cell-sub";
      sub.textContent = ` (${note})`;
      label.appendChild(sub);
    }
    const summary = document.createElement("span");
    summary.className = "db-tree-summary cell-sub mono";
    summary.textContent = dbSelFilterSummary(state, mode, name);
    row.append(caret, label, summary);
    item.appendChild(row);
    if (open) item.appendChild(dbSelRenderColls(groupId, mode, state, name));
    list.appendChild(item);
  });
  const empty = document.getElementById(emptyId);
  if (empty) {
    empty.hidden = all.length > 0;
    empty.textContent = listFailed ? t(listFailed) : t("jobdb.list_none");
  }
  if (focus) dbSelFocus(list, focus.db, focus.coll);
}

// The open part of db's row: its collections as boxes, or a note while they load,
// or a text field for their names when they could not be listed.
function dbSelRenderColls(id, mode, state, db) {
  const group = document.createElement("div");
  group.className = "db-tree-colls";
  group.id = id;
  group.setAttribute("role", "group");
  const info = state.colls.get(db);
  const hint = key => {
    const p = document.createElement("p");
    p.className = "form-hint";
    p.textContent = t(key);
    group.appendChild(p);
  };
  if (!info || info.loading) {
    hint("jobdb.colls_loading");
    return group;
  }
  if (info.failed) {
    hint("jobdb.colls_failed");
    const input = document.createElement("input");
    input.type = "text";
    input.className = "form-input mono";
    input.dataset.manualDb = db;
    input.autocomplete = "off";
    input.spellcheck = false;
    input.placeholder = "orders, customers";
    input.setAttribute("aria-label", tf(mode === "list" ? "jobdb.colls_manual_include" : "jobdb.colls_manual_exclude", { db }));
    const e = dbSelActiveFilter(state, mode, db);
    input.value = e && e.kind === dbSelModeKind(mode) ? Array.from(e.names).join(", ") : "";
    group.appendChild(input);
    return group;
  }
  if (info.items.length === 0) {
    hint("jobdb.colls_none");
    return group;
  }
  info.items.forEach(c => {
    const item = document.createElement("div");
    item.className = "db-tree-coll";
    item.setAttribute("role", "treeitem");
    item.setAttribute("aria-level", "2");
    const label = document.createElement("label");
    label.className = "check-inline db-check";
    label.title = c.name;
    const box = document.createElement("input");
    box.type = "checkbox";
    box.value = c.name;
    box.dataset.collDb = db;
    box.checked = dbSelCollChecked(state, mode, db, c.name);
    const text = document.createElement("span");
    text.className = "mono";
    text.textContent = c.name;
    label.append(box, text);
    if (c.type === "view") {
      const sub = document.createElement("span");
      sub.className = "cell-sub";
      sub.textContent = ` (${t("picker.view")})`;
      label.appendChild(sub);
    }
    item.appendChild(label);
    group.appendChild(item);
  });
  return group;
}

function setupJobDatabases() {
  if (!dbSelBind("job", JOBDB_MODES, jobDbs, {
    setMode: mode => jobDbsSetMode(mode, true),
    render: () => jobDbsRenderLists(),
    changed: () => jobDbsSchedulePreview()
  })) return;
  ["job-db-include", "job-db-exclude", "job-db-auto", "job-connection", "job-database-select", "job-database"].forEach(id => {
    const el = document.getElementById(id);
    if (!el) return;
    el.addEventListener(el.type === "checkbox" || el.tagName === "SELECT" ? "change" : "input", () => jobDbsSchedulePreview());
  });
}

// Switches the selector to mode: the panel of the mode, the collection filters
// (single only), the automatic inclusion (all, pattern) and parallelism (multi).
function jobDbsSetMode(mode, user) {
  if (!JOBDB_MODES.includes(mode)) mode = "single";
  jobDbs.mode = mode;
  dbSelShowMode("job", JOBDB_MODES, mode);
  const multi = mode !== "single";
  const discovers = mode === "all" || mode === "pattern";
  const auto = document.getElementById("job-db-auto-group");
  if (auto) auto.hidden = !discovers;
  const par = document.getElementById("job-db-parallel-group");
  if (par) par.hidden = !multi;
  jobDbsApplyRequired();
  // Several databases choose their collections in the tree (Selected, All).
  const colls = document.getElementById("job-collections-fieldset");
  if (colls) {
    colls.disabled = multi;
    colls.hidden = multi;
  }
  const rtScope = document.getElementById("job-rt-databases-group");
  if (rtScope) rtScope.hidden = !multi;
  // The boxes always show the set of the mode now shown.
  if (multi) jobDbsRenderLists();
  if (multi || user) jobDbsSchedulePreview();
}

// Only a single database needs the shared picker's database field; the browser
// would refuse to submit a form whose hidden field is required.
function jobDbsApplyRequired() {
  const single = jobDbs.mode === "single";
  const select = document.getElementById("job-database-select");
  const manual = document.getElementById("job-database");
  if (select) select.required = single && !select.hidden;
  if (manual) manual.required = single && !manual.hidden;
}

// Called by app.js when the job picker's database list loaded (or failed to).
function jobDbsOnDatabases(p) {
  if (p && p.prefix === "instant") {
    instantDbsOnDatabases(p);
    return;
  }
  if (!p || p.prefix !== "job") return;
  jobDbsApplyRequired();
  jobDbs.names = Array.isArray(p.dbs) ? p.dbs.map(db => db.name) : null;
  jobDbs.listFailed = !Array.isArray(p.dbs) && !!p.dbError;
  // The collections listed for another connection are not reused; the filters of
  // an edited job stay (its databases may not be listed).
  const conn = document.getElementById("job-connection");
  const connId = conn ? conn.value : "";
  if (jobDbs.collsConn !== connId) {
    jobDbs.colls = new Map();
    jobDbs.expanded = new Set();
    jobDbs.collsConn = connId;
  }
  jobDbsRenderLists();
  jobDbsSchedulePreview();
}

// Splits a pattern field into its patterns.
function jobDbsPatterns(id) {
  return parseList(getValue(id));
}

// Renders the checkbox lists of the Selected and All panels from the connection's
// databases (plus checked names the server does not list, so nothing is dropped
// silently while editing).
function jobDbsRenderLists() {
  const names = jobDbs.names || [];
  const failed = jobDbs.listFailed ? "jobdb.list_failed" : "";
  dbSelRenderTree("job-dbs-list", "job-dbs-list-empty", "list", jobDbs, names, failed);
  dbSelRenderTree("job-dbs-exclude-list", "job-dbs-exclude-empty", "all", jobDbs, names, failed);
}

// Prefills the selector from job (null for a new job).
function jobDbsFill(job) {
  const sel = (job && job.database_selection) || {};
  const mode = job && sel.mode ? sel.mode : "single";
  jobDbs.selected = new Set(mode === "list" ? sel.databases || [] : []);
  jobDbs.excluded = new Set();
  jobDbs.extraExcludes = [];
  if (mode === "all") {
    (sel.exclude || []).forEach(p => {
      if (/[*?]/.test(p)) jobDbs.extraExcludes.push(p); else jobDbs.excluded.add(p);
    });
  }
  jobDbs.search = "";
  jobDbs.expanded = new Set();
  dbSelLoadFilters(jobDbs, "list", mode === "list" ? sel.collection_filters : []);
  dbSelLoadFilters(jobDbs, "all", mode === "all" ? sel.collection_filters : []);
  setValue("job-dbs-search", "");
  setValue("job-db-include", mode === "pattern" ? (sel.include || []).join(", ") : "");
  setValue("job-db-exclude", mode === "pattern" ? (sel.exclude || []).join(", ") : "");
  const auto = document.getElementById("job-db-auto");
  if (auto) auto.checked = !!sel.auto_include_new;
  setValue("job-db-parallelism", String(Math.min(Math.max(Number(job && job.parallelism) || 1, 1), 4)));
  jobDbs.extraDatabases = mode === "all" || mode === "pattern" ? (sel.databases || []).slice() : [];
  jobDbsSetMode(mode, false);
  jobDbsRenderLists();
}

// The selection of the form, or null after telling the user what is missing.
function jobDbsSelection() {
  switch (jobDbs.mode) {
    case "list": {
      const problem = dbSelProblem("list", jobDbs.names, jobDbs.selected, jobDbs.excluded);
      if (problem) {
        showToast(t(problem), "error");
        return null;
      }
      return {
        mode: "list",
        databases: dbSelIncluded("list", jobDbs.names, jobDbs.selected, jobDbs.excluded),
        collection_filters: dbSelJobFilters(jobDbs, "list")
      };
    }
    case "all": {
      // Databases the job names explicitly are backed up whatever is excluded.
      const problem = (jobDbs.extraDatabases || []).length > 0 ? "" : dbSelProblem("all", jobDbs.names, jobDbs.selected, jobDbs.excluded);
      if (problem) {
        showToast(t(problem), "error");
        return null;
      }
      return {
        mode: "all",
        databases: (jobDbs.extraDatabases || []).slice(),
        exclude: Array.from(jobDbs.excluded).sort().concat(jobDbs.extraExcludes),
        collection_filters: dbSelJobFilters(jobDbs, "all"),
        auto_include_new: !!(document.getElementById("job-db-auto") || {}).checked
      };
    }
    case "pattern": {
      const include = jobDbsPatterns("job-db-include");
      if (include.length === 0) {
        const input = document.getElementById("job-db-include");
        if (input) input.focus();
        showToast(t("jobdb.need_pattern"), "error");
        return null;
      }
      return {
        mode: "pattern",
        databases: (jobDbs.extraDatabases || []).slice(),
        include,
        exclude: jobDbsPatterns("job-db-exclude"),
        auto_include_new: !!(document.getElementById("job-db-auto") || {}).checked
      };
    }
    default:
      return null;
  }
}

// The source part of the job payload: the shared picker for a single database,
// else the connection with the selection (no collection filters).
function jobDbsSource() {
  if (jobDbs.mode === "single") {
    const source = pickerValue("job");
    if (!source) return null;
    return { ...source, database_selection: { mode: "single", databases: [source.database] }, parallelism: 1 };
  }
  const conn = document.getElementById("job-connection");
  if (!conn || !conn.value) {
    if (conn) conn.focus();
    showToast(t("conn.add_first"), "error");
    return null;
  }
  const selection = jobDbsSelection();
  if (!selection) return null;
  return {
    connection_id: conn.value,
    database: "",
    collections: [],
    exclude_collections: [],
    database_selection: selection,
    parallelism: parseInt(getValue("job-db-parallelism"), 10) || 1
  };
}

// ---------------------------------------------------------------------------
// Job form: live preview (GET .../databases/preview), debounced
// ---------------------------------------------------------------------------

function jobDbsSchedulePreview() {
  clearTimeout(jobDbs.preview.timer);
  if (jobDbs.mode === "single") {
    jobDbsRenderPreview(null);
    return;
  }
  jobDbs.preview.timer = setTimeout(jobDbsLoadPreview, 400);
}

// The query of a preview of the form's selection (without validation messages).
function jobDbsPreviewQuery() {
  const conn = getValue("job-connection");
  if (!conn) return null;
  const q = new URLSearchParams();
  q.set("connection_id", conn);
  q.set("mode", jobDbs.mode);
  const add = (key, list) => list.forEach(v => q.append(key, v));
  const auto = !!(document.getElementById("job-db-auto") || {}).checked;
  switch (jobDbs.mode) {
    case "list":
      if (jobDbs.selected.size === 0) return null;
      add("databases", Array.from(jobDbs.selected));
      break;
    case "all":
      add("databases", jobDbs.extraDatabases || []);
      add("exclude", Array.from(jobDbs.excluded).concat(jobDbs.extraExcludes));
      q.set("auto_include_new", String(auto));
      break;
    case "pattern": {
      const include = jobDbsPatterns("job-db-include");
      if (include.length === 0) return null;
      add("databases", jobDbs.extraDatabases || []);
      add("include", include);
      add("exclude", jobDbsPatterns("job-db-exclude"));
      q.set("auto_include_new", String(auto));
      break;
    }
    default:
      return null;
  }
  return q;
}

async function jobDbsLoadPreview() {
  const q = jobDbsPreviewQuery();
  const seq = ++jobDbs.preview.seq;
  if (!q) {
    jobDbsRenderPreview(null);
    return;
  }
  jobDbsRenderPreview({ loading: true });
  const base = editingJobId
    ? `/api/v1/jobs/${encodeURIComponent(editingJobId)}/databases/preview`
    : "/api/v1/jobs/databases/preview";
  try {
    const json = await apiJSON(`${base}?${q.toString()}`);
    if (seq !== jobDbs.preview.seq) return;
    if (!json.success || !json.data) {
      jobDbsRenderPreview({ error: json.error || "" });
      return;
    }
    jobDbsRenderPreview({ data: json.data });
  } catch (err) {
    if (seq !== jobDbs.preview.seq) return;
    jobDbsRenderPreview({ error: err.message });
  }
}

// "a, b, c +N more" for names.
function jobDbsNameList(names, max) {
  const limit = max || JOBDB_PREVIEW_NAMES;
  const shown = names.slice(0, limit).join(", ");
  return names.length > limit ? `${shown} ${tf("jobdb.preview_more", { n: names.length - limit })}` : shown;
}

// Renders the preview box: nothing (single), loading, an error or the result.
function jobDbsRenderPreview(state) {
  const box = document.getElementById("job-db-preview");
  if (!box) return;
  box.textContent = "";
  if (!state || jobDbs.mode === "single") {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  box.classList.remove("text-danger");
  const line = (text, cls) => {
    const p = document.createElement("p");
    if (cls) p.className = cls;
    p.textContent = text;
    box.appendChild(p);
    return p;
  };
  if (state.loading) {
    line(t("jobdb.preview_loading"), "form-hint");
    return;
  }
  if (state.error !== undefined) {
    line(tf("jobdb.preview_failed", { error: truncate(errorSummary(state.error), 160) || "?" }), "form-hint text-danger");
    return;
  }
  const d = state.data;
  const included = d.included || [];
  if (included.length === 0) {
    line(t("jobdb.preview_none"), "preview-title text-danger");
  } else {
    line(included.length === 1 ? t("jobdb.preview_one") : tf("jobdb.preview_n", { n: included.length }), "preview-title");
    const names = line(jobDbsNameList(included), "mono preview-names");
    names.title = included.join(", ");
  }
  const fresh = d.new_since_last_run || [];
  if (fresh.length > 0) {
    const auto = !!(document.getElementById("job-db-auto") || {}).checked;
    line(tf(auto ? "jobdb.preview_new_included" : "jobdb.preview_new_excluded", { n: fresh.length, list: jobDbsNameList(fresh, 6) }), "form-hint");
  }
  const missing = d.missing || [];
  if (missing.length > 0) line(tf("jobdb.preview_missing", { list: jobDbsNameList(missing, 6) }), "form-hint text-danger");
  (d.warnings || []).forEach(w => line(w, "form-hint"));
}

// ---------------------------------------------------------------------------
// Jobs table and details
// ---------------------------------------------------------------------------

// The databases a multi-database job backs up as far as the dashboard knows them:
// the named ones of a list, else its known databases plus the named ones.
function jobDbsKnownNames(job) {
  const sel = job.database_selection || {};
  if (sel.mode === "list") return (sel.databases || []).slice();
  const known = Array.isArray(job.known_databases) ? job.known_databases.slice() : null;
  if (!known) return null;
  (sel.databases || []).forEach(n => { if (!known.includes(n)) known.push(n); });
  return known.sort();
}

// Database cell of the jobs table: the database, or "3 databases" / "All (12)" /
// "prod_* (4)" with the names in the tooltip.
function jobDatabaseCell(job) {
  const sel = job.database_selection || {};
  if (!sel.mode || sel.mode === "single") {
    return `${ellipsis(job.database || (sel.databases || [])[0] || "", "mono ell-sm")}${collectionScope(job)}`;
  }
  const names = jobDbsKnownNames(job);
  let label;
  switch (sel.mode) {
    case "list":
      label = tf("jobdb.cell_n", { n: (sel.databases || []).length });
      break;
    case "all":
      label = names ? tf("jobdb.cell_all_n", { n: names.length }) : t("jobdb.cell_all");
      break;
    default: {
      const pattern = (sel.include || []).join(", ");
      label = names ? tf("jobdb.cell_pattern_n", { pattern, n: names.length }) : pattern;
    }
  }
  const title = names && names.length > 0 ? jobDbsNameList(names, 40) : jobDatabaseSummary(job);
  return `<span class="ellipsis ell-sm" title="${escapeHtml(title)}">${escapeHtml(label)}</span>`
    + `<div class="cell-sub">${escapeHtml(t(`jobdb.mode_${sel.mode}`))}</div>`;
}

// One-line description of a job's selection for the details dialog.
function jobDatabaseSummary(job) {
  const sel = job.database_selection || {};
  const list = arr => (arr || []).join(", ");
  let text;
  switch (sel.mode) {
    case "list":
      return tf("jobdb.summary_list", { list: list(sel.databases) }) + jobDbsFiltersSummary(sel);
    case "all":
      text = (sel.exclude || []).length > 0 ? tf("jobdb.summary_all_except", { list: list(sel.exclude) }) : t("jobdb.summary_all");
      break;
    case "pattern":
      text = (sel.exclude || []).length > 0
        ? tf("jobdb.summary_pattern_except", { include: list(sel.include), exclude: list(sel.exclude) })
        : tf("jobdb.summary_pattern", { list: list(sel.include) });
      break;
    default:
      return job.database || list(sel.databases);
  }
  if ((sel.databases || []).length > 0) text += `, ${tf("jobdb.summary_plus", { list: list(sel.databases) })}`;
  text += `; ${t(sel.auto_include_new ? "jobdb.summary_auto" : "jobdb.summary_frozen")}`;
  return text + jobDbsFiltersSummary(sel);
}

// "; shop: orders · crm: all except logs" for the collection filters of sel, or "".
function jobDbsFiltersSummary(sel) {
  const filters = sel.collection_filters || [];
  if (filters.length === 0) return "";
  const tmp = { ...dbSelFilterState(), selected: new Set(), excluded: new Set() };
  const mode = sel.mode === "all" ? "all" : "list";
  dbSelLoadFilters(tmp, mode, filters);
  if (mode === "list") filters.forEach(f => tmp.selected.add(f.name));
  const parts = filters.map(f => tf("jobdb.summary_colls", { db: f.name, colls: dbSelFilterSummary(tmp, mode, f.name) }));
  return `; ${parts.join(" · ")}`;
}

// The newest run of multi-database job job (GET /api/v1/stats job_last_runs), or null.
function jobLastRun(job) {
  if (!jobIsMulti(job) || !state.stats || !state.stats.job_last_runs) return null;
  return state.stats.job_last_runs[job.id] || null;
}

// Badge kind and label of a job run status.
function jobRunKind(status) {
  const kinds = { ok: "success", partial: "warn", failed: "danger", cancelled: "warn", running: "running" };
  return [kinds[status] || "neutral", t(`jobdb.run_status_${status}`) || status];
}

// The last-run mark of a multi-database job: "" without runs, null for a
// single-database job (whose newest backup stands). A partial run is an amber
// "Partial" badge with "4/5 databases", never a success mark. detailed (the
// details dialog) also names the failed databases.
function jobLastRunMark(job, detailed) {
  if (!jobIsMulti(job)) return null;
  const run = jobLastRun(job);
  if (!run) return "";
  const [kind, label] = jobRunKind(run.status);
  const failed = run.failed_databases || [];
  const title = failed.length > 0 ? tf("overview.spark_failed_dbs", { list: failed.join(", ") }) : "";
  const count = run.status === "running" ? "" : tf("jobdb.n_of_m", { ok: run.succeeded || 0, n: run.databases || 0 });
  const mark = run.status === "partial" || detailed ? statusBadge(kind, label, title) : statusMark(kind, label);
  let sub = count ? `<span class="cell-sub jobrun-count"${title ? ` title="${escapeHtml(title)}"` : ""}>${escapeHtml(count)}</span>` : "";
  if (detailed && failed.length > 0) sub += ` <span class="cell-sub">${escapeHtml(title)}</span>`;
  return `<span class="jobrun-mark">${mark}${sub}</span>`;
}

// Whether job covers several databases.
function jobIsMulti(job) {
  const mode = job && job.database_selection && job.database_selection.mode;
  return !!mode && mode !== "single";
}

// Adds the selection rows to the details overview (called by app.js).
function jobDbsDetailsOverview(job, overview) {
  if (!jobIsMulti(job)) return false;
  appendKv(overview, t("jobdb.mode_label"), jobDatabaseSummary(job));
  const n = Math.min(Math.max(Number(job.parallelism) || 1, 1), 4);
  if (n > 1) appendKv(overview, t("jobdb.parallelism_label"), tf("jobdb.summary_parallel", { n }));
  const names = jobDbsKnownNames(job);
  if (names && names.length > 0) {
    const el = document.createElement("span");
    el.className = "mono";
    el.textContent = jobDbsNameList(names, 30);
    el.title = names.join(", ");
    appendKv(overview, t("job_details.database"), el);
  }
  return true;
}

// Fetches the runs of job jobID for the details dialog.
async function jobDbsLoadRuns(jobID) {
  if (!jobID) return;
  const seq = ++jobDbs.runs.seq;
  if (jobDbs.runs.jobId !== jobID) {
    jobDbs.runs.jobId = jobID;
    jobDbs.runs.list = null;
  }
  try {
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}/runs?limit=${JOBDB_RUNS_LIMIT}`);
    if (seq !== jobDbs.runs.seq) return;
    if (!json.success) throw new Error(json.error || "");
    jobDbs.runs.list = json.data || [];
    jobDbs.runs.failed = false;
  } catch (err) {
    if (seq !== jobDbs.runs.seq) return;
    jobDbs.runs.failed = true;
  }
  renderJobDetails();
}

// Badge of a job run status.
function jobRunBadge(status) {
  const [kind, label] = jobRunKind(status);
  return statusBadge(kind, label);
}

// Renders the details' run history grouped by run and the newly discovered
// databases of a multi-database job; single-database jobs keep the flat history.
function jobDbsRenderDetails(job) {
  const section = document.getElementById("job-details-jobruns-section");
  const flat = document.getElementById("job-details-h-history");
  const multi = jobIsMulti(job);
  if (section) section.hidden = !multi;
  if (flat) {
    const flatSection = flat.closest("section");
    if (flatSection) flatSection.hidden = multi;
  }
  if (!multi || !section) return;
  if (jobDbs.runs.jobId !== job.id) {
    jobDbsLoadRuns(job.id);
    return;
  }
  const container = document.getElementById("job-details-jobruns");
  const empty = document.getElementById("job-details-jobruns-empty");
  const runs = jobDbs.runs.list;
  container.textContent = "";
  if (!runs) {
    empty.hidden = false;
    empty.textContent = jobDbs.runs.failed ? t("jobdb.runs_failed") : t("tables.loading");
  } else {
    empty.hidden = runs.length > 0;
    empty.textContent = t("jobdb.runs_empty");
    runs.forEach(run => container.appendChild(jobDbsRunNode(run)));
  }
  jobDbsRenderNew(job, runs || []);
}

// One run: "Run of 12:00 — 5/6 databases ok" with its databases.
function jobDbsRunNode(run) {
  const dbs = run.databases || [];
  const ok = dbs.filter(d => d.status === "completed").length;
  const details = document.createElement("details");
  details.className = "jobrun";
  const summary = document.createElement("summary");
  const d = parseDate(run.started_at);
  const when = d ? formatAbsolute(d) : "";
  summary.innerHTML = `${jobRunBadge(run.status)} <span class="jobrun-line">${escapeHtml(tf("jobdb.run_line", { time: when, ok, n: dbs.length }))}</span>`;
  const extras = [];
  if ((run.new_databases || []).length > 0) extras.push(tf("jobdb.run_new_n", { n: run.new_databases.length }));
  if ((run.added_databases || []).length > 0) extras.push(tf("jobdb.run_added_n", { n: run.added_databases.length }));
  if (extras.length > 0) {
    const sub = document.createElement("span");
    sub.className = "cell-sub jobrun-extra";
    sub.textContent = extras.join(" · ");
    summary.appendChild(sub);
  }
  details.appendChild(summary);
  if (run.error) {
    const err = document.createElement("p");
    err.className = "cell-error";
    err.textContent = run.error;
    details.appendChild(err);
  }
  const list = document.createElement("ul");
  list.className = "jobrun-dbs";
  dbs.forEach(db => {
    const li = document.createElement("li");
    const [kind, label] = backupStatus(db.status);
    const name = db.backup_id
      ? `<button type="button" class="link-btn mono" data-action="backup-details" data-id="${escapeHtml(db.backup_id)}" title="${escapeHtml(db.backup_id)}">${escapeHtml(db.database)}</button>`
      : `<span class="mono">${escapeHtml(db.database)}</span>`;
    const error = db.error && db.status !== "completed"
      ? ` <span class="cell-error" title="${escapeHtml(db.error)}">${escapeHtml(truncate(db.error === "database not found" ? t("jobdb.not_found") : errorSummary(db.error), 80))}</span>`
      : "";
    li.innerHTML = `${statusMark(kind, label)} ${name}${error}`;
    list.appendChild(li);
  });
  details.appendChild(list);
  if (run.status !== "ok") details.open = run === (jobDbs.runs.list || [])[0];
  return details;
}

// The databases the latest run found but did not back up, with "Add to job".
function jobDbsRenderNew(job, runs) {
  const box = document.getElementById("job-details-newdbs");
  if (!box) return;
  box.textContent = "";
  const sel = job.database_selection || {};
  const latest = runs.find(r => r.status !== "running") || null;
  const named = new Set(sel.databases || []);
  const fresh = latest ? (latest.new_databases || []).filter(n => !named.has(n)) : [];
  box.hidden = fresh.length === 0 || sel.auto_include_new;
  if (box.hidden) return;
  const title = document.createElement("h5");
  title.className = "detail-subheading";
  title.textContent = t("jobdb.new_title");
  const hint = document.createElement("p");
  hint.className = "form-hint";
  hint.textContent = t("jobdb.new_hint");
  const list = document.createElement("ul");
  list.className = "newdb-list";
  fresh.forEach(name => {
    const li = document.createElement("li");
    const label = document.createElement("span");
    label.className = "mono";
    label.textContent = name;
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn btn-secondary btn-sm";
    btn.dataset.action = "jobdb-add";
    btn.dataset.id = job.id;
    btn.dataset.db = name;
    btn.disabled = jobDbs.adding.has(`${job.id}\x00${name}`);
    btn.textContent = t("jobdb.add_to_job");
    li.append(label, btn);
    list.appendChild(li);
  });
  box.append(title, hint, list);
}

// Adds database name to job jobID: a list job lists it, an all or pattern job
// names it explicitly (always backed up). The job is fetched fresh and sent back
// with its updated_at as precondition.
async function jobDbsAddDatabase(jobID, name) {
  const key = `${jobID}\x00${name}`;
  if (!jobID || !name || jobDbs.adding.has(key)) return;
  jobDbs.adding.add(key);
  renderJobDetails();
  try {
    const fresh = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}`);
    if (!fresh.success || !fresh.data) throw new Error(fresh.error || t("jobdb.add_failed"));
    const job = fresh.data;
    const sel = { ...(job.database_selection || {}) };
    sel.databases = Array.from(new Set([...(sel.databases || []), name]));
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...jobUpdateBody(job), database_selection: sel, updated_at: job.updated_at })
    });
    if (json.success) {
      showToast(tf("jobdb.added_toast", { db: name }), "success");
    } else if (json.httpStatus === 409) {
      showToast(t("job_edit.conflict"), "error");
    } else {
      showToast(json.error || t("jobdb.add_failed"), "error");
    }
  } catch (err) {
    showToast(err.message || t("jobdb.add_failed"), "error");
  } finally {
    jobDbs.adding.delete(key);
    refreshAll();
  }
}

// The fields of a PUT that keep job as it is stored (the caller adds its change).
function jobUpdateBody(job) {
  return {
    name: job.name || "",
    cron_expression: job.cron_expression || "",
    database: job.database || "",
    database_selection: job.database_selection,
    parallelism: job.parallelism || 1,
    collections: job.collections || [],
    exclude_collections: job.exclude_collections || [],
    connection_id: job.connection_id || "",
    storage_target_id: job.storage_target_id || ""
  };
}

// Stops the current run of job jobID (every database of it) after confirmation.
async function jobDbsStopRun(jobID) {
  if (!jobID) return;
  if (!(await confirmDialog({ body: t("jobdb.stop_confirm"), danger: true, confirmLabel: t("dialog.confirm") }))) return;
  try {
    const json = await apiJSON(`/api/v1/jobs/${encodeURIComponent(jobID)}/cancel`, { method: "POST" });
    if (json.success) {
      showToast(t("jobdb.stop_requested"), "info");
    } else {
      showToast(json.error || t("run.cancel_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    refreshAll();
  }
}

// Text of the per-database breakdown of a retention preview, or "".
function jobDbsRetentionBreakdown(preview) {
  const dbs = (preview && preview.databases) || [];
  if (dbs.length < 2) return "";
  const parts = dbs.filter(d => d.delete > 0).map(d => `${d.database} ${d.delete}`);
  return parts.length > 0 ? tf("jobdb.retention_per_db", { list: parts.join(", ") }) : "";
}

// ---------------------------------------------------------------------------
// Backup now: one or several databases
// ---------------------------------------------------------------------------

const INSTANTDB_MODES = ["single", "list", "all"];
// The most databases one Backup now may name (MaxBackupDatabases on the server).
const INSTANTDB_MAX = 200;

const instantDbs = {
  mode: "single",
  // Databases of the connection ({name, size_bytes, empty}), or null.
  dbs: null,
  listFailed: false,
  selected: new Set(),
  excluded: new Set(),
  search: "",
  // Collection filters per database (see dbSelFilterState).
  ...dbSelFilterState(),
};

function setupInstantDatabases() {
  dbSelBind("instant", INSTANTDB_MODES, instantDbs, {
    setMode: mode => instantDbsSetMode(mode),
    render: () => instantDbsRenderLists(),
    changed: () => instantDbsRenderSummary()
  });
}

// Switches the Backup now dialog to mode: one database with its collection
// filters, or several (checked, or all but the checked ones) at a parallelism.
function instantDbsSetMode(mode) {
  if (!INSTANTDB_MODES.includes(mode)) mode = "single";
  instantDbs.mode = mode;
  dbSelShowMode("instant", INSTANTDB_MODES, mode);
  const multi = mode !== "single";
  const par = document.getElementById("instant-db-parallel-group");
  if (par) par.hidden = !multi;
  // Several databases choose their collections in the tree (Selected, All).
  const colls = document.getElementById("instant-collections-fieldset");
  if (colls) {
    colls.disabled = multi;
    colls.hidden = multi;
  }
  instantDbsApplyRequired();
  // The boxes always show the set of the mode now shown.
  if (multi) instantDbsRenderLists();
  instantDbsRenderSummary();
}

// Only a single database needs the picker's database field (see jobDbsApplyRequired).
function instantDbsApplyRequired() {
  const single = instantDbs.mode === "single";
  const select = document.getElementById("instant-database-select");
  const manual = document.getElementById("instant-database");
  if (select) select.required = single && !select.hidden;
  if (manual) manual.required = single && !manual.hidden;
}

// Called (through jobDbsOnDatabases) when the dialog's database list loaded or failed.
function instantDbsOnDatabases(p) {
  instantDbs.dbs = Array.isArray(p.dbs) ? p.dbs : null;
  instantDbs.listFailed = !Array.isArray(p.dbs) && !!p.dbError;
  // Checked names of another connection do not carry over.
  const names = instantDbsListed();
  instantDbs.selected = new Set(Array.from(instantDbs.selected).filter(n => names.includes(n)));
  instantDbs.excluded = new Set(Array.from(instantDbs.excluded).filter(n => names.includes(n)));
  const conn = document.getElementById("instant-connection");
  dbSelKeepFilters(instantDbs, names, conn ? conn.value : "");
  instantDbsApplyRequired();
  instantDbsRenderLists();
  instantDbsRenderSummary();
}

// Names of the databases the connection lists.
function instantDbsListed() {
  return (instantDbs.dbs || []).map(db => db.name);
}

function instantDbsRenderLists() {
  const names = instantDbsListed();
  const sizes = new Map((instantDbs.dbs || []).map(db => [db.name, db]));
  const detail = name => {
    const db = sizes.get(name);
    if (!db) return "";
    return db.empty ? t("picker.empty_db") : formatBytes(db.size_bytes);
  };
  const failed = instantDbs.listFailed ? "instantdb.list_failed" : "";
  dbSelRenderTree("instant-dbs-list", "instant-dbs-list-empty", "list", instantDbs, names, failed, detail);
  dbSelRenderTree("instant-dbs-exclude-list", "instant-dbs-exclude-empty", "all", instantDbs, names, failed, detail);
}

// Resets the dialog to a single database (openBackupNowModal).
function instantDbsReset() {
  instantDbs.dbs = null;
  instantDbs.listFailed = false;
  instantDbs.selected = new Set();
  instantDbs.excluded = new Set();
  instantDbs.search = "";
  Object.assign(instantDbs, dbSelFilterState());
  setValue("instant-dbs-search", "");
  setValue("instant-db-parallelism", "1");
  instantDbsSetMode("single");
  instantDbsRenderLists();
}

// The databases the dialog backs up in a multi-database mode, sorted.
function instantDbsNames() {
  return dbSelIncluded(instantDbs.mode, instantDbsListed(), instantDbs.selected, instantDbs.excluded);
}

// The translation key of what keeps the dialog's databases from being backed up,
// or "" (see dbSelProblem). All backs up the listed databases, so it also needs the
// list: still loading, failed or empty.
function instantDbsProblem() {
  const problem = dbSelProblem(instantDbs.mode, instantDbsListed(), instantDbs.selected, instantDbs.excluded);
  if (problem || instantDbs.mode !== "all" || instantDbsListed().length > 0) return problem;
  if (instantDbs.listFailed) return "instantdb.list_failed";
  return instantDbs.dbs === null ? "jobdb.preview_loading" : "jobdb.list_none";
}

// "3 databases selected · about 1.2 GB" from the sizes the connection listed.
function instantDbsRenderSummary() {
  const box = document.getElementById("instant-db-summary");
  if (!box) return;
  if (instantDbs.mode === "single") {
    box.hidden = true;
    box.textContent = "";
    return;
  }
  box.hidden = false;
  const names = instantDbsNames();
  const problem = instantDbsProblem();
  box.classList.toggle("text-danger", (problem !== "" && problem !== "jobdb.preview_loading") || names.length > INSTANTDB_MAX);
  if (problem) {
    box.textContent = t(problem);
    return;
  }
  if (names.length > INSTANTDB_MAX) {
    box.textContent = tf("instantdb.too_many", { max: INSTANTDB_MAX });
    return;
  }
  const sizes = new Map((instantDbs.dbs || []).map(db => [db.name, Number(db.size_bytes) || 0]));
  const known = names.filter(n => sizes.has(n));
  const total = known.reduce((sum, n) => sum + sizes.get(n), 0);
  box.textContent = known.length > 0
    ? tf("instantdb.summary_n", { n: names.length, size: formatBytes(total) })
    : tf("instantdb.summary_n_nosize", { n: names.length });
}

// The source part of the Backup now request: the picker for a single database,
// else the connection, the databases and the parallelism; null after telling the
// user what is missing.
function instantDbsSource() {
  if (instantDbs.mode === "single") return pickerValue("instant");
  const conn = document.getElementById("instant-connection");
  if (!conn || !conn.value) {
    if (conn) conn.focus();
    showToast(t("conn.add_first"), "error");
    return null;
  }
  const problem = instantDbsProblem();
  if (problem) {
    showToast(t(problem), "error");
    return null;
  }
  const databases = instantDbsNames();
  if (databases.length > INSTANTDB_MAX) {
    showToast(tf("instantdb.too_many", { max: INSTANTDB_MAX }), "error");
    return null;
  }
  return {
    connection_id: conn.value,
    // A database with a collection filter is sent as {name, collections | exclude_collections}.
    databases: databases.map(db => dbSelEntry(instantDbs, instantDbs.mode, db)),
    parallelism: parseInt(getValue("instant-db-parallelism"), 10) || 1
  };
}

// Reports a started run of several backups (the answer of POST /api/v1/backups with
// databases) in one toast that opens the run's backups.
function instantDbsStarted(run) {
  const busy = (run && run.busy) || [];
  let text = tf("instantdb.started_n", { n: ((run && run.backups) || []).length });
  if (busy.length > 0) text += ` ${tf("instantdb.busy_list", { list: busy.map(b => b.database).join(", ") })}`;
  const id = run && run.run_id;
  const opts = id && typeof showRunBackups === "function"
    ? { action: { label: t("instantdb.view_run"), run: () => showRunBackups(id) } } : undefined;
  showToast(text, "info", opts);
}

// Whether backup record b holds only some collections of its database.
function backupIsFiltered(b) {
  return !!b && (!!b.filtered || (b.collections || []).length > 0 || (b.exclude_collections || []).length > 0);
}

// What the collection filter of backup b kept, in words ("Only these collections:
// orders" or "Without these collections: logs, tmp"), or "".
function backupFilterText(b) {
  const inc = (b && b.collections) || [];
  const exc = (b && b.exclude_collections) || [];
  const parts = [];
  if (inc.length > 0) parts.push(tf("jobdb.filtered_only", { list: jobDbsNameList(inc, 8) }));
  if (exc.length > 0) parts.push(tf("jobdb.filtered_without", { list: jobDbsNameList(exc, 8) }));
  return parts.join("; ");
}

// The "Filtered" chip of a backup that holds only some collections, or "".
function backupFilterBadge(b) {
  if (!backupIsFiltered(b)) return "";
  return `<span class="trust-chip trust-warn" title="${escapeHtml(backupFilterText(b))}">${escapeHtml(t("jobdb.filtered_badge"))}</span>`;
}

// Delegated actions of this module (called by runs.js for actions it does not
// handle).
function handleJobDbAction(action, id, btn) {
  if (action === "jobdb-add") {
    jobDbsAddDatabase(id, btn.dataset.db || "");
    return true;
  }
  return false;
}

document.addEventListener("DOMContentLoaded", setupJobDatabases);
document.addEventListener("DOMContentLoaded", setupInstantDatabases);
