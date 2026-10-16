package sidecar

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os/exec"
	"time"
)

var errResponseClosed = errors.New("audio stream client disconnected")

type streamWriteResult struct {
	emitted bool
	err     error
}

func (p *LocalProvider) Stream(id string, w http.ResponseWriter, r *http.Request) error {
	channel, ok := p.channel(id)
	if !ok {
		return fmt.Errorf("channel %q not found", id)
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	runtime := p.runtimeFor(id)
	skipSignal := runtime.subscribe()
	defer runtime.unsubscribe(skipSignal)

	// Keep a single MP3 encoder alive for the entire HTTP response. Restarting
	// the MP3 encoder at every track boundary produces a byte stream that some
	// browser decoders accept but SoundTouch speakers reject partway through.
	encoder := exec.CommandContext(ctx, p.ffmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0",
		"-vn", "-ac", "2", "-ar", "44100",
		"-c:a", "libmp3lame", "-b:a", "256k",
		"-f", "mp3", "pipe:1",
	)
	encoderInput, err := encoder.StdinPipe()
	if err != nil {
		return fmt.Errorf("open FFmpeg audio input: %w", err)
	}
	encoderOutput, err := encoder.StdoutPipe()
	if err != nil {
		_ = encoderInput.Close()
		return fmt.Errorf("open FFmpeg audio output: %w", err)
	}
	encoderStderr, err := encoder.StderrPipe()
	if err != nil {
		_ = encoderInput.Close()
		return fmt.Errorf("open FFmpeg diagnostics: %w", err)
	}
	if err := encoder.Start(); err != nil {
		_ = encoderInput.Close()
		return fmt.Errorf("start FFmpeg MP3 encoder: %w", err)
	}
	go func() { _, _ = io.Copy(io.Discard, encoderStderr) }()
	encoderWaited := false
	defer func() {
		cancel()
		_ = encoderInput.Close()
		if !encoderWaited {
			if encoder.Process != nil {
				_ = encoder.Process.Kill()
			}
			_ = encoder.Wait()
		}
	}()

	writeDone := make(chan streamWriteResult, 1)
	go func() {
		result := streamWriteResult{}
		defer func() { writeDone <- result }()
		buf := make([]byte, 64*1024)
		headersSent := false
		for {
			n, readErr := encoderOutput.Read(buf)
			if n > 0 {
				if !headersSent {
					w.Header().Set("Content-Type", "audio/mpeg")
					w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
					w.Header().Set("Pragma", "no-cache")
					w.Header().Set("Connection", "close")
					w.WriteHeader(http.StatusOK)
					headersSent = true
				}
				written, writeErr := w.Write(buf[:n])
				if writeErr != nil {
					result.err = fmt.Errorf("%w: %v", errResponseClosed, writeErr)
					cancel()
					return
				}
				if written != n {
					result.err = errResponseClosed
					cancel()
					return
				}
				result.emitted = true
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					result.err = readErr
					cancel()
				}
				return
			}
		}
	}()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	var oneTrack Track
	if channel.Repeat == RepeatOne {
		index := 0
		if channel.Shuffle {
			index = rng.Intn(len(channel.Tracks))
		}
		oneTrack = channel.Tracks[index]
	}
	var firstErr error
	for {
		latest, exists := p.channel(id)
		if !exists {
			break
		}
		order := append([]Track(nil), latest.Tracks...)
		if channel.Shuffle && channel.Repeat != RepeatOne {
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		}
		if channel.Repeat == RepeatOne {
			order = []Track{oneTrack}
		}

		cycleEmitted := false
		cycleSkipped := false
		for _, track := range order {
			if ctx.Err() != nil {
				break
			}
			p.setNowPlaying(id, track)
			emitted, trackErr := p.streamTrackPCM(ctx, track, encoderInput, skipSignal)
			cycleEmitted = cycleEmitted || emitted
			cycleSkipped = cycleSkipped || errors.Is(trackErr, errSkipTrack)
			if trackErr != nil && !errors.Is(trackErr, errSkipTrack) {
				if firstErr == nil {
					firstErr = trackErr
				}
				if ctx.Err() == nil {
					log.Printf("channel %s: %v", id, trackErr)
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if !cycleEmitted && !cycleSkipped {
			if firstErr == nil {
				firstErr = fmt.Errorf("channel %q contains no playable audio", id)
			}
			break
		}
		if channel.Repeat == RepeatOff {
			break
		}
	}

	_ = encoderInput.Close()
	writeResult := <-writeDone
	waitErr := encoder.Wait()
	encoderWaited = true
	if ctx.Err() != nil || errors.Is(writeResult.err, errResponseClosed) {
		return nil
	}
	if writeResult.err != nil {
		return writeResult.err
	}
	if !writeResult.emitted {
		if firstErr != nil {
			return firstErr
		}
		return fmt.Errorf("channel %q contains no playable audio", id)
	}
	if waitErr != nil {
		log.Printf("MP3 encoder for channel %s exited: %v", id, waitErr)
	}
	return nil
}

// streamTrackPCM decodes one file to raw PCM and feeds it to the channel's
// single long-lived MP3 encoder. Track boundaries therefore never restart the
// MP3 bitstream sent to the speaker.
func (p *LocalProvider) streamTrackPCM(ctx context.Context, track Track, encoderInput io.Writer, skipSignal <-chan struct{}) (bool, error) {
	trackCtx, stopTrack := context.WithCancel(ctx)
	defer stopTrack()
	cmd := exec.CommandContext(trackCtx, p.ffmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-re", "-i", track.Path, "-map", "0:a:0", "-vn", "-sn", "-dn",
		"-ac", "2", "-ar", "44100", "-f", "s16le", "pipe:1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("open FFmpeg output for %q: %w", track.Title, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return false, fmt.Errorf("open FFmpeg diagnostics for %q: %w", track.Title, err)
	}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start FFmpeg for %q: %w", track.Title, err)
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	waited := false
	defer func() {
		if !waited {
			stopTrack()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}
	}()

	buf := make([]byte, 64*1024)
	output := make(chan ffmpegOutput, 1)
	go func() {
		for {
			n, readErr := stdout.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				select {
				case output <- ffmpegOutput{data: data}:
				case <-trackCtx.Done():
					return
				}
			}
			if readErr != nil {
				select {
				case output <- ffmpegOutput{err: readErr}:
				case <-trackCtx.Done():
				}
				return
			}
		}
	}()

	emitted := false
	for {
		select {
		case <-ctx.Done():
			return emitted, ctx.Err()
		case <-skipSignal:
			return emitted, errSkipTrack
		case result := <-output:
			if len(result.data) > 0 {
				written, writeErr := encoderInput.Write(result.data)
				if written > 0 {
					emitted = true
				}
				if writeErr != nil {
					return emitted, fmt.Errorf("feed PCM for %q to MP3 encoder: %w", track.Title, writeErr)
				}
				if written != len(result.data) {
					return emitted, fmt.Errorf("feed PCM for %q to MP3 encoder: %w", track.Title, io.ErrShortWrite)
				}
			}
			if result.err != nil {
				if result.err != io.EOF {
					return emitted, fmt.Errorf("decode %q: %w", track.Title, result.err)
				}
				goto finished
			}
		}
	}

finished:
	waitErr := cmd.Wait()
	waited = true
	if waitErr != nil {
		return emitted, fmt.Errorf("FFmpeg could not decode %q: %w", track.Title, waitErr)
	}
	return emitted, nil
}

type ffmpegOutput struct {
	data []byte
	err  error
}
