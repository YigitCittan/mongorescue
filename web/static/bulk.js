/**
 * MongoRescue dashboard: row selection, bulk actions and confirmation dialogs.
 *
 * Checkbox selection on the Backups, Restores and Jobs tables (page, shift-click
 * ranges, "all matching the filters"), the sticky bulk toolbar, the bulk review /
 * progress / result dialog and confirmDialog(), the app's replacement for
 * window.confirm(). Loaded after app.js, whose helpers (t, tf, apiJSON, escapeHtml,
 * showToast, openModal, closeModal, ...) it uses; app.js calls bulkCell(), bulkSync(),
 * bulkForget(), bulkReset(), onModalClosed() and modalLocked(). The same security
 * invariants apply: server values reach innerHTML only through escapeHtml() (or are
 * set with textContent), kinds in URLs come from a fixed list, and actions go through
 * delegated listeners on data attributes.
 */

// ---------------------------------------------------------------------------
// Translations, merged into i18n.js's table (entries already there win)
// ---------------------------------------------------------------------------

const BULK_I18N = {
  en: {
    bulk: {
      select_row: "Select {name}",
      select_page: "Select all rows on this page",
      selected_n: "{n} selected",
      select_all_matching: "Select all {total} matching the filters",
      select_all_total: "Select all {total}",
      all_matching_selected: "All {total} matching the filters are selected.",
      all_selected: "All {total} are selected.",
      clear: "Clear selection",
      filters_changed: "The filters changed, so the selection was cleared.",
      toolbar: "Bulk actions",
      action_delete: "Delete",
      action_enable: "Enable",
      action_disable: "Pause",
      action_run_now: "Run now",
      action_verify: "Verify",
      action_pin: "Pin",
      action_unpin: "Unpin",
      action_cancel: "Cancel runs",
      checking: "Checking the selection…",
      matched: "Selected",
      actionable: "Will be changed",
      skipped: "Skipped",
      total_size: "Total size",
      skipped_heading: "Skipped, by reason",
      more: "…and {n} more",
      reason_not_found: "Not found (already deleted)",
      reason_in_progress: "Still running",
      reason_last_good_backup: "Last successful backup of its job (protected)",
      reason_already_enabled: "Already enabled",
      reason_already_disabled: "Already paused",
      reason_pinned: "Pinned (legal hold)",
      reason_last_verified: "Last verified backup of its job (protected)",
      reason_not_verifiable: "Not completed or no checksum",
      reason_already_pinned: "Already pinned",
      reason_not_pinned: "Not pinned",
      reason_not_running: "Not running",
      pin_note: "Note (optional)",
      reason_other: "Other",
      detail_job_last_good: "Last successful backup of job {job}",
      detail_job_last_verified: "Last verified backup of job {job}",
      detail_status: "Status: {status}",
      pin_title: "Pin backup",
      nothing_to_do: "Nothing to do: the action applies to none of the selected items.",
      no_undo_backups: "This cannot be undone: the backups and their archives are removed from storage for good.",
      no_undo_restores: "This cannot be undone: the restore history records are removed. Restored databases are not touched.",
      no_undo_jobs: "This cannot be undone: the jobs are removed. Their backups are kept.",
      type_count: "Type {n} to confirm.",
      type_count_label: "Number of items",
      confirm_n: "{action} ({n})",
      running: "Working… {done} of {total}",
      progress: "Progress",
      done: "Finished",
      summary: "{ok} succeeded · {skipped} skipped · {failed} failed",
      show_failed: "Show failed only",
      show_all: "Show all",
      failed_kept: "The failed items stay selected so you can try again.",
      no_failures: "No failures.",
      warning: "Warning",
      stopped: "Stopped: {error}"
    },
    dialog: {
      confirm_title: "Please confirm",
      delete_title: "Delete permanently?",
      confirm: "Confirm",
      type_text: "Type {text} to confirm.",
      cannot_undo: "This cannot be undone."
    }
  },
  tr: {
    bulk: {
      select_row: "{name} öğesini seç",
      select_page: "Bu sayfadaki tüm satırları seç",
      selected_n: "{n} seçildi",
      select_all_matching: "Filtreye uyan {total} kaydın tümünü seç",
      select_all_total: "{total} kaydın tümünü seç",
      all_matching_selected: "Filtreye uyan {total} kaydın tümü seçildi.",
      all_selected: "{total} kaydın tümü seçildi.",
      clear: "Seçimi temizle",
      filters_changed: "Filtreler değiştiği için seçim temizlendi.",
      toolbar: "Toplu işlemler",
      action_delete: "Sil",
      action_enable: "Etkinleştir",
      action_disable: "Duraklat",
      action_run_now: "Şimdi çalıştır",
      action_verify: "Doğrula",
      action_pin: "Sabitle",
      action_unpin: "Sabitlemeyi kaldır",
      action_cancel: "Çalışmaları iptal et",
      checking: "Seçim kontrol ediliyor…",
      matched: "Seçilen",
      actionable: "Değişecek",
      skipped: "Atlanan",
      total_size: "Toplam boyut",
      skipped_heading: "Atlananlar, nedene göre",
      more: "…ve {n} tane daha",
      reason_not_found: "Bulunamadı (zaten silinmiş)",
      reason_in_progress: "Hâlâ çalışıyor",
      reason_last_good_backup: "İşinin son başarılı yedeği (korunuyor)",
      reason_already_enabled: "Zaten etkin",
      reason_already_disabled: "Zaten duraklatılmış",
      reason_pinned: "Sabitlenmiş (yasal saklama)",
      reason_last_verified: "İşinin son doğrulanmış yedeği (korunuyor)",
      reason_not_verifiable: "Tamamlanmamış veya sağlama toplamı yok",
      reason_already_pinned: "Zaten sabitlenmiş",
      reason_not_pinned: "Sabitlenmemiş",
      reason_not_running: "Çalışmıyor",
      pin_note: "Not (isteğe bağlı)",
      reason_other: "Diğer",
      detail_job_last_good: "{job} işinin son başarılı yedeği",
      detail_job_last_verified: "{job} işinin son doğrulanmış yedeği",
      detail_status: "Durum: {status}",
      pin_title: "Yedeği sabitle",
      nothing_to_do: "Yapılacak bir şey yok: işlem seçilen öğelerin hiçbirine uygulanmıyor.",
      no_undo_backups: "Bu işlem geri alınamaz: yedekler ve arşivleri depolamadan kalıcı olarak silinir.",
      no_undo_restores: "Bu işlem geri alınamaz: geri yükleme geçmişi kayıtları silinir. Geri yüklenen veritabanlarına dokunulmaz.",
      no_undo_jobs: "Bu işlem geri alınamaz: işler silinir. Yedekleri korunur.",
      type_count: "Onaylamak için {n} yazın.",
      type_count_label: "Öğe sayısı",
      confirm_n: "{action} ({n})",
      running: "Çalışıyor… {done} / {total}",
      progress: "İlerleme",
      done: "Tamamlandı",
      summary: "{ok} başarılı · {skipped} atlandı · {failed} başarısız",
      show_failed: "Yalnızca başarısızları göster",
      show_all: "Tümünü göster",
      failed_kept: "Başarısız öğeler yeniden deneyebilmeniz için seçili kalır.",
      no_failures: "Başarısız öğe yok.",
      warning: "Uyarı",
      stopped: "Durduruldu: {error}"
    },
    dialog: {
      confirm_title: "Lütfen onaylayın",
      delete_title: "Kalıcı olarak silinsin mi?",
      confirm: "Onayla",
      type_text: "Onaylamak için {text} yazın.",
      cannot_undo: "Bu işlem geri alınamaz."
    }
  },
  de: {
    bulk: {
      select_row: "{name} auswählen",
      select_page: "Alle Zeilen dieser Seite auswählen",
      selected_n: "{n} ausgewählt",
      select_all_matching: "Alle {total} Treffer der Filter auswählen",
      select_all_total: "Alle {total} auswählen",
      all_matching_selected: "Alle {total} Treffer der Filter sind ausgewählt.",
      all_selected: "Alle {total} sind ausgewählt.",
      clear: "Auswahl aufheben",
      filters_changed: "Die Filter wurden geändert, daher wurde die Auswahl aufgehoben.",
      toolbar: "Sammelaktionen",
      action_delete: "Löschen",
      action_enable: "Aktivieren",
      action_disable: "Pausieren",
      action_run_now: "Jetzt ausführen",
      action_verify: "Prüfen",
      action_pin: "Anheften",
      action_unpin: "Lösen",
      action_cancel: "Läufe abbrechen",
      checking: "Auswahl wird geprüft…",
      matched: "Ausgewählt",
      actionable: "Wird geändert",
      skipped: "Übersprungen",
      total_size: "Gesamtgröße",
      skipped_heading: "Übersprungen, nach Grund",
      more: "…und {n} weitere",
      reason_not_found: "Nicht gefunden (bereits gelöscht)",
      reason_in_progress: "Läuft noch",
      reason_last_good_backup: "Letztes erfolgreiches Backup seines Jobs (geschützt)",
      reason_already_enabled: "Bereits aktiv",
      reason_already_disabled: "Bereits pausiert",
      reason_pinned: "Angeheftet (Legal Hold)",
      reason_last_verified: "Letztes geprüftes Backup seines Jobs (geschützt)",
      reason_not_verifiable: "Nicht abgeschlossen oder ohne Prüfsumme",
      reason_already_pinned: "Bereits angeheftet",
      reason_not_pinned: "Nicht angeheftet",
      reason_not_running: "Läuft nicht",
      pin_note: "Notiz (optional)",
      reason_other: "Sonstiges",
      detail_job_last_good: "Letztes erfolgreiches Backup des Jobs {job}",
      detail_job_last_verified: "Letztes geprüftes Backup des Jobs {job}",
      detail_status: "Status: {status}",
      pin_title: "Backup anheften",
      nothing_to_do: "Nichts zu tun: Die Aktion betrifft keines der ausgewählten Elemente.",
      no_undo_backups: "Dies kann nicht rückgängig gemacht werden: Die Backups und ihre Archive werden endgültig aus dem Speicher entfernt.",
      no_undo_restores: "Dies kann nicht rückgängig gemacht werden: Die Einträge des Wiederherstellungsverlaufs werden entfernt. Wiederhergestellte Datenbanken bleiben unberührt.",
      no_undo_jobs: "Dies kann nicht rückgängig gemacht werden: Die Jobs werden entfernt. Ihre Backups bleiben erhalten.",
      type_count: "Geben Sie zur Bestätigung {n} ein.",
      type_count_label: "Anzahl der Elemente",
      confirm_n: "{action} ({n})",
      running: "In Arbeit… {done} von {total}",
      progress: "Fortschritt",
      done: "Fertig",
      summary: "{ok} erfolgreich · {skipped} übersprungen · {failed} fehlgeschlagen",
      show_failed: "Nur fehlgeschlagene zeigen",
      show_all: "Alle zeigen",
      failed_kept: "Die fehlgeschlagenen Elemente bleiben ausgewählt, damit Sie es erneut versuchen können.",
      no_failures: "Keine Fehler.",
      warning: "Warnung",
      stopped: "Abgebrochen: {error}"
    },
    dialog: {
      confirm_title: "Bitte bestätigen",
      delete_title: "Endgültig löschen?",
      confirm: "Bestätigen",
      type_text: "Geben Sie zur Bestätigung {text} ein.",
      cannot_undo: "Dies kann nicht rückgängig gemacht werden."
    }
  },
  es: {
    bulk: {
      select_row: "Seleccionar {name}",
      select_page: "Seleccionar todas las filas de esta página",
      selected_n: "{n} seleccionados",
      select_all_matching: "Seleccionar los {total} que coinciden con los filtros",
      select_all_total: "Seleccionar los {total}",
      all_matching_selected: "Están seleccionados los {total} que coinciden con los filtros.",
      all_selected: "Están seleccionados los {total}.",
      clear: "Borrar selección",
      filters_changed: "Los filtros cambiaron, así que se borró la selección.",
      toolbar: "Acciones en lote",
      action_delete: "Eliminar",
      action_enable: "Activar",
      action_disable: "Pausar",
      action_run_now: "Ejecutar ahora",
      action_verify: "Verificar",
      action_pin: "Fijar",
      action_unpin: "Desfijar",
      action_cancel: "Cancelar ejecuciones",
      checking: "Comprobando la selección…",
      matched: "Seleccionados",
      actionable: "Se cambiarán",
      skipped: "Omitidos",
      total_size: "Tamaño total",
      skipped_heading: "Omitidos, por motivo",
      more: "…y {n} más",
      reason_not_found: "No encontrado (ya eliminado)",
      reason_in_progress: "Aún en ejecución",
      reason_last_good_backup: "Última copia correcta de su tarea (protegida)",
      reason_already_enabled: "Ya activa",
      reason_already_disabled: "Ya en pausa",
      reason_pinned: "Fijada (retención legal)",
      reason_last_verified: "Última copia verificada de su tarea (protegida)",
      reason_not_verifiable: "No completada o sin suma de comprobación",
      reason_already_pinned: "Ya fijada",
      reason_not_pinned: "No fijada",
      reason_not_running: "No está en ejecución",
      pin_note: "Nota (opcional)",
      reason_other: "Otros",
      detail_job_last_good: "Última copia correcta de la tarea {job}",
      detail_job_last_verified: "Última copia verificada de la tarea {job}",
      detail_status: "Estado: {status}",
      pin_title: "Fijar copia",
      nothing_to_do: "Nada que hacer: la acción no se aplica a ninguno de los elementos seleccionados.",
      no_undo_backups: "Esto no se puede deshacer: las copias y sus archivos se eliminan del almacenamiento para siempre.",
      no_undo_restores: "Esto no se puede deshacer: se eliminan los registros del historial de restauraciones. Las bases de datos restauradas no se tocan.",
      no_undo_jobs: "Esto no se puede deshacer: se eliminan las tareas. Sus copias se conservan.",
      type_count: "Escriba {n} para confirmar.",
      type_count_label: "Número de elementos",
      confirm_n: "{action} ({n})",
      running: "Procesando… {done} de {total}",
      progress: "Progreso",
      done: "Terminado",
      summary: "{ok} correctos · {skipped} omitidos · {failed} fallidos",
      show_failed: "Mostrar solo los fallidos",
      show_all: "Mostrar todos",
      failed_kept: "Los elementos fallidos siguen seleccionados para que pueda reintentarlo.",
      no_failures: "Sin fallos.",
      warning: "Aviso",
      stopped: "Detenido: {error}"
    },
    dialog: {
      confirm_title: "Confirme, por favor",
      delete_title: "¿Eliminar definitivamente?",
      confirm: "Confirmar",
      type_text: "Escriba {text} para confirmar.",
      cannot_undo: "Esto no se puede deshacer."
    }
  },
  fr: {
    bulk: {
      select_row: "Sélectionner {name}",
      select_page: "Sélectionner toutes les lignes de cette page",
      selected_n: "{n} sélectionné(s)",
      select_all_matching: "Sélectionner les {total} résultats des filtres",
      select_all_total: "Sélectionner les {total}",
      all_matching_selected: "Les {total} résultats des filtres sont sélectionnés.",
      all_selected: "Les {total} sont sélectionnés.",
      clear: "Effacer la sélection",
      filters_changed: "Les filtres ont changé : la sélection a été effacée.",
      toolbar: "Actions groupées",
      action_delete: "Supprimer",
      action_enable: "Activer",
      action_disable: "Suspendre",
      action_run_now: "Exécuter maintenant",
      action_verify: "Vérifier",
      action_pin: "Épingler",
      action_unpin: "Désépingler",
      action_cancel: "Annuler les exécutions",
      checking: "Vérification de la sélection…",
      matched: "Sélectionnés",
      actionable: "Seront modifiés",
      skipped: "Ignorés",
      total_size: "Taille totale",
      skipped_heading: "Ignorés, par motif",
      more: "…et {n} de plus",
      reason_not_found: "Introuvable (déjà supprimé)",
      reason_in_progress: "Toujours en cours",
      reason_last_good_backup: "Dernière sauvegarde réussie de sa tâche (protégée)",
      reason_already_enabled: "Déjà active",
      reason_already_disabled: "Déjà suspendue",
      reason_pinned: "Épinglée (conservation légale)",
      reason_last_verified: "Dernière sauvegarde vérifiée de sa tâche (protégée)",
      reason_not_verifiable: "Non terminée ou sans somme de contrôle",
      reason_already_pinned: "Déjà épinglée",
      reason_not_pinned: "Non épinglée",
      reason_not_running: "Pas en cours",
      pin_note: "Note (facultative)",
      reason_other: "Autre",
      detail_job_last_good: "Dernière sauvegarde réussie de la tâche {job}",
      detail_job_last_verified: "Dernière sauvegarde vérifiée de la tâche {job}",
      detail_status: "Statut : {status}",
      pin_title: "Épingler la sauvegarde",
      nothing_to_do: "Rien à faire : l’action ne s’applique à aucun des éléments sélectionnés.",
      no_undo_backups: "Action irréversible : les sauvegardes et leurs archives sont définitivement supprimées du stockage.",
      no_undo_restores: "Action irréversible : les entrées de l’historique des restaurations sont supprimées. Les bases restaurées ne sont pas touchées.",
      no_undo_jobs: "Action irréversible : les tâches sont supprimées. Leurs sauvegardes sont conservées.",
      type_count: "Saisissez {n} pour confirmer.",
      type_count_label: "Nombre d’éléments",
      confirm_n: "{action} ({n})",
      running: "En cours… {done} sur {total}",
      progress: "Progression",
      done: "Terminé",
      summary: "{ok} réussi(s) · {skipped} ignoré(s) · {failed} en échec",
      show_failed: "Afficher seulement les échecs",
      show_all: "Tout afficher",
      failed_kept: "Les éléments en échec restent sélectionnés pour réessayer.",
      no_failures: "Aucun échec.",
      warning: "Avertissement",
      stopped: "Interrompu : {error}"
    },
    dialog: {
      confirm_title: "Veuillez confirmer",
      delete_title: "Supprimer définitivement ?",
      confirm: "Confirmer",
      type_text: "Saisissez {text} pour confirmer.",
      cannot_undo: "Action irréversible."
    }
  },
  zh: {
    bulk: {
      select_row: "选择 {name}",
      select_page: "选择本页所有行",
      selected_n: "已选择 {n} 项",
      select_all_matching: "选择符合筛选条件的全部 {total} 项",
      select_all_total: "选择全部 {total} 项",
      all_matching_selected: "已选择符合筛选条件的全部 {total} 项。",
      all_selected: "已选择全部 {total} 项。",
      clear: "清除选择",
      filters_changed: "筛选条件已更改，选择已清除。",
      toolbar: "批量操作",
      action_delete: "删除",
      action_enable: "启用",
      action_disable: "暂停",
      action_run_now: "立即运行",
      action_verify: "校验",
      action_pin: "固定",
      action_unpin: "取消固定",
      action_cancel: "取消运行",
      checking: "正在检查所选内容…",
      matched: "已选择",
      actionable: "将被更改",
      skipped: "已跳过",
      total_size: "总大小",
      skipped_heading: "已跳过（按原因）",
      more: "…还有 {n} 项",
      reason_not_found: "未找到（已删除）",
      reason_in_progress: "仍在运行",
      reason_last_good_backup: "其任务的最后一个成功备份（受保护）",
      reason_already_enabled: "已启用",
      reason_already_disabled: "已暂停",
      reason_pinned: "已固定（法律保留）",
      reason_last_verified: "其任务最后一个已校验的备份（受保护）",
      reason_not_verifiable: "未完成或没有校验和",
      reason_already_pinned: "已固定",
      reason_not_pinned: "未固定",
      reason_not_running: "未在运行",
      pin_note: "备注（可选）",
      reason_other: "其他",
      detail_job_last_good: "任务 {job} 的最后一个成功备份",
      detail_job_last_verified: "任务 {job} 的最后一个已校验备份",
      detail_status: "状态：{status}",
      pin_title: "固定备份",
      nothing_to_do: "无需操作：该操作不适用于任何所选项目。",
      no_undo_backups: "此操作无法撤销：备份及其归档将从存储中永久删除。",
      no_undo_restores: "此操作无法撤销：将删除恢复历史记录。已恢复的数据库不受影响。",
      no_undo_jobs: "此操作无法撤销：将删除这些任务。其备份会保留。",
      type_count: "输入 {n} 以确认。",
      type_count_label: "项目数量",
      confirm_n: "{action}（{n}）",
      running: "处理中… {done} / {total}",
      progress: "进度",
      done: "已完成",
      summary: "成功 {ok} · 跳过 {skipped} · 失败 {failed}",
      show_failed: "仅显示失败项",
      show_all: "显示全部",
      failed_kept: "失败的项目保持选中，以便重试。",
      no_failures: "没有失败项。",
      warning: "警告",
      stopped: "已停止：{error}"
    },
    dialog: {
      confirm_title: "请确认",
      delete_title: "永久删除？",
      confirm: "确认",
      type_text: "输入 {text} 以确认。",
      cannot_undo: "此操作无法撤销。"
    }
  },
  ja: {
    bulk: {
      select_row: "{name} を選択",
      select_page: "このページのすべての行を選択",
      selected_n: "{n} 件選択中",
      select_all_matching: "フィルターに一致する {total} 件をすべて選択",
      select_all_total: "{total} 件をすべて選択",
      all_matching_selected: "フィルターに一致する {total} 件がすべて選択されています。",
      all_selected: "{total} 件がすべて選択されています。",
      clear: "選択を解除",
      filters_changed: "フィルターが変更されたため、選択を解除しました。",
      toolbar: "一括操作",
      action_delete: "削除",
      action_enable: "有効化",
      action_disable: "一時停止",
      action_run_now: "今すぐ実行",
      action_verify: "検証",
      action_pin: "固定",
      action_unpin: "固定を解除",
      action_cancel: "実行をキャンセル",
      checking: "選択内容を確認しています…",
      matched: "選択",
      actionable: "変更対象",
      skipped: "スキップ",
      total_size: "合計サイズ",
      skipped_heading: "スキップ（理由別）",
      more: "…ほか {n} 件",
      reason_not_found: "見つかりません（削除済み）",
      reason_in_progress: "実行中",
      reason_last_good_backup: "ジョブの最後の成功バックアップ（保護）",
      reason_already_enabled: "すでに有効",
      reason_already_disabled: "すでに一時停止中",
      reason_pinned: "固定済み（リーガルホールド）",
      reason_last_verified: "ジョブの最後の検証済みバックアップ（保護）",
      reason_not_verifiable: "未完了またはチェックサムなし",
      reason_already_pinned: "すでに固定済み",
      reason_not_pinned: "固定されていません",
      reason_not_running: "実行中ではありません",
      pin_note: "メモ（任意）",
      reason_other: "その他",
      detail_job_last_good: "ジョブ {job} の最後の成功バックアップ",
      detail_job_last_verified: "ジョブ {job} の最後の検証済みバックアップ",
      detail_status: "状態: {status}",
      pin_title: "バックアップを固定",
      nothing_to_do: "対象がありません。選択した項目のいずれにもこの操作は適用されません。",
      no_undo_backups: "元に戻せません。バックアップとそのアーカイブはストレージから完全に削除されます。",
      no_undo_restores: "元に戻せません。リストア履歴のレコードが削除されます。リストア済みのデータベースには影響しません。",
      no_undo_jobs: "元に戻せません。ジョブが削除されます。バックアップは残ります。",
      type_count: "確認のため {n} と入力してください。",
      type_count_label: "項目数",
      confirm_n: "{action}（{n}）",
      running: "処理中… {done} / {total}",
      progress: "進捗",
      done: "完了",
      summary: "成功 {ok} · スキップ {skipped} · 失敗 {failed}",
      show_failed: "失敗のみ表示",
      show_all: "すべて表示",
      failed_kept: "失敗した項目は再試行できるよう選択されたままです。",
      no_failures: "失敗はありません。",
      warning: "警告",
      stopped: "停止しました: {error}"
    },
    dialog: {
      confirm_title: "確認してください",
      delete_title: "完全に削除しますか？",
      confirm: "確認",
      type_text: "確認のため {text} と入力してください。",
      cannot_undo: "この操作は元に戻せません。"
    }
  },
  ru: {
    bulk: {
      select_row: "Выбрать {name}",
      select_page: "Выбрать все строки на этой странице",
      selected_n: "Выбрано: {n}",
      select_all_matching: "Выбрать все {total}, подходящие под фильтры",
      select_all_total: "Выбрать все {total}",
      all_matching_selected: "Выбраны все {total}, подходящие под фильтры.",
      all_selected: "Выбраны все {total}.",
      clear: "Снять выделение",
      filters_changed: "Фильтры изменились, поэтому выделение снято.",
      toolbar: "Массовые действия",
      action_delete: "Удалить",
      action_enable: "Включить",
      action_disable: "Приостановить",
      action_run_now: "Запустить сейчас",
      action_verify: "Проверить",
      action_pin: "Закрепить",
      action_unpin: "Открепить",
      action_cancel: "Отменить запуски",
      checking: "Проверка выделения…",
      matched: "Выбрано",
      actionable: "Будет изменено",
      skipped: "Пропущено",
      total_size: "Общий размер",
      skipped_heading: "Пропущено, по причинам",
      more: "…и ещё {n}",
      reason_not_found: "Не найдено (уже удалено)",
      reason_in_progress: "Ещё выполняется",
      reason_last_good_backup: "Последняя успешная копия своего задания (защищена)",
      reason_already_enabled: "Уже включено",
      reason_already_disabled: "Уже приостановлено",
      reason_pinned: "Закреплено (юридическое удержание)",
      reason_last_verified: "Последняя проверенная копия своего задания (защищена)",
      reason_not_verifiable: "Не завершена или нет контрольной суммы",
      reason_already_pinned: "Уже закреплена",
      reason_not_pinned: "Не закреплена",
      reason_not_running: "Не выполняется",
      pin_note: "Заметка (необязательно)",
      reason_other: "Другое",
      detail_job_last_good: "Последняя успешная копия задания {job}",
      detail_job_last_verified: "Последняя проверенная копия задания {job}",
      detail_status: "Статус: {status}",
      pin_title: "Закрепить копию",
      nothing_to_do: "Нечего делать: действие не применимо ни к одному из выбранных элементов.",
      no_undo_backups: "Это нельзя отменить: резервные копии и их архивы будут безвозвратно удалены из хранилища.",
      no_undo_restores: "Это нельзя отменить: записи истории восстановлений будут удалены. Восстановленные базы данных не затрагиваются.",
      no_undo_jobs: "Это нельзя отменить: задания будут удалены. Их резервные копии сохранятся.",
      type_count: "Введите {n} для подтверждения.",
      type_count_label: "Количество элементов",
      confirm_n: "{action} ({n})",
      running: "Выполняется… {done} из {total}",
      progress: "Ход выполнения",
      done: "Готово",
      summary: "Успешно: {ok} · пропущено: {skipped} · с ошибкой: {failed}",
      show_failed: "Только с ошибкой",
      show_all: "Показать все",
      failed_kept: "Элементы с ошибкой остаются выбранными, чтобы повторить попытку.",
      no_failures: "Ошибок нет.",
      warning: "Предупреждение",
      stopped: "Остановлено: {error}"
    },
    dialog: {
      confirm_title: "Подтвердите действие",
      delete_title: "Удалить безвозвратно?",
      confirm: "Подтвердить",
      type_text: "Введите {text} для подтверждения.",
      cannot_undo: "Это действие нельзя отменить."
    }
  }
};

