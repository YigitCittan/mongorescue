/**
 * MongoRescue dashboard: read preferences, throttling and backup windows.
 *
 * The job form's "Read preference, throttling and backup window" section
 * (read_preference, read_preference_tags, max_upload_mbps,
 * num_parallel_collections, backup_window), the connection form's read preference
 * and max_concurrent_backups, the member a connection test reports (read_member),
 * the general max_upload_mbps setting (wired in app.js) and the warning that a
 * manual run ignores the job's backup window.
 *
 * Loaded after app.js, trust.js (mergeTranslations) and bulk.js (confirmDialog);
 * app.js calls throttleFillJobForm(), throttleJobPayload(),
 * throttleFillConnectionForm(), throttleConnectionPayload(),
 * throttleConnectionReadPref(), throttleDescribeMember() and
 * throttleConfirmManualRun(). Server values reach the DOM only through
 * textContent or escapeHtml().
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const THROTTLE_TRANSLATIONS = {
  en: {
    throttle: {
      job_section: "Read preference, throttling and backup window",
      read_pref: "Read preference",
      read_pref_connection: "Connection's setting",
      read_pref_uri: "As in the connection string",
      read_pref_job_hint: "The member mongodump reads from. secondary fails when no secondary is available; secondaryPreferred falls back to the primary.",
      read_pref_conn_hint: "Which member backups read from; a job may override it. Use secondary or secondaryPreferred to keep dumps off a busy primary.",
      tags: "Tag sets",
      tags_hint: "Optional. name=value pairs separated by commas; separate tag sets with semicolons, tried in order. Not with primary.",
      tags_invalid: "Write tags as name=value, separated by commas, and tag sets separated by semicolons.",
      max_upload: "Upload cap (Mbit/s)",
      max_upload_job_hint: "Empty or 0 uses the general setting.",
      max_upload_settings_hint: "Caps the upload of every backup whose job sets no cap. 0 is unlimited.",
      num_parallel: "Collections dumped in parallel",
      num_parallel_hint: "mongodump --numParallelCollections, 1 to 16. Empty keeps mongodump's default of 4; 1 is the gentlest.",
      max_concurrent: "Concurrent backups",
      max_concurrent_hint: "At most this many backups read from this connection at once; others wait (shown as waiting). Empty or 0 is unlimited.",
      settings_title: "Throttling",
      window_enable: "Start scheduled runs only within a backup window",
      window_start: "Opens at",
      window_end: "Closes at",
      window_end_hint: "A time before the opening time closes the window the next day.",
      window_tz: "Time zone",
      window_days: "Days",
      window_days_hint: "The days the window opens on; none checked means every day.",
      window_cancel: "Cancel a run still going when the window closes",
      window_note: "Runs outside the window are recorded as skipped, not failed. Manual runs ignore the window. The default RPO counts only the runs the window allows.",
      window_required: "Enter when the window opens and closes; they must differ.",
      window_summary: "{days} {start}–{end} ({tz})",
      every_day: "Every day",
      manual_title: "Run outside the backup window?",
      manual_body: "This job's scheduled runs start only within {window}. A manual run ignores the window and starts now, also when it is closed.",
      manual_confirm: "Run now",
      member: " Backups read from {member}.",
      member_rp: " With {rp}, backups read from {member}.",
      member_failed: " No member matches the read preference: {error}",
    },
    run: { phase_waiting: "Waiting for a slot" },
    jobdb: { run_status_skipped: "Skipped", run_skipped_line: "{n} scheduled runs skipped outside the window since {time}" },
    notify: { events: { backup_skipped: "Backup skipped (outside window)" } },
  },
  tr: {
    throttle: {
      job_section: "Okuma tercihi, kısıtlama ve yedekleme penceresi",
      read_pref: "Okuma tercihi",
      read_pref_connection: "Bağlantının ayarı",
      read_pref_uri: "Bağlantı dizesindeki gibi",
      read_pref_job_hint: "mongodump'ın okuduğu üye. Kullanılabilir ikincil yoksa secondary başarısız olur; secondaryPreferred birincile döner.",
      read_pref_conn_hint: "Yedeklerin okuduğu üye; bir iş bunu değiştirebilir. Dökümleri yoğun birincilden uzak tutmak için secondary veya secondaryPreferred kullanın.",
      tags: "Etiket kümeleri",
      tags_hint: "İsteğe bağlı. Virgülle ayrılmış ad=değer çiftleri; etiket kümelerini noktalı virgülle ayırın, sırayla denenir. primary ile kullanılamaz.",
      tags_invalid: "Etiketleri virgülle ayrılmış ad=değer olarak, etiket kümelerini noktalı virgülle ayırarak yazın.",
      max_upload: "Yükleme sınırı (Mbit/sn)",
      max_upload_job_hint: "Boş veya 0 genel ayarı kullanır.",
      max_upload_settings_hint: "İşi kendi sınırını belirlemeyen her yedeğin yüklemesini sınırlar. 0 sınırsızdır.",
      num_parallel: "Paralel dökülen koleksiyonlar",
      num_parallel_hint: "mongodump --numParallelCollections, 1 ile 16 arası. Boş bırakılırsa mongodump'ın varsayılanı 4 kalır; 1 en hafifidir.",
      max_concurrent: "Eşzamanlı yedekler",
      max_concurrent_hint: "Bu bağlantıdan aynı anda en fazla bu kadar yedek okur; diğerleri bekler (bekliyor olarak gösterilir). Boş veya 0 sınırsızdır.",
      settings_title: "Kısıtlama",
      window_enable: "Zamanlanmış çalıştırmaları yalnızca bir yedekleme penceresinde başlat",
      window_start: "Açılış",
      window_end: "Kapanış",
      window_end_hint: "Açılıştan önceki bir saat, pencereyi ertesi gün kapatır.",
      window_tz: "Saat dilimi",
      window_days: "Günler",
      window_days_hint: "Pencerenin açıldığı günler; hiçbiri işaretli değilse her gün.",
      window_cancel: "Pencere kapandığında hâlâ süren çalıştırmayı iptal et",
      window_note: "Pencere dışındaki çalıştırmalar başarısız değil, atlandı olarak kaydedilir. Elle çalıştırmalar pencereyi yok sayar. Varsayılan RPO yalnızca pencerenin izin verdiği çalıştırmaları sayar.",
      window_required: "Pencerenin açılış ve kapanış saatini girin; farklı olmalılar.",
      window_summary: "{days} {start}–{end} ({tz})",
      every_day: "Her gün",
      manual_title: "Yedekleme penceresi dışında çalıştırılsın mı?",
      manual_body: "Bu işin zamanlanmış çalıştırmaları yalnızca {window} içinde başlar. Elle çalıştırma pencereyi yok sayar ve pencere kapalı olsa da şimdi başlar.",
      manual_confirm: "Şimdi çalıştır",
      member: " Yedekler {member} üyesinden okur.",
      member_rp: " {rp} ile yedekler {member} üyesinden okur.",
      member_failed: " Okuma tercihine uyan üye yok: {error}",
    },
    run: { phase_waiting: "Sıra bekliyor" },
    jobdb: { run_status_skipped: "Atlandı", run_skipped_line: "{time} itibarıyla pencere dışında kalan {n} zamanlanmış çalıştırma atlandı" },
    notify: { events: { backup_skipped: "Yedek atlandı (pencere dışında)" } },
  },
  de: {
    throttle: {
      job_section: "Lesepräferenz, Drosselung und Sicherungsfenster",
      read_pref: "Lesepräferenz",
      read_pref_connection: "Einstellung der Verbindung",
      read_pref_uri: "Wie im Verbindungsstring",
      read_pref_job_hint: "Das Mitglied, von dem mongodump liest. secondary schlägt fehl, wenn kein Secondary verfügbar ist; secondaryPreferred weicht auf den Primary aus.",
      read_pref_conn_hint: "Von welchem Mitglied Sicherungen lesen; ein Job kann es überschreiben. secondary oder secondaryPreferred halten Dumps von einem ausgelasteten Primary fern.",
      tags: "Tag-Sets",
      tags_hint: "Optional. Name=Wert-Paare durch Kommas getrennt; Tag-Sets durch Semikolons getrennt, in Reihenfolge versucht. Nicht mit primary.",
      tags_invalid: "Tags als Name=Wert durch Kommas getrennt angeben, Tag-Sets durch Semikolons.",
      max_upload: "Upload-Limit (Mbit/s)",
      max_upload_job_hint: "Leer oder 0 verwendet die allgemeine Einstellung.",
      max_upload_settings_hint: "Begrenzt den Upload jeder Sicherung, deren Job kein eigenes Limit setzt. 0 ist unbegrenzt.",
      num_parallel: "Parallel gesicherte Collections",
      num_parallel_hint: "mongodump --numParallelCollections, 1 bis 16. Leer behält den Standard von 4; 1 ist am schonendsten.",
      max_concurrent: "Gleichzeitige Sicherungen",
      max_concurrent_hint: "Höchstens so viele Sicherungen lesen gleichzeitig von dieser Verbindung; weitere warten (als wartend angezeigt). Leer oder 0 ist unbegrenzt.",
      settings_title: "Drosselung",
      window_enable: "Geplante Läufe nur in einem Sicherungsfenster starten",
      window_start: "Öffnet um",
      window_end: "Schließt um",
      window_end_hint: "Eine Zeit vor der Öffnungszeit schließt das Fenster am nächsten Tag.",
      window_tz: "Zeitzone",
      window_days: "Tage",
      window_days_hint: "Die Tage, an denen das Fenster öffnet; keiner markiert bedeutet jeden Tag.",
      window_cancel: "Einen Lauf abbrechen, der beim Schließen des Fensters noch läuft",
      window_note: "Läufe außerhalb des Fensters werden als übersprungen erfasst, nicht als fehlgeschlagen. Manuelle Läufe ignorieren das Fenster. Das Standard-RPO zählt nur die Läufe, die das Fenster erlaubt.",
      window_required: "Öffnungs- und Schließzeit angeben; sie müssen sich unterscheiden.",
      window_summary: "{days} {start}–{end} ({tz})",
      every_day: "Jeden Tag",
      manual_title: "Außerhalb des Sicherungsfensters ausführen?",
      manual_body: "Die geplanten Läufe dieses Jobs starten nur in {window}. Ein manueller Lauf ignoriert das Fenster und startet jetzt, auch wenn es geschlossen ist.",
      manual_confirm: "Jetzt ausführen",
      member: " Sicherungen lesen von {member}.",
      member_rp: " Mit {rp} lesen Sicherungen von {member}.",
      member_failed: " Kein Mitglied entspricht der Lesepräferenz: {error}",
    },
    run: { phase_waiting: "Wartet auf einen Platz" },
    jobdb: { run_status_skipped: "Übersprungen", run_skipped_line: "{n} geplante Läufe außerhalb des Fensters übersprungen seit {time}" },
    notify: { events: { backup_skipped: "Sicherung übersprungen (außerhalb des Fensters)" } },
  },
  es: {
    throttle: {
      job_section: "Preferencia de lectura, limitación y ventana de copia",
      read_pref: "Preferencia de lectura",
      read_pref_connection: "Ajuste de la conexión",
      read_pref_uri: "Como en la cadena de conexión",
      read_pref_job_hint: "El miembro del que lee mongodump. secondary falla si no hay un secundario disponible; secondaryPreferred recurre al primario.",
      read_pref_conn_hint: "De qué miembro leen las copias; un trabajo puede cambiarlo. Use secondary o secondaryPreferred para no cargar un primario ocupado.",
      tags: "Conjuntos de etiquetas",
      tags_hint: "Opcional. Pares nombre=valor separados por comas; separe los conjuntos con punto y coma, se prueban en orden. No con primary.",
      tags_invalid: "Escriba las etiquetas como nombre=valor separadas por comas y los conjuntos separados por punto y coma.",
      max_upload: "Límite de subida (Mbit/s)",
      max_upload_job_hint: "Vacío o 0 usa el ajuste general.",
      max_upload_settings_hint: "Limita la subida de cada copia cuyo trabajo no fija un límite. 0 es ilimitado.",
      num_parallel: "Colecciones volcadas en paralelo",
      num_parallel_hint: "mongodump --numParallelCollections, de 1 a 16. Vacío mantiene el valor por defecto de 4; 1 es el más suave.",
      max_concurrent: "Copias simultáneas",
      max_concurrent_hint: "Como máximo esta cantidad de copias lee de esta conexión a la vez; las demás esperan (se muestran en espera). Vacío o 0 es ilimitado.",
      settings_title: "Limitación",
      window_enable: "Iniciar las ejecuciones programadas solo dentro de una ventana de copia",
      window_start: "Abre a las",
      window_end: "Cierra a las",
      window_end_hint: "Una hora anterior a la de apertura cierra la ventana al día siguiente.",
      window_tz: "Zona horaria",
      window_days: "Días",
      window_days_hint: "Los días en que abre la ventana; ninguno marcado significa todos los días.",
      window_cancel: "Cancelar una ejecución que siga en curso cuando cierre la ventana",
      window_note: "Las ejecuciones fuera de la ventana se registran como omitidas, no como fallidas. Las ejecuciones manuales ignoran la ventana. El RPO por defecto cuenta solo las ejecuciones que permite la ventana.",
      window_required: "Indique cuándo abre y cierra la ventana; deben ser distintas.",
      window_summary: "{days} {start}–{end} ({tz})",
      every_day: "Todos los días",
      manual_title: "¿Ejecutar fuera de la ventana de copia?",
      manual_body: "Las ejecuciones programadas de este trabajo solo empiezan dentro de {window}. Una ejecución manual ignora la ventana y empieza ahora, aunque esté cerrada.",
      manual_confirm: "Ejecutar ahora",
      member: " Las copias leen de {member}.",
      member_rp: " Con {rp}, las copias leen de {member}.",
      member_failed: " Ningún miembro cumple la preferencia de lectura: {error}",
    },
    run: { phase_waiting: "Esperando turno" },
    jobdb: { run_status_skipped: "Omitida", run_skipped_line: "{n} ejecuciones programadas omitidas fuera de la ventana desde {time}" },
    notify: { events: { backup_skipped: "Copia omitida (fuera de la ventana)" } },
  },
  fr: {
    throttle: {
      job_section: "Préférence de lecture, limitation et fenêtre de sauvegarde",
      read_pref: "Préférence de lecture",
      read_pref_connection: "Réglage de la connexion",
      read_pref_uri: "Comme dans la chaîne de connexion",
      read_pref_job_hint: "Le membre que mongodump lit. secondary échoue sans secondaire disponible ; secondaryPreferred se rabat sur le primaire.",
      read_pref_conn_hint: "Le membre que lisent les sauvegardes ; une tâche peut le remplacer. secondary ou secondaryPreferred épargnent un primaire chargé.",
      tags: "Jeux de tags",
      tags_hint: "Facultatif. Paires nom=valeur séparées par des virgules ; jeux séparés par des points-virgules, essayés dans l'ordre. Pas avec primary.",
      tags_invalid: "Écrivez les tags en nom=valeur séparés par des virgules, et les jeux séparés par des points-virgules.",
      max_upload: "Limite d'envoi (Mbit/s)",
      max_upload_job_hint: "Vide ou 0 utilise le réglage général.",
      max_upload_settings_hint: "Limite l'envoi de chaque sauvegarde dont la tâche ne fixe pas de limite. 0 est illimité.",
      num_parallel: "Collections exportées en parallèle",
      num_parallel_hint: "mongodump --numParallelCollections, de 1 à 16. Vide garde la valeur par défaut de 4 ; 1 est le plus doux.",
      max_concurrent: "Sauvegardes simultanées",
      max_concurrent_hint: "Au plus ce nombre de sauvegardes lit cette connexion en même temps ; les autres attendent (affichées en attente). Vide ou 0 est illimité.",
      settings_title: "Limitation",
      window_enable: "Ne démarrer les exécutions planifiées que dans une fenêtre de sauvegarde",
      window_start: "Ouvre à",
      window_end: "Ferme à",
      window_end_hint: "Une heure antérieure à l'ouverture ferme la fenêtre le lendemain.",
      window_tz: "Fuseau horaire",
      window_days: "Jours",
      window_days_hint: "Les jours où la fenêtre s'ouvre ; aucun coché signifie tous les jours.",
      window_cancel: "Annuler une exécution encore en cours à la fermeture de la fenêtre",
      window_note: "Les exécutions hors fenêtre sont enregistrées comme ignorées, pas comme échouées. Les exécutions manuelles ignorent la fenêtre. Le RPO par défaut ne compte que les exécutions permises par la fenêtre.",
      window_required: "Indiquez l'ouverture et la fermeture de la fenêtre ; elles doivent différer.",
      window_summary: "{days} {start}–{end} ({tz})",
      every_day: "Tous les jours",
      manual_title: "Exécuter hors de la fenêtre de sauvegarde ?",
      manual_body: "Les exécutions planifiées de cette tâche ne démarrent que dans {window}. Une exécution manuelle ignore la fenêtre et démarre maintenant, même fermée.",
      manual_confirm: "Exécuter maintenant",
      member: " Les sauvegardes lisent {member}.",
      member_rp: " Avec {rp}, les sauvegardes lisent {member}.",
      member_failed: " Aucun membre ne correspond à la préférence de lecture : {error}",
    },
    run: { phase_waiting: "En attente d'une place" },
    jobdb: { run_status_skipped: "Ignorée", run_skipped_line: "{n} exécutions planifiées ignorées hors fenêtre depuis {time}" },
    notify: { events: { backup_skipped: "Sauvegarde ignorée (hors fenêtre)" } },
  },
  zh: {
    throttle: {
      job_section: "读取偏好、限速和备份窗口",
      read_pref: "读取偏好",
      read_pref_connection: "使用连接的设置",
      read_pref_uri: "与连接字符串相同",
      read_pref_job_hint: "mongodump 读取的成员。没有可用的从节点时 secondary 会失败；secondaryPreferred 会退回主节点。",
      read_pref_conn_hint: "备份读取哪个成员；作业可以覆盖。使用 secondary 或 secondaryPreferred 可避免占用繁忙的主节点。",
      tags: "标签集",
      tags_hint: "可选。用逗号分隔的 名称=值 对；标签集之间用分号分隔，按顺序尝试。不能与 primary 一起使用。",
      tags_invalid: "标签写作 名称=值，用逗号分隔；标签集之间用分号分隔。",
      max_upload: "上传限速 (Mbit/s)",
      max_upload_job_hint: "留空或 0 表示使用常规设置。",
      max_upload_settings_hint: "限制所有未设置自身限速的作业的备份上传速度。0 表示不限。",
      num_parallel: "并行导出的集合数",
      num_parallel_hint: "mongodump --numParallelCollections，1 到 16。留空保持 mongodump 默认值 4；1 对服务器影响最小。",
      max_concurrent: "并发备份数",
      max_concurrent_hint: "同一时间最多有这么多备份从此连接读取；其余的等待（显示为等待中）。留空或 0 表示不限。",
      settings_title: "限速",
      window_enable: "仅在备份窗口内启动计划运行",
      window_start: "开始时间",
      window_end: "结束时间",
      window_end_hint: "早于开始时间的结束时间表示窗口在次日关闭。",
      window_tz: "时区",
      window_days: "日期",
      window_days_hint: "窗口开启的日期；都不勾选表示每天。",
      window_cancel: "窗口关闭时取消仍在运行的备份",
      window_note: "窗口外的运行记录为已跳过，而不是失败。手动运行忽略窗口。默认 RPO 只计算窗口允许的运行。",
      window_required: "请输入窗口的开始和结束时间，两者不能相同。",
      window_summary: "{days} {start}–{end}（{tz}）",
      every_day: "每天",
      manual_title: "在备份窗口外运行？",
      manual_body: "此作业的计划运行只在 {window} 内开始。手动运行会忽略窗口并立即开始，即使窗口已关闭。",
      manual_confirm: "立即运行",
      member: " 备份从 {member} 读取。",
      member_rp: " 使用 {rp} 时，备份从 {member} 读取。",
      member_failed: " 没有成员符合读取偏好：{error}",
    },
    run: { phase_waiting: "等待空位" },
    jobdb: { run_status_skipped: "已跳过", run_skipped_line: "自 {time} 起有 {n} 次计划运行因在窗口外而跳过" },
    notify: { events: { backup_skipped: "备份已跳过（窗口外）" } },
  },
  ja: {
    throttle: {
      job_section: "読み取り設定、帯域制限、バックアップウィンドウ",
      read_pref: "読み取り設定",
      read_pref_connection: "接続の設定に従う",
      read_pref_uri: "接続文字列のとおり",
      read_pref_job_hint: "mongodump が読み取るメンバー。secondary は利用できるセカンダリがないと失敗し、secondaryPreferred はプライマリにフォールバックします。",
      read_pref_conn_hint: "バックアップが読み取るメンバー。ジョブで上書きできます。忙しいプライマリを避けるには secondary か secondaryPreferred を使います。",
      tags: "タグセット",
      tags_hint: "任意。名前=値 をカンマで区切り、タグセットはセミコロンで区切ります（順に試行）。primary とは併用できません。",
      tags_invalid: "タグは 名前=値 をカンマ区切りで、タグセットはセミコロン区切りで入力してください。",
      max_upload: "アップロード上限 (Mbit/s)",
      max_upload_job_hint: "空または 0 で全般設定を使います。",
      max_upload_settings_hint: "独自の上限がないジョブのすべてのバックアップのアップロードを制限します。0 は無制限です。",
      num_parallel: "並列にダンプするコレクション数",
      num_parallel_hint: "mongodump --numParallelCollections、1〜16。空なら mongodump の既定値 4 のまま。1 が最も負荷が軽くなります。",
      max_concurrent: "同時バックアップ数",
      max_concurrent_hint: "この接続から同時に読み取るバックアップの上限です。超えた分は待機します（待機中と表示）。空または 0 は無制限です。",
      settings_title: "帯域制限",
      window_enable: "スケジュール実行をバックアップウィンドウ内でのみ開始する",
      window_start: "開始時刻",
      window_end: "終了時刻",
      window_end_hint: "開始時刻より前の時刻は翌日にウィンドウを閉じます。",
      window_tz: "タイムゾーン",
      window_days: "曜日",
      window_days_hint: "ウィンドウが開く曜日。何も選ばなければ毎日です。",
      window_cancel: "ウィンドウが閉じた時点で実行中のバックアップをキャンセルする",
      window_note: "ウィンドウ外の実行は失敗ではなくスキップとして記録されます。手動実行はウィンドウを無視します。既定の RPO はウィンドウが許す実行だけを数えます。",
      window_required: "ウィンドウの開始と終了の時刻を入力してください（同じ時刻は不可）。",
      window_summary: "{days} {start}–{end}（{tz}）",
      every_day: "毎日",
      manual_title: "バックアップウィンドウ外で実行しますか？",
      manual_body: "このジョブのスケジュール実行は {window} の間だけ開始します。手動実行はウィンドウを無視し、閉じていても今すぐ開始します。",
      manual_confirm: "今すぐ実行",
      member: " バックアップは {member} から読み取ります。",
      member_rp: " {rp} では、バックアップは {member} から読み取ります。",
      member_failed: " 読み取り設定に合うメンバーがありません: {error}",
    },
    run: { phase_waiting: "空き待ち" },
    jobdb: { run_status_skipped: "スキップ", run_skipped_line: "{time} 以降、ウィンドウ外の {n} 件のスケジュール実行をスキップ" },
    notify: { events: { backup_skipped: "バックアップをスキップ（ウィンドウ外）" } },
  },
  ru: {
    throttle: {
      job_section: "Предпочтение чтения, ограничения и окно резервного копирования",
      read_pref: "Предпочтение чтения",
      read_pref_connection: "Как у подключения",
      read_pref_uri: "Как в строке подключения",
      read_pref_job_hint: "Узел, с которого читает mongodump. secondary завершается ошибкой без доступного вторичного узла; secondaryPreferred переходит на первичный.",
      read_pref_conn_hint: "С какого узла читают резервные копии; задание может переопределить. secondary или secondaryPreferred разгружают занятый первичный узел.",
      tags: "Наборы тегов",
      tags_hint: "Необязательно. Пары имя=значение через запятую; наборы через точку с запятой, проверяются по порядку. Не с primary.",
      tags_invalid: "Укажите теги как имя=значение через запятую, а наборы через точку с запятой.",
      max_upload: "Ограничение выгрузки (Мбит/с)",
      max_upload_job_hint: "Пусто или 0 — общий параметр.",
      max_upload_settings_hint: "Ограничивает выгрузку каждой резервной копии, задание которой не задаёт своего ограничения. 0 — без ограничения.",
      num_parallel: "Коллекций параллельно",
      num_parallel_hint: "mongodump --numParallelCollections, от 1 до 16. Пусто — значение mongodump по умолчанию (4); 1 — самая щадящая нагрузка.",
      max_concurrent: "Одновременных резервных копий",
      max_concurrent_hint: "Не больше стольких резервных копий одновременно читают с этого подключения; остальные ждут (показаны как ожидающие). Пусто или 0 — без ограничения.",
      settings_title: "Ограничения",
      window_enable: "Запускать плановые запуски только в окне резервного копирования",
      window_start: "Открывается в",
      window_end: "Закрывается в",
      window_end_hint: "Время раньше открытия закрывает окно на следующий день.",
      window_tz: "Часовой пояс",
      window_days: "Дни",
      window_days_hint: "Дни, в которые открывается окно; если ничего не отмечено — каждый день.",
      window_cancel: "Отменять запуск, который ещё идёт при закрытии окна",
      window_note: "Запуски вне окна записываются как пропущенные, а не как неудачные. Ручные запуски игнорируют окно. RPO по умолчанию учитывает только разрешённые окном запуски.",
      window_required: "Укажите время открытия и закрытия окна; они должны различаться.",
      window_summary: "{days} {start}–{end} ({tz})",
      every_day: "Каждый день",
      manual_title: "Запустить вне окна резервного копирования?",
      manual_body: "Плановые запуски этого задания начинаются только в {window}. Ручной запуск игнорирует окно и начинается сейчас, даже если оно закрыто.",
      manual_confirm: "Запустить сейчас",
      member: " Резервные копии читают с {member}.",
      member_rp: " С {rp} резервные копии читают с {member}.",
      member_failed: " Нет узла, подходящего под предпочтение чтения: {error}",
    },
    run: { phase_waiting: "Ожидает слота" },
    jobdb: { run_status_skipped: "Пропущен", run_skipped_line: "Пропущено плановых запусков вне окна: {n}, начиная с {time}" },
    notify: { events: { backup_skipped: "Резервная копия пропущена (вне окна)" } },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(THROTTLE_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], THROTTLE_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Tag sets: "dc=east,use=backup; dc=west" <-> [{dc: "east", use: "backup"}, {dc: "west"}]
// ---------------------------------------------------------------------------

// throttleParseTags returns the tag sets of text, or null when it cannot be read.
// An empty text is no tag set; "{}" is the empty set (any member).
function throttleParseTags(text) {
  const sets = [];
  const raw = String(text || "").trim();
  if (!raw) return sets;
  for (const part of raw.split(";")) {
    const p = part.trim();
    if (!p) continue;
    const set = {};
    if (p !== "{}") {
      for (const pair of p.split(",")) {
        const i = pair.indexOf("=");
        if (i <= 0) return null;
        const name = pair.slice(0, i).trim();
        const value = pair.slice(i + 1).trim();
        if (!name || name.includes(":") || value.includes(":")) return null;
        set[name] = value;
      }
    }
    sets.push(set);
  }
  return sets;
}

function throttleFormatTags(sets) {
  return (sets || []).map(set => {
    const names = Object.keys(set || {}).sort();
    return names.length ? names.map(n => `${n}=${set[n]}`).join(",") : "{}";
  }).join("; ");
}

// throttleReadPref reads a read preference select and tag field into the API
// fields; the tag field is marked invalid when it cannot be read.
function throttleReadPref(selectId, tagsId) {
  const mode = getValue(selectId);
  const tagsInput = document.getElementById(tagsId);
  const tags = throttleParseTags(tagsInput ? tagsInput.value : "");
  if (tagsInput) tagsInput.setCustomValidity(tags === null ? t("throttle.tags_invalid") : "");
  return { read_preference: mode, read_preference_tags: mode && mode !== "primary" ? (tags || []) : [] };
}

function throttleNumber(id, parse) {
  const v = parse(String(getValue(id) || "").replace(",", "."));
  return Number.isFinite(v) && v > 0 ? v : 0;
}

// ---------------------------------------------------------------------------
// Job form
// ---------------------------------------------------------------------------

const THROTTLE_DAYS = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"];

// The weekday names of the current language, Monday first (2024-01-01 is a Monday).
function throttleDayNames() {
  const lang = typeof currentLang === "string" ? currentLang : "en";
  let fmt;
  try {
    fmt = new Intl.DateTimeFormat(lang, { weekday: "short", timeZone: "UTC" });
  } catch (_) {
    fmt = new Intl.DateTimeFormat("en", { weekday: "short", timeZone: "UTC" });
  }
  return THROTTLE_DAYS.map((_, i) => fmt.format(new Date(Date.UTC(2024, 0, 1 + i))));
}

function throttleRenderDays(checked) {
  const box = document.getElementById("job-window-days");
  if (!box) return;
  const keep = checked || THROTTLE_DAYS.filter(d => {
    const el = document.getElementById(`job-window-day-${d}`);
    return el && el.checked;
  });
  box.textContent = "";
  box.setAttribute("aria-label", t("throttle.window_days"));
  const names = throttleDayNames();
  THROTTLE_DAYS.forEach((d, i) => {
    const label = document.createElement("label");
    label.className = "check-inline";
    const input = document.createElement("input");
    input.type = "checkbox";
    input.id = `job-window-day-${d}`;
    input.value = d;
    input.checked = keep.includes(d);
    const span = document.createElement("span");
    span.textContent = names[i];
    label.append(input, span);
    box.appendChild(label);
  });
}

function throttleBrowserZone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch (_) {
    return "UTC";
  }
}

function throttleToggleWindow() {
  const on = document.getElementById("job-window-enabled");
  const fields = document.getElementById("job-window-fields");
  if (on && fields) fields.hidden = !on.checked;
}

// throttleFillJobForm sets the section from job (defaults for a new job) and opens
// it when the job uses any of its settings.
function throttleFillJobForm(job) {
  const j = job || {};
  setValue("job-read-pref", j.read_preference || "");
  setValue("job-read-pref-tags", throttleFormatTags(j.read_preference_tags));
  setValue("job-max-upload", j.max_upload_mbps > 0 ? String(j.max_upload_mbps) : "");
  setValue("job-num-parallel", j.num_parallel_collections > 0 ? String(j.num_parallel_collections) : "");
  const w = j.backup_window || null;
  const on = document.getElementById("job-window-enabled");
  if (on) on.checked = !!w;
  setValue("job-window-start", w ? w.start : "01:00");
  setValue("job-window-end", w ? (w.end === "24:00" ? "00:00" : w.end) : "05:00");
  setValue("job-window-tz", w ? (w.timezone || "UTC") : throttleBrowserZone());
  const cancel = document.getElementById("job-window-cancel");
  if (cancel) cancel.checked = !!(w && w.cancel_at_window_end);
  throttleRenderDays(w && w.days ? w.days : []);
  throttleToggleWindow();
  const section = document.getElementById("job-throttle");
  if (section) section.open = !!(j.read_preference || j.max_upload_mbps || j.num_parallel_collections || w);
}

// throttleJobPayload returns the section's job fields. A disabled window is sent
// as {} so an edit removes it.
function throttleJobPayload() {
  if (!document.getElementById("job-throttle")) return {};
  const out = {
    ...throttleReadPref("job-read-pref", "job-read-pref-tags"),
    max_upload_mbps: throttleNumber("job-max-upload", parseFloat),
    num_parallel_collections: Math.min(throttleNumber("job-num-parallel", s => parseInt(s, 10)), 16),
    backup_window: {}
  };
  const on = document.getElementById("job-window-enabled");
  const start = getValue("job-window-start");
  let end = getValue("job-window-end");
  const endInput = document.getElementById("job-window-end");
  if (on && on.checked) {
    // 00:00 as the end of a window that opens later the same day is midnight.
    if (end === "00:00" && start !== "00:00") end = "24:00";
    if (end === "00:00" && start === "00:00") end = "24:00";
    const valid = !!start && !!end && start !== end;
    if (endInput) endInput.setCustomValidity(valid ? "" : t("throttle.window_required"));
    out.backup_window = {
      start, end,
      timezone: String(getValue("job-window-tz") || "").trim() || "UTC",
      days: THROTTLE_DAYS.filter(d => {
        const el = document.getElementById(`job-window-day-${d}`);
        return el && el.checked;
      }),
      cancel_at_window_end: !!(document.getElementById("job-window-cancel") || {}).checked
    };
  } else if (endInput) {
    endInput.setCustomValidity("");
  }
  return out;
}

// throttleWindowText renders a window for messages: "Mon, Tue 22:00–02:00 (UTC)".
function throttleWindowText(w) {
  const names = throttleDayNames();
  const days = (w.days || []).length
    ? w.days.map(d => names[THROTTLE_DAYS.indexOf(d)] || d).join(", ")
    : t("throttle.every_day");
  return tf("throttle.window_summary", { days, start: w.start, end: w.end, tz: w.timezone || "UTC" });
}

// throttleConfirmManualRun warns that a manual run of a job with a backup window
// ignores it; it resolves to true to go on.
async function throttleConfirmManualRun(jobID) {
  const job = (state.jobs || []).find(j => j.id === jobID);
  if (!job || !job.backup_window || typeof confirmDialog !== "function") return true;
  return confirmDialog({
    title: t("throttle.manual_title"),
    body: tf("throttle.manual_body", { window: throttleWindowText(job.backup_window) }),
    confirmLabel: t("throttle.manual_confirm")
  });
}

// ---------------------------------------------------------------------------
// Connection form
// ---------------------------------------------------------------------------

function throttleFillConnectionForm(c) {
  setValue("connection-read-pref", (c && c.read_preference) || "");
  setValue("connection-read-pref-tags", throttleFormatTags(c && c.read_preference_tags));
  setValue("connection-max-concurrent", c && c.max_concurrent_backups > 0 ? String(c.max_concurrent_backups) : "");
}

// throttleConnectionReadPref returns the form's read preference (for the test).
function throttleConnectionReadPref() {
  if (!document.getElementById("connection-read-pref")) return {};
  return throttleReadPref("connection-read-pref", "connection-read-pref-tags");
}

function throttleConnectionPayload() {
  if (!document.getElementById("connection-read-pref")) return {};
  return {
    ...throttleConnectionReadPref(),
    max_concurrent_backups: Math.min(throttleNumber("connection-max-concurrent", s => parseInt(s, 10)), 64)
  };
}

// throttleDescribeMember is the part of a connection test result naming the
// member a backup reads from ("" when the server did not say).
function throttleDescribeMember(data) {
  if (!data) return "";
  if (data.read_member_error) return tf("throttle.member_failed", { error: truncate(data.read_member_error, 120) });
  const m = data.read_member;
  if (!m) return "";
  const member = m.host ? `${m.host} (${m.state})` : String(m.state || "");
  return data.read_preference
    ? tf("throttle.member_rp", { rp: data.read_preference, member })
    : tf("throttle.member", { member });
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

function setupThrottle() {
  const on = document.getElementById("job-window-enabled");
  if (on) on.addEventListener("change", throttleToggleWindow);
  ["job-read-pref-tags", "connection-read-pref-tags"].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.addEventListener("input", () => el.setCustomValidity(throttleParseTags(el.value) === null ? t("throttle.tags_invalid") : ""));
  });
  ["job-window-start", "job-window-end", "job-window-enabled"].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.addEventListener("change", () => throttleJobPayload());
  });
  throttleRenderDays([]);
  if (typeof onLanguageChange === "function") onLanguageChange(() => throttleRenderDays());
}

document.addEventListener("DOMContentLoaded", setupThrottle);
