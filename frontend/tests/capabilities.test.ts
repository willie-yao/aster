import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { createElement, type Context } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { probeCapabilities, type CapabilityProbeState } from "../src/lib/capabilities.js";
import type { CapabilityState } from "../src/hooks/useCapabilities.js";
import { STATIC_CAPABILITIES, type Capabilities } from "../src/types/capabilities.js";
import { withSSR } from "./helpers/ssr.js";

const server: Capabilities = {
  mode: "server",
  features: { actions: true, analysis_chat: true },
  auth: { mode: "oauth" },
};

const { CapabilityNotice, CapabilitiesContext } = await withSSR(async (vite) => ({
  ...await vite.ssrLoadModule("/src/components/CapabilitiesProvider.tsx") as {
    CapabilityNotice: () => ReturnType<typeof createElement>;
  },
  ...await vite.ssrLoadModule("/src/hooks/useCapabilities.ts") as {
    CapabilitiesContext: Context<CapabilityState>;
  },
}));

test("static missing endpoints and HTML fallbacks stop discovery without retries", async () => {
  for (const response of [
    new Response("Not found", { status: 404 }),
    new Response("<!doctype html><html></html>", { headers: { "Content-Type": "text/html; charset=utf-8" } }),
    Response.json(STATIC_CAPABILITIES),
  ]) {
    const states: CapabilityProbeState[] = [];
    let calls = 0;
    await probeCapabilities({
      url: "/project/api/capabilities",
      signal: new AbortController().signal,
      fetcher: async (url, init) => {
        calls += 1;
        assert.equal(url, "/project/api/capabilities");
        assert.equal(init?.credentials, "same-origin");
        assert.equal(init?.cache, "no-store");
        return response;
      },
      wait: async () => { assert.fail("static endpoints must not retry"); },
      onResult: (state) => states.push(state),
    });
    assert.equal(calls, 1);
    assert.deepEqual(states, [STATIC_CAPABILITIES]);
  }
});

test("network and 5xx failures recover with a bounded sequential retry", async () => {
  const states: CapabilityProbeState[] = [];
  const delays: number[] = [];
  let calls = 0;
  await probeCapabilities({
    url: "/api/capabilities",
    signal: new AbortController().signal,
    fetcher: async () => {
      calls += 1;
      if (calls === 1) throw new TypeError("Network unavailable");
      if (calls === 2) return new Response("<html>Unavailable</html>", { status: 503, headers: { "Content-Type": "text/html" } });
      return Response.json(server);
    },
    wait: async (delay) => { delays.push(delay); },
    onResult: (state) => states.push(state),
  });
  assert.equal(calls, 3);
  assert.deepEqual(delays, [500, 1000]);
  assert.deepEqual(states, [server]);
});

test("exhausted discovery is unavailable, and an explicit new probe can recover", async () => {
  const states: CapabilityProbeState[] = [];
  let calls = 0;
  let waits = 0;
  const onResult = (state: CapabilityProbeState) => { states.push(state); };
  await probeCapabilities({
    url: "/api/capabilities", signal: new AbortController().signal, onResult,
    fetcher: async () => { calls += 1; return new Response("Down", { status: 502 }); },
    wait: async () => { waits += 1; },
  });
  assert.equal(calls, 3);
  assert.equal(waits, 2);
  assert.deepEqual(states, [{ mode: "unavailable", features: { actions: false }, error: "HTTP 502" }]);
  await probeCapabilities({
    url: "/api/capabilities", signal: new AbortController().signal, onResult,
    fetcher: async () => Response.json(server),
  });
  assert.deepEqual(states.at(-1), server);
});

test("network exhaustion and invalid endpoints never claim static mode", async () => {
  for (const [fetcher, expectedCalls] of [
    [async () => { throw new TypeError("Offline"); }, 3],
    [async () => new Response("Forbidden", { status: 403 }), 1],
    [async () => new Response("{", { headers: { "Content-Type": "application/json" } }), 1],
    [async () => Response.json({ mode: "server" }), 1],
    [async () => Response.json({ mode: "unexpected", features: { actions: true } }), 1],
  ] as const) {
    const states: CapabilityProbeState[] = [];
    let calls = 0;
    await probeCapabilities({
      url: "/api/capabilities", signal: new AbortController().signal,
      fetcher: async () => { calls += 1; return fetcher(); },
      wait: async () => {},
      onResult: (state) => states.push(state),
    });
    assert.equal(states.length, 1);
    assert.equal(states[0].mode, "unavailable");
    assert.equal(states[0].features.actions, false);
    assert.ok(states[0].error);
    assert.equal(calls, expectedCalls);
  }
});