(function mergeBulkTranslations() {
  if (typeof translations !== "object" || !translations) return;
  Object.keys(BULK_I18N).forEach(lang => {
    if (!translations[lang]) translations[lang] = {};
    const target = translations[lang];
    Object.keys(BULK_I18N[lang]).forEach(ns => {
      target[ns] = Object.assign({}, BULK_I18N[lang][ns], target[ns] || {});
    });
  });
})();

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const BULK_KINDS = ["backups", "restores", "jobs"];
// Items sent per request while a bulk action runs (drives the progress bar).
const BULK_CHUNK = 25;
// Destructive actions on more items than this ask to type the count.
const BULK_TYPE_THRESHOLD = 10;
// IDs listed per skip reason and result lines rendered in the dialog.
const BULK_LIST_MAX = 200;
const BULK_RESULTS_MAX = 1000;
// Wait before asking for the action list again after a failure.
const BULK_RETRY_MS = 30000;
// Toolbar order of known actions; destructive ones always come last.
const BULK_ACTION_ORDER = ["run_now", "enable", "disable", "verify", "pin", "unpin", "cancel", "delete"];
// Skip reasons with a translated per-item line, filled from the server's params.
const BULK_DETAIL_KEYS = {
  last_good_backup: "bulk.detail_job_last_good",
  last_verified: "bulk.detail_job_last_verified",
  in_progress: "bulk.detail_status",
  not_running: "bulk.detail_status",
  already_deleted: "bulk.detail_status"
};
const BULK_SKIP_REASONS = ["not_found", "in_progress", "last_good_backup", "last_verified", "pinned", "not_verifiable",
  "already_pinned", "not_pinned", "not_running", "already_enabled", "already_disabled", "already_deleted"];

