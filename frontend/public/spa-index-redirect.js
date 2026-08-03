(function restoreGitHubPagesRoute(location, history) {
  if (location.search.length < 2 || location.search[1] !== "/") return;

  var decoded = location.search
    .slice(1)
    .split("&")
    .map(function decodeAmpersand(value) {
      return value.replace(/~and~/g, "&");
    })
    .join("?");
  var candidate = location.pathname.slice(0, -1) + decoded + location.hash;
  var resolved = new URL(candidate, location.origin);
  if (resolved.origin !== location.origin) return;

  history.replaceState(
    null,
    "",
    resolved.pathname + resolved.search + resolved.hash,
  );
})(window.location, window.history);
