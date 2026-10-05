/**
 * MongoRescue dashboard: the experimental "Point-in-time recovery" panel of the
 * Connections tab.
 *
 * It lists the PITR streams (GET /api/v1/pitr/streams): per connection the
 * collector's state, the newest point-in-time window, the lag, the oplog headroom
 * and the chain breaks, with actions to enable or disable a stream (PATCH, admin),
 * take a base backup now (POST .../base, operator) and delete a disabled stream
 * (DELETE, admin). The form below creates the stream of a connection (POST, admin).
 * Point-in-time restores come in a later release.
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
      desc: "Collects the oplog of a replica set connection into encrypted chunks and takes base backups of the whole instance, so it can later be restored to any moment within its window. Point-in-time restores come in a later release.",
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
      invalid: "Check the schedule, the interval (15-900 seconds) and the retention."
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
      reason_pitr_window_low: "PITR headroom low"
    }
  },
  tr: {
    pitr: {
      title: "Zamana noktasal kurtarma",
      experimental: "Deneysel",
      desc: "Bir replica set bağlantısının oplog'unu şifreli parçalara toplar ve tüm sunucunun temel yedeklerini alır; böylece ileride penceresi içindeki herhangi bir ana geri yüklenebilir. Zamana noktasal geri yükleme sonraki bir sürümde gelecek.",
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
      invalid: "Zamanlamayı, aralığı (15-900 saniye) ve saklama ayarlarını kontrol edin."
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
      reason_pitr_window_low: "PITR payı düşük"
    }
  },
  de: {
    pitr: {
      title: "Point-in-Time-Wiederherstellung",
      experimental: "Experimentell",
      desc: "Sammelt das Oplog einer Replica-Set-Verbindung in verschlüsselten Blöcken und erstellt Basis-Backups der ganzen Instanz, damit sie später zu jedem Zeitpunkt innerhalb ihres Fensters wiederhergestellt werden kann. Point-in-Time-Wiederherstellungen folgen in einer späteren Version.",
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
      invalid: "Prüfen Sie den Zeitplan, das Intervall (15-900 Sekunden) und die Aufbewahrung."
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
      reason_pitr_window_low: "PITR-Reserve gering"
    }
  },
  es: {
    pitr: {
      title: "Recuperación a un punto en el tiempo",
      experimental: "Experimental",
      desc: "Recoge el oplog de una conexión de replica set en fragmentos cifrados y toma copias base de toda la instancia, para poder restaurarla más adelante a cualquier momento dentro de su ventana. Las restauraciones a un punto en el tiempo llegarán en una versión posterior.",
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
      invalid: "Revisa la programación, el intervalo (15-900 segundos) y la retención."
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
      reason_pitr_window_low: "Margen PITR bajo"
    }
  },
  fr: {
    pitr: {
      title: "Restauration à un instant donné",
      experimental: "Expérimental",
      desc: "Collecte l'oplog d'une connexion replica set en blocs chiffrés et réalise des sauvegardes de base de toute l'instance, pour pouvoir la restaurer plus tard à n'importe quel instant de sa fenêtre. Les restaurations à un instant donné arriveront dans une version ultérieure.",
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
      invalid: "Vérifiez la planification, l'intervalle (15-900 secondes) et la rétention."
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
      reason_pitr_window_low: "Marge PITR faible"
    }
  },
  zh: {
    pitr: {
      title: "时间点恢复",
      experimental: "实验性",
      desc: "将副本集连接的 oplog 收集为加密的分块，并对整个实例进行基础备份，以便日后恢复到其窗口内的任意时刻。时间点恢复功能将在后续版本提供。",
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
      invalid: "请检查计划、间隔（15-900 秒）和保留设置。"
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
      reason_pitr_window_low: "PITR 余量不足"
    }
  },
  ja: {
    pitr: {
      title: "ポイントインタイムリカバリ",
      experimental: "実験的",
      desc: "レプリカセット接続の oplog を暗号化されたチャンクとして収集し、インスタンス全体のベースバックアップを取得します。これにより、後でウィンドウ内の任意の時点に復元できます。ポイントインタイム復元は今後のリリースで提供されます。",
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
      invalid: "スケジュール、間隔（15-900 秒）、保持設定を確認してください。"
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
      reason_pitr_window_low: "PITR 余裕不足"
    }
  },
  ru: {
    pitr: {
      title: "Восстановление на момент времени",
      experimental: "Экспериментально",
      desc: "Собирает oplog подключения к набору реплик в зашифрованные фрагменты и делает базовые резервные копии всего экземпляра, чтобы позже его можно было восстановить на любой момент внутри окна. Восстановление на момент времени появится в следующем выпуске.",
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
      invalid: "Проверьте расписание, интервал (15-900 секунд) и хранение."
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
      reason_pitr_window_low: "Мало запаса PITR"
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
    base_on_gap: !!(document.getElementById("pitr-base-on-gap") || {}).checked
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
// Wiring
// ---------------------------------------------------------------------------

function pitrSetup() {
  if (typeof ROLE_ACTION_SCOPES === "object") {
    Object.assign(ROLE_ACTION_SCOPES, { "pitr-toggle": "admin", "pitr-delete": "admin", "pitr-base": "operator" });
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