const bulkState = {
  // Available actions from GET /api/v1/bulk/actions (null until loaded).
  actions: null,
  loading: false,
  failedAt: 0,
  // Selection per kind: "ids" keeps explicit IDs across pages; "all" means every
  // record matching the filters (the filters are sent, not the IDs). key is the
  // filter state the selection was made under.
  sel: {}
};
BULK_KINDS.forEach(kind => {
  bulkState.sel[kind] = { mode: "ids", ids: new Set(), key: null, anchor: "" };
});

// The open bulk dialog.
const bulkRun = { kind: "", action: null, request: null, dry: null, running: false, results: null, failedOnly: false, seq: 0 };

// bulkKind returns kind when it is one of BULK_KINDS (a literal), and "" otherwise.
function bulkKind(value) {
  switch (value) {
    case "backups":
      return "backups";
    case "restores":
      return "restores";
    case "jobs":
      return "jobs";
    default:
      return "";
  }
}

// ---------------------------------------------------------------------------
// Hooks called by app.js
// ---------------------------------------------------------------------------

// Checkbox cell of a table row. label names the row for screen readers.
function bulkCell(kind, id, label) {
  const aria = escapeHtml(tf("bulk.select_row", { name: label || id }));
  return `<td class="col-select"><label class="bulk-hit"><input type="checkbox" class="bulk-check" data-bulk-kind="${escapeHtml(kind)}" data-id="${escapeHtml(id)}" aria-label="${aria}"></label></td>`;
}

