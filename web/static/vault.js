/**
 * MongoRescue dashboard: credentials read from HashiCorp Vault.
 *
 * The "Credentials from Vault" sections of the connection form (a user name and
 * password from a KV v2 secret, or a dynamic user of the database secrets engine)
 * and of the S3 storage target form (access keys from a KV v2 secret, or dynamic
 * keys of the AWS secrets engine). Only the reference is stored: the mount, the
 * path or role and the field names. "Test Vault" resolves the reference on the
 * server (POST /api/v1/vault/test), which never returns a value, only what it
 * found (see docs/security.md).
 *
 * Loaded after app.js and trust.js (mergeTranslations); app.js calls
 * vaultFillConnectionForm(), vaultConnectionPayload(), vaultFormChanged(),
 * vaultConnectionBadges(), vaultFillTargetForm(), vaultTargetPayload() and
 * vaultTargetUnchanged(). Server values reach the DOM only through form values
 * and textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const VAULT_TRANSLATIONS = {
  en: {
    vault: {
      section: "Credentials from Vault",
      conn_intro: "Optional. Read the user name and password from HashiCorp Vault whenever the connection is used, instead of storing them. Nothing read from Vault is stored.",
      target_intro: "Optional. Read the access keys from HashiCorp Vault whenever the target is used, instead of storing them. Nothing read from Vault is stored.",
      not_configured: "Vault is not configured on this server: set MONGORESCUE_VAULT_ADDR and a Vault authentication (VAULT_TOKEN, AppRole or Kubernetes), then restart.",
      use_conn: "Read the user name and password from Vault",
      use_target: "Read the access keys from Vault",
      engine: "Secrets engine",
      engine_kv: "KV version 2 (static secret)",
      engine_database: "Database secrets engine (dynamic user)",
      engine_aws: "AWS secrets engine (dynamic keys)",
      mount: "Mount",
      mount_hint: "Path of the secrets engine, without /v1/. Empty uses the default (secret, database or aws).",
      path: "Secret path",
      path_hint: "Path of the secret below the mount, without data/ (for example mongorescue/production).",
      role: "Role",
      role_hint: "Role that issues the credentials. They are leased: renewed while a run uses them and revoked after it.",
      username_field: "User name field",
      password_field: "Password field",
      access_key_field: "Access key field",
      secret_key_field: "Secret key field",
      session_token_field: "Session token field",
      fields_hint: "Field names in the secret; empty uses the defaults shown. The session token is optional.",
      conn_uri_hint: "Leave the user name and password out of the connection string: Vault supplies both.",
      target_keys_hint: "The access key fields are not used: Vault supplies the keys.",
      test: "Test Vault",
      testing: "Reading from Vault…",
      test_ok_kv: "Found {fields} (version {version}) in {ms} ms. No value is shown.",
      test_ok_lease: "Issued credentials with a {ttl}-second lease ({renewable}) in {ms} ms and revoked them. No value is shown.",
      renewable: "renewable",
      not_renewable: "not renewable",
      test_failed: "Vault test failed: {error}",
      err_path: "Enter the secret path.",
      err_role: "Enter the role.",
      badge: "Vault",
      badge_title: "Credentials are read from Vault when used",
    },
  },
  tr: {
    vault: {
      section: "Vault'tan kimlik bilgileri",
      conn_intro: "İsteğe bağlı. Kullanıcı adı ve parolayı saklamak yerine bağlantı her kullanıldığında HashiCorp Vault'tan okuyun. Vault'tan okunan hiçbir şey saklanmaz.",
      target_intro: "İsteğe bağlı. Erişim anahtarlarını saklamak yerine hedef her kullanıldığında HashiCorp Vault'tan okuyun. Vault'tan okunan hiçbir şey saklanmaz.",
      not_configured: "Bu sunucuda Vault yapılandırılmamış: MONGORESCUE_VAULT_ADDR ve bir Vault kimlik doğrulaması (VAULT_TOKEN, AppRole veya Kubernetes) ayarlayıp yeniden başlatın.",
      use_conn: "Kullanıcı adı ve parolayı Vault'tan oku",
      use_target: "Erişim anahtarlarını Vault'tan oku",
      engine: "Gizli bilgi motoru",
      engine_kv: "KV sürüm 2 (statik gizli bilgi)",
      engine_database: "Veritabanı gizli bilgi motoru (dinamik kullanıcı)",
      engine_aws: "AWS gizli bilgi motoru (dinamik anahtarlar)",
      mount: "Bağlama noktası",
      mount_hint: "Gizli bilgi motorunun yolu, /v1/ olmadan. Boş bırakılırsa varsayılan kullanılır (secret, database veya aws).",
      path: "Gizli bilgi yolu",
      path_hint: "Bağlama noktasının altındaki gizli bilginin yolu, data/ olmadan (örneğin mongorescue/production).",
      role: "Rol",
      role_hint: "Kimlik bilgilerini veren rol. Kiralanırlar: bir çalıştırma kullandıkça yenilenir ve sonrasında iptal edilir.",
      username_field: "Kullanıcı adı alanı",
      password_field: "Parola alanı",
      access_key_field: "Erişim anahtarı alanı",
      secret_key_field: "Gizli anahtar alanı",
      session_token_field: "Oturum belirteci alanı",
      fields_hint: "Gizli bilgideki alan adları; boş bırakılırsa gösterilen varsayılanlar kullanılır. Oturum belirteci isteğe bağlıdır.",
      conn_uri_hint: "Kullanıcı adı ve parolayı bağlantı dizesine yazmayın: ikisini de Vault sağlar.",
      target_keys_hint: "Erişim anahtarı alanları kullanılmaz: anahtarları Vault sağlar.",
      test: "Vault'u test et",
      testing: "Vault'tan okunuyor…",
      test_ok_kv: "{fields} bulundu (sürüm {version}), {ms} ms. Hiçbir değer gösterilmez.",
      test_ok_lease: "{ttl} saniyelik kiralamayla ({renewable}) kimlik bilgileri {ms} ms içinde verildi ve iptal edildi. Hiçbir değer gösterilmez.",
      renewable: "yenilenebilir",
      not_renewable: "yenilenemez",
      test_failed: "Vault testi başarısız: {error}",
      err_path: "Gizli bilgi yolunu girin.",
      err_role: "Rolü girin.",
      badge: "Vault",
      badge_title: "Kimlik bilgileri kullanıldığında Vault'tan okunur",
    },
  },
  de: {
    vault: {
      section: "Zugangsdaten aus Vault",
      conn_intro: "Optional. Benutzername und Passwort bei jeder Verwendung der Verbindung aus HashiCorp Vault lesen, statt sie zu speichern. Nichts, was aus Vault gelesen wird, wird gespeichert.",
      target_intro: "Optional. Die Zugriffsschlüssel bei jeder Verwendung des Ziels aus HashiCorp Vault lesen, statt sie zu speichern. Nichts, was aus Vault gelesen wird, wird gespeichert.",
      not_configured: "Vault ist auf diesem Server nicht konfiguriert: MONGORESCUE_VAULT_ADDR und eine Vault-Authentifizierung (VAULT_TOKEN, AppRole oder Kubernetes) setzen und neu starten.",
      use_conn: "Benutzername und Passwort aus Vault lesen",
      use_target: "Zugriffsschlüssel aus Vault lesen",
      engine: "Secrets Engine",
      engine_kv: "KV Version 2 (statisches Secret)",
      engine_database: "Database Secrets Engine (dynamischer Benutzer)",
      engine_aws: "AWS Secrets Engine (dynamische Schlüssel)",
      mount: "Mount",
      mount_hint: "Pfad der Secrets Engine, ohne /v1/. Leer verwendet den Standard (secret, database oder aws).",
      path: "Secret-Pfad",
      path_hint: "Pfad des Secrets unterhalb des Mounts, ohne data/ (zum Beispiel mongorescue/production).",
      role: "Rolle",
      role_hint: "Rolle, die die Zugangsdaten ausstellt. Sie sind geleast: verlängert, solange ein Lauf sie nutzt, und danach widerrufen.",
      username_field: "Feld des Benutzernamens",
      password_field: "Feld des Passworts",
      access_key_field: "Feld des Zugriffsschlüssels",
      secret_key_field: "Feld des geheimen Schlüssels",
      session_token_field: "Feld des Sitzungstokens",
      fields_hint: "Feldnamen im Secret; leer verwendet die angezeigten Standards. Das Sitzungstoken ist optional.",
      conn_uri_hint: "Benutzername und Passwort nicht in den Connection String schreiben: Vault liefert beides.",
      target_keys_hint: "Die Felder der Zugriffsschlüssel werden nicht verwendet: Vault liefert die Schlüssel.",
      test: "Vault testen",
      testing: "Lese aus Vault…",
      test_ok_kv: "{fields} gefunden (Version {version}) in {ms} ms. Es wird kein Wert angezeigt.",
      test_ok_lease: "Zugangsdaten mit einem Lease von {ttl} Sekunden ({renewable}) in {ms} ms ausgestellt und widerrufen. Es wird kein Wert angezeigt.",
      renewable: "verlängerbar",
      not_renewable: "nicht verlängerbar",
      test_failed: "Vault-Test fehlgeschlagen: {error}",
      err_path: "Geben Sie den Secret-Pfad ein.",
      err_role: "Geben Sie die Rolle ein.",
      badge: "Vault",
      badge_title: "Zugangsdaten werden bei Verwendung aus Vault gelesen",
    },
  },
  es: {
    vault: {
      section: "Credenciales de Vault",
      conn_intro: "Opcional. Leer el usuario y la contraseña de HashiCorp Vault cada vez que se usa la conexión, en lugar de guardarlos. Nada de lo leído de Vault se guarda.",
      target_intro: "Opcional. Leer las claves de acceso de HashiCorp Vault cada vez que se usa el destino, en lugar de guardarlas. Nada de lo leído de Vault se guarda.",
      not_configured: "Vault no está configurado en este servidor: defina MONGORESCUE_VAULT_ADDR y una autenticación de Vault (VAULT_TOKEN, AppRole o Kubernetes) y reinicie.",
      use_conn: "Leer el usuario y la contraseña de Vault",
      use_target: "Leer las claves de acceso de Vault",
      engine: "Motor de secretos",
      engine_kv: "KV versión 2 (secreto estático)",
      engine_database: "Motor de secretos de bases de datos (usuario dinámico)",
      engine_aws: "Motor de secretos de AWS (claves dinámicas)",
      mount: "Montaje",
      mount_hint: "Ruta del motor de secretos, sin /v1/. Vacío usa el valor predeterminado (secret, database o aws).",
      path: "Ruta del secreto",
      path_hint: "Ruta del secreto bajo el montaje, sin data/ (por ejemplo mongorescue/production).",
      role: "Rol",
      role_hint: "Rol que emite las credenciales. Se arriendan: se renuevan mientras una ejecución las usa y se revocan después.",
      username_field: "Campo del usuario",
      password_field: "Campo de la contraseña",
      access_key_field: "Campo de la clave de acceso",
      secret_key_field: "Campo de la clave secreta",
      session_token_field: "Campo del token de sesión",
      fields_hint: "Nombres de los campos en el secreto; vacío usa los valores predeterminados mostrados. El token de sesión es opcional.",
      conn_uri_hint: "No incluya el usuario ni la contraseña en la cadena de conexión: Vault proporciona ambos.",
      target_keys_hint: "Los campos de las claves de acceso no se usan: Vault proporciona las claves.",
      test: "Probar Vault",
      testing: "Leyendo de Vault…",
      test_ok_kv: "Se encontró {fields} (versión {version}) en {ms} ms. No se muestra ningún valor.",
      test_ok_lease: "Se emitieron credenciales con un arrendamiento de {ttl} segundos ({renewable}) en {ms} ms y se revocaron. No se muestra ningún valor.",
      renewable: "renovable",
      not_renewable: "no renovable",
      test_failed: "La prueba de Vault falló: {error}",
      err_path: "Introduzca la ruta del secreto.",
      err_role: "Introduzca el rol.",
      badge: "Vault",
      badge_title: "Las credenciales se leen de Vault al usarse",
    },
  },
  fr: {
    vault: {
      section: "Identifiants depuis Vault",
      conn_intro: "Facultatif. Lire le nom d'utilisateur et le mot de passe dans HashiCorp Vault à chaque utilisation de la connexion, au lieu de les stocker. Rien de ce qui est lu dans Vault n'est stocké.",
      target_intro: "Facultatif. Lire les clés d'accès dans HashiCorp Vault à chaque utilisation de la cible, au lieu de les stocker. Rien de ce qui est lu dans Vault n'est stocké.",
      not_configured: "Vault n'est pas configuré sur ce serveur : définissez MONGORESCUE_VAULT_ADDR et une authentification Vault (VAULT_TOKEN, AppRole ou Kubernetes), puis redémarrez.",
      use_conn: "Lire le nom d'utilisateur et le mot de passe dans Vault",
      use_target: "Lire les clés d'accès dans Vault",
      engine: "Moteur de secrets",
      engine_kv: "KV version 2 (secret statique)",
      engine_database: "Moteur de secrets de bases de données (utilisateur dynamique)",
      engine_aws: "Moteur de secrets AWS (clés dynamiques)",
      mount: "Point de montage",
      mount_hint: "Chemin du moteur de secrets, sans /v1/. Vide utilise la valeur par défaut (secret, database ou aws).",
      path: "Chemin du secret",
      path_hint: "Chemin du secret sous le point de montage, sans data/ (par exemple mongorescue/production).",
      role: "Rôle",
      role_hint: "Rôle qui émet les identifiants. Ils sont loués : renouvelés tant qu'une exécution les utilise et révoqués ensuite.",
      username_field: "Champ du nom d'utilisateur",
      password_field: "Champ du mot de passe",
      access_key_field: "Champ de la clé d'accès",
      secret_key_field: "Champ de la clé secrète",
      session_token_field: "Champ du jeton de session",
      fields_hint: "Noms des champs du secret ; vide utilise les valeurs par défaut affichées. Le jeton de session est facultatif.",
      conn_uri_hint: "N'indiquez ni nom d'utilisateur ni mot de passe dans la chaîne de connexion : Vault fournit les deux.",
      target_keys_hint: "Les champs des clés d'accès ne sont pas utilisés : Vault fournit les clés.",
      test: "Tester Vault",
      testing: "Lecture depuis Vault…",
      test_ok_kv: "{fields} trouvé(s) (version {version}) en {ms} ms. Aucune valeur n'est affichée.",
      test_ok_lease: "Identifiants émis avec un bail de {ttl} secondes ({renewable}) en {ms} ms, puis révoqués. Aucune valeur n'est affichée.",
      renewable: "renouvelable",
      not_renewable: "non renouvelable",
      test_failed: "Échec du test de Vault : {error}",
      err_path: "Saisissez le chemin du secret.",
      err_role: "Saisissez le rôle.",
      badge: "Vault",
      badge_title: "Les identifiants sont lus dans Vault à l'utilisation",
    },
  },
  zh: {
    vault: {
      section: "来自 Vault 的凭据",
      conn_intro: "可选。每次使用连接时从 HashiCorp Vault 读取用户名和密码，而不是保存它们。从 Vault 读取的任何内容都不会被保存。",
      target_intro: "可选。每次使用目标时从 HashiCorp Vault 读取访问密钥，而不是保存它们。从 Vault 读取的任何内容都不会被保存。",
      not_configured: "此服务器未配置 Vault：请设置 MONGORESCUE_VAULT_ADDR 和 Vault 身份验证（VAULT_TOKEN、AppRole 或 Kubernetes），然后重新启动。",
      use_conn: "从 Vault 读取用户名和密码",
      use_target: "从 Vault 读取访问密钥",
      engine: "机密引擎",
      engine_kv: "KV 版本 2（静态机密）",
      engine_database: "数据库机密引擎（动态用户）",
      engine_aws: "AWS 机密引擎（动态密钥）",
      mount: "挂载路径",
      mount_hint: "机密引擎的路径，不含 /v1/。留空则使用默认值（secret、database 或 aws）。",
      path: "机密路径",
      path_hint: "挂载路径下的机密路径，不含 data/（例如 mongorescue/production）。",
      role: "角色",
      role_hint: "签发凭据的角色。凭据带有租约：在运行使用期间续期，运行结束后撤销。",
      username_field: "用户名字段",
      password_field: "密码字段",
      access_key_field: "访问密钥字段",
      secret_key_field: "秘密密钥字段",
      session_token_field: "会话令牌字段",
      fields_hint: "机密中的字段名；留空则使用显示的默认值。会话令牌是可选的。",
      conn_uri_hint: "连接字符串中不要包含用户名和密码：两者都由 Vault 提供。",
      target_keys_hint: "不使用访问密钥字段：密钥由 Vault 提供。",
      test: "测试 Vault",
      testing: "正在从 Vault 读取…",
      test_ok_kv: "已找到 {fields}（版本 {version}），用时 {ms} 毫秒。不显示任何值。",
      test_ok_lease: "已签发租约为 {ttl} 秒（{renewable}）的凭据，用时 {ms} 毫秒，并已撤销。不显示任何值。",
      renewable: "可续期",
      not_renewable: "不可续期",
      test_failed: "Vault 测试失败：{error}",
      err_path: "请输入机密路径。",
      err_role: "请输入角色。",
      badge: "Vault",
      badge_title: "使用时从 Vault 读取凭据",
    },
  },
  ja: {
    vault: {
      section: "Vault からの認証情報",
      conn_intro: "任意。ユーザー名とパスワードを保存せず、接続を使うたびに HashiCorp Vault から読み取ります。Vault から読み取った内容は一切保存されません。",
      target_intro: "任意。アクセスキーを保存せず、ターゲットを使うたびに HashiCorp Vault から読み取ります。Vault から読み取った内容は一切保存されません。",
      not_configured: "このサーバーでは Vault が設定されていません。MONGORESCUE_VAULT_ADDR と Vault の認証（VAULT_TOKEN、AppRole または Kubernetes）を設定して再起動してください。",
      use_conn: "ユーザー名とパスワードを Vault から読み取る",
      use_target: "アクセスキーを Vault から読み取る",
      engine: "シークレットエンジン",
      engine_kv: "KV バージョン 2（静的シークレット）",
      engine_database: "データベースシークレットエンジン（動的ユーザー）",
      engine_aws: "AWS シークレットエンジン（動的キー）",
      mount: "マウント",
      mount_hint: "シークレットエンジンのパス（/v1/ なし）。空の場合は既定値（secret、database または aws）を使います。",
      path: "シークレットのパス",
      path_hint: "マウント配下のシークレットのパス（data/ なし。例: mongorescue/production）。",
      role: "ロール",
      role_hint: "認証情報を発行するロール。リース付きで、実行が使っている間は更新され、終了後に取り消されます。",
      username_field: "ユーザー名のフィールド",
      password_field: "パスワードのフィールド",
      access_key_field: "アクセスキーのフィールド",
      secret_key_field: "シークレットキーのフィールド",
      session_token_field: "セッショントークンのフィールド",
      fields_hint: "シークレット内のフィールド名。空の場合は表示された既定値を使います。セッショントークンは任意です。",
      conn_uri_hint: "接続文字列にユーザー名とパスワードを含めないでください。どちらも Vault が提供します。",
      target_keys_hint: "アクセスキーのフィールドは使われません。キーは Vault が提供します。",
      test: "Vault をテスト",
      testing: "Vault から読み取り中…",
      test_ok_kv: "{fields} が見つかりました（バージョン {version}、{ms} ms）。値は表示されません。",
      test_ok_lease: "{ttl} 秒のリース（{renewable}）で認証情報を {ms} ms で発行し、取り消しました。値は表示されません。",
      renewable: "更新可能",
      not_renewable: "更新不可",
      test_failed: "Vault のテストに失敗しました: {error}",
      err_path: "シークレットのパスを入力してください。",
      err_role: "ロールを入力してください。",
      badge: "Vault",
      badge_title: "認証情報は使用時に Vault から読み取られます",
    },
  },
  ru: {
    vault: {
      section: "Учётные данные из Vault",
      conn_intro: "Необязательно. Считывать имя пользователя и пароль из HashiCorp Vault при каждом использовании подключения вместо их хранения. Ничего из прочитанного в Vault не сохраняется.",
      target_intro: "Необязательно. Считывать ключи доступа из HashiCorp Vault при каждом использовании хранилища вместо их хранения. Ничего из прочитанного в Vault не сохраняется.",
      not_configured: "Vault не настроен на этом сервере: задайте MONGORESCUE_VAULT_ADDR и аутентификацию Vault (VAULT_TOKEN, AppRole или Kubernetes) и перезапустите.",
      use_conn: "Считывать имя пользователя и пароль из Vault",
      use_target: "Считывать ключи доступа из Vault",
      engine: "Механизм секретов",
      engine_kv: "KV версии 2 (статический секрет)",
      engine_database: "Механизм секретов баз данных (динамический пользователь)",
      engine_aws: "Механизм секретов AWS (динамические ключи)",
      mount: "Точка монтирования",
      mount_hint: "Путь механизма секретов без /v1/. Если пусто, используется значение по умолчанию (secret, database или aws).",
      path: "Путь секрета",
      path_hint: "Путь секрета под точкой монтирования без data/ (например, mongorescue/production).",
      role: "Роль",
      role_hint: "Роль, выдающая учётные данные. Они выдаются в аренду: продлеваются, пока их использует запуск, и отзываются после него.",
      username_field: "Поле имени пользователя",
      password_field: "Поле пароля",
      access_key_field: "Поле ключа доступа",
      secret_key_field: "Поле секретного ключа",
      session_token_field: "Поле токена сеанса",
      fields_hint: "Имена полей в секрете; если пусто, используются показанные значения по умолчанию. Токен сеанса необязателен.",
      conn_uri_hint: "Не указывайте имя пользователя и пароль в строке подключения: оба предоставляет Vault.",
      target_keys_hint: "Поля ключей доступа не используются: ключи предоставляет Vault.",
      test: "Проверить Vault",
      testing: "Чтение из Vault…",
      test_ok_kv: "Найдено: {fields} (версия {version}) за {ms} мс. Значения не показываются.",
      test_ok_lease: "Учётные данные с арендой на {ttl} с ({renewable}) выданы за {ms} мс и отозваны. Значения не показываются.",
      renewable: "продлеваемой",
      not_renewable: "непродлеваемой",
      test_failed: "Проверка Vault не удалась: {error}",
      err_path: "Введите путь секрета.",
      err_role: "Введите роль.",
      badge: "Vault",
      badge_title: "Учётные данные считываются из Vault при использовании",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(VAULT_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], VAULT_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Shared
// ---------------------------------------------------------------------------

// vaultState holds whether the server reads credentials from Vault (GET
// /api/v1/vault) and the reference each form loaded, to tell a changed form.
const vaultState = { configured: null, connSnapshot: "", targetSnapshot: "" };

function vaultEl(id) {
  return document.getElementById(id);
}

function vaultVal(id) {
  const el = vaultEl(id);
  return el ? el.value.trim() : "";
}

// vaultLoadStatus asks once whether Vault is configured; a failure counts as not.
async function vaultLoadStatus() {
  if (vaultState.configured !== null) return vaultState.configured;
  try {
    const json = await apiJSON("/api/v1/vault");
    vaultState.configured = !!(json.success && json.data && json.data.configured);
  } catch (_) {
    vaultState.configured = false;
  }
  return vaultState.configured;
}

// vaultForm describes the section of one form: its element ID prefix, the
// engines it offers and the kv-v2 fields it names.
const VAULT_FORMS = {
  connection: { prefix: "connection-vault", use: "connection", fields: ["username_field", "password_field"] },
  storage: { prefix: "storage-vault", use: "s3", fields: ["access_key_field", "secret_key_field", "session_token_field"] },
};

const VAULT_FIELD_IDS = {
  username_field: "user-field",
  password_field: "password-field",
  access_key_field: "access-field",
  secret_key_field: "secret-field",
  session_token_field: "token-field",
};

function vaultEnabled(form) {
  const box = vaultEl(`${form.prefix}-enabled`);
  return !!(box && box.checked);
}

// vaultShowEngine shows the inputs of the selected engine.
function vaultShowEngine(form) {
  const engine = vaultVal(`${form.prefix}-engine`) || "kv-v2";
  const section = vaultEl(form.prefix);
  if (!section) return;
  section.querySelectorAll("[data-vault-engine]").forEach(el => {
    el.hidden = el.dataset.vaultEngine === "kv-v2" ? engine !== "kv-v2" : engine === "kv-v2";
  });
  const fields = vaultEl(`${form.prefix}-fields`);
  if (fields) fields.hidden = !vaultEnabled(form);
  if (form.use === "s3") {
    // The stored keys are not used with Vault.
    const keys = vaultEl("storage-access-key");
    const row = keys ? keys.closest(".form-row") : null;
    if (row) row.hidden = vaultEnabled(form);
    const hint = vaultEl("storage-secret-hint");
    if (hint) hint.hidden = vaultEnabled(form);
  }
}

// vaultFill shows reference ref (from a stored connection or target) in form.
async function vaultFill(form, source, ref) {
  const section = vaultEl(form.prefix);
  if (!section) return;
  ref = ref || {};
  const on = source === "vault";
  const box = vaultEl(`${form.prefix}-enabled`);
  if (box) box.checked = on;
  const set = (id, v) => { const el = vaultEl(id); if (el) el.value = v || ""; };
  set(`${form.prefix}-engine`, ref.engine || "kv-v2");
  set(`${form.prefix}-mount`, ref.mount);
  set(`${form.prefix}-path`, ref.path);
  set(`${form.prefix}-role`, ref.role);
  form.fields.forEach(f => set(`${form.prefix}-${VAULT_FIELD_IDS[f]}`, ref[f]));
  vaultResult(form, "", "");
  vaultShowEngine(form);
  section.open = on;
  const configured = await vaultLoadStatus();
  const off = vaultEl(`${form.prefix}-off`);
  if (off) off.hidden = configured;
  // A stored reference stays editable (so it can be turned off) without Vault.
  if (box) box.disabled = !configured && !on;
}

// vaultRef returns the reference of form, or throws FieldError when incomplete.
function vaultRef(form) {
  const engine = vaultVal(`${form.prefix}-engine`) || "kv-v2";
  const ref = { engine };
  const mount = vaultVal(`${form.prefix}-mount`);
  if (mount) ref.mount = mount;
  if (engine === "kv-v2") {
    ref.path = vaultVal(`${form.prefix}-path`);
    if (!ref.path) throw new FieldError(`${form.prefix}-path`, t("vault.err_path"));
    form.fields.forEach(f => {
      const v = vaultVal(`${form.prefix}-${VAULT_FIELD_IDS[f]}`);
      if (v) ref[f] = v;
    });
  } else {
    ref.role = vaultVal(`${form.prefix}-role`);
    if (!ref.role) throw new FieldError(`${form.prefix}-role`, t("vault.err_role"));
  }
  return ref;
}

// vaultPayload returns the credential fields of form's payload.
function vaultPayload(form) {
  if (!vaultEl(form.prefix)) return {};
  if (!vaultEnabled(form)) return { credential_source: "stored" };
  return { credential_source: "vault", vault: vaultRef(form) };
}

function vaultResult(form, kind, text) {
  const el = vaultEl(`${form.prefix}-result`);
  if (!el) return;
  el.className = kind ? `test-result test-${kind}` : "test-result";
  el.textContent = text;
}

// vaultDescribe renders a test result, which never holds a value.
function vaultDescribe(data) {
  const ms = Math.round(Number(data.latency_ms) || 0);
  if (!data.ok) return tf("vault.test_failed", { error: truncate(data.error || "", 200) });
  if (data.engine === "kv-v2") {
    return tf("vault.test_ok_kv", { fields: (data.fields || []).join(", "), version: data.version || "?", ms });
  }
  return tf("vault.test_ok_lease", { ttl: data.lease_ttl_seconds || 0, renewable: t(data.renewable ? "vault.renewable" : "vault.not_renewable"), ms });
}

// vaultTest resolves the reference of form on the server.
async function vaultTest(form) {
  let ref;
  try {
    ref = vaultRef(form);
  } catch (err) {
    if (!(err instanceof FieldError)) throw err;
    vaultResult(form, "fail", err.message);
    const field = vaultEl(err.field);
    if (field) field.focus();
    return;
  }
  const btn = vaultEl(`${form.prefix}-test`);
  if (btn) btn.disabled = true;
  vaultResult(form, "pending", t("vault.testing"));
  try {
    const json = await apiJSON("/api/v1/vault/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ use: form.use, vault: ref }),
    });
    const data = json.success ? (json.data || {}) : { ok: false, error: json.error || "" };
    vaultResult(form, data.ok ? "ok" : "fail", vaultDescribe(data));
  } catch (err) {
    vaultResult(form, "fail", tf("vault.test_failed", { error: err.message }));
  } finally {
    if (btn) btn.disabled = false;
  }
}

// vaultSnapshot is the form's reference as JSON ("" when invalid).
function vaultSnapshot(form) {
  try {
    return JSON.stringify(vaultPayload(form));
  } catch (_) {
    return "";
  }
}

// ---------------------------------------------------------------------------
// Connection form
// ---------------------------------------------------------------------------

// vaultFillConnectionForm shows connection c (null for a new one) in the section.
function vaultFillConnectionForm(c) {
  const form = VAULT_FORMS.connection;
  vaultFill(form, c && c.credential_source, c && c.vault).then(() => {
    vaultState.connSnapshot = vaultSnapshot(form);
  });
  vaultState.connSnapshot = vaultSnapshot(form);
}

// vaultConnectionPayload returns the credential fields of the connection payload,
// or null (after showing why) when the reference is incomplete.
function vaultConnectionPayload() {
  const form = VAULT_FORMS.connection;
  try {
    return vaultPayload(form);
  } catch (err) {
    if (!(err instanceof FieldError)) throw err;
    vaultEl(form.prefix).open = true;
    showToast(err.message, "error");
    const field = vaultEl(err.field);
    if (field) field.focus();
    return null;
  }
}

// vaultFormChanged reports whether the connection's section differs from what it
// loaded, so the form's test sends it instead of testing the stored connection.
function vaultFormChanged() {
  return !!vaultEl(VAULT_FORMS.connection.prefix) && vaultSnapshot(VAULT_FORMS.connection) !== vaultState.connSnapshot;
}

// vaultConnectionBadges returns the badge of the connections table for c.
function vaultConnectionBadges(c) {
  return c && c.credential_source === "vault" ? ` ${statusBadge("neutral", t("vault.badge"), t("vault.badge_title"))}` : "";
}

// ---------------------------------------------------------------------------
// Storage target form
// ---------------------------------------------------------------------------

// vaultFillTargetForm shows the S3 settings s3 of a target in the section.
function vaultFillTargetForm(s3) {
  const form = VAULT_FORMS.storage;
  vaultFill(form, s3 && s3.credential_source, s3 && s3.vault);
}

// vaultTargetPayload returns the credential fields of the s3 payload; with Vault
// the stored keys are cleared. It throws FieldError for an incomplete reference.
function vaultTargetPayload() {
  const out = vaultPayload(VAULT_FORMS.storage);
  if (out.credential_source === "vault") {
    out.access_key_id = "";
    out.secret_access_key = "";
  }
  return out;
}

// vaultTargetUnchanged reports whether the stored s3 settings a and the payload's
// b read their keys from the same place.
function vaultTargetUnchanged(a, b) {
  const src = x => (x && x.credential_source === "vault" ? "vault" : "stored");
  if (src(a) !== src(b)) return false;
  if (src(a) !== "vault") return true;
  const ra = a.vault || {};
  const rb = b.vault || {};
  return ["engine", "mount", "path", "role", "access_key_field", "secret_key_field", "session_token_field"]
    .every(k => !rb[k] || String(ra[k] || "") === String(rb[k]));
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

function setupVault() {
  Object.values(VAULT_FORMS).forEach(form => {
    if (!vaultEl(form.prefix)) return;
    const changed = () => {
      vaultShowEngine(form);
      vaultResult(form, "", "");
      if (form.use === "connection" && typeof resetConnectionTest === "function") resetConnectionTest();
      if (form.use === "s3" && typeof resetStorageTest === "function") resetStorageTest();
    };
    const box = vaultEl(`${form.prefix}-enabled`);
    if (box) box.addEventListener("change", changed);
    const engine = vaultEl(`${form.prefix}-engine`);
    if (engine) engine.addEventListener("change", changed);
    vaultEl(form.prefix).querySelectorAll("input[type=text]").forEach(el => el.addEventListener("input", changed));
    const btn = vaultEl(`${form.prefix}-test`);
    if (btn) btn.addEventListener("click", () => { vaultTest(form); });
  });
}

document.addEventListener("DOMContentLoaded", setupVault);
