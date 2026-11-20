// Counts down the seconds on the confirm page. The page works without it.
(function () {
  "use strict";
  var left = document.getElementById("left");
  if (!left) return;
  var n = parseInt(left.textContent, 10);
  var timer = setInterval(function () {
    n--;
    if (n > 0) {
      left.textContent = n;
      return;
    }
    clearInterval(timer);
    left.textContent = 0;
    document.getElementById("countdown").textContent =
      "Time is up: the previous settings are coming back. Reload this page in a few seconds.";
  }, 1000);
})();
