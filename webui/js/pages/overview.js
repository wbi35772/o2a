// Page: 概览 — readiness checklist, metrics, resources and token usage.
import * as store from "../store.js";
import { $, bindTabs, el, emptyRow, formatNumber, timeOnly } from "../ui.js";
import { t, onChange } from "../i18n.js";

function renderChecklist() {
  const config = store.get("config");
  const monitor = store.get("monitor");
  if (!config || !monitor) return;
  const resources = monitor.resources;
  const models = resources.models || {};
  const modelDescription = models.stale
    ? t(
        `目录来自${models.cache_source || "磁盘"}快取，等待后台刷新。`,
        "guide.models.cached",
        models.cache_source || "磁盘",
      )
    : t("上游目录已完成首次刷新且有可暴露模型。", "guide.models.ready");
  const checks = [
    [
      t("配置本地 Key", "guide.step.serverkey"),
      t("客户端使用 Server Key 访问 Gateway。", "guide.step.serverkey.desc"),
      (config.server_keys || []).length > 0,
    ],
    [
      t("选择上游通道", "guide.step.upstream"),
      t("至少有 Zen / Go Key，或启用匿名通道。", "guide.step.upstream.desc"),
      (config.zen_keys || []).length + (config.go_keys || []).length > 0 || config.anonymous,
    ],
    [
      t("检查 Proxy", "guide.step.proxy"),
      t("至少一个代理节点健康，或 WARP 正在承载免费流量。", "guide.step.proxy.desc"),
      (resources.warp?.active || 0) > 0 ||
        (resources.proxies || []).some((proxy) => proxy.healthy),
    ],
    [
      t("等待模型目录", "guide.step.models"),
      modelDescription,
      models.exposed > 0 && !!models.updated_at,
    ],
  ];
  const card = $("#overview-checklist");
  const ready = checks.every(([, , done]) => done);
  card.classList.toggle("hidden", ready);
  if (ready) return;
  const host = $("#guide-steps");
  host.replaceChildren();
  checks.forEach(([title, description, done]) => {
    host.append(
      el("article", { class: "guide-step" }, [
        el("h3", { text: title }),
        el("p", { text: description }),
        el("div", {
          class: `step-state ${done ? "ok" : "warn"}`,
          text: done ? t("✓ 已就绪", "guide.done") : t("○ 待处理", "guide.todo"),
        }),
      ]),
    );
  });
}

const proxyView = { q: "", page: 1, size: 20 };

function proxyRow(address, healthy, binding) {
  return el("tr", {}, [
    el("td", { text: address }),
    el("td", {
      class: `status ${healthy ? "ok" : "bad"}`,
      text: healthy ? t("正常", "state.healthy") : t("不可用", "state.unavailable"),
    }),
    el("td", { text: binding }),
  ]);
}

function renderProxyTable(resources) {
  const body = $("#proxy-table");
  const query = proxyView.q.toLowerCase();
  const rows = (resources.proxies || [])
    .filter((proxy) => !query || proxy.address.toLowerCase().includes(query))
    .map((proxy) =>
      proxyRow(
        proxy.address,
        proxy.healthy,
        `Z ${proxy.zen_keys} · G ${proxy.go_keys} · A ${
          proxy.anonymous ? t("开", "common.on") : t("关", "common.off")
        }`,
      ),
    );
  const warp = resources.warp;
  if (warp?.enabled && (!query || `warp ${warp.stack}`.toLowerCase().includes(query))) {
    // WARP is not a proxy: show it beside them so the egress that carries
    // free-model traffic is visible without pretending it is a bound node.
    rows.push(
      proxyRow(`WARP · ${warp.stack}`, warp.active > 0, `${warp.active}/${warp.size} · ${
        (warp.egress || []).join(", ") || "-"
      }`),
    );
  }
  const pages = Math.max(1, Math.ceil(rows.length / proxyView.size));
  proxyView.page = Math.min(Math.max(1, proxyView.page), pages);
  const start = (proxyView.page - 1) * proxyView.size;
  body.replaceChildren(...rows.slice(start, start + proxyView.size));
  if (!rows.length) emptyRow(body, 3);
  $("#overview-proxy-info").textContent = `${proxyView.page} / ${pages} · ${rows.length}`;
  $("#overview-proxy-prev").disabled = proxyView.page <= 1;
  $("#overview-proxy-next").disabled = proxyView.page >= pages;
}

