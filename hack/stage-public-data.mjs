#!/usr/bin/env node
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const rootFiles = JSON.parse(fs.readFileSync(
  new URL("../backend/internal/output/public_paths.json", import.meta.url), "utf8",
));

// Mirror output.IsPublicDataPath against the shared contract and fixtures.
export function isPublicDataPath(name) {
  const parts = name.split("/");
  if (parts.length === 1) return rootFiles.includes(name);
  if (parts.length !== 2 || !parts[1].endsWith(".json")) return false;
  const stem = parts[1].slice(0, -5);
  if (parts[0] === "jobs") {
    return /^[A-Za-z0-9_-]+$/.test(stem)
      && Buffer.from(stem, "base64url").toString("base64url") === stem;
  }
  if (parts[0] === "pull-requests") {
    return /^[1-9][0-9]*$/.test(stem) && BigInt(stem).toString() === stem
      && BigInt(stem) <= 9223372036854775807n;
  }
  return false;
}

function copyRegularFile(source, target) {
  const before = fs.lstatSync(source);
  if (!before.isFile()) return;
  const fd = fs.openSync(source, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
  try {
    const opened = fs.fstatSync(fd);
    if (!opened.isFile() || opened.dev !== before.dev || opened.ino !== before.ino) {
      throw new Error("Public data file changed while staging: " + source);
    }
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, fs.readFileSync(fd), { flag: "wx", mode: 0o644 });
  } finally {
    fs.closeSync(fd);
  }
}

// Replace the destination only after a complete public tree has been staged.
export function stagePublicData(source, destination) {
  source = path.resolve(source);
  destination = path.resolve(destination);
  const info = fs.lstatSync(source);
  if (!info.isDirectory() || info.isSymbolicLink()) {
    throw new Error("Public data source must be a regular directory: " + source);
  }
  if (source !== destination) {
    for (const relative of [path.relative(source, destination), path.relative(destination, source)]) {
      if (relative !== ".." && !relative.startsWith(".." + path.sep) && !path.isAbsolute(relative)) {
        throw new Error("Public data source and destination must not contain each other");
      }
    }
  }
  fs.mkdirSync(path.dirname(destination), { recursive: true });
  const staging = fs.mkdtempSync(path.join(path.dirname(destination), ".aster-public-data-"));
  try {
    for (const entry of fs.readdirSync(source, { withFileTypes: true })) {
      if (entry.isFile() && isPublicDataPath(entry.name)) {
        copyRegularFile(path.join(source, entry.name), path.join(staging, entry.name));
      } else if (entry.isDirectory() && ["jobs", "pull-requests"].includes(entry.name)) {
        for (const child of fs.readdirSync(path.join(source, entry.name), { withFileTypes: true })) {
          const relative = entry.name + "/" + child.name;
          if (child.isFile() && isPublicDataPath(relative)) {
            copyRegularFile(path.join(source, relative), path.join(staging, relative));
          }
        }
      }
    }
    fs.rmSync(destination, { recursive: true, force: true });
    fs.renameSync(staging, destination);
  } finally {
    fs.rmSync(staging, { recursive: true, force: true });
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  if (process.argv.length !== 4) {
    throw new Error("Usage: node hack/stage-public-data.mjs <source> <destination>");
  }
  stagePublicData(process.argv[2], process.argv[3]);
}
