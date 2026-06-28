export { initImageLab };

const SIZE_MAP = {
  standard: {
    "1:1": "1024x1024",
    "2:3": "1024x1536",
    "3:2": "1536x1024",
    "3:4": "768x1024",
    "4:3": "1024x768",
    "9:16": "1008x1792",
    "16:9": "1792x1008",
  },
  "2k": {
    "1:1": "2048x2048",
    "2:3": "1344x2016",
    "3:2": "2016x1344",
    "3:4": "1536x2048",
    "4:3": "2048x1536",
    "9:16": "1152x2048",
    "16:9": "2048x1152",
  },
  "4k": {
    "1:1": "2880x2880",
    "2:3": "2336x3504",
    "3:2": "3504x2336",
    "3:4": "2448x3264",
    "4:3": "3264x2448",
    "9:16": "2160x3840",
    "16:9": "3840x2160",
  },
};

const DEFAULT_LIMITS = {
  max_count: 20,
  max_input_images: 8,
  max_image_bytes: 12 * 1024 * 1024,
  max_total_bytes: 50 * 1024 * 1024,
};

function initImageLab(toastUI, confirmUI) {
  const root = document.querySelector("[data-image-lab]");
  if (!(root instanceof HTMLElement)) {
    return;
  }

  const refs = {
    form: root.querySelector("[data-image-lab-form]"),
    client: root.querySelector("[data-image-lab-client]"),
    model: root.querySelector("[data-image-lab-model]"),
    prompt: root.querySelector("[data-image-lab-prompt]"),
    upload: root.querySelector("[data-image-lab-upload]"),
    clearInputs: root.querySelector("[data-image-lab-clear-inputs]"),
    file: root.querySelector("[data-image-lab-file]"),
    inputs: root.querySelector("[data-image-lab-inputs]"),
    resolution: root.querySelector("[data-image-lab-resolution]"),
    resolutionOptions: root.querySelector("[data-image-lab-resolution-options]"),
    ratio: root.querySelector("[data-image-lab-ratio]"),
    ratioOptions: root.querySelector("[data-image-lab-ratio-options]"),
    count: root.querySelector("[data-image-lab-count]"),
    size: root.querySelector("[data-image-lab-size]"),
    submit: root.querySelector("[data-image-lab-submit]"),
    error: root.querySelector("[data-image-lab-error]"),
    note: root.querySelector("[data-image-lab-run-note]"),
    empty: root.querySelector("[data-image-lab-empty]"),
    runs: root.querySelector("[data-image-lab-tasks]"),
    clearResults: root.querySelector("[data-image-lab-clear-finished]"),
    history: root.querySelector("[data-image-lab-history]"),
    toggleHistory: root.querySelector("[data-image-lab-toggle-history]"),
    clearHistory: root.querySelector("[data-image-lab-clear-history]"),
  };

  if (!(refs.form instanceof HTMLFormElement) || !(refs.client instanceof HTMLSelectElement) || !(refs.model instanceof HTMLSelectElement)) {
    return;
  }

  const state = {
    clients: [],
    modelsByClient: {},
    limits: { ...DEFAULT_LIMITS },
    inputImages: [],
    runs: [],
    submission: null,
    history: [],
    historyCollapsed: false,
    bootstrapping: true,
    tasksLoading: false,
  };

  let previewClose = null;
  let renderTicker = 0;
  let submissionRenderFrame = 0;
  const jobPollers = new Map();

  const showToast = (message, tone = "info") => {
    if (!message) {
      return;
    }
    if (toastUI && typeof toastUI.show === "function") {
      toastUI.show({ message, tone });
    }
  };

  const confirmAction = async (message) => {
    const text = String(message || "").trim();
    if (!text) {
      return true;
    }
    if (confirmUI && typeof confirmUI.open === "function") {
      return Boolean(await confirmUI.open(text));
    }
    return window.confirm(text);
  };

  const setError = (message) => {
    if (!(refs.error instanceof HTMLElement)) {
      return;
    }
    refs.error.textContent = message || "";
    refs.error.hidden = !message;
  };

  const setRunNote = (message, tone = "ok") => {
    if (!(refs.note instanceof HTMLElement)) {
      return;
    }
    refs.note.textContent = message || "";
    refs.note.classList.toggle("is-working", tone === "working");
    refs.note.classList.toggle("is-error", tone === "error");
  };

  const refreshSelect = (select) => {
    if (select instanceof HTMLSelectElement) {
      select.dispatchEvent(new CustomEvent("select:refresh", { bubbles: true }));
    }
  };

  const setSelectValue = (select, value) => {
    if (!(select instanceof HTMLSelectElement)) {
      return false;
    }
    const next = String(value || "");
    if (!next) {
      return false;
    }
    const option = Array.from(select.options).find((candidate) => candidate.value === next && !candidate.disabled);
    if (!option) {
      return false;
    }
    select.value = next;
    refreshSelect(select);
    return true;
  };

  const runningRun = () => state.runs.find((run) => run.status === "running") || null;
  const activeLoadingRun = () => state.submission || runningRun();

  const snapshotToRun = (snapshot) => {
    if (!snapshot || typeof snapshot !== "object") {
      return null;
    }
    const count = clampNumber(Number(snapshot.count || 1), 1, state.limits.max_count || DEFAULT_LIMITS.max_count);
    const results = new Array(count).fill(null).map((_, index) => {
      const result = Array.isArray(snapshot.results) ? snapshot.results[index] : null;
      if (result?.ok && (result.image || result.remoteUrl)) {
        const src = String(result.image || result.remoteUrl || "");
        return {
          ok: true,
          image: src,
          remoteUrl: String(result.remoteUrl || (/^https?:\/\//i.test(src) ? src : "")),
          mime: String(result.mime || dataURLMIME(src) || "image/png"),
          text: String(result.text || ""),
          elapsedMs: Number(result.elapsedMs || 0),
        };
      }
      if (result && typeof result === "object" && !result.ok) {
        return {
          ok: false,
          status: String(result.status || "failed"),
          error: String(result.error || "生成失败"),
          text: String(result.text || ""),
          elapsedMs: Number(result.elapsedMs || 0),
        };
      }
      return null;
    });
    return {
      id: String(snapshot.id || createId("run")),
      createdAt: Number(snapshot.createdAt || Date.now()),
      prompt: String(snapshot.prompt || ""),
      ratio: String(snapshot.ratio || "1:1"),
      resolution: String(snapshot.resolution || "standard"),
      size: String(snapshot.size || ""),
      model: String(snapshot.model || ""),
      inputImageCount: Number(snapshot.inputImageCount || 0),
      count,
      status: normalizeRunStatus(snapshot.status),
      text: "",
      error: String(snapshot.error || ""),
      elapsedMs: Number(snapshot.elapsedMs || 0),
      results,
    };
  };

  const upsertRunFromSnapshot = (snapshot, { prepend = false } = {}) => {
    const next = snapshotToRun(snapshot);
    if (!next) {
      return null;
    }
    const index = state.runs.findIndex((run) => run.id === next.id);
    if (index >= 0) {
      state.runs[index] = { ...state.runs[index], ...next };
    } else if (prepend) {
      state.runs.unshift(next);
    } else {
      state.runs.push(next);
    }
    const run = index >= 0 ? state.runs[index] : next;
    if (run.status === "running") {
      startJobPoller(run.id);
    } else {
      stopJobPoller(run.id);
    }
    return run;
  };

  const hydrateServerTasks = (snapshots) => {
    for (const snapshot of Array.isArray(snapshots) ? snapshots : []) {
      const run = upsertRunFromSnapshot(snapshot);
      if (run && run.status !== "running") {
        saveRunHistory(run).catch(() => undefined);
      }
    }
    trimRuns();
    renderRuns();
    updateRenderTicker();
  };

  const setRunEmptyState = () => {
    if (!(refs.empty instanceof HTMLElement)) {
      return;
    }
    const loading = state.bootstrapping || state.tasksLoading;
    refs.empty.replaceChildren();
    refs.empty.classList.toggle("is-loading", loading);
    if (loading) {
      const spinner = document.createElement("span");
      spinner.className = "image-lab-empty-spinner";
      spinner.setAttribute("aria-hidden", "true");
      refs.empty.append(spinner);
    }
    const title = document.createElement("strong");
    title.textContent = loading ? (state.bootstrapping ? "正在加载后台任务" : "正在同步后台任务") : "暂无生成结果";
    refs.empty.append(title);
    if (loading) {
      const detail = document.createElement("small");
      detail.textContent = state.bootstrapping
        ? "正在检查未完成的生成任务，请不要重复提交。"
        : "正在读取最新进度，完成后会自动更新结果。";
      refs.empty.append(detail);
    }
  };

  const normalizeRunStatus = (status) => {
    switch (status) {
      case "running":
      case "submitting":
      case "completed":
      case "failed":
      case "cancelled":
        return status;
      default:
        return "failed";
    }
  };

  const updateControls = () => {
    const hasClient = refs.client.value.trim() !== "";
    const hasModel = refs.model.value.trim() !== "";
    const hasEnabledClient = state.clients.some((client) => client.enabled);
    const runningCount = state.runs.filter((run) => run.status === "running").length;
    const isSubmitting = Boolean(state.submission);
    if (refs.submit instanceof HTMLButtonElement) {
      refs.submit.disabled = isSubmitting || !hasClient || !hasModel;
      refs.submit.textContent = submitButtonText(runningCount);
      refs.submit.classList.toggle("is-submitting", isSubmitting);
      refs.submit.setAttribute("aria-busy", isSubmitting ? "true" : "false");
    }
    refs.client.disabled = isSubmitting || !hasEnabledClient;
    refs.model.disabled = isSubmitting || !hasModel;
    refreshSelect(refs.client);
    refreshSelect(refs.model);
    if (refs.prompt instanceof HTMLTextAreaElement) {
      refs.prompt.disabled = isSubmitting;
    }
    if (refs.upload instanceof HTMLElement) {
      refs.upload.classList.toggle("is-disabled", isSubmitting);
    }
    if (refs.clearResults instanceof HTMLButtonElement) {
      refs.clearResults.disabled = state.runs.length === 0;
    }
    if (refs.clearInputs instanceof HTMLButtonElement) {
      refs.clearInputs.disabled = isSubmitting || state.inputImages.length === 0;
    }
    if (refs.file instanceof HTMLInputElement) {
      refs.file.disabled = isSubmitting;
    }
    if (refs.count instanceof HTMLInputElement) {
      refs.count.disabled = isSubmitting;
    }
    if (refs.resolution instanceof HTMLSelectElement) {
      refs.resolution.disabled = isSubmitting;
    }
    if (refs.ratio instanceof HTMLSelectElement) {
      refs.ratio.disabled = isSubmitting;
    }
    refs.inputs?.querySelectorAll(".image-lab-input-remove").forEach((button) => {
      if (button instanceof HTMLButtonElement) {
        button.disabled = isSubmitting;
      }
    });
    syncPickerButtons();
  };

  const currentSize = () => {
    const ratio = refs.ratio instanceof HTMLSelectElement ? refs.ratio.value : "1:1";
    const resolution = refs.resolution instanceof HTMLSelectElement ? refs.resolution.value : "standard";
    if (ratio === "auto") {
      return "自动";
    }
    const tier = resolution === "auto" ? "standard" : resolution;
    return SIZE_MAP[tier]?.[ratio] || "自动";
  };

  const normalizeRatio = () => {
    if (!(refs.ratio instanceof HTMLSelectElement) || !(refs.resolution instanceof HTMLSelectElement)) {
      return;
    }
    if (refs.resolution.value !== "auto" && refs.ratio.value === "auto") {
      refs.ratio.value = "1:1";
    }
  };

  const syncPickerButtons = () => {
    if (refs.resolutionOptions instanceof HTMLElement && refs.resolution instanceof HTMLSelectElement) {
      refs.resolutionOptions.querySelectorAll("[data-resolution]").forEach((button) => {
        const active = button instanceof HTMLElement && button.dataset.resolution === refs.resolution.value;
        if (button instanceof HTMLButtonElement) {
          button.disabled = Boolean(state.submission);
        }
        button.classList.toggle("is-active", active);
        button.setAttribute("aria-checked", active ? "true" : "false");
      });
    }
    if (refs.ratioOptions instanceof HTMLElement && refs.ratio instanceof HTMLSelectElement) {
      refs.ratioOptions.querySelectorAll("[data-ratio]").forEach((button) => {
        if (!(button instanceof HTMLButtonElement)) {
          return;
        }
        const disabled = refs.resolution instanceof HTMLSelectElement && refs.resolution.value !== "auto" && button.dataset.ratio === "auto";
        if (disabled && refs.ratio.value === "auto") {
          refs.ratio.value = "1:1";
        }
        button.disabled = Boolean(state.submission) || disabled;
        const active = button.dataset.ratio === refs.ratio.value;
        button.classList.toggle("is-active", active);
        button.setAttribute("aria-checked", active ? "true" : "false");
      });
    }
  };

  const updateSize = () => {
    normalizeRatio();
    if (refs.size instanceof HTMLElement) {
      refs.size.textContent = currentSize();
    }
    syncPickerButtons();
  };

  const renderClients = (defaultClientID = "", preferredClientID = "", preferredModel = "") => {
    refs.client.replaceChildren();
    const enabledClients = state.clients.filter((client) => client.enabled);
    if (!enabledClients.length) {
      const option = document.createElement("option");
      option.value = "";
      option.textContent = "没有可用 API 密钥";
      refs.client.append(option);
      renderModels("");
      updateControls();
      return;
    }
    for (const client of state.clients) {
      const option = document.createElement("option");
      option.value = client.id;
      option.textContent = client.name || client.id;
      option.disabled = !client.enabled;
      refs.client.append(option);
    }
    if (!setSelectValue(refs.client, preferredClientID)) {
      if (!setSelectValue(refs.client, defaultClientID)) {
        refs.client.value = enabledClients[0].id;
        refreshSelect(refs.client);
      }
    }
    renderModels(preferredModel);
  };

  const renderModels = (preferredModel = "") => {
    const models = state.modelsByClient[refs.client.value] || [];
    refs.model.replaceChildren();
    if (!models.length) {
      const option = document.createElement("option");
      option.value = "";
      option.textContent = "没有可用模型";
      refs.model.append(option);
      refreshSelect(refs.model);
      updateControls();
      return;
    }
    for (const model of models) {
      const option = document.createElement("option");
      option.value = model.id;
      option.textContent = model.id;
      refs.model.append(option);
    }
    if (!setSelectValue(refs.model, preferredModel)) {
      refs.model.value = models[0].id;
      refreshSelect(refs.model);
    }
    updateControls();
  };

  const loadBootstrap = async () => {
    try {
      const response = await fetch("/images/api/bootstrap", {
        credentials: "same-origin",
        headers: { Accept: "application/json" },
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) {
        throw new Error(payload.message || payload.error?.message || response.statusText);
      }
      state.clients = Array.isArray(payload.clients) ? payload.clients : [];
      state.modelsByClient = payload.models_by_client || {};
      state.limits = { ...DEFAULT_LIMITS, ...(payload.limits || {}) };
      renderClients(
        payload.default_client_id || "",
        "",
        payload.default_model || "",
      );
      hydrateServerTasks(payload.active_tasks);
    } catch (error) {
      const message = error instanceof Error ? error.message : "加载配置失败";
      setError(message);
      showToast(message, "error");
    } finally {
      state.bootstrapping = false;
      renderRuns();
      updateControls();
    }
  };

  const loadServerTasks = async ({ showLoading = false } = {}) => {
    if (showLoading) {
      state.tasksLoading = true;
      renderRuns();
    }
    try {
      const response = await fetch("/images/api/jobs", {
        credentials: "same-origin",
        headers: { Accept: "application/json" },
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) {
        throw new Error(payload.message || payload.error?.message || response.statusText);
      }
      hydrateServerTasks(payload.active_tasks);
    } finally {
      if (showLoading) {
        state.tasksLoading = false;
        renderRuns();
      }
    }
  };

  const scheduleDraftSave = () => {};

  const normalizeInputImages = (images) => {
    const out = [];
    let total = 0;
    for (const image of Array.isArray(images) ? images : []) {
      const dataUrl = String(image?.dataUrl || image?.data_url || "");
      if (!dataUrl.startsWith("data:")) {
        continue;
      }
      const type = String(image?.type || dataURLMIME(dataUrl) || "image/png");
      const size = Number(image?.size || estimateDataURLBytes(dataUrl));
      if (!isSupportedInputImageType(type) || !Number.isFinite(size) || size <= 0) {
        continue;
      }
      if (out.length >= state.limits.max_input_images) {
        break;
      }
      if (size > state.limits.max_image_bytes || total + size > state.limits.max_total_bytes) {
        continue;
      }
      total += size;
      out.push({
        id: String(image?.id || createId("input")),
        name: String(image?.name || `reference-${out.length + 1}${extensionFromMIME(type)}`),
        type,
        dataUrl,
        size,
      });
    }
    return out;
  };

  const buildPayload = () => {
    const prompt = refs.prompt instanceof HTMLTextAreaElement ? refs.prompt.value.trim() : "";
    if (!prompt) {
      throw new Error("请先填写提示词");
    }
    if (!refs.client.value) {
      throw new Error("请选择 API 密钥");
    }
    if (!refs.model.value) {
      throw new Error("请选择模型");
    }
    updateSize();
    const count = readCount();
    if (refs.count instanceof HTMLInputElement) {
      refs.count.value = String(count);
    }
    scheduleDraftSave();
    return {
      client_id: refs.client.value,
      prompt,
      ratio: refs.ratio instanceof HTMLSelectElement ? refs.ratio.value : "1:1",
      resolution: refs.resolution instanceof HTMLSelectElement ? refs.resolution.value : "standard",
      model: refs.model.value,
      count,
      input_images: state.inputImages.map((image) => ({
        id: image.id,
        name: image.name,
        type: image.type,
        data_url: image.dataUrl,
        size: image.size,
      })),
    };
  };

  const readCount = () => {
    const value = refs.count instanceof HTMLInputElement ? refs.count.valueAsNumber : 1;
    return clampNumber(value, 1, state.limits.max_count || DEFAULT_LIMITS.max_count);
  };

  const setCountValue = (value) => {
    if (!(refs.count instanceof HTMLInputElement)) {
      return;
    }
    refs.count.value = String(clampNumber(Number(value || 1), 1, state.limits.max_count || DEFAULT_LIMITS.max_count));
  };

  const submissionProgressPercent = (submission) => {
    const loaded = Number(submission?.uploadLoaded || 0);
    const total = Number(submission?.uploadTotal || 0);
    if (!Number.isFinite(loaded) || !Number.isFinite(total) || total <= 0) {
      return null;
    }
    return Math.max(0, Math.min(100, Math.round((loaded / total) * 100)));
  };

  const submissionStageText = (submission) => {
    const percent = submissionProgressPercent(submission);
    switch (submission?.stage) {
      case "preparing":
        return submission?.inputImageCount > 0 ? "正在打包参考图" : "正在准备请求";
      case "uploading":
        return percent === null ? "正在上传请求" : `正在上传 ${percent}%`;
      case "creating":
        return "正在创建后台任务";
      default:
        return "正在提交";
    }
  };

  const submissionDetailText = (submission) => {
    const stage = submission?.stage || "preparing";
    const loaded = Number(submission?.uploadLoaded || 0);
    const total = Number(submission?.uploadTotal || 0);
    if (stage === "uploading" && total > 0) {
      return `已发送 ${formatBytes(loaded)} / ${formatBytes(total)}`;
    }
    if (stage === "creating") {
      return "服务器正在创建任务，请不要重复提交";
    }
    const inputBytes = Number(submission?.inputBytes || 0);
    if (inputBytes > 0) {
      return `${submission.inputImageCount || 0} 张参考图 · ${formatBytes(inputBytes)}`;
    }
    return "请求会在后台运行";
  };

  const submitButtonText = (runningCount) => {
    if (state.submission) {
      return submissionStageText(state.submission);
    }
    return runningCount > 0 ? `继续提交（${runningCount} 个生成中）` : "开始生成";
  };

  const submissionRunFromForm = () => {
    const count = readCount();
    return {
      id: createId("submit"),
      createdAt: Date.now(),
      prompt: refs.prompt instanceof HTMLTextAreaElement ? refs.prompt.value.trim() : "",
      ratio: refs.ratio instanceof HTMLSelectElement ? refs.ratio.value : "1:1",
      resolution: refs.resolution instanceof HTMLSelectElement ? refs.resolution.value : "standard",
      size: currentSize(),
      model: refs.model.value.trim(),
      inputImageCount: state.inputImages.length,
      inputBytes: state.inputImages.reduce((sum, image) => sum + Number(image.size || 0), 0),
      count,
      status: "submitting",
      stage: "preparing",
      uploadLoaded: 0,
      uploadTotal: 0,
      error: "",
      results: new Array(count).fill(null),
    };
  };

  const requestSubmissionRender = () => {
    if (submissionRenderFrame) {
      return;
    }
    submissionRenderFrame = window.requestAnimationFrame(() => {
      submissionRenderFrame = 0;
      renderRuns();
      updateRenderTicker();
    });
  };

  const updateSubmission = (patch) => {
    if (!state.submission) {
      return;
    }
    state.submission = { ...state.submission, ...patch };
    setRunNote(submissionStageText(state.submission), "working");
    requestSubmissionRender();
  };

  const submit = async () => {
    if (state.submission) {
      showToast("已有图片任务正在提交，请等待提交完成", "info");
      return;
    }
    setError("");
    state.submission = submissionRunFromForm();
    setRunNote("正在提交后台任务，请不要重复点击", "working");
    renderRuns();
    updateRenderTicker();
    await nextFrame();
    try {
      updateSubmission({ stage: "preparing" });
      await nextFrame();
      const payload = buildPayload();
      const body = JSON.stringify(payload);
      updateSubmission({ stage: "uploading", uploadLoaded: 0, uploadTotal: body.length });
      await nextFrame();
      const snapshot = await createImageLabJob(root.dataset.csrfToken || "", body, updateSubmission);
      state.submission = null;
      const run = upsertRunFromSnapshot(snapshot, { prepend: true });
      trimRuns();
      renderRuns();
      updateRenderTicker();
      if (run) {
        setRunNote(Number(run.count || 1) > 1 ? `已提交后台任务，生成 ${run.count} 张图片。` : "已提交后台任务。", "ok");
      } else {
        setRunNote("已提交后台任务。", "ok");
      }
      showToast("后台任务已开始", "ok");
    } catch (error) {
      state.submission = null;
      renderRuns();
      updateRenderTicker();
      const message = error instanceof Error ? error.message : "提交失败";
      setRunNote(`提交失败：${message}`, "error");
      throw error;
    }
  };

  const fetchJobSnapshot = async (id) => {
    const response = await fetch(`/images/api/jobs/${encodeURIComponent(id)}`, {
      credentials: "same-origin",
      headers: { Accept: "application/json" },
    });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) {
      const error = new Error(payload.message || payload.error?.message || response.statusText);
      error.status = response.status;
      throw error;
    }
    return payload;
  };

  const pollJobStatus = async (id) => {
    const run = state.runs.find((candidate) => candidate.id === id);
    if (!run) {
      stopJobPoller(id);
      return;
    }
    try {
      const snapshot = await fetchJobSnapshot(id);
      const nextRun = upsertRunFromSnapshot(snapshot);
      renderRuns();
      if (nextRun && nextRun.status !== "running") {
        stopJobPoller(id);
        const completion = runCompletion(nextRun);
        if (refs.note instanceof HTMLElement) {
          refs.note.textContent = completion.message;
        }
        if (completion.okCount > 0) {
          await saveRunHistory(nextRun).catch(() => undefined);
        }
        showToast(completion.message, completion.okCount > 0 ? "ok" : (nextRun.status === "cancelled" ? "info" : "error"));
      }
    } catch (error) {
      if (error && typeof error === "object" && Number(error.status || 0) === 404) {
        stopJobPoller(id);
      }
    }
  };

  const startJobPoller = (id) => {
    id = String(id || "").trim();
    if (!id || jobPollers.has(id)) {
      return;
    }
    jobPollers.set(id, window.setInterval(() => {
      pollJobStatus(id).catch(() => undefined);
    }, 2000));
    pollJobStatus(id).catch(() => undefined);
  };

  const stopJobPoller = (id) => {
    const timer = jobPollers.get(String(id || ""));
    if (timer) {
      window.clearInterval(timer);
      jobPollers.delete(String(id || ""));
    }
  };

  document.addEventListener("ag:image-job-updated", (event) => {
    const snapshot = event.detail || {};
    if (!snapshot || !snapshot.id) {
      return;
    }
    if (snapshot.dismissed === true) {
      state.runs = state.runs.filter((run) => run.id !== snapshot.id);
      stopJobPoller(snapshot.id);
      renderRuns();
      updateRenderTicker();
      return;
    }
    const existing = state.runs.find((run) => run.id === snapshot.id);
    const nextRun = upsertRunFromSnapshot(snapshot, { prepend: !existing });
    if (!nextRun) {
      return;
    }
    trimRuns();
    renderRuns();
    updateRenderTicker();
    if (nextRun.status !== "running") {
      const completion = runCompletion(nextRun);
      if (refs.note instanceof HTMLElement) {
        refs.note.textContent = completion.message;
      }
      if (completion.okCount > 0) {
        saveRunHistory(nextRun).catch(() => undefined);
      }
    }
  });

  const saveRunHistory = async (run) => {
    const okResults = run.results.filter((result) => result?.ok && (result.image || result.remoteUrl));
    if (!okResults.length) {
      return;
    }
    const item = {
      id: run.id,
      createdAt: run.createdAt,
      prompt: run.prompt,
      ratio: run.ratio,
      resolution: run.resolution,
      size: run.size,
      model: run.model,
      images: okResults.map((result) => result.image || result.remoteUrl),
      remoteUrls: okResults.map((result) => result.remoteUrl || (/^https?:\/\//i.test(result.image || "") ? result.image : "")),
      mimes: okResults.map((result) => result.mime || dataURLMIME(result.image || result.remoteUrl) || "image/png"),
      text: okResults.map((result) => result.text || "").filter(Boolean).join("\n"),
      failedCount: Math.max(0, Number(run.count || 0) - okResults.length),
      elapsedMs: run.elapsedMs,
    };
    state.history = [item, ...state.history.filter((existing) => existing.id !== item.id)].slice(0, 30);
    renderHistory();
  };

  const firstRunError = (run) => {
    for (const result of Array.isArray(run?.results) ? run.results : []) {
      const message = typeof result?.error === "string" ? result.error.trim() : "";
      if (message) {
        return message;
      }
    }
    return "";
  };

  const runCompletion = (run) => {
    const results = Array.isArray(run?.results) ? run.results : [];
    const okCount = results.filter((result) => result?.ok && result.image).length;
    const cancelledCount = results.filter((result) => result?.status === "cancelled").length;
    const failedCount = Math.max(0, Number(run?.count || 0) - okCount - cancelledCount);
    if (run?.status === "cancelled") {
      return {
        okCount,
        failedCount,
        cancelledCount,
        message: okCount > 0 ? `任务已停止，已生成 ${okCount} 张图片` : "任务已停止",
      };
    }
    if (okCount > 0) {
      const interrupted = failedCount + cancelledCount;
      return {
        okCount,
        failedCount,
        cancelledCount,
        message: interrupted > 0
          ? `生成结束：成功 ${okCount}，失败 ${failedCount}，停止 ${cancelledCount}`
          : `任务成功生成 ${okCount} 张图片`,
      };
    }
    return {
      okCount,
      failedCount,
      cancelledCount,
      message: run?.error || firstRunError(run) || "生成失败",
    };
  };

  const trimRuns = () => {
    let finishedKept = 0;
    state.runs = state.runs.filter((run) => {
      if (run.status === "running") {
        return true;
      }
      finishedKept += 1;
      return finishedKept <= 12;
    });
  };

  const renderRuns = () => {
    if (!(refs.runs instanceof HTMLElement)) {
      return;
    }
    refs.runs.replaceChildren();
    const runs = state.submission ? [state.submission, ...state.runs] : state.runs;
    if (refs.empty instanceof HTMLElement) {
      setRunEmptyState();
      refs.empty.hidden = runs.length > 0;
    }
    for (const run of runs) {
      refs.runs.append(renderRun(run));
    }
    updateControls();
  };

  const renderRun = (run) => {
    const article = document.createElement("article");
    article.className = `image-lab-task is-${run.status}`;

    const head = document.createElement("div");
    head.className = "image-lab-task-head";
    const title = document.createElement("div");
    const heading = document.createElement("h3");
    heading.textContent = run.inputImageCount > 0 ? "图生图任务" : "文生图任务";
    const meta = document.createElement("span");
    meta.textContent = `${run.model} · ${run.size || "自动"} · ${run.count || 1} 张`;
    title.append(heading, meta);
    const pill = document.createElement("span");
    pill.className = statusPillClass(run.status);
    pill.textContent = runStatusText(run.status);
    head.append(title, pill);
    article.append(head);

    const prompt = document.createElement("p");
    prompt.className = "image-lab-task-prompt";
    prompt.textContent = run.prompt;
    article.append(prompt);

    const grid = document.createElement("div");
    grid.className = "image-lab-result-grid";
    for (let index = 0; index < Number(run.count || 1); index += 1) {
      grid.append(renderResult(run, index));
    }
    article.append(grid);

    const actions = document.createElement("div");
    actions.className = "image-lab-task-actions";
    if (run.status === "submitting") {
      const note = document.createElement("span");
      note.className = "image-lab-task-note";
      note.textContent = submissionDetailText(run);
      actions.append(note);
    } else if (run.status === "running") {
      actions.append(actionButton("停止", async () => {
        if (!(await confirmAction(root.dataset.confirmCancelRun || "确认停止当前生成？"))) {
          return;
        }
        const snapshot = await cancelServerRun(root.dataset.csrfToken || "", run.id);
        upsertRunFromSnapshot(snapshot);
        renderRuns();
      }));
    } else {
      actions.append(actionButton("复制提示词", () => copyText(run.prompt, showToast)));
      actions.append(actionButton("移除", async () => {
        if (!(await confirmAction(root.dataset.confirmRemoveRun || "确认移除这条生成结果？"))) {
          return;
        }
        await removeRun(run.id);
      }));
    }
    if (run.error && run.status !== "running") {
      const error = document.createElement("span");
      error.className = "image-lab-task-error";
      error.textContent = run.error;
      actions.append(error);
    }
    article.append(actions);
    return article;
  };

  const previewItemsForRun = (run) => (Array.isArray(run?.results) ? run.results : [])
    .map((result, index) => {
      if (!result?.ok || !result.image) {
        return null;
      }
      return {
        src: result.image,
        resultIndex: index,
      };
    })
    .filter(Boolean);

  const renderResult = (run, index) => {
    const result = Array.isArray(run.results) ? run.results[index] : null;
    const card = document.createElement("div");
    card.className = "image-lab-result";
    if (!result) {
      if (run.status !== "running") {
        card.classList.add("is-error");
        const strong = document.createElement("strong");
        strong.textContent = `第 ${index + 1} 张未完成`;
        const message = document.createElement("span");
        message.textContent = run.error || "任务已中断";
        card.append(strong, message);
        return card;
      }
      card.classList.add("is-loading");
      if (run.status === "submitting") {
        card.classList.add("is-submitting");
      }
      card.setAttribute("aria-busy", "true");
      const skeleton = document.createElement("div");
      skeleton.className = "image-lab-skeleton";
      const waitTime = document.createElement("span");
      waitTime.className = "image-lab-loading-time";
      waitTime.dataset.createdAt = String(run.createdAt);
      waitTime.setAttribute("data-image-lab-loading-time", "");
      waitTime.textContent = formatDuration(Date.now() - run.createdAt);
      const info = document.createElement("div");
      info.className = "image-lab-loading-info";
      const status = document.createElement("span");
      status.className = "image-lab-loading-status";
      const label = document.createElement("span");
      label.className = "image-lab-loading-label";
      label.textContent = run.status === "submitting" ? submissionStageText(run) : "正在生成";
      const meta = document.createElement("span");
      meta.className = "image-lab-loading-meta";
      meta.textContent = run.status === "submitting"
        ? submissionDetailText(run)
        : (run.size ? `第 ${index + 1} 张 · ${run.size}` : `第 ${index + 1} 张`);
      status.append(label);
      info.append(status, meta);
      skeleton.append(waitTime, info);
      if (run.status === "submitting") {
        const percent = submissionProgressPercent(run);
        const progress = document.createElement("span");
        progress.className = "image-lab-submit-progress";
        progress.setAttribute("aria-hidden", "true");
        const bar = document.createElement("span");
        if (percent !== null) {
          bar.style.width = `${percent}%`;
        }
        progress.append(bar);
        skeleton.append(progress);
      }
      card.append(skeleton);
      return card;
    }
    if (!result.ok) {
      card.classList.add("is-error");
      const strong = document.createElement("strong");
      strong.textContent = `第 ${index + 1} 张失败`;
      const message = document.createElement("span");
      message.textContent = result.error || "生成失败";
      card.append(strong, message);
      return card;
    }
    const src = result.image;
    const previewItems = previewItemsForRun(run);
    const previewIndex = Math.max(0, previewItems.findIndex((item) => item.resultIndex === index));
    const gallery = previewItems.map((item) => item.src);
    const imageButton = document.createElement("button");
    imageButton.type = "button";
    imageButton.className = "image-lab-image-button";
    imageButton.setAttribute("aria-label", `预览生成结果 ${index + 1}`);
    imageButton.addEventListener("click", () => openPreview(gallery, previewIndex, run));
    const img = document.createElement("img");
    img.src = imageDisplayURL(src);
    img.alt = `生成结果 ${index + 1}`;
    img.loading = "lazy";
    imageButton.append(img);
    const elapsedMs = Number(result.elapsedMs || 0)
      || Number(run.elapsedMs || 0)
      || Math.max(0, Date.now() - run.createdAt);
    const elapsed = document.createElement("span");
    elapsed.className = "image-lab-result-time";
    elapsed.textContent = formatDuration(elapsedMs);
    const actions = document.createElement("div");
    actions.className = "image-lab-result-actions";
    actions.append(
      actionButton("预览", () => openPreview(gallery, previewIndex, run)),
      actionButton("下载", () => downloadImage(src, imageFileName(run.prompt, index), showToast)),
      actionButton("复制", () => copyImageToClipboard(src, showToast), { title: "复制图片" }),
      actionButton("参考", () => useResultAsReference(result, run, index)),
      actionButton("提示词", () => copyText(run.prompt, showToast), { title: "复制提示词" }),
    );
    card.append(imageButton, elapsed, actions);
    return card;
  };

  const useResultAsReference = async (result, run, index) => {
    try {
      const src = result.image;
      const input = await imageSourceToInput(src, {
        id: createId("input"),
        name: `result-${index + 1}${extensionFromMIME(result.mime || "image/png")}`,
        type: result.mime || "image/png",
      });
      state.inputImages = normalizeInputImages([...state.inputImages, input]);
      if (refs.prompt instanceof HTMLTextAreaElement) {
        refs.prompt.value = run.prompt;
      }
      renderInputs();
      scheduleDraftSave();
      showToast("已加入参考图", "ok");
    } catch (error) {
      showToast(error instanceof Error ? error.message : "添加参考图失败", "error");
    }
  };

  const removeRun = async (id) => {
    stopJobPoller(id);
    await deleteServerRun(root.dataset.csrfToken || "", id).catch(() => undefined);
    state.runs = state.runs.filter((run) => run.id !== id);
    renderRuns();
  };

  const runStatusText = (status) => {
    switch (status) {
      case "submitting":
        return "提交中";
      case "running":
        return "生成中";
      case "completed":
        return "已完成";
      case "failed":
        return "失败";
      case "cancelled":
        return "已停止";
      default:
        return "生成中";
    }
  };

  const statusPillClass = (status) => {
    switch (status) {
      case "completed":
        return "pill tone-good";
      case "failed":
        return "pill tone-bad";
      case "cancelled":
        return "pill tone-muted";
      default:
        return "pill";
    }
  };

  const refreshLoadingTimes = () => {
    if (!(refs.runs instanceof HTMLElement)) {
      return;
    }
    refs.runs.querySelectorAll("[data-image-lab-loading-time]").forEach((element) => {
      if (!(element instanceof HTMLElement)) {
        return;
      }
      const createdAt = Number(element.dataset.createdAt || 0);
      if (createdAt > 0) {
        element.textContent = formatDuration(Date.now() - createdAt);
      }
    });
  };

  const updateRenderTicker = () => {
    if (activeLoadingRun()) {
      if (!renderTicker) {
        renderTicker = window.setInterval(() => {
          if (activeLoadingRun()) {
            refreshLoadingTimes();
          } else {
            window.clearInterval(renderTicker);
            renderTicker = 0;
          }
        }, 1000);
      }
      return;
    }
    if (renderTicker) {
      window.clearInterval(renderTicker);
      renderTicker = 0;
    }
  };

  const renderInputs = () => {
    if (!(refs.inputs instanceof HTMLElement)) {
      return;
    }
    refs.inputs.replaceChildren();
    if (!state.inputImages.length) {
      const empty = document.createElement("div");
      empty.className = "image-lab-upload-empty";
      empty.textContent = "拖入 PNG / JPG / WebP，或点击选择图片。";
      refs.inputs.append(empty);
    } else {
      state.inputImages.forEach((image, index) => {
        refs.inputs.append(renderInputImage(image, index));
      });
    }
    updateControls();
  };

  const renderInputImage = (image, index) => {
    const figure = document.createElement("figure");
    figure.className = "image-lab-input";
    const media = document.createElement("div");
    media.className = "image-lab-input-media";
    const preview = document.createElement("button");
    preview.type = "button";
    preview.className = "image-lab-image-button";
    preview.setAttribute("aria-label", `预览${image.name || `参考图 ${index + 1}`}`);
    const img = document.createElement("img");
    img.alt = image.name || `参考图 ${index + 1}`;
    img.src = image.dataUrl;
    preview.append(img);
    preview.addEventListener("click", () => {
      const images = state.inputImages.map((item) => item.dataUrl).filter(Boolean);
      openPreview(images, index, { prompt: image.name || `参考图 ${index + 1}` });
    });
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "icon-button image-lab-input-remove";
    remove.title = "移除参考图";
    remove.setAttribute("aria-label", "移除参考图");
    remove.textContent = "×";
    remove.disabled = Boolean(state.submission);
    remove.addEventListener("click", async () => {
      if (state.submission) {
        showToast("任务提交中，完成后再调整参考图", "info");
        return;
      }
      if (!(await confirmAction(root.dataset.confirmRemoveInput || "确认移除这张参考图？"))) {
        return;
      }
      state.inputImages.splice(index, 1);
      renderInputs();
      scheduleDraftSave();
    });
    media.append(preview, remove);
    const caption = document.createElement("figcaption");
    const name = document.createElement("span");
    name.textContent = image.name || `参考图 ${index + 1}`;
    const size = document.createElement("small");
    size.textContent = `${image.type || "image"} · ${formatBytes(image.size)}`;
    caption.append(name, size);
    figure.append(media, caption);
    return figure;
  };

  const handleFiles = async (fileList) => {
    if (state.submission) {
      showToast("任务提交中，完成后再调整参考图", "info");
      return;
    }
    const files = Array.from(fileList || []);
    if (!files.length) {
      return;
    }
    let next = [...state.inputImages];
    let total = next.reduce((sum, image) => sum + Number(image.size || 0), 0);
    for (const file of files) {
      if (next.length >= state.limits.max_input_images) {
        showToast(`参考图不能超过 ${state.limits.max_input_images} 张`, "error");
        break;
      }
      if (!isSupportedInputImageType(file.type)) {
        showToast(`${file.name} 不是支持的图片格式`, "error");
        continue;
      }
      if (file.size > state.limits.max_image_bytes) {
        showToast(`${file.name} 超过 12MB`, "error");
        continue;
      }
      if (total + file.size > state.limits.max_total_bytes) {
        showToast("参考图总大小不能超过 50MB", "error");
        break;
      }
      const image = await fileToInputImage(file);
      next.push(image);
      total += file.size;
    }
    state.inputImages = next;
    renderInputs();
    scheduleDraftSave();
  };

  const loadHistory = async () => {
    renderHistory();
  };

  const renderHistory = () => {
    if (!(refs.history instanceof HTMLElement)) {
      return;
    }
    refs.history.replaceChildren();
    refs.history.classList.toggle("image-lab-history-collapsed", state.historyCollapsed);
    if (refs.toggleHistory instanceof HTMLButtonElement) {
      refs.toggleHistory.textContent = state.historyCollapsed ? "展开" : "收起";
    }
    if (state.historyCollapsed) {
      return;
    }
    if (!state.history.length) {
      const empty = document.createElement("div");
      empty.className = "image-lab-history-empty";
      empty.textContent = "暂无历史记录。";
      refs.history.append(empty);
      return;
    }
    for (const item of state.history) {
      refs.history.append(renderHistoryItem(item));
    }
  };

  const renderHistoryItem = (item) => {
    const article = document.createElement("article");
    article.className = "image-lab-history-item";
    const images = Array.isArray(item.images) ? item.images : [];
    const thumbs = document.createElement("div");
    thumbs.className = "image-lab-history-thumbs";
    images.slice(0, 4).forEach((src, index) => {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "image-lab-history-thumb";
      const img = document.createElement("img");
      img.alt = item.prompt || "历史图片";
      img.loading = "lazy";
      img.src = imageDisplayURL(src);
      button.append(img);
      button.addEventListener("click", () => openPreview(images, index, item));
      thumbs.append(button);
    });
    if (images.length > 4) {
      const more = document.createElement("div");
      more.className = "image-lab-history-more";
      more.textContent = `+${images.length - 4}`;
      thumbs.append(more);
    }

    const body = document.createElement("div");
    body.className = "image-lab-history-body";
    const title = document.createElement("strong");
    title.textContent = item.prompt || "未命名生成";
    const meta = document.createElement("span");
    meta.textContent = `${item.model || "model"} · ${item.size || "自动"} · ${formatDate(item.createdAt)}`;
    const actions = document.createElement("div");
    actions.className = "image-lab-history-actions";
    actions.append(
      actionButton("复用", () => {
        if (refs.prompt instanceof HTMLTextAreaElement) {
          refs.prompt.value = item.prompt || "";
          refs.prompt.focus();
        }
        setSelectValue(refs.resolution, item.resolution || "standard");
        setSelectValue(refs.ratio, item.ratio || "1:1");
        updateSize();
        scheduleDraftSave();
      }),
      actionButton("删除", async () => {
        if (!(await confirmAction(root.dataset.confirmDeleteHistoryItem || "确认删除这条历史记录？"))) {
          return;
        }
        state.history = state.history.filter((existing) => existing.id !== item.id);
        renderHistory();
      }),
    );
    body.append(title, meta, actions);
    article.append(thumbs, body);
    return article;
  };

  const clearResults = async () => {
    if (!state.runs.length) {
      return;
    }
    if (!(await confirmAction(root.dataset.confirmClearResults || "确认清理所有生成结果？"))) {
      return;
    }
    for (const run of state.runs) {
      if (run.status === "running") {
        await cancelServerRun(root.dataset.csrfToken || "", run.id).catch(() => undefined);
      }
      await deleteServerRun(root.dataset.csrfToken || "", run.id).catch(() => undefined);
      stopJobPoller(run.id);
    }
    state.runs = [];
    renderRuns();
  };

  const openPreview = (images, startIndex, meta = {}) => {
    const list = (Array.isArray(images) ? images : []).filter(Boolean);
    if (!list.length) {
      return;
    }
    closePreview();
    let currentIndex = Math.max(0, Math.min(list.length - 1, Number(startIndex || 0)));
    let scale = 1;
    let translateX = 0;
    let translateY = 0;
    let dragging = false;
    let dragStartX = 0;
    let dragStartY = 0;
    let dragBaseX = 0;
    let dragBaseY = 0;
    const previousOverflow = document.body.style.overflow;
    const mask = document.createElement("div");
    mask.className = "image-lab-preview-mask";
    const dialog = document.createElement("div");
    dialog.className = "image-lab-preview-dialog";
    const top = document.createElement("div");
    top.className = "image-lab-preview-top";
    const title = document.createElement("div");
    title.className = "image-lab-preview-title";
    const strong = document.createElement("strong");
    strong.textContent = meta.prompt || "生成图片";
    const info = document.createElement("div");
    info.className = "image-lab-preview-info";
    title.append(strong, info);
    const actions = document.createElement("div");
    actions.className = "image-lab-preview-actions";
    const image = document.createElement("img");
    image.draggable = false;
    const stage = document.createElement("div");
    stage.className = "image-lab-preview-stage";
    stage.append(image);

    const panBounds = () => {
      const stageRect = stage.getBoundingClientRect();
      const width = image.clientWidth * scale;
      const height = image.clientHeight * scale;
      return {
        x: Math.max(0, (width - stageRect.width) / 2),
        y: Math.max(0, (height - stageRect.height) / 2),
      };
    };

    const updateTransform = () => {
      if (scale <= 1) {
        translateX = 0;
        translateY = 0;
      } else {
        const bounds = panBounds();
        translateX = Math.max(-bounds.x, Math.min(bounds.x, translateX));
        translateY = Math.max(-bounds.y, Math.min(bounds.y, translateY));
      }
      image.style.transform = `translate3d(${translateX}px, ${translateY}px, 0) scale(${scale})`;
      stage.classList.toggle("can-pan", scale > 1);
      zoomReset.textContent = `${Math.round(scale * 100)}%`;
    };

    const resetView = () => {
      scale = 1;
      translateX = 0;
      translateY = 0;
      updateTransform();
    };

    const setScale = (nextScale, origin) => {
      const oldScale = scale;
      scale = Math.max(0.25, Math.min(8, nextScale));
      if (origin && oldScale > 0 && scale > 1) {
        translateX = origin.x - ((origin.x - translateX) * scale / oldScale);
        translateY = origin.y - ((origin.y - translateY) * scale / oldScale);
      }
      updateTransform();
    };

    const zoomBy = (factor, event) => {
      const rect = stage.getBoundingClientRect();
      const origin = event
        ? { x: event.clientX - rect.left - rect.width / 2, y: event.clientY - rect.top - rect.height / 2 }
        : { x: 0, y: 0 };
      setScale(scale * factor, origin);
    };

    const showImage = (index) => {
      currentIndex = (index + list.length) % list.length;
      const src = list[currentIndex];
      image.src = imageDisplayURL(src);
      image.alt = meta.prompt || "生成图片";
      info.textContent = `${currentIndex + 1}/${list.length} · ${formatImageSize(src)}`;
      resetView();
    };

    const zoomOut = actionButton("缩小", () => zoomBy(0.85), { title: "缩小" });
    const zoomReset = actionButton("100%", () => resetView(), { title: "恢复原始缩放" });
    const zoomIn = actionButton("放大", () => zoomBy(1.18), { title: "放大" });
    actions.append(
      zoomOut,
      zoomReset,
      zoomIn,
      actionButton("下载", () => downloadImage(list[currentIndex], imageFileName(meta.prompt || "", currentIndex), showToast)),
      actionButton("复制", () => copyImageToClipboard(list[currentIndex], showToast)),
    );
    if (list.length > 1) {
      actions.append(
        actionButton("上一张", () => showImage(currentIndex - 1)),
        actionButton("下一张", () => showImage(currentIndex + 1)),
      );
    }
    const close = document.createElement("button");
    close.type = "button";
    close.className = "image-lab-preview-close";
    close.textContent = "关闭";
    close.addEventListener("click", closePreview);
    actions.append(close);
    top.append(title, actions);
    dialog.append(top, stage);
    mask.append(dialog);
    mask.addEventListener("mousedown", (event) => {
      if (event.target === mask || event.target === stage) {
        closePreview();
      }
    });
    stage.addEventListener("wheel", (event) => {
      event.preventDefault();
      zoomBy(event.deltaY < 0 ? 1.12 : 0.88, event);
    }, { passive: false });
    const onMouseMove = (event) => {
      if (!dragging) {
        return;
      }
      translateX = dragBaseX + event.clientX - dragStartX;
      translateY = dragBaseY + event.clientY - dragStartY;
      updateTransform();
    };
    const onMouseUp = () => {
      dragging = false;
      stage.classList.remove("is-dragging");
    };
    stage.addEventListener("mousedown", (event) => {
      if (event.button !== 0 || scale <= 1) {
        return;
      }
      event.preventDefault();
      dragging = true;
      dragStartX = event.clientX;
      dragStartY = event.clientY;
      dragBaseX = translateX;
      dragBaseY = translateY;
      stage.classList.add("is-dragging");
    });
    window.addEventListener("mousemove", onMouseMove);
    window.addEventListener("mouseup", onMouseUp);
    const onKeyDown = (event) => {
      if (event.key === "Escape") {
        closePreview();
      } else if (event.key === "ArrowLeft") {
        showImage(currentIndex - 1);
      } else if (event.key === "ArrowRight") {
        showImage(currentIndex + 1);
      } else if (event.key === "+" || event.key === "=") {
        zoomBy(1.18);
      } else if (event.key === "-") {
        zoomBy(0.85);
      } else if (event.key === "0") {
        resetView();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    const onResize = () => updateTransform();
    window.addEventListener("resize", onResize);
    document.body.style.overflow = "hidden";
    document.body.append(mask);
    showImage(currentIndex);
    previewClose = () => {
      window.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("resize", onResize);
      window.removeEventListener("mousemove", onMouseMove);
      window.removeEventListener("mouseup", onMouseUp);
      document.body.style.overflow = previousOverflow;
      mask.remove();
    };
  };

  const closePreview = () => {
    if (previewClose) {
      const close = previewClose;
      previewClose = null;
      close();
    }
  };

  refs.client.addEventListener("change", () => {
    renderModels();
    scheduleDraftSave();
  });
  refs.model.addEventListener("change", scheduleDraftSave);
  refs.prompt?.addEventListener("input", scheduleDraftSave);
  refs.count?.addEventListener("input", scheduleDraftSave);
  refs.count?.addEventListener("change", () => {
    setCountValue(refs.count instanceof HTMLInputElement ? refs.count.valueAsNumber : 1);
    scheduleDraftSave();
  });
  refs.resolution?.addEventListener("change", () => {
    updateSize();
    scheduleDraftSave();
  });
  refs.ratio?.addEventListener("change", () => {
    updateSize();
    scheduleDraftSave();
  });
  refs.resolutionOptions?.addEventListener("click", (event) => {
    const button = event.target instanceof Element ? event.target.closest("[data-resolution]") : null;
    if (button instanceof HTMLButtonElement && refs.resolution instanceof HTMLSelectElement) {
      refs.resolution.value = button.dataset.resolution || "standard";
      refs.resolution.dispatchEvent(new Event("change", { bubbles: true }));
    }
  });
  refs.ratioOptions?.addEventListener("click", (event) => {
    const button = event.target instanceof Element ? event.target.closest("[data-ratio]") : null;
    if (button instanceof HTMLButtonElement && !button.disabled && refs.ratio instanceof HTMLSelectElement) {
      refs.ratio.value = button.dataset.ratio || "1:1";
      refs.ratio.dispatchEvent(new Event("change", { bubbles: true }));
    }
  });
  refs.file?.addEventListener("change", (event) => {
    const input = event.currentTarget;
    if (input instanceof HTMLInputElement) {
      handleFiles(input.files).finally(() => {
        input.value = "";
      });
    }
  });
  if (refs.upload instanceof HTMLElement) {
    ["dragenter", "dragover"].forEach((name) => {
      refs.upload.addEventListener(name, (event) => {
        event.preventDefault();
        refs.upload.classList.add("is-dragging");
      });
    });
    ["dragleave", "drop"].forEach((name) => {
      refs.upload.addEventListener(name, (event) => {
        event.preventDefault();
        refs.upload.classList.remove("is-dragging");
      });
    });
    refs.upload.addEventListener("drop", (event) => {
      handleFiles(event.dataTransfer?.files).catch(() => undefined);
    });
  }
  refs.clearInputs?.addEventListener("click", async () => {
    if (state.submission) {
      showToast("任务提交中，完成后再调整参考图", "info");
      return;
    }
    if (state.inputImages.length && !(await confirmAction(root.dataset.confirmClearInputs || "确认清空所有参考图？"))) {
      return;
    }
    state.inputImages = [];
    renderInputs();
    scheduleDraftSave();
  });
  refs.form.addEventListener("submit", (event) => {
    event.preventDefault();
    submit().catch((error) => {
      const message = error instanceof Error ? error.message : "提交失败";
      setError(message);
      showToast(message, "error");
    });
  });
  refs.clearResults?.addEventListener("click", () => {
    clearResults().catch(() => undefined);
  });
  refs.toggleHistory?.addEventListener("click", () => {
    state.historyCollapsed = !state.historyCollapsed;
    renderHistory();
  });
  refs.clearHistory?.addEventListener("click", async () => {
    if (state.history.length && !(await confirmAction(root.dataset.confirmClearHistory || "确认清空本次历史记录？"))) {
      return;
    }
    state.history = [];
    renderHistory();
  });
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "hidden") {
      return;
    }
    loadServerTasks({ showLoading: true }).catch(() => undefined);
  });

  renderInputs();
  renderRuns();
  renderHistory();
  updateSize();
  loadServerTasks({ showLoading: true }).catch(() => undefined);
  loadBootstrap().catch(() => undefined);
}

function createImageLabJob(csrfToken, body, onProgress) {
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open("POST", "/images/api/generate", true);
    request.withCredentials = true;
    request.setRequestHeader("Accept", "application/json");
    request.setRequestHeader("Content-Type", "application/json");
    request.setRequestHeader("X-CSRF-Token", csrfToken);
    request.setRequestHeader("X-Requested-With", "fetch");
    request.upload.onprogress = (event) => {
      if (typeof onProgress === "function") {
        const progress = {
          stage: "uploading",
          uploadLoaded: event.loaded,
        };
        if (event.lengthComputable) {
          progress.uploadTotal = event.total;
        }
        onProgress(progress);
      }
    };
    request.upload.onload = () => {
      if (typeof onProgress === "function") {
        onProgress({ stage: "creating" });
      }
    };
    request.onerror = () => reject(new Error("网络连接失败，任务未提交"));
    request.onabort = () => reject(new Error("提交已取消"));
    request.onload = () => {
      const data = parseJSONResponse(request.responseText);
      if (request.status < 200 || request.status >= 300) {
        reject(new Error(data.message || data.error?.message || request.statusText || "提交失败"));
        return;
      }
      resolve(data);
    };
    request.send(typeof body === "string" ? body : JSON.stringify(body));
  });
}

function parseJSONResponse(text) {
  try {
    return text ? JSON.parse(text) : {};
  } catch {
    return {};
  }
}

function nextFrame() {
  return new Promise((resolve) => {
    window.requestAnimationFrame(() => resolve());
  });
}

async function cancelServerRun(csrfToken, id) {
  const response = await fetch(`/images/api/jobs/${encodeURIComponent(id)}/cancel`, {
    method: "POST",
    credentials: "same-origin",
    headers: {
      Accept: "application/json",
      "X-CSRF-Token": csrfToken,
      "X-Requested-With": "fetch",
    },
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(data.message || data.error?.message || response.statusText);
  }
  return data;
}

async function deleteServerRun(csrfToken, id) {
  const response = await fetch(`/images/api/jobs/${encodeURIComponent(id)}`, {
    method: "DELETE",
    credentials: "same-origin",
    headers: {
      Accept: "application/json",
      "X-CSRF-Token": csrfToken,
      "X-Requested-With": "fetch",
    },
  });
  if (!response.ok && response.status !== 404) {
    const data = await response.json().catch(() => ({}));
    throw new Error(data.message || data.error?.message || response.statusText);
  }
}

function fileToInputImage(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(reader.error || new Error("读取图片失败"));
    reader.onload = () => {
      resolve({
        id: createId("input"),
        name: file.name,
        type: file.type || "image/png",
        dataUrl: String(reader.result || ""),
        size: file.size,
      });
    };
    reader.readAsDataURL(file);
  });
}

function actionButton(label, onClick, options = {}) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = options.className ? `ghost ${options.className}` : "ghost";
  button.textContent = label;
  if (options.title) {
    button.title = options.title;
    button.setAttribute("aria-label", options.title);
  }
  button.addEventListener("click", () => {
    const result = onClick();
    if (result && typeof result.catch === "function") {
      result.catch(() => undefined);
    }
  });
  return button;
}

