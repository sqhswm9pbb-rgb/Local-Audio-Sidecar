package sidecar

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type RepeatMode string

const (
	RepeatOff RepeatMode = "off"
	RepeatAll RepeatMode = "all"
	RepeatOne RepeatMode = "one"
)

type Track struct {
	ID            string
	Path          string
	Title         string
	Artist        string
	Album         string
	ArtworkStream int
	HasArtwork    bool
	FileSize      int64
	ModifiedAt    int64
}

type AudioChannel struct {
	Station Station
	Tracks  []Track
	Shuffle bool
	Repeat  RepeatMode
	rank    int
}

type LocalProvider struct {
	catalogMu    sync.RWMutex
	channels     []AudioChannel
	byID         map[string]int
	mu           sync.RWMutex
	now          map[string]NowPlaying
	ffmpeg       string
	cfg          Config
	configDir    string
	forceRepeat  bool
	forceShuffle bool
	runtimeMu    sync.Mutex
	runtimes     map[string]*channelRuntime
	ffprobe      string
	metadata     *metadataCache
	trackByID    map[string]Track
	artworkMu    sync.Mutex
	artworkCache map[string][]byte
	artworkOrder []string
}

var customIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

var supportedAudio = map[string]bool{
	".aac": true, ".aif": true, ".aiff": true, ".alac": true,
	".flac": true, ".m4a": true, ".m4b": true, ".mp3": true,
	".mp4": true, ".oga": true, ".ogg": true, ".opus": true,
	".wav": true, ".wma": true,
}

func NewLocalProvider(cfg Config, configDir string) (*LocalProvider, error) {
	return newLocalProvider(cfg, configDir, newMetadataCache())
}

