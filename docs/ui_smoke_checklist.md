# UI Smoke Checklist (Svelte SPA)

Manual browser verification after building the SPA and Go binary.

---

## Prerequisites

```bash
cd ui && npm run build && cd ..
go build ./cmd/sabnzbd
./sabnzbd --config ~/.config/sabnzbd-go/sabnzbd.yaml --serve
```

Open `http://127.0.0.1:8080/` in a browser.

---

## 1. API Key Prompt

- [ ] First visit shows the API key entry card
- [ ] Entering a wrong key shows an error message
- [ ] Entering the correct key (from `sabnzbd.yaml`) connects and shows the main UI
- [ ] Refreshing the page skips the prompt (key stored in localStorage)

## 2. Navbar

- [ ] "SABnzbd" title visible
- [ ] Speed display shows a value (may be "0 B/s" if nothing is downloading)
- [ ] Pause/Resume button toggles and reflects state
- [ ] Settings gear icon opens the settings dialog
- [ ] "+ Add NZB" button opens the add dialog

## 3. Status Bar

- [ ] Shows item count, remaining size, ETA
- [ ] Speed sparkline graph renders (shows flat line if idle)
- [ ] "PAUSED" indicator appears when paused

## 4. Queue Tab

- [ ] Active queue items display in a table with progress bars
- [ ] Progress bars update on each poll (~2 seconds)
- [ ] Pause/Resume per-item buttons work
- [ ] Delete button removes the item from the queue
- [ ] Empty state shows "Queue is empty" when no items

## 5. History Tab

- [ ] Completed/failed items display with status badges
- [ ] "Completed" items show green badge, "Failed" shows red
- [ ] Delete button removes an item
- [ ] Completed timestamp is a readable date
- [ ] Empty state shows "History is empty"

## 6. Warnings Tab

- [ ] Warning count badge appears on the tab when warnings exist
- [ ] Warning list renders with numbered entries
- [ ] "Clear all" button clears warnings
- [ ] Toast notification appears at bottom-right when a new warning arrives

## 7. Add NZB Dialog

- [ ] File tab: drag-and-drop zone accepts .nzb files
- [ ] File tab: click to browse works
- [ ] File tab: uploading adds the NZB to the queue
- [ ] URL tab: pasting a URL and clicking Fetch submits it
- [ ] Success/error feedback shows inline

## 8. Settings Dialog

- [ ] Opens and loads config from the API
- [ ] Config sections are collapsible
- [ ] Values display correctly (strings, booleans, numbers, nested objects)
- [ ] Close button works

## 9. Page Title

- [ ] Browser tab title updates with queue status: "▶ 3 items | SABnzbd-Go"
- [ ] Changes to "⏸" when paused

## 10. Dark Mode

- [ ] If OS is set to dark mode, the page background is dark

---

## Expected Limitations

These are deliberate deferrals, not bugs:

| UI element | Why |
|---|---|
| Drag-and-drop queue reordering | Backend `mode=switch` is stubbed (returns 400) |
| Settings editing | Dialog is read-only; `mode=set_config` wiring is a future step |
| Keyboard shortcuts | Not yet implemented |
| Speed display accuracy | Computed from poll deltas, not a dedicated API field |
| History entries persisting | History DB is opened but no pipeline stage writes entries yet |
| Bandwidth limit slider | Scheduler logs the limit but does not pass it to the downloader |

If you encounter behavior not in this list, check the browser console (F12)
and the API response:

```bash
curl 'http://127.0.0.1:8080/api?mode=fullstatus&apikey=YOUR_KEY&output=json'
```
