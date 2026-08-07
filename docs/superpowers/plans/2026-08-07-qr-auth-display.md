# QR Auth Display Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Render the Riot Mobile QR image in Discord by sending the required multipart attachment metadata without changing the server-side Riot login flow.

**Architecture:** Keep QR creation and Riot session polling unchanged. Acknowledge the Discord component before Riot network I/O, then derive partial attachment records from uploaded files and include them in the original-message edit so `attachment://riot-qr.png` resolves.

**Tech Stack:** Go 1.22+, discordgo v0.29.0, `net/http/httptest`, multipart MIME parsing.

## Global Constraints

- The phone must only approve the login; the bot server must create, poll, and exchange the Riot QR session.
- No inbound localhost listener or user-installed helper may be required for QR auth.
- Keep the change limited to the QR attachment transport and supporting documentation.
- Use TDD and run the complete Go verification suite before commit and push.

---

### Task 1: Preserve QR attachment metadata in Discord component updates

**Files:**
- Create: `internal/bot/discord_internal_test.go`
- Modify: `internal/bot/discord.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `Response.Files []*discordgo.File`, `deferComponentUpdate(*discordgo.Session, *discordgo.InteractionCreate) error`, and `editInteractionWithFiles(*discordgo.Session, *discordgo.InteractionCreate, Response) error`.
- Produces: `attachmentsForFiles([]*discordgo.File) *[]*discordgo.MessageAttachment`, with multipart index strings matching Discord's `files[n]` parts.

- [x] **Step 1: Write the failing end-to-end component test**

  Route Discord endpoints to an `httptest.Server` and use an `AuthStarter` fake whose `BeginQRAuth` records whether callback type `6` was already received. Invoke the real QR component handler and assert acknowledgement precedes Riot session creation, then inspect the original-message multipart edit.

- [x] **Step 2: Run the component test and verify RED**

  Run: `go test ./internal/bot -run TestAuthQRComponent_AcknowledgesThenEditsWithMappedAttachment -count=1 -v`

  Expected: FAIL because the current handler calls `BeginQRAuth` first and then takes the generic type `4` ephemeral branch without the QR file or button.

- [x] **Step 3: Assert the multipart edit contract in the component test**

  Parse the webhook edit's `payload_json` and assert the literal mapping `id: "0"` and `filename: "riot-qr.png"`, the generated PNG in `files[0]`, the `attachment://riot-qr.png` embed, and the Riot Mobile link component.

- [x] **Step 4: Confirm the real dispatch path exposes the missing edit**

  Run: `go test ./internal/bot -run TestAuthQRComponent_AcknowledgesThenEditsWithMappedAttachment -count=1 -v`

  Expected: FAIL with callback type `4` and `BeginQRAuth ran before Discord acknowledged`.

- [x] **Step 5: Add deferred ACK and the attachment-aware edit**

  Respond with `InteractionResponseDeferredMessageUpdate` before `HandleAuthQR`. Then derive `[]*discordgo.MessageAttachment` from files, setting each entry's `ID` to `strconv.Itoa(index)` and `Filename` to `file.Name`; assign it to `WebhookEdit.Attachments` next to `Files`.

- [x] **Step 6: Verify GREEN and the package suite**

  Run: `go test ./internal/bot -run TestAuthQRComponent_AcknowledgesThenEditsWithMappedAttachment -count=1 -v`

  Expected: PASS.

  Run: `go test ./internal/bot -count=1`

  Expected: PASS.

- [x] **Step 7: Document server-side QR behavior**

  Clarify in `README.md` that the localhost redirect is parsed from Riot's JSON response and is never opened for QR auth; document that official RSO requires Riot production approval and does not replace this bot's private store session.

- [x] **Step 8: Run full verification and prepare the commit**

  Run: `gofmt -w internal/bot/discord.go internal/bot/discord_internal_test.go`, `go test ./... -count=1`, `go test -race ./... -count=1`, `go vet ./...`, and the repository's existing build checks.

  Verification completed before committing as `Fix Riot Mobile QR rendering`.
