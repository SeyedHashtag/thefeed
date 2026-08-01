package telemirror

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store persists the user-added channel list. Defaults are listed first, and
// stay there unless the user removes one — List() re-adds them on every call,
// so a removal has to be recorded explicitly in Hidden.
type Store struct {
	path       string
	titlesPath string
	mu         sync.Mutex
	titles     map[string]string // in-memory cache, nil until first load
}

func NewStore(dataDir string) *Store {
	return &Store{
		path:       filepath.Join(dataDir, "telemirror_channels.json"),
		titlesPath: filepath.Join(dataDir, "telemirror_titles.json"),
	}
}

type subsFile struct {
	Channels []string `json:"channels"`
	// Hidden holds defaults the user removed. Absent in files written by
	// older builds, which simply means nothing is hidden.
	Hidden []string `json:"hidden,omitempty"`
}

func (s *Store) loadFileLocked() subsFile {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return subsFile{}
	}
	var f subsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return subsFile{}
	}
	return f
}

func (s *Store) saveFileLocked(f subsFile) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0600)
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// List returns the full channel list with defaults pinned to the front.
func (s *Store) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.loadFileLocked()
	user := f.Channels
	seen := make(map[string]bool, len(DefaultChannels)+len(user))
	out := make([]string, 0, len(DefaultChannels)+len(user))
	for _, d := range DefaultChannels {
		if containsFold(f.Hidden, d) {
			continue
		}
		seen[strings.ToLower(d)] = true
		out = append(out, d)
	}
	for _, u := range user {
		clean := SanitizeUsername(u)
		if clean == "" || seen[strings.ToLower(clean)] {
			continue
		}
		seen[strings.ToLower(clean)] = true
		out = append(out, clean)
	}
	return out
}

func (s *Store) Add(username string) error {
	username = SanitizeUsername(username)
	if username == "" {
		return ErrEmptyUsername
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.loadFileLocked()
	// Re-adding a default just un-hides it; List() supplies the entry.
	if IsDefault(username) {
		if !containsFold(f.Hidden, username) {
			return nil
		}
		kept := f.Hidden[:0]
		for _, h := range f.Hidden {
			if !strings.EqualFold(h, username) {
				kept = append(kept, h)
			}
		}
		f.Hidden = kept
		return s.saveFileLocked(f)
	}
	if containsFold(f.Channels, username) {
		return nil
	}
	f.Channels = append(f.Channels, username)
	return s.saveFileLocked(f)
}

// --- channel titles (persisted server-side so they're shared across ports,
// not stuck in one browser's localStorage) ---

// ensureTitlesLocked loads the titles file into the in-memory cache once.
func (s *Store) ensureTitlesLocked() {
	if s.titles != nil {
		return
	}
	s.titles = map[string]string{}
	b, err := os.ReadFile(s.titlesPath)
	if err != nil {
		return
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err == nil && m != nil {
		s.titles = m
	}
}

func (s *Store) saveTitlesLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.titlesPath), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.titles, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.titlesPath, b, 0600)
}

// SetTitle records/updates a channel's latest display title (keyed by lowercase
// username). Called whenever a channel is fetched, so the newest title wins.
// In-memory cache → no disk read per call; only writes when the title changed.
func (s *Store) SetTitle(username, title string) {
	username = strings.ToLower(SanitizeUsername(username))
	title = strings.TrimSpace(title)
	if username == "" || title == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureTitlesLocked()
	if s.titles[username] == title {
		return
	}
	s.titles[username] = title
	_ = s.saveTitlesLocked()
}

// Titles returns a COPY of the lowercase-username → title map (callers must not
// mutate the cache).
func (s *Store) Titles() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureTitlesLocked()
	out := make(map[string]string, len(s.titles))
	for k, v := range s.titles {
		out[k] = v
	}
	return out
}

func (s *Store) Remove(username string) error {
	username = SanitizeUsername(username)
	if username == "" {
		return ErrEmptyUsername
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.loadFileLocked()
	// A default is re-added by List() on every call, so record the removal.
	// Also drop any user-list copy, or List()'s second loop re-adds it.
	if IsDefault(username) {
		if !containsFold(f.Hidden, username) {
			f.Hidden = append(f.Hidden, username)
		}
		kept := f.Channels[:0]
		for _, u := range f.Channels {
			if !strings.EqualFold(u, username) {
				kept = append(kept, u)
			}
		}
		f.Channels = kept
		return s.saveFileLocked(f)
	}
	out := f.Channels[:0]
	for _, u := range f.Channels {
		if !strings.EqualFold(u, username) {
			out = append(out, u)
		}
	}
	f.Channels = out
	return s.saveFileLocked(f)
}
