import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { instagramEmbedPath, mastodonStatusPathFromAlternate, mediaSelection, parseCanonicalDecimal, validEmbedPath } from "./routes.ts";

const decimalCases = JSON.parse(await readFile(new URL("./decimal-cases.json", import.meta.url), "utf8"));
const mediaCases = JSON.parse(await readFile(new URL("./media-selection-cases.json", import.meta.url), "utf8"));
test("embed accepts only supported Instagram paths", () => {
  for (const path of ["/p/Ab_12", "/reel/Ab_12", "/name/p/Ab_12", "/stories/user.name/1234567890", "/username"]) {
    assert.equal(validEmbedPath(path), true, path);
  }
  for (const path of ["https://evil.test/p/Ab_12", "//evil.test/p/Ab_12", "/\\evil.test/p/Ab_12", "/p/Ab_12?x=1", "/api/embed", "/p/!", "/p/Ab_12/01", "/p/Ab_12/+1", "/p/Ab_12/-1", "/p/Ab_12/1.5", "/p/Ab_12/1oops", "/stories/username/not-a-number", "/bad/path"]) {
    assert.equal(validEmbedPath(path), false, path);
  }
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

test("Discord alternate links resolve only to same-origin Mastodon statuses", () => {
  const origin = "https://oginstagram.com";
  for (const href of [
    "/users/user.name/statuses/123",
    "https://oginstagram.com/users/user.name/statuses/123?cache=2",
  ]) {
    assert.equal(mastodonStatusPathFromAlternate(href, origin), "/api/v1/statuses/123", href);
  }
  for (const href of [
    "https://evil.test/users/user.name/statuses/123",
    "/users/user!/statuses/123",
    "/users/user.name/statuses/not-a-snowcode",
    "/users/user.name/statuses/123/extra",
    `/users/user.name/statuses/${"1".repeat(257)}`,
  ]) {
    assert.equal(mastodonStatusPathFromAlternate(href, origin), null, href);
  }
});

test("browser media selection follows the shared canonical cases", () => {
  for (const { query, pathIndex, selected } of mediaCases) {
    assert.equal(mediaSelection(new URLSearchParams(query), pathIndex), selected, query || `path=${pathIndex}`);
  }
});
