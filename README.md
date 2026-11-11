# texts-mcp

Read your Android phone's texts from Claude, for free.

- **Covers:** SMS, MMS and RCS (blue-bubble style chat), group chats, texts you sent, muted and archived chats.
- **Read-only:** Claude can read and search texts. It cannot send, delete or change anything.
- **Private:** login and verification codes are blanked out on the phone before they leave it. Spam, blocked and deleted chats never leave the phone. Texts older than 30 days are deleted.
- **Free:** the phone does the work in Termux, and a free Cloudflare Worker stores the texts and serves them to Claude.

```
Google Messages on the phone
        │  (Google's own "Messages for web" link)
texts-bridge in Termux on the same phone
        │  (encrypted, passphrase-protected)
Cloudflare Worker + D1 database (free plan)
        │  (secret connector address)
Claude custom connector
```

It needs Google Messages as the phone's texting app. Samsung Messages and other apps are not supported.

## Setup

About 20 minutes, once. You need the computer and the phone.

### 1. Choose a passphrase

Make up a passphrase of at least 16 characters and save it in your password manager. It protects everything. Don't paste it into any chat, Claude included.

### 2. Deploy the Worker (computer)

1. In the Cloudflare dashboard, open **Workers & Pages**, then **Create** > **Import a repository**.
2. Pick GitHub, then **ZacVblack/texts_mcp**. Keep the project name **texts-mcp** (it must match). Press **Deploy**. The database is created for you.
3. When it's deployed, open the Worker's **Settings** > **Variables and Secrets** > **Add**. Type **Secret**, name `PASSPHRASE`, value your passphrase. Save and deploy.
4. Open the Worker's address (it looks like `https://texts-mcp.<your-name>.workers.dev`). It should say `texts-mcp is running.` Keep this address handy.

### 3. Link Google Messages (computer)

Use **Firefox** for this step. Chrome (and other Chromium browsers) tie their Google sign-in to the computer, so a sign-in copied out of them stops working within hours and the link drops. texts-bridge warns you if you paste a request copied from one of them.

1. On the phone, open Google Messages, tap your profile picture, and make sure it's signed in to your Google account (**Messages settings** > **Google account**, or **Device pairing**).
2. On the computer, download **texts-bridge-windows-amd64.exe** from this repo's [latest release](../../releases/latest).
3. Double-click it. If Windows says it protected your PC, press **More info** > **Run anyway**.
4. Type the Worker address, then your passphrase (typing is hidden).
5. Follow the steps it shows:
   - In Firefox, open a private window (**Ctrl+Shift+P**) and the Google sign-in address it prints. Sign in with the same Google account as the phone.
   - Press **F12**, open the **Network** tab, press **F5**.
   - Right-click the request named **config**, then **Copy Value** > **Copy as cURL (POSIX)**.
   - Go back to texts-bridge and press **Enter**. It reads what you copied.
6. Your phone shows a few emoji. Tap the one texts-bridge shows.
7. It says **Linked** and prints your **connector address**. Close the private window without signing out.

### 4. Run it on the phone

1. Install **F-Droid** from [f-droid.org](https://f-droid.org), then install **Termux** and **Termux:Boot** from F-Droid. (The Play Store version of Termux is outdated and won't work.)
2. Open Termux and run:
   ```
   curl -fsSLo install.sh https://raw.githubusercontent.com/ZacVblack/texts_mcp/main/scripts/termux-install.sh && bash install.sh
   ```
3. Type the same Worker address and passphrase.
4. In Android settings, set **Apps** > **Termux** > **Battery** to **Unrestricted**.
5. Open the **Termux:Boot** app once, so it starts texts-bridge after every reboot. If Android refuses to install Termux:Boot (Play Protect blocks it as "built for an older version of Android" and offers no **Install anyway**), skip it: texts-bridge also starts whenever you open Termux, so just open Termux once after the phone restarts.

### 5. Add it to Claude

In Claude, open **Settings** > **Connectors** > **Add custom connector**. Name it **Texts** and paste the connector address from step 3. (Lost it? Run `texts-bridge url` in Termux.)

Treat the connector address like a password: anyone with it can read your texts.

## Day to day

- **Phone off or no signal:** texts catch up when it's back. Claude's tools say when the phone last checked in.
- **Opening Messages for web in a browser:** texts-bridge takes the link back within a few minutes, so use the phone app instead.
- **"Run texts-bridge pair":** Google unlinked it (for example after a password change, signing out, or a sign-in copied from Chrome). The log line says which. Double-click the .exe on the computer again and link from a Firefox private window. The phone picks up the new link by itself within 2 minutes.
- **Check it's alive:** in Termux, `tail -f ~/.texts-mcp/bridge.log`. Or ask Claude for the texts link status.
- **Update:** run the install command in Termux again, and download the newest .exe.
- **Change the passphrase:** change the `PASSPHRASE` secret, run `texts-bridge setup` on both devices, then pair again. The connector address changes too.

## Claude's tools

| Tool | What it does |
| --- | --- |
| `get_recent_texts` | Texts from the last N minutes, grouped by conversation, including late arrivals |
| `list_conversations` | Recent conversations with unread state and last text |
| `get_conversation` | One conversation by name, number or id |
| `search_texts` | Find texts containing words |
| `texts_status` | Whether the phone link is working |

## Development

```
cd bridge && go test ./...          # phone/computer program (Go)
cd worker && node --test test/*.test.mjs   # Worker (no dependencies)
```

Every push to `main` that touches `bridge/` publishes a new release with builds for Android, Windows, Linux and macOS.

## License

AGPL-3.0, because texts-bridge uses [libgm from mautrix-gmessages](https://github.com/mautrix/gmessages) (AGPL-3.0) to talk to Google Messages.
