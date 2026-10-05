export async function requestPreview(path: string, token: string | undefined, signal: AbortSignal, fetcher: typeof fetch = fetch): Promise<Response> {
  const response = await fetcher("/api/embed", {
    method: "POST",
    headers: {
      "accept": "text/html",
      "content-type": "application/json",
    },
    body: JSON.stringify(token === undefined ? { path } : { path, token }),
    signal,
  });
  if (response.headers.get("cf-mitigated") === "challenge") throw new Error("cloudflare-challenge");
  return response;
}

export async function continueCurrentAttempt(loading: Promise<void>, current: () => boolean, next: () => void, fail: () => void): Promise<void> {
  try {
    await loading;
    if (current()) next();
  } catch {
    if (current()) fail();
  }
}
