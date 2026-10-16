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
