// Page: 代理中心 — subscriptions, node pool, probes, logs.
import { api, hasSession } from "../api.js";
import { t, onChange } from "../i18n.js";
import { $, el, emptyRow, localTime, toast } from "../ui.js";

const state = {
  store: null,
  nodes: [],
  total: 0,
  page: 1,
  size: 50,
  status: "",
  provider: "",
  q: "",
  selected: new Set(),
  timer: null,
  panel: "nodes",
  cfEditing: "",
};

const statusText = () => ({
  alive: t("可用", "proxy.state.alive"),
  limited: t("可连但被限流", "proxy.state.limited"),
  dead: t("不通", "proxy.state.dead"),
  unknown: t("未测试", "proxy.state.unknown"),
});

const statusTone = { alive: "ok", limited: "warn", dead: "bad", unknown: "" };

const quotaText = () => ({
  ok: t("可用", "proxy.quota.ok"),
  limited: t("已限额", "proxy.quota.limited"),
  blocked: t("被封", "proxy.quota.blocked"),
  error: t("探测失败", "proxy.quota.error"),
  unknown: t("未探测", "proxy.quota.unknown"),
});

const quotaTone = { ok: "ok", limited: "warn", blocked: "bad", error: "warn", unknown: "" };

const egressText = () => ({
  proxy: t("代理池（全部流量）", "proxy.mode.proxy"),
  warp: t("WARP（仅免费模型）", "proxy.mode.warp"),
  "warp-only": t("WARP（免费模型，其余直连）", "proxy.mode.warpOnly"),
  direct: t("直连（不使用代理）", "proxy.mode.direct"),
});

const egressNote = () => ({
  proxy: t("免费模型和 Go/付费流量都走代理池。", "proxy.mode.note.proxy"),
  warp: t("免费模型走内置 WARP，Go/付费流量走代理池。", "proxy.mode.note.warp"),
  "warp-only": t("免费模型走内置 WARP；其余流量直连，不使用代理池。", "proxy.mode.note.warpOnly"),
  direct: t("全部流量直连，不使用 WARP 和代理池。", "proxy.mode.note.direct"),
});

function currentEgressMode() {
  return state.store?.settings?.egress_mode || "proxy";
}

function renderEgressMode() {
  const mode = currentEgressMode();
  $("#proxy-egress-mode").value = mode;
  $("#proxy-mode-current").textContent = egressText()[mode] || mode;
  $("#proxy-mode-note").textContent = egressNote()[mode] || "";
}


function quotaLabel(node) {
  const label = quotaText()[node.quota] || node.quota || "—";
  const retry = Number(node.retry_after_seconds) || 0;
  if (retry > 0 && node.quota !== "ok") return `${label} · ${Math.ceil(retry / 3600)}h`;
  return label;
}

async function loadStore() {
  const store = await api("/api/proxy");
  if (!hasSession()) return;
  state.store = store;
  renderOverview();
  renderProviders();
  renderCFSources();
  renderSettings();
  scheduleJobPoll();
}

async function loadNodes() {
  const params = new URLSearchParams({
    page: String(state.page),
    size: String(state.size),
  });
  if (state.status) params.set("status", state.status);
  if (state.provider) params.set("provider", state.provider);
  if (state.q) params.set("q", state.q);
  const result = await api(`/api/proxy/nodes?${params}`);
  if (!hasSession()) return;
  state.nodes = result.nodes || [];
  state.total = result.total || 0;
  renderNodes();
}

function scheduleJobPoll() {
  clearTimeout(state.timer);
  if (state.store?.job?.running) {
    state.timer = setTimeout(async () => {
      try {
        await loadStore();
        await loadNodes();
      } catch {
        /* a transient poll failure leaves the next tick to retry */
      }
    }, 2500);
  }
}

function fact(label, value) {
  return el("div", { class: "fact" }, [
    el("span", { class: "fact-label", text: label }),
    el("span", { class: "fact-value", text: String(value) }),
  ]);
}

