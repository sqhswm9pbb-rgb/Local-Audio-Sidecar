package sidecar

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type Station struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type StationInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	StreamURL   string `json:"stream_url"`
	ContentType string `json:"content_type"`
	Title       string `json:"title,omitempty"`
	ArtURL      string `json:"art_url,omitempty"`
}

type NowPlaying struct {
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
	ArtURL string `json:"art_url,omitempty"`
}

type Provider interface {
	Stations() []Station
	NowPlaying(id string) (NowPlaying, error)
	Skip(id string) (int, error)
	Artwork(trackID string) ([]byte, error)
	Stream(id string, w http.ResponseWriter, r *http.Request) error
}

type API struct {
	provider Provider
	baseURL  string
}

func NewAPI(provider Provider, baseURL string) *API {
	return &API{provider: provider, baseURL: strings.TrimRight(baseURL, "/")}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (a *API) stations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.Path != "/api/stations" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, a.provider.Stations())
}

func (a *API) station(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/stations/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	}
	id := parts[0]
	station, ok := findStation(a.provider.Stations(), id)
	if !ok {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	}
	switch {
	case len(parts) == 1:
		np, _ := a.provider.NowPlaying(id)
		if np.ArtURL != "" {
			np.ArtURL = a.baseURL + np.ArtURL
		}
		writeJSON(w, http.StatusOK, StationInfo{
			ID: station.ID, Name: station.Name,
			StreamURL:   a.baseURL + "/api/stations/" + url.PathEscape(station.ID) + "/stream",
			ContentType: "audio/mpeg", Title: np.Title, ArtURL: np.ArtURL,
		})
	case len(parts) == 2 && parts[1] == "now-playing":
		np, err := a.provider.NowPlaying(id)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if np.ArtURL != "" {
			np.ArtURL = a.baseURL + np.ArtURL
		}
		writeJSON(w, http.StatusOK, np)
	case len(parts) == 2 && parts[1] == "stream":
		if r.URL.Query().Has("skip") {
			active, err := a.provider.Skip(id)
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			if active == 0 {
				writeError(w, http.StatusConflict, "no active stream for this channel")
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status":         "skip_requested",
				"active_streams": active,
			})
			return
		}
		if err := a.provider.Stream(id, w, r); err != nil {
			if errors.Is(err, http.ErrAbortHandler) {
				return
			}
			if w.Header().Get("Content-Type") == "" {
				writeError(w, http.StatusBadGateway, err.Error())
			}
		}
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func findStation(stations []Station, id string) (Station, bool) {
	for _, station := range stations {
		if station.ID == id {
			return station, true
		}
	}
	return Station{}, false
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/stations", a.stations)
	mux.HandleFunc("/api/stations/", a.station)
	mux.HandleFunc("/api/tracks/", a.track)
	return mux
}

func (a *API) track(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "tracks" || parts[3] != "art" {
		writeError(w, http.StatusNotFound, "track artwork not found")
		return
	}
	artwork, err := a.provider.Artwork(parts[2])
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(artwork)
}

func ValidateBaseURL(baseURL string) error {
	if strings.TrimSpace(baseURL) == "" || strings.ContainsAny(baseURL, " \t\r\n") {
		return fmt.Errorf("base URL must be a complete HTTP or HTTPS address")
	}
	u, err := url.ParseRequestURI(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("base URL must be a complete HTTP or HTTPS address without credentials, query, or fragment")
	}
	return nil
}
