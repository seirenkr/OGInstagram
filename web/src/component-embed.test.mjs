import assert from "node:assert/strict";
import test from "node:test";
import { formatTimestamp, galleryRows, offloadPoster, parseComponentEmbed, parseMarkdown } from "./component-embed.ts";

const media = (path) => ({ media: { url: `https://oginstagram.com/offload/${path}?v=2&kid=k1&exp=1&sig=abc` } });
const payload = (...components) => JSON.stringify({ component: { type: 17, accent_color: 0xff0069, components } });

test("parses the post layout the server emits", () => {
  const embed = parseComponentEmbed(payload(
    { type: 9, components: [{ type: 10, content: "**Name** <:verified:1556598045670514798>\n[@name](https://www.instagram.com/name/)\n\n❤️ **1,234**" }],
      accessory: { type: 11, media: media("Ab/avatar").media } },
    { type: 10, content: "caption" },
    { type: 12, items: [media("Ab/1"), media("Ab/2")] },
    { type: 9, components: [{ type: 10, content: "-# <:OGInstagram:1556597080229810266> OGInstagram · <t:1792079955:s>" }],
      accessory: { type: 2, style: 5, label: "📷 Instagram", url: "https://www.instagram.com/p/Ab/" } },
    { type: 14 },
    { type: 1, components: [{ type: 2, style: 5, label: "☕ Support me", url: "https://ko-fi.com/seirenkr" }] },
  ), (url) => url.replace("https://oginstagram.com", "http://localhost:8080"));
  assert.equal(embed.accent, "#ff0069");
  assert.deepEqual(embed.children.map((c) => c.type), ["section", "text", "gallery", "section", "separator", "row"]);
  const [header, , gallery, footer, separator, row] = embed.children;
  assert.deepEqual(header.texts[0], [
    { tag: "b", children: ["Name"] }, " ", { tag: "emoji", name: "verified" }, "\n",
    { tag: "a", href: "https://www.instagram.com/name/", children: ["@name"] }, "\n\n❤️ ", { tag: "b", children: ["1,234"] },
  ]);
  assert.equal(header.thumbnail.url, "http://localhost:8080/offload/Ab/avatar?v=2&kid=k1&exp=1&sig=abc");
  assert.equal(gallery.items.length, 2);
  assert.deepEqual(footer.texts[0], [{ tag: "sub", children: [
    { tag: "emoji", name: "OGInstagram" }, " OGInstagram · ", { tag: "time", unix: 1792079955, style: "s" },
  ] }]);
  assert.deepEqual(footer.button, { label: "📷 Instagram", href: "https://www.instagram.com/p/Ab/" });
  assert.deepEqual(separator, { type: "separator", divider: true, large: false });
  assert.equal(row.buttons[0].href, "https://ko-fi.com/seirenkr");
});

test("payloads Discord would reject fall back to Open Graph", () => {
  const text = { type: 10, content: "x" };
  const rejected = {
    "invalid JSON": "{",
    "no container": JSON.stringify({ component: text }),
    "empty container": payload(),
    "nested container": payload({ type: 17, components: [text] }),
    "unknown component": payload({ type: 13, file: { url: "attachment://a.zip" } }),
    "over 3000 bytes": payload({ type: 10, content: "x".repeat(3000) }),
    "over 40 components": payload(...Array.from({ length: 40 }, () => text)),
    "over 10 gallery items": payload({ type: 12, items: Array.from({ length: 6 }, (_, i) => media(`a/${i}`)) },
      { type: 12, items: Array.from({ length: 5 }, (_, i) => media(`b/${i}`)) }),
    "non-link button": payload({ type: 1, components: [{ type: 2, style: 1, label: "x", custom_id: "x" }] }),
    "extra button key": payload({ type: 1, components: [{ type: 2, style: 5, label: "x", url: "https://a.example/", sku_id: "1" }] }),
    "javascript: button": payload({ type: 1, components: [{ type: 2, style: 5, label: "x", url: "javascript:alert(1)" }] }),
    "data: media": payload({ type: 12, items: [{ media: { url: "data:image/png;base64,AA" } }] }),
    "section without accessory": payload({ type: 9, components: [text] }),
  };
  for (const [name, json] of Object.entries(rejected)) assert.equal(parseComponentEmbed(json), null, name);
  assert.ok(parseComponentEmbed(payload(...Array.from({ length: 39 }, () => text))), "39 children + container is the limit");
});

test("markdown follows Discord's rules for what our builders emit", () => {
  assert.deepEqual(parseMarkdown("\\# not a heading\n1\\. item \\*x\\* https\\://a.example @​everyone"),
    ["# not a heading\n1. item *x* https://a.example @​everyone"]);
  assert.deepEqual(parseMarkdown("# H1\n## H2\n### H3\n#### no\n-# small"), [
    { tag: "h1", children: ["H1"] }, { tag: "h2", children: ["H2"] }, { tag: "h3", children: ["H3"] }, "#### no\n", { tag: "sub", children: ["small"] },
  ]);
  assert.deepEqual(parseMarkdown("*i* _i_ __u__ ~~s~~ snake_case_name"), [
    { tag: "i", children: ["i"] }, " ", { tag: "i", children: ["i"] }, " ", { tag: "u", children: ["u"] }, " ", { tag: "s", children: ["s"] }, " snake_case_name",
  ]);
  assert.deepEqual(parseMarkdown("<https://a.example/x> and https://b.example/y."), [
    { tag: "a", href: "https://a.example/x", children: ["https://a.example/x"] }, " and ",
    { tag: "a", href: "https://b.example/y", children: ["https://b.example/y"] }, ".",
  ]);
  // Labels keep backslashes and formatting, but cannot hold links.
  assert.deepEqual(parseMarkdown("[a\\*b **c**](https://a.example/%28x%29)"), [
    { tag: "a", href: "https://a.example/%28x%29", children: ["a\\*b ", { tag: "b", children: ["c"] }] },
  ]);
  assert.deepEqual(parseMarkdown("[x](javascript:alert(1)) <t:99999999999999:s>"), ["[x](javascript:alert(1)) <t:99999999999999:s>"]);
});

test("gallery rows, video posters and timestamps", () => {
  assert.deepEqual([1, 2, 4, 5, 7, 10].map((n) => galleryRows(Array.from({ length: n })).map((row) => row.length)),
    [[1], [2], [2, 2], [2, 3], [1, 3, 3], [1, 3, 3, 3]]);
  const signed = "http://localhost:8080/offload/Ab/1?v=2&kid=k1&exp=1&sig=abc";
  assert.equal(offloadPoster(signed, "http://localhost:8080"), "http://localhost:8080/offload/Ab/1?thumbnail=1&v=2&kid=k1&exp=1&sig=abc");
  assert.equal(offloadPoster(offloadPoster(signed, "http://localhost:8080"), "http://localhost:8080"), undefined);
  assert.equal(offloadPoster(signed, "https://oginstagram.com"), undefined);
  assert.equal(formatTimestamp(1792079955, "s", "en"),
    new Intl.DateTimeFormat("en", { dateStyle: "short", timeStyle: "short" }).format(1792079955000));
});
