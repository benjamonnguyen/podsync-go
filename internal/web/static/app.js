// Copy-to-clipboard buttons: <button data-copy="#input-id">
document.addEventListener("click", function (e) {
  var btn = e.target.closest("[data-copy]");
  if (!btn) return;
  var input = document.querySelector(btn.getAttribute("data-copy"));
  if (!input) return;
  input.focus();
  input.select();
  var done = function () {
    var old = btn.textContent;
    btn.textContent = "Copied";
    setTimeout(function () { btn.textContent = old; }, 1500);
  };
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(input.value).then(done);
  } else {
    try { if (document.execCommand("copy")) done(); } catch (err) {}
  }
});

// An episode downloaded through "Load more" later appears in the main list
// too (the list refreshes while work is running). Keep the main-list copy.
document.addEventListener("htmx:afterSettle", function () {
  var main = document.getElementById("episodes");
  if (!main) return;
  var seen = {};
  main.querySelectorAll(".row[id]").forEach(function (r) { seen[r.id] = true; });
  document.querySelectorAll(".row[id]").forEach(function (r) {
    if (!main.contains(r) && seen[r.id]) r.remove();
  });
});