function copyText(text, showToast) {
  if (navigator.clipboard?.writeText) {
    navigator.clipboard.writeText(text)
      .then(() => showToast("已复制", "ok"))
      .catch(() => fallbackCopyText(text, showToast));
    return;
  }
  fallbackCopyText(text, showToast);
}

function fallbackCopyText(text, showToast) {
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.style.position = "fixed";
  textarea.style.left = "-9999px";
  textarea.style.top = "0";
  textarea.setAttribute("readonly", "readonly");
  document.body.append(textarea);
  textarea.focus();
  textarea.select();
  const ok = document.execCommand("copy");
  textarea.remove();
  showToast(ok ? "已复制" : "复制失败", ok ? "ok" : "error");
}

async function copyImageToClipboard(src, showToast) {
  try {
    if (typeof ClipboardItem === "undefined" || !navigator.clipboard?.write) {
      copyText(src, showToast);
      return;
    }
    const blob = await fetchImageBlob(src);
    await navigator.clipboard.write([
      new ClipboardItem({ [blob.type || "image/png"]: blob }),
    ]);
    showToast("图片已复制到剪贴板", "ok");
  } catch (error) {
    showToast(error instanceof Error ? error.message : "复制图片失败", "error");
  }
}

async function downloadImage(src, fileName, showToast) {
  let objectURL = "";
  try {
    const blob = await fetchImageBlob(src);
    objectURL = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = objectURL;
    a.download = fileName;
    a.rel = "noopener";
    document.body.append(a);
    a.click();
    a.remove();
    showToast("已开始下载", "ok");
  } catch (error) {
    showToast(error instanceof Error ? error.message : "下载失败", "error");
  } finally {
    if (objectURL) {
      window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
    }
  }
}

