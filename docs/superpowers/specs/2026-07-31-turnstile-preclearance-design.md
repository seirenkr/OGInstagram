# Turnstile pre-clearance design

## Goal

Run Turnstile only when Cloudflare challenges `POST /api/embed`. After the
visitor passes once, reuse Cloudflare's `cf_clearance` for the zone Challenge
Passage period (normally 30 minutes) instead of producing and validating a new
Turnstile token for every preview.

## Chosen approach

Use Cloudflare's native WAF/pre-clearance boundary; do not add an application
session endpoint or a second cookie.

The production WAF rule is a Managed Challenge with this exact expression:

```text
http.request.method eq "POST" and http.request.uri.path eq "/api/embed"
```

The existing Turnstile widget must use the same registered hostname as the
zone, enable pre-clearance, and use clearance level `managed`. The zone's
Challenge Passage controls cookie lifetime. A valid cookie may still be
invalidated earlier by Cloudflare's precursor clearance checks.

## Request flow

1. The browser first posts `{ "path": ... }` to `/api/embed` without executing
   Turnstile.
2. A normal HTML response is parsed as today.
3. A response is treated as a Cloudflare challenge only when
   `response.headers.get("cf-mitigated") === "challenge"`. Status code and body
   format are not used as challenge detectors.
4. On that signal, the browser explicitly executes the existing Turnstile
   widget once. Successful pre-clearance creates `cf_clearance` and returns a
   single-use token.
5. The browser retries the same `/api/embed` request once with
   `{ "path": ..., "token": ... }`. The Worker validates that token with
   Siteverify before serving the preview.
6. The widget is reset only after the protected request completes. A second
   challenge response or any widget callback failure ends the attempt; there
   is no retry loop.
7. Later preview requests omit `token`. WAF authenticates `cf_clearance` before
   the Worker, while the Worker uses its digest only as the existing rate-limit
   key. The Worker never claims to validate this opaque cookie itself.

Every Turnstile token produced by this application is therefore sent to
Siteverify exactly once. Requests that reuse `cf_clearance` produce no token.

## Worker contract and errors

`token` becomes optional in the JSON request. Production requests still
require exactly one non-empty `cf_clearance` cookie; localhost keeps the
existing development exception because `wrangler dev` has no challenge
platform.

When `token` is present, validation follows the current Turnstile Spin Worker
example:

- reject an empty token, a token over 2048 characters, or missing hostname
  configuration;
- post URL-encoded `secret`, `response`, and `remoteip` to the canonical
  Siteverify endpoint with a 10-second timeout;
- require `success`, action `turnstile-spin-v1`, and the deployment hostname;
- return exactly plain-text `forbidden` with HTTP 403 for malformed input,
  Siteverify transport/HTTP failures, and rejected tokens.

The same plain-text `403 forbidden` is used when the production clearance
cookie is missing or ambiguous. Existing application errors unrelated to
Turnstile retain their current RFC 9457 problem responses and localized UI
copy.

## Tests

One focused Worker test file will guard both paths:

- a production request without `cf_clearance` fails with `403 forbidden`;
- a production request with `cf_clearance` and no token reaches the protected
  handler without Siteverify;
- a request carrying a token invokes canonical Siteverify and rejects all
  invalid/error outcomes with `403 forbidden`;
- action, hostname, token size, content type, remote IP, and timeout remain
  explicit in the implementation.

Type checking and the repository's existing check command cover the browser
branch. The frontend diff will keep the challenge detector in the shared
`fetchPreview` path so every submit follows the same one-retry rule.

## Official sources

- [Cloudflare clearance and Turnstile pre-clearance](https://developers.cloudflare.com/cloudflare-challenges/concepts/clearance/)
- [Challenge Page compatibility guidance](https://developers.cloudflare.com/cloudflare-challenges/challenge-types/challenge-pages/)
- [Turnstile Spin canonical Worker example](https://developers.cloudflare.com/turnstile/spin/)
- [Detect a Challenge Page response](https://developers.cloudflare.com/cloudflare-challenges/challenge-types/challenge-pages/detect-response/)
