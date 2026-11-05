// fetch wrapper: JSON handling, CSRF for writes, 401 routed to the login view.
import * as store from "./store.js";
import { t } from "./i18n.js";

let csrf = "";
let onUnauthorized = null;

export function setCsrf(value) {
  csrf = value || "";
}

export function clearCsrf() {
  csrf = "";
}

export function hasSession() {
  return csrf !== "";
}

export function setUnauthorizedHandler(handler) {
  onUnauthorized = handler;
}

export async function api(path, options = {}) {
  const headers = { ...(options.headers || {}), Accept: "application/json" };
  if (options.body) headers["Content-Type"] = "application/json";
  if (csrf && !["GET", "HEAD"].includes((options.method || "GET").toUpperCase()))
    headers["X-CSRF-Token"] = csrf;
  const response = await fetch(path, { ...options, headers });
  if (response.status === 204) return null;
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    // The login endpoint answers 401 for bad credentials: surface its message
    // instead of treating the attempt as an expired session.
    if (response.status === 401 && path !== "/api/auth/login") {
      if (onUnauthorized) onUnauthorized();
      throw new Error(t("登录已失效", "error.session"));
    }
    throw new Error(data.error?.message || `HTTP ${response.status}`);
  }
  return data;
}

// Client-facing API base derived from the configured listen address. When the
// UI and the API share a listener the page's own origin is exactly right,
// which also covers a reverse proxy that terminates TLS on 443 while the
// container listens on another port.
export function derivedAPIBase() {
  const config = store.get("config");
  if (config?.effective?.same_port) return location.origin;
  const listen = config?.effective?.listen || config?.listen || ":8080";
  const match = listen.match(/^\[.*\]:(\d+)$/) || listen.match(/:(\d+)$/);
  const port = match?.[1] || "8080";
  const defaultPort = (location.protocol === "https:" && port === "443") || (location.protocol === "http:" && port === "80");
  return defaultPort ? `${location.protocol}//${location.hostname}` : `${location.protocol}//${location.hostname}:${port}`;
}