async function imageSourceToInput(src, base) {
  if (!src) {
    throw new Error("图片地址为空");
  }
  if (String(src).startsWith("data:")) {
    return {
      ...base,
      dataUrl: src,
      size: estimateDataURLBytes(src),
      type: dataURLMIME(src) || base.type || "image/png",
    };
  }
  const blob = await fetchImageBlob(src);
  const dataUrl = await blobToDataURL(blob);
  return {
    ...base,
    dataUrl,
    size: blob.size,
    type: blob.type || base.type || "image/png",
  };
}

async function fetchImageBlob(src) {
  const response = await fetch(imageFetchURL(src), {
    credentials: "same-origin",
    cache: "force-cache",
  });
  if (!response.ok) {
    throw new Error(`图片读取失败：HTTP ${response.status}`);
  }
  const blob = await response.blob();
  if (!String(blob.type || "").startsWith("image/")) {
    throw new Error("返回内容不是图片");
  }
  return blob;
}

function imageDisplayURL(src) {
  return imageNeedsProxy(src) ? `/images/api/proxy?url=${encodeURIComponent(normalizeRemoteURL(src))}` : src;
}

function imageFetchURL(src) {
  return imageDisplayURL(src);
}

function imageNeedsProxy(src) {
  const value = String(src || "");
  return /^https?:\/\//i.test(value) || value.startsWith("//");
}