function renderOverview() {
  const store = state.store;
  if (!store) return;
  const host = $("#proxy-cards");
  host.replaceChildren();
  const rt = store.runtime || {};
  const counts = rt.statuses || {};
  host.append(
    fact(t("出口总数", "proxy.fact.total"), rt.proxylines ?? 0),
    fact(t("可用", "proxy.fact.alive"), counts.alive ?? 0),
    fact(t("限流", "proxy.fact.limited"), counts.limited ?? 0),
    fact(t("不通", "proxy.fact.dead"), counts.dead ?? 0),
    fact(t("未测", "proxy.fact.unknown"), counts.unknown ?? 0),
    fact(t("已选", "proxy.fact.selected"), rt.selected ?? 0),
    fact("mihomo", rt.mihomo_alive ? `PID ${rt.mihomo_pid}` : t("未运行", "proxy.mihomoDown")),
    fact(t("上次同步", "proxy.fact.sync"), rt.last_sync ? localTime(rt.last_sync) : "—"),
  );
  const job = store.job;
  const status = $("#proxy-status");
  if (job?.running) status.textContent = t(`${job.action} 进行中…`, "proxy.jobRunning", job.action);
  else if (job?.error) status.textContent = t(`上次任务失败：${job.error}`, "proxy.jobFailed", job.error);
  else if (rt.last_sync_error) status.textContent = rt.last_sync_error;
  else status.textContent = t("订阅改动后点「同步订阅」，选中结果会落到本地 SOCKS 端口。", "proxy.hint");
}

function renderProviders() {
  const body = $("#proxy-providers");
  body.replaceChildren();
  const providers = state.store?.providers || [];
  for (const provider of providers) {
    const toggle = el("input", { type: "checkbox", checked: provider.enabled });
    toggle.addEventListener("change", async () => {
      try {
        await api(`/api/proxy/providers/${provider.id}`, {
          method: "PATCH",
          body: JSON.stringify({ enabled: toggle.checked }),
        });
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      }
    });
    const remove = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("删除", "common.delete") });
    remove.addEventListener("click", async () => {
      if (!confirm(t(`删除订阅「${provider.name}」？`, "proxy.confirmDelete", provider.name))) return;
      try {
        await api(`/api/proxy/providers/${provider.id}`, { method: "DELETE" });
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      }
    });
    body.append(
      el("tr", {}, [
        el("td", { class: "mono", text: provider.id }),
        el("td", { text: provider.name }),
        el("td", { class: "mono proxy-url", text: provider.url }),
        el("td", { class: "num", text: String(provider.node_count || 0) }),
        el("td", { text: provider.last_fetch ? localTime(provider.last_fetch) : "—" }),
        el("td", { text: provider.last_error || "—" }),
        el("td", { class: "num" }, [toggle]),
        el("td", {}, [remove]),
      ]),
    );
  }
  if (!providers.length) emptyRow(body, 8, t("还没有订阅", "proxy.noProviders"));
}

function renderSettings() {
  const settings = state.store?.settings;
  if (!settings) return;
  $("#proxy-node-count").value = settings.node_count;
  $("#proxy-port-base").value = settings.port_base;
  $("#proxy-test-url").value = settings.test_url;
  $("#proxy-timeout").value = settings.test_timeout_seconds;
  $("#proxy-auto-select").checked = Boolean(settings.auto_select_healthy);
  renderEgressMode();
}

