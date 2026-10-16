package sidecar

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Artwork returns a cached JPEG rendition of the embedded cover image for a
// track. The returned image is capped at 512 pixels on either dimension.
func (p *LocalProvider) Artwork(trackID string) ([]byte, error) {
	p.catalogMu.RLock()
	track, exists := p.trackByID[trackID]
	p.catalogMu.RUnlock()
	if !exists || !track.HasArtwork || track.ArtworkStream < 0 {
		return nil, fmt.Errorf("track artwork not found")
	}

	p.artworkMu.Lock()
	if data, ok := p.artworkCache[trackID]; ok {
		cached := append([]byte(nil), data...)
		p.artworkMu.Unlock()
		return cached, nil
	}
	p.artworkMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.ffmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", track.Path, "-map", fmt.Sprintf("0:%d", track.ArtworkStream),
		"-frames:v", "1", "-vf", "scale=512:512:force_original_aspect_ratio=decrease",
		"-q:v", "4", "-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1",
	)
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("could not read embedded artwork")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("track artwork not found")
	}

	p.artworkMu.Lock()
	if _, exists := p.artworkCache[trackID]; !exists {
		if len(p.artworkOrder) == 32 {
			oldest := p.artworkOrder[0]
			p.artworkOrder = p.artworkOrder[1:]
			delete(p.artworkCache, oldest)
		}
		p.artworkOrder = append(p.artworkOrder, trackID)
		p.artworkCache[trackID] = append([]byte(nil), data...)
	}
	p.artworkMu.Unlock()
	return data, nil
}
