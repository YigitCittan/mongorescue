/**
 * MongoRescue dashboard: key rotation (Settings → Security → Key rotation).
 *
 * Three actions, each behind a confirmation that explains its effects:
 * rotating secret.key (POST /api/v1/security/rotate-secret-key, password
 * confirmed; everyone signs in again), rotating the backup encryption key
 * (POST /api/v1/encryption/rotate, optionally re-encrypting existing backups)
 * and rotating the S3 credentials of a storage target
 * (POST /api/v1/storage-targets/{id}/rotate-credentials, probed first).
 *
 * Loaded after app.js, trust.js (mergeTranslations), bulk.js (confirmDialog) and
 * protection.js (protectionApprovalPending). Server values reach the DOM only
 * through textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const KEYROT_TRANSLATIONS = {
  en: {
    keyrot: {
      title: "Key rotation",
      desc: "Replace secret.key, the backup encryption key or storage credentials, for example after a suspected leak. Every rotation is audited and sends the security.key_rotated event.",
      secret_title: "secret.key",
      secret_desc: "Seals every stored credential. Rotating it re-encrypts them all under a new key in one transaction.",
      secret_fp: "Current fingerprint: {fp}",
      secret_env: "Set by MONGORESCUE_SECRET_KEY: rotate it by hand (see docs/encryption.md).",
      secret_prev: "secret.key.previous is kept: include it in the next recovery kit to open older metadata snapshots.",
      secret_btn: "Rotate secret.key",
      secret_confirm_title: "Rotate secret.key?",
      secret_confirm_1: "Every stored credential is re-encrypted under a new key. The old key no longer opens the database.",
      secret_confirm_2: "Everyone, you included, is signed out and has to sign in again.",
      secret_confirm_3: "Metadata snapshots taken before now still need the old key: download a new recovery kit afterwards and keep the old one, or include secret.key.previous.",
      password: "Your password",
      secret_done: "secret.key rotated. Sign in again.",
      enc_title: "Backup encryption key",
      enc_desc: "New backups use a new key; the old one is retired and still restores older backups.",
      enc_reencrypt: "Also re-encrypt existing backups (their old archives are deleted when the delete grace period ends)",
      enc_btn: "Rotate encryption key",
      enc_confirm_title: "Rotate the backup encryption key?",
      enc_confirm_x25519: "A new key pair is generated and replaces the current one among the recipients. The old private key is retired and kept, so older backups stay restorable.",
      enc_confirm_pass: "Enter the new passphrase. The old one is retired and kept, so older backups stay restorable.",
      enc_confirm_reencrypt: "Existing backups are re-encrypted in the background; each old archive is deleted when the delete grace period ends.",
      passphrase: "New passphrase",
      enc_done: "Encryption key rotated.",
      enc_off: "Backup encryption is off.",
      job: "Re-encryption: {status}, {done} of {total} done, {failed} failed",
      creds_title: "Storage credentials",
      creds_desc: "New S3 keys are tested with a write, read, list and delete on a temporary object before they replace the current ones.",
      creds_target: "Storage target",
      creds_access: "Access key ID",
      creds_secret: "Secret access key",
      creds_btn: "Test and rotate",
      creds_none: "There is no S3 storage target.",
      creds_confirm_title: "Rotate the credentials of {name}?",
      creds_confirm: "The new credentials replace the current ones once every probe passed. Revoke the old key at your provider afterwards.",
      creds_done: "Credentials of {name} rotated.",
      creds_failed: "The new credentials failed the {step} probe; nothing was changed.",
      creds_required: "Enter the access key ID and the secret access key.",
      prev_warning: "The metadata snapshots sealed with the replaced secret key were deleted. Download a recovery kit (secret.key.previous is then deleted), or delete secret.key.previous from the data directory."
    }
  },
  tr: {
    keyrot: {
      title: "Anahtar yenileme",
      desc: "secret.key'i, yedek şifreleme anahtarını veya depolama kimlik bilgilerini değiştirin (ör. sızıntı şüphesinde). Her yenileme denetim kaydına yazılır ve security.key_rotated olayını gönderir.",
      secret_title: "secret.key",
      secret_desc: "Saklanan tüm kimlik bilgilerini mühürler. Yenilemek hepsini tek bir işlemde yeni bir anahtarla yeniden şifreler.",
      secret_fp: "Geçerli parmak izi: {fp}",
      secret_env: "MONGORESCUE_SECRET_KEY ile ayarlanmış: elle yenileyin (bkz. docs/encryption.md).",
      secret_prev: "secret.key.previous saklanıyor: eski meta veri anlık görüntülerini açmak için bir sonraki kurtarma kitine ekleyin.",
      secret_btn: "secret.key'i yenile",
      secret_confirm_title: "secret.key yenilensin mi?",
      secret_confirm_1: "Saklanan tüm kimlik bilgileri yeni bir anahtarla yeniden şifrelenir. Eski anahtar veritabanını artık açamaz.",
      secret_confirm_2: "Siz dahil herkesin oturumu kapanır ve yeniden giriş yapması gerekir.",
      secret_confirm_3: "Bundan önce alınan meta veri anlık görüntüleri hâlâ eski anahtarı gerektirir: ardından yeni bir kurtarma kiti indirin ve eskisini saklayın ya da secret.key.previous'ı ekleyin.",
      password: "Parolanız",
      secret_done: "secret.key yenilendi. Yeniden giriş yapın.",
      enc_title: "Yedek şifreleme anahtarı",
      enc_desc: "Yeni yedekler yeni bir anahtar kullanır; eskisi emekliye ayrılır ve eski yedekleri geri yüklemeye devam eder.",
      enc_reencrypt: "Mevcut yedekleri de yeniden şifrele (eski arşivler silme bekleme süresi bitince silinir)",
      enc_btn: "Şifreleme anahtarını yenile",
      enc_confirm_title: "Yedek şifreleme anahtarı yenilensin mi?",
      enc_confirm_x25519: "Yeni bir anahtar çifti üretilir ve alıcılar arasında geçerli olanın yerini alır. Eski özel anahtar emekliye ayrılıp saklanır, böylece eski yedekler geri yüklenebilir kalır.",
      enc_confirm_pass: "Yeni parola ifadesini girin. Eskisi emekliye ayrılıp saklanır, böylece eski yedekler geri yüklenebilir kalır.",
      enc_confirm_reencrypt: "Mevcut yedekler arka planda yeniden şifrelenir; her eski arşiv silme bekleme süresi bitince silinir.",
      passphrase: "Yeni parola ifadesi",
      enc_done: "Şifreleme anahtarı yenilendi.",
      enc_off: "Yedek şifreleme kapalı.",
      job: "Yeniden şifreleme: {status}, {total} yedeğin {done} tanesi bitti, {failed} başarısız",
      creds_title: "Depolama kimlik bilgileri",
      creds_desc: "Yeni S3 anahtarları, mevcutların yerini almadan önce geçici bir nesnede yazma, okuma, listeleme ve silme ile sınanır.",
      creds_target: "Depolama hedefi",
      creds_access: "Erişim anahtarı kimliği",
      creds_secret: "Gizli erişim anahtarı",
      creds_btn: "Sına ve yenile",
      creds_none: "S3 depolama hedefi yok.",
      creds_confirm_title: "{name} kimlik bilgileri yenilensin mi?",
      creds_confirm: "Yeni kimlik bilgileri tüm sınamalar geçince mevcutların yerini alır. Ardından eski anahtarı sağlayıcınızda iptal edin.",
      creds_done: "{name} kimlik bilgileri yenilendi.",
      creds_failed: "Yeni kimlik bilgileri {step} sınamasında başarısız oldu; hiçbir şey değişmedi.",
      creds_required: "Erişim anahtarı kimliğini ve gizli erişim anahtarını girin.",
      prev_warning: "Değiştirilen gizli anahtarla mühürlü meta veri anlık görüntüleri silindi. Bir kurtarma kiti indirin (secret.key.previous ardından silinir) ya da secret.key.previous'ı veri dizininden silin."
    }
  },
  de: {
    keyrot: {
      title: "Schlüsselrotation",
      desc: "Ersetzen Sie secret.key, den Backup-Verschlüsselungsschlüssel oder Speicherzugangsdaten, etwa nach einem vermuteten Leck. Jede Rotation wird protokolliert und sendet das Ereignis security.key_rotated.",
      secret_title: "secret.key",
      secret_desc: "Versiegelt alle gespeicherten Zugangsdaten. Eine Rotation verschlüsselt sie alle in einer Transaktion mit einem neuen Schlüssel.",
      secret_fp: "Aktueller Fingerabdruck: {fp}",
      secret_env: "Über MONGORESCUE_SECRET_KEY gesetzt: manuell rotieren (siehe docs/encryption.md).",
      secret_prev: "secret.key.previous wird aufbewahrt: Nehmen Sie ihn in das nächste Recovery-Kit auf, um ältere Metadaten-Snapshots zu öffnen.",
      secret_btn: "secret.key rotieren",
      secret_confirm_title: "secret.key rotieren?",
      secret_confirm_1: "Alle gespeicherten Zugangsdaten werden mit einem neuen Schlüssel neu verschlüsselt. Der alte Schlüssel öffnet die Datenbank nicht mehr.",
      secret_confirm_2: "Alle, auch Sie, werden abgemeldet und müssen sich neu anmelden.",
      secret_confirm_3: "Metadaten-Snapshots von vorher brauchen weiterhin den alten Schlüssel: Laden Sie danach ein neues Recovery-Kit herunter und behalten Sie das alte, oder nehmen Sie secret.key.previous auf.",
      password: "Ihr Passwort",
      secret_done: "secret.key rotiert. Bitte neu anmelden.",
      enc_title: "Backup-Verschlüsselungsschlüssel",
      enc_desc: "Neue Backups verwenden einen neuen Schlüssel; der alte wird stillgelegt und stellt ältere Backups weiter wieder her.",
      enc_reencrypt: "Bestehende Backups ebenfalls neu verschlüsseln (alte Archive werden nach Ablauf der Löschfrist entfernt)",
      enc_btn: "Verschlüsselungsschlüssel rotieren",
      enc_confirm_title: "Backup-Verschlüsselungsschlüssel rotieren?",
      enc_confirm_x25519: "Ein neues Schlüsselpaar wird erzeugt und ersetzt das aktuelle unter den Empfängern. Der alte private Schlüssel wird stillgelegt und aufbewahrt, ältere Backups bleiben wiederherstellbar.",
      enc_confirm_pass: "Geben Sie die neue Passphrase ein. Die alte wird stillgelegt und aufbewahrt, ältere Backups bleiben wiederherstellbar.",
      enc_confirm_reencrypt: "Bestehende Backups werden im Hintergrund neu verschlüsselt; jedes alte Archiv wird nach Ablauf der Löschfrist entfernt.",
      passphrase: "Neue Passphrase",
      enc_done: "Verschlüsselungsschlüssel rotiert.",
      enc_off: "Die Backup-Verschlüsselung ist aus.",
      job: "Neuverschlüsselung: {status}, {done} von {total} erledigt, {failed} fehlgeschlagen",
      creds_title: "Speicherzugangsdaten",
      creds_desc: "Neue S3-Schlüssel werden mit Schreiben, Lesen, Auflisten und Löschen eines temporären Objekts geprüft, bevor sie die aktuellen ersetzen.",
      creds_target: "Speicherziel",
      creds_access: "Zugriffsschlüssel-ID",
      creds_secret: "Geheimer Zugriffsschlüssel",
      creds_btn: "Prüfen und rotieren",
      creds_none: "Es gibt kein S3-Speicherziel.",
      creds_confirm_title: "Zugangsdaten von {name} rotieren?",
      creds_confirm: "Die neuen Zugangsdaten ersetzen die aktuellen, sobald alle Prüfungen bestanden sind. Widerrufen Sie danach den alten Schlüssel beim Anbieter.",
      creds_done: "Zugangsdaten von {name} rotiert.",
      creds_failed: "Die neuen Zugangsdaten sind bei der Prüfung „{step}“ gescheitert; nichts wurde geändert.",
      creds_required: "Geben Sie Zugriffsschlüssel-ID und geheimen Zugriffsschlüssel ein.",
      prev_warning: "Die mit dem ersetzten geheimen Schlüssel versiegelten Metadaten-Snapshots wurden gelöscht. Laden Sie ein Recovery-Kit herunter (secret.key.previous wird dann gelöscht) oder löschen Sie secret.key.previous aus dem Datenverzeichnis."
    }
  },
  es: {
    keyrot: {
      title: "Rotación de claves",
      desc: "Sustituya secret.key, la clave de cifrado de las copias o las credenciales de almacenamiento, por ejemplo ante una posible filtración. Cada rotación se audita y envía el evento security.key_rotated.",
      secret_title: "secret.key",
      secret_desc: "Sella todas las credenciales guardadas. Rotarla las vuelve a cifrar todas con una clave nueva en una sola transacción.",
      secret_fp: "Huella actual: {fp}",
      secret_env: "Definida por MONGORESCUE_SECRET_KEY: rótela a mano (vea docs/encryption.md).",
      secret_prev: "Se conserva secret.key.previous: inclúyala en el próximo kit de recuperación para abrir instantáneas de metadatos antiguas.",
      secret_btn: "Rotar secret.key",
      secret_confirm_title: "¿Rotar secret.key?",
      secret_confirm_1: "Todas las credenciales guardadas se vuelven a cifrar con una clave nueva. La clave antigua ya no abre la base de datos.",
      secret_confirm_2: "Todos, usted incluido, cierran sesión y deben volver a iniciarla.",
      secret_confirm_3: "Las instantáneas de metadatos anteriores siguen necesitando la clave antigua: descargue después un kit de recuperación nuevo y conserve el anterior, o incluya secret.key.previous.",
      password: "Su contraseña",
      secret_done: "secret.key rotada. Inicie sesión de nuevo.",
      enc_title: "Clave de cifrado de las copias",
      enc_desc: "Las copias nuevas usan una clave nueva; la antigua se retira y sigue restaurando las copias anteriores.",
      enc_reencrypt: "Volver a cifrar también las copias existentes (sus archivos antiguos se borran al terminar el periodo de gracia)",
      enc_btn: "Rotar clave de cifrado",
      enc_confirm_title: "¿Rotar la clave de cifrado de las copias?",
      enc_confirm_x25519: "Se genera un par de claves nuevo que sustituye al actual entre los destinatarios. La clave privada antigua se retira y se conserva, así las copias anteriores siguen siendo restaurables.",
      enc_confirm_pass: "Introduzca la frase de contraseña nueva. La antigua se retira y se conserva, así las copias anteriores siguen siendo restaurables.",
      enc_confirm_reencrypt: "Las copias existentes se vuelven a cifrar en segundo plano; cada archivo antiguo se borra al terminar el periodo de gracia.",
      passphrase: "Frase de contraseña nueva",
      enc_done: "Clave de cifrado rotada.",
      enc_off: "El cifrado de las copias está desactivado.",
      job: "Nuevo cifrado: {status}, {done} de {total} hechas, {failed} fallidas",
      creds_title: "Credenciales de almacenamiento",
      creds_desc: "Las claves S3 nuevas se prueban escribiendo, leyendo, listando y borrando un objeto temporal antes de sustituir a las actuales.",
      creds_target: "Destino de almacenamiento",
      creds_access: "ID de clave de acceso",
      creds_secret: "Clave de acceso secreta",
      creds_btn: "Probar y rotar",
      creds_none: "No hay ningún destino S3.",
      creds_confirm_title: "¿Rotar las credenciales de {name}?",
      creds_confirm: "Las credenciales nuevas sustituyen a las actuales cuando pasan todas las pruebas. Después revoque la clave antigua en su proveedor.",
      creds_done: "Credenciales de {name} rotadas.",
      creds_failed: "Las credenciales nuevas fallaron la prueba {step}; no se cambió nada.",
      creds_required: "Introduzca el ID de clave de acceso y la clave de acceso secreta.",
      prev_warning: "Se borraron las instantáneas de metadatos selladas con la clave secreta sustituida. Descargue un kit de recuperación (entonces se borra secret.key.previous) o borre secret.key.previous del directorio de datos."
    }
  },
  fr: {
    keyrot: {
      title: "Rotation des clés",
      desc: "Remplacez secret.key, la clé de chiffrement des sauvegardes ou les identifiants de stockage, par exemple après une fuite présumée. Chaque rotation est auditée et envoie l'événement security.key_rotated.",
      secret_title: "secret.key",
      secret_desc: "Scelle tous les identifiants enregistrés. La rotation les rechiffre tous avec une nouvelle clé en une seule transaction.",
      secret_fp: "Empreinte actuelle : {fp}",
      secret_env: "Définie par MONGORESCUE_SECRET_KEY : faites la rotation à la main (voir docs/encryption.md).",
      secret_prev: "secret.key.previous est conservée : ajoutez-la au prochain kit de récupération pour ouvrir les anciens instantanés de métadonnées.",
      secret_btn: "Faire tourner secret.key",
      secret_confirm_title: "Faire tourner secret.key ?",
      secret_confirm_1: "Tous les identifiants enregistrés sont rechiffrés avec une nouvelle clé. L'ancienne clé n'ouvre plus la base.",
      secret_confirm_2: "Tout le monde, vous compris, est déconnecté et doit se reconnecter.",
      secret_confirm_3: "Les instantanés de métadonnées antérieurs exigent toujours l'ancienne clé : téléchargez ensuite un nouveau kit de récupération et gardez l'ancien, ou incluez secret.key.previous.",
      password: "Votre mot de passe",
      secret_done: "secret.key a été remplacée. Reconnectez-vous.",
      enc_title: "Clé de chiffrement des sauvegardes",
      enc_desc: "Les nouvelles sauvegardes utilisent une nouvelle clé ; l'ancienne est retirée et restaure toujours les sauvegardes antérieures.",
      enc_reencrypt: "Rechiffrer aussi les sauvegardes existantes (leurs anciennes archives sont supprimées à la fin du délai de grâce)",
      enc_btn: "Faire tourner la clé de chiffrement",
      enc_confirm_title: "Faire tourner la clé de chiffrement des sauvegardes ?",
      enc_confirm_x25519: "Une nouvelle paire de clés est générée et remplace l'actuelle parmi les destinataires. L'ancienne clé privée est retirée et conservée : les sauvegardes antérieures restent restaurables.",
      enc_confirm_pass: "Saisissez la nouvelle phrase secrète. L'ancienne est retirée et conservée : les sauvegardes antérieures restent restaurables.",
      enc_confirm_reencrypt: "Les sauvegardes existantes sont rechiffrées en arrière-plan ; chaque ancienne archive est supprimée à la fin du délai de grâce.",
      passphrase: "Nouvelle phrase secrète",
      enc_done: "Clé de chiffrement remplacée.",
      enc_off: "Le chiffrement des sauvegardes est désactivé.",
      job: "Rechiffrement : {status}, {done} sur {total} faites, {failed} en échec",
      creds_title: "Identifiants de stockage",
      creds_desc: "Les nouvelles clés S3 sont testées par l'écriture, la lecture, le listage et la suppression d'un objet temporaire avant de remplacer les actuelles.",
      creds_target: "Cible de stockage",
      creds_access: "ID de clé d'accès",
      creds_secret: "Clé d'accès secrète",
      creds_btn: "Tester et remplacer",
      creds_none: "Il n'y a aucune cible S3.",
      creds_confirm_title: "Remplacer les identifiants de {name} ?",
      creds_confirm: "Les nouveaux identifiants remplacent les actuels une fois tous les tests réussis. Révoquez ensuite l'ancienne clé chez votre fournisseur.",
      creds_done: "Identifiants de {name} remplacés.",
      creds_failed: "Les nouveaux identifiants ont échoué au test {step} ; rien n'a été modifié.",
      creds_required: "Saisissez l'ID de clé d'accès et la clé d'accès secrète.",
      prev_warning: "Les instantanés de métadonnées scellés avec la clé secrète remplacée ont été supprimés. Téléchargez un kit de récupération (secret.key.previous est alors supprimée) ou supprimez secret.key.previous du répertoire de données."
    }
  },
  zh: {
    keyrot: {
      title: "密钥轮换",
      desc: "更换 secret.key、备份加密密钥或存储凭据，例如在怀疑泄露之后。每次轮换都会记入审计并发送 security.key_rotated 事件。",
      secret_title: "secret.key",
      secret_desc: "用于封存所有已存储的凭据。轮换会在一个事务中用新密钥重新加密全部凭据。",
      secret_fp: "当前指纹：{fp}",
      secret_env: "由 MONGORESCUE_SECRET_KEY 设置：请手动轮换（见 docs/encryption.md）。",
      secret_prev: "已保留 secret.key.previous：将其放入下一个恢复套件，以打开较早的元数据快照。",
      secret_btn: "轮换 secret.key",
      secret_confirm_title: "轮换 secret.key？",
      secret_confirm_1: "所有已存储的凭据都会用新密钥重新加密。旧密钥将无法再打开数据库。",
      secret_confirm_2: "所有人（包括您）都会被登出，需要重新登录。",
      secret_confirm_3: "此前的元数据快照仍需要旧密钥：之后请下载新的恢复套件并保留旧的，或包含 secret.key.previous。",
      password: "您的密码",
      secret_done: "secret.key 已轮换。请重新登录。",
      enc_title: "备份加密密钥",
      enc_desc: "新备份使用新密钥；旧密钥被停用，仍可恢复较早的备份。",
      enc_reencrypt: "同时重新加密现有备份（旧归档在删除宽限期结束后删除）",
      enc_btn: "轮换加密密钥",
      enc_confirm_title: "轮换备份加密密钥？",
      enc_confirm_x25519: "将生成新的密钥对，并在接收者中替换当前密钥。旧私钥被停用并保留，因此较早的备份仍可恢复。",
      enc_confirm_pass: "请输入新的口令。旧口令被停用并保留，因此较早的备份仍可恢复。",
      enc_confirm_reencrypt: "现有备份将在后台重新加密；每个旧归档在删除宽限期结束后删除。",
      passphrase: "新口令",
      enc_done: "加密密钥已轮换。",
      enc_off: "备份加密已关闭。",
      job: "重新加密：{status}，已完成 {done}/{total}，失败 {failed}",
      creds_title: "存储凭据",
      creds_desc: "新的 S3 密钥在替换当前密钥前，会在临时对象上测试写入、读取、列出和删除。",
      creds_target: "存储目标",
      creds_access: "访问密钥 ID",
      creds_secret: "私有访问密钥",
      creds_btn: "测试并轮换",
      creds_none: "没有 S3 存储目标。",
      creds_confirm_title: "轮换 {name} 的凭据？",
      creds_confirm: "所有测试通过后，新凭据将替换当前凭据。之后请在提供商处吊销旧密钥。",
      creds_done: "{name} 的凭据已轮换。",
      creds_failed: "新凭据未通过 {step} 测试；未做任何更改。",
      creds_required: "请输入访问密钥 ID 和私有访问密钥。",
      prev_warning: "用被替换的密钥封存的元数据快照已删除。请下载恢复套件（随后会删除 secret.key.previous），或从数据目录中删除 secret.key.previous。"
    }
  },
  ja: {
    keyrot: {
      title: "鍵のローテーション",
      desc: "漏えいが疑われるときなどに、secret.key、バックアップ暗号化キー、ストレージ認証情報を置き換えます。すべてのローテーションは監査され、security.key_rotated イベントを送信します。",
      secret_title: "secret.key",
      secret_desc: "保存されたすべての認証情報を封印します。ローテーションすると、1 つのトランザクションですべてを新しい鍵で再暗号化します。",
      secret_fp: "現在のフィンガープリント: {fp}",
      secret_env: "MONGORESCUE_SECRET_KEY で設定されています。手動でローテーションしてください（docs/encryption.md を参照）。",
      secret_prev: "secret.key.previous を保持しています。古いメタデータスナップショットを開くには、次のリカバリーキットに含めてください。",
      secret_btn: "secret.key をローテーション",
      secret_confirm_title: "secret.key をローテーションしますか？",
      secret_confirm_1: "保存されたすべての認証情報が新しい鍵で再暗号化されます。古い鍵ではデータベースを開けなくなります。",
      secret_confirm_2: "あなたを含む全員がサインアウトされ、再度サインインが必要です。",
      secret_confirm_3: "これ以前のメタデータスナップショットには古い鍵が必要です。この後、新しいリカバリーキットをダウンロードして古いキットも保管するか、secret.key.previous を含めてください。",
      password: "パスワード",
      secret_done: "secret.key をローテーションしました。再度サインインしてください。",
      enc_title: "バックアップ暗号化キー",
      enc_desc: "新しいバックアップは新しいキーを使います。古いキーは退役し、以前のバックアップの復元に引き続き使われます。",
      enc_reencrypt: "既存のバックアップも再暗号化する（古いアーカイブは削除猶予期間の終了後に削除）",
      enc_btn: "暗号化キーをローテーション",
      enc_confirm_title: "バックアップ暗号化キーをローテーションしますか？",
      enc_confirm_x25519: "新しいキーペアを生成し、受信者の中で現在のキーと置き換えます。古い秘密鍵は退役して保持されるため、以前のバックアップは復元可能なままです。",
      enc_confirm_pass: "新しいパスフレーズを入力してください。古いものは退役して保持されるため、以前のバックアップは復元可能なままです。",
      enc_confirm_reencrypt: "既存のバックアップはバックグラウンドで再暗号化され、古いアーカイブは削除猶予期間の終了後に削除されます。",
      passphrase: "新しいパスフレーズ",
      enc_done: "暗号化キーをローテーションしました。",
      enc_off: "バックアップ暗号化はオフです。",
      job: "再暗号化: {status}、{total} 件中 {done} 件完了、{failed} 件失敗",
      creds_title: "ストレージ認証情報",
      creds_desc: "新しい S3 キーは、現在のキーと置き換える前に一時オブジェクトの書き込み・読み取り・一覧・削除でテストされます。",
      creds_target: "ストレージターゲット",
      creds_access: "アクセスキー ID",
      creds_secret: "シークレットアクセスキー",
      creds_btn: "テストしてローテーション",
      creds_none: "S3 ストレージターゲットがありません。",
      creds_confirm_title: "{name} の認証情報をローテーションしますか？",
      creds_confirm: "すべてのテストに合格すると、新しい認証情報が現在のものと置き換わります。その後、プロバイダーで古いキーを無効にしてください。",
      creds_done: "{name} の認証情報をローテーションしました。",
      creds_failed: "新しい認証情報は {step} テストに失敗しました。何も変更されていません。",
      creds_required: "アクセスキー ID とシークレットアクセスキーを入力してください。",
      prev_warning: "置き換えた秘密鍵で封印されたメタデータスナップショットは削除されました。リカバリーキットをダウンロードする（その後 secret.key.previous は削除されます）か、データディレクトリから secret.key.previous を削除してください。"
    }
  },
  ru: {
    keyrot: {
      title: "Ротация ключей",
      desc: "Замените secret.key, ключ шифрования резервных копий или учётные данные хранилища, например при подозрении на утечку. Каждая ротация попадает в аудит и отправляет событие security.key_rotated.",
      secret_title: "secret.key",
      secret_desc: "Запечатывает все сохранённые учётные данные. Ротация перешифровывает их новым ключом в одной транзакции.",
      secret_fp: "Текущий отпечаток: {fp}",
      secret_env: "Задан через MONGORESCUE_SECRET_KEY: выполните ротацию вручную (см. docs/encryption.md).",
      secret_prev: "secret.key.previous сохранён: добавьте его в следующий комплект восстановления, чтобы открыть старые снимки метаданных.",
      secret_btn: "Ротировать secret.key",
      secret_confirm_title: "Ротировать secret.key?",
      secret_confirm_1: "Все сохранённые учётные данные будут перешифрованы новым ключом. Старый ключ больше не откроет базу.",
      secret_confirm_2: "Все, включая вас, выйдут из системы и должны будут войти снова.",
      secret_confirm_3: "Снимкам метаданных, сделанным раньше, по-прежнему нужен старый ключ: после ротации скачайте новый комплект восстановления и сохраните старый или включите secret.key.previous.",
      password: "Ваш пароль",
      secret_done: "secret.key заменён. Войдите снова.",
      enc_title: "Ключ шифрования резервных копий",
      enc_desc: "Новые копии используют новый ключ; старый выводится из обращения и по-прежнему восстанавливает прежние копии.",
      enc_reencrypt: "Также перешифровать существующие копии (старые архивы удаляются по окончании льготного срока)",
      enc_btn: "Ротировать ключ шифрования",
      enc_confirm_title: "Ротировать ключ шифрования резервных копий?",
      enc_confirm_x25519: "Будет создана новая пара ключей, она заменит текущую среди получателей. Старый закрытый ключ выводится из обращения и сохраняется, поэтому прежние копии остаются восстановимыми.",
      enc_confirm_pass: "Введите новую парольную фразу. Старая выводится из обращения и сохраняется, поэтому прежние копии остаются восстановимыми.",
      enc_confirm_reencrypt: "Существующие копии перешифровываются в фоне; каждый старый архив удаляется по окончании льготного срока.",
      passphrase: "Новая парольная фраза",
      enc_done: "Ключ шифрования заменён.",
      enc_off: "Шифрование резервных копий выключено.",
      job: "Перешифрование: {status}, готово {done} из {total}, ошибок {failed}",
      creds_title: "Учётные данные хранилища",
      creds_desc: "Новые ключи S3 проверяются записью, чтением, листингом и удалением временного объекта, прежде чем заменить текущие.",
      creds_target: "Хранилище",
      creds_access: "ID ключа доступа",
      creds_secret: "Секретный ключ доступа",
      creds_btn: "Проверить и заменить",
      creds_none: "Нет хранилища S3.",
      creds_confirm_title: "Заменить учётные данные {name}?",
      creds_confirm: "Новые учётные данные заменят текущие, когда пройдут все проверки. Затем отзовите старый ключ у провайдера.",
      creds_done: "Учётные данные {name} заменены.",
      creds_failed: "Новые учётные данные не прошли проверку «{step}»; ничего не изменено.",
      creds_required: "Введите ID ключа доступа и секретный ключ доступа.",
      prev_warning: "Снимки метаданных, запечатанные заменённым секретным ключом, удалены. Скачайте комплект восстановления (после этого secret.key.previous удаляется) или удалите secret.key.previous из каталога данных."
    }
  }
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(KEYROT_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], KEYROT_TRANSLATIONS[lang]);
  });
}

if (typeof ROLE_ACTION_SCOPES === "object") {
  Object.assign(ROLE_ACTION_SCOPES, {
    "keyrot-secret": "admin",
    "keyrot-encryption": "admin",
    "keyrot-credentials": "admin"
  });
}

// ---------------------------------------------------------------------------
// Section
// ---------------------------------------------------------------------------

const keyrot = { status: null, job: null };

// WARNING_PREVIOUS_KEY is the settings warning that secret.key.previous is no
// longer needed (settings.WarningPreviousKey).
const WARNING_PREVIOUS_KEY = "previous_secret_key";

// keyrotRenderWarnings refreshes the section when the warnings change (app.js
// renderWarnings).
function keyrotRenderWarnings() {
  if (document.getElementById("keyrot-section")) renderKeyRotation();
}

// keyrotEl creates an element with a class and text.
function keyrotEl(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

// keyrotButton creates an admin action button.
function keyrotButton(action, label, danger) {
  const b = keyrotEl("button", danger ? "btn btn-danger btn-sm" : "btn btn-secondary btn-sm", label);
  b.type = "button";
  b.dataset.action = action;
  return b;
}

// keyrotGroup creates a fieldset with a title and a description.
function keyrotGroup(title, desc) {
  const fs = keyrotEl("fieldset", "settings-group");
  fs.appendChild(keyrotEl("legend", "settings-group-title", title));
  fs.appendChild(keyrotEl("p", "form-hint", desc));
  return fs;
}

// keyrotS3Targets returns the S3 storage targets the dashboard loaded.
function keyrotS3Targets() {
  return (state.storageTargets || []).filter(x => x && x.type === "s3");
}

// renderKeyRotation (re)builds the Key rotation section of Settings → Security.
function renderKeyRotation() {
  const panel = document.getElementById("settings-security");
  if (!panel) return;
  let box = document.getElementById("keyrot-section");
  if (!box) {
    box = keyrotEl("div", "settings-form");
    box.id = "keyrot-section";
    panel.appendChild(box);
  }
  box.textContent = "";
  box.appendChild(keyrotEl("h3", "section-title", t("keyrot.title")));
  box.appendChild(keyrotEl("p", "section-desc", t("keyrot.desc")));

  const sk = (keyrot.status && keyrot.status.secret_key) || null;
  const secret = keyrotGroup(t("keyrot.secret_title"), t("keyrot.secret_desc"));
  if (sk && sk.fingerprint) secret.appendChild(keyrotEl("p", "form-hint", tf("keyrot.secret_fp", { fp: sk.fingerprint })));
  if (sk && sk.from_env) secret.appendChild(keyrotEl("p", "notice notice-warn", t("keyrot.secret_env")));
  if (sk && sk.previous_key_kept) secret.appendChild(keyrotEl("p", "form-hint", t("keyrot.secret_prev")));
  const warnings = state.settings && Array.isArray(state.settings.warnings) ? state.settings.warnings : [];
  if (warnings.some(w => w && w.id === WARNING_PREVIOUS_KEY)) {
    secret.appendChild(keyrotEl("p", "notice notice-warn", t("keyrot.prev_warning")));
  }
  const sb = keyrotButton("keyrot-secret", t("keyrot.secret_btn"), true);
  sb.disabled = !sk || !!sk.from_env;
  secret.appendChild(sb);
  box.appendChild(secret);

  const enc = (state.settings && state.settings.encryption) || {};
  const encGroup = keyrotGroup(t("keyrot.enc_title"), t("keyrot.enc_desc"));
  if (!enc.enabled) encGroup.appendChild(keyrotEl("p", "form-hint", t("keyrot.enc_off")));
  const label = keyrotEl("label", "checkbox-label");
  const check = document.createElement("input");
  check.type = "checkbox";
  check.id = "keyrot-reencrypt";
  label.appendChild(check);
  label.appendChild(document.createTextNode(" " + t("keyrot.enc_reencrypt")));
  encGroup.appendChild(label);
  const eb = keyrotButton("keyrot-encryption", t("keyrot.enc_btn"), false);
  eb.disabled = !enc.enabled;
  encGroup.appendChild(eb);
  const job = keyrot.job;
  if (job && job.status) {
    encGroup.appendChild(keyrotEl("p", "form-hint", tf("keyrot.job", {
      status: job.status, done: formatCount(job.done || 0), total: formatCount(job.total || 0), failed: formatCount(job.failed || 0)
    })));
  }
  box.appendChild(encGroup);

  const creds = keyrotGroup(t("keyrot.creds_title"), t("keyrot.creds_desc"));
  const targets = keyrotS3Targets();
  if (!targets.length) {
    creds.appendChild(keyrotEl("p", "form-hint", t("keyrot.creds_none")));
  } else {
    const field = (id, text, input) => {
      const g = keyrotEl("div", "form-group");
      const l = keyrotEl("label", "form-label", text);
      l.htmlFor = id;
      input.id = id;
      input.className = "form-input";
      g.appendChild(l);
      g.appendChild(input);
      creds.appendChild(g);
    };
    const select = document.createElement("select");
    targets.forEach(tg => {
      const o = document.createElement("option");
      o.value = tg.id;
      o.textContent = tg.name;
      select.appendChild(o);
    });
    field("keyrot-target", t("keyrot.creds_target"), select);
    const access = document.createElement("input");
    access.type = "text";
    access.autocomplete = "off";
    field("keyrot-access", t("keyrot.creds_access"), access);
    const secretInput = document.createElement("input");
    secretInput.type = "password";
    secretInput.autocomplete = "new-password";
    field("keyrot-secret-key", t("keyrot.creds_secret"), secretInput);
    creds.appendChild(keyrotButton("keyrot-credentials", t("keyrot.creds_btn"), false));
  }
  box.appendChild(creds);
}

// loadKeyRotation fetches the key state and the re-encryption job.
async function loadKeyRotation() {
  if (!auth.user) return;
  try {
    const [st, job] = await Promise.all([
      apiJSON("/api/v1/security/key-rotation"),
      apiJSON("/api/v1/encryption/reencryption")
    ]);
    if (st.success) keyrot.status = st.data || null;
    if (job.success) keyrot.job = job.data || null;
  } catch (err) {
    // The section shows what it has; errors surface on the actions.
  }
  renderKeyRotation();
}

// rotateSecretKey confirms and rotates secret.key; everyone signs in again.
async function rotateSecretKey() {
  const password = await confirmDialog({
    title: t("keyrot.secret_confirm_title"),
    body: [t("keyrot.secret_confirm_1"), t("keyrot.secret_confirm_2"), t("keyrot.secret_confirm_3")],
    danger: true,
    confirmLabel: t("keyrot.secret_btn"),
    prompt: { label: t("keyrot.password"), secret: true, maxLength: 1024 }
  });
  if (!password) return;
  try {
    const json = await apiJSON("/api/v1/security/rotate-secret-key", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ current_password: password })
    });
    if (typeof protectionApprovalPending === "function" && protectionApprovalPending(json)) return;
    if (!json.success) throw new Error(json.error);
    showToast(t("keyrot.secret_done"), "success");
    handleUnauthorized();
  } catch (err) {
    showToast(err.message, "error");
  }
}

// rotateEncryptionKey confirms and rotates the backup encryption key.
async function rotateEncryptionKey() {
  const enc = (state.settings && state.settings.encryption) || {};
  const reencrypt = !!(document.getElementById("keyrot-reencrypt") || {}).checked;
  const passMode = enc.mode === "passphrase";
  const body = [passMode ? t("keyrot.enc_confirm_pass") : t("keyrot.enc_confirm_x25519")];
  if (reencrypt) body.push(t("keyrot.enc_confirm_reencrypt"));
  const opts = { title: t("keyrot.enc_confirm_title"), body, confirmLabel: t("keyrot.enc_btn") };
  if (passMode) opts.prompt = { label: t("keyrot.passphrase"), secret: true, maxLength: 1024 };
  const answer = await confirmDialog(opts);
  if (!answer) return;
  const payload = { reencrypt };
  if (passMode) payload.passphrase = answer;
  try {
    const json = await apiJSON("/api/v1/encryption/rotate", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload)
    });
    if (!json.success) throw new Error(json.error);
    showToast(t("keyrot.enc_done"), "success");
    if (typeof loadSettings === "function") await loadSettings(true);
    await loadKeyRotation();
  } catch (err) {
    showToast(err.message, "error");
  }
}

// rotateCredentials confirms, probes and swaps the S3 credentials of a target.
async function rotateCredentials() {
  const id = (document.getElementById("keyrot-target") || {}).value || "";
  const access = ((document.getElementById("keyrot-access") || {}).value || "").trim();
  const secretInput = document.getElementById("keyrot-secret-key");
  const secret = ((secretInput || {}).value || "").trim();
  const target = keyrotS3Targets().find(x => x.id === id);
  if (!target) return;
  if (!access || !secret) {
    showToast(t("keyrot.creds_required"), "error");
    return;
  }
  const ok = await confirmDialog({
    title: tf("keyrot.creds_confirm_title", { name: target.name }),
    body: [t("keyrot.creds_confirm")],
    confirmLabel: t("keyrot.creds_btn")
  });
  if (!ok) return;
  try {
    const json = await apiJSON(`/api/v1/storage-targets/${encodeURIComponent(id)}/rotate-credentials`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ access_key_id: access, secret_access_key: secret })
    });
    if (json.httpStatus === 422 && json.data && Array.isArray(json.data.steps)) {
      const failed = json.data.steps.find(s => !s.ok);
      throw new Error(tf("keyrot.creds_failed", { step: failed ? String(failed.name) : "?" }) + (failed && failed.error ? " " + String(failed.error) : ""));
    }
    if (!json.success) throw new Error(json.error);
    if (secretInput) secretInput.value = "";
    showToast(tf("keyrot.creds_done", { name: target.name }), "success");
    if (typeof loadStorageTargets === "function") await loadStorageTargets();
    renderKeyRotation();
  } catch (err) {
    showToast(err.message, "error");
  }
}

function setupKeyRotation() {
  document.addEventListener("click", (e) => {
    const btn = e.target instanceof Element ? e.target.closest("[data-action]") : null;
    if (!btn || btn.disabled) return;
    switch (btn.dataset.action) {
      case "keyrot-secret":
        rotateSecretKey();
        break;
      case "keyrot-encryption":
        rotateEncryptionKey();
        break;
      case "keyrot-credentials":
        rotateCredentials();
        break;
      case "settings-section":
        if (btn.dataset.section === "security") loadKeyRotation();
        break;
      default:
    }
  });
  onLanguageChange(renderKeyRotation);
}

document.addEventListener("DOMContentLoaded", setupKeyRotation);
