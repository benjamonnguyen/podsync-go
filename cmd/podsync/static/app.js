function toast(msg) {
  var old = document.querySelector(".toast");
  if (old) old.remove();
  var t = document.createElement("div");
  t.className = "toast";
  t.setAttribute("role", "status");
  t.textContent = msg;
  document.body.appendChild(t);
  setTimeout(function () { t.remove(); }, 2000);
}

// Click or tap an element marked data-copy (an input) to copy its value.
document.addEventListener("click", function (e) {
  var input = e.target.closest("[data-copy]");
  if (!input) return;
  input.focus();
  input.select();
  var ok = function () { toast("Feed URL copied"); };
  var legacy = function () {
    try { if (document.execCommand("copy")) return ok(); } catch (err) {}
    toast("Could not copy. Select and copy it manually.");
  };
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(input.value).then(ok, legacy);
  } else {
    legacy();
  }
});

// "More info" toggles a clamped description; only shown when the text is cut off.
function initClips() {
  document.querySelectorAll("[data-toggle-clip]").forEach(function (btn) {
    var el = document.querySelector(btn.getAttribute("data-toggle-clip"));
    if (el && (el.classList.contains("open") || el.scrollHeight > el.clientHeight + 1)) btn.hidden = false;
  });
}
document.addEventListener("click", function (e) {
  var btn = e.target.closest("[data-toggle-clip]");
  if (!btn) return;
  var el = document.querySelector(btn.getAttribute("data-toggle-clip"));
  if (!el) return;
  var open = el.classList.toggle("open");
  btn.textContent = open ? "Less" : "More info";
  btn.setAttribute("aria-expanded", open);
});
document.addEventListener("DOMContentLoaded", initClips);
window.addEventListener("load", initClips);

// Select mode on the feed page: tick episodes and delete their files.
var selected = new Set();
function selectState() {
  var present = {};
  document.querySelectorAll("input.sel").forEach(function (cb) {
    present[cb.value] = true;
    cb.checked = selected.has(cb.value);
  });
  selected.forEach(function (id) { if (!present[id]) selected.delete(id); });
  var n = selected.size;
  var count = document.getElementById("sel-count");
  if (count) count.textContent = n + " selected";
  var del = document.querySelector("#bulk-delete button[type=submit]");
  if (del) del.disabled = n === 0;
}
function setSelecting(on) {
  document.body.classList.toggle("selecting", on);
  var form = document.getElementById("bulk-delete");
  if (form) form.hidden = !on;
  var toggle = document.querySelector("[data-select-toggle]");
  if (toggle) toggle.setAttribute("aria-pressed", on);
  if (!on) selected.clear();
  selectState();
}
document.addEventListener("click", function (e) {
  if (e.target.closest("[data-select-toggle]")) {
    setSelecting(!document.body.classList.contains("selecting"));
  } else if (e.target.closest("[data-select-all]")) {
    document.querySelectorAll("input.sel").forEach(function (cb) { selected.add(cb.value); });
    selectState();
  }
});
document.addEventListener("change", function (e) {
  var cb = e.target.closest("input.sel");
  if (!cb) return;
  if (cb.checked) selected.add(cb.value); else selected.delete(cb.value);
  selectState();
});
document.addEventListener("htmx:afterRequest", function (e) {
  if (e.target.id === "bulk-delete" && e.detail.successful) setSelecting(false);
});
document.addEventListener("htmx:responseError", function (e) {
  var msg = (e.detail.xhr && e.detail.xhr.responseText || "").trim();
  toast(msg ? msg.slice(0, 200) : "Request failed");
});

// An episode downloaded through "Load more" later appears in the main list
// too (the list refreshes while work is running). Keep the main-list copy.
document.addEventListener("htmx:afterSettle", function () {
  selectState();
  var main = document.getElementById("episodes");
  if (!main) return;
  var seen = {};
  main.querySelectorAll(".row[id]").forEach(function (r) { seen[r.id] = true; });
  document.querySelectorAll(".row[id]").forEach(function (r) {
    if (!main.contains(r) && seen[r.id]) r.remove();
  });
});

// Dialogs: data-open-dialog="id" opens one, data-close-dialog (or a tap on the
// backdrop) closes it.
document.addEventListener("click", function (e) {
  var open = e.target.closest("[data-open-dialog]");
  if (open) {
    var d = document.getElementById(open.getAttribute("data-open-dialog"));
    if (d) { pickState(d); d.showModal(); }
    return;
  }
  var close = e.target.closest("[data-close-dialog]");
  if (close) { close.closest("dialog").close(); return; }
  if (e.target.tagName === "DIALOG") e.target.close();
});

// Pick lists (the library's refresh dialog): a "Select all" box that follows
// the feed boxes, and a submit button that names how many are picked.
function pickState(root) {
  var boxes = root.querySelectorAll("input[name=feed]");
  var n = 0;
  boxes.forEach(function (cb) { if (cb.checked) n++; });
  var all = root.querySelector("[data-pick-all]");
  if (all) {
    all.checked = n === boxes.length;
    all.indeterminate = n > 0 && n < boxes.length;
  }
  var count = root.querySelector("[data-pick-count]");
  if (count) count.textContent = n + " of " + boxes.length;
  var submit = root.querySelector("[data-pick-submit]");
  if (submit) {
    submit.disabled = n === 0;
    submit.textContent = n === boxes.length ? "Check all feeds" : n === 1 ? "Check 1 feed" : "Check " + n + " feeds";
  }
}
document.addEventListener("change", function (e) {
  var form = e.target.closest("[data-pick-form]");
  if (!form) return;
  if (e.target.matches("[data-pick-all]")) {
    form.querySelectorAll("input[name=feed]").forEach(function (cb) { cb.checked = e.target.checked; });
  }
  pickState(form);
});
document.addEventListener("htmx:afterRequest", function (e) {
  if (e.target.matches("[data-pick-form]") && e.detail.successful) e.target.closest("dialog").close();
});
