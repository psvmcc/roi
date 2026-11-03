(() => {
  const modes = new Set(["system", "light", "dark"]);
  const key = "roi.theme";
  let current = "system";
  try {
    const saved = localStorage.getItem(key);
    if (modes.has(saved)) current = saved;
  } catch { /* System mode also works when browser storage is unavailable. */ }

  function apply(mode) {
    current = modes.has(mode) ? mode : "system";
    document.documentElement.dataset.theme = current;
    document.querySelectorAll("[data-theme-choice]").forEach(button => {
      button.setAttribute("aria-pressed", String(button.dataset.themeChoice === current));
    });
  }
  apply(current);
  document.addEventListener("DOMContentLoaded", () => {
    apply(current);
    document.querySelectorAll("[data-theme-choice]").forEach(button => {
      button.addEventListener("click", () => {
        apply(button.dataset.themeChoice);
        try { localStorage.setItem(key, current); } catch { /* Keep this page usable. */ }
      });
    });
  });
  window.addEventListener("storage", event => {
    if (event.key === key || event.key === null) apply(event.newValue);
  });
})();
