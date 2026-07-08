import { initSupportNotificationPermissionPrompt, maybeNotifySupportMessage, setNavSupportUnread, setSupportWidgetUnread } from "./support_notifications.js?v=2026061511";

export function initSupportChat(root = document) {
  initSupportNotificationPermissionPrompt();
  root.querySelectorAll("[data-support-root]").forEach((node) => {
    if (!(node instanceof HTMLElement) || node.dataset.supportReady === "true") {
      return;
    }
    node.dataset.supportReady = "true";
    createSupportClient(node);
  });
}

function createSupportClient(root) {
  const admin = root.dataset.supportAdmin === "true";
  const widget = root.hasAttribute("data-support-widget");
  const single = root.dataset.supportSingle === "true";
  const wsPath = root.dataset.supportWs || "/support/ws";
  const widgetToggle = root.querySelector("[data-support-widget-toggle]");
  const widgetClose = root.querySelector("[data-support-widget-close]");
  const widgetPanel = root.querySelector(".support-widget-panel");
  const widgetDrag = root.querySelector("[data-support-widget-drag]");
  const widgetResize = root.querySelector("[data-support-widget-resize]");
  const list = root.querySelector("[data-support-ticket-list]");
  const chat = root.querySelector(".support-chat");
  const messages = root.querySelector("[data-support-messages]");
  const compose = root.querySelector("[data-support-compose]");
  const connection = root.querySelector("[data-support-connection]");
  const activeTitle = root.querySelector("[data-support-active-title]");
  const activeMeta = root.querySelector("[data-support-active-meta]");
  const activeAvatar = root.querySelector("[data-support-active-avatar]");
  const error = root.querySelector("[data-support-error]");
  const ticketInput = root.querySelector("[data-support-compose-ticket]");
  const titleField = root.querySelector("[data-support-title-field]");
  const newButton = root.querySelector("[data-support-new]");
  const search = root.querySelector("[data-support-search]");
  const tickets = new Map();
  const ticketOrder = [];
  const messageIDs = new Set();
  const pendingRequests = new Map();
  const labels = {
    connecting: root.dataset.supportConnecting || "Connecting",
    connected: root.dataset.supportConnected || "Connected",
    disconnected: root.dataset.supportDisconnected || "Disconnected",
    emptyTickets: root.dataset.supportEmpty || "",
    emptyMessages: root.dataset.supportEmptyMessages || "",
    noActive: root.dataset.supportNoActive || "",
    status: {
      open: root.dataset.supportStatusOpen || "open",
      pending_admin: root.dataset.supportStatusPendingAdmin || "waiting for support",
      pending_user: root.dataset.supportStatusPendingUser || "waiting for user",
      resolved: root.dataset.supportStatusResolved || "resolved",
      closed: root.dataset.supportStatusClosed || "closed",
    },
  };
  let activeID = String(root.dataset.supportActive || "");
  let activeLoaded = Boolean(activeID);
  let socket = null;
  let reconnectTimer = 0;
  let connectStarted = false;
  let connectOnOpen = Boolean(!widget);

  const setWidgetOpen = (open) => {
    if (!widget) {
      return;
    }
    root.classList.toggle("collapsed", !open);
    if (widgetPanel instanceof HTMLElement) {
      widgetPanel.hidden = !open;
      if (open) {
        placeWidgetPanel(widgetPanel);
      }
    }
  };

  root.querySelectorAll("[data-support-ticket-id]").forEach((item) => {
    if (!(item instanceof HTMLElement)) {
      return;
    }
    const id = item.dataset.supportTicketId || "";
    if (id) {
      if (!ticketOrder.includes(id)) {
        ticketOrder.push(id);
      }
      tickets.set(id, {
        id,
        title: text(item.querySelector(".support-ticket-title")),
        user_id: item.dataset.supportUserId || "",
        username: admin ? text(item.querySelector("[data-support-user-link], .support-ticket-title")) : "",
        status: "",
        last_message: text(item.querySelector(".support-ticket-preview")),
        updated_at: "",
      });
    }
  });

  const send = (payload, pending = null) => {
    ensureConnected();
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      showError(labels.disconnected);
      return "";
    }
    const id = requestID();
    if (pending) {
      pendingRequests.set(id, pending);
    }
    socket.send(JSON.stringify({ request_id: id, ...payload }));
    return id;
  };

  const connect = () => {
    connectStarted = true;
    window.clearTimeout(reconnectTimer);
    setConnection(labels.connecting, "connecting");
    const nextSocket = new WebSocket(websocketURL(wsPath));
    socket = nextSocket;
    nextSocket.addEventListener("open", () => {
      if (socket !== nextSocket) {
        return;
      }
      setConnection(labels.connected, "connected");
      hideError();
      refreshOpenWidgetReadState();
    });
    nextSocket.addEventListener("message", (event) => {
      if (socket !== nextSocket) {
        return;
      }
      const payload = parseJSON(event.data);
      handleEvent(payload);
    });
    nextSocket.addEventListener("close", () => {
      if (socket !== nextSocket) {
        return;
      }
      socket = null;
      setConnection(labels.disconnected, "disconnected");
      if (connectStarted) {
        reconnectTimer = window.setTimeout(connect, 1800);
      }
    });
    nextSocket.addEventListener("error", () => {
      if (socket !== nextSocket) {
        return;
      }
      setConnection(labels.disconnected, "disconnected");
    });
  };

  const ensureConnected = () => {
    if (socket && (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING)) {
      return;
    }
    connect();
  };

  const disconnect = () => {
    connectStarted = false;
    window.clearTimeout(reconnectTimer);
    if (socket) {
      const current = socket;
      socket = null;
      current.close();
    }
  };

  const chatCanAutoRead = () => !widget || !(widgetPanel instanceof HTMLElement) || !widgetPanel.hidden;

  const markActiveRead = (force = false) => {
    if (!activeID || (!force && !chatCanAutoRead())) {
      return;
    }
    send({ type: "support.ticket.read", ticket_id: activeID }, { type: "support.ticket.read" });
  };

  const refreshOpenWidgetReadState = () => {
    if (widget && chatCanAutoRead() && activeID && activeLoaded && socket?.readyState === WebSocket.OPEN) {
      markActiveRead(true);
    }
  };

  const handleEvent = (event) => {
    switch (event.type) {
      case "support.bootstrap":
        replaceTickets(event.tickets || []);
        setNavSupportUnread(event.unread);
        setSupportWidgetUnread(event.unread);
        if (single) {
          const latestID = firstTicketFromState();
          if (latestID) {
            loadTicket(latestID);
          } else {
            activeID = "";
            activeLoaded = false;
            renderActive(null, []);
          }
        } else if (activeID) {
          send({ type: "support.ticket.load", ticket_id: activeID });
        } else if (admin && firstTicketID()) {
          loadTicket(firstTicketID());
        } else {
          renderActive(null, []);
        }
        break;
      case "support.ticket.loaded":
        pendingRequests.delete(event.request_id || "");
        event.ticket = normalizeTicket(event.ticket);
        event.messages = (event.messages || []).map(normalizeMessage);
        upsertTicket(event.ticket);
        activeID = event.ticket?.id || "";
        activeLoaded = Boolean(activeID);
        renderTickets();
        renderActive(event.ticket, event.messages || []);
        if (widget && chatCanAutoRead()) {
          markActiveRead(true);
        }
        setNavSupportUnread(event.unread);
        setSupportWidgetUnread(event.unread);
        break;
      case "support.message.created":
        event.ticket = normalizeTicket(event.ticket);
        event.message = normalizeMessage(event.message);
        maybeNotifySupportMessage(event, admin);
        upsertTicket(event.ticket);
        bumpTicket(event.ticket?.id);
        renderTickets();
        if ((single || !activeID) && !admin && event.ticket?.id) {
          activeID = event.ticket.id;
        }
        if (event.ticket?.id === activeID) {
          activeLoaded = true;
        }
        if (ticketInput instanceof HTMLInputElement && event.ticket?.id === activeID) {
          ticketInput.value = activeID;
        }
        if (event.ticket?.id === activeID) {
          appendMessage(event.message);
          renderActiveHeader(event.ticket);
        }
        setNavSupportUnread(event.unread);
        setSupportWidgetUnread(event.unread);
        break;
      case "support.ticket.read":
        pendingRequests.delete(event.request_id || "");
        event.ticket = normalizeTicket(event.ticket);
        upsertTicket(event.ticket);
        renderTickets();
        setNavSupportUnread(event.unread);
        setSupportWidgetUnread(event.unread);
        break;
      case "support.ticket.deleted":
        pendingRequests.delete(event.request_id || "");
        event.ticket = normalizeTicket(event.ticket);
        removeTicket(event.ticket?.id || "");
        setNavSupportUnread(event.unread);
        setSupportWidgetUnread(event.unread);
        break;
      case "support.error":
        if (!handleSupportError(event)) {
          showError(event.error || "Support request failed");
        }
        break;
      default:
        break;
    }
  };

  const replaceTickets = (items) => {
    tickets.clear();
    const ordered = items.map(normalizeTicket).filter((ticket) => ticket?.id).sort(compareTickets);
    setTicketOrder(ordered);
    ordered.forEach((ticket) => upsertTicket(ticket));
    if (!activeID && !admin && !single && ordered.length > 0) {
      activeID = ordered[0].id || "";
    }
    renderTickets();
  };

  const upsertTicket = (ticket) => {
    ticket = normalizeTicket(ticket);
    if (!ticket || !ticket.id) {
      return;
    }
    tickets.set(ticket.id, ticket);
    rememberTicket(ticket.id);
  };

  const rememberTicket = (id) => {
    if (!id || ticketOrder.includes(id)) {
      return;
    }
    ticketOrder.push(id);
  };

  const setTicketOrder = (items) => {
    ticketOrder.splice(0, ticketOrder.length);
    items.forEach((ticket) => {
      if (ticket?.id && !ticketOrder.includes(ticket.id)) {
        ticketOrder.push(ticket.id);
      }
    });
  };

  const bumpTicket = (id) => {
    if (!id) {
      return;
    }
    const index = ticketOrder.indexOf(id);
    if (index >= 0) {
      ticketOrder.splice(index, 1);
    }
    ticketOrder.unshift(id);
  };

  const removeTicket = (id) => {
    if (!id) {
      return;
    }
    tickets.delete(id);
    const index = ticketOrder.indexOf(id);
    if (index >= 0) {
      ticketOrder.splice(index, 1);
    }
    const deletingActive = id === activeID;
    if (deletingActive) {
      activeID = "";
      activeLoaded = false;
      if (ticketInput instanceof HTMLInputElement) {
        ticketInput.value = "";
      }
    }
    renderTickets();
    if (deletingActive) {
      renderActive(null, []);
      if (admin && ticketOrder.length > 0) {
        loadTicket(ticketOrder[0]);
      }
    }
  };

  const orderedTickets = (query) => {
    const seen = new Set();
    const ordered = [];
    ticketOrder.forEach((id) => {
      const ticket = tickets.get(id);
      if (ticket && ticketMatches(ticket, query)) {
        seen.add(id);
        ordered.push(ticket);
      }
    });
    Array.from(tickets.values())
      .filter((ticket) => !seen.has(ticket.id) && ticketMatches(ticket, query))
      .sort(compareTickets)
      .forEach((ticket) => ordered.push(ticket));
    return ordered;
  };

  const renderTickets = () => {
    if (!(list instanceof HTMLElement)) {
      return;
    }
    list.replaceChildren();
    const query = search instanceof HTMLInputElement ? search.value.trim().toLowerCase() : "";
    const items = orderedTickets(query);
    if (items.length === 0) {
      const empty = document.createElement("div");
      empty.className = "support-empty";
      empty.dataset.supportEmpty = "";
      empty.textContent = labels.emptyTickets;
      list.appendChild(empty);
      return;
    }
    items.forEach((ticket) => {
      const button = document.createElement("div");
      button.role = "button";
      button.tabIndex = 0;
      button.className = `support-ticket${ticket.id === activeID ? " active" : ""}`;
      button.dataset.supportTicketId = ticket.id;
      const avatar = element("span", "support-ticket-avatar", admin ? avatarInitial(ticket.username || ticket.user_id || ticket.title) : "客");
      const main = element("span", "support-ticket-main", "");
      const row = element("span", "support-ticket-row", "");
      const time = element("span", "support-ticket-time", shortTime(ticket.updated_at));
      const unread = unreadBadge(ticket);
      if (unread) {
        time.append(unread);
      }
      row.append(
        admin ? createSupportUserLink(ticket, "support-ticket-title") : element("span", "support-ticket-title", ticket.title || ticket.id),
        time
      );
      main.append(
        row,
        element("span", "support-ticket-preview", ticket.last_message || "")
      );
      button.append(
        avatar,
        main
      );
      if (admin) {
        button.append(createTicketDeleteButton(ticket.id));
      }
      list.appendChild(button);
    });
  };

  const loadTicket = (id, options = {}) => {
    if (!id) {
      return;
    }
    activeID = id;
    activeLoaded = false;
    renderTickets();
    hideError();
    send({ type: "support.ticket.load", ticket_id: id }, { type: "support.ticket.load" });
    if (options.read) {
      markActiveRead(true);
    }
  };

  const firstTicketFromState = () => {
    const first = Array.from(tickets.values()).sort(compareTickets)[0];
    return first?.id || "";
  };

  const renderActive = (ticket, incomingMessages) => {
    renderActiveHeader(ticket);
    if (chat instanceof HTMLElement) {
      chat.classList.toggle("is-empty", !ticket?.id && !single);
    }
    if (ticketInput instanceof HTMLInputElement) {
      ticketInput.value = ticket?.id || "";
    }
    if (titleField instanceof HTMLElement) {
      titleField.hidden = Boolean(admin || single || ticket?.id);
    }
    if (!(messages instanceof HTMLElement)) {
      return;
    }
    messageIDs.clear();
    messages.replaceChildren();
    if (!ticket?.id || incomingMessages.length === 0) {
      if (single) {
        const empty = document.createElement("div");
        empty.className = "support-empty";
        empty.dataset.supportMessageEmpty = "";
        empty.textContent = "发送消息开始咨询";
        messages.appendChild(empty);
        return;
      }
      const empty = document.createElement("div");
      empty.className = "support-empty";
      empty.dataset.supportMessageEmpty = "";
      empty.textContent = labels.emptyMessages;
      messages.appendChild(empty);
      return;
    }
    incomingMessages.forEach((message) => appendMessage(message));
  };

  const renderActiveHeader = (ticket) => {
    if (activeTitle instanceof HTMLElement) {
      activeTitle.textContent = single ? "在线客服" : ticket?.title || labels.noActive;
    }
    if (activeMeta instanceof HTMLElement) {
      renderActiveMeta(activeMeta, ticket, admin, single, labels);
    }
    if (activeAvatar instanceof HTMLElement) {
      activeAvatar.textContent = single ? "客" : ticket ? (admin ? avatarInitial(ticket.username || ticket.user_id || ticket.title) : "客") : "-";
    }
    setText("[data-support-detail-user]", ticket?.username || "-");
    setText("[data-support-detail-status]", statusLabel(ticket?.status, labels) || "-");
    setText("[data-support-detail-updated]", formatTime(ticket?.updated_at) || "-");
  };

  const appendMessage = (message) => {
    message = normalizeMessage(message);
    if (!message?.id || !(messages instanceof HTMLElement) || messageIDs.has(message.id)) {
      return;
    }
    messages.querySelector("[data-support-message-empty]")?.remove();
    messageIDs.add(message.id);
    const article = document.createElement("article");
    const ownMessage = admin ? message.actor_role === "admin" : message.actor_role !== "admin";
    article.className = `support-message ${ownMessage ? "admin" : "user"}`;
    article.dataset.supportMessageId = message.id;
    article.append(
      element("div", "support-message-body", message.body || ""),
      element("div", "support-message-meta", formatTime(message.created_at))
    );
    messages.appendChild(article);
    messages.scrollTop = messages.scrollHeight;
  };

  const showError = (value) => {
    if (error instanceof HTMLElement) {
      error.textContent = value;
      error.hidden = false;
    }
  };

  const hideError = () => {
    if (error instanceof HTMLElement) {
      error.textContent = "";
      error.hidden = true;
    }
  };

  const setConnection = (label, state) => {
    if (connection instanceof HTMLElement) {
      connection.textContent = label;
      connection.dataset.state = state;
    }
  };

  const setText = (selector, value) => {
    const node = root.querySelector(selector);
    if (node instanceof HTMLElement) {
      node.textContent = value;
    }
  };

  list?.addEventListener("click", (event) => {
    const userLink = event.target instanceof Element ? event.target.closest("[data-support-user-link]") : null;
    if (userLink instanceof HTMLElement) {
      event.stopPropagation();
      return;
    }
    const deleteButton = event.target instanceof Element ? event.target.closest("[data-support-ticket-delete]") : null;
    if (deleteButton instanceof HTMLElement) {
      event.preventDefault();
      event.stopPropagation();
      const ticket = deleteButton.closest("[data-support-ticket-id]");
      const ticketID = ticket instanceof HTMLElement ? ticket.dataset.supportTicketId || "" : "";
      if (ticketID && window.confirm("删除这个会话及其消息？")) {
        send({ type: "support.ticket.delete", ticket_id: ticketID });
      }
      return;
    }
    const ticket = event.target instanceof Element ? event.target.closest("[data-support-ticket-id]") : null;
    if (ticket instanceof HTMLElement) {
      loadTicket(ticket.dataset.supportTicketId || "", { read: true });
    }
  });

  list?.addEventListener("keydown", (event) => {
    if (!(event instanceof KeyboardEvent)) {
      return;
    }
    if (event.key !== "Enter" && event.key !== " ") {
      return;
    }
    const userLink = event.target instanceof Element ? event.target.closest("[data-support-user-link]") : null;
    if (userLink instanceof HTMLElement) {
      return;
    }
    const deleteButton = event.target instanceof Element ? event.target.closest("[data-support-ticket-delete]") : null;
    if (deleteButton instanceof HTMLElement) {
      return;
    }
    const target = event.target instanceof Element ? event.target.closest("[data-support-ticket-id]") : null;
    if (target instanceof HTMLElement) {
      event.preventDefault();
      loadTicket(target.dataset.supportTicketId || "", { read: true });
    }
  });

  newButton?.addEventListener("click", () => {
    activeID = "";
    activeLoaded = false;
    renderTickets();
    renderActive(null, []);
    if (widget && chat instanceof HTMLElement) {
      chat.classList.remove("is-empty");
    }
    if (widget && messages instanceof HTMLElement) {
      messages.replaceChildren();
    }
    const titleInput = compose?.querySelector('input[name="title"]');
    if (titleInput instanceof HTMLInputElement) {
      titleInput.focus();
    }
  });

  search?.addEventListener("input", () => renderTickets());

  widgetToggle?.addEventListener("click", () => {
    connectOnOpen = true;
    ensureConnected();
    setWidgetOpen(true);
    refreshOpenWidgetReadState();
  });
  widgetClose?.addEventListener("click", () => {
    connectOnOpen = false;
    disconnect();
    setWidgetOpen(false);
  });
  if (widget && widgetPanel instanceof HTMLElement) {
    initWidgetMove(widgetPanel, widgetDrag);
    initWidgetResize(widgetPanel, widgetResize);
    window.addEventListener("resize", () => {
      if (!widgetPanel.hidden) {
        placeWidgetPanel(widgetPanel);
      }
    });
  }

  const submitCompose = () => {
    if (!(compose instanceof HTMLFormElement)) {
      return false;
    }
    const form = new FormData(compose);
    const body = String(form.get("body") || "").trim();
    const title = String(form.get("title") || "").trim() || defaultTicketTitle(body);
    const ticketID = single && !activeLoaded ? "" : String(form.get("ticket_id") || activeID || "").trim();
    if (!body) {
      return false;
    }
    const ok = ticketID
        ? send({ type: "support.message.send", ticket_id: ticketID, body }, { type: "support.message.send", title, body })
        : send({ type: "support.ticket.create", title, body }, { type: "support.ticket.create", title, body });
    if (ok) {
      compose.reset();
      hideError();
    }
    return ok;
  };

  const handleSupportError = (event) => {
    const pending = pendingRequests.get(event.request_id || "");
    pendingRequests.delete(event.request_id || "");
    const message = String(event.error || "");
    const code = String(event.error_code || "");
    if (pending?.type === "support.ticket.read") {
      return true;
    }
    if (single && (code === "not_found" || message.toLowerCase().includes("not found"))) {
      activeID = "";
      activeLoaded = false;
      if (ticketInput instanceof HTMLInputElement) {
        ticketInput.value = "";
      }
      renderActive(null, []);
      hideError();
      return true;
    }
    if (single && code === "auth_required") {
      if (connection instanceof HTMLElement) {
        connection.textContent = message || "登录状态已失效，请刷新页面后重新登录";
        connection.dataset.state = "disconnected";
      }
      hideError();
      return true;
    }
    if (single) {
      activeLoaded = false;
      if (ticketInput instanceof HTMLInputElement) {
        ticketInput.value = "";
      }
      renderActive(null, []);
      hideError();
      return true;
    }
    return false;
  };

  compose?.addEventListener("submit", (event) => {
    event.preventDefault();
    submitCompose();
  });

  compose?.querySelector('textarea[name="body"]')?.addEventListener("keydown", (event) => {
    if (event instanceof KeyboardEvent && event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      submitCompose();
    }
  });

  window.addEventListener("beforeunload", () => {
    disconnect();
  });

  if (connectOnOpen) {
    ensureConnected();
  }
}

