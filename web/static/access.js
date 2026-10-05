/**
 * MongoRescue dashboard: per-connection access (docs/design/roles.md).
 *
 * A user or an API key reaches every connection (the default) or only some. The
 * users table gets a Connections column whose button opens a checklist
 * (PUT /api/v1/users/{id}/connections), the Add user and Create API key dialogs a
 * checklist for administrators (connection_ids), the key list a chip with the
 * connections a key reaches, the single sign-on group mappings a connection select
 * per mapping, and the user menu the caller's own connections. A limited caller
 * needs nothing else here: every list the server returns is already limited to its
 * connections, and the server stays authoritative.
 *
 * Loaded after app.js, trust.js (mergeTranslations), roles.js and sso.js. Server
 * values reach the DOM only through escapeHtml and textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const ACCESS_TRANSLATIONS = {
  en: {
    access: {
      label: "Connections",
      col_connections: "Connections",
      all: "All connections",
      none: "No connection",
      some: "Connections: {count}",
      none_available: "No connections yet.",
      deleted: "{id} (deleted)",
      user_hint: "Check All connections, or the connections the user may use; none checked means no connection. Administrators always have every connection.",
      key_hint: "Check All connections (those you may use), or the connections the key may use; none checked means no connection. Admin keys always have every connection.",
      edit_title: "Connections of {name}",
      edit_hint: "The user sees, backs up and restores only these connections, and so do their API keys. None checked means no connection.",
      edit_button_label: "Connections of {name}: {value}",
      save: "Save",
      saved: "Connections of {name} saved.",
      admin_all: "Administrators always have every connection.",
      managed: "The single sign-on group mappings decide this user's connections.",
      limited_note: "Your connections: {names}",
      limited_none: "You have no connection.",
      key_limited: "Connections: {count}",
      key_limited_title: "This key reaches only: {names}",
      key_none: "No connection",
      key_none_title: "The key's connections and its creator's do not overlap: it reaches no connection.",
      mapping_label: "Connections of the mapping; none selected means every connection",
      mapping_admin: "Admin mappings always reach every connection."
    }
  },
  tr: {
    access: {
      label: "Bağlantılar",
      col_connections: "Bağlantılar",
      all: "Tüm bağlantılar",
      none: "Bağlantı yok",
      some: "Bağlantılar: {count}",
      none_available: "Henüz bağlantı yok.",
      deleted: "{id} (silinmiş)",
      user_hint: "Tüm bağlantıları ya da kullanıcının kullanabileceği bağlantıları işaretleyin; hiçbiri işaretli değilse hiçbir bağlantı yoktur. Yöneticiler her zaman tüm bağlantılara erişir.",
      key_hint: "Tüm bağlantıları (sizin kullanabildikleriniz) ya da anahtarın kullanabileceği bağlantıları işaretleyin; hiçbiri işaretli değilse hiçbir bağlantı yoktur. Yönetici anahtarları her zaman tüm bağlantılara erişir.",
      edit_title: "{name} kullanıcısının bağlantıları",
      edit_hint: "Kullanıcı yalnızca bu bağlantıları görür, yedekler ve geri yükler; API anahtarları da öyle. Hiçbiri işaretli değilse hiçbir bağlantı yoktur.",
      edit_button_label: "{name} kullanıcısının bağlantıları: {value}",
      save: "Kaydet",
      saved: "{name} kullanıcısının bağlantıları kaydedildi.",
      admin_all: "Yöneticiler her zaman tüm bağlantılara erişir.",
      managed: "Bu kullanıcının bağlantılarını çoklu oturum açma grup eşlemeleri belirler.",
      limited_note: "Bağlantılarınız: {names}",
      limited_none: "Hiçbir bağlantınız yok.",
      key_limited: "Bağlantılar: {count}",
      key_limited_title: "Bu anahtar yalnızca şunlara erişir: {names}",
      key_none: "Bağlantı yok",
      key_none_title: "Anahtarın bağlantıları ile oluşturanınkiler kesişmiyor: hiçbir bağlantıya erişmez.",
      mapping_label: "Eşlemenin bağlantıları; seçim yoksa tüm bağlantılar",
      mapping_admin: "Yönetici eşlemeleri her zaman tüm bağlantılara erişir."
    }
  },
  de: {
    access: {
      label: "Verbindungen",
      col_connections: "Verbindungen",
      all: "Alle Verbindungen",
      none: "Keine Verbindung",
      some: "Verbindungen: {count}",
      none_available: "Noch keine Verbindungen.",
      deleted: "{id} (gelöscht)",
      user_hint: "Alle Verbindungen ankreuzen oder die Verbindungen, die der Benutzer nutzen darf; ohne Häkchen hat er keine Verbindung. Administratoren haben immer alle Verbindungen.",
      key_hint: "Alle Verbindungen ankreuzen (die Sie nutzen dürfen) oder die Verbindungen des Schlüssels; ohne Häkchen hat er keine Verbindung. Admin-Schlüssel haben immer alle Verbindungen.",
      edit_title: "Verbindungen von {name}",
      edit_hint: "Der Benutzer sieht, sichert und stellt nur diese Verbindungen wieder her, ebenso seine API-Schlüssel. Ohne Häkchen hat er keine Verbindung.",
      edit_button_label: "Verbindungen von {name}: {value}",
      save: "Speichern",
      saved: "Verbindungen von {name} gespeichert.",
      admin_all: "Administratoren haben immer alle Verbindungen.",
      managed: "Die Gruppenzuordnungen der Einmalanmeldung bestimmen die Verbindungen dieses Benutzers.",
      limited_note: "Ihre Verbindungen: {names}",
      limited_none: "Sie haben keine Verbindung.",
      key_limited: "Verbindungen: {count}",
      key_limited_title: "Dieser Schlüssel erreicht nur: {names}",
      key_none: "Keine Verbindung",
      key_none_title: "Die Verbindungen des Schlüssels und die seines Erstellers überschneiden sich nicht: Er erreicht keine Verbindung.",
      mapping_label: "Verbindungen der Zuordnung; ohne Auswahl alle Verbindungen",
      mapping_admin: "Admin-Zuordnungen erreichen immer alle Verbindungen."
    }
  },
  es: {
    access: {
      label: "Conexiones",
      col_connections: "Conexiones",
      all: "Todas las conexiones",
      none: "Ninguna conexión",
      some: "Conexiones: {count}",
      none_available: "Aún no hay conexiones.",
      deleted: "{id} (eliminada)",
      user_hint: "Marque Todas las conexiones o las conexiones que el usuario puede usar; sin ninguna marcada no tiene ninguna conexión. Los administradores siempre tienen todas las conexiones.",
      key_hint: "Marque Todas las conexiones (las que usted puede usar) o las conexiones de la clave; sin ninguna marcada no tiene ninguna conexión. Las claves de administrador siempre tienen todas las conexiones.",
      edit_title: "Conexiones de {name}",
      edit_hint: "El usuario solo ve, respalda y restaura estas conexiones, y sus claves de API también. Sin ninguna marcada no tiene ninguna conexión.",
      edit_button_label: "Conexiones de {name}: {value}",
      save: "Guardar",
      saved: "Conexiones de {name} guardadas.",
      admin_all: "Los administradores siempre tienen todas las conexiones.",
      managed: "Las asignaciones de grupos del inicio de sesión único deciden las conexiones de este usuario.",
      limited_note: "Sus conexiones: {names}",
      limited_none: "No tiene ninguna conexión.",
      key_limited: "Conexiones: {count}",
      key_limited_title: "Esta clave solo alcanza: {names}",
      key_none: "Ninguna conexión",
      key_none_title: "Las conexiones de la clave y las de su creador no coinciden: no alcanza ninguna conexión.",
      mapping_label: "Conexiones de la asignación; sin selección, todas las conexiones",
      mapping_admin: "Las asignaciones de administrador siempre alcanzan todas las conexiones."
    }
  },
  fr: {
    access: {
      label: "Connexions",
      col_connections: "Connexions",
      all: "Toutes les connexions",
      none: "Aucune connexion",
      some: "Connexions : {count}",
      none_available: "Aucune connexion pour l'instant.",
      deleted: "{id} (supprimée)",
      user_hint: "Cochez Toutes les connexions, ou les connexions que l'utilisateur peut utiliser ; sans case cochée, il n'a aucune connexion. Les administrateurs ont toujours toutes les connexions.",
      key_hint: "Cochez Toutes les connexions (celles que vous pouvez utiliser), ou les connexions de la clé ; sans case cochée, elle n'a aucune connexion. Les clés administrateur ont toujours toutes les connexions.",
      edit_title: "Connexions de {name}",
      edit_hint: "L'utilisateur ne voit, ne sauvegarde et ne restaure que ces connexions, ses clés d'API aussi. Sans case cochée, il n'a aucune connexion.",
      edit_button_label: "Connexions de {name} : {value}",
      save: "Enregistrer",
      saved: "Connexions de {name} enregistrées.",
      admin_all: "Les administrateurs ont toujours toutes les connexions.",
      managed: "Les correspondances de groupes de l'authentification unique décident des connexions de cet utilisateur.",
      limited_note: "Vos connexions : {names}",
      limited_none: "Vous n'avez aucune connexion.",
      key_limited: "Connexions : {count}",
      key_limited_title: "Cette clé n'atteint que : {names}",
      key_none: "Aucune connexion",
      key_none_title: "Les connexions de la clé et celles de son créateur ne se recoupent pas : elle n'atteint aucune connexion.",
      mapping_label: "Connexions de la correspondance ; sans sélection, toutes les connexions",
      mapping_admin: "Les correspondances administrateur atteignent toujours toutes les connexions."
    }
  },
  zh: {
    access: {
      label: "连接",
      col_connections: "连接",
      all: "所有连接",
      none: "无连接",
      some: "连接：{count}",
      none_available: "还没有连接。",
      deleted: "{id}（已删除）",
      user_hint: "勾选“所有连接”，或勾选该用户可使用的连接；都不勾选表示没有任何连接。管理员始终拥有所有连接。",
      key_hint: "勾选“所有连接”（即您可使用的连接），或勾选该密钥可使用的连接；都不勾选表示没有任何连接。管理员密钥始终拥有所有连接。",
      edit_title: "{name} 的连接",
      edit_hint: "该用户只能查看、备份和恢复这些连接，其 API 密钥也是如此。都不勾选表示没有任何连接。",
      edit_button_label: "{name} 的连接：{value}",
      save: "保存",
      saved: "已保存 {name} 的连接。",
      admin_all: "管理员始终拥有所有连接。",
      managed: "此用户的连接由单点登录组映射决定。",
      limited_note: "您的连接：{names}",
      limited_none: "您没有任何连接。",
      key_limited: "连接：{count}",
      key_limited_title: "此密钥只能访问：{names}",
      key_none: "无连接",
      key_none_title: "密钥的连接与其创建者的连接没有交集：它无法访问任何连接。",
      mapping_label: "映射的连接；不选择表示所有连接",
      mapping_admin: "管理员映射始终拥有所有连接。"
    }
  },
  ja: {
    access: {
      label: "接続",
      col_connections: "接続",
      all: "すべての接続",
      none: "接続なし",
      some: "接続: {count}",
      none_available: "接続はまだありません。",
      deleted: "{id}（削除済み）",
      user_hint: "「すべての接続」か、ユーザーが利用できる接続をチェックしてください。何もチェックしないと接続はありません。管理者は常にすべての接続を利用できます。",
      key_hint: "「すべての接続」（あなたが利用できる接続）か、キーが利用できる接続をチェックしてください。何もチェックしないと接続はありません。管理者キーは常にすべての接続を利用できます。",
      edit_title: "{name} の接続",
      edit_hint: "このユーザーはこれらの接続だけを表示、バックアップ、復元できます。API キーも同様です。何もチェックしないと接続はありません。",
      edit_button_label: "{name} の接続: {value}",
      save: "保存",
      saved: "{name} の接続を保存しました。",
      admin_all: "管理者は常にすべての接続を利用できます。",
      managed: "このユーザーの接続はシングルサインオンのグループマッピングで決まります。",
      limited_note: "あなたの接続: {names}",
      limited_none: "利用できる接続はありません。",
      key_limited: "接続: {count}",
      key_limited_title: "このキーが利用できるのは次の接続だけです: {names}",
      key_none: "接続なし",
      key_none_title: "キーの接続と作成者の接続が重なっていないため、どの接続も利用できません。",
      mapping_label: "マッピングの接続。選択しない場合はすべての接続",
      mapping_admin: "管理者マッピングは常にすべての接続を利用できます。"
    }
  },
  ru: {
    access: {
      label: "Подключения",
      col_connections: "Подключения",
      all: "Все подключения",
      none: "Нет подключений",
      some: "Подключения: {count}",
      none_available: "Подключений пока нет.",
      deleted: "{id} (удалено)",
      user_hint: "Отметьте «Все подключения» или подключения, доступные пользователю; если ничего не отмечено, подключений нет. Администраторы всегда имеют доступ ко всем подключениям.",
      key_hint: "Отметьте «Все подключения» (доступные вам) или подключения ключа; если ничего не отмечено, подключений нет. Ключи администратора всегда имеют доступ ко всем подключениям.",
      edit_title: "Подключения пользователя {name}",
      edit_hint: "Пользователь видит, резервирует и восстанавливает только эти подключения, как и его API-ключи. Если ничего не отмечено, подключений нет.",
      edit_button_label: "Подключения пользователя {name}: {value}",
      save: "Сохранить",
      saved: "Подключения пользователя {name} сохранены.",
      admin_all: "Администраторы всегда имеют доступ ко всем подключениям.",
      managed: "Подключения этого пользователя определяют сопоставления групп единого входа.",
      limited_note: "Ваши подключения: {names}",
      limited_none: "У вас нет ни одного подключения.",
      key_limited: "Подключения: {count}",
      key_limited_title: "Этот ключ имеет доступ только к: {names}",
      key_none: "Нет подключений",
      key_none_title: "Подключения ключа и его создателя не пересекаются: ключ не имеет доступа ни к одному подключению.",
      mapping_label: "Подключения сопоставления; без выбора — все подключения",
      mapping_admin: "Сопоставления администратора всегда имеют доступ ко всем подключениям."
    }
  }
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(ACCESS_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], ACCESS_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// accessConnectionName returns the name of connection id, or the ID marked deleted.
function accessConnectionName(id) {
  const c = (state.connections || []).find(x => x.id === id);
  return c ? String(c.name || id) : tf("access.deleted", { id });
}

function accessConnectionNames(ids) {
  return ids.map(accessConnectionName).join(", ");
}

// accessEnsureConnections loads the connection list (once, unless fresh), for the
// names and the checklists.
let accessConnectionsLoading = null;
function accessEnsureConnections(fresh) {
  if ((state.loaded.connections && !fresh) || typeof loadConnections !== "function") return Promise.resolve();
  if (!accessConnectionsLoading) accessConnectionsLoading = loadConnections().finally(() => { accessConnectionsLoading = null; });
  return accessConnectionsLoading;
}

// accessOf reads the connection access of a server object: every connection only
// with all_connections true; otherwise exactly connection_ids (none when empty).
function accessOf(o) {
  const all = !!(o && o.all_connections === true);
  const ids = !all && o && Array.isArray(o.connection_ids) ? o.connection_ids.map(String) : [];
  return { all, ids };
}

// fillConnectionChecklist renders, into containerId, an "All connections" toggle
// (id containerId-all) and one checkbox per connection, from access ({all, ids}).
// IDs of deleted connections stay listed (checked) so the server can drop them.
async function fillConnectionChecklist(containerId, access) {
  const box = document.getElementById(containerId);
  if (!box) return;
  const a = access || { all: true, ids: [] };
  // A dialog offers the connections as they are now.
  await accessEnsureConnections(true);
  const chosen = new Set((a.ids || []).map(String));
  const list = (state.connections || []).map(c => ({ id: String(c.id), name: String(c.name || c.id) }));
  chosen.forEach(id => {
    if (!list.some(c => c.id === id)) list.push({ id, name: tf("access.deleted", { id }) });
  });
  const toggle = `<label class="check-inline access-all"><input type="checkbox" id="${escapeHtml(containerId)}-all" data-access-all="${escapeHtml(containerId)}"${a.all ? " checked" : ""}> <span>${escapeHtml(t("access.all"))}</span></label>`;
  const boxes = list.length === 0
    ? `<span class="muted">${escapeHtml(t("access.none_available"))}</span>`
    : list.map(c => `<label class="check-inline"><input type="checkbox" class="access-connection" value="${escapeHtml(c.id)}"${chosen.has(c.id) ? " checked" : ""}${a.all ? " disabled" : ""}> <span>${escapeHtml(c.name)}</span></label>`).join("");
  box.innerHTML = toggle + boxes;
}

// readConnectionAccess returns the access chosen in containerId as a request body
// part: {all_connections: true} or {all_connections: false, connection_ids: [...]}
// (an empty list is no connection).
function readConnectionAccess(containerId) {
  const all = document.getElementById(`${containerId}-all`);
  if (!all || all.checked) return { all_connections: true };
  const ids = Array.from(document.querySelectorAll(`#${containerId} input.access-connection:checked`)).map(el => el.value);
  return { all_connections: false, connection_ids: ids };
}

// accessLabel describes an access: all, none or a count.
function accessLabel(a) {
  if (a.all) return t("access.all");
  return a.ids.length ? tf("access.some", { count: a.ids.length }) : t("access.none");
}

// ---------------------------------------------------------------------------
// Users, API keys and the user menu (called by app.js)
// ---------------------------------------------------------------------------

// userConnectionsCell shows a user's connections to administrators, with a button
// that edits them; administrators and users whose group mappings decide them get
// no button.
function userConnectionsCell(u, managed) {
  if (!can("admin")) return "";
  if (u.role === "admin") {
    return `<span class="muted" title="${escapeHtml(t("access.admin_all"))}">${escapeHtml(t("access.all"))}</span>`;
  }
  const a = accessOf(u);
  const value = accessLabel(a);
  const title = !a.all && a.ids.length ? accessConnectionNames(a.ids) : "";
  if (managed) {
    return `<span class="muted" title="${escapeHtml(t("access.managed"))}">${escapeHtml(value)}</span>`;
  }
  return `<button type="button" class="btn btn-ghost btn-sm" data-action="edit-user-connections" data-requires="admin" data-id="${escapeHtml(u.id)}"
    aria-label="${escapeHtml(tf("access.edit_button_label", { name: u.username, value }))}"${title ? ` title="${escapeHtml(title)}"` : ""}>${escapeHtml(value)}</button>`;
}

// keyConnectionsNote marks a key that reaches only some connections (its own,
// within its creator's).
function keyConnectionsNote(k) {
  if (k.effective_all_connections !== false) return "";
  const eff = Array.isArray(k.effective_connection_ids) ? k.effective_connection_ids : [];
  if (eff.length === 0) {
    return ` <span class="chip chip-warn" title="${escapeHtml(t("access.key_none_title"))}">${escapeHtml(t("access.key_none"))}</span>`;
  }
  return ` <span class="chip" title="${escapeHtml(tf("access.key_limited_title", { names: accessConnectionNames(eff) }))}">${escapeHtml(tf("access.key_limited", { count: eff.length }))}</span>`;
}

// renderAccessNote names the caller's own connections in the user menu.
function renderAccessNote() {
  const el = document.getElementById("user-menu-access");
  if (!el) return;
  const ids = Array.isArray(auth.connections) ? auth.connections : null;
  el.hidden = !ids;
  if (!ids) return;
  el.textContent = ids.length ? tf("access.limited_note", { names: accessConnectionNames(ids) }) : t("access.limited_none");
  if (ids.length && !state.loaded.connections) accessEnsureConnections().then(renderAccessNote);
}

// ---------------------------------------------------------------------------
// Editing a user's connections
// ---------------------------------------------------------------------------

async function openUserConnections(id) {
  const user = (state.users || []).find(u => u.id === id);
  if (!user) return;
  setValue("user-connections-user-id", id);
  setText("user-connections-title", tf("access.edit_title", { name: user.username }));
  hideFormError("user-connections-error");
  await fillConnectionChecklist("user-connections-edit", accessOf(user));
  openModal("modal-user-connections");
}

async function saveUserConnections(e) {
  e.preventDefault();
  const id = getValue("user-connections-user-id");
  const user = (state.users || []).find(u => u.id === id);
  try {
    const json = await apiJSON(`/api/v1/users/${encodeURIComponent(id)}/connections`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(readConnectionAccess("user-connections-edit"))
    });
    if (!json.success) {
      showFormError("user-connections-error", json.error || t("toasts.request_failed"));
      return;
    }
    showToast(tf("access.saved", { name: user ? user.username : id }), "success");
    closeModal("modal-user-connections");
    loadUsers();
    loadApiKeys();
  } catch (err) {
    showFormError("user-connections-error", err.message);
  }
}

// ---------------------------------------------------------------------------
// Single sign-on group mappings (called by sso.js ssoRenderMappings)
// ---------------------------------------------------------------------------

let accessMappingsWaiting = false;
const ACCESS_ALL_OPTION = "__all__";

// mappingConnectionsSelect renders the connections of mapping m (index i): a
// multiple select whose first option is "All connections"; nothing selected is no
// connection. Admin mappings always reach every connection: disabled.
function mappingConnectionsSelect(m, i) {
  if (!state.loaded.connections && !accessMappingsWaiting && typeof ssoRenderMappings === "function") {
    accessMappingsWaiting = true;
    accessEnsureConnections().then(() => {
      if (typeof ssoReadMappings === "function") ssoReadMappings();
      ssoRenderMappings();
    });
  }
  const a = accessOf(m);
  const chosen = new Set(a.ids);
  const list = (state.connections || []).map(c => ({ id: String(c.id), name: String(c.name || c.id) }));
  chosen.forEach(id => {
    if (!list.some(c => c.id === id)) list.push({ id, name: tf("access.deleted", { id }) });
  });
  const admin = m.role === "admin";
  const options = [`<option value="${ACCESS_ALL_OPTION}"${a.all || admin ? " selected" : ""}>${escapeHtml(t("access.all"))}</option>`]
    .concat(list.map(c => `<option value="${escapeHtml(c.id)}"${!a.all && chosen.has(c.id) && !admin ? " selected" : ""}>${escapeHtml(c.name)}</option>`))
    .join("");
  return `<select multiple class="form-select sso-mapping-connections" data-index="${i}" size="${Math.min(Math.max(list.length + 1, 2), 5)}"
    aria-label="${escapeHtml(t("access.mapping_label"))}" title="${escapeHtml(admin ? t("access.mapping_admin") : t("access.mapping_label"))}"${admin ? " disabled" : ""}>${options}</select>`;
}

// readMappingAccess returns the access chosen in a mapping's select.
function readMappingAccess(select) {
  const values = Array.from(select.selectedOptions).map(o => o.value);
  if (values.includes(ACCESS_ALL_OPTION)) return { all_connections: true, connection_ids: [] };
  return { all_connections: false, connection_ids: values };
}

// ---------------------------------------------------------------------------
// Setup
// ---------------------------------------------------------------------------

document.addEventListener("DOMContentLoaded", () => {
  document.addEventListener("click", (e) => {
    const btn = e.target instanceof Element ? e.target.closest('[data-action="edit-user-connections"]') : null;
    if (btn && !btn.disabled && can("admin")) openUserConnections(btn.dataset.id || "");
  });
  document.addEventListener("change", (e) => {
    const el = e.target instanceof Element ? e.target : null;
    if (!el) return;
    // The "All connections" toggle disables the list.
    if (el.matches("[data-access-all]")) {
      document.querySelectorAll(`#${el.dataset.accessAll} input.access-connection`).forEach(cb => { cb.disabled = el.checked; });
      return;
    }
    // An admin mapping reaches every connection: its select is disabled.
    const role = el.closest(".sso-mapping-role");
    if (!role) return;
    const select = document.querySelector(`.sso-mapping-connections[data-index="${role.dataset.index}"]`);
    if (select) {
      select.disabled = role.value === "admin";
      if (select.disabled) Array.from(select.options).forEach(o => { o.selected = o.value === ACCESS_ALL_OPTION; });
    }
  });
  const form = document.getElementById("form-user-connections");
  if (form) form.addEventListener("submit", saveUserConnections);
  if (typeof onLanguageChange === "function") onLanguageChange(renderAccessNote);
});
