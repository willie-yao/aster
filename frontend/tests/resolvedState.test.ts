import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";
import {
  initialResolvedRead, readResolvedState, resolvedReadState, resolutionAvailability,
  type ResolvedReadEvent,
} from "../src/lib/resolvedState.js";

const known = { resolved: { pattern: { resolved_at: "2026-08-17", resolved_by: "admin", note: "done", watermark: "100" } }, causes: {} };

test("resolution reads start unknown and preserve known data without classifying it during refresh or failure", () => {
  assert.equal(initialResolvedRead.data, null);
  assert.equal(resolutionAvailability(initialResolvedRead), "loading");
  const unavailable = resolvedReadState(initialResolvedRead, { type: "error", error: "offline" });
  assert.equal(unavailable.data, null);
  assert.equal(resolutionAvailability(unavailable), "unavailable");
  const ready = resolvedReadState(initialResolvedRead, { type: "success", data: known });
  assert.equal(resolutionAvailability(ready), "available");
  const refreshing = resolvedReadState(ready, { type: "loading" });
  assert.equal(refreshing.data, known);
  assert.equal(resolutionAvailability(refreshing), "loading");
  const failed = resolvedReadState(refreshing, { type: "error", error: "offline" });
  assert.equal(failed.data, known);
  assert.equal(resolutionAvailability(failed), "unavailable");
  assert.equal(resolutionAvailability(resolvedReadState(failed, { type: "loading" })), "loading");
});

test("404 is a known empty resolution set and omitted causes normalize on success", async () => {
  for (const response of [new Response(null, { status: 404 }), Response.json({ resolved: {} })]) {
    const events: ResolvedReadEvent[] = [];
    await readResolvedState({
      url: "/resolved.json", signal: new AbortController().signal,
      fetcher: async () => response, onResult: (event) => events.push(event),
    });
    assert.deepEqual(events, [{ type: "success", data: { resolved: {}, causes: {} } }]);
  }
});

test("resolution read retries are bounded and exhausted errors never become empty success", async () => {
  let calls = 0;
  const delays: number[] = [];
  const events: ResolvedReadEvent[] = [];
  await readResolvedState({
    url: "/resolved.json", signal: new AbortController().signal,
    fetcher: async () => { calls += 1; return new Response(null, { status: 503 }); },
    wait: async (delay) => { delays.push(delay); }, onResult: (event) => events.push(event),
  });
  assert.equal(calls, 5);
  assert.deepEqual(delays, [500, 1000, 2000, 4000]);
  assert.deepEqual(events, [{ type: "error", error: "resolved.json: 503" }]);
});

test("a transient resolution failure recovers on a later attempt", async () => {
  let calls = 0;
  const events: ResolvedReadEvent[] = [];
  await readResolvedState({
    url: "/resolved.json", signal: new AbortController().signal,
    fetcher: async () => {
      calls += 1;
      if (calls === 1) throw new Error("offline");
      return Response.json(known);
    },
    wait: async () => {}, onResult: (event) => events.push(event),
  });
  assert.equal(calls, 2);
  assert.deepEqual(events, [{ type: "success", data: known }]);
});

test("invalid resolution documents remain unavailable after bounded retries", async () => {
  for (const body of [null, {}, { resolved: [] }, { resolved: {}, causes: [] }, "invalid JSON"]) {
    const events: ResolvedReadEvent[] = [];
    let calls = 0;
    await readResolvedState({
      url: "/resolved.json", signal: new AbortController().signal,
      fetcher: async () => {
        calls += 1;
        return typeof body === "string" ? new Response(body) : Response.json(body);
      },
      wait: async () => {}, onResult: (event) => events.push(event),
    });
    assert.equal(calls, 5);
    assert.equal(events.length, 1);
    assert.equal(events[0].type, "error");
  }
});

test("cancellation during retry backoff stops further attempts", async () => {
  const controller = new AbortController();
  const events: ResolvedReadEvent[] = [];
  let calls = 0;
  let release!: () => void;
  let entered!: () => void;
  const waiting = new Promise<void>((resolve) => { entered = resolve; });
  const reading = readResolvedState({
    url: "/resolved.json", signal: controller.signal,
    fetcher: async () => { calls += 1; return new Response(null, { status: 503 }); },
    wait: async (_delay, signal) => {
      assert.equal(signal, controller.signal);
      entered();
      await new Promise<void>((resolve) => { release = resolve; });
    },
    onResult: (event) => events.push(event),
  });
  await waiting;
  controller.abort();
  release();
  await reading;
  assert.equal(calls, 1);
  assert.deepEqual(events, []);
});

test("an older resolution request cannot restore availability after a replacement read", async () => {
  const old = new AbortController();
  let respond!: (response: Response) => void;
  const response = new Promise<Response>((resolve) => { respond = resolve; });
  let state = initialResolvedRead;
  const onResult = (event: ResolvedReadEvent) => { state = resolvedReadState(state, event); };
  const first = readResolvedState({ url: "/resolved.json", signal: old.signal, fetcher: () => response, onResult });
  old.abort();
  state = resolvedReadState(state, { type: "loading" });
  await readResolvedState({
    url: "/resolved.json", signal: new AbortController().signal,
    fetcher: async () => Response.json(known), onResult,
  });
  respond(new Response(null, { status: 404 }));
  await first;
  assert.equal(state.data?.resolved.pattern.resolved_by, "admin");
  assert.equal(resolutionAvailability(state), "available");
});

test("cancellation suppresses late response bodies and retries", async () => {
  const controller = new AbortController();
  let respond!: (body: unknown) => void;
  const body = new Promise<unknown>((resolve) => { respond = resolve; });
  const events: ResolvedReadEvent[] = [];
  const reading = readResolvedState({
    url: "/resolved.json", signal: controller.signal,
    fetcher: async () => ({ ok: true, status: 200, json: () => body }) as Response,
    onResult: (event) => events.push(event),
  });
  await Promise.resolve();
  controller.abort();
  respond(known);
  await reading;
  assert.deepEqual(events, []);
});

test("useResolved invalidates the active read and marks refresh unknown immediately", () => {
  const source = readFileSync(resolve(process.cwd(), "src/hooks/useData.ts"), "utf8");
  assert.match(source, /useReducer\(resolvedReadState, initialResolvedRead\)/);
  assert.match(source, /activeRead\.current\?\.abort\(\);\s*dispatch\(\{ type: "loading" \}\)/);
  assert.match(source, /return \(\) => controller\.abort\(\)/);
});