function defaultTicketTitle(body) {
  const value = String(body || "").trim().replace(/\s+/g, " ");
  if (!value) {
    return "在线客服";
  }
  return value.length > 32 ? `${value.slice(0, 32)}...` : value;
}

function normalizeTicket(ticket) {
  if (!ticket) {
    return null;
  }
  return {
    ...ticket,
    id: ticket.id ?? ticket.ID ?? "",
    user_id: ticket.user_id ?? ticket.UserID ?? "",
    username: ticket.username ?? ticket.Username ?? "",
    title: ticket.title ?? ticket.Title ?? "",
    status: ticket.status ?? ticket.Status ?? "",
    last_message: ticket.last_message ?? ticket.LastMessage ?? "",
    last_actor_id: ticket.last_actor_id ?? ticket.LastActorID ?? "",
    created_at: ticket.created_at ?? ticket.CreatedAt ?? "",
    updated_at: ticket.updated_at ?? ticket.UpdatedAt ?? "",
    unread_count: ticket.unread_count ?? ticket.UnreadCount ?? 0,
  };
}

function normalizeMessage(message) {
  if (!message) {
    return null;
  }
  return {
    ...message,
    id: message.id ?? message.ID ?? "",
    ticket_id: message.ticket_id ?? message.TicketID ?? "",
    actor_id: message.actor_id ?? message.ActorID ?? "",
    actor_role: message.actor_role ?? message.ActorRole ?? "",
    body: message.body ?? message.Body ?? "",
    created_at: message.created_at ?? message.CreatedAt ?? "",
  };
}

