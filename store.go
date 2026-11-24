package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Participant struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	// Username is the Discord username (the unique @handle, unlike Name,
	// which is the server nickname). Refreshed whenever they update their
	// wishlist or set a gift, so the JSON shows who sent each gift. Never
	// shown to the recipient.
	Username string `json:"username,omitempty"`
	Wishlist string `json:"wishlist"`
	// Gift is what this person (as a Santa) is giving their recipient: a note,
	// link... Held until the organizer runs /santa end.
	Gift string `json:"gift,omitempty"`
	// Attachments are the gift's attachment files, in this round's attachment folder.
	Attachments []string `json:"attachments,omitempty"`
	// RecipientUsername is the username of the person this Santa is buying
	// for, so each entry reads sender → recipient without looking up IDs in
	// Assignments. See FillRecipientUsernames.
	RecipientUsername string `json:"recipient_username,omitempty"`
}

// Event is one Secret Santa round in one server. One active event per server.
type Event struct {
	GuildID     string                  `json:"guild_id"`
	ChannelID   string                  `json:"channel_id"`
	OrganizerID string                  `json:"organizer_id"`
	Date        string                  `json:"date"`
	CreatedAt   time.Time               `json:"created_at"`
	Joined      []string                `json:"joined"` // join order, for stable listing
	People      map[string]*Participant `json:"people"`
	// Exclusions[a][b] = true means a asked not to be paired with b.
	// Applied in both directions at draw time.
	Exclusions  map[string]map[string]bool `json:"exclusions"`
	Drawn       bool                       `json:"drawn"`
	Assignments map[string]string          `json:"assignments"` // giver → recipient
}

// Key names this round's attachment and backup folders. It includes the
// creation time so a new round in the same server never mixes with an old one.
func (e *Event) Key() string { return fmt.Sprintf("%s-%d", e.GuildID, e.CreatedAt.UnixNano()) }

// FillRecipientUsernames copies each recipient's current username onto their
// Santa's entry. Called after the draw, whenever someone's username is
// refreshed, and just before the round is backed up, so it's never stale for long.
func (e *Event) FillRecipientUsernames() {
	for santa, recipient := range e.Assignments {
		if s, r := e.People[santa], e.People[recipient]; s != nil && r != nil {
			s.RecipientUsername = r.Username
		}
	}
}

func (e *Event) Excluded(a, b string) bool { return e.Exclusions[a][b] }

func (e *Event) SantaOf(recipient string) (string, bool) {
	for g, r := range e.Assignments {
		if r == recipient {
			return g, true
		}
	}
	return "", false
}

func (e *Event) Remove(id string) {
	delete(e.People, id)
	delete(e.Exclusions, id)
	for _, m := range e.Exclusions {
		delete(m, id)
	}
	for i, j := range e.Joined {
		if j == id {
			e.Joined = append(e.Joined[:i], e.Joined[i+1:]...)
			break
		}
	}
}

// Store keeps every event in one JSON file. Small scale on purpose: a
// Secret Santa bot handles a handful of servers with tens of people each.
type Store struct {
	mu     sync.Mutex
	path   string
	Events map[string]*Event `json:"events"` // guild ID → event
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, Events: map[string]*Event{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Events == nil {
		s.Events = map[string]*Event{}
	}
	return s, nil
}

// Update runs fn with the lock held and saves if fn returns nil.
// All reads and writes go through here so handlers never race.
func (s *Store) Update(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(); err != nil {
		return err
	}
	return s.save()
}

// View runs fn with the lock held and does not save.
func (s *Store) View(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

// save writes to a temp file then renames, so a crash mid-write can't
// leave a half-written (and unparseable) data file behind.
func (s *Store) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".santa-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// BackupEvent saves one round, just as it was when it ended, into its own
// folder under dir and returns that folder:
//
//	santa-backups/<ended at>-<guildID>/santa.json
//
// The file has the same shape as the main santa.json but holds only this
// round. Backing up the whole main file instead would copy other servers'
// rounds that are still running (their pairings and gifts) into every backup.
func BackupEvent(dir string, e *Event, ended time.Time) (string, error) {
	folder := filepath.Join(dir, fmt.Sprintf("%s-%s", ended.Format("2006-01-02_15-04-05"), e.GuildID))
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(map[string]any{"events": map[string]*Event{e.GuildID: e}}, "", "  ")
	if err != nil {
		return "", err
	}
	return folder, os.WriteFile(filepath.Join(folder, "santa.json"), b, 0o600)
}
