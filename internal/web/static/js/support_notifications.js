const supportNotificationPromptKey = "__supportNotificationPromptReady";
const supportNotificationChannelName = "ag:support-notifications";
const supportNotificationStoreKey = "ag:support-notifications-seen-v1";
const supportNotificationServiceWorkerURL = "/support-notification-sw.js?v=2026061503";
const supportNotificationTTL = 10 * 60 * 1000;
const supportNotificationStoreLimit = 64;
const supportNotificationSoundCooldown = 1200;

let supportNotificationCache = null;
let supportNotificationChannel = null;
let supportNotificationServiceWorkerRegistration = null;
let supportNotificationServiceWorkerPromise = null;
let supportNotificationPermissionPromise = null;
let supportNotificationAudioContext = null;
let supportNotificationLastSoundAt = 0;
const supportNotificationTabID = `${Date.now()}-${Math.random().toString(16).slice(2)}`;

export function initSupportNotificationPermissionPrompt(root = document) {
  if (window[supportNotificationPromptKey]) {
    return;
  }
  window[supportNotificationPromptKey] = true;
  installSupportNotificationDiagnostics();
  registerSupportNotificationServiceWorker();
  const request = () => {
    requestSupportNotificationPermission();
    primeSupportNotificationSound();
  };
  window.addEventListener("pointerdown", request, { once: true, passive: true });
  window.addEventListener("keydown", request, { once: true });
}

export function maybeNotifySupportMessage(event, admin) {
  const message = normalizeSupportMessage(event?.message);
  const ticket = normalizeSupportTicket(event?.ticket);
  if (!message || !ticket) {
    reportSupportNotificationState("skipped", { reason: "missing_payload" });
    return;
  }
  if (!supportMessageIsIncoming(message, admin)) {
    reportSupportNotificationState("skipped", { reason: "outgoing", actor_role: message.actor_role, admin });
    return;
  }
  if (supportNotificationSeen(message.id)) {
    reportSupportNotificationState("skipped", { reason: "seen", message_id: message.id });
    return;
  }
  const title = admin ? ticket.username || ticket.title || "用户消息" : "在线客服";
  const body = message.body || ticket.last_message || "你有一条新消息";
  claimSupportNotification(message.id, async () => {
    flashSupportTitle();
    playSupportNotificationSound();
    const shown = await showSupportNotification(title, body, admin, message.id);
    if (shown) {
      rememberSupportNotification(message.id);
      releaseSupportNotificationClaim(message.id);
    } else {
      releaseSupportNotificationClaim(message.id);
    }
  });
}

export function supportMessageIsIncoming(message, admin) {
  return admin ? message.actor_role !== "admin" : message.actor_role === "admin";
}

export function setNavSupportUnread(count) {
  const value = Math.max(0, Number(count || 0));
  document.querySelectorAll("[data-nav-support-link]").forEach((link) => {
    if (!(link instanceof HTMLElement)) {
      return;
    }
    let badge = link.querySelector(".nav-badge");
    if (value <= 0) {
      badge?.remove();
      return;
    }
    if (!(badge instanceof HTMLElement)) {
      badge = document.createElement("span");
      badge.className = "nav-badge";
      link.appendChild(badge);
    }
    badge.textContent = String(value);
  });
}

export function setSupportUnread(count) {
  setNavSupportUnread(count);
  setSupportWidgetUnread(count);
}

export function setSupportWidgetUnread(count) {
  const value = Math.max(0, Number(count || 0));
  document.querySelectorAll("[data-support-widget-badge]").forEach((badge) => {
    if (!(badge instanceof HTMLElement)) {
      return;
    }
    badge.hidden = value <= 0;
    badge.textContent = value > 99 ? "99+" : String(value);
  });
}

export function supportNotificationsAvailable() {
  return "Notification" in window && (window.isSecureContext || window.location.hostname === "localhost" || window.location.hostname === "127.0.0.1");
}

function requestSupportNotificationPermission() {
  if (!supportNotificationsAvailable()) {
    return false;
  }
  if (Notification.permission === "granted") {
    return true;
  }
  if (Notification.permission !== "default") {
    return false;
  }
  if (supportNotificationPermissionPromise) {
    return supportNotificationPermissionPromise;
  }
  try {
    const result = Notification.requestPermission();
    supportNotificationPermissionPromise = Promise.resolve(result)
      .then((permission) => permission === "granted")
      .catch(() => false)
      .finally(() => {
        supportNotificationPermissionPromise = null;
      });
    return supportNotificationPermissionPromise;
  } catch {
    return Promise.resolve(false);
  }
}