function normalizeRemoteURL(src) {
  const value = String(src || "");
  return value.startsWith("//") ? `${window.location.protocol}${value}` : value;
}

function createId(prefix) {
  return `${prefix}_${Date.now()}_${Math.random().toString(16).slice(2)}`;
}

function clampNumber(value, min, max) {
  const number = Number(value);
  if (!Number.isFinite(number)) {
    return min;
  }
  return Math.max(min, Math.min(max, Math.round(number)));
}

function estimateDataURLBytes(dataURL) {
  const comma = String(dataURL).indexOf(",");
  const payload = comma >= 0 ? String(dataURL).slice(comma + 1) : String(dataURL);
  const padding = payload.endsWith("==") ? 2 : payload.endsWith("=") ? 1 : 0;
  return Math.max(0, Math.floor(payload.length * 0.75) - padding);
}

function dataURLMIME(dataURL) {
  const match = String(dataURL).match(/^data:([^;,]+)/i);
  return match?.[1] || "";
}

function isSupportedInputImageType(mimeType) {
  return /^image\/(png|jpe?g|webp)$/i.test(String(mimeType || ""));
}

function extensionFromMIME(mimeType) {
  const normalized = String(mimeType || "").toLowerCase();
  if (normalized.includes("jpeg") || normalized.includes("jpg")) {
    return ".jpg";
  }
  if (normalized.includes("webp")) {
    return ".webp";
  }
  return ".png";
}