// Called after every render of a table body: applies the selection to the rows,
// clears it when the filters changed, and updates the toolbar.
function bulkSync(tbodyId) {
  const kind = BULK_KINDS.find(k => `${k}-tbody` === tbodyId) || "";
  if (!kind) return;
  ensureBulkActions();
  const sel = bulkState.sel[kind];
  const key = bulkFilterKey(kind);
  if (sel.key !== null && key !== sel.key && bulkCount(kind) > 0) {
    clearBulkSelection(kind, false);
    showToast(t("bulk.filters_changed"), "info");
  }
  sel.key = key;
  renderBulkUI(kind);
}

// Drops id from the selection (it was deleted by a single-item action).
function bulkForget(kind, id) {
  const sel = bulkState.sel[bulkKind(kind)];
  if (sel && sel.ids.delete(id)) renderBulkUI(kind);
}

// Signing out forgets the selections and the action list.
function bulkReset() {
  BULK_KINDS.forEach(kind => clearBulkSelection(kind, false));
  bulkState.actions = null;
  bulkState.failedAt = 0;
}

// closeModal hooks: a running bulk action keeps its dialog open; closing the confirm
// dialog settles its promise.
function modalLocked(id) {
  return id === "modal-bulk" && bulkRun.running;
}

function onModalClosed(id) {
  if (id === "modal-confirm") settleConfirm();
  if (id === "modal-bulk") {
    bulkRun.seq++;
    bulkRun.kind = "";
  }
}

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

function ensureBulkActions() {
  if (bulkState.actions || bulkState.loading || !auth.user) return;
  if (bulkState.failedAt && Date.now() - bulkState.failedAt < BULK_RETRY_MS) return;
  bulkState.loading = true;
  apiJSON("/api/v1/bulk/actions")
    .then(json => {
      if (json.success && Array.isArray(json.data)) {
        bulkState.actions = json.data;
        bulkState.failedAt = 0;
        BULK_KINDS.forEach(renderBulkUI);
      } else {
        bulkState.failedAt = Date.now();
      }
    })
    .catch(() => { bulkState.failedAt = Date.now(); })
    .finally(() => { bulkState.loading = false; });
}

