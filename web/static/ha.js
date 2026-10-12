/**
 * MongoRescue dashboard: the standby banner of active-passive high availability.
 *
 * GET /api/v1/health reports "role" ("active" or "standby") and an "ha" object
 * (cluster, epoch, holder) when the server runs in active-passive HA mode. On a
 * standby the banner says that the instance is read-only and names the active
 * instance; the API refuses changes there with 503.
 *
 * Loaded after app.js and trust.js (mergeTranslations); app.js checkHealth()
 * calls haRenderRole(data). Server values reach the DOM only through textContent.
 */

// ---------------------------------------------------------------------------
// Translations (merged into i18n.js)
// ---------------------------------------------------------------------------

const HA_TRANSLATIONS = {
  en: {
    ha: {
      standby_title: "Standby instance: read-only",
      standby_desc: "This instance is the standby of an active-passive HA cluster. It runs no backups and refuses changes; make changes on the active instance. It takes over by itself when the active instance stops.",
      cluster: "Cluster",
      active_instance: "Active instance",
      unknown: "unknown",
    },
  },
  tr: {
    ha: {
      standby_title: "Yedek örnek: salt okunur",
      standby_desc: "Bu örnek, aktif-pasif bir HA kümesinin yedek örneğidir. Yedekleme çalıştırmaz ve değişiklikleri reddeder; değişiklikleri aktif örnekte yapın. Aktif örnek durduğunda görevi kendiliğinden devralır.",
      cluster: "Küme",
      active_instance: "Aktif örnek",
      unknown: "bilinmiyor",
    },
  },
  de: {
    ha: {
      standby_title: "Standby-Instanz: nur lesend",
      standby_desc: "Diese Instanz ist der Standby eines Active-Passive-HA-Clusters. Sie führt keine Backups aus und lehnt Änderungen ab; nehmen Sie Änderungen auf der aktiven Instanz vor. Sie übernimmt von selbst, wenn die aktive Instanz ausfällt.",
      cluster: "Cluster",
      active_instance: "Aktive Instanz",
      unknown: "unbekannt",
    },
  },
  es: {
    ha: {
      standby_title: "Instancia en espera: solo lectura",
      standby_desc: "Esta instancia es la de reserva de un clúster HA activo-pasivo. No ejecuta copias de seguridad y rechaza los cambios; hágalos en la instancia activa. Toma el relevo por sí sola cuando la instancia activa se detiene.",
      cluster: "Clúster",
      active_instance: "Instancia activa",
      unknown: "desconocida",
    },
  },
  fr: {
    ha: {
      standby_title: "Instance de secours : lecture seule",
      standby_desc: "Cette instance est l'instance de secours d'un cluster HA actif-passif. Elle n'exécute aucune sauvegarde et refuse les modifications ; faites-les sur l'instance active. Elle prend le relais d'elle-même quand l'instance active s'arrête.",
      cluster: "Cluster",
      active_instance: "Instance active",
      unknown: "inconnue",
    },
  },
  zh: {
    ha: {
      standby_title: "备用实例：只读",
      standby_desc: "此实例是主备（active-passive）HA 集群中的备用实例。它不运行备份并拒绝更改；请在活动实例上进行更改。活动实例停止时，它会自动接管。",
      cluster: "集群",
      active_instance: "活动实例",
      unknown: "未知",
    },
  },
  ja: {
    ha: {
      standby_title: "スタンバイインスタンス: 読み取り専用",
      standby_desc: "このインスタンスはアクティブ/パッシブ HA クラスターのスタンバイです。バックアップは実行せず、変更を拒否します。変更はアクティブインスタンスで行ってください。アクティブインスタンスが停止すると自動的に引き継ぎます。",
      cluster: "クラスター",
      active_instance: "アクティブインスタンス",
      unknown: "不明",
    },
  },
  ru: {
    ha: {
      standby_title: "Резервный экземпляр: только чтение",
      standby_desc: "Этот экземпляр — резервный в кластере высокой доступности active-passive. Он не выполняет резервное копирование и отклоняет изменения; вносите их на активном экземпляре. Он сам возьмёт работу на себя, если активный экземпляр остановится.",
      cluster: "Кластер",
      active_instance: "Активный экземпляр",
      unknown: "неизвестно",
    },
  },
};

if (typeof translations === "object" && typeof mergeTranslations === "function") {
  Object.keys(HA_TRANSLATIONS).forEach(lang => {
    if (translations[lang]) mergeTranslations(translations[lang], HA_TRANSLATIONS[lang]);
  });
}

// haRenderRole shows the standby banner when the health data reports the standby
// role, and hides it otherwise (single instance, active instance, or no answer).
function haRenderRole(data) {
  const banner = document.getElementById("ha-standby-banner");
  if (!banner) return;
  const ha = data && typeof data.ha === "object" && data.ha ? data.ha : null;
  const standby = !!data && data.role === "standby" && !!ha;
  banner.hidden = !standby;
  if (!standby) return;
  const unknown = typeof t === "function" ? t("ha.unknown") : "unknown";
  const cluster = document.getElementById("ha-standby-cluster");
  if (cluster) cluster.textContent = typeof ha.cluster === "string" && ha.cluster ? ha.cluster : unknown;
  const holder = document.getElementById("ha-standby-holder");
  if (holder) holder.textContent = typeof ha.holder === "string" && ha.holder ? ha.holder : unknown;
}