func newLocalProvider(cfg Config, configDir string, metadata *metadataCache) (*LocalProvider, error) {
	ffmpeg := strings.TrimSpace(os.Getenv("FFMPEG"))
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	ffmpegPath, err := exec.LookPath(ffmpeg)
	if err != nil {
		return nil, fmt.Errorf("FFmpeg is required to serve local audio (set FFMPEG to its path): %w", err)
	}
	ffprobePath := locateFFprobe(ffmpegPath)
	folderRepeat, fileRepeat := RepeatAll, RepeatOne
	autoShuffle := false
	if cfg.Defaults != nil {
		autoShuffle = cfg.Defaults.Shuffle
		if raw := strings.ToLower(strings.TrimSpace(cfg.Defaults.Repeat)); raw != "" {
			configuredRepeat := RepeatMode(raw)
			if configuredRepeat != RepeatOff && configuredRepeat != RepeatAll && configuredRepeat != RepeatOne {
				return nil, fmt.Errorf("defaults.repeat must be off, all, or one")
			}
			folderRepeat, fileRepeat = configuredRepeat, configuredRepeat
		}
	}

	channels := make([]AudioChannel, 0)
	byID := make(map[string]int)
	usedChannelIDs := make(map[string]bool)
	for _, definition := range cfg.Channels {
		usedChannelIDs[strings.ToLower(definition.ID)] = true
	}
	add := func(channel AudioChannel) error {
		channel.Tracks = uniqueTracks(channel.Tracks)
		if len(channel.Tracks) == 0 {
			return nil
		}
		if _, exists := byID[channel.Station.ID]; exists {
			return fmt.Errorf("duplicate channel id %q", channel.Station.ID)
		}
		byID[channel.Station.ID] = len(channels)
		usedChannelIDs[strings.ToLower(channel.Station.ID)] = true
		channels = append(channels, channel)
		return nil
	}

	folderPaths := make(map[string][]string)
	allFiles := make(map[string]string)
	allFolders := make(map[string]bool)
	for _, rawRoot := range cfg.MusicDirs {
		root, err := resolveConfiguredPath(configDir, rawRoot)
		if err != nil {
			return nil, fmt.Errorf("music_dirs: %w", err)
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			if err == nil {
				err = fmt.Errorf("not a directory")
			}
			return nil, fmt.Errorf("music directory %q: %w", rawRoot, err)
		}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fmt.Errorf("scan %q: %w", path, walkErr)
			}
			if entry.IsDir() {
				allFolders[path] = true
				return nil
			}
			if entry.Type()&fs.ModeSymlink != 0 || !isAudioPath(path) {
				return nil
			}
			key := pathKey(path)
			if _, exists := allFiles[key]; !exists {
				allFiles[key] = path
			}
			for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
				folderPaths[dir] = append(folderPaths[dir], path)
				if samePath(dir, root) {
					break
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	allPaths := make([]string, 0, len(allFiles))
	for _, path := range allFiles {
		allPaths = append(allPaths, path)
	}
	allTracks := loadTracks(allPaths, ffprobePath, metadata)
	tracksByPath := make(map[string]Track, len(allTracks))
	for _, track := range allTracks {
		tracksByPath[pathKey(track.Path)] = track
	}

	folders := make([]string, 0, len(allFolders))
	for path := range allFolders {
		folders = append(folders, path)
	}
	sort.Slice(folders, func(i, j int) bool { return strings.ToLower(folders[i]) < strings.ToLower(folders[j]) })
	for _, folder := range folders {
		tracks := make([]Track, 0, len(folderPaths[folder]))
		for _, path := range folderPaths[folder] {
			if track, ok := tracksByPath[pathKey(path)]; ok {
				tracks = append(tracks, track)
			}
		}
		if len(tracks) == 0 {
			continue
		}
		name := filepath.Base(folder)
		if name == "." || name == string(filepath.Separator) {
			name = folder
		}
		if err := add(AudioChannel{
			Station: Station{ID: channelIDFromName(name, folder, usedChannelIDs), Name: name},
			Tracks:  tracks,
			Shuffle: autoShuffle,
			Repeat:  folderRepeat,
			rank:    1,
		}); err != nil {
			return nil, err
		}
	}

	files := make([]Track, 0, len(allFiles))
	for _, track := range allTracks {
		files = append(files, track)
	}
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Path) < strings.ToLower(files[j].Path) })
	for _, track := range files {
		stationName := track.Title
		if track.Artist != "" {
			stationName = track.Artist + " - " + track.Title
		}
		if err := add(AudioChannel{
			Station: Station{ID: channelIDFromName(filepath.Base(track.Path), track.Path, usedChannelIDs), Name: stationName},
			Tracks:  []Track{track},
			Shuffle: autoShuffle,
			Repeat:  fileRepeat,
			rank:    2,
		}); err != nil {
			return nil, err
		}
	}

	for _, definition := range cfg.Channels {
		if !customIDPattern.MatchString(definition.ID) {
			return nil, fmt.Errorf("channel id %q must start with a letter or number and contain only letters, numbers, _ or - (max 64 characters)", definition.ID)
		}
		name := strings.TrimSpace(definition.Name)
		if name == "" {
			return nil, fmt.Errorf("channel %q must have a name", definition.ID)
		}
		repeat := RepeatMode(strings.ToLower(strings.TrimSpace(definition.Repeat)))
		if repeat == "" {
			repeat = RepeatAll
		}
		if repeat != RepeatOff && repeat != RepeatAll && repeat != RepeatOne {
			return nil, fmt.Errorf("channel %q repeat must be off, all, or one", definition.ID)
		}
		tracks := make([]Track, 0)
		for _, rawFolder := range definition.Folders {
			folder, err := resolveConfiguredPath(configDir, rawFolder)
			if err != nil {
				return nil, fmt.Errorf("channel %q folder: %w", definition.ID, err)
			}
			found, err := tracksInDirectory(folder, func(paths []string) []Track {
				return loadTracks(paths, ffprobePath, metadata)
			})
			if err != nil {
				return nil, fmt.Errorf("channel %q folder %q: %w", definition.ID, rawFolder, err)
			}
			tracks = append(tracks, found...)
		}
		for _, rawFile := range definition.Files {
			file, err := resolveConfiguredPath(configDir, rawFile)
			if err != nil {
				return nil, fmt.Errorf("channel %q file: %w", definition.ID, err)
			}
			info, err := os.Stat(file)
			if err != nil || !info.Mode().IsRegular() {
				if err == nil {
					err = fmt.Errorf("not a regular file")
				}
				return nil, fmt.Errorf("channel %q file %q: %w", definition.ID, rawFile, err)
			}
			if !isAudioPath(file) {
				return nil, fmt.Errorf("channel %q file %q has an unsupported audio extension", definition.ID, rawFile)
			}
			tracks = append(tracks, loadTracks([]string{file}, ffprobePath, metadata)...)
		}
		if len(tracks) == 0 {
			return nil, fmt.Errorf("channel %q has no audio files; add folders or files", definition.ID)
		}
		if err := add(AudioChannel{
			Station: Station{ID: definition.ID, Name: name},
			Tracks:  tracks,
			Shuffle: definition.Shuffle,
			Repeat:  repeat,
			rank:    0,
		}); err != nil {
			return nil, err
		}
	}

	if len(channels) == 0 {
		return nil, fmt.Errorf("no playable audio found; add a path to music_dirs or define a channel")
	}
	trackByID := make(map[string]Track)
	for _, channel := range channels {
		for _, track := range channel.Tracks {
			trackByID[track.ID] = track
		}
	}
	sort.SliceStable(channels, func(i, j int) bool {
		if channels[i].rank != channels[j].rank {
			return channels[i].rank < channels[j].rank
		}
		left, right := strings.ToLower(channels[i].Station.Name), strings.ToLower(channels[j].Station.Name)
		if left == right {
			return channels[i].Station.ID < channels[j].Station.ID
		}
		return left < right
	})
	byID = make(map[string]int, len(channels))
	for i := range channels {
		byID[channels[i].Station.ID] = i
	}
	return &LocalProvider{
		channels: channels, byID: byID, now: make(map[string]NowPlaying),
		ffmpeg: ffmpegPath, ffprobe: ffprobePath, cfg: cfg, configDir: configDir,
		runtimes: make(map[string]*channelRuntime),
		metadata: metadata, trackByID: trackByID, artworkCache: make(map[string][]byte),
	}, nil
}

