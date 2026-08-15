import assert from "node:assert/strict";
import { test } from "node:test";

import {
  actionRequestPath,
  buildFailurePath,
  jobPath,
  jobRunPath,
  testPath,
  testRunPath,
} from "../src/lib/routes.js";

test("route parameters remain encoded inside same-origin app paths", () => {
  assert.equal(jobPath("//evil.example/path"), "/job/%2F%2Fevil.example%2Fpath");
  assert.equal(jobPath("\\evil.example\\path"), "/job/%5Cevil.example%5Cpath");
  assert.equal(
    testPath("periodic/capz job", "[It] creates / deletes?"),
    "/job/periodic%2Fcapz%20job/test/%5BIt%5D%20creates%20%2F%20deletes%3F",
  );
  assert.equal(
    buildFailurePath("periodic/capz", "build/123"),
    "/job/periodic%2Fcapz/build/build%2F123/failure",
  );
  assert.equal(
    jobRunPath("periodic/capz", "build/123"),
    "/job/periodic%2Fcapz?run=build%2F123",
  );
  assert.equal(
    testRunPath("periodic/capz", "[It] creates", "build/123"),
    "/job/periodic%2Fcapz/test/%5BIt%5D%20creates?run=build%2F123",
  );
  assert.equal(
    actionRequestPath("//request\\id"),
    "/action-request/%2F%2Frequest%5Cid",
  );
});
