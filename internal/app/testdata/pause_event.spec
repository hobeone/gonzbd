pkg ./internal/app/
run TestPauseResumeDownloads_BroadcastQueueUpdated

[PauseDownloads stops broadcasting]
file internal/app/app.go
--- anchor
		app.downloader.Pause()
	}
	app.mu.Unlock()
	// --- No lock held below this line ---
	app.emit(Event{Type: "queue_updated"})
--- replace
		app.downloader.Pause()
	}
	app.mu.Unlock()
--- end

[ResumeDownloads stops broadcasting]
file internal/app/app.go
--- anchor
		app.downloader.Resume()
	}
	app.mu.Unlock()
	// --- No lock held below this line ---
	app.emit(Event{Type: "queue_updated"})
--- replace
		app.downloader.Resume()
	}
	app.mu.Unlock()
--- end
