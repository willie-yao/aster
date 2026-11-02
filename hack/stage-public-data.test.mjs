import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import test from "node:test";
import { isPublicDataPath, stagePublicData } from "./stage-public-data.mjs";

const fixtures = JSON.parse(fs.readFileSync(
  new URL("../backend/internal/output/testdata/public_paths.json", import.meta.url), "utf8",
));

function temporary(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "aster-public-data-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function write(dir, name, content = name) {
  const target = path.join(dir, name);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, content);
}

function files(dir, prefix = "") {
  return fs.readdirSync(path.join(dir, prefix), { withFileTypes: true }).flatMap(entry => {
    const relative = prefix ? prefix + "/" + entry.name : entry.name;
    return entry.isDirectory() ? files(dir, relative) : [relative];
  }).sort();
}

test("shared public path contract", async t => {
  for (const fixture of fixtures) {
    await t.test(fixture.name, () => {
      assert.equal(isPublicDataPath(fixture.path), fixture.public);
    });
  }
});

test("staging replaces dirty output and preserves the private source", t => {
  const dir = temporary(t);
  const source = path.join(dir, "source");
  const destination = path.join(dir, "destination");
  const accepted = fixtures.filter(fixture => fixture.public).map(fixture => fixture.path);
  for (const name of accepted) write(source, name);
  for (const name of [
    "private_state.json", "issue_state.json.lock", "dashboard.json.tmp-123",
    ".fetch-status/status.json", "jobs/nested/Zg.json", "jobs/Zh.json",
    "jobs/Zg.json.lock", "pull-requests/0123.json",
  ]) write(source, name, "private");
  write(destination, "private_state.json", "stale private");
  write(destination, "jobs/b2xk.json", "stale public");
  stagePublicData(source, destination);
  assert.deepEqual(files(destination), accepted.sort());
  for (const name of accepted) assert.equal(fs.readFileSync(path.join(destination, name), "utf8"), name);
  assert.equal(fs.readFileSync(path.join(source, "private_state.json"), "utf8"), "private");
  // Retained PR details are public even when no current index references them.
  assert.equal(fs.readFileSync(path.join(destination, "pull-requests/123.json"), "utf8"), "pull-requests/123.json");
});

test("in-place cleaning precedes provenance and final output replacement", t => {
  const dir = temporary(t);
  const source = path.join(dir, "public", "data");
  const destination = path.join(dir, "dist", "data");
  write(source, "manifest.json", "{}");
  write(source, "dashboard.json", "{}");
  write(source, "issue_state.json", "private");
  stagePublicData(source, source);
  assert.deepEqual(files(source), ["dashboard.json", "manifest.json"]);
  write(source, "provenance.json", '{"engine_commit":"published"}');
  write(destination, "unknown.json", "stale");
  stagePublicData(source, destination);
  assert.deepEqual(files(destination), ["dashboard.json", "manifest.json", "provenance.json"]);
  assert.equal(fs.readFileSync(path.join(destination, "provenance.json"), "utf8"), '{"engine_commit":"published"}');
});

test("staging omits symlink files, symlink directories, and nonregular inputs", t => {
  const dir = temporary(t);
  const source = path.join(dir, "source");
  const destination = path.join(dir, "destination");
  const privateDir = path.join(dir, "private");
  write(source, "dashboard.json", "public");
  write(privateDir, "private.json", "private");
  write(privateDir, "Zg.json", "private");
  write(privateDir, "123.json", "private");
  fs.symlinkSync(path.join(privateDir, "private.json"), path.join(source, "manifest.json"));
  fs.symlinkSync(privateDir, path.join(source, "jobs"));
  fs.mkdirSync(path.join(source, "pull-requests"));
  fs.symlinkSync(path.join(privateDir, "private.json"), path.join(source, "pull-requests", "123.json"));
  fs.mkdirSync(path.join(source, "resolved.json"));
  stagePublicData(source, destination);
  assert.deepEqual(files(destination), ["dashboard.json"]);

  const link = path.join(dir, "source-link");
  fs.symlinkSync(source, link);
  assert.throws(() => stagePublicData(link, destination), /regular directory/);
  assert.deepEqual(files(destination), ["dashboard.json"]);
});

test("missing and non-directory sources preserve the destination", t => {
  const dir = temporary(t);
  const destination = path.join(dir, "destination");
  write(destination, "dashboard.json", "previous");
  assert.throws(() => stagePublicData(path.join(dir, "missing"), destination), /ENOENT/);
  write(dir, "source-file", "not a directory");
  assert.throws(() => stagePublicData(path.join(dir, "source-file"), destination), /regular directory/);
  assert.equal(fs.readFileSync(path.join(destination, "dashboard.json"), "utf8"), "previous");
});

test("replacement removes destination symlinks without touching their targets", t => {
  const dir = temporary(t);
  const source = path.join(dir, "source");
  const outside = path.join(dir, "outside");
  const destination = path.join(dir, "destination");
  write(source, "dashboard.json", "public");
  write(outside, "private.json", "private");
  fs.symlinkSync(outside, destination);
  stagePublicData(source, destination);
  assert.deepEqual(files(destination), ["dashboard.json"]);
  assert.equal(fs.readFileSync(path.join(outside, "private.json"), "utf8"), "private");
});

test("overlapping source and destination paths are rejected", t => {
  const source = temporary(t);
  write(source, "dashboard.json", "public");
  assert.throws(() => stagePublicData(source, path.join(source, "data")), /contain each other/);
  fs.mkdirSync(path.join(source, "data"));
  assert.throws(() => stagePublicData(path.join(source, "data"), source), /contain each other/);
  assert.equal(fs.readFileSync(path.join(source, "dashboard.json"), "utf8"), "public");
});
