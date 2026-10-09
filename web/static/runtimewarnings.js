/**
 * MongoRescue dashboard: the generic warnings banner.
 *
 * Shows the persistent warnings of GET /api/v1/settings (settings.Warning) that
 * no feature module renders itself: a full data directory (data_dir_full),
 * notification channels that cannot be loaded (notification_channel_unreadable),
 * and any warning a newer server reports, with the server's English text when no
 * translation exists.
 *
 * Loaded after app.js and trust.js (mergeTranslations); app.js renderWarnings()
 * calls runtimeRenderWarnings(list). Server values reach the DOM only through
 * textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const RUNTIME_WARNING_TRANSLATIONS = {
  en: {
    warnings: {
      data_dir_full_title: "The data directory is full",
      data_dir_full_desc: "The metadata database cannot be written, so new backups and restores are refused. Free space on the disk of the data directory; MongoRescue resumes on its own once there is enough.",
      notification_channel_unreadable_title: "Notification channels cannot be loaded",
      notification_channel_unreadable_desc: "The notifications of these channels fail and are given up. Edit and save each channel (re-entering its secrets) or delete it.",
    },
  },
  tr: {
    warnings: {
      data_dir_full_title: "Veri dizini dolu",
      data_dir_full_desc: "Meta veri veritabanına yazılamıyor, bu yüzden yeni yedeklemeler ve geri yüklemeler reddediliyor. Veri dizininin diskinde yer açın; yeterli alan olduğunda MongoRescue kendiliğinden devam eder.",
      notification_channel_unreadable_title: "Bildirim kanalları yüklenemiyor",
      notification_channel_unreadable_desc: "Bu kanalların bildirimleri başarısız oluyor ve bırakılıyor. Her kanalı düzenleyip kaydedin (gizli bilgilerini yeniden girerek) ya da silin.",
    },
  },
  de: {
    warnings: {
      data_dir_full_title: "Das Datenverzeichnis ist voll",
      data_dir_full_desc: "Die Metadatenbank kann nicht beschrieben werden, daher werden neue Backups und Wiederherstellungen abgelehnt. Schaffen Sie Platz auf dem Datenträger des Datenverzeichnisses; MongoRescue macht von selbst weiter, sobald genug frei ist.",
      notification_channel_unreadable_title: "Benachrichtigungskanäle können nicht geladen werden",
      notification_channel_unreadable_desc: "Die Benachrichtigungen dieser Kanäle schlagen fehl und werden aufgegeben. Bearbeiten und speichern Sie jeden Kanal (mit erneut eingegebenen Geheimnissen) oder löschen Sie ihn.",
    },
  },
  es: {
    warnings: {
      data_dir_full_title: "El directorio de datos está lleno",
      data_dir_full_desc: "No se puede escribir en la base de datos de metadatos, así que se rechazan nuevas copias de seguridad y restauraciones. Libere espacio en el disco del directorio de datos; MongoRescue continúa por sí solo cuando haya suficiente.",
      notification_channel_unreadable_title: "No se pueden cargar canales de notificación",
      notification_channel_unreadable_desc: "Las notificaciones de estos canales fallan y se abandonan. Edite y guarde cada canal (volviendo a introducir sus secretos) o elimínelo.",
    },
  },
  fr: {
    warnings: {
      data_dir_full_title: "Le répertoire de données est plein",
      data_dir_full_desc: "La base de métadonnées ne peut pas être écrite, les nouvelles sauvegardes et restaurations sont donc refusées. Libérez de l'espace sur le disque du répertoire de données ; MongoRescue reprend de lui-même dès qu'il y en a assez.",
      notification_channel_unreadable_title: "Des canaux de notification ne peuvent pas être chargés",
      notification_channel_unreadable_desc: "Les notifications de ces canaux échouent et sont abandonnées. Modifiez et enregistrez chaque canal (en ressaisissant ses secrets) ou supprimez-le.",
    },
  },
  zh: {
    warnings: {
      data_dir_full_title: "数据目录已满",
      data_dir_full_desc: "无法写入元数据数据库，因此新的备份和恢复会被拒绝。请释放数据目录所在磁盘的空间；空间足够后 MongoRescue 会自动恢复。",
      notification_channel_unreadable_title: "无法加载通知渠道",
      notification_channel_unreadable_desc: "这些渠道的通知会失败并被放弃。请编辑并保存每个渠道（重新输入其密钥），或将其删除。",
    },
  },
  ja: {
    warnings: {
      data_dir_full_title: "データディレクトリがいっぱいです",
      data_dir_full_desc: "メタデータベースに書き込めないため、新しいバックアップとリストアは拒否されます。データディレクトリのディスクの空き容量を増やしてください。十分な空きができると MongoRescue は自動的に再開します。",
      notification_channel_unreadable_title: "通知チャネルを読み込めません",
      notification_channel_unreadable_desc: "これらのチャネルの通知は失敗し、破棄されます。各チャネルを編集して保存する（シークレットを再入力する）か、削除してください。",
    },
  },
  ru: {
    warnings: {
      data_dir_full_title: "Каталог данных заполнен",
      data_dir_full_desc: "Запись в базу метаданных невозможна, поэтому новые резервные копии и восстановления отклоняются. Освободите место на диске каталога данных; MongoRescue продолжит работу сам, когда места станет достаточно.",
      notification_channel_unreadable_title: "Не удаётся загрузить каналы уведомлений",
      notification_channel_unreadable_desc: "Уведомления этих каналов не доставляются и отбрасываются. Отредактируйте и сохраните каждый канал (заново введя его секреты) или удалите его.",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(RUNTIME_WARNING_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], RUNTIME_WARNING_TRANSLATIONS[lang]);
  });
}

// The warnings that feature modules render in banners of their own.
const RUNTIME_WARNINGS_RENDERED_ELSEWHERE = new Set([
  "encryption_off_after_upgrade",
  "metadata_backup_unencrypted",
  "recovery_kit_missing",
  "oidc_role_kept",
  "previous_secret_key",
  "post_restore_clones_kept",
  "connection_tls_loosened",
]);

// Warnings shown as errors (the rest as warnings).
const RUNTIME_WARNINGS_DANGER = new Set(["data_dir_full"]);

// runtimeWarningText returns the translation of warnings.<id>_<part>, or
// fallback when there is none.
function runtimeWarningText(id, part, fallback) {
  const key = `warnings.${id}_${part}`;
  const text = typeof t === "function" ? t(key) : key;
  return text && text !== key ? text : fallback;
}

// runtimeRenderWarnings renders the warnings of list that no other module shows.
function runtimeRenderWarnings(list) {
  const box = document.getElementById("runtime-warnings");
  if (!box) return;
  const shown = (Array.isArray(list) ? list : []).filter(w => w && typeof w.id === "string" && !RUNTIME_WARNINGS_RENDERED_ELSEWHERE.has(w.id));
  box.replaceChildren();
  box.hidden = shown.length === 0;
  shown.forEach(w => {
    const section = document.createElement("section");
    section.className = `callout ${RUNTIME_WARNINGS_DANGER.has(w.id) ? "callout-danger" : "callout-warning"}`;
    section.setAttribute("role", "alert");
    section.dataset.warning = w.id;
    const body = document.createElement("div");
    const title = document.createElement("h2");
    title.className = "callout-title";
    title.textContent = runtimeWarningText(w.id, "title", w.id);
    const desc = document.createElement("p");
    desc.className = "section-desc";
    desc.textContent = runtimeWarningText(w.id, "desc", w.message || "");
    body.append(title, desc);
    const channels = Array.isArray(w.channels) ? w.channels : [];
    if (channels.length > 0) {
      const ul = document.createElement("ul");
      ul.className = "postrestore-kept-list";
      channels.forEach(c => {
        const li = document.createElement("li");
        li.textContent = String(c);
        ul.append(li);
      });
      body.append(ul);
    }
    section.append(body);
    box.append(section);
  });
}
