package main

import (
	"errors"
	"testing"
)

func TestIsTransientDockerPullError(t *testing.T) {
	transient := []string{
		`failed to pull image eclipse-temurin:17-jre: failed to copy: httpReadSeeker: failed open: failed to do request: local error: tls: bad record MAC`,
		`unexpected EOF`,
		`Get "https://registry-1.docker.io/v2/": net/http: TLS handshake timeout`,
		`received unexpected HTTP status: 503 Service Unavailable`,
	}
	for _, msg := range transient {
		if !isTransientDockerPullError(errors.New(msg)) {
			t.Fatalf("expected transient pull error for %q", msg)
		}
	}

	permanent := []string{
		`manifest unknown`,
		`pull access denied, repository does not exist or may require authorization`,
		`unauthorized: authentication required`,
		`invalid reference format`,
	}
	for _, msg := range permanent {
		if isTransientDockerPullError(errors.New(msg)) {
			t.Fatalf("expected permanent pull error for %q", msg)
		}
	}
}

func TestPterodactylRustKeepsHomeContainerVolume(t *testing.T) {
	cfg := imageConfig{
		Entrypoint: []string{"/bin/bash", "/entrypoint.sh"},
		Workdir:    "/home/container",
		Env:        map[string]string{},
	}
	binds, dataDir := sanitizeCustomImageDefaultBinds(
		"ghcr.io/pterodactyl/games:rust",
		cfg,
		[]string{"/srv/adpanel/rust:/home/container"},
		"/home/container",
	)
	if len(binds) != 1 || binds[0] != "/srv/adpanel/rust:/home/container" {
		t.Fatalf("expected /home/container bind to be preserved, got %#v", binds)
	}
	if dataDir != "/home/container" {
		t.Fatalf("expected dataDir /home/container, got %q", dataDir)
	}
}

func TestImageWorkdirBindIsKeptWhenItIsKnownGameDataPath(t *testing.T) {
	cfg := imageConfig{
		Entrypoint: []string{"/entrypoint.sh"},
		Workdir:    "/steamcmd/rust",
		Env:        map[string]string{},
	}
	binds, dataDir := sanitizeCustomImageDefaultBinds(
		"didstopia/rust-server:full",
		cfg,
		[]string{"/srv/adpanel/rust:/steamcmd/rust"},
		"/steamcmd/rust",
	)
	if len(binds) != 1 || binds[0] != "/srv/adpanel/rust:/steamcmd/rust" {
		t.Fatalf("expected known data-path bind to be preserved, got %#v", binds)
	}
	if dataDir != "/steamcmd/rust" {
		t.Fatalf("expected dataDir /steamcmd/rust, got %q", dataDir)
	}
}

func TestRustRestartRecreatesContainer(t *testing.T) {
	if !shouldRecreateContainerOnRestart(map[string]any{"type": "rust"}) {
		t.Fatalf("expected rust template restart to recreate the container")
	}
	if !shouldRecreateContainerOnRestart(map[string]any{
		"type": "custom",
		"runtime": map[string]any{
			"image": "ghcr.io/pterodactyl/games",
			"tag":   "rust",
		},
	}) {
		t.Fatalf("expected pterodactyl rust image restart to recreate the container")
	}
	if shouldRecreateContainerOnRestart(map[string]any{"type": "minecraft"}) {
		t.Fatalf("did not expect minecraft restart to use rust recreate path")
	}
}
