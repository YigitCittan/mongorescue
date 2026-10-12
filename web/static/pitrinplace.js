/**
 * MongoRescue dashboard: the in-place option of the point-in-time restore wizard
 * (pitr.js), issue #142.
 *
 * Administrators may tick "Replace the original databases": the restore still
 * goes into clones first and is checked, then a safety backup of each original is
 * taken, the PITR stream is paused, the clones are copied over the originals and
 * the stream starts a new chain. The request carries "pitr": {"in_place": true,
 * "confirm_in_place": "<databases>"} (and "keep_clone"), only for a list of
 * databases, and starts after a danger confirmation in which the administrator
 * types the database names. Everyone else, and the default, restores into new
 * clones as before.
 *
 * Loaded after app.js, trust.js (mergeTranslations), bulk.js (confirmDialog),
 * pitr.js and protection.js; pitr.js calls the pitrInPlace* hooks below when they
 * exist. Server values reach the DOM only through textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const PITR_INPLACE_TRANSLATIONS = {
  en: {
    pitr: {
      restore_hint: "Every database, or the ones you list, is restored into a new database named <db>_rescue_<timestamp>_<id>; existing data is untouched unless an administrator chooses to replace the original databases. Times are UTC on the primary's clock."
    },
    pitr_inplace: {
      option: "Replace the original databases",
      downtime: "Downtime: applications must stop writing to these databases until the restore has completed; anything written meanwhile is overwritten.",
      safety: "The databases are restored into clones and checked first. Then a safety backup of each original is taken (labelled pre-pitr-in-place), the PITR stream is paused, the clones are copied over the originals and dropped, and the stream starts a new chain with a new base backup. Plan for about three times the data: the originals, the clones and the safety backups.",
      approval: "With the two-person rule on, a second administrator must approve the restore before it starts.",
      keep_clone: "Keep the restored clones afterwards",
      need_databases: "List the databases to replace: an in-place restore never replaces the whole instance.",
      confirm_title: "Replace the original databases?",
      confirm_body: "Replaces {databases} with their state at {at}; their current data is overwritten. Estimated duration of the restore: {duration}, plus the safety backup and the swap.",
      undo_note: "The safety backup taken first keeps the current data: restore it to go back.",
      confirm_label: "Replace",
      started: "In-place point-in-time restore {id} started."
    },
    run: {
      phase_safety_backup: "Safety backup",
      phase_swapping: "Replacing databases"
    },
    notify: {
      events: {
        restore_in_place_pitr: "In-place point-in-time restore"
      }
    }
  },
  tr: {
    pitr: {
      restore_hint: "Tüm veritabanları ya da listelediğiniz veritabanları <db>_rescue_<timestamp>_<id> adlı yeni bir veritabanına geri yüklenir; bir yönetici özgün veritabanlarını değiştirmeyi seçmedikçe mevcut verilere dokunulmaz. Saatler UTC'dir ve birincil sunucunun saatine göredir."
    },
    pitr_inplace: {
      option: "Özgün veritabanlarını değiştir",
      downtime: "Kesinti: geri yükleme tamamlanana kadar uygulamalar bu veritabanlarına yazmayı durdurmalıdır; bu sırada yazılan her şeyin üzerine yazılır.",
      safety: "Veritabanları önce klonlara geri yüklenir ve denetlenir. Ardından her özgün veritabanının bir güvenlik yedeği alınır (pre-pitr-in-place etiketiyle), PITR akışı duraklatılır, klonlar özgün veritabanlarının üzerine kopyalanıp silinir ve akış yeni bir temel yedekle yeni bir zincir başlatır. Verinin yaklaşık üç katı kadar yer ayırın: özgün veritabanları, klonlar ve güvenlik yedekleri.",
      approval: "İki kişi kuralı açıkken geri yükleme, başlamadan önce ikinci bir yöneticinin onayını bekler.",
      keep_clone: "Geri yüklenen klonları sonra da sakla",
      need_databases: "Değiştirilecek veritabanlarını listeleyin: yerinde geri yükleme hiçbir zaman tüm sunucuyu değiştirmez.",
      confirm_title: "Özgün veritabanları değiştirilsin mi?",
      confirm_body: "{databases} veritabanlarını {at} anındaki hâlleriyle değiştirir; mevcut verilerinin üzerine yazılır. Geri yüklemenin tahmini süresi: {duration}, buna güvenlik yedeği ve değiştirme süresi eklenir.",
      undo_note: "Önce alınan güvenlik yedeği mevcut verileri saklar: geri dönmek için onu geri yükleyin.",
      confirm_label: "Değiştir",
      started: "Yerinde zamana noktasal geri yükleme {id} başladı."
    },
    run: {
      phase_safety_backup: "Güvenlik yedeği",
      phase_swapping: "Veritabanları değiştiriliyor"
    },
    notify: {
      events: {
        restore_in_place_pitr: "Yerinde zamana noktasal geri yükleme"
      }
    }
  },
  de: {
    pitr: {
      restore_hint: "Jede Datenbank, oder die angegebenen, wird in eine neue Datenbank namens <db>_rescue_<timestamp>_<id> wiederhergestellt; vorhandene Daten bleiben unberührt, es sei denn, ein Administrator lässt die ursprünglichen Datenbanken ersetzen. Zeiten sind UTC nach der Uhr des Primary."
    },
    pitr_inplace: {
      option: "Ursprüngliche Datenbanken ersetzen",
      downtime: "Ausfallzeit: Anwendungen dürfen bis zum Abschluss der Wiederherstellung nicht in diese Datenbanken schreiben; was in der Zwischenzeit geschrieben wird, wird überschrieben.",
      safety: "Die Datenbanken werden zuerst in Klone wiederhergestellt und geprüft. Dann wird von jeder ursprünglichen Datenbank ein Sicherheits-Backup erstellt (Label pre-pitr-in-place), der PITR-Stream pausiert, die Klone werden über die Originale kopiert und gelöscht, und der Stream beginnt eine neue Kette mit einem neuen Basis-Backup. Planen Sie etwa die dreifache Datenmenge ein: Originale, Klone und Sicherheits-Backups.",
      approval: "Bei aktiver Zwei-Personen-Regel muss ein zweiter Administrator die Wiederherstellung genehmigen, bevor sie beginnt.",
      keep_clone: "Wiederhergestellte Klone danach behalten",
      need_databases: "Geben Sie die zu ersetzenden Datenbanken an: Eine In-Place-Wiederherstellung ersetzt nie die ganze Instanz.",
      confirm_title: "Ursprüngliche Datenbanken ersetzen?",
      confirm_body: "Ersetzt {databases} durch ihren Stand von {at}; ihre aktuellen Daten werden überschrieben. Geschätzte Dauer der Wiederherstellung: {duration}, zuzüglich Sicherheits-Backup und Austausch.",
      undo_note: "Das zuerst erstellte Sicherheits-Backup enthält die aktuellen Daten: Stellen Sie es wieder her, um zurückzukehren.",
      confirm_label: "Ersetzen",
      started: "In-Place-Point-in-Time-Wiederherstellung {id} gestartet."
    },
    run: {
      phase_safety_backup: "Sicherheits-Backup",
      phase_swapping: "Datenbanken werden ersetzt"
    },
    notify: {
      events: {
        restore_in_place_pitr: "In-Place-Point-in-Time-Wiederherstellung"
      }
    }
  },
  es: {
    pitr: {
      restore_hint: "Cada base de datos, o las que indiques, se restaura en una base de datos nueva llamada <db>_rescue_<timestamp>_<id>; los datos existentes no se tocan salvo que un administrador elija reemplazar las bases de datos originales. Las horas son UTC según el reloj del primario."
    },
    pitr_inplace: {
      option: "Reemplazar las bases de datos originales",
      downtime: "Tiempo de inactividad: las aplicaciones deben dejar de escribir en estas bases de datos hasta que termine la restauración; lo que se escriba mientras tanto se sobrescribe.",
      safety: "Primero las bases de datos se restauran en clones y se comprueban. Después se toma una copia de seguridad de cada original (etiquetada pre-pitr-in-place), se pausa el flujo PITR, los clones se copian sobre los originales y se eliminan, y el flujo empieza una cadena nueva con una copia base nueva. Prevé unas tres veces el tamaño de los datos: los originales, los clones y las copias de seguridad.",
      approval: "Con la regla de dos personas activada, un segundo administrador debe aprobar la restauración antes de que empiece.",
      keep_clone: "Conservar después los clones restaurados",
      need_databases: "Indica las bases de datos que se reemplazan: una restauración in situ nunca reemplaza la instancia completa.",
      confirm_title: "¿Reemplazar las bases de datos originales?",
      confirm_body: "Reemplaza {databases} por su estado en {at}; sus datos actuales se sobrescriben. Duración estimada de la restauración: {duration}, más la copia de seguridad y el reemplazo.",
      undo_note: "La copia de seguridad tomada antes conserva los datos actuales: restáurala para volver atrás.",
      confirm_label: "Reemplazar",
      started: "Restauración in situ a un punto en el tiempo {id} iniciada."
    },
    run: {
      phase_safety_backup: "Copia de seguridad previa",
      phase_swapping: "Reemplazando bases de datos"
    },
    notify: {
      events: {
        restore_in_place_pitr: "Restauración in situ a un punto en el tiempo"
      }
    }
  },
  fr: {
    pitr: {
      restore_hint: "Chaque base, ou celles que vous indiquez, est restaurée dans une nouvelle base nommée <db>_rescue_<timestamp>_<id> ; les données existantes ne sont pas touchées, sauf si un administrateur choisit de remplacer les bases d'origine. Les heures sont en UTC selon l'horloge du primaire."
    },
    pitr_inplace: {
      option: "Remplacer les bases d'origine",
      downtime: "Interruption : les applications doivent cesser d'écrire dans ces bases jusqu'à la fin de la restauration ; tout ce qui est écrit entre-temps est écrasé.",
      safety: "Les bases sont d'abord restaurées dans des clones et vérifiées. Ensuite, une sauvegarde de sécurité de chaque base d'origine est faite (libellée pre-pitr-in-place), le flux PITR est mis en pause, les clones sont copiés sur les bases d'origine puis supprimés, et le flux démarre une nouvelle chaîne avec une nouvelle sauvegarde de base. Prévoyez environ trois fois le volume des données : les bases d'origine, les clones et les sauvegardes de sécurité.",
      approval: "Avec la règle des deux personnes, un second administrateur doit approuver la restauration avant qu'elle ne démarre.",
      keep_clone: "Conserver ensuite les clones restaurés",
      need_databases: "Indiquez les bases à remplacer : une restauration sur place ne remplace jamais toute l'instance.",
      confirm_title: "Remplacer les bases d'origine ?",
      confirm_body: "Remplace {databases} par leur état à {at} ; leurs données actuelles sont écrasées. Durée estimée de la restauration : {duration}, plus la sauvegarde de sécurité et le remplacement.",
      undo_note: "La sauvegarde de sécurité faite d'abord conserve les données actuelles : restaurez-la pour revenir en arrière.",
      confirm_label: "Remplacer",
      started: "Restauration sur place à un instant donné {id} lancée."
    },
    run: {
      phase_safety_backup: "Sauvegarde de sécurité",
      phase_swapping: "Remplacement des bases"
    },
    notify: {
      events: {
        restore_in_place_pitr: "Restauration sur place à un instant donné"
      }
    }
  },
  zh: {
    pitr: {
      restore_hint: "所有数据库（或您列出的数据库）都会恢复到名为 <db>_rescue_<timestamp>_<id> 的新数据库中；除非管理员选择替换原始数据库，否则现有数据不会被改动。时间为 UTC，以主节点时钟为准。"
    },
    pitr_inplace: {
      option: "替换原始数据库",
      downtime: "停机：在恢复完成之前，应用程序必须停止写入这些数据库；期间写入的内容都会被覆盖。",
      safety: "数据库会先恢复到克隆中并进行检查。随后为每个原始数据库创建安全备份（标签为 pre-pitr-in-place），暂停 PITR 流，将克隆复制覆盖原始数据库并删除克隆，然后该流以新的基础备份开始新的链。请预留约三倍于数据量的空间：原始数据库、克隆和安全备份。",
      approval: "启用双人规则时，恢复开始前必须由另一位管理员批准。",
      keep_clone: "之后保留已恢复的克隆",
      need_databases: "请列出要替换的数据库：原地恢复从不替换整个实例。",
      confirm_title: "替换原始数据库？",
      confirm_body: "将 {databases} 替换为其在 {at} 的状态；当前数据会被覆盖。恢复预计时长：{duration}，另加安全备份和替换的时间。",
      undo_note: "先创建的安全备份保存了当前数据：如需回退，请恢复它。",
      confirm_label: "替换",
      started: "原地时间点恢复 {id} 已开始。"
    },
    run: {
      phase_safety_backup: "安全备份",
      phase_swapping: "正在替换数据库"
    },
    notify: {
      events: {
        restore_in_place_pitr: "原地时间点恢复"
      }
    }
  },
  ja: {
    pitr: {
      restore_hint: "すべてのデータベース（または指定したもの）が <db>_rescue_<timestamp>_<id> という新しいデータベースに復元されます。管理者が元のデータベースの置き換えを選ばない限り、既存のデータには触れません。時刻はプライマリの時計による UTC です。"
    },
    pitr_inplace: {
      option: "元のデータベースを置き換える",
      downtime: "ダウンタイム: 復元が完了するまで、アプリケーションはこれらのデータベースへの書き込みを止める必要があります。その間に書き込まれた内容は上書きされます。",
      safety: "まずデータベースをクローンに復元して検証します。その後、元の各データベースの安全バックアップ（ラベル pre-pitr-in-place）を取得し、PITR ストリームを一時停止して、クローンを元のデータベースに上書きコピーしてから削除し、ストリームは新しいベースバックアップで新しいチェーンを開始します。データ量の約 3 倍の容量（元のデータベース、クローン、安全バックアップ）を見込んでください。",
      approval: "2 人承認ルールが有効な場合、復元の開始前に別の管理者の承認が必要です。",
      keep_clone: "復元したクローンを後も残す",
      need_databases: "置き換えるデータベースを指定してください。インプレース復元がインスタンス全体を置き換えることはありません。",
      confirm_title: "元のデータベースを置き換えますか？",
      confirm_body: "{databases} を {at} 時点の状態に置き換えます。現在のデータは上書きされます。復元の推定所要時間: {duration}（安全バックアップと置き換えの時間は別）。",
      undo_note: "最初に取得する安全バックアップに現在のデータが残ります。元に戻すにはそれを復元してください。",
      confirm_label: "置き換え",
      started: "インプレースのポイントインタイム復元 {id} を開始しました。"
    },
    run: {
      phase_safety_backup: "安全バックアップ",
      phase_swapping: "データベースを置き換え中"
    },
    notify: {
      events: {
        restore_in_place_pitr: "インプレースのポイントインタイム復元"
      }
    }
  },
  ru: {
    pitr: {
      restore_hint: "Каждая база данных (или указанные вами) восстанавливается в новую базу с именем <db>_rescue_<timestamp>_<id>; существующие данные не затрагиваются, если только администратор не решит заменить исходные базы. Время указано в UTC по часам первичного узла."
    },
    pitr_inplace: {
      option: "Заменить исходные базы данных",
      downtime: "Простой: приложения должны прекратить запись в эти базы до завершения восстановления; всё, что будет записано за это время, перезаписывается.",
      safety: "Сначала базы восстанавливаются в клоны и проверяются. Затем для каждой исходной базы создаётся страховочная резервная копия (с меткой pre-pitr-in-place), поток PITR приостанавливается, клоны копируются поверх исходных баз и удаляются, а поток начинает новую цепочку с новой базовой копией. Заложите примерно трёхкратный объём данных: исходные базы, клоны и страховочные копии.",
      approval: "При включённом правиле двух лиц восстановление начнётся только после одобрения вторым администратором.",
      keep_clone: "Сохранить восстановленные клоны после завершения",
      need_databases: "Укажите заменяемые базы данных: восстановление на месте никогда не заменяет весь экземпляр.",
      confirm_title: "Заменить исходные базы данных?",
      confirm_body: "Заменяет {databases} их состоянием на {at}; текущие данные перезаписываются. Ожидаемая длительность восстановления: {duration}, плюс страховочная копия и замена.",
      undo_note: "Страховочная копия, созданная в начале, сохраняет текущие данные: восстановите её, чтобы вернуться назад.",
      confirm_label: "Заменить",
      started: "Восстановление на момент времени на месте {id} запущено."
    },
    run: {
      phase_safety_backup: "Страховочная копия",
      phase_swapping: "Замена баз данных"
    },
    notify: {
      events: {
        restore_in_place_pitr: "Восстановление на момент времени на месте"
      }
    }
  }
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(PITR_INPLACE_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], PITR_INPLACE_TRANSLATIONS[lang]);
  });
}

// ---------------------------------------------------------------------------
// Hooks of the wizard (pitr.js)
// ---------------------------------------------------------------------------

// pitrInPlaceChecked reports whether the wizard replaces the original databases.
function pitrInPlaceChecked() {
  const group = document.getElementById("pitr-inplace-group");
  const box = document.getElementById("pitr-restore-inplace");
  return !!group && !group.hidden && !!box && box.checked && !box.disabled;
}

// pitrInPlaceToggle shows the notes of the in-place option while it is ticked.
function pitrInPlaceToggle() {
  const notes = document.getElementById("pitr-inplace-notes");
  if (notes) notes.hidden = !pitrInPlaceChecked();
}

// pitrInPlaceFill resets the option when the wizard opens: unticked, and shown to
// administrators only (the default stays a restore into new clones).
function pitrInPlaceFill() {
  const group = document.getElementById("pitr-inplace-group");
  const box = document.getElementById("pitr-restore-inplace");
  const keep = document.getElementById("pitr-restore-keep-clone");
  if (!group || !box) return;
  group.hidden = !(typeof can === "function" && can("admin"));
  box.checked = false;
  if (keep) keep.checked = false;
  pitrInPlaceToggle();
}

// pitrInPlacePayload returns the in-place fields of the "pitr" object of a
// request for databases dbs ({} for a restore into clones), or null, with the
// error shown, when no database is listed.
function pitrInPlacePayload(dbs) {
  if (!pitrInPlaceChecked()) return {};
  if (!dbs.length) {
    showFormError("pitr-restore-error", t("pitr_inplace.need_databases"));
    return null;
  }
  const out = { in_place: true, confirm_in_place: dbs.join(",") };
  const keep = document.getElementById("pitr-restore-keep-clone");
  if (keep && keep.checked) out.keep_clone = true;
  return out;
}

// pitrInPlaceConfirm asks for the danger confirmation of an in-place restore:
// the administrator types the database names.
function pitrInPlaceConfirm(body, plan) {
  const names = String(body.pitr.confirm_in_place || "");
  if (typeof confirmDialog !== "function") return Promise.resolve(false);
  return confirmDialog({
    title: t("pitr_inplace.confirm_title"),
    body: [
      tf("pitr_inplace.confirm_body", { databases: names.split(",").join(", "), at: body.pitr.at, duration: formatDuration((plan && plan.estimated_seconds) || 0) }),
      t("pitr_inplace.downtime"),
      t("pitr_inplace.safety"),
      t("pitr_inplace.approval")
    ],
    danger: true,
    undoNote: t("pitr_inplace.undo_note"),
    requireText: names,
    confirmLabel: t("pitr_inplace.confirm_label")
  });
}

function pitrInPlaceSetup() {
  const box = document.getElementById("pitr-restore-inplace");
  const keep = document.getElementById("pitr-restore-keep-clone");
  // A changed option needs a new check: the preflight of an in-place restore
  // also checks the databases it replaces.
  const changed = () => {
    pitrInPlaceToggle();
    if (typeof pitrRestoreReset === "function") pitrRestoreReset();
  };
  if (box) box.addEventListener("change", changed);
  if (keep) keep.addEventListener("change", changed);
}

document.addEventListener("DOMContentLoaded", pitrInPlaceSetup);
