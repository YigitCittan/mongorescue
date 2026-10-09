/**
 * MongoRescue dashboard: the upload stall timeout (general.storage_stall_timeout).
 *
 * The field lives in the General settings form (index.html) and is filled and
 * collected by app.js; this file only adds its strings for the languages that
 * i18n.js does not carry them for.
 *
 * Loaded after app.js and trust.js (mergeTranslations).
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js; other languages fall back to English)
// ---------------------------------------------------------------------------

const STALL_TRANSLATIONS = {
  zh: {
    settings: {
      storage_stall_timeout: "上传停滞超时",
      storage_stall_timeout_hint: "存储目标在这段时间内未接收任何数据时，上传失败（1m 至 1h）。",
    },
  },
  ja: {
    settings: {
      storage_stall_timeout: "アップロード停滞タイムアウト",
      storage_stall_timeout_hint: "ストレージターゲットがこの時間データを受け付けない場合、アップロードを失敗にします（1m〜1h）。",
    },
  },
  ru: {
    settings: {
      storage_stall_timeout: "Тайм-аут простоя загрузки",
      storage_stall_timeout_hint: "Прервать загрузку с ошибкой, если хранилище так долго не принимает данные (от 1m до 1h).",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(STALL_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], STALL_TRANSLATIONS[lang]);
  });
}