async function sendSupportNotificationTest() {
  const messageID = `manual-${Date.now()}`;
  const permitted = await requestSupportNotificationPermission();
  playSupportNotificationSound({ force: true });
  if (!permitted) {
    reportSupportNotificationState("blocked", { reason: "permission_not_granted_for_test", permission: "Notification" in window ? Notification.permission : "unsupported" });
    return false;
  }
  return showSupportNotification("客服通知测试", "这是一条桌面通知测试", document.querySelector("[data-nav-support-link]") instanceof HTMLElement, messageID);
}

async function registerSupportNotificationServiceWorker() {
  if (!("serviceWorker" in navigator) || !supportNotificationsAvailable()) {
    return null;
  }
  if (supportNotificationServiceWorkerRegistration) {
    return supportNotificationServiceWorkerRegistration;
  }
  if (supportNotificationServiceWorkerPromise) {
    return supportNotificationServiceWorkerPromise;
  }
  try {
    supportNotificationServiceWorkerPromise = navigator.serviceWorker
      .register(supportNotificationServiceWorkerURL, { scope: "/" })
      .then(() => navigator.serviceWorker.ready)
      .then((registration) => {
        supportNotificationServiceWorkerRegistration = registration;
        return registration;
      })
      .catch(() => null)
      .finally(() => {
        supportNotificationServiceWorkerPromise = null;
      });
    return supportNotificationServiceWorkerPromise;
  } catch {
    supportNotificationServiceWorkerRegistration = null;
    supportNotificationServiceWorkerPromise = null;
    return null;
  }
}

async function showSupportNotification(title, body, admin, messageID) {
  if (!supportNotificationsAvailable()) {
    reportSupportNotificationState("blocked", { reason: "unavailable" });
    return false;
  }
  if (Notification.permission === "default") {
    const permitted = await requestSupportNotificationPermission();
    if (!permitted) {
      reportSupportNotificationState("blocked", { reason: "permission_default", permission: Notification.permission });
      return false;
    }
  }
  if (Notification.permission !== "granted") {
    reportSupportNotificationState("blocked", { reason: "permission_denied", permission: Notification.permission });
    return false;
  }
  const options = {
    body: String(body || "").slice(0, 120),
    icon: "/favicon.ico",
    tag: `support-chat-${encodeURIComponent(String(messageID || Date.now()))}`,
    renotify: true,
    requireInteraction: true,
    data: { url: supportNotificationClickURL(admin) },
  };
  const registration = await registerSupportNotificationServiceWorker();
  if (registration?.showNotification) {
    try {
      await registration.showNotification(title, options);
      const activeNotifications = typeof registration.getNotifications === "function"
        ? await registration.getNotifications({ tag: options.tag }).catch(() => [])
        : [];
      reportSupportNotificationState("shown", {
        via: "service_worker",
        message_id: messageID,
        permission: Notification.permission,
        active_count: activeNotifications.length,
      });
      return true;
    } catch (error) {
      reportSupportNotificationState("failed", { via: "service_worker", message_id: messageID, error: String(error?.message || error || "") });
    }
  }
  try {
    const notification = new Notification(title, options);
    notification.onclick = () => {
      window.focus();
      notification.close();
    };
    reportSupportNotificationState("shown", { via: "window", message_id: messageID, permission: Notification.permission });
    return true;
  } catch (error) {
    reportSupportNotificationState("failed", { via: "window", message_id: messageID, error: String(error?.message || error || "") });
    return false;
  }
}

function supportNotificationClickURL(admin) {
  if (admin || document.querySelector("[data-nav-support-link]") instanceof HTMLElement) {
    return "/admin/support";
  }
  return "/support";
}

function installSupportNotificationDiagnostics() {
  try {
    window.__agSupportNotificationStatus = () => ({
      available: supportNotificationsAvailable(),
      permission: "Notification" in window ? Notification.permission : "unsupported",
      secure_context: window.isSecureContext,
      service_worker: "serviceWorker" in navigator,
      last_state: window.__agSupportNotificationLastState || null,
      visibility: document.visibilityState,
      focused: document.hasFocus?.() || false,
      origin: window.location.origin,
      user_agent: navigator.userAgent,
    });
    window.__agTestSupportNotification = async () => {
      return sendSupportNotificationTest();
    };
  } catch {
  }
}

function primeSupportNotificationSound() {
  const context = supportNotificationAudio();
  if (!context) {
    return;
  }
  try {
    if (context.state === "suspended") {
      context.resume().catch(() => {});
    }
    const gain = context.createGain();
    gain.gain.value = 0;
    gain.connect(context.destination);
    const oscillator = context.createOscillator();
    oscillator.frequency.value = 880;
    oscillator.connect(gain);
    oscillator.start();
    oscillator.stop(context.currentTime + 0.01);
  } catch {
  }
}