// Actions of kind the signed-in user may run, in toolbar order.
function bulkActionsFor(kind) {
  const list = (bulkState.actions || []).filter(a => a && a.resource === kind && a.allowed);
  const rank = a => {
    const i = BULK_ACTION_ORDER.indexOf(a.name);
    return (a.destructive ? 100 : 0) + (i >= 0 ? i : 50);
  };
  return list.slice().sort((a, b) => rank(a) - rank(b));
}

function bulkActionLabel(name) {
  return t(`bulk.action_${name}`, String(name || ""));
}

// The filter state a selection is made under (the Jobs filters live in tables.js).
function bulkFilterKey(kind) {
  if (kind === "jobs") return typeof tablesFilterKey === "function" ? tablesFilterKey("jobs") : "";
  return typeof lists === "object" && lists[kind] ? JSON.stringify(lists[kind].filters) : "";
}

// Records matching the current filters.
function bulkTotal(kind) {
  if (kind === "jobs") return typeof tablesTotal === "function" ? tablesTotal("jobs") : state.jobs.length;
  return lists[kind] ? lists[kind].total : 0;
}

// Whether kind's list is narrowed by filters.
function bulkFiltered(kind) {
  if (kind === "jobs") return typeof tablesActiveCount === "function" && tablesActiveCount("jobs") > 0;
  return typeof activeFilterCount === "function" && activeFilterCount(kind) > 0;
}

function bulkCount(kind) {
  const sel = bulkState.sel[kind];
  return sel.mode === "all" ? bulkTotal(kind) : sel.ids.size;
}

function pageBoxes(kind) {
  const tbody = document.getElementById(`${kind}-tbody`);
  return tbody ? Array.from(tbody.querySelectorAll("input.bulk-check")) : [];
}

function clearBulkSelection(kind, focus) {
  const sel = bulkState.sel[kind];
  sel.mode = "ids";
  sel.ids.clear();
  sel.anchor = "";
  renderBulkUI(kind);
  if (focus) {
    const all = document.querySelector(`input.bulk-check-all[data-bulk-kind="${kind}"]`);
    if (all && all.offsetParent !== null) all.focus();
  }
}

// Leaving "all matching" for explicit IDs keeps the rows of this page.
function bulkToIds(kind) {
  const sel = bulkState.sel[kind];
  if (sel.mode !== "all") return;
  sel.mode = "ids";
  sel.ids = new Set(pageBoxes(kind).map(b => b.dataset.id || ""));
}

function onRowCheck(box, shift) {
  const kind = bulkKind(box.dataset.bulkKind);
  if (!kind) return;
  const sel = bulkState.sel[kind];
  const id = box.dataset.id || "";
  const on = box.checked;
  bulkToIds(kind);
  const boxes = pageBoxes(kind);
  const from = shift && sel.anchor ? boxes.findIndex(b => b.dataset.id === sel.anchor) : -1;
  const to = boxes.indexOf(box);
  if (from >= 0 && to >= 0) {
    const [a, b] = from < to ? [from, to] : [to, from];
    boxes.slice(a, b + 1).forEach(b2 => {
      if (on) sel.ids.add(b2.dataset.id || "");
      else sel.ids.delete(b2.dataset.id || "");
    });
  } else if (on) {
    sel.ids.add(id);
  } else {
    sel.ids.delete(id);
  }
  sel.anchor = id;
  if (sel.key === null) sel.key = bulkFilterKey(kind);
  renderBulkUI(kind);
}

function onPageCheck(all) {
  const kind = bulkKind(all.dataset.bulkKind);
  if (!kind) return;
  const sel = bulkState.sel[kind];
  const boxes = pageBoxes(kind);
  if (sel.mode === "all") {
    clearBulkSelection(kind, false);
    return;
  }
  const ids = boxes.map(b => b.dataset.id || "");
  const allOn = ids.length > 0 && ids.every(id => sel.ids.has(id));
  ids.forEach(id => {
    if (allOn) sel.ids.delete(id);
    else sel.ids.add(id);
  });
  if (sel.key === null) sel.key = bulkFilterKey(kind);
  renderBulkUI(kind);
}

function selectAllMatching(kind) {
  const sel = bulkState.sel[kind];
  sel.mode = "all";
  sel.ids.clear();
  sel.key = bulkFilterKey(kind);
  renderBulkUI(kind);
  const count = document.getElementById(`bulk-count-${kind}`);
  if (count) count.focus();
}

// ---------------------------------------------------------------------------
// Rendering: checkboxes, header checkbox, toolbar
// ---------------------------------------------------------------------------

// Toolbar shown above a table while rows are selected.
function bulkToolbar(kind) {
  let bar = document.getElementById(`bulk-bar-${kind}`);
  if (bar) return bar;
  const pane = document.getElementById(`tab-${kind}`);
  const wrap = pane ? pane.querySelector(".table-wrap") : null;
  if (!wrap) return null;
  bar = document.createElement("div");
  bar.id = `bulk-bar-${kind}`;
  bar.className = "bulk-bar";
  bar.setAttribute("role", "region");
  bar.setAttribute("aria-label", t("bulk.toolbar"));
  bar.hidden = true;
  wrap.parentNode.insertBefore(bar, wrap);
  return bar;
}

function renderBulkUI(kind) {
  kind = bulkKind(kind);
  if (!kind) return;
  const sel = bulkState.sel[kind];
  const actions = bulkActionsFor(kind);
  const enabled = actions.length > 0;
  const tbody = document.getElementById(`${kind}-tbody`);
  const table = tbody ? tbody.closest("table") : null;
  if (table) table.classList.toggle("bulk-on", enabled);
  if (!enabled && bulkCount(kind) > 0) {
    sel.mode = "ids";
    sel.ids.clear();
  }

  const boxes = pageBoxes(kind);
  let onPage = 0;
  boxes.forEach(b => {
    const on = sel.mode === "all" || sel.ids.has(b.dataset.id || "");
    b.checked = on;
    const row = b.closest("tr");
    if (row) row.classList.toggle("row-selected", on);
    if (on) onPage++;
  });
  const all = document.querySelector(`input.bulk-check-all[data-bulk-kind="${kind}"]`);
  if (all) {
    all.disabled = boxes.length === 0;
    all.checked = boxes.length > 0 && onPage === boxes.length;
    all.indeterminate = onPage > 0 && onPage < boxes.length;
  }

  const bar = bulkToolbar(kind);
  if (!bar) return;
  const count = bulkCount(kind);
  bar.hidden = !enabled || count === 0;
  if (bar.hidden) {
    bar.textContent = "";
    bar.dataset.html = "";
    return;
  }
  const total = bulkTotal(kind);
  const filtered = bulkFiltered(kind);
  let banner = "";
  if (sel.mode === "all") {
    banner = `<span class="bulk-banner">${escapeHtml(tf(filtered ? "bulk.all_matching_selected" : "bulk.all_selected", { total: formatCount(total) }))}</span>`;
  } else if (boxes.length > 0 && onPage === boxes.length && total > boxes.length) {
    const label = tf(filtered ? "bulk.select_all_matching" : "bulk.select_all_total", { total: formatCount(total) });
    banner = `<span class="bulk-sep" aria-hidden="true">·</span><button type="button" class="link-btn" data-bulk="select-all" data-bulk-kind="${kind}">${escapeHtml(label)}</button>`;
  }
  const buttons = actions.map(a => {
    const cls = a.destructive ? "btn btn-danger btn-sm" : "btn btn-secondary btn-sm";
    return `<button type="button" class="${cls}" data-bulk="action" data-bulk-kind="${kind}" data-bulk-action="${escapeHtml(a.name)}">${escapeHtml(bulkActionLabel(a.name))}</button>`;
  }).join("");
  const html = `<div class="bulk-info"><span class="bulk-count" id="bulk-count-${kind}" tabindex="-1" aria-live="polite">${escapeHtml(tf("bulk.selected_n", { n: formatCount(count) }))}</span>${banner}</div>
    <div class="bulk-actions">${buttons}<button type="button" class="btn btn-ghost btn-sm" data-bulk="clear" data-bulk-kind="${kind}">${escapeHtml(t("bulk.clear"))}</button></div>`;
  if (bar.dataset.html !== html) {
    const focused = bar.contains(document.activeElement) ? document.activeElement.dataset.bulk || "" : "";
    bar.innerHTML = html;
    bar.dataset.html = html;
    bar.setAttribute("aria-label", t("bulk.toolbar"));
    if (focused) {
      const again = bar.querySelector(`[data-bulk="${focused}"]`) || document.getElementById(`bulk-count-${kind}`);
      if (again) again.focus();
    }
  }
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

document.addEventListener("click", (e) => {
  const el = e.target;
  if (!(el instanceof Element)) return;
  if (el.matches("input.bulk-check")) {
    onRowCheck(el, e.shiftKey);
    return;
  }
  if (el.matches("input.bulk-check-all")) {
    onPageCheck(el);
    return;
  }
  const btn = el.closest("[data-bulk]");
  if (!btn || btn.disabled) return;
  const kind = bulkKind(btn.dataset.bulkKind);
  switch (btn.dataset.bulk) {
    case "select-all":
      if (kind) selectAllMatching(kind);
      break;
    case "clear":
      if (kind) clearBulkSelection(kind, true);
      break;
    case "action":
      if (kind) openBulkAction(kind, btn.dataset.bulkAction || "");
      break;
    case "bulk-confirm":
      executeBulk();
      break;
    case "bulk-failed-only":
      bulkRun.failedOnly = !bulkRun.failedOnly;
      renderBulkResults();
      break;
    case "confirm-ok":
      confirmState.result = true;
      closeModal("modal-confirm");
      break;
    case "confirm-cancel":
      closeModal("modal-confirm");
      break;
  }
});

// Escape clears the selection of the visible table (dialogs and menus first).
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape" || e.defaultPrevented) return;
  if ((typeof modalStack !== "undefined" && modalStack.length > 0) || (typeof rowMenu !== "undefined" && rowMenu.trigger)) return;
  const target = e.target;
  if (target instanceof HTMLTextAreaElement || target instanceof HTMLSelectElement ||
      (target instanceof HTMLInputElement && target.type !== "checkbox")) return;
  const kind = bulkKind(String(activeTabId()).replace(/^tab-/, ""));
  if (!kind || bulkCount(kind) === 0) return;
  e.preventDefault();
  clearBulkSelection(kind, true);
});

