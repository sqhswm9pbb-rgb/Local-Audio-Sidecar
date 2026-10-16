package sidecar

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type metadataCache struct {
	mu      sync.RWMutex
	entries map[string]cachedMetadata
}

type cachedMetadata struct {
	size       int64
	modifiedAt int64
	track      Track
}

func newMetadataCache() *metadataCache {
	return &metadataCache{entries: make(map[string]cachedMetadata)}
}

func locateFFprobe(ffmpegPath string) string {
	if configured := strings.TrimSpace(os.Getenv("FFPROBE")); configured != "" {
		if path, err := exec.LookPath(configured); err == nil {
			return path
		}
	}
	ext := filepath.Ext(ffmpegPath)
	sibling := strings.TrimSuffix(ffmpegPath, ext) + "ffprobe" + ext
	if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
		return sibling
	}
	if path, err := exec.LookPath("ffprobe"); err == nil {
		return path
	}
	return ""
}

func loadTracks(paths []string, ffprobe string, cache *metadataCache) []Track {
	if cache == nil {
		cache = newMetadataCache()
	}
	tracks := make([]Track, len(paths))
	if len(paths) == 0 {
		return tracks
	}
	workers := 4
	if len(paths) < workers {
		workers = len(paths)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				tracks[index] = loadTrack(paths[index], ffprobe, cache)
			}
		}()
	}
	for i := range paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return tracks
}

func loadTrack(path, ffprobe string, cache *metadataCache) Track {
	track := trackFromPath(path)
	info, err := os.Stat(path)
	if err != nil {
		return track
	}
	track.FileSize = info.Size()
	track.ModifiedAt = info.ModTime().UnixNano()
	key := pathKey(path)
	stamp := track.ModifiedAt
	cache.mu.RLock()
	entry, ok := cache.entries[key]
	cache.mu.RUnlock()
	if ok && entry.size == info.Size() && entry.modifiedAt == stamp {
		return entry.track
	}
	if ffprobe != "" {
		if probed, probeErr := probeTrack(ffprobe, path); probeErr == nil {
			if probed.Title != "" {
				track.Title = probed.Title
			}
			if probed.Artist != "" {
				track.Artist = probed.Artist
			}
			track.Album = probed.Album
			track.ArtworkStream = probed.ArtworkStream
			track.HasArtwork = probed.HasArtwork
		}
	}
	cache.mu.Lock()
	cache.entries[key] = cachedMetadata{size: info.Size(), modifiedAt: stamp, track: track}
	cache.mu.Unlock()
	return track
}

type probeResult struct {
	Streams []struct {
		Index       int               `json:"index"`
		Disposition map[string]int    `json:"disposition"`
		Tags        map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		Tags map[string]string `json:"tags"`
	} `json:"format"`
}

type probedTrack struct {
	Title         string
	Artist        string
	Album         string
	ArtworkStream int
	HasArtwork    bool
}

func probeTrack(ffprobe, path string) (probedTrack, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error", "-show_entries",
		"format_tags=title,artist,album:stream=index,disposition=attached_pic:stream_tags=title,artist,album",
		"-of", "json", path,
	)
	data, err := cmd.Output()
	if err != nil {
		return probedTrack{}, err
	}
	var result probeResult
	if err := json.Unmarshal(data, &result); err != nil {
		return probedTrack{}, err
	}
	metadata := probedTrack{Title: tagValue(result.Format.Tags, "title"), Artist: tagValue(result.Format.Tags, "artist"), Album: tagValue(result.Format.Tags, "album"), ArtworkStream: -1}
	for _, stream := range result.Streams {
		if metadata.Title == "" {
			metadata.Title = tagValue(stream.Tags, "title")
		}
		if metadata.Artist == "" {
			metadata.Artist = tagValue(stream.Tags, "artist")
		}
		if metadata.Album == "" {
			metadata.Album = tagValue(stream.Tags, "album")
		}
		if stream.Disposition["attached_pic"] == 1 && !metadata.HasArtwork {
			metadata.HasArtwork = true
			metadata.ArtworkStream = stream.Index
		}
	}
	return metadata, nil
}

func tagValue(tags map[string]string, key string) string {
	for name, value := range tags {
		if strings.EqualFold(name, key) && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