function playSupportNotificationSound(options = {}) {
  const now = Date.now();
  if (!options.force && now - supportNotificationLastSoundAt < supportNotificationSoundCooldown) {
    return;
  }
  const context = supportNotificationAudio();
  if (!context) {
    return;
  }
  supportNotificationLastSoundAt = now;
  const play = () => {
    try {
      const start = context.currentTime + 0.02;
      playSupportTone(context, start, 740);
      playSupportTone(context, start + 0.16, 980);
    } catch {
    }
  };
  try {
    if (context.state === "suspended") {
      context.resume().then(play).catch(() => {});
      return;
    }
    play();
  } catch {
  }
}

function playSupportTone(context, start, frequency) {
  const oscillator = context.createOscillator();
  const gain = context.createGain();
  oscillator.type = "sine";
  oscillator.frequency.setValueAtTime(frequency, start);
  gain.gain.setValueAtTime(0.0001, start);
  gain.gain.exponentialRampToValueAtTime(0.16, start + 0.018);
  gain.gain.exponentialRampToValueAtTime(0.0001, start + 0.11);
  oscillator.connect(gain);
  gain.connect(context.destination);
  oscillator.start(start);
  oscillator.stop(start + 0.13);
}

function supportNotificationAudio() {
  if (supportNotificationAudioContext) {
    return supportNotificationAudioContext;
  }
  const AudioContextClass = window.AudioContext || window.webkitAudioContext;
  if (typeof AudioContextClass !== "function") {
    return null;
  }
  try {
    supportNotificationAudioContext = new AudioContextClass();
    return supportNotificationAudioContext;
  } catch {
    return null;
  }
}

function flashSupportTitle(prefix = "新消息") {
  const original = document.title || "";
  const marker = `[${prefix}] `;
  if (document.title.startsWith(marker)) {
    return;
  }
  document.title = `${marker}${original}`;
  window.clearTimeout(window.__supportTitleTimer);
  window.__supportTitleTimer = window.setTimeout(() => {
    if (document.title === `${marker}${original}`) {
      document.title = original;
    }
  }, 8000);
}

function normalizeSupportTicket(ticket) {
  if (!ticket) {
    return null;
  }
  return {
    ...ticket,
    id: ticket.id ?? ticket.ID ?? "",
    user_id: ticket.user_id ?? ticket.UserID ?? "",
    username: ticket.username ?? ticket.Username ?? "",
    title: ticket.title ?? ticket.Title ?? "",
    last_message: ticket.last_message ?? ticket.LastMessage ?? "",
  };
}

function normalizeSupportMessage(message) {
  if (!message) {
    return null;
  }
  return {
    ...message,
    id: message.id ?? message.ID ?? "",
    actor_role: message.actor_role ?? message.ActorRole ?? "",
    body: message.body ?? message.Body ?? "",
  };
}

function supportNotificationSeen(messageID) {
  messageID = String(messageID || "").trim();
  if (!messageID) {
    return true;
  }
  const cache = supportNotificationCacheEntries();
  const seenAt = cache.get(messageID);
  return Number.isFinite(seenAt) && Date.now() - seenAt <= supportNotificationTTL;
}

function rememberSupportNotification(messageID) {
  messageID = String(messageID || "").trim();
  if (!messageID) {
    return;
  }
  const cache = supportNotificationCacheEntries();
  const now = Date.now();
  cache.set(messageID, now);
  pruneSupportNotificationCache(cache, now);
  persistSupportNotificationCache(cache);
  broadcastSupportNotificationSeen(messageID, now);
}

function claimSupportNotification(messageID, notify) {
  messageID = String(messageID || "").trim();
  if (!messageID || supportNotificationSeen(messageID)) {
    return;
  }
  const claimKey = supportNotificationClaimKey(messageID);
  const seenAt = Date.now();
  try {
    const storage = window.localStorage;
    if (!storage) {
      throw new Error("localStorage unavailable");
    }
    const token = `${supportNotificationTabID}-${Math.random().toString(16).slice(2)}`;
    storage.setItem(claimKey, JSON.stringify({ owner: supportNotificationTabID, token, seen_at: seenAt }));
    window.setTimeout(() => {
      if (supportNotificationSeen(messageID)) {
        return;
      }
      let claim = {};
      try {
        claim = JSON.parse(storage.getItem(claimKey) || "{}");
      } catch {
        return;
      }
      if (claim.owner !== supportNotificationTabID || claim.token !== token) {
        return;
      }
      notify();
    }, 40 + Math.floor(Math.random() * 40));
  } catch {
    notify();
  }
}