// The type-to-confirm fields enable their button as they are filled in.
document.addEventListener("input", (e) => {
  const el = e.target;
  if (!(el instanceof HTMLInputElement)) return;
  if (el.id === "bulk-type") updateBulkConfirm();
  if (el.id === "confirm-input") updateConfirmDialog();
});

document.addEventListener("keydown", (e) => {
  const el = e.target;
  if (e.key !== "Enter" || !(el instanceof HTMLInputElement)) return;
  if (el.id === "bulk-type" || el.id === "confirm-input") {
    e.preventDefault();
    const btn = document.getElementById(el.id === "bulk-type" ? "bulk-confirm" : "confirm-ok");
    if (btn && !btn.disabled) btn.click();
  }
});

// ---------------------------------------------------------------------------
// Dialog markup (added once; app.js's setupModals wires Escape and the backdrop)
// ---------------------------------------------------------------------------

const BULK_CLOSE_ICON = '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M4 4l8 8M12 4l-8 8"/></svg>';

(function addBulkDialogs() {
  if (!document.body || document.getElementById("modal-bulk")) return;
  const holder = document.createElement("div");
  holder.innerHTML = `
  <div id="modal-bulk" class="modal-backdrop" aria-hidden="true">
    <div class="modal-card" role="dialog" aria-modal="true" aria-labelledby="bulk-title" aria-describedby="bulk-status">
      <div class="modal-header">
        <h3 class="modal-title" id="bulk-title"></h3>
        <button type="button" class="modal-close" id="bulk-close" data-action="close-modal" data-target="modal-bulk" aria-label="Close" data-i18n-aria-label="actions.close">${BULK_CLOSE_ICON}</button>
      </div>
      <div class="modal-body scrollable" id="bulk-body"></div>
      <div class="modal-footer" id="bulk-footer"></div>
    </div>
  </div>
  <div id="modal-confirm" class="modal-backdrop" aria-hidden="true">
    <div class="modal-card modal-sm" role="alertdialog" aria-modal="true" aria-labelledby="confirm-title" aria-describedby="confirm-body">
      <div class="modal-header">
        <h3 class="modal-title" id="confirm-title"></h3>
        <button type="button" class="modal-close" data-action="close-modal" data-target="modal-confirm" aria-label="Close" data-i18n-aria-label="actions.close">${BULK_CLOSE_ICON}</button>
      </div>
      <div class="modal-body">
        <div class="confirm-body" id="confirm-body"></div>
        <div class="form-group" id="confirm-require" hidden>
          <label class="form-label" for="confirm-input" id="confirm-require-label"></label>
          <input type="text" class="form-input" id="confirm-input" autocomplete="off" autocapitalize="none" spellcheck="false">
        </div>
      </div>
      <div class="modal-footer">
        <button type="button" class="btn btn-secondary" id="confirm-cancel" data-bulk="confirm-cancel"></button>
        <button type="button" class="btn btn-primary" id="confirm-ok" data-bulk="confirm-ok"></button>
      </div>
    </div>
  </div>`;
  while (holder.firstElementChild) document.body.appendChild(holder.firstElementChild);
})();

// ---------------------------------------------------------------------------
// confirmDialog: the app's own confirmation (replaces window.confirm)
// ---------------------------------------------------------------------------

const confirmState = { resolve: null, result: false, require: "", prompt: false };

// confirmDialog({title, body, danger, confirmLabel, requireText}) opens the
// confirmation dialog and resolves to true when the user confirms, false when they
// cancel (button, Escape, backdrop). body is a string or a list of paragraphs (set as
// text). With requireText the confirm button stays disabled until it is typed.
// Focus moves into the dialog (Cancel for danger) and returns to the opener. With
// prompt ({label, value, maxLength}) it asks for a text instead (see promptDialog).
function confirmDialog(opts) {
  const o = opts || {};
  if (confirmState.resolve) {
    // Only one confirmation at a time: an older one counts as cancelled.
    confirmState.result = false;
    closeModal("modal-confirm");
    settleConfirm();
  }
  return new Promise(resolve => {
    confirmState.resolve = resolve;
    confirmState.result = false;
    confirmState.require = o.requireText && !o.prompt ? String(o.requireText) : "";
    confirmState.prompt = !!o.prompt;
    setText("confirm-title", o.title || t("dialog.confirm_title"));
    const body = document.getElementById("confirm-body");
    body.textContent = "";
    (Array.isArray(o.body) ? o.body : [o.body]).filter(Boolean).forEach(text => {
      const p = document.createElement("p");
      p.textContent = String(text);
      body.appendChild(p);
    });
    if (o.danger) {
      // undoNote replaces "This cannot be undone" for actions that can be undone for
      // a while (soft deletes, protection.js).
      const p = document.createElement("p");
      p.className = o.undoNote ? "notice notice-warn" : "notice notice-danger";
      p.textContent = o.undoNote ? String(o.undoNote) : t("dialog.cannot_undo");
      body.appendChild(p);
    }
    const require = document.getElementById("confirm-require");
    const input = document.getElementById("confirm-input");
    require.hidden = !confirmState.require && !o.prompt;
    input.value = o.prompt ? String(o.prompt.value || "") : "";
    // A secret prompt (a password, a passphrase) is masked and never autofilled.
    input.type = o.prompt && o.prompt.secret ? "password" : "text";
    input.autocomplete = o.prompt && o.prompt.secret ? "new-password" : "off";
    if (o.prompt && o.prompt.maxLength) input.maxLength = Number(o.prompt.maxLength);
    else input.removeAttribute("maxlength");
    setText("confirm-require-label", o.prompt ? String(o.prompt.label || "")
      : confirmState.require ? tf("dialog.type_text", { text: confirmState.require }) : "");
    const ok = document.getElementById("confirm-ok");
    ok.className = o.danger ? "btn btn-danger" : "btn btn-primary";
    ok.textContent = o.confirmLabel || t("dialog.confirm");
    setText("confirm-cancel", t("actions.cancel"));
    updateConfirmDialog();
    openModal("modal-confirm");
    const focus = confirmState.require || o.prompt ? input : o.danger ? document.getElementById("confirm-cancel") : ok;
    if (focus) focus.focus();
  });
}

