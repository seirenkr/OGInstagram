# Discord emoji assets

- Brand logo: `web/public/favicon-192.png`.
- Verification badge: `verified.png` (128 × 128, transparent), rendered from
  the included `verified.svg`. The shape comes from the MIT-licensed Phosphor
  `SealCheck` icon; its license is included here. The badge uses a blue fill
  and a white check for both light and dark backgrounds.

Upload these as guild custom emojis, then set their numeric IDs in
`DISCORD_BRAND_EMOJI_ID` and `DISCORD_VERIFIED_EMOJI_ID`.
The latter is displayed only when the Instagram response explicitly reports
`is_verified: true`. It is our display of that source flag, not a native
Discord verification status. The renderer never substitutes a Unicode check.

Production registration (2026-10-05):

| Name | Emoji ID |
| --- | --- |
| `oginstagram` | `1556597080229810266` |
| `verified` | `1556598045670514798` |

These public IDs refer to the assets registered in the project's Discord
server. They are configuration values, not authentication credentials.