func (p *LocalProvider) Stations() []Station {
	p.catalogMu.RLock()
	defer p.catalogMu.RUnlock()
	out := make([]Station, len(p.channels))
	for i := range p.channels {
		out[i] = p.channels[i].Station
	}
	return out
}

// ApplyPlaybackOverrides applies command-line repeat and shuffle switches to
// every channel, including user-defined channels.
func (p *LocalProvider) ApplyPlaybackOverrides(repeat, shuffle bool) {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	p.forceRepeat = p.forceRepeat || repeat
	p.forceShuffle = p.forceShuffle || shuffle
	for i := range p.channels {
		if repeat {
			p.channels[i].Repeat = RepeatAll
		}
		if shuffle {
			p.channels[i].Shuffle = true
		}
	}
}

// Refresh rescans the configured library and atomically publishes it. A bad
// scan leaves the current channel list available to listeners.
func (p *LocalProvider) Refresh() (bool, error) {
	fresh, err := newLocalProvider(p.cfg, p.configDir, p.metadata)
	if err != nil {
		return false, err
	}
	p.catalogMu.Lock()
	if p.forceRepeat || p.forceShuffle {
		for i := range fresh.channels {
			if p.forceRepeat {
				fresh.channels[i].Repeat = RepeatAll
			}
			if p.forceShuffle {
				fresh.channels[i].Shuffle = true
			}
		}
	}
	changed := !reflect.DeepEqual(p.channels, fresh.channels)
	p.channels, p.byID, p.trackByID = fresh.channels, fresh.byID, fresh.trackByID
	p.catalogMu.Unlock()
	if changed {
		p.artworkMu.Lock()
		p.artworkCache = make(map[string][]byte)
		p.artworkOrder = nil
		p.artworkMu.Unlock()
	}

	p.mu.Lock()
	for id := range p.now {
		if _, exists := fresh.byID[id]; !exists {
			delete(p.now, id)
		}
	}
	p.mu.Unlock()
	p.pruneRuntimes(fresh.byID)
	return changed, nil
}

