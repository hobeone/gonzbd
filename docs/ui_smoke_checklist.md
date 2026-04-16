# Glitter UI Smoke Checklist

Manual verification checklist for the Glitter web UI. Run once after a fresh
build to confirm end-to-end rendering. Takes 5-10 minutes.

---

## 1. Build and start

```bash
go build ./cmd/sabnzbd
./sabnzbd --config ~/.config/sabnzbd-go/sabnzbd.yaml --serve
```

Expected log line (stdout, structured):

```
level=INFO msg="http listener starting" addr=127.0.0.1:8080 ...
```

If you see an error about missing directories, create them first (see
README Quickstart step 4).

---

## 2. Open the UI

Open `http://127.0.0.1:8080/` in a browser (Chrome, Firefox, or Safari).

Expected:
- The page title bar reads **SABnzbd**.
- The top menu bar appears with speed and queue-size indicators on the right.
- Three content tabs are visible below the menu: **Queue**, **History**,
  and **Warnings**.
- No "cannot GET /" or 404 error page.

---

## 3. API key

Glitter fetches queue data by sending the `apiKey` JS variable (injected
into the page from your config's `api_key` field) with each API call.

On first load: the UI should start polling the API immediately. You will
not be prompted to enter a key manually — the key is baked into the page
at render time.

If the network tab shows `401 Unauthorized` responses, check that the
`api_key` in `sabnzbd.yaml` matches the value that appears when you
view-source the rendered page and search for `var apiKey`.

---

## 4. Tab switching

Click each tab in turn:

- **Queue** — the content area should show the queue pane (empty list if
  no downloads are active).
- **History** — the history pane appears.
- **Warnings** — the warnings/messages pane appears.

Check the browser console (F12 > Console) after each click. Knockout
binding errors would appear here as red `TypeError` or `ReferenceError`
lines. A few yellow warnings about empty observables are expected (see
section 7 below).

---

## 5. Menu items

- Click the **gear icon** (top-right menu area). Confirm the Options modal
  opens showing tabs: General, Servers, Categories, RSS, Scheduling,
  Notifications, Special.
- Click the **Add NZB** (plus/upload icon). Confirm the Add NZB modal opens
  with URL and file-upload fields.
- Click outside a modal or press Escape to close it.

---

## 6. Page refresh

Reload the page (Ctrl-R / Cmd-R). Confirm:
- The page loads cleanly a second time.
- No double-init errors in the console (would look like "ko.applyBindings
  called twice").
- The active tab defaults back to Queue after reload (expected behavior).

---

## 7. Expected console output (not bugs)

The following are known and harmless:

- **Knockout observable warnings** — the initial API poll may log
  `TypeError: Cannot read properties of undefined` for queue speed
  or ETA fields while the first `/api?mode=queue` response is in flight.
  These resolve once the first poll completes.
- **Favicon `data-bind` flash** — the favicon `href` starts at
  `/staticcfg/ico/favicon.ico` and is updated by the `SABIcon` Knockout
  observable once the status poll returns. A brief "404" for the initial
  static path before the observable hydrates is normal.
- **Platform / CPU / SIMD empty rows** — the Options > Status tab shows
  blank "Platform", "CPU", and "SIMD" rows. These fields are deferred
  (see `implementation_notes.md` §6 — *Glitter sysinfo*).
- **Missing 404s for `/staticcfg/...`** — icons and generated assets
  under `/staticcfg/` are served from the config `admin_dir`. If that
  directory was not pre-populated with icons, you will see 404s for
  PNG/SVG files. The UI degrades gracefully (missing icons, not a crash).

---

## 8. What will not work yet

These items render as inactive or blank UI elements. They are deliberate
deferrals, not bugs. See `docs/implementation_notes.md` §6 for the full
list.

| UI element | Why it is inactive |
|---|---|
| Options > Status: Platform / CPU / SIMD | Sysinfo package not yet wired; fields return empty strings |
| Shutdown / Standby / Hibernate menu options | OS-power options omitted (`$power_options` deferral); HTTP shutdown returns 501 |
| "Resume post-processing" menu entry | `$pp_pause_event` flag not yet backed by runtime state |
| Bandwidth limit slider actually throttling | Scheduler logs the limit but does not pass it to the downloader |
| History entries persisting across restarts | History DB is opened but no pipeline stage writes entries yet |
| Speed graph animating | Requires active downloads; empty queue shows flat line |

If you encounter UI behavior not in this list that appears broken, check
the browser console for errors and cross-reference the API response with:

```bash
curl 'http://127.0.0.1:8080/api?mode=fullstatus&apikey=YOUR_KEY&output=json'
```
