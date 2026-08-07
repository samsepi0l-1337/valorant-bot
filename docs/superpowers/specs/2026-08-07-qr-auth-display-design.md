# QR Auth Display Design

## Goal

Make the Riot Mobile QR code reliably visible in the ephemeral Discord `/auth`
message while keeping account authentication executable by a remote bot server
without an inbound localhost listener.

## Root Cause

`HandleAuthQR` produces a valid PNG and references it as
`attachment://riot-qr.png`, but it also marks the response ephemeral. The
generic ephemeral branch runs before the QR-specific branch and sends only the
embed: it drops the PNG file and Riot Mobile link button. This produces the
empty red embed shell in the supplied screenshot. The bypassed QR helper also
omits the partial attachment entry that maps JSON attachment ID `0` to
multipart `files[0]`, so merely reordering those branches would remain fragile.

## Considered Approaches

1. Immediately defer the component update, create the Riot QR session, then
   PATCH the original message with explicit `attachments` metadata. This
   satisfies Discord's three-second acknowledgement window and its documented
   multipart mapping between `files[n]`, attachment ID `n`, and filename.
2. Remove the embed and rely on Discord's plain attachment preview. This loses
   the intentional embed presentation and still leaves attachment-edit
   semantics implicit.
3. Keep a single component callback and only add attachment metadata. This is
   smaller, but Riot discovery and QR-session creation happen before the ACK
   and can exceed Discord's three-second response deadline.

Approach 1 is selected.

## Authentication Architecture

The existing Riot Mobile flow remains server-side: the bot creates the QR
session, retains its cookies, polls Riot for approval, exchanges the returned
login token, and stores the persistent `ssid` session. The phone only scans or
opens the Riot Mobile approval URL. The `http://localhost/redirect` value used
during the final `riot-client` authorization is a registered redirect
identifier returned inside Riot's JSON response; no browser follows it and no
listener is required.

Riot Sign On is the supported third-party OAuth alternative, but it requires an
approved production application and RSO client. Its opt-in identity access does
not replace the authenticated private store session required by this bot, so it
cannot transparently replace the QR flow.

## Components and Data Flow

- `HandleAuthQR` continues to generate `riot-qr.png` and the matching embed.
- The Discord handler acknowledges the QR button before making Riot requests.
- The original interaction message edit derives one partial attachment per
  uploaded file, using the zero-based multipart index as its attachment ID.
- The edit includes content, embed, link button, file bytes, and attachment
  metadata in one PATCH request.
- QR polling and token exchange remain unchanged.

## Error Handling

Each uploaded file receives attachment metadata with the same multipart index.
The Discord API continues to return any upload failure to the existing
component error log. Authentication timeout and Riot session errors retain
their current localized messages.

## Testing

HTTP-boundary regression tests assert that the component is acknowledged before
`BeginQRAuth` runs and capture the real multipart edit emitted by discordgo.
The multipart test asserts that its JSON payload maps attachment ID `0` to
`riot-qr.png` while the matching PNG is uploaded as `files[0]`. Existing QR
generation and Riot server-side session tests continue to cover the other
boundaries.
