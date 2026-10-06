/**
 * MongoRescue dashboard: post-restore commands of a connection.
 *
 * The connection form's "Post-restore commands" editor (administrators only):
 * a JSON array of {database, command} that every safe-clone restore into the
 * connection runs against its clones before it completes, for example to
 * re-apply erasures (post_restore_commands, see docs/privacy.md). The editor
 * checks the JSON and the allowed command names as you type; the server checks
 * everything again.
 *
 * Loaded after app.js and trust.js (mergeTranslations); app.js calls
 * postRestoreFillConnectionForm() and postRestoreConnectionPayload(). Server
 * values reach the DOM only through textContent and form values.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const POSTRESTORE_TRANSLATIONS = {
  en: {
    postrestore: {
      label: "Post-restore commands",
      hint: "Optional. A JSON array of {\"database\", \"command\"} run against the clones of every safe-clone restore into this connection before it completes, for example to re-apply erasures. \"database\" is a restored database or \"*\" for all of them. Allowed: delete, update, findAndModify, dropIndexes, collMod, drop. A failing command fails the restore and keeps the clone.",
      invalid_json: "Not valid JSON: {error}",
      not_array: "Enter a JSON array, for example [{\"database\": \"*\", \"command\": {\"drop\": \"tmp\"}}].",
      too_many: "At most {max} commands.",
      item_object: "Item {n}: enter an object with \"database\" and \"command\".",
      item_database: "Item {n}: \"database\" must name a database, or \"*\" for every restored database (not admin, config or local).",
      item_command: "Item {n}: \"command\" must be a command document such as {\"delete\": \"users\", \"deletes\": [...]}.",
      item_not_allowed: "Item {n}: {name} is not allowed. Allowed: {allowed}.",
      fix_first: "Fix the post-restore commands first.",
    },
  },
  tr: {
    postrestore: {
      label: "Geri yükleme sonrası komutlar",
      hint: "İsteğe bağlı. Bu bağlantıya yapılan her güvenli kopya geri yüklemesi tamamlanmadan önce kopyalarda çalıştırılan {\"database\", \"command\"} öğelerinden oluşan bir JSON dizisi; örneğin silme taleplerini yeniden uygulamak için. \"database\" geri yüklenen bir veritabanı ya da hepsi için \"*\" olur. İzin verilenler: delete, update, findAndModify, dropIndexes, collMod, drop. Başarısız bir komut geri yüklemeyi başarısız kılar ve kopyayı korur.",
      invalid_json: "Geçerli bir JSON değil: {error}",
      not_array: "Bir JSON dizisi girin, örneğin [{\"database\": \"*\", \"command\": {\"drop\": \"tmp\"}}].",
      too_many: "En fazla {max} komut.",
      item_object: "Öğe {n}: \"database\" ve \"command\" içeren bir nesne girin.",
      item_database: "Öğe {n}: \"database\" bir veritabanı adı ya da geri yüklenen tüm veritabanları için \"*\" olmalı (admin, config veya local olamaz).",
      item_command: "Öğe {n}: \"command\" {\"delete\": \"users\", \"deletes\": [...]} gibi bir komut belgesi olmalı.",
      item_not_allowed: "Öğe {n}: {name} izinli değil. İzin verilenler: {allowed}.",
      fix_first: "Önce geri yükleme sonrası komutları düzeltin.",
    },
  },
  de: {
    postrestore: {
      label: "Befehle nach der Wiederherstellung",
      hint: "Optional. Ein JSON-Array aus {\"database\", \"command\"}, das jede Wiederherstellung als sichere Kopie in diese Verbindung vor ihrem Abschluss auf ihren Kopien ausführt, etwa um Löschungen erneut anzuwenden. \"database\" ist eine wiederhergestellte Datenbank oder \"*\" für alle. Erlaubt: delete, update, findAndModify, dropIndexes, collMod, drop. Ein fehlschlagender Befehl lässt die Wiederherstellung fehlschlagen und behält die Kopie.",
      invalid_json: "Kein gültiges JSON: {error}",
      not_array: "Geben Sie ein JSON-Array ein, zum Beispiel [{\"database\": \"*\", \"command\": {\"drop\": \"tmp\"}}].",
      too_many: "Höchstens {max} Befehle.",
      item_object: "Eintrag {n}: Geben Sie ein Objekt mit \"database\" und \"command\" ein.",
      item_database: "Eintrag {n}: \"database\" muss eine Datenbank nennen oder \"*\" für alle wiederhergestellten Datenbanken (nicht admin, config oder local).",
      item_command: "Eintrag {n}: \"command\" muss ein Befehlsdokument sein, etwa {\"delete\": \"users\", \"deletes\": [...]}.",
      item_not_allowed: "Eintrag {n}: {name} ist nicht erlaubt. Erlaubt: {allowed}.",
      fix_first: "Korrigieren Sie zuerst die Befehle nach der Wiederherstellung.",
    },
  },
  es: {
    postrestore: {
      label: "Comandos posteriores a la restauración",
      hint: "Opcional. Un array JSON de {\"database\", \"command\"} que toda restauración como copia segura en esta conexión ejecuta sobre sus copias antes de completarse, por ejemplo para volver a aplicar supresiones. \"database\" es una base de datos restaurada o \"*\" para todas. Permitidos: delete, update, findAndModify, dropIndexes, collMod, drop. Un comando que falla hace fallar la restauración y conserva la copia.",
      invalid_json: "JSON no válido: {error}",
      not_array: "Introduzca un array JSON, por ejemplo [{\"database\": \"*\", \"command\": {\"drop\": \"tmp\"}}].",
      too_many: "Como máximo {max} comandos.",
      item_object: "Elemento {n}: introduzca un objeto con \"database\" y \"command\".",
      item_database: "Elemento {n}: \"database\" debe nombrar una base de datos, o \"*\" para todas las restauradas (no admin, config ni local).",
      item_command: "Elemento {n}: \"command\" debe ser un documento de comando como {\"delete\": \"users\", \"deletes\": [...]}.",
      item_not_allowed: "Elemento {n}: {name} no está permitido. Permitidos: {allowed}.",
      fix_first: "Corrija primero los comandos posteriores a la restauración.",
    },
  },
  fr: {
    postrestore: {
      label: "Commandes après restauration",
      hint: "Facultatif. Un tableau JSON de {\"database\", \"command\"} que chaque restauration en copie sûre dans cette connexion exécute sur ses copies avant de se terminer, par exemple pour réappliquer des effacements. \"database\" est une base restaurée ou \"*\" pour toutes. Autorisées : delete, update, findAndModify, dropIndexes, collMod, drop. Une commande en échec fait échouer la restauration et conserve la copie.",
      invalid_json: "JSON non valide : {error}",
      not_array: "Saisissez un tableau JSON, par exemple [{\"database\": \"*\", \"command\": {\"drop\": \"tmp\"}}].",
      too_many: "{max} commandes au maximum.",
      item_object: "Élément {n} : saisissez un objet avec \"database\" et \"command\".",
      item_database: "Élément {n} : \"database\" doit nommer une base, ou \"*\" pour toutes les bases restaurées (pas admin, config ni local).",
      item_command: "Élément {n} : \"command\" doit être un document de commande comme {\"delete\": \"users\", \"deletes\": [...]}.",
      item_not_allowed: "Élément {n} : {name} n'est pas autorisée. Autorisées : {allowed}.",
      fix_first: "Corrigez d'abord les commandes après restauration.",
    },
  },
  zh: {
    postrestore: {
      label: "恢复后命令",
      hint: "可选。由 {\"database\", \"command\"} 组成的 JSON 数组：每次以安全副本方式恢复到此连接时，在完成之前对副本执行，例如重新执行数据删除请求。\"database\" 为已恢复的数据库，或用 \"*\" 表示全部。允许：delete、update、findAndModify、dropIndexes、collMod、drop。命令失败会使恢复失败并保留副本。",
      invalid_json: "不是有效的 JSON：{error}",
      not_array: "请输入 JSON 数组，例如 [{\"database\": \"*\", \"command\": {\"drop\": \"tmp\"}}]。",
      too_many: "最多 {max} 条命令。",
      item_object: "第 {n} 项：请输入包含 \"database\" 和 \"command\" 的对象。",
      item_database: "第 {n} 项：\"database\" 必须是数据库名称，或用 \"*\" 表示所有已恢复的数据库（不能是 admin、config 或 local）。",
      item_command: "第 {n} 项：\"command\" 必须是命令文档，例如 {\"delete\": \"users\", \"deletes\": [...]}。",
      item_not_allowed: "第 {n} 项：不允许 {name}。允许：{allowed}。",
      fix_first: "请先修正恢复后命令。",
    },
  },
  ja: {
    postrestore: {
      label: "リストア後のコマンド",
      hint: "任意。{\"database\", \"command\"} の JSON 配列で、この接続への安全なクローンのリストアが完了する前にクローンに対して実行されます（例：削除要求の再適用）。\"database\" はリストアされたデータベース、またはすべてを表す \"*\" です。使用可能：delete、update、findAndModify、dropIndexes、collMod、drop。コマンドが失敗するとリストアは失敗し、クローンは保持されます。",
      invalid_json: "有効な JSON ではありません：{error}",
      not_array: "JSON 配列を入力してください。例：[{\"database\": \"*\", \"command\": {\"drop\": \"tmp\"}}]",
      too_many: "コマンドは最大 {max} 件です。",
      item_object: "項目 {n}：\"database\" と \"command\" を持つオブジェクトを入力してください。",
      item_database: "項目 {n}：\"database\" はデータベース名、またはリストアされたすべてのデータベースを表す \"*\" にしてください（admin、config、local は不可）。",
      item_command: "項目 {n}：\"command\" は {\"delete\": \"users\", \"deletes\": [...]} のようなコマンドドキュメントにしてください。",
      item_not_allowed: "項目 {n}：{name} は使用できません。使用可能：{allowed}。",
      fix_first: "先にリストア後のコマンドを修正してください。",
    },
  },
  ru: {
    postrestore: {
      label: "Команды после восстановления",
      hint: "Необязательно. JSON-массив из {\"database\", \"command\"}, который каждое восстановление в безопасную копию в это подключение выполняет над своими копиями перед завершением, например чтобы повторно применить удаления данных. \"database\" — восстановленная база данных или \"*\" для всех. Разрешены: delete, update, findAndModify, dropIndexes, collMod, drop. Ошибка команды приводит к ошибке восстановления, копия сохраняется.",
      invalid_json: "Некорректный JSON: {error}",
      not_array: "Введите JSON-массив, например [{\"database\": \"*\", \"command\": {\"drop\": \"tmp\"}}].",
      too_many: "Не более {max} команд.",
      item_object: "Элемент {n}: введите объект с \"database\" и \"command\".",
      item_database: "Элемент {n}: \"database\" должно называть базу данных или \"*\" для всех восстановленных баз (не admin, config или local).",
      item_command: "Элемент {n}: \"command\" должно быть документом команды, например {\"delete\": \"users\", \"deletes\": [...]}.",
      item_not_allowed: "Элемент {n}: {name} не разрешена. Разрешены: {allowed}.",
      fix_first: "Сначала исправьте команды после восстановления.",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(POSTRESTORE_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], POSTRESTORE_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Editor
// ---------------------------------------------------------------------------

// The commands the server allows (internal/postrestore.AllowedCommands) and its
// limit of commands per connection.
const POSTRESTORE_ALLOWED = ["delete", "update", "findAndModify", "dropIndexes", "collMod", "drop"];
const POSTRESTORE_MAX = 100;
const POSTRESTORE_SYSTEM_DBS = ["admin", "config", "local"];

function postRestoreField() {
  return document.getElementById("connection-post-restore");
}

// postRestoreParse checks the editor's text and returns {commands} or {error}.
function postRestoreParse(text) {
  if (!text.trim()) return { commands: [] };
  let value;
  try {
    value = JSON.parse(text);
  } catch (err) {
    return { error: tf("postrestore.invalid_json", { error: err.message }) };
  }
  if (!Array.isArray(value)) return { error: t("postrestore.not_array") };
  if (value.length > POSTRESTORE_MAX) return { error: tf("postrestore.too_many", { max: POSTRESTORE_MAX }) };
  for (const [i, item] of value.entries()) {
    const n = i + 1;
    if (!item || typeof item !== "object" || Array.isArray(item)) return { error: tf("postrestore.item_object", { n }) };
    const db = item.database;
    if (typeof db !== "string" || !db.trim() || POSTRESTORE_SYSTEM_DBS.includes(db)) return { error: tf("postrestore.item_database", { n }) };
    const cmd = item.command;
    if (!cmd || typeof cmd !== "object" || Array.isArray(cmd) || !Object.keys(cmd).length) return { error: tf("postrestore.item_command", { n }) };
    const name = Object.keys(cmd)[0];
    if (!POSTRESTORE_ALLOWED.includes(name)) {
      return { error: tf("postrestore.item_not_allowed", { n, name, allowed: POSTRESTORE_ALLOWED.join(", ") }) };
    }
  }
  return { commands: value };
}

// postRestoreValidate shows the outcome of the check next to the editor and
// returns the parsed commands, or null when they are invalid.
function postRestoreValidate() {
  const field = postRestoreField();
  if (!field) return [];
  const res = postRestoreParse(field.value);
  const error = document.getElementById("connection-post-restore-error");
  field.setCustomValidity(res.error || "");
  field.setAttribute("aria-invalid", res.error ? "true" : "false");
  if (error) {
    error.textContent = res.error || "";
    error.hidden = !res.error;
  }
  return res.error ? null : res.commands;
}

function postRestoreFillConnectionForm(c) {
  const field = postRestoreField();
  if (!field) return;
  const cmds = c && Array.isArray(c.post_restore_commands) ? c.post_restore_commands : [];
  field.value = cmds.length ? JSON.stringify(cmds, null, 2) : "";
  postRestoreValidate();
}

// postRestoreConnectionPayload returns the post_restore_commands field of the
// connection payload ({} when the editor is absent or the user is no
// administrator), or null when the commands are invalid.
function postRestoreConnectionPayload() {
  const field = postRestoreField();
  if (!field || field.disabled) return {};
  const commands = postRestoreValidate();
  if (commands === null) {
    field.focus();
    showToast(t("postrestore.fix_first"), "error");
    return null;
  }
  return { post_restore_commands: commands };
}

function setupPostRestore() {
  const field = postRestoreField();
  if (field) field.addEventListener("input", postRestoreValidate);
  if (typeof onLanguageChange === "function") onLanguageChange(() => { if (postRestoreField()) postRestoreValidate(); });
}

document.addEventListener("DOMContentLoaded", setupPostRestore);
