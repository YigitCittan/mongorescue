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
      timeout: "Post-restore command timeout",
      timeout_hint: "Maximum run time of each post-restore command of a connection (1s to 24h); a command that takes longer fails the restore.",
      kept_title: "Restored clones still hold erased data",
      kept_desc: "A post-restore command failed, so these clones were kept for inspection without every command applied (such as erasures). Do not use them; drop them when done.",
      kept_item: "Restore {id}: {databases}",
      drop: "Drop clones",
      drop_confirm: "Drop {databases}? The data in them is deleted for good.",
      dropped: "Clones dropped.",
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
      timeout: "Geri yükleme sonrası komut zaman aşımı",
      timeout_hint: "Bir bağlantının her geri yükleme sonrası komutunun en uzun çalışma süresi (1 sn ile 24 sa arası); daha uzun süren bir komut geri yüklemeyi başarısız kılar.",
      kept_title: "Geri yüklenen kopyalar hâlâ silinmiş veri içeriyor",
      kept_desc: "Bir geri yükleme sonrası komut başarısız oldu; bu kopyalar her komut (örneğin silmeler) uygulanmadan incelemek için korundu. Kullanmayın; işiniz bitince silin.",
      kept_item: "Geri yükleme {id}: {databases}",
      drop: "Kopyaları sil",
      drop_confirm: "{databases} silinsin mi? İçlerindeki veriler kalıcı olarak silinir.",
      dropped: "Kopyalar silindi.",
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
      timeout: "Zeitlimit für Befehle nach der Wiederherstellung",
      timeout_hint: "Maximale Laufzeit jedes Befehls nach der Wiederherstellung einer Verbindung (1 s bis 24 h); ein länger laufender Befehl lässt die Wiederherstellung fehlschlagen.",
      kept_title: "Wiederhergestellte Kopien enthalten noch gelöschte Daten",
      kept_desc: "Ein Befehl nach der Wiederherstellung ist fehlgeschlagen; diese Kopien wurden zur Prüfung behalten, ohne dass alle Befehle (etwa Löschungen) angewendet wurden. Verwenden Sie sie nicht und löschen Sie sie danach.",
      kept_item: "Wiederherstellung {id}: {databases}",
      drop: "Kopien löschen",
      drop_confirm: "{databases} löschen? Die Daten darin werden endgültig gelöscht.",
      dropped: "Kopien gelöscht.",
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
      timeout: "Tiempo límite de los comandos posteriores a la restauración",
      timeout_hint: "Duración máxima de cada comando posterior a la restauración de una conexión (de 1 s a 24 h); un comando que tarda más hace fallar la restauración.",
      kept_title: "Las copias restauradas aún contienen datos suprimidos",
      kept_desc: "Un comando posterior a la restauración falló, así que estas copias se conservaron para inspección sin aplicar todos los comandos (como las supresiones). No las use; elimínelas al terminar.",
      kept_item: "Restauración {id}: {databases}",
      drop: "Eliminar copias",
      drop_confirm: "¿Eliminar {databases}? Sus datos se borran definitivamente.",
      dropped: "Copias eliminadas.",
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
      timeout: "Délai des commandes après restauration",
      timeout_hint: "Durée maximale de chaque commande après restauration d'une connexion (de 1 s à 24 h) ; une commande plus longue fait échouer la restauration.",
      kept_title: "Des copies restaurées contiennent encore des données effacées",
      kept_desc: "Une commande après restauration a échoué : ces copies ont été conservées pour inspection sans que toutes les commandes (comme les effacements) soient appliquées. Ne les utilisez pas ; supprimez-les ensuite.",
      kept_item: "Restauration {id} : {databases}",
      drop: "Supprimer les copies",
      drop_confirm: "Supprimer {databases} ? Leurs données sont effacées définitivement.",
      dropped: "Copies supprimées.",
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
      timeout: "恢复后命令超时",
      timeout_hint: "连接的每条恢复后命令的最长运行时间（1 秒到 24 小时）；超时的命令会使恢复失败。",
      kept_title: "已恢复的副本仍包含已删除的数据",
      kept_desc: "一条恢复后命令失败，因此这些副本被保留以供检查，但并未执行全部命令（例如数据删除）。请勿使用，检查完毕后删除。",
      kept_item: "恢复 {id}：{databases}",
      drop: "删除副本",
      drop_confirm: "删除 {databases}？其中的数据将被永久删除。",
      dropped: "副本已删除。",
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
      timeout: "リストア後コマンドのタイムアウト",
      timeout_hint: "接続のリストア後コマンド 1 件あたりの最大実行時間（1 秒〜24 時間）。これを超えるとリストアは失敗します。",
      kept_title: "リストアしたクローンに削除済みのデータが残っています",
      kept_desc: "リストア後のコマンドが失敗したため、これらのクローンはすべてのコマンド（削除の再適用など）が適用されないまま調査用に保持されています。使用せず、確認後に削除してください。",
      kept_item: "リストア {id}：{databases}",
      drop: "クローンを削除",
      drop_confirm: "{databases} を削除しますか？ データは完全に削除されます。",
      dropped: "クローンを削除しました。",
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
      timeout: "Тайм-аут команд после восстановления",
      timeout_hint: "Максимальное время выполнения каждой команды после восстановления подключения (от 1 с до 24 ч); более долгая команда приводит к ошибке восстановления.",
      kept_title: "Восстановленные копии всё ещё содержат удалённые данные",
      kept_desc: "Команда после восстановления завершилась ошибкой, поэтому эти копии сохранены для проверки без применения всех команд (например, удалений). Не используйте их и удалите после проверки.",
      kept_item: "Восстановление {id}: {databases}",
      drop: "Удалить копии",
      drop_confirm: "Удалить {databases}? Данные в них будут удалены безвозвратно.",
      dropped: "Копии удалены.",
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

