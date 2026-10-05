/**
 * MongoRescue dashboard: single sign-on (OpenID Connect).
 *
 * The sign-in page asks GET /api/v1/auth/methods (public) whether single sign-on
 * is on and shows its button: a plain link to /auth/oidc/start, so the CSP
 * form-action 'self' needs no exception. While the password form is limited to
 * local administrators it collapses under "Sign in with a local account". A failed
 * sign-in comes back as /?oidc_error=<code>, shown in the current language and then
 * removed from the address bar.
 *
 * Settings → Single sign-on edits the "oidc" group of PUT /api/v1/settings: the
 * provider, the client (its secret comes back masked; sending the mask back keeps
 * it), the callback URL with a copy button, the group → role mapping editor, the
 * domain filter and the break-glass mode, and a Test button
 * (POST /api/v1/settings/oidc/test). The users table gets a provider badge
 * (ssoProviderBadge) and the role of a single sign-on user is shown as managed by
 * the identity provider while mappings exist (ssoRoleManaged).
 *
 * Loaded after app.js, trust.js (mergeTranslations) and roles.js. Server values
 * reach the DOM only through escapeHtml() or textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const SSO_TRANSLATIONS = {
  en: {
    sso: {
      nav: "Single sign-on",
      desc: "Let people sign in with your identity provider over OpenID Connect (Entra ID, Google, Okta, Keycloak). Local accounts remain, and a local administrator can always sign in with a password.",
      desktop: "Single sign-on is not available in the desktop app, which serves only this computer: local accounts are the way in. Run the server or the Docker image to use single sign-on.",
      group_provider: "Identity provider",
      group_roles: "Roles",
      group_access: "Who may sign in",
      enabled: "Enable single sign-on",
      enabled_hint: "Turning it on checks the provider's discovery document and needs at least one local administrator.",
      display_name: "Button label",
      issuer: "Issuer URL",
      issuer_hint: "Such as https://login.microsoftonline.com/<tenant-id>/v2.0 or https://keycloak.example.com/realms/ops. https only.",
      test: "Test provider",
      testing: "Testing…",
      test_ok: "Provider found: {algs} signatures, {keys} keys, token endpoint {token}.",
      test_failed: "Provider test failed: {error}",
      client_id: "Client ID",
      client_secret: "Client secret",
      client_secret_hint: "Stored encrypted. Leave empty for a public client (PKCE only).",
      scopes: "Scopes",
      scopes_hint: "Separated by spaces; openid is always requested.",
      redirect_url: "Callback URL",
      redirect_url_hint: "Register exactly this URL at your provider. Its path must be /auth/oidc/callback.",
      copy: "Copy",
      copied: "Copied",
      username_claim: "Username claim",
      username_claim_hint: "Names new users; falls back to email, then the subject.",
      groups_claim: "Groups claim",
      groups_claim_hint: "A claim or dot path, such as groups or realm_access.roles.",
      mappings: "Group mappings",
      mappings_hint: "Members of a group get its role, matched exactly; the highest role wins. The admin role only ever comes from a mapping.",
      mapping_group: "Group",
      mapping_role: "Role",
      mapping_add: "Add mapping",
      mapping_remove: "Remove mapping",
      mappings_empty: "No mappings yet.",
      mapping_invalid: "Every mapping needs a group.",
      default_role: "Role without a matching group",
      default_deny: "None: refuse the sign-in",
      domains: "Allowed email domains",
      domains_hint: "One per line, such as example.com. Requires a verified email. Leave empty to allow any.",
      auto_create: "Create users at their first sign-in",
      auto_create_hint: "When off, only people who already have a user can sign in.",
      local_login: "Password sign-in",
      local_all: "Every local user",
      local_admins: "Local administrators only (break-glass)",
      local_login_hint: "Switching to administrators only ends the sessions of the other local users.",
      rp_logout: "Also sign out at the identity provider",
      rp_logout_hint: "Signing out of MongoRescue then ends the session at the provider too.",
      button: "Sign in with {name}",
      or: "or",
      local_toggle: "Sign in with a local account",
      badge: "SSO",
      badge_title: "Signs in through the identity provider",
      role_managed: "Managed by your identity provider",
      warn_title: "An administrator role was kept",
      warn: "A single sign-on would have demoted the last administrator, so the stored admin role was kept. Map an identity provider group to admin or keep a second administrator.",
      warn_dismiss: "Dismiss",
      err_state_mismatch: "The sign-in expired or was started in another browser. Please try again.",
      err_idp_error: "The identity provider could not sign you in. Try again or contact your administrator.",
      err_token_invalid: "The identity provider's answer could not be verified. Contact your administrator.",
      err_domain_not_allowed: "Your email domain is not allowed to sign in.",
      err_no_role: "Your account has no access to MongoRescue. Ask an administrator to add you to a group.",
      err_account_conflict: "A user with your name already exists. Ask an administrator to rename one of the accounts.",
      err_disabled: "Single sign-on is not enabled.",
      err_throttled: "Too many sign-in attempts. Wait a minute and try again.",
      err_unknown: "Single sign-on failed. Please try again.",
    },
  },
  tr: {
    sso: {
      nav: "Çoklu oturum açma",
      desc: "Kişilerin OpenID Connect ile kimlik sağlayıcınız üzerinden oturum açmasını sağlayın (Entra ID, Google, Okta, Keycloak). Yerel hesaplar kalır ve yerel bir yönetici her zaman parolayla oturum açabilir.",
      desktop: "Çoklu oturum açma yalnızca bu bilgisayara hizmet veren masaüstü uygulamasında kullanılamaz: giriş yolu yerel hesaplardır. Çoklu oturum açma için sunucuyu veya Docker imajını çalıştırın.",
      group_provider: "Kimlik sağlayıcı",
      group_roles: "Roller",
      group_access: "Kimler oturum açabilir",
      enabled: "Çoklu oturum açmayı etkinleştir",
      enabled_hint: "Açmak, sağlayıcının keşif belgesini denetler ve en az bir yerel yönetici gerektirir.",
      display_name: "Düğme etiketi",
      issuer: "Issuer URL'si",
      issuer_hint: "Örneğin https://login.microsoftonline.com/<tenant-id>/v2.0 veya https://keycloak.example.com/realms/ops. Yalnızca https.",
      test: "Sağlayıcıyı test et",
      testing: "Test ediliyor…",
      test_ok: "Sağlayıcı bulundu: {algs} imzaları, {keys} anahtar, token uç noktası {token}.",
      test_failed: "Sağlayıcı testi başarısız: {error}",
      client_id: "İstemci kimliği",
      client_secret: "İstemci gizli anahtarı",
      client_secret_hint: "Şifreli saklanır. Genel istemci için boş bırakın (yalnızca PKCE).",
      scopes: "Kapsamlar",
      scopes_hint: "Boşlukla ayrılır; openid her zaman istenir.",
      redirect_url: "Geri dönüş URL'si",
      redirect_url_hint: "Sağlayıcınızda tam olarak bu URL'yi kaydedin. Yolu /auth/oidc/callback olmalıdır.",
      copy: "Kopyala",
      copied: "Kopyalandı",
      username_claim: "Kullanıcı adı claim'i",
      username_claim_hint: "Yeni kullanıcıları adlandırır; yoksa e-posta, sonra subject kullanılır.",
      groups_claim: "Gruplar claim'i",
      groups_claim_hint: "Bir claim veya noktalı yol, örneğin groups ya da realm_access.roles.",
      mappings: "Grup eşlemeleri",
      mappings_hint: "Bir grubun üyeleri o grubun rolünü alır, birebir eşleşme ile; en yüksek rol kazanır. Yönetici rolü yalnızca bir eşlemeden gelir.",
      mapping_group: "Grup",
      mapping_role: "Rol",
      mapping_add: "Eşleme ekle",
      mapping_remove: "Eşlemeyi kaldır",
      mappings_empty: "Henüz eşleme yok.",
      mapping_invalid: "Her eşlemenin bir grubu olmalıdır.",
      default_role: "Eşleşen grup yoksa rol",
      default_deny: "Yok: oturum açmayı reddet",
      domains: "İzin verilen e-posta alan adları",
      domains_hint: "Satır başına bir tane, örneğin example.com. Doğrulanmış e-posta gerektirir. Hepsine izin vermek için boş bırakın.",
      auto_create: "Kullanıcıları ilk oturum açışta oluştur",
      auto_create_hint: "Kapalıyken yalnızca zaten kullanıcısı olan kişiler oturum açabilir.",
      local_login: "Parolayla oturum açma",
      local_all: "Tüm yerel kullanıcılar",
      local_admins: "Yalnızca yerel yöneticiler (acil durum erişimi)",
      local_login_hint: "Yalnızca yöneticilere geçmek diğer yerel kullanıcıların oturumlarını sonlandırır.",
      rp_logout: "Kimlik sağlayıcıda da oturumu kapat",
      rp_logout_hint: "MongoRescue'dan çıkış yapmak sağlayıcıdaki oturumu da sonlandırır.",
      button: "{name} ile oturum aç",
      or: "veya",
      local_toggle: "Yerel hesapla oturum aç",
      badge: "SSO",
      badge_title: "Kimlik sağlayıcı üzerinden oturum açar",
      role_managed: "Kimlik sağlayıcınız tarafından yönetilir",
      warn_title: "Bir yönetici rolü korundu",
      warn: "Bir çoklu oturum açma son yöneticinin rolünü düşürecekti, bu yüzden kayıtlı yönetici rolü korundu. Bir kimlik sağlayıcı grubunu yöneticiye eşleyin veya ikinci bir yönetici bulundurun.",
      warn_dismiss: "Kapat",
      err_state_mismatch: "Oturum açmanın süresi doldu veya başka bir tarayıcıda başlatıldı. Lütfen tekrar deneyin.",
      err_idp_error: "Kimlik sağlayıcı oturumunuzu açamadı. Tekrar deneyin veya yöneticinize başvurun.",
      err_token_invalid: "Kimlik sağlayıcının yanıtı doğrulanamadı. Yöneticinize başvurun.",
      err_domain_not_allowed: "E-posta alan adınızın oturum açmasına izin verilmiyor.",
      err_no_role: "Hesabınızın MongoRescue erişimi yok. Bir yöneticiden sizi bir gruba eklemesini isteyin.",
      err_account_conflict: "Sizin adınızla bir kullanıcı zaten var. Bir yöneticiden hesaplardan birini yeniden adlandırmasını isteyin.",
      err_disabled: "Çoklu oturum açma etkin değil.",
      err_throttled: "Çok fazla oturum açma denemesi. Bir dakika bekleyip tekrar deneyin.",
      err_unknown: "Çoklu oturum açma başarısız oldu. Lütfen tekrar deneyin.",
    },
  },
  de: {
    sso: {
      nav: "Single Sign-on",
      desc: "Anmeldung über Ihren Identitätsanbieter per OpenID Connect (Entra ID, Google, Okta, Keycloak). Lokale Konten bleiben bestehen, und ein lokaler Administrator kann sich immer mit Passwort anmelden.",
      desktop: "Single Sign-on ist in der Desktop-App nicht verfügbar, die nur diesen Computer bedient: Dort führen lokale Konten hinein. Nutzen Sie den Server oder das Docker-Image für Single Sign-on.",
      group_provider: "Identitätsanbieter",
      group_roles: "Rollen",
      group_access: "Wer sich anmelden darf",
      enabled: "Single Sign-on aktivieren",
      enabled_hint: "Beim Aktivieren wird das Discovery-Dokument des Anbieters geprüft; mindestens ein lokaler Administrator ist nötig.",
      display_name: "Beschriftung der Schaltfläche",
      issuer: "Issuer-URL",
      issuer_hint: "Etwa https://login.microsoftonline.com/<tenant-id>/v2.0 oder https://keycloak.example.com/realms/ops. Nur https.",
      test: "Anbieter testen",
      testing: "Wird getestet…",
      test_ok: "Anbieter gefunden: {algs}-Signaturen, {keys} Schlüssel, Token-Endpunkt {token}.",
      test_failed: "Anbietertest fehlgeschlagen: {error}",
      client_id: "Client-ID",
      client_secret: "Client-Secret",
      client_secret_hint: "Wird verschlüsselt gespeichert. Für einen öffentlichen Client leer lassen (nur PKCE).",
      scopes: "Scopes",
      scopes_hint: "Durch Leerzeichen getrennt; openid wird immer angefordert.",
      redirect_url: "Callback-URL",
      redirect_url_hint: "Registrieren Sie genau diese URL beim Anbieter. Ihr Pfad muss /auth/oidc/callback sein.",
      copy: "Kopieren",
      copied: "Kopiert",
      username_claim: "Claim für den Benutzernamen",
      username_claim_hint: "Benennt neue Benutzer; sonst E-Mail, dann das Subject.",
      groups_claim: "Claim für Gruppen",
      groups_claim_hint: "Ein Claim oder Punktpfad, etwa groups oder realm_access.roles.",
      mappings: "Gruppenzuordnungen",
      mappings_hint: "Mitglieder einer Gruppe erhalten deren Rolle (exakter Vergleich); die höchste Rolle gewinnt. Die Administratorrolle kommt nur aus einer Zuordnung.",
      mapping_group: "Gruppe",
      mapping_role: "Rolle",
      mapping_add: "Zuordnung hinzufügen",
      mapping_remove: "Zuordnung entfernen",
      mappings_empty: "Noch keine Zuordnungen.",
      mapping_invalid: "Jede Zuordnung braucht eine Gruppe.",
      default_role: "Rolle ohne passende Gruppe",
      default_deny: "Keine: Anmeldung ablehnen",
      domains: "Erlaubte E-Mail-Domains",
      domains_hint: "Eine pro Zeile, etwa example.com. Erfordert eine bestätigte E-Mail. Leer lassen, um alle zu erlauben.",
      auto_create: "Benutzer bei der ersten Anmeldung anlegen",
      auto_create_hint: "Wenn aus, können sich nur Personen anmelden, die bereits einen Benutzer haben.",
      local_login: "Anmeldung mit Passwort",
      local_all: "Alle lokalen Benutzer",
      local_admins: "Nur lokale Administratoren (Notfallzugang)",
      local_login_hint: "Der Wechsel zu „nur Administratoren“ beendet die Sitzungen der übrigen lokalen Benutzer.",
      rp_logout: "Auch beim Identitätsanbieter abmelden",
      rp_logout_hint: "Das Abmelden bei MongoRescue beendet dann auch die Sitzung beim Anbieter.",
      button: "Mit {name} anmelden",
      or: "oder",
      local_toggle: "Mit einem lokalen Konto anmelden",
      badge: "SSO",
      badge_title: "Meldet sich über den Identitätsanbieter an",
      role_managed: "Von Ihrem Identitätsanbieter verwaltet",
      warn_title: "Eine Administratorrolle wurde beibehalten",
      warn: "Ein Single Sign-on hätte den letzten Administrator herabgestuft, daher wurde die gespeicherte Administratorrolle beibehalten. Ordnen Sie eine Gruppe des Identitätsanbieters der Rolle Administrator zu oder behalten Sie einen zweiten Administrator.",
      warn_dismiss: "Ausblenden",
      err_state_mismatch: "Die Anmeldung ist abgelaufen oder wurde in einem anderen Browser begonnen. Bitte erneut versuchen.",
      err_idp_error: "Der Identitätsanbieter konnte Sie nicht anmelden. Erneut versuchen oder Ihren Administrator kontaktieren.",
      err_token_invalid: "Die Antwort des Identitätsanbieters konnte nicht überprüft werden. Kontaktieren Sie Ihren Administrator.",
      err_domain_not_allowed: "Ihre E-Mail-Domain darf sich nicht anmelden.",
      err_no_role: "Ihr Konto hat keinen Zugriff auf MongoRescue. Bitten Sie einen Administrator, Sie einer Gruppe hinzuzufügen.",
      err_account_conflict: "Ein Benutzer mit Ihrem Namen existiert bereits. Bitten Sie einen Administrator, eines der Konten umzubenennen.",
      err_disabled: "Single Sign-on ist nicht aktiviert.",
      err_throttled: "Zu viele Anmeldeversuche. Warten Sie eine Minute und versuchen Sie es erneut.",
      err_unknown: "Single Sign-on fehlgeschlagen. Bitte erneut versuchen.",
    },
  },
  es: {
    sso: {
      nav: "Inicio de sesión único",
      desc: "Permite iniciar sesión con su proveedor de identidad mediante OpenID Connect (Entra ID, Google, Okta, Keycloak). Las cuentas locales se mantienen y un administrador local siempre puede entrar con contraseña.",
      desktop: "El inicio de sesión único no está disponible en la aplicación de escritorio, que solo sirve a este equipo: allí se entra con cuentas locales. Use el servidor o la imagen de Docker para el inicio de sesión único.",
      group_provider: "Proveedor de identidad",
      group_roles: "Roles",
      group_access: "Quién puede iniciar sesión",
      enabled: "Activar el inicio de sesión único",
      enabled_hint: "Al activarlo se comprueba el documento de descubrimiento del proveedor y se necesita al menos un administrador local.",
      display_name: "Texto del botón",
      issuer: "URL del emisor (issuer)",
      issuer_hint: "Por ejemplo https://login.microsoftonline.com/<tenant-id>/v2.0 o https://keycloak.example.com/realms/ops. Solo https.",
      test: "Probar proveedor",
      testing: "Probando…",
      test_ok: "Proveedor encontrado: firmas {algs}, {keys} claves, endpoint de token {token}.",
      test_failed: "La prueba del proveedor falló: {error}",
      client_id: "ID de cliente",
      client_secret: "Secreto de cliente",
      client_secret_hint: "Se guarda cifrado. Déjelo vacío para un cliente público (solo PKCE).",
      scopes: "Ámbitos (scopes)",
      scopes_hint: "Separados por espacios; openid se solicita siempre.",
      redirect_url: "URL de retorno",
      redirect_url_hint: "Registre exactamente esta URL en su proveedor. Su ruta debe ser /auth/oidc/callback.",
      copy: "Copiar",
      copied: "Copiado",
      username_claim: "Claim del nombre de usuario",
      username_claim_hint: "Nombra a los usuarios nuevos; si falta, se usa el correo y luego el subject.",
      groups_claim: "Claim de grupos",
      groups_claim_hint: "Un claim o ruta con puntos, como groups o realm_access.roles.",
      mappings: "Asignaciones de grupos",
      mappings_hint: "Los miembros de un grupo reciben su rol (coincidencia exacta); gana el rol más alto. El rol de administrador solo llega por una asignación.",
      mapping_group: "Grupo",
      mapping_role: "Rol",
      mapping_add: "Añadir asignación",
      mapping_remove: "Quitar asignación",
      mappings_empty: "Aún no hay asignaciones.",
      mapping_invalid: "Cada asignación necesita un grupo.",
      default_role: "Rol sin grupo coincidente",
      default_deny: "Ninguno: rechazar el inicio de sesión",
      domains: "Dominios de correo permitidos",
      domains_hint: "Uno por línea, como example.com. Requiere un correo verificado. Déjelo vacío para permitir cualquiera.",
      auto_create: "Crear usuarios en su primer inicio de sesión",
      auto_create_hint: "Si está desactivado, solo pueden entrar quienes ya tienen un usuario.",
      local_login: "Inicio de sesión con contraseña",
      local_all: "Todos los usuarios locales",
      local_admins: "Solo administradores locales (acceso de emergencia)",
      local_login_hint: "Cambiar a solo administradores cierra las sesiones de los demás usuarios locales.",
      rp_logout: "Cerrar sesión también en el proveedor de identidad",
      rp_logout_hint: "Al salir de MongoRescue también se cierra la sesión en el proveedor.",
      button: "Iniciar sesión con {name}",
      or: "o",
      local_toggle: "Iniciar sesión con una cuenta local",
      badge: "SSO",
      badge_title: "Inicia sesión a través del proveedor de identidad",
      role_managed: "Gestionado por su proveedor de identidad",
      warn_title: "Se mantuvo un rol de administrador",
      warn: "Un inicio de sesión único habría degradado al último administrador, así que se mantuvo el rol de administrador guardado. Asigne un grupo del proveedor de identidad a administrador o mantenga un segundo administrador.",
      warn_dismiss: "Descartar",
      err_state_mismatch: "El inicio de sesión caducó o se empezó en otro navegador. Inténtelo de nuevo.",
      err_idp_error: "El proveedor de identidad no pudo iniciar su sesión. Inténtelo de nuevo o contacte con su administrador.",
      err_token_invalid: "No se pudo verificar la respuesta del proveedor de identidad. Contacte con su administrador.",
      err_domain_not_allowed: "Su dominio de correo no tiene permiso para iniciar sesión.",
      err_no_role: "Su cuenta no tiene acceso a MongoRescue. Pida a un administrador que le añada a un grupo.",
      err_account_conflict: "Ya existe un usuario con su nombre. Pida a un administrador que cambie el nombre de una de las cuentas.",
      err_disabled: "El inicio de sesión único no está activado.",
      err_throttled: "Demasiados intentos de inicio de sesión. Espere un minuto e inténtelo de nuevo.",
      err_unknown: "El inicio de sesión único falló. Inténtelo de nuevo.",
    },
  },
  fr: {
    sso: {
      nav: "Authentification unique",
      desc: "Permet de se connecter via votre fournisseur d'identité en OpenID Connect (Entra ID, Google, Okta, Keycloak). Les comptes locaux restent, et un administrateur local peut toujours se connecter avec un mot de passe.",
      desktop: "L'authentification unique n'est pas disponible dans l'application de bureau, qui ne sert que cet ordinateur : on y entre avec les comptes locaux. Utilisez le serveur ou l'image Docker pour l'authentification unique.",
      group_provider: "Fournisseur d'identité",
      group_roles: "Rôles",
      group_access: "Qui peut se connecter",
      enabled: "Activer l'authentification unique",
      enabled_hint: "L'activation vérifie le document de découverte du fournisseur et exige au moins un administrateur local.",
      display_name: "Libellé du bouton",
      issuer: "URL de l'émetteur (issuer)",
      issuer_hint: "Par exemple https://login.microsoftonline.com/<tenant-id>/v2.0 ou https://keycloak.example.com/realms/ops. https uniquement.",
      test: "Tester le fournisseur",
      testing: "Test en cours…",
      test_ok: "Fournisseur trouvé : signatures {algs}, {keys} clés, point de terminaison du jeton {token}.",
      test_failed: "Échec du test du fournisseur : {error}",
      client_id: "ID client",
      client_secret: "Secret client",
      client_secret_hint: "Stocké chiffré. Laissez vide pour un client public (PKCE uniquement).",
      scopes: "Portées (scopes)",
      scopes_hint: "Séparées par des espaces ; openid est toujours demandé.",
      redirect_url: "URL de rappel",
      redirect_url_hint: "Enregistrez exactement cette URL chez votre fournisseur. Son chemin doit être /auth/oidc/callback.",
      copy: "Copier",
      copied: "Copié",
      username_claim: "Claim du nom d'utilisateur",
      username_claim_hint: "Nomme les nouveaux utilisateurs ; à défaut l'e-mail, puis le subject.",
      groups_claim: "Claim des groupes",
      groups_claim_hint: "Un claim ou un chemin à points, comme groups ou realm_access.roles.",
      mappings: "Correspondances de groupes",
      mappings_hint: "Les membres d'un groupe reçoivent son rôle (correspondance exacte) ; le rôle le plus élevé l'emporte. Le rôle administrateur ne vient que d'une correspondance.",
      mapping_group: "Groupe",
      mapping_role: "Rôle",
      mapping_add: "Ajouter une correspondance",
      mapping_remove: "Supprimer la correspondance",
      mappings_empty: "Aucune correspondance pour l'instant.",
      mapping_invalid: "Chaque correspondance nécessite un groupe.",
      default_role: "Rôle sans groupe correspondant",
      default_deny: "Aucun : refuser la connexion",
      domains: "Domaines e-mail autorisés",
      domains_hint: "Un par ligne, comme example.com. Exige un e-mail vérifié. Laissez vide pour tout autoriser.",
      auto_create: "Créer les utilisateurs à leur première connexion",
      auto_create_hint: "Désactivé, seules les personnes ayant déjà un utilisateur peuvent se connecter.",
      local_login: "Connexion par mot de passe",
      local_all: "Tous les utilisateurs locaux",
      local_admins: "Administrateurs locaux uniquement (accès de secours)",
      local_login_hint: "Passer aux administrateurs uniquement met fin aux sessions des autres utilisateurs locaux.",
      rp_logout: "Se déconnecter aussi du fournisseur d'identité",
      rp_logout_hint: "Se déconnecter de MongoRescue met alors aussi fin à la session chez le fournisseur.",
      button: "Se connecter avec {name}",
      or: "ou",
      local_toggle: "Se connecter avec un compte local",
      badge: "SSO",
      badge_title: "Se connecte via le fournisseur d'identité",
      role_managed: "Géré par votre fournisseur d'identité",
      warn_title: "Un rôle administrateur a été conservé",
      warn: "Une authentification unique aurait rétrogradé le dernier administrateur ; le rôle administrateur enregistré a donc été conservé. Associez un groupe du fournisseur d'identité au rôle administrateur ou gardez un deuxième administrateur.",
      warn_dismiss: "Ignorer",
      err_state_mismatch: "La connexion a expiré ou a été lancée dans un autre navigateur. Veuillez réessayer.",
      err_idp_error: "Le fournisseur d'identité n'a pas pu vous connecter. Réessayez ou contactez votre administrateur.",
      err_token_invalid: "La réponse du fournisseur d'identité n'a pas pu être vérifiée. Contactez votre administrateur.",
      err_domain_not_allowed: "Votre domaine e-mail n'est pas autorisé à se connecter.",
      err_no_role: "Votre compte n'a pas accès à MongoRescue. Demandez à un administrateur de vous ajouter à un groupe.",
      err_account_conflict: "Un utilisateur porte déjà votre nom. Demandez à un administrateur de renommer l'un des comptes.",
      err_disabled: "L'authentification unique n'est pas activée.",
      err_throttled: "Trop de tentatives de connexion. Attendez une minute puis réessayez.",
      err_unknown: "L'authentification unique a échoué. Veuillez réessayer.",
    },
  },
  zh: {
    sso: {
      nav: "单点登录",
      desc: "让用户通过 OpenID Connect 使用您的身份提供商登录（Entra ID、Google、Okta、Keycloak）。本地账户保留，本地管理员始终可以用密码登录。",
      desktop: "桌面应用仅服务于本机，不提供单点登录：请使用本地账户登录。如需单点登录，请运行服务器或 Docker 镜像。",
      group_provider: "身份提供商",
      group_roles: "角色",
      group_access: "谁可以登录",
      enabled: "启用单点登录",
      enabled_hint: "启用时会检查提供商的发现文档，并且至少需要一个本地管理员。",
      display_name: "按钮文字",
      issuer: "Issuer URL",
      issuer_hint: "例如 https://login.microsoftonline.com/<tenant-id>/v2.0 或 https://keycloak.example.com/realms/ops。仅限 https。",
      test: "测试提供商",
      testing: "正在测试…",
      test_ok: "已找到提供商：{algs} 签名，{keys} 个密钥，令牌端点 {token}。",
      test_failed: "提供商测试失败：{error}",
      client_id: "客户端 ID",
      client_secret: "客户端密钥",
      client_secret_hint: "加密存储。公共客户端请留空（仅 PKCE）。",
      scopes: "范围（scopes）",
      scopes_hint: "以空格分隔；始终请求 openid。",
      redirect_url: "回调 URL",
      redirect_url_hint: "在提供商处登记完全相同的 URL。其路径必须是 /auth/oidc/callback。",
      copy: "复制",
      copied: "已复制",
      username_claim: "用户名声明",
      username_claim_hint: "为新用户命名；缺失时依次使用邮箱和 subject。",
      groups_claim: "组声明",
      groups_claim_hint: "声明名或点分路径，例如 groups 或 realm_access.roles。",
      mappings: "组映射",
      mappings_hint: "组成员获得该组的角色（精确匹配），取最高角色。管理员角色只能来自映射。",
      mapping_group: "组",
      mapping_role: "角色",
      mapping_add: "添加映射",
      mapping_remove: "删除映射",
      mappings_empty: "尚无映射。",
      mapping_invalid: "每个映射都需要一个组。",
      default_role: "无匹配组时的角色",
      default_deny: "无：拒绝登录",
      domains: "允许的邮箱域",
      domains_hint: "每行一个，例如 example.com。需要已验证的邮箱。留空则允许任何域。",
      auto_create: "首次登录时创建用户",
      auto_create_hint: "关闭后，只有已有用户的人才能登录。",
      local_login: "密码登录",
      local_all: "所有本地用户",
      local_admins: "仅本地管理员（应急访问）",
      local_login_hint: "切换为仅管理员会结束其他本地用户的会话。",
      rp_logout: "同时从身份提供商退出",
      rp_logout_hint: "退出 MongoRescue 时也会结束提供商处的会话。",
      button: "使用 {name} 登录",
      or: "或",
      local_toggle: "使用本地账户登录",
      badge: "SSO",
      badge_title: "通过身份提供商登录",
      role_managed: "由您的身份提供商管理",
      warn_title: "已保留一个管理员角色",
      warn: "一次单点登录本会降级最后一位管理员，因此保留了已存储的管理员角色。请将身份提供商的某个组映射为管理员，或保留第二位管理员。",
      warn_dismiss: "关闭",
      err_state_mismatch: "登录已过期或在其他浏览器中发起。请重试。",
      err_idp_error: "身份提供商无法为您登录。请重试或联系管理员。",
      err_token_invalid: "无法验证身份提供商的响应。请联系管理员。",
      err_domain_not_allowed: "您的邮箱域不允许登录。",
      err_no_role: "您的账户无权访问 MongoRescue。请让管理员将您加入某个组。",
      err_account_conflict: "已存在与您同名的用户。请让管理员重命名其中一个账户。",
      err_disabled: "未启用单点登录。",
      err_throttled: "登录尝试过多。请等待一分钟后重试。",
      err_unknown: "单点登录失败。请重试。",
    },
  },
  ja: {
    sso: {
      nav: "シングルサインオン",
      desc: "OpenID Connect で ID プロバイダー（Entra ID、Google、Okta、Keycloak）からサインインできるようにします。ローカルアカウントは残り、ローカル管理者はいつでもパスワードでサインインできます。",
      desktop: "このコンピューターだけで動くデスクトップアプリではシングルサインオンを使えません。ローカルアカウントでサインインしてください。シングルサインオンにはサーバーまたは Docker イメージを使用してください。",
      group_provider: "ID プロバイダー",
      group_roles: "ロール",
      group_access: "サインインできる人",
      enabled: "シングルサインオンを有効にする",
      enabled_hint: "有効にするとプロバイダーのディスカバリー文書を確認します。ローカル管理者が 1 人以上必要です。",
      display_name: "ボタンの表示名",
      issuer: "Issuer URL",
      issuer_hint: "例: https://login.microsoftonline.com/<tenant-id>/v2.0 や https://keycloak.example.com/realms/ops。https のみ。",
      test: "プロバイダーをテスト",
      testing: "テスト中…",
      test_ok: "プロバイダーが見つかりました: {algs} 署名、鍵 {keys} 個、トークンエンドポイント {token}。",
      test_failed: "プロバイダーのテストに失敗しました: {error}",
      client_id: "クライアント ID",
      client_secret: "クライアントシークレット",
      client_secret_hint: "暗号化して保存します。パブリッククライアントの場合は空にします（PKCE のみ）。",
      scopes: "スコープ",
      scopes_hint: "スペース区切り。openid は常に要求されます。",
      redirect_url: "コールバック URL",
      redirect_url_hint: "この URL をそのままプロバイダーに登録してください。パスは /auth/oidc/callback である必要があります。",
      copy: "コピー",
      copied: "コピーしました",
      username_claim: "ユーザー名のクレーム",
      username_claim_hint: "新しいユーザーの名前になります。ない場合はメール、次に subject を使います。",
      groups_claim: "グループのクレーム",
      groups_claim_hint: "クレーム名またはドット区切りのパス（groups、realm_access.roles など）。",
      mappings: "グループのマッピング",
      mappings_hint: "グループのメンバーはそのロールを得ます（完全一致）。最も高いロールが優先されます。管理者ロールはマッピングからのみ付与されます。",
      mapping_group: "グループ",
      mapping_role: "ロール",
      mapping_add: "マッピングを追加",
      mapping_remove: "マッピングを削除",
      mappings_empty: "マッピングはまだありません。",
      mapping_invalid: "各マッピングにはグループが必要です。",
      default_role: "一致するグループがない場合のロール",
      default_deny: "なし: サインインを拒否",
      domains: "許可するメールドメイン",
      domains_hint: "1 行に 1 つ（example.com など）。確認済みのメールが必要です。空にするとすべて許可します。",
      auto_create: "初回サインイン時にユーザーを作成",
      auto_create_hint: "オフの場合、既にユーザーがいる人だけがサインインできます。",
      local_login: "パスワードでのサインイン",
      local_all: "すべてのローカルユーザー",
      local_admins: "ローカル管理者のみ（緊急用アクセス）",
      local_login_hint: "管理者のみに切り替えると、ほかのローカルユーザーのセッションが終了します。",
      rp_logout: "ID プロバイダーからもサインアウト",
      rp_logout_hint: "MongoRescue からサインアウトすると、プロバイダーのセッションも終了します。",
      button: "{name} でサインイン",
      or: "または",
      local_toggle: "ローカルアカウントでサインイン",
      badge: "SSO",
      badge_title: "ID プロバイダー経由でサインインします",
      role_managed: "ID プロバイダーで管理されています",
      warn_title: "管理者ロールが維持されました",
      warn: "シングルサインオンにより最後の管理者が降格されるところだったため、保存されている管理者ロールを維持しました。ID プロバイダーのグループを管理者にマッピングするか、2 人目の管理者を置いてください。",
      warn_dismiss: "閉じる",
      err_state_mismatch: "サインインの有効期限が切れたか、別のブラウザーで開始されました。もう一度お試しください。",
      err_idp_error: "ID プロバイダーがサインインできませんでした。もう一度試すか、管理者に連絡してください。",
      err_token_invalid: "ID プロバイダーの応答を検証できませんでした。管理者に連絡してください。",
      err_domain_not_allowed: "このメールドメインはサインインを許可されていません。",
      err_no_role: "このアカウントには MongoRescue へのアクセス権がありません。管理者にグループへの追加を依頼してください。",
      err_account_conflict: "同じ名前のユーザーが既に存在します。管理者にどちらかのアカウント名の変更を依頼してください。",
      err_disabled: "シングルサインオンは有効になっていません。",
      err_throttled: "サインインの試行が多すぎます。1 分待ってからもう一度お試しください。",
      err_unknown: "シングルサインオンに失敗しました。もう一度お試しください。",
    },
  },
  ru: {
    sso: {
      nav: "Единый вход",
      desc: "Вход через ваш поставщик удостоверений по OpenID Connect (Entra ID, Google, Okta, Keycloak). Локальные учётные записи остаются, а локальный администратор всегда может войти с паролем.",
      desktop: "Единый вход недоступен в настольном приложении, которое обслуживает только этот компьютер: там входят по локальным учётным записям. Для единого входа используйте сервер или образ Docker.",
      group_provider: "Поставщик удостоверений",
      group_roles: "Роли",
      group_access: "Кто может войти",
      enabled: "Включить единый вход",
      enabled_hint: "При включении проверяется документ обнаружения поставщика; нужен хотя бы один локальный администратор.",
      display_name: "Надпись на кнопке",
      issuer: "URL издателя (issuer)",
      issuer_hint: "Например https://login.microsoftonline.com/<tenant-id>/v2.0 или https://keycloak.example.com/realms/ops. Только https.",
      test: "Проверить поставщика",
      testing: "Проверка…",
      test_ok: "Поставщик найден: подписи {algs}, ключей: {keys}, конечная точка токенов {token}.",
      test_failed: "Проверка поставщика не удалась: {error}",
      client_id: "Идентификатор клиента",
      client_secret: "Секрет клиента",
      client_secret_hint: "Хранится в зашифрованном виде. Оставьте пустым для публичного клиента (только PKCE).",
      scopes: "Области (scopes)",
      scopes_hint: "Через пробел; openid запрашивается всегда.",
      redirect_url: "URL обратного вызова",
      redirect_url_hint: "Зарегистрируйте у поставщика именно этот URL. Его путь должен быть /auth/oidc/callback.",
      copy: "Копировать",
      copied: "Скопировано",
      username_claim: "Claim имени пользователя",
      username_claim_hint: "Задаёт имя новых пользователей; иначе берётся email, затем subject.",
      groups_claim: "Claim групп",
      groups_claim_hint: "Claim или путь через точку, например groups или realm_access.roles.",
      mappings: "Сопоставление групп",
      mappings_hint: "Участники группы получают её роль (точное совпадение); побеждает самая высокая роль. Роль администратора выдаётся только через сопоставление.",
      mapping_group: "Группа",
      mapping_role: "Роль",
      mapping_add: "Добавить сопоставление",
      mapping_remove: "Удалить сопоставление",
      mappings_empty: "Сопоставлений пока нет.",
      mapping_invalid: "Для каждого сопоставления нужна группа.",
      default_role: "Роль без подходящей группы",
      default_deny: "Нет: отказать во входе",
      domains: "Разрешённые почтовые домены",
      domains_hint: "По одному в строке, например example.com. Нужен подтверждённый email. Оставьте пустым, чтобы разрешить любые.",
      auto_create: "Создавать пользователей при первом входе",
      auto_create_hint: "Если выключено, войти могут только те, у кого уже есть пользователь.",
      local_login: "Вход по паролю",
      local_all: "Все локальные пользователи",
      local_admins: "Только локальные администраторы (аварийный доступ)",
      local_login_hint: "Переключение на «только администраторы» завершает сеансы остальных локальных пользователей.",
      rp_logout: "Выходить и у поставщика удостоверений",
      rp_logout_hint: "Выход из MongoRescue тогда завершает и сеанс у поставщика.",
      button: "Войти через {name}",
      or: "или",
      local_toggle: "Войти с локальной учётной записью",
      badge: "SSO",
      badge_title: "Входит через поставщика удостоверений",
      role_managed: "Управляется вашим поставщиком удостоверений",
      warn_title: "Роль администратора сохранена",
      warn: "Единый вход понизил бы последнего администратора, поэтому сохранённая роль администратора оставлена. Сопоставьте группу поставщика удостоверений с ролью администратора или держите второго администратора.",
      warn_dismiss: "Скрыть",
      err_state_mismatch: "Время входа истекло или вход начат в другом браузере. Попробуйте ещё раз.",
      err_idp_error: "Поставщик удостоверений не смог выполнить вход. Попробуйте ещё раз или обратитесь к администратору.",
      err_token_invalid: "Не удалось проверить ответ поставщика удостоверений. Обратитесь к администратору.",
      err_domain_not_allowed: "Вашему почтовому домену вход не разрешён.",
      err_no_role: "У вашей учётной записи нет доступа к MongoRescue. Попросите администратора добавить вас в группу.",
      err_account_conflict: "Пользователь с вашим именем уже существует. Попросите администратора переименовать одну из учётных записей.",
      err_disabled: "Единый вход не включён.",
      err_throttled: "Слишком много попыток входа. Подождите минуту и попробуйте снова.",
      err_unknown: "Единый вход не удался. Попробуйте ещё раз.",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(SSO_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], SSO_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// SSO_ERROR_CODES are the failure codes of /auth/oidc/callback (/?oidc_error=).
const SSO_ERROR_CODES = ["state_mismatch", "idp_error", "token_invalid", "domain_not_allowed", "no_role",
  "account_conflict", "disabled", "throttled"];
const SSO_CALLBACK_PATH = "/auth/oidc/callback";
const WARNING_OIDC_ROLE_KEPT = "oidc_role_kept";
const SSO_MAX_MAPPINGS = 100;

const sso = {
  // GET /api/v1/auth/methods
  methods: null,
  // The last oidc_error of this page load (shown on the sign-in card).
  error: "",
  // The mapping editor's rows ({group, role}).
  mappings: [],
  dirty: false,
};

// ---------------------------------------------------------------------------
// Sign-in page
// ---------------------------------------------------------------------------

// ssoTakeError reads oidc_error from the address bar once and removes it.
function ssoTakeError() {
  let params;
  try {
    params = new URLSearchParams(window.location.search);
  } catch (err) {
    return;
  }
  const code = params.get("oidc_error");
  if (code === null) return;
  sso.error = SSO_ERROR_CODES.includes(code) ? code : "unknown";
  params.delete("oidc_error");
  const query = params.toString();
  try {
    window.history.replaceState(null, "", window.location.pathname + (query ? `?${query}` : "") + window.location.hash);
  } catch (err) {
    // Older browsers keep the parameter; it is shown once either way.
  }
}

// ssoReturnPath is where the sign-in returns to: the current dashboard path.
function ssoReturnPath() {
  const path = window.location.pathname + window.location.hash;
  return path.startsWith("/") && !path.startsWith("//") ? path : "/";
}

async function ssoLoadMethods() {
  try {
    const res = await fetch("/api/v1/auth/methods", { credentials: "same-origin" });
    const json = await readJSON(res);
    sso.methods = json && json.success && json.data ? json.data : null;
  } catch (err) {
    sso.methods = null;
  }
  return sso.methods;
}

// ssoShowLocalForm shows (or collapses) the password form.
function ssoShowLocalForm(show) {
  const form = document.getElementById("form-login");
  const toggle = document.getElementById("login-local-toggle");
  if (form) form.hidden = !show;
  if (toggle) toggle.hidden = show;
}

// ssoRenderLogin is called by showLogin: it shows the single sign-on button and a
// failed sign-in, and collapses the password form in admins_only mode.
async function ssoRenderLogin() {
  const box = document.getElementById("sso-login");
  const errorEl = document.getElementById("sso-login-error");
  if (errorEl) {
    errorEl.textContent = sso.error ? t(`sso.err_${sso.error}`) : "";
    errorEl.hidden = !sso.error;
  }
  const m = await ssoLoadMethods();
  const on = !!(m && m.oidc && m.oidc.enabled);
  if (box) box.hidden = !on;
  if (!on) {
    ssoShowLocalForm(true);
    return;
  }
  const link = document.getElementById("sso-login-link");
  if (link) link.setAttribute("href", `/auth/oidc/start?return_to=${encodeURIComponent(ssoReturnPath())}`);
  const name = String(m.oidc.display_name || t("sso.nav"));
  setText("sso-login-label", tf("sso.button", { name }));
  const adminsOnly = m.local === "admins_only";
  ssoShowLocalForm(!adminsOnly);
  if (adminsOnly && link) link.focus();
}

// ---------------------------------------------------------------------------
// Users table (called by app.js renderUsers and roles.js userRoleSelect)
// ---------------------------------------------------------------------------

// ssoIsExternal reports whether u signs in through the identity provider.
function ssoIsExternal(u) {
  return !!u && u.auth_provider === "oidc";
}

// ssoProviderBadge marks a single sign-on user.
function ssoProviderBadge(u) {
  if (!ssoIsExternal(u)) return "";
  return `<span class="chip chip-accent" title="${escapeHtml(t("sso.badge_title"))}">${escapeHtml(t("sso.badge"))}</span>`;
}

// ssoRoleManaged reports whether the role of u follows the group mappings (the
// server refuses manual changes then).
function ssoRoleManaged(u) {
  const mappings = settingsGroup("oidc").role_mappings;
  return ssoIsExternal(u) && Array.isArray(mappings) && mappings.length > 0;
}

// ---------------------------------------------------------------------------
// Settings → Single sign-on
// ---------------------------------------------------------------------------

function ssoCallbackURL() {
  return `${window.location.origin}${SSO_CALLBACK_PATH}`;
}

function ssoRoleOptions(selected) {
  return USER_ROLES.map(r =>
    `<option value="${r}"${r === selected ? " selected" : ""}>${escapeHtml(roleLabel(r))}</option>`).join("");
}

function ssoRenderMappings() {
  const tbody = document.getElementById("sso-mappings-tbody");
  if (!tbody) return;
  if (sso.mappings.length === 0) {
    setTbody(tbody, `<tr class="empty-row"><td colspan="4"><span class="muted">${escapeHtml(t("sso.mappings_empty"))}</span></td></tr>`);
  } else {
    setTbody(tbody, sso.mappings.map((m, i) => `<tr>
      <td><input type="text" class="form-input mono sso-mapping-group" data-index="${i}" maxlength="256" spellcheck="false" autocomplete="off"
        value="${escapeHtml(m.group)}" aria-label="${escapeHtml(t("sso.mapping_group"))}"></td>
      <td><select class="form-select sso-mapping-role" data-index="${i}" aria-label="${escapeHtml(t("sso.mapping_role"))}">${ssoRoleOptions(m.role)}</select></td>
      <td>${typeof mappingConnectionsSelect === "function" ? mappingConnectionsSelect(m, i) : ""}</td>
      <td class="col-actions"><button type="button" class="btn btn-ghost btn-sm btn-danger-text" data-action="sso-remove-mapping" data-index="${i}">${escapeHtml(t("sso.mapping_remove"))}</button></td>
    </tr>`).join(""));
  }
  const add = document.getElementById("sso-add-mapping");
  if (add) add.disabled = sso.mappings.length >= SSO_MAX_MAPPINGS;
}

// ssoFillSettings is called by app.js fillSettingsForms.
function ssoFillSettings(force) {
  const form = document.getElementById("form-sso");
  if (!form) return;
  const submit = form.querySelector("[type=submit]");
  if (submit) submit.disabled = !state.loaded.settings;
  ssoRenderAvailability();
  if (!state.loaded.settings || (sso.dirty && !force)) return;
  sso.dirty = false;
  const o = settingsGroup("oidc");
  document.getElementById("sso-enabled").checked = !!o.enabled;
  setValue("sso-display-name", o.display_name || "");
  setValue("sso-issuer", o.issuer || "");
  setValue("sso-client-id", o.client_id || "");
  setValue("sso-client-secret", o.client_secret || "");
  setValue("sso-scopes", Array.isArray(o.scopes) ? o.scopes.join(" ") : "openid email profile");
  setValue("sso-redirect-url", o.redirect_url || ssoCallbackURL());
  setValue("sso-username-claim", o.username_claim || "preferred_username");
  setValue("sso-groups-claim", o.groups_claim || "");
  setValue("sso-default-role", ["viewer", "operator"].includes(o.default_role) ? o.default_role : "");
  setValue("sso-domains", Array.isArray(o.allowed_email_domains) ? o.allowed_email_domains.join("\n") : "");
  document.getElementById("sso-auto-create").checked = o.auto_create_users !== false;
  setValue("sso-local-login", o.local_login === "admins_only" ? "admins_only" : "all");
  document.getElementById("sso-rp-logout").checked = !!o.rp_logout;
  sso.mappings = (Array.isArray(o.role_mappings) ? o.role_mappings : []).map(m => ({
    group: String((m && m.group) || ""), role: USER_ROLES.includes(m && m.role) ? m.role : "viewer",
    // Stored mappings carry all_connections; one without it predates connection
    // access and reaches every connection.
    all_connections: !(m && m.all_connections === false),
    connection_ids: Array.isArray(m && m.connection_ids) ? m.connection_ids.map(String) : []
  }));
  ssoRenderMappings();
  hideFormError("sso-error");
}

// ssoRenderAvailability explains why the section is read-only in the desktop app.
async function ssoRenderAvailability() {
  const note = document.getElementById("sso-desktop-note");
  const form = document.getElementById("form-sso");
  if (!note || !form) return;
  const m = sso.methods || await ssoLoadMethods();
  const unavailable = !!m && m.oidc && m.oidc.available === false;
  note.hidden = !unavailable;
  form.hidden = unavailable;
}

// ssoRenderWarnings is called by app.js renderWarnings.
function ssoRenderWarnings(list) {
  const el = document.getElementById("sso-role-warning");
  if (el) el.hidden = !(Array.isArray(list) && list.some(w => w && w.id === WARNING_OIDC_ROLE_KEPT));
}

async function ssoDismissWarning() {
  try {
    const json = await apiJSON(`/api/v1/settings/warnings/${WARNING_OIDC_ROLE_KEPT}/dismiss`, { method: "POST" });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    state.settings = { ...(state.settings || {}), warnings: (json.data && json.data.warnings) || [] };
    renderWarnings();
  } catch (err) {
    showToast(err.message, "error");
  }
}

function ssoReadMappings() {
  document.querySelectorAll(".sso-mapping-group").forEach(el => {
    const i = Number(el.dataset.index);
    if (sso.mappings[i]) sso.mappings[i].group = el.value;
  });
  document.querySelectorAll(".sso-mapping-role").forEach(el => {
    const i = Number(el.dataset.index);
    if (sso.mappings[i]) sso.mappings[i].role = el.value;
  });
  document.querySelectorAll(".sso-mapping-connections").forEach(el => {
    const i = Number(el.dataset.index);
    if (sso.mappings[i] && typeof readMappingAccess === "function") Object.assign(sso.mappings[i], readMappingAccess(el));
  });
}

function ssoLines(id) {
  return getValue(id).split(/[\s,]+/).map(s => s.trim()).filter(Boolean);
}

async function ssoSaveSettings(e) {
  e.preventDefault();
  ssoReadMappings();
  // An admin mapping always reaches every connection.
  const mappings = sso.mappings.map(m => {
    const all = m.role === "admin" || m.all_connections !== false;
    return { group: String(m.group || "").trim(), role: m.role, all_connections: all, connection_ids: all ? [] : (m.connection_ids || []) };
  });
  if (mappings.some(m => !m.group)) {
    showFormError("sso-error", t("sso.mapping_invalid"));
    return;
  }
  const secretEl = document.getElementById("sso-client-secret");
  const payload = {
    oidc: {
      enabled: document.getElementById("sso-enabled").checked,
      display_name: getValue("sso-display-name"),
      issuer: getValue("sso-issuer"),
      client_id: getValue("sso-client-id"),
      client_secret: secretEl ? secretEl.value : "",
      scopes: ssoLines("sso-scopes"),
      redirect_url: getValue("sso-redirect-url"),
      username_claim: getValue("sso-username-claim"),
      groups_claim: getValue("sso-groups-claim"),
      role_mappings: mappings,
      default_role: getValue("sso-default-role"),
      allowed_email_domains: ssoLines("sso-domains"),
      auto_create_users: document.getElementById("sso-auto-create").checked,
      local_login: getValue("sso-local-login"),
      rp_logout: document.getElementById("sso-rp-logout").checked
    }
  };
  const submit = e.submitter || document.querySelector("#form-sso [type=submit]");
  if (submit) submit.disabled = true;
  try {
    const json = await apiJSON("/api/v1/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    });
    if (!json.success) {
      showFormError("sso-error", json.error || t("toasts.request_failed"));
      return;
    }
    hideFormError("sso-error");
    // Only this form is refilled, so unsaved edits in the other forms stay.
    if (json.data && json.data.oidc) {
      state.settings = { ...(state.settings || {}), oidc: json.data.oidc, warnings: json.data.warnings || [] };
    }
    sso.dirty = false;
    sso.methods = null;
    ssoFillSettings(true);
    renderWarnings();
    if (typeof renderUsers === "function") renderUsers();
    setText("sso-save-status", t("settings.saved"));
    setTimeout(() => setText("sso-save-status", ""), 3000);
  } catch (err) {
    showFormError("sso-error", err.message);
  } finally {
    if (submit) submit.disabled = false;
  }
}

async function ssoTest(btn) {
  const out = document.getElementById("sso-test-result");
  if (btn) btn.disabled = true;
  if (out) {
    out.textContent = t("sso.testing");
    out.classList.remove("text-danger");
  }
  try {
    const json = await apiJSON("/api/v1/settings/oidc/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ issuer: getValue("sso-issuer") })
    });
    if (!out) return;
    if (json.success && json.data) {
      const d = json.data;
      out.textContent = tf("sso.test_ok", {
        algs: (Array.isArray(d.usable_algorithms) ? d.usable_algorithms : []).join(", "),
        keys: Number(d.keys) || 0,
        token: String(d.token_endpoint || "")
      });
    } else {
      out.textContent = tf("sso.test_failed", { error: json.error || t("toasts.request_failed") });
      out.classList.add("text-danger");
    }
  } catch (err) {
    if (out) {
      out.textContent = tf("sso.test_failed", { error: err.message });
      out.classList.add("text-danger");
    }
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function ssoCopyCallback(btn) {
  const url = getValue("sso-redirect-url") || ssoCallbackURL();
  if (typeof copyText === "function") {
    await copyText(url, btn);
    return;
  }
  try {
    await navigator.clipboard.writeText(url);
    showToast(t("sso.copied"), "success");
  } catch (err) {
    showToast(err.message, "error");
  }
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

function ssoFocus(id) {
  const el = document.getElementById(id);
  if (el) el.focus();
}

function ssoSetup() {
  if (typeof ROLE_ACTION_SCOPES === "object") {
    Object.assign(ROLE_ACTION_SCOPES, {
      "sso-test": "admin", "sso-add-mapping": "admin", "sso-remove-mapping": "admin",
      "sso-copy-callback": "admin", "sso-dismiss-warning": "admin"
    });
  }
  document.addEventListener("click", (e) => {
    const btn = e.target instanceof Element ? e.target.closest("[data-action^='sso-']") : null;
    if (!btn || btn.disabled) return;
    switch (btn.dataset.action) {
      case "sso-show-local":
        ssoShowLocalForm(true);
        ssoFocus("login-username");
        break;
      case "sso-test":
        ssoTest(btn);
        break;
      case "sso-copy-callback":
        ssoCopyCallback(btn);
        break;
      case "sso-add-mapping":
        ssoReadMappings();
        if (sso.mappings.length < SSO_MAX_MAPPINGS) sso.mappings.push({ group: "", role: "viewer", all_connections: true, connection_ids: [] });
        sso.dirty = true;
        ssoRenderMappings();
        {
          const last = document.querySelector(`.sso-mapping-group[data-index="${sso.mappings.length - 1}"]`);
          if (last) last.focus();
        }
        break;
      case "sso-remove-mapping":
        ssoReadMappings();
        sso.mappings.splice(Number(btn.dataset.index), 1);
        sso.dirty = true;
        ssoRenderMappings();
        break;
      case "sso-dismiss-warning":
        ssoDismissWarning();
        break;
    }
  });
  const form = document.getElementById("form-sso");
  if (form) {
    form.addEventListener("submit", ssoSaveSettings);
    form.addEventListener("input", () => { sso.dirty = true; });
    form.addEventListener("change", () => { sso.dirty = true; });
  }
  if (typeof onLanguageChange === "function") {
    onLanguageChange(() => {
      ssoReadMappings();
      ssoRenderMappings();
      const card = document.getElementById("login-card");
      if (card && !card.hidden) ssoRenderLogin();
    });
  }
}

// The failure code is taken before app.js renders the sign-in page.
ssoTakeError();
document.addEventListener("DOMContentLoaded", ssoSetup);
