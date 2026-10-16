package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sidecar "github.com/codester28351/local-audio-sidecar"
)

type options struct {
	positionals   []string
	config        string
	output        string
	port          string
	watchInterval time.Duration
	generate      bool
	temp          bool
	repeat        bool
	shuffle       bool
	force         bool
	watch         bool
	mix           bool
	help          bool
}

const helpText = `Local Audio Sidecar streams local music folders to STR.

Usage:
  local-audio-sidecar [config.json] [options]
  local-audio-sidecar MUSIC_FOLDER --generate [--output music.json] [--force]
  local-audio-sidecar MUSIC_FOLDER... --temp [--repeat] [--shuffle] [--watch] [--port PORT]
  local-audio-sidecar MUSIC_FOLDER MUSIC_FOLDER --mix [--shuffle] [--watch]

Options:
  --generate       Scan one MUSIC_FOLDER and write music.json, then exit.
  --output FILE    Choose the generated config path (only with --generate).
  --force          Replace an existing generated config (only with --generate).
  --temp           Use MUSIC_FOLDER directly without reading or writing JSON.
  --repeat         Repeat all channels continuously.
  --shuffle        Shuffle tracks within each channel.
  --mix            Add a combination channel for all supplied folders (needs 2+).
  --watch          Rescan folders for added or removed tracks (default: every 30s).
  --watch-interval Duration between scans, e.g. 10s or 2m (only with --watch).
  --port PORT      Listen on PORT (default: 8092).
  --config FILE    Load a config file (default: ./music.json).
  -h, --help       Show this help.

Examples:
  local-audio-sidecar.exe C:\Music --generate
  local-audio-sidecar.exe C:\Music --temp --repeat --port 8095
  local-audio-sidecar.exe C:\Music "C:\Users\you\Rolling Stones" --shuffle --mix --watch
  local-audio-sidecar.exe --config D:\STR\music.json --port 8092

The server prints the LAN URL to enter in STR's Stream source settings.
Append ?skip=1 to a channel's /api/stations/{id}/stream URL to skip its
currently playing track.
`

func parseOptions(args []string) (options, error) {
	var opts options
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			opts.positionals = append(opts.positionals, args[i+1:]...)
			break
		}
		if arg == "-h" || arg == "--help" {
			opts.help = true
			return opts, nil
		}
		if strings.HasPrefix(arg, "--port=") {
			opts.port = strings.TrimPrefix(arg, "--port=")
			if opts.port == "" {
				return opts, fmt.Errorf("--port needs a value")
			}
			continue
		}
		if strings.HasPrefix(arg, "--config=") {
			opts.config = strings.TrimPrefix(arg, "--config=")
			if opts.config == "" {
				return opts, fmt.Errorf("--config needs a value")
			}
			continue
		}
		if strings.HasPrefix(arg, "--output=") {
			opts.output = strings.TrimPrefix(arg, "--output=")
			if opts.output == "" {
				return opts, fmt.Errorf("--output needs a value")
			}
			continue
		}
		if strings.HasPrefix(arg, "--watch-interval=") {
			value := strings.TrimPrefix(arg, "--watch-interval=")
			interval, err := time.ParseDuration(value)
			if err != nil || interval < 5*time.Second {
				return opts, fmt.Errorf("--watch-interval must be at least 5s, such as 15s or 1m")
			}
			opts.watchInterval = interval
			continue
		}
		switch arg {
		case "--generate":
			opts.generate = true
		case "--temp":
			opts.temp = true
		case "--repeat":
			opts.repeat = true
		case "--shuffle":
			opts.shuffle = true
		case "--watch":
			opts.watch = true
		case "--mix":
			opts.mix = true
		case "--force":
			opts.force = true
		case "--port", "-p":
			value, err := nextValue(args, &i, arg)
			if err != nil {
				return opts, err
			}
			opts.port = value
		case "--config":
			value, err := nextValue(args, &i, arg)
			if err != nil {
				return opts, err
			}
			opts.config = value
		case "--output":
			value, err := nextValue(args, &i, arg)
			if err != nil {
				return opts, err
			}
			opts.output = value
		case "--watch-interval":
			value, err := nextValue(args, &i, arg)
			if err != nil {
				return opts, err
			}
			interval, err := time.ParseDuration(value)
			if err != nil || interval < 5*time.Second {
				return opts, fmt.Errorf("--watch-interval must be at least 5s, such as 15s or 1m")
			}
			opts.watchInterval = interval
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown option %q", arg)
			}
			opts.positionals = append(opts.positionals, arg)
		}
	}
	if opts.config != "" && len(opts.positionals) != 0 {
		return opts, fmt.Errorf("use either a positional config path or --config, not both")
	}
	if opts.mix && len(opts.positionals) < 2 {
		return opts, fmt.Errorf("--mix needs at least two music folders")
	}
	if opts.generate && len(opts.positionals) != 1 {
		return opts, fmt.Errorf("--generate needs exactly one music folder")
	}
	if len(opts.positionals) > 1 && !opts.generate && !opts.temp && !opts.mix {
		return opts, fmt.Errorf("multiple folder paths need --temp or --mix")
	}
	if opts.port != "" {
		port, err := strconv.Atoi(opts.port)
		if err != nil || port < 1 || port > 65535 {
			return opts, fmt.Errorf("port must be a number from 1 to 65535")
		}
	}
	if opts.generate && opts.temp {
		return opts, fmt.Errorf("--generate and --temp cannot be used together")
	}
	if opts.generate && opts.config != "" {
		return opts, fmt.Errorf("--config cannot be used with --generate")
	}
	if opts.generate && (opts.port != "" || opts.repeat || opts.shuffle || opts.watch || opts.watchInterval != 0) {
		return opts, fmt.Errorf("playback options cannot be used with --generate")
	}
	if (opts.temp || opts.mix) && opts.config != "" {
		return opts, fmt.Errorf("--config cannot be used with --temp or --mix")
	}
	if opts.generate && opts.mix {
		return opts, fmt.Errorf("--mix cannot be used with --generate")
	}
	if opts.watchInterval != 0 && !opts.watch {
		return opts, fmt.Errorf("--watch-interval requires --watch")
	}
	if !opts.generate && (opts.output != "" || opts.force) {
		return opts, fmt.Errorf("--output and --force can only be used with --generate")
	}
	return opts, nil
}