function formatBytes(bytes) {
  const value = Number(bytes || 0);
  if (value >= 1024 * 1024) {
    return `${(value / 1024 / 1024).toFixed(1)} MB`;
  }
  return `${Math.max(1, Math.round(value / 1024))} KB`;
}

function formatDuration(ms) {
  const seconds = Math.max(0, Number(ms || 0) / 1000);
  if (seconds < 10) {
    return `${seconds.toFixed(1)}s`;
  }
  return `${Math.round(seconds)}s`;
}

function formatDate(timestamp) {
  const date = new Date(Number(timestamp || 0));
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  return date.toLocaleString();
}

function formatImageSize(src) {
  if (String(src || "").startsWith("data:")) {
    return formatBytes(estimateDataURLBytes(src));
  }
  return "远程图片";
}

function blobToDataURL(blob) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(reader.error || new Error("图片读取失败"));
    reader.onload = () => resolve(String(reader.result || ""));
    reader.readAsDataURL(blob);
  });
}

function imageFileName(prompt, index = 0) {
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const slug = String(prompt || "")
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9\u4e00-\u9fa5]+/gi, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 36);
  const suffix = index > 0 ? `-${index + 1}` : "";
  return slug ? `image-lab-${stamp}-${slug}${suffix}.png` : `image-lab-${stamp}${suffix}.png`;
}
