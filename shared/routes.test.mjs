import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { instagramEmbedPath, mediaSelection, parseCanonicalDecimal, validEmbedPath } from "./routes.ts";

const decimalCases = JSON.parse(await readFile(new URL("./decimal-cases.json", import.meta.url), "utf8"));
const mediaCases = JSON.parse(await readFile(new URL("./media-selection-cases.json", import.meta.url), "utf8"));
const routeCases = JSON.parse(await readFile(new URL("./route-cases.json", import.meta.url), "utf8"));
test("embed accepts only supported Instagram paths", () => {
  for (const { path, valid } of routeCases) assert.equal(validEmbedPath(path), valid, path);
  for (const { raw, value } of decimalCases) assert.equal(parseCanonicalDecimal(raw), value, raw);
});

test("live preview preserves Instagram img_index", () => {
  const path = instagramEmbedPath(
    "https://www.instagram.com/p/Ab_12/?img_index=2&utm_source=ig_web_copy_link"
  );
  assert.equal(path, "/p/Ab_12?img_index=2");
  assert.equal(validEmbedPath(path ?? ""), true);
  assert.equal(instagramEmbedPath("https://instagram.com/%09/evil.test/p/Ab_12"), null);
});

test("browser media selection follows the shared canonical cases", () => {
  for (const { query, pathIndex, selected } of mediaCases) {
    assert.equal(mediaSelection(new URLSearchParams(query), pathIndex), selected, query || `path=${pathIndex}`);
  }
});