function updateConfirmDialog() {
  const ok = document.getElementById("confirm-ok");
  const input = document.getElementById("confirm-input");
  if (ok) ok.disabled = !!confirmState.require && (!input || input.value.trim() !== confirmState.require);
}

function settleConfirm() {
  const resolve = confirmState.resolve;
  confirmState.resolve = null;
  if (resolve) {
    const input = document.getElementById("confirm-input");
    if (confirmState.prompt) resolve(confirmState.result && input ? input.value.trim() : null);
    else resolve(confirmState.result);
  }
  confirmState.result = false;
  confirmState.prompt = false;
}

// promptDialog({title, body, label, value, maxLength, confirmLabel}) asks for a text
// in the app's dialog (replaces window.prompt): it resolves to the trimmed text, or to
// null when the user cancels.
function promptDialog(opts) {
  const o = opts || {};
  return confirmDialog({
    title: o.title, body: o.body, confirmLabel: o.confirmLabel,
    prompt: { label: o.label || "", value: o.value || "", maxLength: o.maxLength }
  });
}

// ---------------------------------------------------------------------------
// Bulk dialog: review (dry run) → progress → result
// ---------------------------------------------------------------------------

function bulkPost(kind, body) {
  const k = bulkKind(kind);
  if (!k) return Promise.reject(new Error("unknown list"));
  return apiJSON(`/api/v1/${k}/bulk`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body)
  });
}

// The list filters of kind in the API's names, without paging and sorting.
function bulkApiFilter(kind) {
  if (kind === "jobs") return typeof tablesApiFilter === "function" ? tablesApiFilter("jobs") : {};
  const p = new URLSearchParams(listParams(kind));
  p.delete("limit");
  p.delete("offset");
  p.delete("sort");
  const f = {};
  p.forEach((v, k) => { f[k] = v; });
  return f;
}

function bulkSelection(kind) {
  const sel = bulkState.sel[kind];
  return sel.mode === "all" ? { filter: bulkApiFilter(kind) } : { ids: Array.from(sel.ids) };
}

async function openBulkAction(kind, name) {
  const action = bulkActionsFor(kind).find(a => a.name === name);
  if (!action || bulkCount(kind) === 0) return;
  const seq = ++bulkRun.seq;
  Object.assign(bulkRun, { kind, action, request: bulkSelection(kind), dry: null, running: false, results: null, failedOnly: false });
  setText("bulk-title", `${bulkActionLabel(action.name)} · ${t(`tabs.${kind}`)}`);
  setBulkBody(`<p class="muted" id="bulk-status" role="status">${escapeHtml(t("bulk.checking"))}</p>`);
  setBulkFooter([closeButton(t("actions.cancel"))]);
  openModal("modal-bulk");
  try {
    const json = await bulkPost(kind, { action: action.name, ...bulkRun.request, dry_run: true });
    if (seq !== bulkRun.seq) return;
    if (!json.success) {
      setBulkBody(`<p class="form-error" id="bulk-status" role="alert">${escapeHtml(json.error || t("toasts.request_failed"))}</p>`);
      return;
    }
    bulkRun.dry = json.data || {};
    renderBulkReview();
  } catch (err) {
    if (seq !== bulkRun.seq) return;
    setBulkBody(`<p class="form-error" id="bulk-status" role="alert">${escapeHtml(err.message)}</p>`);
  }
}

function setBulkBody(html) {
  const body = document.getElementById("bulk-body");
  if (body) body.innerHTML = html;
}

function closeButton(label) {
  return `<button type="button" class="btn btn-secondary" data-action="close-modal" data-target="modal-bulk">${escapeHtml(label)}</button>`;
}

function setBulkFooter(parts) {
  const footer = document.getElementById("bulk-footer");
  if (footer) footer.innerHTML = parts.join("");
}

// The per-item line of a skip: the translated code with its params, or the server's
// English detail for codes this dashboard does not know.
function skipDetail(reason, skip) {
  const params = skip && skip.params && typeof skip.params === "object" ? skip.params : {};
  const key = BULK_DETAIL_KEYS[reason];
  if (key) {
    if (key === "bulk.detail_status") {
      if (!params.status) return "";
      const label = typeof backupStatus === "function" ? backupStatus(String(params.status))[1] : String(params.status);
      return tf(key, { status: label });
    }
    return params.job ? tf(key, { job: String(params.job) }) : "";
  }
  return reason === "other" ? String((skip && skip.detail) || "") : "";
}

function skipReasonLabel(reason) {
  return BULK_SKIP_REASONS.includes(reason) ? t(`bulk.reason_${reason}`) : t("bulk.reason_other");
}

// The skipped items grouped by reason, each group a disclosure with its IDs.
function skippedGroups(skipped) {
  const groups = new Map();
  (skipped || []).forEach(s => {
    const reason = BULK_SKIP_REASONS.includes(s.reason) ? s.reason : "other";
    if (!groups.has(reason)) groups.set(reason, []);
    groups.get(reason).push(s);
  });
  if (groups.size === 0) return "";
  const parts = [];
  groups.forEach((items, reason) => {
    const lines = items.slice(0, BULK_LIST_MAX).map(s =>
      `<li><span class="mono">${escapeHtml(s.id)}</span>${skipDetail(reason, s) ? ` <span class="muted">${escapeHtml(skipDetail(reason, s))}</span>` : ""}</li>`).join("");
    const more = items.length > BULK_LIST_MAX ? `<li class="muted">${escapeHtml(tf("bulk.more", { n: formatCount(items.length - BULK_LIST_MAX) }))}</li>` : "";
    parts.push(`<details class="bulk-group"><summary>${escapeHtml(skipReasonLabel(reason))} <span class="badge">${escapeHtml(formatCount(items.length))}</span></summary><ul class="bulk-list">${lines}${more}</ul></details>`);
  });
  return `<section class="bulk-section" aria-labelledby="bulk-skipped-h"><h4 class="detail-heading" id="bulk-skipped-h">${escapeHtml(t("bulk.skipped_heading"))}</h4>${parts.join("")}</section>`;
}

function bulkStat(label, value) {
  return `<div class="bulk-stat"><dt>${escapeHtml(label)}</dt><dd>${escapeHtml(value)}</dd></div>`;
}

function needsTyping() {
  return bulkRun.action && bulkRun.action.destructive && bulkRun.dry && Number(bulkRun.dry.actionable) > BULK_TYPE_THRESHOLD;
}

function renderBulkReview() {
  const d = bulkRun.dry;
  const kind = bulkRun.kind;
  const actionable = Number(d.actionable) || 0;
  const stats = [
    bulkStat(t("bulk.matched"), formatCount(Number(d.matched) || 0)),
    bulkStat(t("bulk.actionable"), formatCount(actionable)),
    bulkStat(t("bulk.skipped"), formatCount((d.skipped || []).length))
  ];
  if (kind === "backups") stats.push(bulkStat(t("bulk.total_size"), formatBytes(d.total_size_bytes)));
  const parts = [`<dl class="bulk-stats" id="bulk-status">${stats.join("")}</dl>`];
  if (actionable === 0) {
    parts.push(`<p class="notice notice-warn">${escapeHtml(t("bulk.nothing_to_do"))}</p>`);
  } else if (kind === "backups" && bulkRun.action.name === "delete" && typeof protectionBulkNote === "function") {
    // Deleting backups is soft: the archives stay for the grace period (protection.js).
    parts.push(`<p class="notice notice-warn">${escapeHtml(protectionBulkNote())}</p>`);
  } else if (bulkRun.action.destructive) {
    parts.push(`<p class="notice notice-danger">${escapeHtml(t(`bulk.no_undo_${kind}`))}</p>`);
  }
  parts.push(skippedGroups(d.skipped));
  if (actionable > 0 && bulkRun.action.name === "pin") {
    parts.push(`<div class="form-group"><label class="form-label" for="bulk-note">${escapeHtml(t("bulk.pin_note"))}</label>
      <input type="text" class="form-input" id="bulk-note" maxlength="500" autocomplete="off"></div>`);
  }
  if (actionable > 0 && needsTyping()) {
    parts.push(`<div class="form-group"><label class="form-label" for="bulk-type">${escapeHtml(tf("bulk.type_count", { n: String(actionable) }))}</label>
      <input type="text" class="form-input bulk-type" id="bulk-type" inputmode="numeric" autocomplete="off" spellcheck="false" aria-label="${escapeHtml(t("bulk.type_count_label"))}"></div>`);
  }
  setBulkBody(parts.join(""));
  const footer = [closeButton(t("actions.cancel"))];
  if (actionable > 0) {
    const cls = bulkRun.action.destructive ? "btn btn-danger" : "btn btn-primary";
    footer.push(`<button type="button" class="${cls}" id="bulk-confirm" data-bulk="bulk-confirm">${escapeHtml(tf("bulk.confirm_n", { action: bulkActionLabel(bulkRun.action.name), n: formatCount(actionable) }))}</button>`);
  }
  setBulkFooter(footer);
  updateBulkConfirm();
  const first = document.getElementById("bulk-type") || (bulkRun.action.destructive ? document.querySelector("#bulk-footer .btn-secondary") : document.getElementById("bulk-confirm"));
  if (first) first.focus();
}

