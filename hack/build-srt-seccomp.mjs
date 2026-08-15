#!/usr/bin/env node
import {spawnSync} from "node:child_process";
import {chmodSync, mkdirSync, readFileSync, rmSync, writeFileSync} from "node:fs";
import path from "node:path";

function fail(message) {
  console.error(message);
  process.exit(1);
}

function run(command, args) {
  const result = spawnSync(command, args, {stdio: "inherit"});
  if (result.error) fail(`${command} failed to start: ${result.error.message}`);
  if (result.status !== 0) fail(`${command} exited ${result.status ?? result.signal}`);
}

if (process.platform !== "linux") fail(`seccomp helper build requires linux, got ${process.platform}`);
const archDir = {x64: "x64", arm64: "arm64"}[process.arch];
if (!archDir) fail(`unsupported seccomp helper architecture: ${process.arch}`);
const sourceRoot = process.argv[2] ? path.resolve(process.argv[2]) : "";
if (!sourceRoot) fail("usage: build-srt-seccomp.mjs /absolute/sandbox-runtime-source");

const sourceDir = path.join(sourceRoot, "vendor", "seccomp-src");
const outputDir = path.join(sourceRoot, "vendor", "seccomp", archDir);
mkdirSync(outputDir, {recursive: true});
const generator = path.join(outputDir, "seccomp-unix-block");
const header = path.join(outputDir, "unix-block-bpf.h");
const helper = path.join(outputDir, "apply-seccomp");
const temporary = [];

function toCArray(bytes) {
  const hex = Array.from(bytes, byte => `0x${byte.toString(16).padStart(2, "0")}`);
  const lines = [];
  for (let index = 0; index < hex.length; index += 8) {
    lines.push(`    ${hex.slice(index, index + 8).join(", ")},`);
  }
  return lines.join("\n");
}

try {
  const cflags = ["-static", "-O2", "-Wall", "-Wextra"];
  run("gcc", [...cflags, "-o", generator, path.join(sourceDir, "seccomp-unix-block.c"), "-lseccomp"]);
  temporary.push(generator);

  const bpf = {};
  for (const target of ["x86_64", "aarch64"]) {
    const output = path.join(outputDir, `${target}.bpf`);
    run(generator, [output, target]);
    temporary.push(output);
    bpf[target] = readFileSync(output);
  }

  writeFileSync(
    header,
    "#if defined(__x86_64__)\n" +
      "static const unsigned char unix_block_bpf[] = {\n" +
      toCArray(bpf.x86_64) +
      "\n};\n" +
      "#elif defined(__aarch64__)\n" +
      "static const unsigned char unix_block_bpf[] = {\n" +
      toCArray(bpf.aarch64) +
      "\n};\n" +
      "#else\n" +
      '#error "unsupported architecture for unix-block BPF filter"\n' +
      "#endif\n",
  );
  temporary.push(header);

  run("gcc", [...cflags, "-I", outputDir, "-o", helper, path.join(sourceDir, "apply-seccomp.c")]);
  run("strip", [helper]);
  chmodSync(helper, 0o755);
  console.log(helper);
} finally {
  for (const candidate of temporary) rmSync(candidate, {force: true});
}