function supportNotificationClaimKey(messageID) {
  return `${supportNotificationStoreKey}:claim:${encodeURIComponent(messageID)}`;
}

function releaseSupportNotificationClaim(messageID) {
  const claimKey = supportNotificationClaimKey(messageID);
  try {
    window.localStorage?.removeItem(claimKey);
  } catch {
  }
}

function reportSupportNotificationState(status, detail = {}) {
  try {
    const payload = {
      status,
      permission: "Notification" in window ? Notification.permission : "unsupported",
      visible: document.visibilityState,
      focused: document.hasFocus?.() || false,
      ...detail,
    };
    if (window.__agSupportNotificationDebug) {
      console.debug("[support-notification]", payload);
    }
    window.__agSupportNotificationLastState = payload;
    document.dispatchEvent(new CustomEvent("ag:support-notification-debug", { detail: payload }));
  } catch {
  }
}

function supportNotificationCacheEntries() {
  if (supportNotificationCache instanceof Map) {
    return supportNotificationCache;
  }
  supportNotificationCache = new Map();
  supportNotificationChannel = supportNotificationChannel || createSupportNotificationChannel();
  try {
    const raw = window.localStorage?.getItem(supportNotificationStoreKey);
    if (!raw) {
      return supportNotificationCache;
    }
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) {
      return supportNotificationCache;
    }
    const now = Date.now();
    parsed.forEach((entry) => {
      const messageID = String(entry?.message_id || "").trim();
      const seenAt = Number(entry?.seen_at || 0);
      if (messageID && Number.isFinite(seenAt) && now - seenAt <= supportNotificationTTL) {
        supportNotificationCache.set(messageID, seenAt);
      }
    });
  } catch {
    supportNotificationCache = new Map();
  }
  return supportNotificationCache;
}

function pruneSupportNotificationCache(cache, now = Date.now()) {
  const entries = Array.from(cache.entries())
    .filter(([, seenAt]) => Number.isFinite(seenAt) && now - seenAt <= supportNotificationTTL)
    .sort((a, b) => a[1] - b[1]);
  cache.clear();
  entries.slice(-supportNotificationStoreLimit).forEach(([messageID, seenAt]) => {
    cache.set(messageID, seenAt);
  });
}

function persistSupportNotificationCache(cache) {
  try {
    const entries = Array.from(cache.entries())
      .map(([messageID, seenAt]) => ({ message_id: messageID, seen_at: seenAt }))
      .sort((a, b) => a.seen_at - b.seen_at)
      .slice(-supportNotificationStoreLimit);
    window.localStorage?.setItem(supportNotificationStoreKey, JSON.stringify(entries));
  } catch {
  }
}

function createSupportNotificationChannel() {
  if (typeof window.BroadcastChannel !== "function") {
    window.addEventListener("storage", handleSupportNotificationStorageEvent);
    return null;
  }
  window.addEventListener("storage", handleSupportNotificationStorageEvent);
  try {
    const channel = new BroadcastChannel(supportNotificationChannelName);
    channel.addEventListener("message", (event) => {
      const payload = event.data || {};
      if (payload.type === "support.notification.seen") {
        syncSupportNotificationSeen(payload.message_id, payload.seen_at, false);
      }
    });
    return channel;
  } catch {
    return null;
  }
}

function handleSupportNotificationStorageEvent(event) {
  if (event.key !== supportNotificationStoreKey || !event.newValue) {
    return;
  }
  try {
    const parsed = JSON.parse(event.newValue);
    if (!Array.isArray(parsed)) {
      return;
    }
    parsed.forEach((entry) => {
      syncSupportNotificationSeen(entry?.message_id, entry?.seen_at, false);
    });
  } catch {
  }
}

function broadcastSupportNotificationSeen(messageID, seenAt) {
  if (supportNotificationChannel && typeof window.BroadcastChannel === "function") {
    try {
      supportNotificationChannel.postMessage({ type: "support.notification.seen", message_id: messageID, seen_at: seenAt });
    } catch {
    }
  }
}

function syncSupportNotificationSeen(messageID, seenAt, persist = true) {
  messageID = String(messageID || "").trim();
  seenAt = Number(seenAt || 0);
  if (!messageID || !Number.isFinite(seenAt)) {
    return;
  }
  const cache = supportNotificationCacheEntries();
  const existing = cache.get(messageID);
  if (!Number.isFinite(existing) || existing < seenAt) {
    cache.set(messageID, seenAt);
    pruneSupportNotificationCache(cache, Date.now());
    if (persist) {
      persistSupportNotificationCache(cache);
      broadcastSupportNotificationSeen(messageID, seenAt);
    }
  }
}