function renderResources(resources) {
  renderProxyTable(resources);
  const keys = $("#key-table");
  keys.replaceChildren();
  if (resources.anonymous) {
    keys.append(
      el("tr", {}, [
        el("td", { text: "anonymous" }),
        el("td", { text: "zen" }),
        el("td", { class: "status ok", text: t("已启用", "state.enabled") }),
      ]),
    );
  }
  (resources.keys || []).forEach((key) => {
    keys.append(
      el("tr", {}, [
        el("td", { text: key.id }),
        el("td", { text: key.tier }),
        el("td", {
          class: `status ${key.cooldown_until ? "warn" : "ok"}`,
          text: key.cooldown_until ? t("冷却中", "state.cooling") : t("可用", "state.available"),
        }),
      ]),
    );
  });
  if (!keys.children.length) emptyRow(keys, 3);
}

function renderBreakdowns(metrics) {
  const host = $("#breakdowns");
  host.replaceChildren();
  [
    [t("端点", "breakdown.endpoint"), metrics.endpoints],
    [t("状态码", "breakdown.status"), metrics.statuses],
    [t("模型", "breakdown.model"), metrics.models],
    ["Tier", metrics.tiers],
  ].forEach(([title, data]) => {
    const body = el("tbody");
    Object.entries(data || {})
      .sort((a, b) => b[1] - a[1])
      .slice(0, 10)
      .forEach(([key, value]) => {
        body.append(el("tr", {}, [el("td", { text: key }), el("td", { class: "num", text: value })]));
      });
    if (!body.children.length) emptyRow(body, 2, t("暂无请求", "breakdown.empty"));
    host.append(
      el("div", { class: "breakdown" }, [
        el("h3", { text: title }),
        el("div", { class: "table-wrap" }, [el("table", { class: "table" }, [body])]),
      ]),
    );
  });
}

function renderSummary(monitor) {
  const metrics = monitor.metrics;
  $("#m-total").textContent = formatNumber(metrics.last_hour.total);
  $("#m-rate").textContent = `${((metrics.last_hour.success_rate || 0) * 100).toFixed(1)}%`;
  $("#m-p95").textContent = `${metrics.last_hour.p95_ms || 0} ms`;
  $("#m-active").textContent = `${metrics.active_requests || 0} / ${metrics.active_streams || 0}`;
  renderResources(monitor.resources);
  renderBreakdowns(metrics);
}

function renderTrend(metrics) {
  const trend = $("#usage-trend");
  const series = metrics?.series || [];
  const maximum = Math.max(1, ...series.map((point) => point.total_tokens || 0));
  trend.replaceChildren();
  series.forEach((point) => {
    const bar = el("div", { class: "trend-bar" });
    bar.style.height = `${Math.max(2, Math.round(((point.total_tokens || 0) / maximum) * 100))}%`;
    bar.title = `${timeOnly(point.minute)} · input ${formatNumber(point.input_tokens)} · output ${formatNumber(point.output_tokens)} · total ${formatNumber(point.total_tokens)}`;
    trend.append(bar);
  });
  $("#usage-trend-meta").textContent = series.length
    ? t(
        `峰值 ${formatNumber(maximum)} tokens / 分钟 · 近 ${series.length} 分钟`,
        "usage.trend.peak",
        formatNumber(maximum),
        series.length,
      )
    : t("尚无趋势数据", "usage.trend.empty");
}

function renderUsageModels(usage) {
  const host = $("#usage-models");
  host.replaceChildren();
  Object.entries(usage.models || {})
    .sort((a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0))
    .slice(0, 15)
    .forEach(([model, value]) => {
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: model }),
          el("td", { class: "num", text: formatNumber(value.input_tokens) }),
          el("td", { class: "num", text: formatNumber(value.output_tokens) }),
          el("td", { class: "num", text: formatNumber(value.total_tokens) }),
        ]),
      );
    });
  if (!host.children.length) emptyRow(host, 4, t("尚无 Token 用量", "usage.empty.models"));
}

function renderTiers(usage) {
  const host = $("#usage-tiers");
  const entries = Object.entries(usage.tiers || {}).sort(
    (a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0),
  );
  const total = Math.max(1, ...entries.map(([, value]) => value.total_tokens || 0));
  host.replaceChildren();
  entries.forEach(([tier, value]) => {
    const fill = el("div", { class: "tier-fill" });
    fill.style.width = `${Math.round(((value.total_tokens || 0) / total) * 100)}%`;
    host.append(
      el("div", { class: "tier-row" }, [
        el("strong", { text: tier }),
        el("div", { class: "tier-track" }, [fill]),
        el("span", { class: "tier-value", text: formatNumber(value.total_tokens) }),
      ]),
    );
  });
  if (!entries.length) host.append(el("div", { class: "empty", text: t("尚无 Tier 用量", "usage.empty.tiers") }));
}

