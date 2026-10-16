package sidecar

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config describes the folders to scan and any named channels that combine
// selected folders or tracks.
type Config struct {
	MusicDirs []string         `json:"music_dirs"`
	Channels  []ChannelConfig  `json:"channels"`
	Defaults  *ChannelDefaults `json:"defaults,omitempty"`
}

// ChannelDefaults affect the automatically discovered folder and file
// channels. Named channels keep their own shuffle and repeat settings.
type ChannelDefaults struct {
	Shuffle bool   `json:"shuffle,omitempty"`
	Repeat  string `json:"repeat,omitempty"`
}

// ChannelConfig adds a named channel. Paths can be absolute or relative to the
// config file. Folder contents are scanned recursively.
type ChannelConfig struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Folders []string `json:"folders,omitempty"`
	Files   []string `json:"files,omitempty"`
	Shuffle bool     `json:"shuffle,omitempty"`
	Repeat  string   `json:"repeat,omitempty"`
}

func LoadConfig(path string) (Config, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, "", fmt.Errorf("resolve config path: %w", err)
	}
	body, err := os.ReadFile(absPath)
	if err != nil {
		return Config{}, "", fmt.Errorf("read config %q: %w", absPath, err)
	}
	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return Config{}, "", fmt.Errorf("parse config %q: %w", absPath, err)
	}
	return cfg, filepath.Dir(absPath), nil
}

func resolveConfiguredPath(baseDir, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", raw, err)
	}
	return filepath.Clean(absPath), nil
}