func nextValue(args []string, i *int, flag string) (string, error) {
	if *i+1 >= len(args) || strings.HasPrefix(args[*i+1], "-") {
		return "", fmt.Errorf("%s needs a value", flag)
	}
	*i++
	return args[*i], nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func detectLANIP() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil || ipNet.IP.To4() == nil || ipNet.IP.IsLoopback() {
				continue
			}
			return ipNet.IP.String()
		}
	}
	return "127.0.0.1"
}

func run() error {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, helpText)
	}
	if opts.help {
		fmt.Print(helpText)
		return nil
	}
	if opts.generate {
		if len(opts.positionals) != 1 {
			return fmt.Errorf("--generate needs a music folder\n\n%s", helpText)
		}
		count, err := sidecar.GenerateMusicConfig(opts.positionals[0], opts.output, opts.force)
		if err != nil {
			return err
		}
		output := opts.output
		if output == "" {
			output = "music.json"
		}
		fmt.Printf("Generated %s for %d audio files.\n", output, count)
		return nil
	}

	var cfg sidecar.Config
	var configDir string
	if opts.temp || opts.mix {
		if len(opts.positionals) == 0 {
			return fmt.Errorf("--temp needs at least one music folder\n\n%s", helpText)
		}
		repeat := "off"
		if opts.repeat {
			repeat = "all"
		}
		folders := make([]string, 0, len(opts.positionals))
		folderNames := make([]string, 0, len(opts.positionals))
		for _, folder := range opts.positionals {
			absFolder, err := filepath.Abs(folder)
			if err != nil {
				return fmt.Errorf("resolve music folder %q: %w", folder, err)
			}
			info, err := os.Stat(absFolder)
			if err != nil {
				return fmt.Errorf("music folder %q: %w", folder, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("music folder %q is not a directory", folder)
			}
			folders = append(folders, absFolder)
			folderNames = append(folderNames, filepath.Base(filepath.Clean(absFolder)))
		}
		cfg = sidecar.Config{
			MusicDirs: folders,
			Defaults:  &sidecar.ChannelDefaults{Shuffle: opts.shuffle, Repeat: repeat},
		}
		if opts.mix {
			cfg.Channels = append(cfg.Channels, sidecar.ChannelConfig{
				ID: "combined-mix", Name: "Mix: " + strings.Join(folderNames, " + "),
				Folders: folders, Shuffle: opts.shuffle, Repeat: repeat,
			})
		}
		configDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get current directory: %w", err)
		}
	} else {
		configPath := opts.config
		if configPath == "" && len(opts.positionals) == 1 {
			configPath = opts.positionals[0]
		}
		if configPath == "" {
			configPath = envOr("LOCAL_AUDIO_CONFIG", "music.json")
		}
		cfg, configDir, err = sidecar.LoadConfig(configPath)
		if err != nil {
			return err
		}
	}
	provider, err := sidecar.NewLocalProvider(cfg, configDir)
	if err != nil {
		return err
	}
	provider.ApplyPlaybackOverrides(opts.repeat, opts.shuffle)

	host := envOr("SIDECAR_HOST", "0.0.0.0")
	port := envOr("SIDECAR_PORT", "8092")
	if opts.port != "" {
		port = opts.port
	}
	if parsed, err := strconv.Atoi(port); err != nil || parsed < 1 || parsed > 65535 {
		return fmt.Errorf("SIDECAR_PORT must be a number from 1 to 65535")
	}
	baseURL := envOr("SIDECAR_BASE_URL", "http://"+detectLANIP()+":"+port)
	if err := sidecar.ValidateBaseURL(baseURL); err != nil {
		return err
	}
	addr := net.JoinHostPort(host, port)
	server := &http.Server{
		Addr: addr, Handler: sidecar.NewAPI(provider, baseURL).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	log.Printf("Local audio sidecar listening on %s", addr)
	log.Printf("LAN base URL: %s", baseURL)
	log.Printf("Channels: %d", len(provider.Stations()))
	if opts.watch {
		interval := opts.watchInterval
		if interval == 0 {
			interval = 30 * time.Second
		}
		log.Printf("Folder watch enabled (rescan every %s)", interval)
		go watchLibrary(provider, interval)
	}
	return server.ListenAndServe()
}

func watchLibrary(provider *sidecar.LocalProvider, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		changed, err := provider.Refresh()
		if err != nil {
			log.Printf("folder watch scan failed; keeping the previous channel list: %v", err)
			continue
		}
		if changed {
			log.Printf("folder watch refreshed %d channels", len(provider.Stations()))
		}
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