function websocketURL(path) {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}${path}`;
}

function requestID() {
  if (window.crypto?.randomUUID) {
    return window.crypto.randomUUID();
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function parseJSON(value) {
  try {
    return JSON.parse(value || "{}");
  } catch {
    return {};
  }
}

function compareTickets(a, b) {
  const at = Date.parse(a?.updated_at || "") || 0;
  const bt = Date.parse(b?.updated_at || "") || 0;
  if (at !== bt) {
    return bt - at;
  }
  return String(a?.id || "").localeCompare(String(b?.id || ""));
}

function ticketMeta(ticket, admin, labels) {
  const parts = [];
  if (admin && ticket.username) {
    parts.push(ticket.username);
  }
  parts.push(statusLabel(ticket.status, labels));
  const when = formatTime(ticket.updated_at);
  if (when) {
    parts.push(when);
  }
  return parts.filter(Boolean).join(" · ");
}

function renderActiveMeta(node, ticket, admin, single, labels) {
  if (single) {
    node.textContent = statusLabel(ticket?.status || "pending_admin", labels);
    return;
  }
  if (!ticket) {
    node.textContent = "";
    return;
  }
  if (!admin) {
    node.textContent = ticketMeta(ticket, admin, labels);
    return;
  }
  node.replaceChildren(createSupportUserLink(ticket, "support-active-user-link"));
  const meta = [
    statusLabel(ticket.status, labels),
    formatTime(ticket.updated_at),
  ].filter(Boolean).join(" · ");
  if (meta) {
    node.append(document.createTextNode(` · ${meta}`));
  }
}

function createSupportUserLink(ticket, className = "") {
  const label = supportUserLabel(ticket);
  const href = supportUserURL(ticket);
  if (!href) {
    return element("span", className, label);
  }
  const link = document.createElement("a");
  link.className = className ? `${className} support-user-link` : "support-user-link";
  link.dataset.supportUserLink = "";
  link.href = href;
  link.textContent = label;
  return link;
}

function supportUserLabel(ticket) {
  return ticket?.username || ticket?.user_id || ticket?.title || ticket?.id || "";
}

function supportUserURL(ticket) {
  const userID = String(ticket?.user_id || "").trim();
  return userID ? `/admin/users?q=${encodeURIComponent(userID)}` : "";
}

function ticketMatches(ticket, query) {
  if (!query) {
    return true;
  }
  return [
    ticket?.id,
    ticket?.title,
    ticket?.username,
    ticket?.user_id,
    ticket?.last_message,
    ticket?.status,
  ].some((value) => String(value || "").toLowerCase().includes(query));
}

function avatarInitial(value) {
  const text = String(value || "").trim();
  return text ? text[0].toUpperCase() : "-";
}

function statusLabel(status, labels) {
  return labels?.status?.[status] || status || "";
}

function formatTime(value) {
  const date = new Date(value || "");
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  return date.toLocaleString();
}

function shortTime(value) {
  const date = new Date(value || "");
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function unreadBadge(ticket) {
  if (!ticket || ticket.unread_count <= 0) {
    return null;
  }
  const badge = document.createElement("span");
  badge.className = "support-ticket-unread";
  badge.textContent = String(Math.min(99, Number(ticket.unread_count || 0)));
  return badge;
}

function createTicketDeleteButton(ticketID) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "support-ticket-delete";
  button.dataset.supportTicketDelete = "";
  button.dataset.supportTicketDeleteID = ticketID;
  button.setAttribute("aria-label", "删除会话");
  button.title = "删除会话";
  const icon = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  icon.setAttribute("viewBox", "0 0 24 24");
  icon.setAttribute("aria-hidden", "true");
  ["M4 7h16", "M10 11v6", "M14 11v6", "M6 7l1 14h10l1-14", "M9 7V4h6v3"].forEach((d) => {
    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", d);
    icon.appendChild(path);
  });
  button.appendChild(icon);
  return button;
}

function element(tag, className, value) {
  const node = document.createElement(tag);
  node.className = className;
  node.textContent = value;
  return node;
}

function text(node) {
  return node instanceof HTMLElement ? node.textContent.trim() : "";
}

function firstTicketID() {
  const first = document.querySelector("[data-support-root] [data-support-ticket-id]");
  return first instanceof HTMLElement ? first.dataset.supportTicketId || "" : "";
}

const widgetPanelStorageKey = "ag:support-widget-panel";
const widgetSinglePanelStorageKey = "ag:support-widget-panel-single-v5";

function placeWidgetPanel(panel) {
  if (supportWidgetUsesMobilePanel()) {
    Object.assign(panel.style, {
      left: "",
      top: "",
      right: "",
      bottom: "",
      width: "",
      height: "",
    });
    return;
  }
  const single = panel.closest("[data-support-single='true']") instanceof HTMLElement;
  const saved = loadWidgetPanelState(single);
  const viewport = widgetViewport();
  const minWidth = Math.min(360, viewport.width - 24);
  const minHeight = Math.min(360, viewport.height - 24);
  const defaultWidth = Math.min(single ? 420 : 760, viewport.width - 32);
  const defaultHeight = Math.min(single ? 540 : 620, viewport.height - 112);
  const width = clamp(saved.width || panel.offsetWidth || defaultWidth, minWidth, viewport.width - 24);
  const height = clamp(saved.height || panel.offsetHeight || defaultHeight, minHeight, viewport.height - 24);
  const defaultLeft = viewport.width - width - 24;
  const defaultTop = viewport.height - height - 90;
  const left = clamp(Number.isFinite(saved.left) ? saved.left : defaultLeft, 12, viewport.width - width - 12);
  const top = clamp(Number.isFinite(saved.top) ? saved.top : defaultTop, 12, viewport.height - height - 12);
  Object.assign(panel.style, {
    left: `${left}px`,
    top: `${top}px`,
    right: "auto",
    bottom: "auto",
    width: `${width}px`,
    height: `${height}px`,
  });
}

function loadWidgetPanelState(single = false) {
  try {
    const saved = parseJSON(window.localStorage?.getItem(widgetPanelKey(single)) || "{}");
    return {
      left: finiteNumber(saved.left),
      top: finiteNumber(saved.top),
      width: finiteNumber(saved.width),
      height: finiteNumber(saved.height),
    };
  } catch {
    return {};
  }
}

function finiteNumber(value) {
  const number = Number(value);
  return Number.isFinite(number) ? number : undefined;
}

function saveWidgetPanel(panel) {
  const rect = panel.getBoundingClientRect();
  try {
    const single = panel.closest("[data-support-single='true']") instanceof HTMLElement;
    window.localStorage?.setItem(widgetPanelKey(single), JSON.stringify({
      left: Math.round(rect.left),
      top: Math.round(rect.top),
      width: Math.round(rect.width),
      height: Math.round(rect.height),
    }));
  } catch {
    // Ignore storage failures; the widget should still open normally.
  }
}

function widgetPanelKey(single) {
  return single ? widgetSinglePanelStorageKey : widgetPanelStorageKey;
}

function initWidgetMove(panel, handle) {
  if (!(handle instanceof HTMLElement)) {
    return;
  }
  handle.addEventListener("pointerdown", (event) => {
    if (supportWidgetUsesMobilePanel()) {
      return;
    }
    if (event.target instanceof Element && event.target.closest("button, input, textarea, select, a")) {
      return;
    }
    event.preventDefault();
    const start = panel.getBoundingClientRect();
    const startX = event.clientX;
    const startY = event.clientY;
    handle.setPointerCapture?.(event.pointerId);
    const move = (moveEvent) => {
      const viewport = widgetViewport();
      const left = clamp(start.left + moveEvent.clientX - startX, 12, viewport.width - start.width - 12);
      const top = clamp(start.top + moveEvent.clientY - startY, 12, viewport.height - start.height - 12);
      panel.style.left = `${left}px`;
      panel.style.top = `${top}px`;
      panel.style.right = "auto";
      panel.style.bottom = "auto";
    };
    const done = () => {
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("pointerup", done);
      handle.removeEventListener("pointercancel", done);
      saveWidgetPanel(panel);
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", done);
    handle.addEventListener("pointercancel", done);
  });
}

function initWidgetResize(panel, handle) {
  if (!(handle instanceof HTMLElement)) {
    return;
  }
  handle.addEventListener("pointerdown", (event) => {
    if (supportWidgetUsesMobilePanel()) {
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    const start = panel.getBoundingClientRect();
    const startX = event.clientX;
    const startY = event.clientY;
    handle.setPointerCapture?.(event.pointerId);
    const move = (moveEvent) => {
      const viewport = widgetViewport();
      const width = clamp(start.width + moveEvent.clientX - startX, 360, viewport.width - start.left - 12);
      const height = clamp(start.height + moveEvent.clientY - startY, 360, viewport.height - start.top - 12);
      panel.style.width = `${width}px`;
      panel.style.height = `${height}px`;
    };
    const done = () => {
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("pointerup", done);
      handle.removeEventListener("pointercancel", done);
      saveWidgetPanel(panel);
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", done);
    handle.addEventListener("pointercancel", done);
  });
}

function widgetViewport() {
  return {
    width: Math.max(320, window.innerWidth || document.documentElement.clientWidth || 320),
    height: Math.max(320, window.innerHeight || document.documentElement.clientHeight || 320),
  };
}

function supportWidgetUsesMobilePanel() {
  return window.matchMedia?.("(max-width: 760px)").matches || (window.innerWidth || 0) <= 760;
}

function clamp(value, min, max) {
  if (!Number.isFinite(value)) {
    return min;
  }
  return Math.min(Math.max(value, min), Math.max(min, max));
}
