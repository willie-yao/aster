import assert from "node:assert/strict";
import { test } from "node:test";

import { searchIndexPath } from "../src/lib/search.js";

test("search index stays disabled until search activation", () => {
  assert.equal(searchIndexPath(false), null);
  assert.equal(searchIndexPath(true), "search-index.json");
});
