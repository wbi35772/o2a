// Apply the stored theme before first paint; theme.js owns every change.
// Kept as a separate file so the content security policy can forbid inline
// scripts.
try {
  var opencodeTheme = localStorage.getItem("opencode2api.ui.theme") || "auto";
  var opencodeDark =
    opencodeTheme === "dark" ||
    (opencodeTheme === "auto" && matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.setAttribute("data-theme", opencodeDark ? "dark" : "light");
} catch (_) {}