const keyColor = (index) => `hsl(${(index * 47 + 198) % 360} 64% 48%)`;

function renderUsageKeys(usage) {
  const config = store.get("config");
  const names = new Map((config?.server_keys || []).map((key) => [key.id, key.display || key.id]));
  const entries = Object.entries(usage.server_keys || {})
    .map(([id, value]) => ({
      id,
      display: names.get(id) || (id === "playground" ? t("请求测试", "usage.keys.playground") : id),
      ...value,
    }))
    .sort((a, b) => (b.total_tokens || 0) - (a.total_tokens || 0));
  const total = entries.reduce((sum, entry) => sum + (entry.total_tokens || 0), 0);
  const share = $("#usage-key-share");
  const table = $("#usage-keys");
  share.replaceChildren();
  table.replaceChildren();
  if (!entries.length) {
    share.append(el("div", { class: "empty", text: t("尚无 Server Key 用量", "usage.empty.keys") }));
    emptyRow(table, 7);
    return;
  }
  const bar = el("div", { class: "key-share-bar" });
  const legend = el("div", { class: "key-share-legend" });
  entries.forEach((entry, index) => {
    const color = keyColor(index);
    const percent = total ? ((entry.total_tokens || 0) / total) * 100 : 0;
    const segment = el("span", { class: "key-share-segment" });
    segment.style.width = `${percent}%`;
    segment.style.background = color;
    segment.title = `${entry.display} · ${formatNumber(entry.total_tokens)} (${percent.toFixed(1)}%)`;
    bar.append(segment);
    const legendDot = el("span", { class: "key-dot" });
    legendDot.style.background = color;
    legend.append(
      el("span", { class: "key-share-item" }, [
        legendDot,
        el("span", { text: `${entry.display} · ${percent.toFixed(1)}%` }),
      ]),
    );
    const rowDot = el("span", { class: "key-dot" });
    rowDot.style.background = color;
    table.append(
      el("tr", {}, [
        el("td", {}, [rowDot, el("span", { class: "mono", text: entry.display })]),
        el("td", { class: "num", text: formatNumber(entry.requests) }),
        el("td", { class: "num", text: formatNumber(entry.input_tokens) }),
        el("td", { class: "num", text: formatNumber(entry.output_tokens) }),
        el("td", { class: "num", text: formatNumber(entry.cached_tokens) }),
        el("td", { class: "num", text: formatNumber(entry.reasoning_tokens) }),
        el("td", { class: "num", text: formatNumber(entry.total_tokens) }),
      ]),
    );
  });
  share.append(bar, legend);
}

function renderUsage(monitor) {
  const usage = monitor.usage?.last_hour || {};
  const tokens = usage.tokens || {};
  $("#u-coverage").textContent = `${((usage.coverage || 0) * 100).toFixed(1)}%`;
  $("#u-total").textContent = formatNumber(tokens.total_tokens);
  $("#u-cached").textContent = formatNumber(tokens.cached_tokens);
  $("#u-reasoning").textContent = formatNumber(tokens.reasoning_tokens);
  renderTrend(monitor.metrics);
  renderUsageModels(usage);
  renderUsageKeys(usage);
  renderTiers(usage);
}

function render() {
  renderChecklist();
  const monitor = store.get("monitor");
  if (!monitor) return;
  renderSummary(monitor);
  renderUsage(monitor);
}

export default {
  id: "overview",
  title: "概览",
  sub: "运行指标、Token 用量与资源状态。",
  init() {
    bindTabs($("#page-overview"));
    $("#overview-proxy-search").addEventListener("input", (event) => {
      proxyView.q = event.target.value.trim();
      proxyView.page = 1;
      const monitor = store.get("monitor");
      if (monitor) renderProxyTable(monitor.resources);
    });
    $("#overview-proxy-prev").addEventListener("click", () => {
      if (proxyView.page > 1) {
        proxyView.page -= 1;
        const monitor = store.get("monitor");
        if (monitor) renderProxyTable(monitor.resources);
      }
    });
    $("#overview-proxy-next").addEventListener("click", () => {
      proxyView.page += 1;
      const monitor = store.get("monitor");
      if (monitor) renderProxyTable(monitor.resources);
    });
    $("#overview-proxy-size").addEventListener("change", (event) => {
      proxyView.size = Number(event.target.value) || 20;
      proxyView.page = 1;
      const monitor = store.get("monitor");
      if (monitor) renderProxyTable(monitor.resources);
    });
    store.on("monitor", render);
    store.on("config", renderChecklist);
    onChange(render);
  },
  show: render,
};