function renderNodes() {
  const body = $("#proxy-nodes");
  body.replaceChildren();
  const tone = statusTone;
  const labels = statusText();
  for (const node of state.nodes) {
    if (node.selected) state.selected.add(node.name);
    const box = el("input", { type: "checkbox", checked: state.selected.has(node.name) });
    box.addEventListener("change", () => {
      if (box.checked) state.selected.add(node.name);
      else state.selected.delete(node.name);
    });
    const test = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("测试", "proxy.test") });
    test.addEventListener("click", async () => {
      test.disabled = true;
      try {
        await api("/api/proxy/test", { method: "POST", body: JSON.stringify({ names: [node.name] }) });
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      } finally {
        test.disabled = false;
      }
    });
    body.append(
      el("tr", {}, [
        el("td", { class: "num" }, [box]),
        el("td", { class: "mono", text: node.name }),
        el("td", { text: node.provider }),
        el("td", { text: node.type }),
        el("td", { class: "mono", text: `${node.server}:${node.port}` }),
        el("td", { class: "num", text: node.listen_port ? String(node.listen_port) : "—" }),
        el("td", { class: "mono", text: node.egress_ip || "—" }),
        el("td", {
          class: `proxy-state ${quotaTone[node.quota] || ""}`,
          text: quotaLabel(node),
          title: node.quota_note || "",
        }),
        el("td", { class: `proxy-state ${tone[node.status] || ""}`, text: labels[node.status] || node.status }),
        el("td", { class: "num", text: node.latency_ms ? `${node.latency_ms} ms` : "—" }),
        el("td", { text: node.last_test ? localTime(node.last_test) : "—" }),
        el("td", {}, [test]),
      ]),
    );
  }
  if (!state.nodes.length) emptyRow(body, 12, t("没有符合条件的节点", "proxy.noNodes"));
  const pages = Math.max(1, Math.ceil(state.total / state.size));
  $("#proxy-page").textContent = `${state.page} / ${pages} · ${state.total}`;
  $("#proxy-prev").disabled = state.page <= 1;
  $("#proxy-next").disabled = state.page >= pages;
}

async function refreshAll() {
  try {
    await Promise.all([loadStore(), loadNodes()]);
  } catch (error) {
    toast(error.message, true);
  }
}

async function syncSubscriptions() {
  try {
    await api("/api/proxy/sync", { method: "POST", body: "{}" });
    toast(t("已开始同步", "toast.proxy.sync"));
    await refreshAll();
  } catch (error) {
    toast(error.message, true);
  }
}

async function testSelection(allMatching) {
  const body = allMatching
    ? { status: state.status || undefined, provider: state.provider || undefined }
    : { names: [...state.selected] };
  if (!allMatching && !body.names.length) {
    toast(t("先在列表里勾选节点", "toast.proxy.pickNodes"), true);
    return;
  }
  try {
    const result = await api("/api/proxy/test", { method: "POST", body: JSON.stringify(body) });
    toast(t(`已开始测试 ${result.targets} 个节点`, "toast.proxy.testing", result.targets));
    await refreshAll();
  } catch (error) {
    toast(error.message, true);
  }
}

