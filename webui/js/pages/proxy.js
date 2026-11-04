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
};

const statusText = () => ({
  alive: t("可用", "proxy.state.alive"),
  limited: t("可连但被限流", "proxy.state.limited"),
  dead: t("不通", "proxy.state.dead"),
  unknown: t("未测试", "proxy.state.unknown"),
});

const statusTone = { alive: "ok", limited: "warn", dead: "bad", unknown: "" };

async function loadStore() {
  const store = await api("/api/proxy");
  if (!hasSession()) return;
  state.store = store;
  renderOverview();
  renderProviders();
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
  $("#proxy-timeout").value = settings.test_timeout_sec;
  $("#proxy-auto-select").checked = Boolean(settings.auto_select_healthy);
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
        el("td", { class: `proxy-state ${tone[node.status] || ""}`, text: labels[node.status] || node.status }),
        el("td", { class: "num", text: node.latency_ms ? `${node.latency_ms} ms` : "—" }),
        el("td", { text: node.last_test ? localTime(node.last_test) : "—" }),
        el("td", {}, [test]),
      ]),
    );
  }
  if (!state.nodes.length) emptyRow(body, 10, t("没有符合条件的节点", "proxy.noNodes"));
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
  event.preventDefault();
  try {
    await api("/api/proxy/settings", {
      method: "PUT",
      body: JSON.stringify({
        node_count: Number($("#proxy-node-count").value),
        port_base: Number($("#proxy-port-base").value),
        test_url: $("#proxy-test-url").value.trim(),
        test_timeout_sec: Number($("#proxy-timeout").value),
        auto_select_healthy: $("#proxy-auto-select").checked,
      }),
    });
    toast(t("设置已保存", "toast.proxy.settings"));
    await loadStore();
  } catch (error) {
    toast(error.message, true);
  }
}

async function addProvider(event) {
  event.preventDefault();
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
  for (const button of document.querySelectorAll("#proxy-tabs .tab"))
    button.classList.toggle("active", button.dataset.panel === name);
  for (const panel of document.querySelectorAll("#page-proxy [data-panel]"))
    panel.classList.toggle("hidden", panel.dataset.panel !== name);
  if (name === "logs") loadLogs();
}

function bind() {
  $("#proxy-refresh").addEventListener("click", refreshAll);
  $("#proxy-sync").addEventListener("click", syncSubscriptions);
  $("#proxy-test-all").addEventListener("click", () => testSelection(true));
  $("#proxy-test-picked").addEventListener("click", () => testSelection(false));
  $("#proxy-apply").addEventListener("click", () => applySelection(false));
  $("#proxy-apply-auto").addEventListener("click", () => applySelection(true));
  $("#proxy-settings-form").addEventListener("submit", saveSettings);
  $("#proxy-provider-form").addEventListener("submit", addProvider);
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
  $("#proxy-logs-refresh").addEventListener("click", loadLogs);
  for (const button of document.querySelectorAll("#proxy-tabs .tab"))
    button.addEventListener("click", () => showPanel(button.dataset.panel));
}

export default {
  id: "proxy",
  title: "代理中心",
  sub: "管理订阅、节点池与本地 SOCKS 出口。",
  init() {
    bind();
    onChange(() => {
      if (state.store) renderOverview();
    });
  },
  show() {
    refreshAll();
  },
  hide() {
    clearTimeout(state.timer);
  },
};