test("each request has a deadline and timeout failures exhaust the same retry budget", async (t) => {
  const deadlines: number[] = [];
  t.mock.method(AbortSignal, "timeout", (delay: number) => {
    deadlines.push(delay);
    return AbortSignal.abort(new DOMException("Request timed out", "TimeoutError"));
  });
  const states: CapabilityProbeState[] = [];
  await probeCapabilities({
    url: "/api/capabilities", signal: new AbortController().signal,
    fetcher: async (_url, init) => {
      assert.ok(init?.signal?.aborted);
      throw init.signal.reason;
    },
    wait: async () => {},
    onResult: (state) => states.push(state),
  });
  assert.deepEqual(deadlines, [5000, 5000, 5000]);
  assert.deepEqual(states, [{ mode: "unavailable", features: { actions: false }, error: "Request timed out" }]);
});

test("cancelled probes ignore late responses, body reads, and errors after replacement", async () => {
  for (const late of ["response", "body", "error"] as const) {
    const controller = new AbortController();
    const states: CapabilityProbeState[] = [];
    let complete!: () => void;
    let started!: () => void;
    let requestSignal: AbortSignal | undefined;
    const ready = new Promise<void>((resolve) => { started = resolve; });
    const pending = new Promise<void>((resolve) => { complete = resolve; });
    const old = probeCapabilities({
      url: "/api/capabilities", signal: controller.signal,
      fetcher: async (_url, init) => {
        assert.ok(init?.signal);
        requestSignal = init.signal;
        if (late === "body") {
          const response = Response.json(server);
          response.json = async () => { started(); await pending; return server; };
          return response;
        }
        started();
        await pending;
        if (late === "error") throw new TypeError("Old failure");
        return Response.json(server);
      },
      onResult: (state) => states.push(state),
    });
    await ready;
    controller.abort();
    assert.equal(requestSignal?.aborted, true);
    await probeCapabilities({
      url: "/api/capabilities", signal: new AbortController().signal,
      fetcher: async () => Response.json(STATIC_CAPABILITIES),
      onResult: (state) => states.push(state),
    });
    complete();
    await old;
    assert.deepEqual(states, [STATIC_CAPABILITIES], late);
  }
});

test("cancellation during backoff stops the next request", async () => {
  const controller = new AbortController();
  let calls = 0;
  await probeCapabilities({
    url: "/api/capabilities", signal: controller.signal,
    fetcher: async () => { calls += 1; return new Response("Down", { status: 500 }); },
    wait: async () => { controller.abort(); },
    onResult: () => assert.fail("cancelled probe must not publish"),
  });
  assert.equal(calls, 1);
});

test("capability notices distinguish pending and unavailable without blocking page content", () => {
  for (const mode of ["pending", "unavailable", "server", "static"] as const) {
    const html = renderToStaticMarkup(createElement(CapabilitiesContext.Provider, {
      value: { mode, features: { actions: false }, error: "HTTP 503", retry: () => {} },
    }, createElement(CapabilityNotice)));
    if (mode === "pending") {
      assert.match(html, /role="status"/);
      assert.match(html, /Checking server capabilities/);
      assert.doesNotMatch(html, />Retry</);
    } else if (mode === "unavailable") {
      assert.match(html, /role="alert"/);
      assert.match(html, /HTTP 503/);
      assert.match(html, /Server-only features remain disabled/);
      assert.match(html, /<button[^>]*>Retry</);
    } else {
      assert.equal(html, "");
    }
  }
  const provider = readFileSync("src/components/CapabilitiesProvider.tsx", "utf8");
  assert.match(provider, /return \(\) => controller\.abort\(\)/);
  assert.match(provider, /function retry\(\) \{\s*setCapabilities\(\{ mode: "pending", features: \{ actions: false \} \}\);\s*setAttempt/);
  const layout = readFileSync("src/components/Layout.tsx", "utf8");
  assert.equal(layout.match(/<CapabilityNotice \/>/g)?.length, 1);
});
