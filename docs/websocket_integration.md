# SABnzbd-Go Real-Time UI Plan (WebSocket Integration)

## Objective
Replace the inefficient, fixed-interval HTTP polling (2s for Queue, 5s for History) in the Svelte SPA with a real-time, WebSocket-based event system to reduce backend load and improve UI responsiveness.

## Background & Motivation
Currently, the SPA relies on `setInterval` to call the `/api?mode=queue` and `/api?mode=history` endpoints. This causes continuous HTTP overhead (headers, auth, routing) even when the system is idle. A WebSocket connection provides a persistent, low-latency channel for the server to push updates exactly when they happen.

## Proposed Architecture: Event-Driven Polling & High-Frequency Data Push
Given the complexity of the newly added pagination, searching, and filtering logic, pushing the *entire* customized queue/history state over a WebSocket to multiple clients with different filters is overly complex. 

Instead, we will use a hybrid approach:
1. **Event-Driven Polling for Complex State**: The WebSocket serves as a lightweight "signal" channel for paginated data. The server broadcasts simple events (e.g., `{"event": "queue_updated"}`, `{"event": "history_updated"}`). The Svelte stores listen for these events and trigger their `poll()` methods to fetch filtered data via REST.
2. **Direct Push for High-Frequency Data**: High-frequency, unpaginated data (like current download speed in B/s, total remaining bytes) is pushed directly within the WebSocket payload. The UI consumes this data immediately to update the speed graph and progress bars without triggering an HTTP request, allowing for silky-smooth, sub-second updates.

## Implementation Steps

### Phase 1: Backend Event Bus & WebSocket Endpoint
1. **Dependency**: Introduce a modern WebSocket library (e.g., `github.com/coder/websocket` or `github.com/gorilla/websocket`) to the Go module.
2. **Broadcaster**: Create a simple Pub/Sub broadcaster in `internal/api/` (e.g., `events.go`) that manages connected WebSocket clients.
3. **API Endpoint**: Add a `/api/ws` route in `internal/api/server.go`.
   - Must authenticate the connection using the existing `sab_apikey` cookie or query parameter.
   - Must handle the HTTP upgrade to WebSocket.
   - Must run a write-pump goroutine to send broadcasted events to the client.
4. **Integration (State & Signals)**: 
   - Modify `internal/app/app.go` or `internal/queue/queue.go` to fire "queue_updated" and "history_updated" events.
5. **High-Frequency Push (Speed/Metrics)**:
   - Create a background ticker in the API broadcaster or queue manager that calculates current download speed (B/s) and emits a `{"event": "metrics", "speed": 1024500, "remaining": 50000000}` event frequently (e.g., every 500ms or 1s) while active downloads are running.

### Phase 2: Frontend WebSocket Client
1. **WS Store**: Create `ui/src/lib/stores/websocket.svelte.ts` to encapsulate the connection logic.
   - Implement automatic reconnection with exponential backoff.
   - Expose a `$state` boolean for connection status (Connected/Disconnected) to show in the UI.
   - Expose a subscription mechanism for other stores to register event listeners.

### Phase 3: Migrate Svelte Stores
1. **Update `queue.svelte.ts`**:
   - Remove `setInterval(poll, POLL_INTERVAL)`.
   - Subscribe to `queue_updated` events and call `poll()` (REST fetch) when received.
   - Subscribe to `metrics` events and directly update `speedBytesPerSec`, `speedHistory`, and overall queue totals *without* polling.
   - Add a fallback mechanism (e.g., poll once every 30 seconds just in case an event is missed).
2. **Update `history.svelte.ts`**:
   - Apply the same pattern, removing the 5s interval and listening for `history_updated` events to trigger REST polling.

## Alternatives Considered
- **Server-Sent Events (SSE)**: Simpler than WebSockets (unidirectional, standard HTTP), but WebSockets leave the door open for future bidirectional real-time commands (e.g., dragging and dropping queue items without REST overhead).
- **Full Data Push via WS**: Pushing the raw JSON data instead of a "ping" for the queue. Rejected because the server would need to track every client's current pagination page, limit, and search filters to push the correct slice of data, vastly increasing backend state complexity.

## Verification
- Confirm the network tab in browser DevTools shows a single upgraded `101 Switching Protocols` connection and no continuous `/api?mode=queue` requests while idle.
- Confirm the speed graph updates fluidly multiple times a second without triggering HTTP requests.
- Confirm that adding a fake job (via CLI or script) immediately triggers a `queue_updated` WS message and a subsequent REST fetch, updating the table instantly.
