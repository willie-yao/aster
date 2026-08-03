(function redirectGitHubPagesRoute(location) {
  var pathSegmentsToKeep = 1;
  var basePath = location.pathname
    .split("/")
    .slice(0, 1 + pathSegmentsToKeep)
    .join("/");
  var routePath = location.pathname
    .slice(1)
    .split("/")
    .slice(pathSegmentsToKeep)
    .join("/")
    .replace(/&/g, "~and~");
  var query = location.search
    ? "&" + location.search.slice(1).replace(/&/g, "~and~")
    : "";
  var destination = new URL(
    basePath + "/?/" + routePath + query + location.hash,
    location.origin,
  );
  if (destination.origin !== location.origin) return;
  location.replace(destination.href);
})(window.location);
