/**
 * MongoRescue dashboard: delete protection.
 *
 * Deleting a backup is soft: its archive stays in storage for the delete grace
 * period (security.delete_grace_days) and the deletion can be undone until then from
 * the "Deleted" view of the backups list. Lowering a protection (a shorter grace
 * period or job retention) takes effect only after the current grace period; the
 * pending changes are listed under Settings → Security. With the two-person rule
 * (security.require_second_approver) destructive actions wait for a second
 * administrator: the header's Approvals button opens the waiting requests.
 *
 * Loaded after app.js, trust.js (mergeTranslations), bulk.js (confirmDialog) and
 * roles.js (ROLE_ACTION_SCOPES). The server stays authoritative for every rule; the
 * dashboard only explains them. Server values reach the DOM only through
 * escapeHtml() or textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const PROTECTION_TRANSLATIONS = {
  en: {
    status: { deleted: "Deleted", purged: "Purged" },
    bulk: { reason_already_deleted: "Already deleted" },
    notify: { events: { security_destructive_action: "Destructive action", security_approval_requested: "Approval requested" } },
    protection: {
      delete_title: "Delete backup?",
      confirm_delete: "Delete this backup? Its archive stays in storage for {days} days, until {date}, and you can undo the deletion until then.",
      undo_note: "Recoverable until {date}.",
      deleted_toast: "Backup deleted. You can undo this until {date}.",
      undo: "Undo delete",
      undo_confirm: "Undo the deletion of this backup? It gets back its earlier status and can be restored again.",
      undone_toast: "Deletion undone",
      show_deleted: "Deleted",
      show_deleted_title: "Show deleted backups that can still be undone",
      deleted_at: "Deleted",
      deleted_by: "Deleted by",
      approved_by: "Approved by",
      reason: "Reason",
      purge_after: "Recoverable until",
      purged_at: "Purged",
      bulk_soft_delete: "The backups are deleted, but their archives stay in storage for {days} days: you can undo each deletion from the Deleted view until then. The purge removes them afterwards.",
      approval_waiting: "A second administrator must approve this: {summary}",
      must_change_password: "An administrator reset your password. Choose a new password to continue.",
      password_reset_forced: "Password reset. The user will be forced to choose a new password at their next sign-in.",
      approvals: "Approvals",
      approvals_title: "Pending approvals",
      approvals_desc: "With the two-person rule, destructive actions wait here until another administrator approves them. Requests expire after 72 hours.",
      approvals_empty: "No requests are waiting.",
      requested_by: "Requested by {who}, {when}",
      via_api_key: "with an API key",
      expires: "Expires {when}",
      approve: "Approve",
      reject: "Reject",
      approve_confirm: "Approve this request and run it now? {summary}",
      reject_confirm: "Reject this request? Nothing is changed.",
      own_request: "You requested this; another administrator must approve it",
      approved_toast: "Approved: {result}",
      approve_failed: "Approved, but the action failed: {error}",
      rejected_toast: "Request rejected",
      badge_label: "{n} requests waiting for approval",
      group_title: "Delete protection",
      grace_label: "Delete grace period (days)",
      grace_hint: "Deleted backups keep their archive this long and the deletion can be undone ({min}–{max} days). A shorter period takes effect only after the current one.",
      second_approver: "Require a second administrator",
      second_approver_hint: "Deleting backups or storage targets, shortening retention, unpinning and lowering these protections wait for another administrator's approval. Needs at least two administrators; turning it off needs an approval too.",
      err_grace: "Enter a whole number of days between {min} and {max}.",
      pending_title: "Pending changes",
      pending_none: "No lowered protection is waiting.",
      pending_grace: "Grace period lowered to {days} days, effective {when}",
      pending_retention: "Retention of job {job} shortened to {value}, effective {when}",
      pending_metadata: "Metadata snapshots kept lowered to {n}, effective {when}",
      pending_disable: "Two-person rule turned off, effective {when} (no second administrator can approve)",
      pending_by: "requested by {who}",
      pending_cancel: "Cancel",
      pending_cancel_confirm: "Cancel this pending change? The current protection stays.",
      pending_cancelled: "Pending change cancelled; the current protection stays",
      pending_saved: "Saved. The lower grace period takes effect on {when}.",
      approval_requested_saved: "Saved. A change waits for a second administrator.",
      retention_pending_saved: "Saved. The shorter retention takes effect on {when}.",
      retention_approval_saved: "Saved. The shorter retention waits for a second administrator."
    }
  },
  tr: {
    status: { deleted: "Silindi", purged: "Kalıcı silindi" },
    bulk: { reason_already_deleted: "Zaten silinmiş" },
    notify: { events: { security_destructive_action: "Yıkıcı işlem", security_approval_requested: "Onay istendi" } },
    protection: {
      delete_title: "Yedek silinsin mi?",
      confirm_delete: "Bu yedek silinsin mi? Arşivi {days} gün boyunca, {date} tarihine kadar depolamada kalır ve o zamana kadar silme geri alınabilir.",
      undo_note: "{date} tarihine kadar kurtarılabilir.",
      deleted_toast: "Yedek silindi. {date} tarihine kadar geri alabilirsiniz.",
      undo: "Silmeyi geri al",
      undo_confirm: "Bu yedeğin silinmesi geri alınsın mı? Önceki durumuna döner ve yeniden geri yüklenebilir.",
      undone_toast: "Silme geri alındı",
      show_deleted: "Silinenler",
      show_deleted_title: "Hâlâ geri alınabilen silinmiş yedekleri göster",
      deleted_at: "Silinme",
      deleted_by: "Silen",
      approved_by: "Onaylayan",
      reason: "Neden",
      purge_after: "Kurtarılabilir son tarih",
      purged_at: "Kalıcı silinme",
      bulk_soft_delete: "Yedekler silinir, ancak arşivleri {days} gün boyunca depolamada kalır: o zamana kadar her silmeyi Silinenler görünümünden geri alabilirsiniz. Ardından temizleme onları kaldırır.",
      approval_waiting: "Bunu ikinci bir yönetici onaylamalı: {summary}",
      must_change_password: "Parolanızı bir yönetici sıfırladı. Devam etmek için yeni bir parola seçin.",
      password_reset_forced: "Parola sıfırlandı. Kullanıcı bir sonraki girişinde yeni bir parola seçmek zorunda kalacak.",
      approvals: "Onaylar",
      approvals_title: "Bekleyen onaylar",
      approvals_desc: "İki kişi kuralı açıkken yıkıcı işlemler, başka bir yönetici onaylayana kadar burada bekler. İstekler 72 saat sonra sona erer.",
      approvals_empty: "Bekleyen istek yok.",
      requested_by: "{who} istedi, {when}",
      via_api_key: "API anahtarıyla",
      expires: "Bitiş: {when}",
      approve: "Onayla",
      reject: "Reddet",
      approve_confirm: "Bu istek onaylanıp şimdi çalıştırılsın mı? {summary}",
      reject_confirm: "Bu istek reddedilsin mi? Hiçbir şey değişmez.",
      own_request: "Bunu siz istediniz; başka bir yönetici onaylamalı",
      approved_toast: "Onaylandı: {result}",
      approve_failed: "Onaylandı, ancak işlem başarısız oldu: {error}",
      rejected_toast: "İstek reddedildi",
      badge_label: "Onay bekleyen {n} istek",
      group_title: "Silme koruması",
      grace_label: "Silme bekleme süresi (gün)",
      grace_hint: "Silinen yedekler arşivlerini bu süre boyunca tutar ve silme geri alınabilir ({min}–{max} gün). Daha kısa bir süre ancak mevcut süre dolduktan sonra geçerli olur.",
      second_approver: "İkinci bir yönetici gerektir",
      second_approver_hint: "Yedekleri veya depolama hedeflerini silmek, saklamayı kısaltmak, sabitlemeyi kaldırmak ve bu korumaları düşürmek başka bir yöneticinin onayını bekler. En az iki yönetici gerekir; kapatmak da onay gerektirir.",
      err_grace: "{min} ile {max} arasında tam bir gün sayısı girin.",
      pending_title: "Bekleyen değişiklikler",
      pending_none: "Bekleyen düşürülmüş koruma yok.",
      pending_grace: "Bekleme süresi {days} güne düşürüldü, geçerlilik {when}",
      pending_retention: "{job} görevinin saklama süresi {value} olarak kısaltıldı, geçerlilik {when}",
      pending_metadata: "Saklanan meta veri anlık görüntüsü sayısı {n} olarak düşürüldü, geçerlilik {when}",
      pending_disable: "İki kişi kuralı kapatıldı, geçerlilik {when} (onaylayabilecek ikinci bir yönetici yok)",
      pending_by: "isteyen: {who}",
      pending_cancel: "İptal et",
      pending_cancel_confirm: "Bu bekleyen değişiklik iptal edilsin mi? Mevcut koruma kalır.",
      pending_cancelled: "Bekleyen değişiklik iptal edildi; mevcut koruma kalır",
      pending_saved: "Kaydedildi. Daha kısa bekleme süresi {when} tarihinde geçerli olur.",
      approval_requested_saved: "Kaydedildi. Bir değişiklik ikinci bir yöneticiyi bekliyor.",
      retention_pending_saved: "Kaydedildi. Daha kısa saklama süresi {when} tarihinde geçerli olur.",
      retention_approval_saved: "Kaydedildi. Daha kısa saklama süresi ikinci bir yöneticiyi bekliyor."
    }
  },
  de: {
    status: { deleted: "Gelöscht", purged: "Endgültig gelöscht" },
    bulk: { reason_already_deleted: "Bereits gelöscht" },
    notify: { events: { security_destructive_action: "Destruktive Aktion", security_approval_requested: "Freigabe angefordert" } },
    protection: {
      delete_title: "Backup löschen?",
      confirm_delete: "Dieses Backup löschen? Sein Archiv bleibt {days} Tage, bis {date}, im Speicher, und das Löschen kann bis dahin rückgängig gemacht werden.",
      undo_note: "Wiederherstellbar bis {date}.",
      deleted_toast: "Backup gelöscht. Rückgängig machen ist bis {date} möglich.",
      undo: "Löschen rückgängig machen",
      undo_confirm: "Das Löschen dieses Backups rückgängig machen? Es erhält seinen früheren Status zurück und kann wieder wiederhergestellt werden.",
      undone_toast: "Löschen rückgängig gemacht",
      show_deleted: "Gelöscht",
      show_deleted_title: "Gelöschte Backups anzeigen, deren Löschen noch rückgängig gemacht werden kann",
      deleted_at: "Gelöscht",
      deleted_by: "Gelöscht von",
      approved_by: "Freigegeben von",
      reason: "Grund",
      purge_after: "Wiederherstellbar bis",
      purged_at: "Endgültig gelöscht",
      bulk_soft_delete: "Die Backups werden gelöscht, ihre Archive bleiben aber {days} Tage im Speicher: Bis dahin kann jedes Löschen in der Ansicht „Gelöscht“ rückgängig gemacht werden. Danach entfernt die Bereinigung sie.",
      approval_waiting: "Ein zweiter Administrator muss das freigeben: {summary}",
      must_change_password: "Ein Administrator hat Ihr Passwort zurückgesetzt. Wählen Sie ein neues Passwort, um fortzufahren.",
      password_reset_forced: "Passwort zurückgesetzt. Der Benutzer muss bei der nächsten Anmeldung ein neues Passwort wählen.",
      approvals: "Freigaben",
      approvals_title: "Ausstehende Freigaben",
      approvals_desc: "Mit dem Vier-Augen-Prinzip warten destruktive Aktionen hier, bis ein anderer Administrator sie freigibt. Anfragen verfallen nach 72 Stunden.",
      approvals_empty: "Keine Anfragen warten.",
      requested_by: "Angefordert von {who}, {when}",
      via_api_key: "mit einem API-Schlüssel",
      expires: "Verfällt {when}",
      approve: "Freigeben",
      reject: "Ablehnen",
      approve_confirm: "Diese Anfrage freigeben und jetzt ausführen? {summary}",
      reject_confirm: "Diese Anfrage ablehnen? Es wird nichts geändert.",
      own_request: "Sie haben das angefordert; ein anderer Administrator muss es freigeben",
      approved_toast: "Freigegeben: {result}",
      approve_failed: "Freigegeben, aber die Aktion ist fehlgeschlagen: {error}",
      rejected_toast: "Anfrage abgelehnt",
      badge_label: "{n} Anfragen warten auf Freigabe",
      group_title: "Löschschutz",
      grace_label: "Karenzzeit beim Löschen (Tage)",
      grace_hint: "Gelöschte Backups behalten ihr Archiv so lange, und das Löschen kann rückgängig gemacht werden ({min}–{max} Tage). Eine kürzere Zeit gilt erst nach Ablauf der aktuellen.",
      second_approver: "Zweiten Administrator verlangen",
      second_approver_hint: "Das Löschen von Backups oder Speicherzielen, das Verkürzen der Aufbewahrung, das Lösen von Pins und das Senken dieser Schutzmaßnahmen warten auf die Freigabe eines anderen Administrators. Erfordert mindestens zwei Administratoren; auch das Ausschalten braucht eine Freigabe.",
      err_grace: "Geben Sie eine ganze Zahl von Tagen zwischen {min} und {max} ein.",
      pending_title: "Ausstehende Änderungen",
      pending_none: "Keine gesenkte Schutzmaßnahme wartet.",
      pending_grace: "Karenzzeit auf {days} Tage gesenkt, wirksam {when}",
      pending_retention: "Aufbewahrung von Job {job} auf {value} verkürzt, wirksam {when}",
      pending_metadata: "Aufbewahrte Metadaten-Snapshots auf {n} gesenkt, wirksam {when}",
      pending_disable: "Vier-Augen-Prinzip ausgeschaltet, wirksam {when} (kein zweiter Administrator kann freigeben)",
      pending_by: "angefordert von {who}",
      pending_cancel: "Abbrechen",
      pending_cancel_confirm: "Diese ausstehende Änderung abbrechen? Der aktuelle Schutz bleibt.",
      pending_cancelled: "Ausstehende Änderung abgebrochen; der aktuelle Schutz bleibt",
      pending_saved: "Gespeichert. Die kürzere Karenzzeit gilt ab {when}.",
      approval_requested_saved: "Gespeichert. Eine Änderung wartet auf einen zweiten Administrator.",
      retention_pending_saved: "Gespeichert. Die kürzere Aufbewahrung gilt ab {when}.",
      retention_approval_saved: "Gespeichert. Die kürzere Aufbewahrung wartet auf einen zweiten Administrator."
    }
  },
  es: {
    status: { deleted: "Eliminada", purged: "Purgada" },
    bulk: { reason_already_deleted: "Ya eliminada" },
    notify: { events: { security_destructive_action: "Acción destructiva", security_approval_requested: "Aprobación solicitada" } },
    protection: {
      delete_title: "¿Eliminar la copia?",
      confirm_delete: "¿Eliminar esta copia? Su archivo permanece en el almacenamiento {days} días, hasta el {date}, y puede deshacer la eliminación hasta entonces.",
      undo_note: "Recuperable hasta el {date}.",
      deleted_toast: "Copia eliminada. Puede deshacerlo hasta el {date}.",
      undo: "Deshacer eliminación",
      undo_confirm: "¿Deshacer la eliminación de esta copia? Recupera su estado anterior y se puede restaurar de nuevo.",
      undone_toast: "Eliminación deshecha",
      show_deleted: "Eliminadas",
      show_deleted_title: "Mostrar las copias eliminadas que aún se pueden recuperar",
      deleted_at: "Eliminada",
      deleted_by: "Eliminada por",
      approved_by: "Aprobada por",
      reason: "Motivo",
      purge_after: "Recuperable hasta",
      purged_at: "Purgada",
      bulk_soft_delete: "Las copias se eliminan, pero sus archivos permanecen en el almacenamiento {days} días: hasta entonces puede deshacer cada eliminación desde la vista Eliminadas. Después, la purga los quita.",
      approval_waiting: "Un segundo administrador debe aprobarlo: {summary}",
      must_change_password: "Un administrador restableció su contraseña. Elija una nueva contraseña para continuar.",
      password_reset_forced: "Contraseña restablecida. El usuario deberá elegir una nueva contraseña en su próximo inicio de sesión.",
      approvals: "Aprobaciones",
      approvals_title: "Aprobaciones pendientes",
      approvals_desc: "Con la regla de dos personas, las acciones destructivas esperan aquí hasta que otro administrador las apruebe. Las solicitudes caducan a las 72 horas.",
      approvals_empty: "No hay solicitudes en espera.",
      requested_by: "Solicitada por {who}, {when}",
      via_api_key: "con una clave API",
      expires: "Caduca {when}",
      approve: "Aprobar",
      reject: "Rechazar",
      approve_confirm: "¿Aprobar esta solicitud y ejecutarla ahora? {summary}",
      reject_confirm: "¿Rechazar esta solicitud? No se cambia nada.",
      own_request: "Usted lo solicitó; otro administrador debe aprobarlo",
      approved_toast: "Aprobada: {result}",
      approve_failed: "Aprobada, pero la acción falló: {error}",
      rejected_toast: "Solicitud rechazada",
      badge_label: "{n} solicitudes esperan aprobación",
      group_title: "Protección contra eliminación",
      grace_label: "Periodo de gracia de eliminación (días)",
      grace_hint: "Las copias eliminadas conservan su archivo durante este tiempo y la eliminación se puede deshacer ({min}–{max} días). Un periodo más corto solo se aplica tras el actual.",
      second_approver: "Exigir un segundo administrador",
      second_approver_hint: "Eliminar copias o destinos de almacenamiento, acortar la retención, quitar anclajes y reducir estas protecciones esperan la aprobación de otro administrador. Requiere al menos dos administradores; desactivarlo también necesita aprobación.",
      err_grace: "Introduzca un número entero de días entre {min} y {max}.",
      pending_title: "Cambios pendientes",
      pending_none: "No hay ninguna protección reducida en espera.",
      pending_grace: "Periodo de gracia reducido a {days} días, efectivo {when}",
      pending_retention: "Retención de la tarea {job} acortada a {value}, efectiva {when}",
      pending_metadata: "Instantáneas de metadatos conservadas reducidas a {n}, efectivo {when}",
      pending_disable: "Regla de dos personas desactivada, efectiva {when} (ningún segundo administrador puede aprobar)",
      pending_by: "solicitado por {who}",
      pending_cancel: "Cancelar",
      pending_cancel_confirm: "¿Cancelar este cambio pendiente? La protección actual se mantiene.",
      pending_cancelled: "Cambio pendiente cancelado; la protección actual se mantiene",
      pending_saved: "Guardado. El periodo de gracia más corto se aplica el {when}.",
      approval_requested_saved: "Guardado. Un cambio espera a un segundo administrador.",
      retention_pending_saved: "Guardado. La retención más corta se aplica el {when}.",
      retention_approval_saved: "Guardado. La retención más corta espera a un segundo administrador."
    }
  },
  fr: {
    status: { deleted: "Supprimée", purged: "Purgée" },
    bulk: { reason_already_deleted: "Déjà supprimée" },
    notify: { events: { security_destructive_action: "Action destructive", security_approval_requested: "Approbation demandée" } },
    protection: {
      delete_title: "Supprimer la sauvegarde ?",
      confirm_delete: "Supprimer cette sauvegarde ? Son archive reste dans le stockage pendant {days} jours, jusqu'au {date}, et vous pouvez annuler la suppression jusque-là.",
      undo_note: "Récupérable jusqu'au {date}.",
      deleted_toast: "Sauvegarde supprimée. Vous pouvez annuler jusqu'au {date}.",
      undo: "Annuler la suppression",
      undo_confirm: "Annuler la suppression de cette sauvegarde ? Elle retrouve son état précédent et peut de nouveau être restaurée.",
      undone_toast: "Suppression annulée",
      show_deleted: "Supprimées",
      show_deleted_title: "Afficher les sauvegardes supprimées encore récupérables",
      deleted_at: "Supprimée",
      deleted_by: "Supprimée par",
      approved_by: "Approuvée par",
      reason: "Motif",
      purge_after: "Récupérable jusqu'au",
      purged_at: "Purgée",
      bulk_soft_delete: "Les sauvegardes sont supprimées, mais leurs archives restent dans le stockage pendant {days} jours : jusque-là, chaque suppression peut être annulée depuis la vue Supprimées. La purge les retire ensuite.",
      approval_waiting: "Un second administrateur doit approuver : {summary}",
      must_change_password: "Un administrateur a réinitialisé votre mot de passe. Choisissez un nouveau mot de passe pour continuer.",
      password_reset_forced: "Mot de passe réinitialisé. L'utilisateur devra choisir un nouveau mot de passe à sa prochaine connexion.",
      approvals: "Approbations",
      approvals_title: "Approbations en attente",
      approvals_desc: "Avec la règle des deux personnes, les actions destructives attendent ici qu'un autre administrateur les approuve. Les demandes expirent après 72 heures.",
      approvals_empty: "Aucune demande en attente.",
      requested_by: "Demandée par {who}, {when}",
      via_api_key: "avec une clé API",
      expires: "Expire {when}",
      approve: "Approuver",
      reject: "Refuser",
      approve_confirm: "Approuver cette demande et l'exécuter maintenant ? {summary}",
      reject_confirm: "Refuser cette demande ? Rien n'est modifié.",
      own_request: "Vous l'avez demandé ; un autre administrateur doit l'approuver",
      approved_toast: "Approuvée : {result}",
      approve_failed: "Approuvée, mais l'action a échoué : {error}",
      rejected_toast: "Demande refusée",
      badge_label: "{n} demandes en attente d'approbation",
      group_title: "Protection contre la suppression",
      grace_label: "Délai de grâce des suppressions (jours)",
      grace_hint: "Les sauvegardes supprimées gardent leur archive pendant ce délai et la suppression peut être annulée ({min}–{max} jours). Un délai plus court ne s'applique qu'à la fin du délai actuel.",
      second_approver: "Exiger un second administrateur",
      second_approver_hint: "Supprimer des sauvegardes ou des cibles de stockage, raccourcir la rétention, retirer un épinglage et abaisser ces protections attendent l'approbation d'un autre administrateur. Nécessite au moins deux administrateurs ; la désactivation demande aussi une approbation.",
      err_grace: "Saisissez un nombre entier de jours entre {min} et {max}.",
      pending_title: "Changements en attente",
      pending_none: "Aucune protection abaissée en attente.",
      pending_grace: "Délai de grâce ramené à {days} jours, effectif {when}",
      pending_retention: "Rétention de la tâche {job} raccourcie à {value}, effective {when}",
      pending_metadata: "Instantanés de métadonnées conservés ramenés à {n}, effectif {when}",
      pending_disable: "Règle des deux personnes désactivée, effective {when} (aucun second administrateur ne peut approuver)",
      pending_by: "demandé par {who}",
      pending_cancel: "Annuler",
      pending_cancel_confirm: "Annuler ce changement en attente ? La protection actuelle reste.",
      pending_cancelled: "Changement en attente annulé ; la protection actuelle reste",
      pending_saved: "Enregistré. Le délai de grâce plus court s'applique le {when}.",
      approval_requested_saved: "Enregistré. Un changement attend un second administrateur.",
      retention_pending_saved: "Enregistré. La rétention plus courte s'applique le {when}.",
      retention_approval_saved: "Enregistré. La rétention plus courte attend un second administrateur."
    }
  },
  zh: {
    status: { deleted: "已删除", purged: "已清除" },
    bulk: { reason_already_deleted: "已删除" },
    notify: { events: { security_destructive_action: "破坏性操作", security_approval_requested: "已请求审批" } },
    protection: {
      delete_title: "删除备份？",
      confirm_delete: "删除此备份？其归档会在存储中保留 {days} 天，直到 {date}，在此之前可以撤销删除。",
      undo_note: "可恢复至 {date}。",
      deleted_toast: "备份已删除。可在 {date} 前撤销。",
      undo: "撤销删除",
      undo_confirm: "撤销删除此备份？它将恢复之前的状态，并可再次用于恢复。",
      undone_toast: "已撤销删除",
      show_deleted: "已删除",
      show_deleted_title: "显示仍可撤销删除的备份",
      deleted_at: "删除时间",
      deleted_by: "删除者",
      approved_by: "审批者",
      reason: "原因",
      purge_after: "可恢复至",
      purged_at: "清除时间",
      bulk_soft_delete: "这些备份会被删除，但其归档会在存储中保留 {days} 天：在此之前可在“已删除”视图中撤销每次删除。之后清除任务会将其移除。",
      approval_waiting: "需要第二位管理员审批：{summary}",
      must_change_password: "管理员已重置您的密码。请设置新密码后继续。",
      password_reset_forced: "密码已重置。该用户下次登录时必须设置新密码。",
      approvals: "审批",
      approvals_title: "待审批",
      approvals_desc: "启用双人规则后，破坏性操作会在此等待另一位管理员审批。请求在 72 小时后过期。",
      approvals_empty: "没有等待中的请求。",
      requested_by: "由 {who} 请求，{when}",
      via_api_key: "通过 API 密钥",
      expires: "{when} 过期",
      approve: "批准",
      reject: "拒绝",
      approve_confirm: "批准此请求并立即执行？{summary}",
      reject_confirm: "拒绝此请求？不会做任何更改。",
      own_request: "这是您发起的请求，需要另一位管理员批准",
      approved_toast: "已批准：{result}",
      approve_failed: "已批准，但操作失败：{error}",
      rejected_toast: "请求已拒绝",
      badge_label: "{n} 个请求等待审批",
      group_title: "删除保护",
      grace_label: "删除宽限期（天）",
      grace_hint: "已删除的备份会在此期间保留归档，并可撤销删除（{min}–{max} 天）。更短的期限只会在当前期限结束后生效。",
      second_approver: "要求第二位管理员",
      second_approver_hint: "删除备份或存储目标、缩短保留期、取消固定以及降低这些保护，都需等待另一位管理员审批。至少需要两位管理员；关闭此规则同样需要审批。",
      err_grace: "请输入 {min} 到 {max} 之间的整数天数。",
      pending_title: "待生效的更改",
      pending_none: "没有等待生效的降低保护。",
      pending_grace: "宽限期降低为 {days} 天，于 {when} 生效",
      pending_retention: "任务 {job} 的保留期缩短为 {value}，于 {when} 生效",
      pending_metadata: "保留的元数据快照数降为 {n}，于 {when} 生效",
      pending_disable: "双人规则关闭，于 {when} 生效（没有第二位管理员可以审批）",
      pending_by: "请求者：{who}",
      pending_cancel: "取消",
      pending_cancel_confirm: "取消这项待生效的更改？当前保护保持不变。",
      pending_cancelled: "已取消待生效的更改；当前保护保持不变",
      pending_saved: "已保存。更短的宽限期将于 {when} 生效。",
      approval_requested_saved: "已保存。有一项更改等待第二位管理员审批。",
      retention_pending_saved: "已保存。更短的保留期将于 {when} 生效。",
      retention_approval_saved: "已保存。更短的保留期等待第二位管理员审批。"
    }
  },
  ja: {
    status: { deleted: "削除済み", purged: "完全削除済み" },
    bulk: { reason_already_deleted: "削除済み" },
    notify: { events: { security_destructive_action: "破壊的な操作", security_approval_requested: "承認の依頼" } },
    protection: {
      delete_title: "バックアップを削除しますか？",
      confirm_delete: "このバックアップを削除しますか？アーカイブは {days} 日間、{date} までストレージに残り、それまでは削除を取り消せます。",
      undo_note: "{date} まで復元できます。",
      deleted_toast: "バックアップを削除しました。{date} まで取り消せます。",
      undo: "削除を取り消す",
      undo_confirm: "このバックアップの削除を取り消しますか？以前の状態に戻り、再びリストアに使えます。",
      undone_toast: "削除を取り消しました",
      show_deleted: "削除済み",
      show_deleted_title: "まだ取り消せる削除済みバックアップを表示",
      deleted_at: "削除日時",
      deleted_by: "削除した人",
      approved_by: "承認した人",
      reason: "理由",
      purge_after: "復元期限",
      purged_at: "完全削除日時",
      bulk_soft_delete: "バックアップは削除されますが、アーカイブは {days} 日間ストレージに残ります。それまでは「削除済み」ビューから各削除を取り消せます。その後、完全削除で取り除かれます。",
      approval_waiting: "2 人目の管理者の承認が必要です: {summary}",
      must_change_password: "管理者があなたのパスワードをリセットしました。続行するには新しいパスワードを設定してください。",
      password_reset_forced: "パスワードをリセットしました。ユーザーは次回のサインイン時に新しいパスワードの設定を求められます。",
      approvals: "承認",
      approvals_title: "承認待ち",
      approvals_desc: "2 人ルールが有効な場合、破壊的な操作は別の管理者が承認するまでここで待機します。依頼は 72 時間で期限切れになります。",
      approvals_empty: "待機中の依頼はありません。",
      requested_by: "{who} が依頼、{when}",
      via_api_key: "API キーで",
      expires: "{when} に期限切れ",
      approve: "承認",
      reject: "却下",
      approve_confirm: "この依頼を承認して今すぐ実行しますか？{summary}",
      reject_confirm: "この依頼を却下しますか？何も変更されません。",
      own_request: "ご自身の依頼です。別の管理者が承認する必要があります",
      approved_toast: "承認しました: {result}",
      approve_failed: "承認しましたが、操作に失敗しました: {error}",
      rejected_toast: "依頼を却下しました",
      badge_label: "承認待ちの依頼 {n} 件",
      group_title: "削除保護",
      grace_label: "削除の猶予期間（日）",
      grace_hint: "削除したバックアップはこの期間アーカイブを保持し、削除を取り消せます（{min}〜{max} 日）。短い期間は現在の期間が終わってから有効になります。",
      second_approver: "2 人目の管理者を必須にする",
      second_approver_hint: "バックアップやストレージターゲットの削除、保持期間の短縮、ピンの解除、これらの保護の引き下げは、別の管理者の承認を待ちます。管理者が 2 人以上必要で、無効にするにも承認が必要です。",
      err_grace: "{min} から {max} までの整数の日数を入力してください。",
      pending_title: "保留中の変更",
      pending_none: "待機中の保護の引き下げはありません。",
      pending_grace: "猶予期間を {days} 日に短縮、{when} に有効",
      pending_retention: "ジョブ {job} の保持期間を {value} に短縮、{when} に有効",
      pending_metadata: "保持するメタデータスナップショット数を {n} に削減、{when} に有効",
      pending_disable: "2 人ルールを無効化、{when} に有効（承認できる 2 人目の管理者がいません）",
      pending_by: "依頼者: {who}",
      pending_cancel: "キャンセル",
      pending_cancel_confirm: "この保留中の変更をキャンセルしますか？現在の保護はそのままです。",
      pending_cancelled: "保留中の変更をキャンセルしました。現在の保護はそのままです",
      pending_saved: "保存しました。短い猶予期間は {when} に有効になります。",
      approval_requested_saved: "保存しました。変更が 2 人目の管理者を待っています。",
      retention_pending_saved: "保存しました。短い保持期間は {when} に有効になります。",
      retention_approval_saved: "保存しました。短い保持期間は 2 人目の管理者を待っています。"
    }
  },
  ru: {
    status: { deleted: "Удалён", purged: "Стёрт" },
    bulk: { reason_already_deleted: "Уже удалён" },
    notify: { events: { security_destructive_action: "Разрушительное действие", security_approval_requested: "Запрошено одобрение" } },
    protection: {
      delete_title: "Удалить бэкап?",
      confirm_delete: "Удалить этот бэкап? Его архив останется в хранилище {days} дн., до {date}, и до этого удаление можно отменить.",
      undo_note: "Можно восстановить до {date}.",
      deleted_toast: "Бэкап удалён. Отменить можно до {date}.",
      undo: "Отменить удаление",
      undo_confirm: "Отменить удаление этого бэкапа? Он вернётся в прежнее состояние и снова будет доступен для восстановления.",
      undone_toast: "Удаление отменено",
      show_deleted: "Удалённые",
      show_deleted_title: "Показать удалённые бэкапы, удаление которых ещё можно отменить",
      deleted_at: "Удалён",
      deleted_by: "Кем удалён",
      approved_by: "Кем одобрено",
      reason: "Причина",
      purge_after: "Можно восстановить до",
      purged_at: "Стёрт",
      bulk_soft_delete: "Бэкапы удаляются, но их архивы остаются в хранилище {days} дн.: до этого каждое удаление можно отменить в представлении «Удалённые». Затем очистка их стирает.",
      approval_waiting: "Требуется одобрение второго администратора: {summary}",
      must_change_password: "Администратор сбросил ваш пароль. Чтобы продолжить, выберите новый пароль.",
      password_reset_forced: "Пароль сброшен. При следующем входе пользователю придётся выбрать новый пароль.",
      approvals: "Одобрения",
      approvals_title: "Ожидают одобрения",
      approvals_desc: "При правиле двух лиц разрушительные действия ждут здесь, пока их не одобрит другой администратор. Запросы истекают через 72 часа.",
      approvals_empty: "Нет ожидающих запросов.",
      requested_by: "Запросил {who}, {when}",
      via_api_key: "API-ключом",
      expires: "Истекает {when}",
      approve: "Одобрить",
      reject: "Отклонить",
      approve_confirm: "Одобрить этот запрос и выполнить сейчас? {summary}",
      reject_confirm: "Отклонить этот запрос? Ничего не изменится.",
      own_request: "Это ваш запрос; одобрить его должен другой администратор",
      approved_toast: "Одобрено: {result}",
      approve_failed: "Одобрено, но действие не удалось: {error}",
      rejected_toast: "Запрос отклонён",
      badge_label: "Запросов, ожидающих одобрения: {n}",
      group_title: "Защита от удаления",
      grace_label: "Отсрочка удаления (дни)",
      grace_hint: "Удалённые бэкапы хранят архив в течение этого срока, и удаление можно отменить ({min}–{max} дн.). Более короткий срок вступает в силу только после окончания текущего.",
      second_approver: "Требовать второго администратора",
      second_approver_hint: "Удаление бэкапов или целей хранения, сокращение хранения, снятие закрепления и ослабление этих защит ждут одобрения другого администратора. Нужны минимум два администратора; для отключения тоже нужно одобрение.",
      err_grace: "Введите целое число дней от {min} до {max}.",
      pending_title: "Ожидающие изменения",
      pending_none: "Нет ожидающих ослаблений защиты.",
      pending_grace: "Отсрочка сокращена до {days} дн., вступает в силу {when}",
      pending_retention: "Хранение задания {job} сокращено до {value}, вступает в силу {when}",
      pending_metadata: "Число хранимых снимков метаданных сокращено до {n}, вступает в силу {when}",
      pending_disable: "Правило двух лиц отключено, вступает в силу {when} (нет второго администратора для одобрения)",
      pending_by: "запросил {who}",
      pending_cancel: "Отменить",
      pending_cancel_confirm: "Отменить это ожидающее изменение? Текущая защита сохранится.",
      pending_cancelled: "Ожидающее изменение отменено; текущая защита сохраняется",
      pending_saved: "Сохранено. Более короткая отсрочка вступит в силу {when}.",
      approval_requested_saved: "Сохранено. Изменение ждёт второго администратора.",
      retention_pending_saved: "Сохранено. Более короткое хранение вступит в силу {when}.",
      retention_approval_saved: "Сохранено. Более короткое хранение ждёт второго администратора."
    }
  }
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(PROTECTION_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], PROTECTION_TRANSLATIONS[lang]);
  });
}

// The scopes of this module's actions (roles.js gates them like the others).
if (typeof ROLE_ACTION_SCOPES === "object") {
  Object.assign(ROLE_ACTION_SCOPES, {
    "undelete-backup": "admin",
    "approval-approve": "admin",
    "approval-reject": "admin",
    "pending-cancel": "admin"
  });
}

// ---------------------------------------------------------------------------
// State and helpers
// ---------------------------------------------------------------------------

const GRACE_MIN_DAYS = 1;
const GRACE_MAX_DAYS = 90;
const GRACE_DEFAULT_DAYS = 7;
const APPROVALS_POLL_MS = 60000;

const protection = { approvals: [], pending: [], timer: null, seq: 0 };

// protectionSecurity returns the security settings the dashboard loaded.
function protectionSecurity() {
  return (state.settings && state.settings.security) || {};
}

// protectionGraceDays returns the delete grace period in force, in days.
function protectionGraceDays() {
  const n = Number(protectionSecurity().delete_grace_days);
  return Number.isInteger(n) && n >= GRACE_MIN_DAYS ? n : GRACE_DEFAULT_DAYS;
}

// protectionPurgeDate returns when a deletion made now can no longer be undone.
function protectionPurgeDate() {
  return new Date(Date.now() + protectionGraceDays() * 24 * 60 * 60 * 1000);
}

// protectionWhen formats a server time for display ("" when missing).
function protectionWhen(value) {
  const d = parseDate(value);
  return d ? formatAbsolute(d) : "";
}

// protectionDeleteConfirm is the confirmation text of deleting one backup.
function protectionDeleteConfirm() {
  return tf("protection.confirm_delete", { days: formatCount(protectionGraceDays()), date: formatAbsolute(protectionPurgeDate()) });
}

// protectionUndoNote is the note shown instead of "This cannot be undone".
function protectionUndoNote() {
  return tf("protection.undo_note", { date: formatAbsolute(protectionPurgeDate()) });
}

// protectionBulkNote is the review note of a bulk delete of backups.
function protectionBulkNote() {
  return tf("protection.bulk_soft_delete", { days: formatCount(protectionGraceDays()) });
}

// protectionApprovalPending reports (and announces) an answer that waits for a
// second administrator (202 with approval_required).
function protectionApprovalPending(json) {
  const data = json && json.data;
  if (!json || json.httpStatus !== 202 || !data || !data.approval_required || !data.approval) return false;
  showToast(tf("protection.approval_waiting", { summary: String(data.approval.summary || "") }), "info");
  loadApprovals();
  return true;
}

// protectionDeletedToast announces a soft delete with its purge time.
function protectionDeletedToast(data) {
  const when = data && data.purge_after ? protectionWhen(data.purge_after) : formatAbsolute(protectionPurgeDate());
  showToast(tf("protection.deleted_toast", { date: when }), "success");
}

// ---------------------------------------------------------------------------
// Backups: rows, details and the Deleted view
// ---------------------------------------------------------------------------

// protectionBackupMenuItems returns the row menu of a deleted or purged backup (app.js
// rowMenuItems), or null for the others.
function protectionBackupMenuItems(id) {
  const b = state.backups.find(x => x.id === id);
  if (!b) return null;
  if (b.status === "deleted") return [["backup-details", t("actions.details")], ["undelete-backup", t("protection.undo")]];
  if (b.status === "purged") return [["backup-details", t("actions.details")]];
  return null;
}

// protectionBackupDetailRows adds the deletion of b to the backup details dialog.
function protectionBackupDetailRows(b, row) {
  if (!b || (b.status !== "deleted" && b.status !== "purged")) return;
  if (b.deleted_at) row(t("protection.deleted_at"), absoluteWithRelative(b.deleted_at));
  if (b.deleted_by) row(t("protection.deleted_by"), String(b.deleted_by));
  if (b.delete_approved_by) row(t("protection.approved_by"), String(b.delete_approved_by));
  if (b.delete_reason) row(t("protection.reason"), String(b.delete_reason));
  if (b.status === "deleted" && b.purge_after) row(t("protection.purge_after"), absoluteWithRelative(b.purge_after));
  if (b.purged_at) row(t("protection.purged_at"), absoluteWithRelative(b.purged_at));
}

async function undeleteBackup(id) {
  if (!id) return;
  if (!(await confirmDialog({ title: t("protection.undo"), body: [t("protection.undo_confirm"), id], confirmLabel: t("protection.undo") }))) return;
  try {
    const json = await apiJSON(`/api/v1/backups/${encodeURIComponent(id)}/undelete`, { method: "POST" });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    showToast(t("protection.undone_toast"), "success");
    refreshAll();
  } catch (err) {
    showToast(err.message, "error");
  }
}

// toggleDeletedView switches the backups list between the deleted backups and all.
function toggleDeletedView() {
  const on = lists.backups.filters.status === "deleted";
  setFilter("backups", "status", on ? "" : "deleted", true);
  renderDeletedToggle();
}

function renderDeletedToggle() {
  const btn = document.getElementById("backups-show-deleted");
  if (btn) btn.setAttribute("aria-pressed", lists.backups.filters.status === "deleted" ? "true" : "false");
}

// ---------------------------------------------------------------------------
// Approvals (the two-person rule)
// ---------------------------------------------------------------------------

const PROTECTION_CLOSE_ICON = '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M4 4l8 8M12 4l-8 8"/></svg>';
const PROTECTION_SHIELD_ICON = '<svg class="icon" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M8 1.75 2.75 3.5v4c0 3.1 2.2 5.6 5.25 6.75 3.05-1.15 5.25-3.65 5.25-6.75v-4z"/><path d="m5.75 8 1.6 1.6 2.9-3.1"/></svg>';

// addProtectionUI adds the header's Approvals button and the approvals dialog.
function addProtectionUI() {
  const actions = document.querySelector(".topbar-actions");
  if (actions && !document.getElementById("approvals-btn")) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.id = "approvals-btn";
    btn.className = "btn btn-secondary btn-sm approvals-btn";
    btn.dataset.action = "approvals-open";
    btn.hidden = true;
    btn.innerHTML = `${PROTECTION_SHIELD_ICON}<span class="approvals-label" data-i18n="protection.approvals"></span><span class="badge" id="approvals-count"></span>`;
    btn.querySelector(".approvals-label").textContent = t("protection.approvals");
    const menu = document.getElementById("user-menu");
    actions.insertBefore(btn, menu || null);
  }
  if (!document.getElementById("modal-approvals")) {
    const holder = document.createElement("div");
    holder.innerHTML = `
    <div id="modal-approvals" class="modal-backdrop" aria-hidden="true">
      <div class="modal-card" role="dialog" aria-modal="true" aria-labelledby="approvals-title" aria-describedby="approvals-desc">
        <div class="modal-header">
          <h3 class="modal-title" id="approvals-title" data-i18n="protection.approvals_title"></h3>
          <button type="button" class="modal-close" data-action="close-modal" data-target="modal-approvals" aria-label="Close" data-i18n-aria-label="actions.close">${PROTECTION_CLOSE_ICON}</button>
        </div>
        <div class="modal-body scrollable">
          <p class="muted" id="approvals-desc" data-i18n="protection.approvals_desc"></p>
          <ul class="approvals-list" id="approvals-list"></ul>
        </div>
      </div>
    </div>`;
    while (holder.firstElementChild) document.body.appendChild(holder.firstElementChild);
    setText("approvals-title", t("protection.approvals_title"));
    setText("approvals-desc", t("protection.approvals_desc"));
  }
}

// loadApprovals fetches the waiting requests (administrators only).
async function loadApprovals() {
  if (!auth.user || !can("admin")) {
    protection.approvals = [];
    renderApprovals();
    return;
  }
  const seq = ++protection.seq;
  try {
    const json = await apiJSON("/api/v1/approvals?status=pending");
    if (seq !== protection.seq) return;
    protection.approvals = json.success && Array.isArray(json.data) ? json.data : [];
  } catch (err) {
    protection.approvals = [];
  }
  renderApprovals();
}

function renderApprovals() {
  const btn = document.getElementById("approvals-btn");
  const n = protection.approvals.length;
  if (btn) {
    const show = !!auth.user && can("admin") && (n > 0 || !!protectionSecurity().require_second_approver);
    btn.hidden = !show;
    const label = tf("protection.badge_label", { n: formatCount(n) });
    btn.setAttribute("title", label);
    btn.setAttribute("aria-label", `${t("protection.approvals")}: ${label}`);
    const count = document.getElementById("approvals-count");
    if (count) {
      count.textContent = formatCount(n);
      count.hidden = n === 0;
    }
    const text = btn.querySelector(".approvals-label");
    if (text) text.textContent = t("protection.approvals");
  }
  const list = document.getElementById("approvals-list");
  if (!list) return;
  if (n === 0) {
    list.innerHTML = `<li class="muted">${escapeHtml(t("protection.approvals_empty"))}</li>`;
    return;
  }
  const me = auth.user ? auth.user.id : "";
  list.innerHTML = protection.approvals.map(a => {
    const own = !!me && a.requested_by_user_id === me;
    const who = a.requested_via === "api_key" ? `${a.requested_by} (${t("protection.via_api_key")})` : String(a.requested_by || "");
    const approveTitle = own ? ` title="${escapeHtml(t("protection.own_request"))}"` : "";
    return `<li class="approval-item">
      <p class="approval-summary">${escapeHtml(a.summary || a.action)}</p>
      ${a.reason ? `<p class="muted">${escapeHtml(t("protection.reason"))}: ${escapeHtml(a.reason)}</p>` : ""}
      <p class="muted">${escapeHtml(tf("protection.requested_by", { who, when: protectionWhen(a.created_at) }))} · ${escapeHtml(tf("protection.expires", { when: protectionWhen(a.expires_at) }))}</p>
      <div class="approval-actions">
        <button type="button" class="btn btn-secondary btn-sm" data-action="approval-reject" data-id="${escapeHtml(a.id)}">${escapeHtml(t("protection.reject"))}</button>
        <button type="button" class="btn btn-primary btn-sm" data-action="approval-approve" data-id="${escapeHtml(a.id)}"${own ? " disabled" : ""}${approveTitle}>${escapeHtml(t("protection.approve"))}</button>
      </div>
    </li>`;
  }).join("");
}

async function openApprovals() {
  addProtectionUI();
  renderApprovals();
  openModal("modal-approvals");
  await loadApprovals();
}

async function decideApproval(id, verb) {
  const a = protection.approvals.find(x => x.id === id);
  if (!a) return;
  const ok = await confirmDialog({
    title: t(verb === "approve" ? "protection.approve" : "protection.reject"),
    body: verb === "approve" ? tf("protection.approve_confirm", { summary: String(a.summary || "") }) : t("protection.reject_confirm"),
    confirmLabel: t(verb === "approve" ? "protection.approve" : "protection.reject")
  });
  if (!ok) return;
  try {
    const json = await apiJSON(`/api/v1/approvals/${encodeURIComponent(id)}/${verb}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}"
    });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
    } else if (verb === "reject") {
      showToast(t("protection.rejected_toast"), "success");
    } else if (json.data && json.data.status === "failed") {
      showToast(tf("protection.approve_failed", { error: String(json.data.error || "") }), "error");
    } else {
      showToast(tf("protection.approved_toast", { result: String((json.data && json.data.result) || "") }), "success");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
  await loadApprovals();
  refreshAll();
  loadSettings(false).then(() => { fillSettingsForms(false); renderPendingChanges(); }).catch(() => {});
}

function scheduleApprovalsPoll() {
  clearTimeout(protection.timer);
  protection.timer = setTimeout(() => {
    if (!document.hidden) loadApprovals();
    scheduleApprovalsPoll();
  }, APPROVALS_POLL_MS);
}

// protectionRefresh is called by app.js refreshAll.
function protectionRefresh() {
  addProtectionUI();
  renderDeletedToggle();
  loadApprovals();
}

// ---------------------------------------------------------------------------
// Settings → Security: grace period, two-person rule, pending changes
// ---------------------------------------------------------------------------

// protectionFillSecurity fills the delete protection fields (app.js fillSecurity).
function protectionFillSecurity(sec) {
  const grace = document.getElementById("set-delete-grace");
  if (grace) grace.value = String(Number(sec.delete_grace_days) || GRACE_DEFAULT_DAYS);
  const second = document.getElementById("set-require-second-approver");
  if (second) second.checked = !!sec.require_second_approver;
  setText("set-delete-grace-hint", tf("protection.grace_hint", { min: GRACE_MIN_DAYS, max: GRACE_MAX_DAYS }));
  if (state.settings && Array.isArray(state.settings.pending_changes)) protection.pending = state.settings.pending_changes;
  renderPendingChanges();
}

// protectionCollectSecurity validates the delete protection fields (app.js
// collectSecurity); a FieldError marks the grace period.
function protectionCollectSecurity() {
  const out = {};
  const grace = document.getElementById("set-delete-grace");
  if (grace) {
    const raw = String(grace.value || "").trim();
    const n = Number(raw);
    if (!/^\d+$/.test(raw) || n < GRACE_MIN_DAYS || n > GRACE_MAX_DAYS) {
      throw new FieldError("set-delete-grace", tf("protection.err_grace", { min: GRACE_MIN_DAYS, max: GRACE_MAX_DAYS }));
    }
    out.delete_grace_days = n;
  }
  const second = document.getElementById("set-require-second-approver");
  if (second) out.require_second_approver = second.checked;
  return out;
}

// protectionSettingsSaved explains what a settings save deferred (app.js
// saveSettingsGroup) and returns true when it showed something.
function protectionSettingsSaved(data) {
  if (!data) return false;
  if (Array.isArray(data.pending_changes)) {
    protection.pending = data.pending_changes;
    if (state.settings) state.settings.pending_changes = data.pending_changes;
  }
  renderPendingChanges();
  const approvals = Array.isArray(data.approvals_requested) ? data.approvals_requested : [];
  if (approvals.length > 0) {
    showToast(t("protection.approval_requested_saved"), "info");
    loadApprovals();
    return true;
  }
  const grace = protection.pending.find(c => c.kind === "delete_grace_days");
  if (grace && Number(grace.delete_grace_days) < protectionGraceDays() && Date.parse(grace.created_at) > Date.now() - 60000) {
    showToast(tf("protection.pending_saved", { when: protectionWhen(grace.effective_at) }), "info");
    return true;
  }
  return false;
}

// protectionJobSaved explains a deferred retention after a job save (app.js).
function protectionJobSaved(data) {
  if (!data) return false;
  if (data.approval) {
    showToast(t("protection.retention_approval_saved"), "info");
    loadApprovals();
    return true;
  }
  const c = data.pending_retention;
  if (c && Date.parse(c.created_at) > Date.now() - 60000) {
    showToast(tf("protection.retention_pending_saved", { when: protectionWhen(c.effective_at) }), "info");
    return true;
  }
  return false;
}

// pendingRetentionText describes the values of a retention change.
function pendingRetentionText(c) {
  const parts = [];
  if (c.retention_days != null) parts.push(Number(c.retention_days) > 0 ? tf("retention.days_n", { n: Number(c.retention_days) }) : t("retention.indefinite"));
  if (c.retention_count != null) parts.push(tf("retention.max_n", { n: Number(c.retention_count) }));
  return parts.join(" · ");
}

function renderPendingChanges() {
  const el = document.getElementById("protection-pending");
  if (!el) return;
  const list = protection.pending;
  if (!Array.isArray(list) || list.length === 0) {
    el.innerHTML = `<p class="form-hint">${escapeHtml(t("protection.pending_none"))}</p>`;
    return;
  }
  el.innerHTML = `<ul class="pending-list">${list.map(c => {
    const when = protectionWhen(c.effective_at);
    let text = tf("protection.pending_grace", { days: formatCount(Number(c.delete_grace_days) || 0), when });
    if (c.kind === "retention") text = tf("protection.pending_retention", { job: String(c.job_id || ""), value: pendingRetentionText(c), when });
    if (c.kind === "metadata_backup_retention") text = tf("protection.pending_metadata", { n: formatCount(Number(c.metadata_retention_count) || 0), when });
    if (c.kind === "disable_second_approver") text = tf("protection.pending_disable", { when });
    const by = c.requested_by ? ` <span class="muted">(${escapeHtml(tf("protection.pending_by", { who: String(c.requested_by) }))})</span>` : "";
    return `<li class="pending-item"><span>${escapeHtml(text)}</span>${by}
      <button type="button" class="btn btn-secondary btn-sm" data-action="pending-cancel" data-id="${escapeHtml(c.id)}">${escapeHtml(t("protection.pending_cancel"))}</button></li>`;
  }).join("")}</ul>`;
}

async function loadPendingChanges() {
  try {
    const json = await apiJSON("/api/v1/pending-changes");
    protection.pending = json.success && Array.isArray(json.data) ? json.data : [];
  } catch (err) {
    protection.pending = [];
  }
  renderPendingChanges();
}

async function cancelPendingChange(id) {
  if (!(await confirmDialog({ body: t("protection.pending_cancel_confirm"), confirmLabel: t("protection.pending_cancel") }))) return;
  try {
    const json = await apiJSON(`/api/v1/pending-changes/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    showToast(t("protection.pending_cancelled"), "success");
  } catch (err) {
    showToast(err.message, "error");
  }
  await loadPendingChanges();
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

function setupProtection() {
  addProtectionUI();
  document.addEventListener("click", (e) => {
    const btn = e.target instanceof Element ? e.target.closest("[data-action]") : null;
    if (!btn || btn.disabled) return;
    const id = btn.dataset.id || "";
    switch (btn.dataset.action) {
      case "undelete-backup":
        if (typeof closeRowMenu === "function") closeRowMenu(false);
        undeleteBackup(id);
        break;
      case "backups-show-deleted":
        toggleDeletedView();
        break;
      case "approvals-open":
        openApprovals();
        break;
      case "approval-approve":
        decideApproval(id, "approve");
        break;
      case "approval-reject":
        decideApproval(id, "reject");
        break;
      case "pending-cancel":
        cancelPendingChange(id);
        break;
      default:
    }
  });
  window.addEventListener("hashchange", renderDeletedToggle);
  onLanguageChange(() => {
    renderApprovals();
    renderPendingChanges();
    setText("approvals-title", t("protection.approvals_title"));
    setText("approvals-desc", t("protection.approvals_desc"));
    setText("set-delete-grace-hint", tf("protection.grace_hint", { min: GRACE_MIN_DAYS, max: GRACE_MAX_DAYS }));
  });
  scheduleApprovalsPoll();
}

document.addEventListener("DOMContentLoaded", setupProtection);
