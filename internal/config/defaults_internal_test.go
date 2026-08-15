package config

import (
	"regexp"
	"testing"
)

// ---------- runningInDockerImage ----------

func TestRunningInDockerImage(t *testing.T) {
	t.Run("unset reports false", func(t *testing.T) {
		t.Setenv("GONZBD_DOCKER", "")
		if got := runningInDockerImage(); got {
			t.Errorf("runningInDockerImage() = true, want false when GONZBD_DOCKER is unset")
		}
	})

	t.Run("set to 1 reports true", func(t *testing.T) {
		t.Setenv("GONZBD_DOCKER", "1")
		if got := runningInDockerImage(); !got {
			t.Errorf("runningInDockerImage() = false, want true when GONZBD_DOCKER=1")
		}
	})

	t.Run("any other value reports false", func(t *testing.T) {
		t.Setenv("GONZBD_DOCKER", "true")
		if got := runningInDockerImage(); got {
			t.Errorf("runningInDockerImage() = true, want false when GONZBD_DOCKER=%q (only \"1\" counts)", "true")
		}
	})
}

// ---------- newAPIKey ----------

var apiKeyHexPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestNewAPIKey(t *testing.T) {
	key := newAPIKey()
	if !apiKeyHexPattern.MatchString(key) {
		t.Errorf("newAPIKey() = %q, want 16 lowercase hex characters", key)
	}
}

func TestNewAPIKey_Unique(t *testing.T) {
	a := newAPIKey()
	b := newAPIKey()
	if a == b {
		t.Errorf("two consecutive newAPIKey() calls returned identical values %q; expected randomness", a)
	}
}
