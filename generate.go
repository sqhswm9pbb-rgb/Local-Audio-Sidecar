package sidecar

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// GenerateMusicConfig writes a config for one scanned music folder and returns
// the number of supported audio files found below it.
func GenerateMusicConfig(folder, destination string, force bool) (int, error) {
	root, err := filepath.Abs(folder)
	if err != nil {
		return 0, fmt.Errorf("resolve music folder: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return 0, fmt.Errorf("music folder %q: %w", folder, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("music folder %q is not a directory", folder)
	}
	count := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("scan %q: %w", path, walkErr)
		}
		if !entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 && isAudioPath(path) {
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, fmt.Errorf("no supported audio files found in %q", root)
	}
	if destination == "" {
		destination = "music.json"
	}
	output, err := filepath.Abs(destination)
	if err != nil {
		return 0, fmt.Errorf("resolve output path: %w", err)
	}
	data, err := json.MarshalIndent(Config{
		MusicDirs: []string{root},
		Channels:  []ChannelConfig{},
	}, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	if force {
		if err := os.WriteFile(output, data, 0o644); err != nil {
			return 0, fmt.Errorf("write config %q: %w", output, err)
		}
	} else {
		file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return 0, fmt.Errorf("create config %q (use --force to replace it): %w", output, err)
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			_ = os.Remove(output)
			return 0, fmt.Errorf("write config %q: %w", output, err)
		}
		if err := file.Close(); err != nil {
			return 0, fmt.Errorf("close config %q: %w", output, err)
		}
	}
	return count, nil
}
