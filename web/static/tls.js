/**
 * MongoRescue dashboard: TLS material of a connection.
 *
 * The connection form's "TLS / client certificate" section: a CA bundle, an x509
 * client certificate and key (pasted or loaded from a file), the key's password,
 * and the two switches that loosen certificate checks, each with a warning.
 * The server masks the client key and its password in responses; the form sends
 * the mask back to keep them (see docs/connections.md).
 *
 * Loaded after app.js and trust.js (mergeTranslations); app.js calls
 * tlsFillConnectionForm(), tlsConnectionPayload(), tlsFormChanged() and
 * tlsConnectionBadges(). Server values reach the DOM only through form values,
 * textContent and escapeHtml.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const TLS_TRANSLATIONS = {
  en: {
    tls: {
      section: "TLS / client certificate",
      intro: "Optional. For servers with a private certificate authority or x509 authentication. Leave empty to use the connection string alone; Atlas and public CAs need nothing here.",
      ca: "CA certificate (PEM)",
      ca_hint: "The certificates of the authority that signed the server certificate. Empty uses the system's trusted roots.",
      cert: "Client certificate (PEM)",
      cert_hint: "For x509 authentication (authMechanism=MONGODB-X509 in the URI). Needs the key below.",
      key: "Client private key (PEM)",
      key_hint: "Stored encrypted and never shown again. An encrypted (PKCS#8) key needs its password.",
      key_stored: "Stored. Leave empty to keep it.",
      password: "Key password",
      password_hint: "Only for an encrypted key.",
      password_stored: "Stored. Leave empty to keep it.",
      file: "Load from file",
      file_too_large: "The file is too large (at most 64 KiB).",
      hostnames: "Allow invalid hostnames",
      hostnames_hint: "Skips the check that the server certificate names the host. The certificate chain is still verified.",
      insecure: "Do not verify the server certificate",
      insecure_hint: "Accepts any certificate: anyone on the network path can read and change the traffic, credentials included.",
      insecure_confirm: "Turn off certificate verification? Anyone on the network path could then intercept this connection, its credentials and your data.",
      warn_hostnames: "Hostname checking is off: a certificate the same CA issued for another server is accepted.",
      warn_insecure: "Certificate verification is off: this connection can be intercepted. Use it only for tests.",
      badge_insecure: "TLS not verified",
      badge_hostnames: "Hostname not checked",
      badge_x509: "x509 certificate",
    },
  },
  tr: {
    tls: {
      section: "TLS / istemci sertifikası",
      intro: "İsteğe bağlı. Özel bir sertifika yetkilisi veya x509 kimlik doğrulaması kullanan sunucular için. Yalnızca bağlantı dizesini kullanmak için boş bırakın; Atlas ve genel CA'lar için burada bir şey gerekmez.",
      ca: "CA sertifikası (PEM)",
      ca_hint: "Sunucu sertifikasını imzalayan yetkilinin sertifikaları. Boş bırakılırsa sistemin güvenilen kökleri kullanılır.",
      cert: "İstemci sertifikası (PEM)",
      cert_hint: "x509 kimlik doğrulaması için (URI'de authMechanism=MONGODB-X509). Aşağıdaki anahtarı gerektirir.",
      key: "İstemci özel anahtarı (PEM)",
      key_hint: "Şifrelenmiş olarak saklanır ve bir daha gösterilmez. Şifrelenmiş (PKCS#8) bir anahtar parolasını gerektirir.",
      key_stored: "Kayıtlı. Korumak için boş bırakın.",
      password: "Anahtar parolası",
      password_hint: "Yalnızca şifrelenmiş bir anahtar için.",
      password_stored: "Kayıtlı. Korumak için boş bırakın.",
      file: "Dosyadan yükle",
      file_too_large: "Dosya çok büyük (en fazla 64 KiB).",
      hostnames: "Geçersiz ana bilgisayar adlarına izin ver",
      hostnames_hint: "Sunucu sertifikasının ana bilgisayarı adlandırdığı denetimini atlar. Sertifika zinciri yine doğrulanır.",
      insecure: "Sunucu sertifikasını doğrulama",
      insecure_hint: "Her sertifikayı kabul eder: ağ yolundaki herkes kimlik bilgileri dahil trafiği okuyabilir ve değiştirebilir.",
      insecure_confirm: "Sertifika doğrulaması kapatılsın mı? Bu durumda ağ yolundaki herkes bu bağlantıyı, kimlik bilgilerini ve verilerinizi ele geçirebilir.",
      warn_hostnames: "Ana bilgisayar adı denetimi kapalı: aynı CA'nın başka bir sunucu için verdiği sertifika kabul edilir.",
      warn_insecure: "Sertifika doğrulaması kapalı: bu bağlantı ele geçirilebilir. Yalnızca testler için kullanın.",
      badge_insecure: "TLS doğrulanmıyor",
      badge_hostnames: "Ana bilgisayar adı denetlenmiyor",
      badge_x509: "x509 sertifikası",
    },
  },
  de: {
    tls: {
      section: "TLS / Client-Zertifikat",
      intro: "Optional. Für Server mit einer eigenen Zertifizierungsstelle oder x509-Authentifizierung. Leer lassen, um nur den Connection String zu verwenden; Atlas und öffentliche CAs brauchen hier nichts.",
      ca: "CA-Zertifikat (PEM)",
      ca_hint: "Die Zertifikate der Stelle, die das Serverzertifikat signiert hat. Leer verwendet die vertrauenswürdigen Stammzertifikate des Systems.",
      cert: "Client-Zertifikat (PEM)",
      cert_hint: "Für x509-Authentifizierung (authMechanism=MONGODB-X509 in der URI). Benötigt den Schlüssel unten.",
      key: "Privater Client-Schlüssel (PEM)",
      key_hint: "Wird verschlüsselt gespeichert und nie wieder angezeigt. Ein verschlüsselter (PKCS#8) Schlüssel braucht sein Passwort.",
      key_stored: "Gespeichert. Leer lassen, um ihn zu behalten.",
      password: "Schlüsselpasswort",
      password_hint: "Nur für einen verschlüsselten Schlüssel.",
      password_stored: "Gespeichert. Leer lassen, um es zu behalten.",
      file: "Aus Datei laden",
      file_too_large: "Die Datei ist zu groß (höchstens 64 KiB).",
      hostnames: "Ungültige Hostnamen zulassen",
      hostnames_hint: "Überspringt die Prüfung, ob das Serverzertifikat den Host nennt. Die Zertifikatskette wird weiterhin geprüft.",
      insecure: "Serverzertifikat nicht prüfen",
      insecure_hint: "Akzeptiert jedes Zertifikat: Jeder auf dem Netzwerkpfad kann den Verkehr samt Zugangsdaten lesen und verändern.",
      insecure_confirm: "Zertifikatsprüfung abschalten? Dann kann jeder auf dem Netzwerkpfad diese Verbindung, ihre Zugangsdaten und Ihre Daten abfangen.",
      warn_hostnames: "Die Hostnamenprüfung ist aus: Ein Zertifikat, das dieselbe CA für einen anderen Server ausgestellt hat, wird akzeptiert.",
      warn_insecure: "Die Zertifikatsprüfung ist aus: Diese Verbindung kann abgefangen werden. Nur für Tests verwenden.",
      badge_insecure: "TLS nicht geprüft",
      badge_hostnames: "Hostname nicht geprüft",
      badge_x509: "x509-Zertifikat",
    },
  },
  es: {
    tls: {
      section: "TLS / certificado de cliente",
      intro: "Opcional. Para servidores con una autoridad de certificación privada o autenticación x509. Déjelo vacío para usar solo la cadena de conexión; Atlas y las CA públicas no necesitan nada aquí.",
      ca: "Certificado de la CA (PEM)",
      ca_hint: "Los certificados de la autoridad que firmó el certificado del servidor. Vacío usa las raíces de confianza del sistema.",
      cert: "Certificado de cliente (PEM)",
      cert_hint: "Para autenticación x509 (authMechanism=MONGODB-X509 en la URI). Necesita la clave de abajo.",
      key: "Clave privada del cliente (PEM)",
      key_hint: "Se guarda cifrada y no se vuelve a mostrar. Una clave cifrada (PKCS#8) necesita su contraseña.",
      key_stored: "Guardada. Déjelo vacío para conservarla.",
      password: "Contraseña de la clave",
      password_hint: "Solo para una clave cifrada.",
      password_stored: "Guardada. Déjelo vacío para conservarla.",
      file: "Cargar desde archivo",
      file_too_large: "El archivo es demasiado grande (64 KiB como máximo).",
      hostnames: "Permitir nombres de host no válidos",
      hostnames_hint: "Omite la comprobación de que el certificado del servidor nombra el host. La cadena de certificados se sigue verificando.",
      insecure: "No verificar el certificado del servidor",
      insecure_hint: "Acepta cualquier certificado: cualquiera en la ruta de red puede leer y modificar el tráfico, credenciales incluidas.",
      insecure_confirm: "¿Desactivar la verificación de certificados? Cualquiera en la ruta de red podría interceptar esta conexión, sus credenciales y sus datos.",
      warn_hostnames: "La comprobación del nombre de host está desactivada: se acepta un certificado que la misma CA emitió para otro servidor.",
      warn_insecure: "La verificación de certificados está desactivada: esta conexión puede ser interceptada. Úsela solo para pruebas.",
      badge_insecure: "TLS sin verificar",
      badge_hostnames: "Host sin comprobar",
      badge_x509: "Certificado x509",
    },
  },
  fr: {
    tls: {
      section: "TLS / certificat client",
      intro: "Facultatif. Pour les serveurs avec une autorité de certification privée ou une authentification x509. Laissez vide pour n'utiliser que la chaîne de connexion ; Atlas et les CA publiques n'ont besoin de rien ici.",
      ca: "Certificat de la CA (PEM)",
      ca_hint: "Les certificats de l'autorité qui a signé le certificat du serveur. Vide utilise les racines de confiance du système.",
      cert: "Certificat client (PEM)",
      cert_hint: "Pour l'authentification x509 (authMechanism=MONGODB-X509 dans l'URI). Nécessite la clé ci-dessous.",
      key: "Clé privée du client (PEM)",
      key_hint: "Stockée chiffrée et jamais réaffichée. Une clé chiffrée (PKCS#8) nécessite son mot de passe.",
      key_stored: "Enregistrée. Laissez vide pour la conserver.",
      password: "Mot de passe de la clé",
      password_hint: "Uniquement pour une clé chiffrée.",
      password_stored: "Enregistré. Laissez vide pour le conserver.",
      file: "Charger depuis un fichier",
      file_too_large: "Le fichier est trop volumineux (64 Kio au maximum).",
      hostnames: "Autoriser les noms d'hôte invalides",
      hostnames_hint: "Ignore la vérification que le certificat du serveur nomme l'hôte. La chaîne de certificats est toujours vérifiée.",
      insecure: "Ne pas vérifier le certificat du serveur",
      insecure_hint: "Accepte n'importe quel certificat : toute personne sur le chemin réseau peut lire et modifier le trafic, identifiants compris.",
      insecure_confirm: "Désactiver la vérification des certificats ? Toute personne sur le chemin réseau pourrait alors intercepter cette connexion, ses identifiants et vos données.",
      warn_hostnames: "La vérification du nom d'hôte est désactivée : un certificat émis par la même CA pour un autre serveur est accepté.",
      warn_insecure: "La vérification des certificats est désactivée : cette connexion peut être interceptée. À n'utiliser que pour des tests.",
      badge_insecure: "TLS non vérifié",
      badge_hostnames: "Nom d'hôte non vérifié",
      badge_x509: "Certificat x509",
    },
  },
  zh: {
    tls: {
      section: "TLS / 客户端证书",
      intro: "可选。用于使用私有证书颁发机构或 x509 身份验证的服务器。留空则只使用连接字符串；Atlas 和公共 CA 无需在此填写。",
      ca: "CA 证书 (PEM)",
      ca_hint: "签发服务器证书的颁发机构的证书。留空则使用系统信任的根证书。",
      cert: "客户端证书 (PEM)",
      cert_hint: "用于 x509 身份验证（URI 中的 authMechanism=MONGODB-X509）。需要下方的私钥。",
      key: "客户端私钥 (PEM)",
      key_hint: "加密存储，不会再次显示。加密的 (PKCS#8) 私钥需要其密码。",
      key_stored: "已保存。留空以保留。",
      password: "私钥密码",
      password_hint: "仅用于加密的私钥。",
      password_stored: "已保存。留空以保留。",
      file: "从文件加载",
      file_too_large: "文件过大（最多 64 KiB）。",
      hostnames: "允许无效的主机名",
      hostnames_hint: "跳过服务器证书是否包含该主机名的检查。仍会验证证书链。",
      insecure: "不验证服务器证书",
      insecure_hint: "接受任何证书：网络路径上的任何人都可以读取和篡改流量，包括凭据。",
      insecure_confirm: "关闭证书验证？之后网络路径上的任何人都可能截获此连接、其凭据和您的数据。",
      warn_hostnames: "主机名检查已关闭：同一 CA 为其他服务器签发的证书也会被接受。",
      warn_insecure: "证书验证已关闭：此连接可能被截获。仅用于测试。",
      badge_insecure: "TLS 未验证",
      badge_hostnames: "未检查主机名",
      badge_x509: "x509 证书",
    },
  },
  ja: {
    tls: {
      section: "TLS / クライアント証明書",
      intro: "任意。プライベート認証局や x509 認証を使うサーバー向けです。接続文字列だけを使う場合は空のままにしてください。Atlas や公的 CA ではここに何も必要ありません。",
      ca: "CA 証明書 (PEM)",
      ca_hint: "サーバー証明書に署名した認証局の証明書。空の場合はシステムの信頼済みルートを使います。",
      cert: "クライアント証明書 (PEM)",
      cert_hint: "x509 認証用（URI に authMechanism=MONGODB-X509）。下の秘密鍵が必要です。",
      key: "クライアント秘密鍵 (PEM)",
      key_hint: "暗号化して保存され、再表示されません。暗号化された (PKCS#8) 鍵にはパスワードが必要です。",
      key_stored: "保存済み。保持するには空のままにしてください。",
      password: "鍵のパスワード",
      password_hint: "暗号化された鍵の場合のみ。",
      password_stored: "保存済み。保持するには空のままにしてください。",
      file: "ファイルから読み込む",
      file_too_large: "ファイルが大きすぎます（最大 64 KiB）。",
      hostnames: "無効なホスト名を許可する",
      hostnames_hint: "サーバー証明書がホスト名を含むかの確認を省きます。証明書チェーンは引き続き検証されます。",
      insecure: "サーバー証明書を検証しない",
      insecure_hint: "あらゆる証明書を受け入れます。ネットワーク経路上の誰もが認証情報を含む通信を読み取り、改ざんできます。",
      insecure_confirm: "証明書の検証をオフにしますか？ネットワーク経路上の誰もがこの接続、その認証情報、データを傍受できるようになります。",
      warn_hostnames: "ホスト名の確認はオフです。同じ CA が別のサーバー用に発行した証明書も受け入れられます。",
      warn_insecure: "証明書の検証はオフです。この接続は傍受される可能性があります。テスト専用にしてください。",
      badge_insecure: "TLS 未検証",
      badge_hostnames: "ホスト名未確認",
      badge_x509: "x509 証明書",
    },
  },
  ru: {
    tls: {
      section: "TLS / клиентский сертификат",
      intro: "Необязательно. Для серверов с частным удостоверяющим центром или аутентификацией x509. Оставьте пустым, чтобы использовать только строку подключения; для Atlas и публичных ЦС здесь ничего не нужно.",
      ca: "Сертификат ЦС (PEM)",
      ca_hint: "Сертификаты центра, подписавшего сертификат сервера. Если пусто, используются доверенные корневые сертификаты системы.",
      cert: "Клиентский сертификат (PEM)",
      cert_hint: "Для аутентификации x509 (authMechanism=MONGODB-X509 в URI). Нужен ключ ниже.",
      key: "Закрытый ключ клиента (PEM)",
      key_hint: "Хранится в зашифрованном виде и больше не показывается. Зашифрованному (PKCS#8) ключу нужен пароль.",
      key_stored: "Сохранён. Оставьте пустым, чтобы сохранить его.",
      password: "Пароль ключа",
      password_hint: "Только для зашифрованного ключа.",
      password_stored: "Сохранён. Оставьте пустым, чтобы сохранить его.",
      file: "Загрузить из файла",
      file_too_large: "Файл слишком большой (не более 64 КиБ).",
      hostnames: "Разрешить недействительные имена хостов",
      hostnames_hint: "Пропускает проверку того, что сертификат сервера содержит имя хоста. Цепочка сертификатов по-прежнему проверяется.",
      insecure: "Не проверять сертификат сервера",
      insecure_hint: "Принимает любой сертификат: любой на сетевом пути может читать и изменять трафик, включая учётные данные.",
      insecure_confirm: "Отключить проверку сертификата? Тогда любой на сетевом пути сможет перехватить это подключение, его учётные данные и ваши данные.",
      warn_hostnames: "Проверка имени хоста отключена: принимается сертификат, выданный тем же ЦС для другого сервера.",
      warn_insecure: "Проверка сертификата отключена: это подключение можно перехватить. Используйте только для тестов.",
      badge_insecure: "TLS не проверяется",
      badge_hostnames: "Имя хоста не проверяется",
      badge_x509: "Сертификат x509",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(TLS_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], TLS_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Connection form
// ---------------------------------------------------------------------------

// TLS_MASK is redact.Mask: the server's placeholder for a stored secret, sent back
// to keep it.
const TLS_MASK = "******";

// TLS_MAX_PEM is mongotls.MaxPEMSize.
const TLS_MAX_PEM = 64 * 1024;

// What the connection being edited has stored (the key and password are masked).
const tlsStored = { key: false, password: false, insecure: false, snapshot: "" };

function tlsEl(id) {
  return document.getElementById(id);
}

function tlsValue(id) {
  const el = tlsEl(id);
  return el ? el.value : "";
}

function tlsChecked(id) {
  const el = tlsEl(id);
  return !!(el && el.checked);
}

// tlsFillConnectionForm shows connection c (null for a new one) in the section.
function tlsFillConnectionForm(c) {
  const section = tlsEl("connection-tls");
  if (!section) return;
  const set = (id, v) => { const el = tlsEl(id); if (el) el.value = v || ""; };
  set("connection-tls-ca", c && c.tls_ca_pem);
  set("connection-tls-cert", c && c.tls_client_cert_pem);
  set("connection-tls-key", "");
  set("connection-tls-password", "");
  tlsStored.key = !!(c && c.tls_client_key_pem);
  tlsStored.password = !!(c && c.tls_client_key_password);
  tlsStored.insecure = !!(c && c.tls_insecure);
  const hostnames = tlsEl("connection-tls-hostnames");
  if (hostnames) hostnames.checked = !!(c && c.tls_allow_invalid_hostnames);
  const insecure = tlsEl("connection-tls-insecure");
  if (insecure) insecure.checked = tlsStored.insecure;
  tlsPlaceholders();
  tlsUpdateWarning();
  tlsStored.snapshot = JSON.stringify(tlsConnectionPayload());
  // Open the section when the connection uses it.
  section.open = !!(c && (c.tls_ca_pem || c.tls_client_cert_pem || c.tls_allow_invalid_hostnames || c.tls_insecure));
}

// tlsPlaceholders tells, in the empty key and password fields, that a value is
// stored and kept.
function tlsPlaceholders() {
  const key = tlsEl("connection-tls-key");
  if (key) key.placeholder = tlsStored.key ? t("tls.key_stored") : "-----BEGIN PRIVATE KEY-----";
  const pw = tlsEl("connection-tls-password");
  if (pw) pw.placeholder = tlsStored.password ? t("tls.password_stored") : "";
}

// tlsConnectionPayload returns the TLS fields of the connection payload, also for
// the form's connection test. Empty key and password fields keep stored values
// (the mask) unless the certificate was removed, which removes them too.
function tlsConnectionPayload() {
  if (!tlsEl("connection-tls")) return {};
  const cert = tlsValue("connection-tls-cert").trim();
  let key = tlsValue("connection-tls-key").trim();
  let password = tlsValue("connection-tls-password");
  if (!key) key = cert && tlsStored.key ? TLS_MASK : "";
  // The stored password belongs to the stored key: a new key brings its own.
  if (!password) password = key === TLS_MASK && tlsStored.password ? TLS_MASK : "";
  const insecure = tlsChecked("connection-tls-insecure");
  const out = {
    tls_ca_pem: tlsValue("connection-tls-ca").trim(),
    tls_client_cert_pem: cert,
    tls_client_key_pem: key,
    tls_client_key_password: password,
    tls_allow_invalid_hostnames: tlsChecked("connection-tls-hostnames"),
    tls_insecure: insecure,
  };
  // Confirmed when the box was ticked (setupTLS).
  if (insecure && !tlsStored.insecure) out.tls_insecure_confirm = true;
  return out;
}

// tlsFormChanged reports whether the section differs from the stored connection,
// so that the form's connection test sends it instead of testing the stored one.
function tlsFormChanged() {
  return !!tlsEl("connection-tls") && JSON.stringify(tlsConnectionPayload()) !== tlsStored.snapshot;
}

// tlsUpdateWarning shows the warning of the loosened checks.
function tlsUpdateWarning() {
  const box = tlsEl("connection-tls-warning");
  const text = tlsEl("connection-tls-warning-text");
  if (!box || !text) return;
  let msg = "";
  if (tlsChecked("connection-tls-insecure")) msg = t("tls.warn_insecure");
  else if (tlsChecked("connection-tls-hostnames")) msg = t("tls.warn_hostnames");
  text.textContent = msg;
  box.hidden = !msg;
  box.classList.toggle("callout-danger", tlsChecked("connection-tls-insecure"));
  box.classList.toggle("callout-warning", !tlsChecked("connection-tls-insecure"));
}

// tlsLoadFile reads the file chosen in input into the textarea it names.
async function tlsLoadFile(input) {
  const target = tlsEl(input.dataset.tlsFile);
  const file = input.files && input.files[0];
  input.value = "";
  if (!target || !file) return;
  if (file.size > TLS_MAX_PEM) {
    showToast(t("tls.file_too_large"), "error");
    return;
  }
  target.value = await file.text();
  tlsChanged();
}

// tlsChanged invalidates an earlier connection test of the form.
function tlsChanged() {
  tlsUpdateWarning();
  if (typeof resetConnectionTest === "function") resetConnectionTest();
}

// tlsConnectionBadges returns the badges of the connections table for c.
function tlsConnectionBadges(c) {
  if (!c) return "";
  let out = "";
  if (c.tls_insecure) out += ` ${statusBadge("danger", t("tls.badge_insecure"), t("tls.warn_insecure"))}`;
  else if (c.tls_allow_invalid_hostnames) out += ` ${statusBadge("warn", t("tls.badge_hostnames"), t("tls.warn_hostnames"))}`;
  if (c.tls_client_cert_pem) out += ` ${statusBadge("neutral", t("tls.badge_x509"))}`;
  return out;
}

function setupTLS() {
  if (!tlsEl("connection-tls")) return;
  ["connection-tls-ca", "connection-tls-cert", "connection-tls-key", "connection-tls-password", "connection-tls-hostnames"].forEach(id => {
    const el = tlsEl(id);
    if (el) el.addEventListener(el.type === "checkbox" ? "change" : "input", tlsChanged);
  });
  const insecure = tlsEl("connection-tls-insecure");
  if (insecure) {
    insecure.addEventListener("change", () => {
      if (insecure.checked && !tlsStored.insecure && !window.confirm(t("tls.insecure_confirm"))) insecure.checked = false;
      tlsChanged();
    });
  }
  document.querySelectorAll("input[data-tls-file]").forEach(input => {
    input.addEventListener("change", () => { tlsLoadFile(input); });
  });
  if (typeof onLanguageChange === "function") onLanguageChange(() => { tlsPlaceholders(); tlsUpdateWarning(); });
}

document.addEventListener("DOMContentLoaded", setupTLS);