function splitLines(value) {
  return value
    .split(/\r?\n/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function renderCFSources() {
  const body = $("#cf-sources");
  if (!body) return;
  body.replaceChildren();
  const sources = state.store?.cf_sources || [];
  for (const source of sources) {
    const edit = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("编辑", "common.edit") });
    edit.addEventListener("click", () => fillCFForm(source));
    const scan = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("优选 IP", "cf.scan") });
    scan.addEventListener("click", async () => {
      scan.disabled = true;
      const label = scan.textContent;
      scan.textContent = t("扫描中…", "cf.scanning");
      try {
        const result = await api(`/api/proxy/cf/${source.id}/scan`, {
          method: "POST",
          body: JSON.stringify({ candidates: 200, timeout_ms: 1500, keep: 8 }),
        });
        const best = result.report?.hits?.[0]?.latency_ms;
        const suffix = best == null ? "" : `，最佳 ${best} ms`;
        const tail = result.sync_started ? "；已自动同步" : "；同步订阅后生效";
        toast(t(`优选完成：${result.report.reachable} 个可用${suffix}${tail}`, "toast.cf.scanned", result.report.reachable, best ?? "—"));
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      } finally {
        scan.disabled = false;
        scan.textContent = label;
      }
    });
    const rebuild = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("生成", "cf.rebuild") });
    rebuild.addEventListener("click", async () => {
      rebuild.disabled = true;
      try {
        await api(`/api/proxy/cf/${source.id}/build`, { method: "POST", body: "{}" });
        toast(t("节点文件已重新生成", "toast.cf.built"));
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      } finally {
        rebuild.disabled = false;
      }
    });
    const remove = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("删除", "common.delete") });
    remove.addEventListener("click", async () => {
      if (!confirm(t(`删除出口源「${source.name}」？`, "cf.confirmDelete", source.name))) return;
      try {
        await api(`/api/proxy/cf/${source.id}`, { method: "DELETE" });
        if (state.cfEditing === source.id) resetCFForm();
        await loadStore();
      } catch (error) {
        toast(error.message, true);
      }
    });
    body.append(
      el("tr", {}, [
        el("td", { text: source.name }),
        el("td", { class: "mono proxy-url", text: source.host }),
        el("td", { class: "num", text: String((source.egress || []).length) }),
        el("td", { class: "num", text: String(source.node_count || 0) }),
        el("td", { text: source.last_build ? localTime(source.last_build) : "—" }),
        el("td", { text: source.last_error || "—" }),
        el("td", {}, [edit, scan, rebuild, remove]),
      ]),
    );
  }
  if (!sources.length) emptyRow(body, 7, t("还没有 Cloudflare 出口源", "cf.empty"));
}

function fillCFForm(source) {
  state.cfEditing = source.id || "";
  $("#cf-name").value = source.name || "";
  $("#cf-host").value = source.host || "";
  $("#cf-uuid").value = source.uuid || "";
  $("#cf-port").value = source.port || 443;
  $("#cf-path").value = source.path_template || "/proxyip=<ip>";
  $("#cf-egress").value = (source.egress || []).join("\n");
  $("#cf-edge").value = (source.edge || []).join("\n");
  $("#cf-enabled").checked = source.enabled !== false;
}

function resetCFForm() {
  fillCFForm({ enabled: true, path_template: "/proxyip=<ip>", port: 443 });
  $("#cf-deploy").value = "";
}

function cfPayload() {
  return {
    name: $("#cf-name").value.trim(),
    host: $("#cf-host").value.trim(),
    uuid: $("#cf-uuid").value.trim(),
    port: Number($("#cf-port").value) || 443,
    path_template: $("#cf-path").value.trim() || "/proxyip=<ip>",
    egress: splitLines($("#cf-egress").value),
    edge: splitLines($("#cf-edge").value),
    enabled: $("#cf-enabled").checked,
  };
}

async function saveCFSource() {
  const payload = cfPayload();
  if (!payload.name || !payload.host || !payload.uuid) {
    toast(t("名称、Worker 域名和 UUID 都要填", "toast.cf.required"), true);
    return;
  }
  try {
    const path = state.cfEditing ? `/api/proxy/cf/${state.cfEditing}` : "/api/proxy/cf";
    const result = await api(path, {
      method: state.cfEditing ? "PATCH" : "POST",
      body: JSON.stringify(payload),
    });
    state.cfEditing = result.id || state.cfEditing;
    toast(t(`已生成 ${result.node_count || 0} 个节点，点「同步订阅」加载`, "toast.cf.saved", result.node_count || 0));
    await loadStore();
  } catch (error) {
    toast(error.message, true);
  }
}

