const signatureVersion = "2";
const keyIdPattern = /^[A-Za-z0-9_-]{1,16}$/;
const signaturePattern = /^[A-Za-z0-9_-]{43}$/;
// Canonical decimal, matching Go's parseCanonicalDecimal: a leading-zero
// variant of the same number is a different signed byte string, so it must
// be rejected before HMAC verification rather than relying on the mismatch.
const expiryPattern = /^(?:0|[1-9]\d{0,14})$/;
const keyBytes = 32;
const offloadTtlSeconds = 14 * 24 * 60 * 60;

type Keyring = {
  keys: Map<string, CryptoKey>;
};

let cachedConfig = "";
let cachedKeyring: Promise<Keyring> | undefined;

// Signed capabilities are verified in the uncached gateway before the cached
// Embed entrypoint is called. This keeps authorization ahead of cache lookup.
export async function authorizeOffload(
  url: URL,
  canonicalPath: string,
  rawConfig: string | undefined
): Promise<boolean> {
  const versions = url.searchParams.getAll("v");
  const keyIds = url.searchParams.getAll("kid");
  const expirations = url.searchParams.getAll("exp");
  const signatures = url.searchParams.getAll("sig");
  if (versions.length !== 1 || keyIds.length !== 1 || expirations.length !== 1 || signatures.length !== 1) {
    return false;
  }

  const version = versions[0];
  const keyId = keyIds[0];
  const signature = signatures[0];
  const expiresRaw = expirations[0];
  if (version !== signatureVersion || !keyIdPattern.test(keyId) || !signaturePattern.test(signature)
    || !expiryPattern.test(expiresRaw)) {
    return false;
  }
  const expires = Number(expiresRaw);
  const now = Math.floor(Date.now() / 1000);
  if (!Number.isSafeInteger(expires) || expires <= now || expires > now + offloadTtlSeconds) {
    return false;
  }

  const thumbnails = url.searchParams.getAll("thumbnail");
  if (thumbnails.length > 1 || (thumbnails.length === 1 && thumbnails[0] !== "1")) {
    return false;
  }
  const previews = url.searchParams.getAll("preview");
  if (previews.length > 1
    || (previews.length === 1 && previews[0] !== "1" && previews[0] !== "avatar")) {
    return false;
  }

  const signatureBytes = decodeBase64URL(signature);
  if (!signatureBytes) return false;
  const key = (await signingKeyring(rawConfig)).keys.get(keyId);
  if (!key) return false;
  return crypto.subtle.verify(
    "HMAC",
    key,
    signatureBytes,
    new TextEncoder().encode(signatureInput(keyId, canonicalPath, expires))
  );
}

export function signatureInput(keyId: string, canonicalPath: string, expires: number): string {
  return "oginstagram-offload-capability\n"
    + `v=${signatureVersion}\n`
    + `kid=${keyId}\n`
    + `exp=${expires}\n`
    + `path=${canonicalPath}\n`;
}

async function signingKeyring(rawConfig: string | undefined): Promise<Keyring> {
  if (!rawConfig) throw new Error("OFFLOAD_SIGNING_KEYS is not configured");
  if (rawConfig !== cachedConfig || !cachedKeyring) {
    cachedConfig = rawConfig;
    cachedKeyring = importKeyring(rawConfig);
  }
  return cachedKeyring;
}

async function importKeyring(rawConfig: string): Promise<Keyring> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawConfig);
  } catch {
    throw new Error("OFFLOAD_SIGNING_KEYS is not valid JSON");
  }
  if (!isRecord(parsed)
    || Object.keys(parsed).length !== 2
    || !("active" in parsed) || !("keys" in parsed)
    || typeof parsed.active !== "string"
    || !keyIdPattern.test(parsed.active)
    || !isRecord(parsed.keys)
    || Object.keys(parsed.keys).length === 0
    || !Object.hasOwn(parsed.keys, parsed.active)) {
    throw new Error("OFFLOAD_SIGNING_KEYS has an invalid keyring");
  }

  const entries = await Promise.all(Object.entries(parsed.keys).map(async ([keyId, encoded]) => {
    if (!keyIdPattern.test(keyId) || typeof encoded !== "string") {
      throw new Error("OFFLOAD_SIGNING_KEYS has an invalid key");
    }
    const raw = decodeBase64URL(encoded);
    if (!raw || raw.byteLength !== keyBytes) {
      throw new Error("OFFLOAD_SIGNING_KEYS keys must be base64url-encoded 32-byte values");
    }
    const key = await crypto.subtle.importKey(
      "raw",
      raw,
      { name: "HMAC", hash: "SHA-256" },
      false,
      ["verify"]
    );
    return [keyId, key] as const;
  }));
  return { keys: new Map(entries) };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function decodeBase64URL(value: string): Uint8Array<ArrayBuffer> | null {
  if (!/^[A-Za-z0-9_-]+$/.test(value)) return null;
  const padding = "=".repeat((4 - value.length % 4) % 4);
  try {
    const binary = atob(value.replaceAll("-", "+").replaceAll("_", "/") + padding);
    const decoded = Uint8Array.from(binary, char => char.charCodeAt(0));
    return encodeBase64URL(decoded) === value ? decoded : null;
  } catch {
    return null;
  }
}

function encodeBase64URL(value: Uint8Array): string {
  let binary = "";
  for (const byte of value) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}
