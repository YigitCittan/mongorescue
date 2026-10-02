/**
 * MongoRescue dashboard: dashboard roles (viewer, operator, admin).
 *
 * The signed-in user's role and effective scope come from GET /api/v1/auth/me
 * (app.js setAuth, can()). This module shapes the UI to them: actions the role may
 * not take are disabled with a tooltip, administrator-only areas (users, the audit
 * log, the MCP activity) are hidden, settings forms are read-only below admin, and
 * the users table gets a role column. The server stays authoritative: every request
 * is checked there, whatever the UI shows.
 *
 * Loaded after app.js and trust.js (mergeTranslations). Server values reach the DOM
 * only through escapeHtml() or textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const ROLES_TRANSLATIONS = {
  en: {
    settings: {
      user_role_col: "Role",
      user_role_label: "Role",
      user_role_viewer: "Viewer",
      user_role_operator: "Operator",
      user_role_admin: "Admin",
      user_role_hint: "Viewer: sees everything, changes nothing. Operator: also starts backups, runs jobs and restores into safe clones. Admin: everything, including users, settings and deletions.",
      user_role_users_desc: "People who can sign in. Their dashboard role decides what they may do, and the API keys they create never get more.",
      user_role_viewer_only: "Your role ({role}) can only view",
      user_role_not_allowed: "Your role ({role}) does not allow this",
      user_role_self: "You cannot change your own role",
      user_role_last_admin: "The last administrator cannot be demoted or deleted",
      user_role_select_label: "Role of {name}",
      user_role_confirm_title: "Change role",
      user_role_confirm_demote: "Change {name} from {from} to {to}? Their sessions end, and the API keys they created are limited to the new role.",
      user_role_changed: "{name} is now {role}",
      user_role_key_capped: "capped at {scope}",
      user_role_key_capped_title: "The creator's role limits this key to {scope}",
      user_role_scope_limit: "Scopes above your role are not offered.",
      user_role_settings_readonly: "Only administrators can change these settings.",
      user_role_menu: "Role: {role}",
    },
  },
  tr: {
    settings: {
      user_role_col: "Rol",
      user_role_label: "Rol",
      user_role_viewer: "Görüntüleyici",
      user_role_operator: "Operatör",
      user_role_admin: "Yönetici",
      user_role_hint: "Görüntüleyici: her şeyi görür, hiçbir şeyi değiştirmez. Operatör: ayrıca yedek başlatır, görev çalıştırır ve güvenli kopyalara geri yükler. Yönetici: kullanıcılar, ayarlar ve silmeler dahil her şey.",
      user_role_users_desc: "Oturum açabilen kişiler. Panel rolleri ne yapabileceklerini belirler; oluşturdukları API anahtarları bundan fazlasını alamaz.",
      user_role_viewer_only: "Rolünüz ({role}) yalnızca görüntüleyebilir",
      user_role_not_allowed: "Rolünüz ({role}) buna izin vermiyor",
      user_role_self: "Kendi rolünüzü değiştiremezsiniz",
      user_role_last_admin: "Son yönetici düşürülemez veya silinemez",
      user_role_select_label: "{name} kullanıcısının rolü",
      user_role_confirm_title: "Rolü değiştir",
      user_role_confirm_demote: "{name} kullanıcısı {from} rolünden {to} rolüne geçirilsin mi? Oturumları sona erer ve oluşturduğu API anahtarları yeni rolle sınırlanır.",
      user_role_changed: "{name} artık {role}",
      user_role_key_capped: "{scope} ile sınırlı",
      user_role_key_capped_title: "Oluşturanın rolü bu anahtarı {scope} ile sınırlıyor",
      user_role_scope_limit: "Rolünüzün üzerindeki kapsamlar sunulmaz.",
      user_role_settings_readonly: "Bu ayarları yalnızca yöneticiler değiştirebilir.",
      user_role_menu: "Rol: {role}",
    },
  },
  de: {
    settings: {
      user_role_col: "Rolle",
      user_role_label: "Rolle",
      user_role_viewer: "Betrachter",
      user_role_operator: "Operator",
      user_role_admin: "Administrator",
      user_role_hint: "Betrachter: sieht alles, ändert nichts. Operator: startet außerdem Backups, führt Jobs aus und stellt in sichere Klone wieder her. Administrator: alles, einschließlich Benutzer, Einstellungen und Löschungen.",
      user_role_users_desc: "Personen, die sich anmelden können. Ihre Dashboard-Rolle bestimmt, was sie dürfen; die API-Schlüssel, die sie erstellen, erhalten nie mehr.",
      user_role_viewer_only: "Ihre Rolle ({role}) darf nur ansehen",
      user_role_not_allowed: "Ihre Rolle ({role}) erlaubt das nicht",
      user_role_self: "Sie können Ihre eigene Rolle nicht ändern",
      user_role_last_admin: "Der letzte Administrator kann nicht herabgestuft oder gelöscht werden",
      user_role_select_label: "Rolle von {name}",
      user_role_confirm_title: "Rolle ändern",
      user_role_confirm_demote: "{name} von {from} auf {to} ändern? Die Sitzungen enden, und die erstellten API-Schlüssel werden auf die neue Rolle begrenzt.",
      user_role_changed: "{name} ist jetzt {role}",
      user_role_key_capped: "begrenzt auf {scope}",
      user_role_key_capped_title: "Die Rolle des Erstellers begrenzt diesen Schlüssel auf {scope}",
      user_role_scope_limit: "Bereiche über Ihrer Rolle werden nicht angeboten.",
      user_role_settings_readonly: "Nur Administratoren können diese Einstellungen ändern.",
      user_role_menu: "Rolle: {role}",
    },
  },
  es: {
    settings: {
      user_role_col: "Rol",
      user_role_label: "Rol",
      user_role_viewer: "Lector",
      user_role_operator: "Operador",
      user_role_admin: "Administrador",
      user_role_hint: "Lector: ve todo y no cambia nada. Operador: además inicia copias, ejecuta tareas y restaura en clones seguros. Administrador: todo, incluidos usuarios, ajustes y eliminaciones.",
      user_role_users_desc: "Personas que pueden iniciar sesión. Su rol en el panel decide qué pueden hacer, y las claves API que crean nunca obtienen más.",
      user_role_viewer_only: "Tu rol ({role}) solo permite ver",
      user_role_not_allowed: "Tu rol ({role}) no permite esto",
      user_role_self: "No puedes cambiar tu propio rol",
      user_role_last_admin: "El último administrador no se puede degradar ni eliminar",
      user_role_select_label: "Rol de {name}",
      user_role_confirm_title: "Cambiar rol",
      user_role_confirm_demote: "¿Cambiar a {name} de {from} a {to}? Sus sesiones terminan y las claves API que creó quedan limitadas al nuevo rol.",
      user_role_changed: "{name} ahora es {role}",
      user_role_key_capped: "limitada a {scope}",
      user_role_key_capped_title: "El rol del creador limita esta clave a {scope}",
      user_role_scope_limit: "No se ofrecen ámbitos por encima de tu rol.",
      user_role_settings_readonly: "Solo los administradores pueden cambiar estos ajustes.",
      user_role_menu: "Rol: {role}",
    },
  },
  fr: {
    settings: {
      user_role_col: "Rôle",
      user_role_label: "Rôle",
      user_role_viewer: "Lecteur",
      user_role_operator: "Opérateur",
      user_role_admin: "Administrateur",
      user_role_hint: "Lecteur : voit tout, ne modifie rien. Opérateur : lance aussi des sauvegardes, exécute des tâches et restaure dans des clones sûrs. Administrateur : tout, y compris les utilisateurs, les paramètres et les suppressions.",
      user_role_users_desc: "Les personnes qui peuvent se connecter. Leur rôle dans le tableau de bord détermine ce qu'elles peuvent faire, et les clés API qu'elles créent n'obtiennent jamais plus.",
      user_role_viewer_only: "Votre rôle ({role}) permet seulement de consulter",
      user_role_not_allowed: "Votre rôle ({role}) ne permet pas cette action",
      user_role_self: "Vous ne pouvez pas modifier votre propre rôle",
      user_role_last_admin: "Le dernier administrateur ne peut être ni rétrogradé ni supprimé",
      user_role_select_label: "Rôle de {name}",
      user_role_confirm_title: "Changer le rôle",
      user_role_confirm_demote: "Passer {name} de {from} à {to} ? Ses sessions prennent fin et les clés API qu'il a créées sont limitées au nouveau rôle.",
      user_role_changed: "{name} est maintenant {role}",
      user_role_key_capped: "limitée à {scope}",
      user_role_key_capped_title: "Le rôle du créateur limite cette clé à {scope}",
      user_role_scope_limit: "Les portées au-dessus de votre rôle ne sont pas proposées.",
      user_role_settings_readonly: "Seuls les administrateurs peuvent modifier ces paramètres.",
      user_role_menu: "Rôle : {role}",
    },
  },
  zh: {
    settings: {
      user_role_col: "角色",
      user_role_label: "角色",
      user_role_viewer: "查看者",
      user_role_operator: "操作员",
      user_role_admin: "管理员",
      user_role_hint: "查看者：可查看所有内容，不能更改。操作员：还可启动备份、运行任务并恢复到安全克隆。管理员：全部权限，包括用户、设置和删除。",
      user_role_users_desc: "可以登录的人员。其仪表板角色决定其可执行的操作，其创建的 API 密钥权限永远不会更高。",
      user_role_viewer_only: "您的角色（{role}）只能查看",
      user_role_not_allowed: "您的角色（{role}）不允许此操作",
      user_role_self: "不能更改自己的角色",
      user_role_last_admin: "不能降级或删除最后一个管理员",
      user_role_select_label: "{name} 的角色",
      user_role_confirm_title: "更改角色",
      user_role_confirm_demote: "将 {name} 从 {from} 更改为 {to}？其会话将结束，其创建的 API 密钥将受限于新角色。",
      user_role_changed: "{name} 现在是 {role}",
      user_role_key_capped: "上限为 {scope}",
      user_role_key_capped_title: "创建者的角色将此密钥限制为 {scope}",
      user_role_scope_limit: "不提供高于您角色的范围。",
      user_role_settings_readonly: "只有管理员可以更改这些设置。",
      user_role_menu: "角色：{role}",
    },
  },
  ja: {
    settings: {
      user_role_col: "ロール",
      user_role_label: "ロール",
      user_role_viewer: "閲覧者",
      user_role_operator: "オペレーター",
      user_role_admin: "管理者",
      user_role_hint: "閲覧者：すべてを表示でき、変更はできません。オペレーター：さらにバックアップの開始、ジョブの実行、安全なクローンへの復元ができます。管理者：ユーザー、設定、削除を含むすべて。",
      user_role_users_desc: "サインインできるユーザーです。ダッシュボードのロールで操作できる範囲が決まり、作成した API キーがそれを超えることはありません。",
      user_role_viewer_only: "あなたのロール（{role}）は閲覧のみ可能です",
      user_role_not_allowed: "あなたのロール（{role}）ではこの操作はできません",
      user_role_self: "自分のロールは変更できません",
      user_role_last_admin: "最後の管理者は降格も削除もできません",
      user_role_select_label: "{name} のロール",
      user_role_confirm_title: "ロールを変更",
      user_role_confirm_demote: "{name} を {from} から {to} に変更しますか？セッションは終了し、作成した API キーは新しいロールに制限されます。",
      user_role_changed: "{name} は {role} になりました",
      user_role_key_capped: "{scope} に制限",
      user_role_key_capped_title: "作成者のロールによりこのキーは {scope} に制限されています",
      user_role_scope_limit: "あなたのロールを超えるスコープは表示されません。",
      user_role_settings_readonly: "これらの設定を変更できるのは管理者のみです。",
      user_role_menu: "ロール：{role}",
    },
  },
  ru: {
    settings: {
      user_role_col: "Роль",
      user_role_label: "Роль",
      user_role_viewer: "Наблюдатель",
      user_role_operator: "Оператор",
      user_role_admin: "Администратор",
      user_role_hint: "Наблюдатель: видит всё, ничего не меняет. Оператор: также запускает резервные копии, задания и восстановление в безопасные клоны. Администратор: всё, включая пользователей, настройки и удаление.",
      user_role_users_desc: "Люди, которые могут входить. Роль в панели определяет, что им разрешено; созданные ими API-ключи никогда не получают больше.",
      user_role_viewer_only: "Ваша роль ({role}) позволяет только просмотр",
      user_role_not_allowed: "Ваша роль ({role}) этого не позволяет",
      user_role_self: "Нельзя изменить собственную роль",
      user_role_last_admin: "Последнего администратора нельзя понизить или удалить",
      user_role_select_label: "Роль пользователя {name}",
      user_role_confirm_title: "Изменить роль",
      user_role_confirm_demote: "Изменить роль {name} с {from} на {to}? Его сеансы завершатся, а созданные им API-ключи будут ограничены новой ролью.",
      user_role_changed: "{name} теперь {role}",
      user_role_key_capped: "ограничен до {scope}",
      user_role_key_capped_title: "Роль создателя ограничивает этот ключ до {scope}",
      user_role_scope_limit: "Области выше вашей роли не предлагаются.",
      user_role_settings_readonly: "Изменять эти настройки могут только администраторы.",
      user_role_menu: "Роль: {role}",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(ROLES_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], ROLES_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// What each action needs
// ---------------------------------------------------------------------------

const USER_ROLES = ["viewer", "operator", "admin"];

// The scope each data-action needs, mirroring the route it calls (see
// internal/server/scopes.go). Actions not listed here only read, or act on the
// caller's own account (password, API keys, sessions), which every role may do.
const ROLE_ACTION_SCOPES = {
  // Operator: start, retry and cancel runs, safe-clone restores, verify, pin, test.
  "backup-now": "operator",
  "trigger-job": "operator",
  "retry-backup": "operator",
  "restore-backup": "operator",
  "cancel-run": "operator",
  "cancel-run-backup": "operator",
  "stop-job-run": "operator",
  "trust-verify": "operator",
  "trust-pin": "operator",
  "trust-restore-test": "operator",
  // Admin: configuration, deletions, users, the recovery kit and the audit log.
  "new-job": "admin",
  "edit-job": "admin",
  "delete-job": "admin",
  "toggle-job": "admin",
  "pause-job": "admin",
  "resume-job": "admin",
  "delete-backup": "admin",
  "trust-unpin": "admin",
  "trust-scan": "admin",
  "trust-import": "admin",
  "trust-run-sweep": "admin",
  "new-connection": "admin",
  "edit-connection": "admin",
  "delete-connection": "admin",
  "test-connection": "admin",
  "test-connection-form": "admin",
  "new-storage": "admin",
  "new-storage-for": "admin",
  "edit-storage": "admin",
  "delete-storage": "admin",
  "test-storage": "admin",
  "default-storage": "admin",
  "test-storage-form": "admin",
  "generate-key": "admin",
  "use-generated-key": "admin",
  "dismiss-encryption-warning": "admin",
  "new-channel": "admin",
  "edit-channel": "admin",
  "delete-channel": "admin",
  "test-channel": "admin",
  "new-rule": "admin",
  "edit-rule": "admin",
  "delete-rule": "admin",
  "new-user": "admin",
  "delete-user": "admin",
  "change-user-password": "admin",
  "recovery-run-metabackup": "admin",
  "recovery-open-kit": "admin",
  "recovery-dismiss-kit": "admin",
  "refresh-audit": "admin",
  "auditlog-verify": "admin",
  "auditlog-export": "admin",
  "auditlog-more": "admin",
  "auditlog-refresh": "admin"
};

// The dialogs the command palette and keyboard shortcuts open directly, with the
// scope of what they submit.
const ROLE_GUARDED_OPENERS = {
  openBackupNowModal: "operator",
  openRestoreModal: "operator",
  openJobModal: "admin",
  openConnectionModal: "admin",
  openStorageModal: "admin",
  openStorageModalFor: "admin",
  openChannelModal: "admin",
  openRuleModal: "admin",
  openUserModal: "admin"
};

// roleLabel names a role in the current language.
function roleLabel(role) {
  return USER_ROLES.includes(role) ? t(`settings.user_role_${role}`) : String(role || "");
}

// roleDeniedText is the tooltip of an action the signed-in role may not take.
function roleDeniedText() {
  const role = roleLabel(auth.role || "viewer");
  return auth.scope === "read" ? tf("settings.user_role_viewer_only", { role }) : tf("settings.user_role_not_allowed", { role });
}

// roleNeed returns the scope element el needs, or "" when any role may use it.
function roleNeed(el) {
  return el.dataset.requires || ROLE_ACTION_SCOPES[el.dataset.action] || "";
}

// ---------------------------------------------------------------------------
// Gating the DOM
// ---------------------------------------------------------------------------

// gateElement disables el (with a tooltip) when the role may not use it, and
// restores it when a later sign-in may.
function gateElement(el) {
  const need = roleNeed(el);
  if (!need) return;
  const blocked = !!auth.user && !can(need);
  if (blocked) {
    if (!el.hasAttribute("data-role-gated")) {
      el.setAttribute("data-role-gated", "");
      el.dataset.roleTitle = el.getAttribute("title") || "";
    }
    // Only write what differs: the observer watches these attributes, so code that
    // re-enables a button (connection gating, finished requests) is undone, and
    // unchanged writes would loop.
    if ("disabled" in el && !el.disabled) el.disabled = true;
    if (el.getAttribute("aria-disabled") !== "true") el.setAttribute("aria-disabled", "true");
    const text = roleDeniedText();
    if (el.getAttribute("title") !== text) el.setAttribute("title", text);
  } else if (el.hasAttribute("data-role-gated")) {
    el.removeAttribute("data-role-gated");
    if ("disabled" in el) el.disabled = false;
    el.removeAttribute("aria-disabled");
    if (el.dataset.roleTitle) el.setAttribute("title", el.dataset.roleTitle);
    else el.removeAttribute("title");
    delete el.dataset.roleTitle;
  }
}

// gateSettingsForm makes a settings form read-only below admin (the settings API
// takes changes from administrators only) and says so.
function gateSettingsForm(form) {
  const readonly = !!auth.user && !can("admin");
  form.querySelectorAll("input, select, textarea, button").forEach(el => {
    if (el.dataset.action === "settings-section") return;
    if (readonly) {
      if (!el.hasAttribute("data-role-readonly")) {
        el.setAttribute("data-role-readonly", "");
        el.dataset.roleWasDisabled = el.disabled ? "1" : "";
      }
      if (!el.disabled) el.disabled = true;
    } else if (el.hasAttribute("data-role-readonly")) {
      el.removeAttribute("data-role-readonly");
      el.disabled = el.dataset.roleWasDisabled === "1";
      delete el.dataset.roleWasDisabled;
    }
  });
  let note = form.querySelector(":scope > .role-readonly-note");
  if (readonly && !note) {
    note = document.createElement("p");
    note.className = "notice role-readonly-note";
    note.setAttribute("data-i18n", "settings.user_role_settings_readonly");
    note.textContent = t("settings.user_role_settings_readonly");
    form.prepend(note);
  } else if (!readonly && note) {
    note.remove();
  }
}

// gateAll applies the role to every gated element and settings form.
function gateAll() {
  document.querySelectorAll("[data-action], [data-requires]").forEach(gateElement);
  document.querySelectorAll("form.settings-form").forEach(gateSettingsForm);
}

let roleGatePending = false;

// scheduleGate re-applies the role once after a batch of DOM changes (lists and
// menus are re-rendered all the time).
function scheduleGate() {
  if (roleGatePending) return;
  roleGatePending = true;
  queueMicrotask(() => {
    roleGatePending = false;
    gateAll();
  });
}

// applyRole is called by setAuth and clearAuth: it records the role on <body> (CSS
// hides administrator-only areas from the others), shows it in the user menu,
// gates the DOM and re-renders what depends on it.
function applyRole() {
  const body = document.body;
  if (!body) return;
  if (auth.user) {
    body.dataset.role = auth.role || "";
    body.dataset.scope = auth.scope || "read";
  } else {
    delete body.dataset.role;
    delete body.dataset.scope;
  }
  const menuRole = document.getElementById("user-menu-role");
  if (menuRole) {
    menuRole.textContent = auth.user && auth.role ? tf("settings.user_role_menu", { role: roleLabel(auth.role) }) : "";
    menuRole.hidden = !(auth.user && auth.role);
  }
  gateAll();
  // A non-admin who had an administrator-only section open is moved off it.
  const open = document.querySelector(".settings-nav-btn.active");
  if (open && ADMIN_SETTINGS_SECTIONS.includes(open.dataset.section) && auth.user && !can("admin")) {
    showSettingsSection("general", false);
  }
}

// guardOpeners wraps the dialogs the palette and shortcuts open directly, so a
// role that may not submit them gets a notice instead of a form.
function guardOpeners() {
  Object.keys(ROLE_GUARDED_OPENERS).forEach(name => {
    const original = window[name];
    if (typeof original !== "function" || original.roleGuarded) return;
    const need = ROLE_GUARDED_OPENERS[name];
    const guarded = function (...args) {
      if (auth.user && !can(need)) {
        showToast(roleDeniedText(), "info");
        return undefined;
      }
      return original.apply(this, args);
    };
    guarded.roleGuarded = true;
    window[name] = guarded;
  });
}

// ---------------------------------------------------------------------------
// Users and API keys (called by app.js renderUsers and renderApiKeys)
// ---------------------------------------------------------------------------

// userRoleSelect renders the role of u: a select for administrators, disabled for
// their own user, for the last administrator and for single sign-on users whose
// role the identity provider's group mappings decide (managed).
function userRoleSelect(u, self, lastAdmin, managed) {
  const role = USER_ROLES.includes(u.role) ? u.role : "viewer";
  const locked = self || lastAdmin || !!managed;
  const title = self ? t("settings.user_role_self") : lastAdmin ? t("settings.user_role_last_admin")
    : managed ? t("sso.role_managed") : "";
  const options = USER_ROLES.map(r =>
    `<option value="${r}"${r === role ? " selected" : ""}>${escapeHtml(roleLabel(r))}</option>`).join("");
  return `<select class="form-select user-role-select" data-id="${escapeHtml(u.id)}" data-role="${role}"
    aria-label="${escapeHtml(tf("settings.user_role_select_label", { name: u.username }))}"${locked ? ` disabled title="${escapeHtml(title)}"` : ""}>${options}</select>`;
}

// keyCapNote marks a key whose creator's role caps it below its own scope.
function keyCapNote(k) {
  const eff = String(k.effective_scope || "");
  if (!eff || eff === k.scope || !API_KEY_SCOPES.includes(eff)) return "";
  const scope = t(`settings.scope_${eff}`);
  return ` <span class="chip chip-warn" title="${escapeHtml(tf("settings.user_role_key_capped_title", { scope }))}">${escapeHtml(tf("settings.user_role_key_capped", { scope }))}</span>`;
}

// changeUserRole asks before a demotion, then sets the role of the user id.
async function changeUserRole(select) {
  const id = select.dataset.id || "";
  const from = select.dataset.role || "";
  const to = select.value;
  const user = state.users.find(u => u.id === id);
  const name = user ? user.username : id;
  if (SCOPE_RANK[ROLE_SCOPES[to]] < SCOPE_RANK[ROLE_SCOPES[from]]) {
    const ok = await confirmDialog({
      title: t("settings.user_role_confirm_title"),
      body: tf("settings.user_role_confirm_demote", { name, from: roleLabel(from), to: roleLabel(to) }),
      danger: true,
      confirmLabel: t("dialog.confirm")
    });
    if (!ok) {
      select.value = from;
      return;
    }
  }
  select.disabled = true;
  try {
    const json = await apiJSON(`/api/v1/users/${encodeURIComponent(id)}/role`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ role: to })
    });
    if (json.success) {
      showToast(tf("settings.user_role_changed", { name, role: roleLabel(to) }), "success");
    } else {
      showToast(json.error || t("toasts.request_failed"), "error");
    }
  } catch (err) {
    showToast(err.message, "error");
  }
  loadUsers();
  loadApiKeys();
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

function setupRoles() {
  guardOpeners();
  // A gated element never acts, even where a handler does not check disabled.
  document.addEventListener("click", (e) => {
    const el = e.target instanceof Element ? e.target.closest("[data-role-gated]") : null;
    if (!el) return;
    e.preventDefault();
    e.stopImmediatePropagation();
  }, true);
  document.addEventListener("change", (e) => {
    const select = e.target instanceof Element ? e.target.closest(".user-role-select") : null;
    if (select) changeUserRole(select);
  });
  new MutationObserver(scheduleGate).observe(document.body, {
    childList: true, subtree: true, attributes: true, attributeFilter: ["disabled", "title"]
  });
  onLanguageChange(() => applyRole());
  applyRole();
}

document.addEventListener("DOMContentLoaded", setupRoles);