// ---------------------------------------------------------------------------
// Clones kept after a failed post-restore command
// ---------------------------------------------------------------------------

// WARNING_POSTRESTORE_CLONES_KEPT is settings.WarningPostRestoreClonesKept.
const WARNING_POSTRESTORE_CLONES_KEPT = "post_restore_clones_kept";

// postRestoreRenderWarnings shows the restores whose clones were kept after a
// failed post-restore command, each with a button that drops them (admin).
function postRestoreRenderWarnings(list) {
  const banner = document.getElementById("postrestore-clones-warning");
  const ul = document.getElementById("postrestore-clones-list");
  if (!banner || !ul) return;
  const kept = (Array.isArray(list) ? list : []).filter(w => w && w.id === WARNING_POSTRESTORE_CLONES_KEPT && w.restore_id);
  banner.hidden = kept.length === 0;
  ul.replaceChildren();
  kept.forEach(w => {
    const databases = Array.isArray(w.databases) ? w.databases.join(", ") : "";
    const li = document.createElement("li");
    const text = document.createElement("span");
    text.textContent = tf("postrestore.kept_item", { id: w.restore_id, databases });
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn btn-danger btn-sm";
    btn.dataset.requires = "admin";
    btn.textContent = t("postrestore.drop");
    btn.addEventListener("click", () => postRestoreDropClones(w.restore_id, databases, btn));
    li.append(text, " ", btn);
    ul.append(li);
  });
  if (typeof gateAll === "function") gateAll();
}

async function postRestoreDropClones(restoreID, databases, btn) {
  const ok = typeof confirmDialog === "function"
    ? await confirmDialog({ body: tf("postrestore.drop_confirm", { databases }), confirmLabel: t("postrestore.drop") })
    : true;
  if (!ok) return;
  btn.disabled = true;
  try {
    const json = await apiJSON(`/api/v1/restores/${encodeURIComponent(restoreID)}/drop-clones`, { method: "POST" });
    if (!json.success) {
      showToast(json.error || t("toasts.request_failed"), "error");
      return;
    }
    showToast(t("postrestore.dropped"), "success");
    const settings = await apiJSON("/api/v1/settings");
    if (settings.success) {
      state.settings = { ...(state.settings || {}), warnings: (settings.data && settings.data.warnings) || [] };
      renderWarnings();
    }
  } catch (err) {
    showToast(err.message, "error");
  } finally {
    btn.disabled = false;
  }
}

function setupPostRestore() {
  const field = postRestoreField();
  if (field) field.addEventListener("input", postRestoreValidate);
  if (typeof onLanguageChange === "function") onLanguageChange(() => { if (postRestoreField()) postRestoreValidate(); });
}

document.addEventListener("DOMContentLoaded", setupPostRestore);
