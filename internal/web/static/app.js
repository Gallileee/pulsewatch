// Live updates: the server sends an event after every check; the page then fetches the
// rendered live part again (at most once a second) and swaps it in. Without JavaScript the
// page still works, it just doesn't update itself.
(() => {
  "use strict";

  const live = document.getElementById("live");
  const state = document.getElementById("live-state");
  const MIN_GAP = 1000;
  let last = 0;
  let timer = null;
  let loading = false;

  function setState(on, text) {
    state.textContent = text;
    state.classList.toggle("on", on);
  }

  async function refresh() {
    timer = null;
    if (loading) {
      schedule();
      return;
    }
    loading = true;
    last = Date.now();
    try {
      const response = await fetch("/partials/live", { cache: "no-store" });
      if (response.ok) {
        live.innerHTML = await response.text();
        relativeTimes();
      }
    } catch {
      // The stream reconnects by itself; the next event tries again.
    } finally {
      loading = false;
    }
  }

  function schedule() {
    if (timer !== null) return;
    timer = setTimeout(refresh, Math.max(0, MIN_GAP - (Date.now() - last)));
  }

  function relative(date) {
    const seconds = Math.round((Date.now() - date.getTime()) / 1000);
    if (seconds < 5) return "just now";
    if (seconds < 60) return `${seconds} s ago`;
    const minutes = Math.round(seconds / 60);
    if (minutes < 60) return `${minutes} min ago`;
    const hours = Math.round(minutes / 60);
    if (hours < 48) return `${hours} h ago`;
    return `${Math.round(hours / 24)} days ago`;
  }

  function relativeTimes() {
    for (const el of document.querySelectorAll("time[data-relative]")) {
      const date = new Date(el.getAttribute("datetime"));
      if (Number.isNaN(date.getTime())) continue;
      if (!el.title) el.title = el.textContent.trim();
      el.textContent = relative(date);
    }
  }

  relativeTimes();
  setInterval(relativeTimes, 5000);

  if (!("EventSource" in window)) return;
  const events = new EventSource("/api/events");
  events.addEventListener("open", () => setState(true, "Live"));
  events.addEventListener("error", () => setState(false, "Reconnecting…"));
  events.addEventListener("check", schedule);
})();
