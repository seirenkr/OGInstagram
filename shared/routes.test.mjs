import assert from "node:assert/strict";
import test from "node:test";
import { validEmbedPath } from "./routes.ts";

test("embed accepts only supported Instagram paths", () => {
  for (const path of ["/p/Ab_12", "/reel/Ab_12", "/name/p/Ab_12", "/username"]) {
    assert.equal(validEmbedPath(path), true, path);
  }
  for (const path of ["https://evil.test/p/Ab_12", "/p/Ab_12?x=1", "/api/embed", "/p/!", "/bad/path"]) {
    assert.equal(validEmbedPath(path), false, path);
  }
});
