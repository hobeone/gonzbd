package api

// NumClients returns the number of connected WebSocket clients.
// For testing purposes only.
func (b *Broadcaster) NumClients() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.clients)
}

