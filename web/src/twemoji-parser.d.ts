declare module "@twemoji/parser" {
  type EmojiEntity = {
    url: string;
    indices: [number, number];
    text: string;
    type: "emoji";
  };

  export function parse(text: string, options?: {
    buildUrl?: (codepoints: string, assetType: "png" | "svg") => string;
  }): EmojiEntity[];
}
