const dummyTurnstileSecret = "1x0000000000000000000000000000000AA";

type TurnstileResult = {
  success?: boolean;
  action?: string;
  hostname?: string;
  "error-codes"?: unknown;
};

export type TurnstileVerification =
  | { outcome: "valid" }
  | { outcome: "rejected" }
  | { outcome: "unavailable"; errorType: string; message: string; httpStatus?: number };

export async function verifyTurnstile(
  token: string,
  request: Request,
  env: Env,
  url: URL,
  fetcher: typeof fetch = fetch
): Promise<TurnstileVerification> {
  if (!env.TURNSTILE_SECRET_KEY) {
    return { outcome: "unavailable", errorType: "invalid_configuration", message: "Turnstile secret is not configured" };
  }
  if (!token || token.length > 2048) return { outcome: "rejected" };

  const form = new URLSearchParams();
  form.set("secret", env.TURNSTILE_SECRET_KEY);
  form.set("response", token);
  const ip = request.headers.get("cf-connecting-ip");
  if (ip) form.set("remoteip", ip);
  // Siteverify tokens are single-use; one idempotency key makes a retry safe.
  form.set("idempotency_key", crypto.randomUUID());

  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      const response = await fetcher("https://challenges.cloudflare.com/turnstile/v0/siteverify", {
        method: "POST",
        body: form,
        signal: AbortSignal.timeout(5000),
      });
      if (!response.ok) {
        if (response.status >= 500 && attempt === 0) continue;
        return {
          outcome: "unavailable",
          errorType: "turnstile_http_error",
          message: `Turnstile Siteverify returned HTTP ${response.status}`,
          httpStatus: response.status,
        };
      }

      const result = await response.json() as TurnstileResult;
      const errorCodes = Array.isArray(result["error-codes"])
        ? result["error-codes"].filter((value): value is string => typeof value === "string")
        : [];
      if (errorCodes.includes("internal-error")) {
        if (attempt === 0) continue;
        return {
          outcome: "unavailable",
          errorType: "turnstile_internal_error",
          message: "Turnstile Siteverify reported an internal error",
        };
      }

      const testing = env.TURNSTILE_SECRET_KEY === dummyTurnstileSecret
        && (url.hostname === "localhost" || url.hostname === "127.0.0.1");
      const valid = result.success === true
        && (testing || (result.action === "turnstile-spin-v1" && result.hostname === url.hostname));
      return { outcome: valid ? "valid" : "rejected" };
    } catch (error) {
      if (attempt === 0) continue;
      return {
        outcome: "unavailable",
        errorType: error instanceof DOMException && error.name === "TimeoutError" ? "timeout" : "connection_error",
        message: errorMessage(error),
      };
    }
  }
  return { outcome: "unavailable", errorType: "turnstile_unavailable", message: "Turnstile Siteverify is unavailable" };
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