func (p *LocalProvider) pruneRuntimes(keep map[string]int) {
	p.runtimeMu.Lock()
	defer p.runtimeMu.Unlock()
	for id, runtime := range p.runtimes {
		if _, exists := keep[id]; exists {
			continue
		}
		runtime.mu.Lock()
		idle := len(runtime.streams) == 0
		runtime.mu.Unlock()
		if idle {
			delete(p.runtimes, id)
		}
	}
}

func (p *LocalProvider) channel(id string) (AudioChannel, bool) {
	p.catalogMu.RLock()
	defer p.catalogMu.RUnlock()
	i, ok := p.byID[id]
	if !ok {
		return AudioChannel{}, false
	}
	return p.channels[i], true
}

func (p *LocalProvider) NowPlaying(id string) (NowPlaying, error) {
	if _, ok := p.channel(id); !ok {
		return NowPlaying{}, fmt.Errorf("channel %q not found", id)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.now[id], nil
}

func (p *LocalProvider) setNowPlaying(id string, track Track) {
	p.mu.Lock()
	artURL := ""
	if track.HasArtwork {
		artURL = "/api/tracks/" + track.ID + "/art"
	}
	p.now[id] = NowPlaying{Title: track.Title, Artist: track.Artist, Album: track.Album, ArtURL: artURL}
	p.mu.Unlock()
}

func tracksInDirectory(root string, load func([]string) []Track) ([]Track, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory")
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || !isAudioPath(path) {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return load(paths), nil
}

func uniqueTracks(tracks []Track) []Track {
	seen := make(map[string]bool, len(tracks))
	out := make([]Track, 0, len(tracks))
	for _, track := range tracks {
		key := pathKey(track.Path)
		if !seen[key] {
			seen[key] = true
			out = append(out, track)
		}
	}
	return out
}

func isAudioPath(path string) bool { return supportedAudio[strings.ToLower(filepath.Ext(path))] }

func trackFromPath(path string) Track {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	t := Track{ID: stableID("track", path), Path: path, Title: name, ArtworkStream: -1}
	if i := strings.Index(name, " - "); i > 0 && i+3 < len(name) {
		t.Artist, t.Title = strings.TrimSpace(name[:i]), strings.TrimSpace(name[i+3:])
	}
	return t
}

func channelIDFromName(name, path string, used map[string]bool) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			builder.WriteRune(r)
		case r == ' ':
			builder.WriteByte('_')
		default:
			builder.WriteByte('_')
		}
	}
	id := builder.String()
	if id == "" || id == "." || id == ".." {
		id = "channel"
	}
	first := id[0]
	if !((first >= '0' && first <= '9') || (first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z')) {
		id = "channel_" + id
	}
	if len(id) > 64 {
		id = id[:64]
	}
	if !used[strings.ToLower(id)] {
		return id
	}

	sum := sha256.Sum256([]byte(pathKey(path)))
	suffix := "_" + hex.EncodeToString(sum[:4])
	base := id
	for duplicate := 1; ; duplicate++ {
		duplicateSuffix := suffix
		if duplicate > 1 {
			duplicateSuffix += fmt.Sprintf("_%d", duplicate)
		}
		maxBaseLength := 64 - len(duplicateSuffix)
		candidateBase := base
		if len(candidateBase) > maxBaseLength {
			candidateBase = candidateBase[:maxBaseLength]
		}
		candidate := candidateBase + duplicateSuffix
		if !used[strings.ToLower(candidate)] {
			return candidate
		}
	}
}

func stableID(kind, path string) string {
	sum := sha256.Sum256([]byte(pathKey(path)))
	return kind + "-" + hex.EncodeToString(sum[:6])
}

func pathKey(path string) string {
	clean := filepath.Clean(path)
	if os.PathSeparator == '\\' {
		return strings.ToLower(clean)
	}
	return clean
}

func samePath(a, b string) bool { return pathKey(a) == pathKey(b) }