function updateBulkConfirm() {
  const btn = document.getElementById("bulk-confirm");
  if (!btn || !bulkRun.dry) return;
  const input = document.getElementById("bulk-type");
  btn.disabled = needsTyping() && (!input || input.value.replace(/\D/g, "") !== String(Number(bulkRun.dry.actionable) || 0));
}

function renderBulkProgress(done, total) {
  const bar = document.getElementById("bulk-progress");
  if (bar) {
    bar.max = Math.max(total, 1);
    bar.value = done;
  }
  setText("bulk-progress-text", tf("bulk.running", { done: formatCount(done), total: formatCount(total) }));
}

// Runs the action on exactly the items the dry run listed, BULK_CHUNK at a time. A
// chunk the server refuses as changed (409) is checked again and only its still
// actionable items run, so nothing beyond what was confirmed is ever changed.
async function executeBulk() {
  if (bulkRun.running || !bulkRun.dry || !bulkRun.action) return;
  const btn = document.getElementById("bulk-confirm");
  if (btn && btn.disabled) return;
  const kind = bulkRun.kind;
  const name = bulkRun.action.name;
  const ids = Array.isArray(bulkRun.dry.actionable_ids) ? bulkRun.dry.actionable_ids.slice() : [];
  const noteInput = document.getElementById("bulk-note");
  const extra = name === "pin" && noteInput && noteInput.value.trim() ? { note: noteInput.value.trim() } : {};
  const results = { items: [], skipped: (bulkRun.dry.skipped || []).slice(), stopped: "" };
  bulkRun.running = true;
  bulkRun.results = results;
  setBulkBody(`<div class="bulk-progress-wrap"><progress id="bulk-progress" max="${Math.max(ids.length, 1)}" value="0" aria-label="${escapeHtml(t("bulk.progress"))}"></progress>
    <p id="bulk-progress-text" role="status" aria-live="polite"></p></div>`);
  setBulkFooter([]);
  const close = document.getElementById("bulk-close");
  if (close) close.disabled = true;
  const progress = document.getElementById("bulk-progress");
  if (progress) progress.focus();
  renderBulkProgress(0, ids.length);

  let done = 0;
  try {
    for (let i = 0; i < ids.length; i += BULK_CHUNK) {
      const chunk = ids.slice(i, i + BULK_CHUNK);
      let json = await bulkPost(kind, { action: name, ids: chunk, confirm_count: chunk.length, ...extra });
      if (json.httpStatus === 409) {
        const again = await bulkPost(kind, { action: name, ids: chunk, dry_run: true });
        if (again.success && again.data) {
          (again.data.skipped || []).forEach(s => results.skipped.push(s));
          const still = again.data.actionable_ids || [];
          json = still.length > 0
            ? await bulkPost(kind, { action: name, ids: still, confirm_count: still.length, ...extra })
            : { success: true, data: { results: [] } };
        } else {
          json = again;
        }
      }
      if (json.success && json.httpStatus === 202 && json.data && json.data.approval_required) {
        // The two-person rule: the chunk waits for a second administrator.
        results.approvals = (results.approvals || []).concat(json.data.approval ? [json.data.approval] : []);
        if (typeof loadApprovals === "function") loadApprovals();
      } else if (json.success && json.data) {
        (json.data.results || []).forEach(r => results.items.push(r));
        // Items that became protected after the check (a pin, a newer verification).
        (json.data.skipped || []).forEach(sk => results.skipped.push(sk));
      } else {
        chunk.forEach(id => results.items.push({ id, ok: false, error: json.error || t("toasts.request_failed") }));
      }
      done += chunk.length;
      renderBulkProgress(done, ids.length);
    }
  } catch (err) {
    results.stopped = err.message || String(err);
    ids.slice(done).forEach(id => results.items.push({ id, ok: false, error: results.stopped }));
  } finally {
    bulkRun.running = false;
    if (close) close.disabled = false;
  }
  finishBulk(kind, results);
}

function finishBulk(kind, results) {
  const failed = results.items.filter(r => !r.ok).map(r => r.id);
  const sel = bulkState.sel[kind];
  sel.mode = "ids";
  sel.ids = new Set(failed);
  sel.anchor = "";
  sel.key = bulkFilterKey(kind);
  renderBulkResults();
  if (typeof refreshAll === "function") refreshAll();
  renderBulkUI(kind);
}

function renderBulkResults() {
  const r = bulkRun.results;
  if (!r) return;
  const ok = r.items.filter(i => i.ok).length;
  const failed = r.items.length - ok;
  const summary = tf("bulk.summary", { ok: formatCount(ok), skipped: formatCount(r.skipped.length), failed: formatCount(failed) });
  const shown = r.items.filter(i => !bulkRun.failedOnly || !i.ok || i.warning);
  const lines = shown.slice(0, BULK_RESULTS_MAX).map(i => {
    const cls = i.ok ? (i.warning ? "warn" : "ok") : "failed";
    const glyph = i.ok ? (i.warning ? "!" : "✓") : "✕";
    const note = i.error || (i.warning ? `${t("bulk.warning")}: ${i.warning}` : i.detail || "");
    return `<li class="bulk-result bulk-result-${cls}"><span class="bulk-glyph" aria-hidden="true">${glyph}</span><span class="mono">${escapeHtml(i.id)}</span>${note ? ` <span class="${i.ok ? "muted" : "cell-error"}">${escapeHtml(note)}</span>` : ""}</li>`;
  }).join("");
  const more = shown.length > BULK_RESULTS_MAX ? `<li class="muted">${escapeHtml(tf("bulk.more", { n: formatCount(shown.length - BULK_RESULTS_MAX) }))}</li>` : "";
  const empty = shown.length === 0 ? `<li class="muted">${escapeHtml(t("bulk.no_failures"))}</li>` : "";
  const parts = [`<p class="bulk-summary" id="bulk-status" role="status">${escapeHtml(summary)}</p>`];
  (r.approvals || []).forEach(a => {
    parts.push(`<p class="notice notice-warn">${escapeHtml(tf("protection.approval_waiting", { summary: String(a.summary || "") }))}</p>`);
  });
  if (r.stopped) parts.push(`<p class="form-error" role="alert">${escapeHtml(tf("bulk.stopped", { error: r.stopped }))}</p>`);
  if (failed > 0) parts.push(`<p class="muted">${escapeHtml(t("bulk.failed_kept"))}</p>`);
  parts.push(`<ul class="bulk-list bulk-results">${lines}${more}${empty}</ul>`);
  parts.push(skippedGroups(r.skipped));
  setBulkBody(parts.join(""));
  setText("bulk-title", `${bulkActionLabel(bulkRun.action ? bulkRun.action.name : "")} · ${t("bulk.done")}`);
  const toggle = `<button type="button" class="btn btn-secondary" data-bulk="bulk-failed-only" aria-pressed="${bulkRun.failedOnly ? "true" : "false"}">${escapeHtml(t(bulkRun.failedOnly ? "bulk.show_all" : "bulk.show_failed"))}</button>`;
  setBulkFooter([toggle, `<button type="button" class="btn btn-primary" data-action="close-modal" data-target="modal-bulk">${escapeHtml(t("actions.close"))}</button>`]);
  const focus = document.querySelector('#bulk-footer [data-bulk="bulk-failed-only"]');
  if (focus) focus.focus();
}
