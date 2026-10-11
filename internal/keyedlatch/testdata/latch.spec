pkg ./internal/keyedlatch/
run TestLatch_

# Latch's own protocol, separately from the sites that use it
# (internal/app/testdata/transition_latch.spec pins jobTransitions' use of it).

[a claim takes a key another claim holds]
file internal/keyedlatch/latch.go
--- anchor
	if _, busy := l.held[key]; busy {
--- replace
	if _, busy := l.held[key]; busy && false {
--- end

[release wakes no waiter]
file internal/keyedlatch/latch.go
--- anchor
		close(ch)
--- replace
		_ = ch
--- end

[release leaves the key in the map]
file internal/keyedlatch/latch.go
--- anchor
		delete(l.held, key)
--- replace
		delete(l.held, "mut-"+key)
--- end

[Held reports the opposite]
file internal/keyedlatch/latch.go
--- anchor
	_, ok := l.held[key]
	return ok
--- replace
	_, ok := l.held[key]
	return !ok
--- end

[Done hands out a channel for an unheld key]
file internal/keyedlatch/latch.go
--- anchor
	return nil
}

// Wait waits
--- replace
	return make(chan struct{})
}

// Wait waits
--- end

[Wait on an unheld key waits]
file internal/keyedlatch/latch.go
--- anchor
	if ch == nil {
		return nil
	}
--- replace
	if ch == nil {
		ch = make(chan struct{})
	}
--- end

[an ended context wins over a release at random]
file internal/keyedlatch/latch.go
--- anchor
	case <-ch:
		return nil
	default:
	}
--- replace
	default:
	}
--- end

[Wait ignores its context]
file internal/keyedlatch/latch.go
--- anchor
	case <-ctx.Done():
--- replace
	case <-make(chan struct{}):
--- end
