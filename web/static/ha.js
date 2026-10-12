/**
 * MongoRescue: the standby page of active-passive high availability.
 *
 * A standby serves no data and no sign-in: GET / returns standby.html, which loads
 * only this file. It reads the role, the cluster and the active instance from the
 * public GET /api/v1/health and shows them in the language the dashboard stored
 * (or the browser's), polling until the instance becomes active, then reloads into
 * the dashboard. On the dashboard (index.html), app.js checkHealth() calls
 * haRenderRole(data), which reloads into the standby page when the instance it is
 * talking to became a standby.
 *
 * Self-contained (no i18n.js): the standby page loads nothing else. Server values
 * reach the DOM only through textContent.
 */

// The texts of the standby page, in the eight languages of the dashboard.
const HA_TEXTS = {
  en: {
    standby_title: "Standby instance",
    standby_desc: "This MongoRescue instance is the standby of an active-passive HA cluster. It shows no data and accepts no sign-ins or changes. Open the active instance instead; this one takes over by itself when the active instance stops.",
    cluster: "Cluster",
    active_instance: "Active instance",
    unknown: "unknown",
  },
  tr: {
    standby_title: "Yedek örnek",
    standby_desc: "Bu MongoRescue örneği, aktif-pasif bir HA kümesinin yedek örneğidir. Veri göstermez; oturum açmayı ve değişiklikleri kabul etmez. Bunun yerine aktif örneği açın; aktif örnek durduğunda bu örnek görevi kendiliğinden devralır.",
    cluster: "Küme",
    active_instance: "Aktif örnek",
    unknown: "bilinmiyor",
  },
  de: {
    standby_title: "Standby-Instanz",
    standby_desc: "Diese MongoRescue-Instanz ist der Standby eines Active-Passive-HA-Clusters. Sie zeigt keine Daten und nimmt weder Anmeldungen noch Änderungen an. Öffnen Sie stattdessen die aktive Instanz; diese hier übernimmt von selbst, wenn die aktive Instanz ausfällt.",
    cluster: "Cluster",
    active_instance: "Aktive Instanz",
    unknown: "unbekannt",
  },
  es: {
    standby_title: "Instancia en espera",
    standby_desc: "Esta instancia de MongoRescue es la de reserva de un clúster HA activo-pasivo. No muestra datos ni acepta inicios de sesión ni cambios. Abra la instancia activa; esta toma el relevo por sí sola cuando la activa se detiene.",
    cluster: "Clúster",
    active_instance: "Instancia activa",
    unknown: "desconocida",
  },
  fr: {
    standby_title: "Instance de secours",
    standby_desc: "Cette instance MongoRescue est l'instance de secours d'un cluster HA actif-passif. Elle n'affiche aucune donnée et n'accepte ni connexion ni modification. Ouvrez plutôt l'instance active ; celle-ci prend le relais d'elle-même quand l'instance active s'arrête.",
    cluster: "Cluster",
    active_instance: "Instance active",
    unknown: "inconnue",
  },
  zh: {
    standby_title: "备用实例",
    standby_desc: "此 MongoRescue 实例是主备（active-passive）HA 集群中的备用实例。它不显示任何数据，也不接受登录或更改。请改为打开活动实例；活动实例停止时，此实例会自动接管。",
    cluster: "集群",
    active_instance: "活动实例",
    unknown: "未知",
  },
  ja: {
    standby_title: "スタンバイインスタンス",
    standby_desc: "この MongoRescue インスタンスはアクティブ/パッシブ HA クラスターのスタンバイです。データは表示せず、サインインや変更も受け付けません。代わりにアクティブインスタンスを開いてください。アクティブインスタンスが停止すると、このインスタンスが自動的に引き継ぎます。",
    cluster: "クラスター",
    active_instance: "アクティブインスタンス",
    unknown: "不明",
  },
  ru: {
    standby_title: "Резервный экземпляр",
    standby_desc: "Этот экземпляр MongoRescue — резервный в кластере высокой доступности active-passive. Он не показывает данные и не принимает ни входы, ни изменения. Откройте активный экземпляр; этот сам возьмёт работу на себя, если активный остановится.",
    cluster: "Кластер",
    active_instance: "Активный экземпляр",
    unknown: "неизвестно",
  },
};

// haLang returns the language the dashboard stored, else the browser's, else English.
function haLang() {
  let stored = "";
  try {
    stored = window.localStorage.getItem("mongorescue_lang") || "";
  } catch (err) {
    stored = "";
  }
  const candidates = [stored].concat(Array.from(navigator.languages || [navigator.language || ""]));
  for (const c of candidates) {
    const code = String(c || "").toLowerCase().split("-")[0];
    if (HA_TEXTS[code]) return code;
  }
  return "en";
}

// haText returns the text key in the page's language.
function haText(key) {
  const texts = HA_TEXTS[haLang()] || HA_TEXTS.en;
  return texts[key] || HA_TEXTS.en[key] || key;
}

// haRenderRole is called by the dashboard with the health data: when the instance
// became a standby, the dashboard has nothing left to show, so the page reloads
// into the standby page.
function haRenderRole(data) {
  if (data && data.role === "standby" && !document.getElementById("ha-standby-page")) {
    window.location.replace("/");
  }
}

// haStandbyPage fills the standby page and polls the health check until the
// instance becomes active.
function haStandbyPage() {
  const page = document.getElementById("ha-standby-page");
  if (!page) return;
  document.documentElement.lang = haLang();
  page.querySelectorAll("[data-ha-i18n]").forEach(el => {
    el.textContent = haText(el.getAttribute("data-ha-i18n"));
  });
  const show = (id, value) => {
    const el = document.getElementById(id);
    if (el) el.textContent = typeof value === "string" && value ? value : haText("unknown");
  };
  const poll = async () => {
    try {
      const res = await fetch("/api/v1/health", { credentials: "omit" });
      const json = await res.json();
      const data = json && json.data ? json.data : {};
      if (data.role === "active" || (res.ok && !data.ha)) {
        window.location.reload();
        return;
      }
      const ha = data.ha && typeof data.ha === "object" ? data.ha : {};
      show("ha-standby-cluster", ha.cluster);
      show("ha-standby-holder", ha.holder);
    } catch (err) {
      show("ha-standby-cluster", "");
      show("ha-standby-holder", "");
    }
  };
  poll();
  window.setInterval(poll, 10000);
}

haStandbyPage();