async function parseCFDeploy() {
  const text = $("#cf-deploy").value.trim();
  if (!text) {
    toast(t("先粘贴部署参数", "toast.cf.noDeploy"), true);
    return;
  }
  try {
    const params = await api("/api/proxy/cf/parse", {
      method: "POST",
      body: JSON.stringify({ text }),
    });
    $("#cf-uuid").value = params.uuid || "";
    $("#cf-host").value = params.host || "";
    $("#cf-path").value = params.path_template || "/proxyip=<ip>";
    if (!$("#cf-name").value) $("#cf-name").value = (params.host || "").split(".")[0] || "cloudflare";
    const socks = params.socks5 || [];
    const proxyIPs = params.proxyip || [];
    const list = (params.path_template || "").startsWith("/socks5=") && socks.length ? socks : proxyIPs;
    if (list.length) {
      $("#cf-egress").value =
        (params.path_template || "").startsWith("/socks5=")
          ? list.map((item) => (item.startsWith("socks5://") ? item : `socks5://${item}`)).join("\n")
          : list.join("\n");
    }
    toast(t("已填入解析结果", "toast.cf.parsed"));
  } catch (error) {
    toast(error.message, true);
  }
}

async function probeEgress(allMatching) {
  const body = allMatching
    ? { status: state.status || undefined, provider: state.provider || undefined }
    : { names: [...state.selected] };
  if (!allMatching && !body.names.length) {
    toast(t("先在列表里勾选节点", "toast.proxy.pickNodes"), true);
    return;
  }
  try {
    const result = await api("/api/proxy/egress", { method: "POST", body: JSON.stringify(body) });
    toast(t(`已开始探测 ${result.targets} 个出口`, "toast.proxy.egress", result.targets));
    await refreshAll();
  } catch (error) {
    toast(error.message, true);
  }
}

async function applySelection(auto) {
  try {
    const result = await api("/api/proxy/select", {
      method: "POST",
      body: JSON.stringify(auto ? { auto: true } : { names: [...state.selected] }),
    });
    toast(t(`已应用 ${result.selected} 个出口`, "toast.proxy.applied", result.selected));
    await refreshAll();
  } catch (error) {
    toast(error.message, true);
  }
}

async function saveSettings(event) {
  event?.preventDefault?.();
  try {
    await api("/api/proxy/settings", {
      method: "PUT",
      body: JSON.stringify({
        node_count: Number($("#proxy-node-count").value),
        port_base: Number($("#proxy-port-base").value),
        test_url: $("#proxy-test-url").value.trim(),
        test_timeout_seconds: Number($("#proxy-timeout").value),
        auto_select_healthy: $("#proxy-auto-select").checked,
        egress_mode: $("#proxy-egress-mode").value,
      }),
    });
    toast(t("设置已保存", "toast.proxy.settings"));
    await loadStore();
  } catch (error) {
    toast(error.message, true);
  }
}

