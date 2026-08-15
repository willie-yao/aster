import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import vm from "node:vm";

function script(name: string): string {
  return readFileSync(join(process.cwd(), "public", name), "utf8");
}

test("index redirect restores an encoded same-origin deep link", () => {
  const replaced: string[] = [];
  const location = {
    origin: "https://dashboard.example",
    pathname: "/repo/",
    search: "?/job/periodic-capz&run=123~and~attempt=2",
    hash: "#details",
  };
  vm.runInNewContext(script("spa-index-redirect.js"), {
    URL,
    window: {
      location,
      history: {
        replaceState: (_state: unknown, _title: string, path: string) => {
          replaced.push(path);
        },
      },
    },
  });
  assert.deepEqual(replaced, [
    "/repo/job/periodic-capz?run=123&attempt=2#details",
  ]);
});

test("index redirect rejects protocol-relative and backslash targets", () => {
  for (const search of ["?//evil.example/path", "?/\\evil.example/path"]) {
    const replaced: string[] = [];
    vm.runInNewContext(script("spa-index-redirect.js"), {
      URL,
      window: {
        location: {
          origin: "https://dashboard.example",
          pathname: "/",
          search,
          hash: "",
        },
        history: {
          replaceState: (_state: unknown, _title: string, path: string) => {
            replaced.push(path);
          },
        },
      },
    });
    assert.deepEqual(replaced, [], search);
  }
});

test("404 redirect keeps the deep link on the current origin", () => {
  const replaced: string[] = [];
  vm.runInNewContext(script("spa-404-redirect.js"), {
    URL,
    window: {
      location: {
        origin: "https://dashboard.example",
        pathname: "/repo/job/periodic-capz/test/TestCluster",
        search: "?run=123&attempt=2",
        hash: "#details",
        replace: (value: string) => replaced.push(value),
      },
    },
  });
  assert.deepEqual(replaced, [
    "https://dashboard.example/repo/?/job/periodic-capz/test/TestCluster&run=123~and~attempt=2#details",
  ]);
});
