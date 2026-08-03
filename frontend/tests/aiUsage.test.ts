import assert from "node:assert/strict"; import { test } from "node:test";
import { formatCost, formatTokens, totalTokens, usageQuery } from "../src/lib/aiUsage.js";
test("AI usage helpers format values and filters",()=>{ assert.equal(formatTokens(1200),"1,200"); assert.match(formatCost("1250000","USD"),/^\$0\.00125/); assert.equal(totalTokens({input_tokens:2,output_tokens:3} as never),5); assert.equal(usageQuery("2026-08-01","2026-08-03","analysis_chat"),"start=2026-08-01&end=2026-08-03&feature=analysis_chat"); });
