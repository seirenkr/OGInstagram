import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { asHomeLocale, mediaSelection, parseCanonicalDecimal, resolveHomeLocale, validEmbedPath } from "./routes.ts";

const decimalCases = JSON.parse(await readFile(new URL("./decimal-cases.json", import.meta.url), "utf8"));
const mediaCases = JSON.parse(await readFile(new URL("./media-selection-cases.json", import.meta.url), "utf8"));

test("embed accepts only supported Instagram paths", () => {
  for (const path of ["/p/Ab_12", "/reel/Ab_12", "/name/p/Ab_12", "/stories/user.name/1234567890", "/username"]) {
    assert.equal(validEmbedPath(path), true, path);
  }
  for (const path of ["https://evil.test/p/Ab_12", "/p/Ab_12?x=1", "/api/embed", "/p/!", "/p/Ab_12/01", "/p/Ab_12/+1", "/p/Ab_12/-1", "/p/Ab_12/1.5", "/p/Ab_12/1oops", "/stories/username/not-a-number", "/bad/path"]) {
    assert.equal(validEmbedPath(path), false, path);
  }
  for (const { raw, value } of decimalCases) assert.equal(parseCanonicalDecimal(raw), value, raw);
});

test("worker media cache selection follows the shared canonical cases", () => {
  for (const { query, pathIndex, selected } of mediaCases) {
    assert.equal(mediaSelection(new URLSearchParams(query), pathIndex), selected, query || `path=${pathIndex}`);
  }
});

test("home locale negotiation", () => {
  const cases = [
    ["ko-KR,ko;q=0.9,en-US;q=0.8", "ko"],
    ["zh-TW", "zh-hant"],
    ["zh-Hant-HK", "zh-hant"],
    ["zh-HK", "zh-hant"],
    ["zh-MO", "zh-hant"],
    ["zh-CN", "zh-hans"],
    ["zh-Hans-SG", "zh-hans"],
    ["zh", "zh-hans"],
    ["pt-BR,pt;q=0.9", "pt"],
    ["fr-FR,fr;q=0.9", "fr"],
    ["es-419", "es"],
    ["en;q=0, ko;q=1", "ko"],
    ["fr;q=0.2, ja;q=0.8", "ja"],
    ["ko;q=0, *;q=1", "en"],
    ["ko;q=bogus, en;q=0.5", "en"],
    ["de-DE,it;q=0.8", "en"],
    ["", "en"],
  ];
  for (const [accept, want] of cases) {
    assert.equal(resolveHomeLocale(accept), want, accept);
  }
  assert.equal(asHomeLocale("zh-hant"), "zh-hant");
  assert.equal(asHomeLocale("nope"), null);
});