async function applyEgressMode() {
  const button = $("#proxy-mode-apply");
  const mode = $("#proxy-egress-mode").value;
  button.disabled = true;
  try {
    const result = await api("/api/proxy/egress-mode", {
      method: "PUT",
      body: JSON.stringify({ mode }),
    });
    if (result.changed === false) toast(t("出口方式未变化", "toast.proxy.modeSame"));
    else toast(t("正在切换出口方式…", "toast.proxy.modeSwitching"));
    await refreshAll();
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

async function addProvider(event) {
  event?.preventDefault?.();
  try {
    await api("/api/proxy/providers", {
      method: "POST",
      body: JSON.stringify({
        name: $("#proxy-provider-name").value.trim(),
        url: $("#proxy-provider-url").value.trim(),
      }),
    });
    $("#proxy-provider-name").value = "";
    $("#proxy-provider-url").value = "";
    toast(t("订阅已添加，记得点同步", "toast.proxy.providerAdded"));
    await loadStore();
  } catch (error) {
    toast(error.message, true);
  }
}

async function loadLogs() {
  try {
    const result = await api("/api/proxy/logs");
    if (!hasSession()) return;
    $("#proxy-logs").textContent = (result.lines || []).join("\n") || t("(空)", "proxy.logsEmpty");
  } catch (error) {
    toast(error.message, true);
  }
}

function showPanel(name) {
  state.panel = name;
  for (const button of document.querySelectorAll("#proxy-tabs [data-proxy-tab]")) {
    const active = button.dataset.proxyTab === name;
    button.classList.toggle("active", active);
    button.setAttribute("aria-selected", active ? "true" : "false");
  }
  for (const panel of document.querySelectorAll("#page-proxy [data-proxy-panel]"))
    panel.classList.toggle("hidden", panel.dataset.proxyPanel !== name);
  if (name === "logs") loadLogs();
}

function bind() {
  $("#proxy-refresh").addEventListener("click", refreshAll);
  $("#proxy-sync").addEventListener("click", syncSubscriptions);
  $("#proxy-test-all").addEventListener("click", () => testSelection(true));
  $("#proxy-test-picked").addEventListener("click", () => testSelection(false));
  $("#proxy-egress-all").addEventListener("click", () => probeEgress(true));
  $("#proxy-egress-picked").addEventListener("click", () => probeEgress(false));
  $("#proxy-apply").addEventListener("click", () => applySelection(false));
  $("#proxy-apply-auto").addEventListener("click", () => applySelection(true));
  $("#cf-save").addEventListener("click", saveCFSource);
  $("#cf-parse").addEventListener("click", parseCFDeploy);
  $("#cf-reset").addEventListener("click", resetCFForm);
  for (const id of ["cf-name", "cf-host", "cf-uuid", "cf-port", "cf-path"])
    $(`#${id}`).addEventListener("keydown", (event) => {
      if (event.key !== "Enter") return;
      event.preventDefault();
      saveCFSource();
    });
  $("#proxy-settings-save").addEventListener("click", saveSettings);
  $("#proxy-mode-apply").addEventListener("click", applyEgressMode);
  $("#proxy-provider-add").addEventListener("click", addProvider);
  $("#proxy-prev").addEventListener("click", () => {
    if (state.page > 1) {
      state.page -= 1;
      loadNodes();
    }
  });
  $("#proxy-next").addEventListener("click", () => {
    state.page += 1;
    loadNodes();
  });
  const reload = () => {
    state.page = 1;
    loadNodes();
  };
  $("#proxy-filter-status").addEventListener("change", (event) => {
    state.status = event.target.value;
    reload();
  });
  $("#proxy-filter-q").addEventListener("input", (event) => {
    state.q = event.target.value.trim();
    clearTimeout(state.qTimer);
    state.qTimer = setTimeout(reload, 300);
  });
  $("#proxy-filter-q").addEventListener("keydown", (event) => {
    if (event.key === "Enter") event.preventDefault();
  });
  // The proxy forms live inside the outer config form now, so Enter must not
  // bubble up into an implicit config submit.
  for (const id of ["proxy-provider-name", "proxy-provider-url"])
    $(`#${id}`).addEventListener("keydown", (event) => {
      if (event.key !== "Enter") return;
      event.preventDefault();
      addProvider();
    });
  for (const id of ["proxy-node-count", "proxy-port-base", "proxy-test-url", "proxy-timeout"])
    $(`#${id}`).addEventListener("keydown", (event) => {
      if (event.key !== "Enter") return;
      event.preventDefault();
      saveSettings();
    });
  $("#proxy-logs-refresh").addEventListener("click", loadLogs);
  for (const button of document.querySelectorAll("#proxy-tabs [data-proxy-tab]"))
    button.addEventListener("click", () => showPanel(button.dataset.proxyTab));
}

let bound = false;

export default {
  id: "proxy",
  title: "代理中心",
  sub: "管理订阅、节点池与本地 SOCKS 出口。",
  init() {
    if (bound) return;
    bound = true;
    bind();
    showPanel(state.panel);
    onChange(() => {
      if (state.store) renderOverview();
    });
  },
  show() {
    showPanel(state.panel);
    refreshAll();
  },
  hide() {
    clearTimeout(state.timer);
  },
};
