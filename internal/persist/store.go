package persist

import (
	"atlas-render/internal/model"
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Store owns one state file. Every process start publishes a new generation
// into the file; a process whose generation no longer matches the file is
// fenced and refuses all further commits. The advisory lock only protects
// processes that share the same file, so ownership is also re-checked before
// every commit and by the service watchdog.
type Store struct {
	mu         sync.RWMutex
	path       string
	state      model.State
	lock       *os.File
	write      func(string, []byte) error
	writer     string
	generation int64
	adoptedBy  string
	fenced     error
}

type Options struct {
	// Writer identifies this process start in status, events and fencing.
	Writer string
	// Adopt verifies a handoff manifest against the state before this process
	// claims it. Nil means this is an ordinary restart of the same state path.
	Adopt func(model.State) error
	// ManifestPath is recorded in the adoption marker written on success.
	ManifestPath string
}

func LockPath(state string) string      { return state + ".lock" }
func TombstonePath(state string) string { return state + ".superseded" }
func AdoptionPath(state string) string  { return state + ".adoption" }

func Open(path string) (*Store, error) { return OpenWith(path, Options{}) }

func OpenWith(path string, opts Options) (*Store, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	if e := CheckSuperseded(path); e != nil {
		return nil, e
	}
	lock, e := AcquireLock(path)
	if e != nil {
		return nil, e
	}
	s := &Store{path: path, state: model.NewState(), lock: lock, write: AtomicWrite, writer: opts.Writer}
	if s.writer == "" {
		s.writer = DefaultWriter()
	}
	data, e := os.ReadFile(path)
	exists := e == nil
	if e != nil && !os.IsNotExist(e) {
		s.Close()
		return nil, e
	}
	if exists {
		if e = json.Unmarshal(data, &s.state); e != nil {
			s.Close()
			return nil, fmt.Errorf("invalid persisted JSON: %w", e)
		}
		// States written before ownership metadata existed are adopted as
		// generation 1 by "legacy" and immediately claimed below.
		if s.state.Generation < 1 {
			s.state.Generation = 1
		}
		if s.state.Writer == "" {
			s.state.Writer = "legacy"
		}
		if e = Validate(s.state); e != nil {
			s.Close()
			return nil, e
		}
		if opts.Adopt != nil {
			if e = opts.Adopt(s.state); e != nil {
				s.Close()
				return nil, e
			}
			s.adoptedBy = opts.ManifestPath
		}
	} else {
		if opts.Adopt != nil {
			s.Close()
			return nil, fmt.Errorf("handoff adoption requires an existing prepared state at %s", path)
		}
		s.state = model.NewState()
	}
	next := s.state.Generation + 1
	if !exists {
		next = 1
	}
	s.state.Generation = next
	s.state.Writer = s.writer
	s.state.Emit("process.claimed", s.writer)
	if s.adoptedBy != "" {
		s.state.Emit("handoff.adopted", s.adoptedBy)
	}
	// Take-over publishes the new generation before any use of the state. If
	// the write fails, the previous file (and its generation) is untouched.
	if e = s.write(s.path, mustJSON(s.state)); e != nil {
		s.Close()
		return nil, fmt.Errorf("claim state generation: %w", e)
	}
	s.generation = next
	// The previous owner may still be running with this path open on another
	// host or in a copied directory. Removing its lock file does not release
	// its lock, but the reported generation now fences it on its next commit.
	// The adoption marker is written only after the claim landed.
	if s.adoptedBy != "" {
		marker := fmt.Sprintf("manifest=%s\ngeneration=%d\nwriter=%s\nclaimedAt=%s\n", s.adoptedBy, next, s.writer, time.Now().UTC().Format(time.RFC3339Nano))
		if e = AtomicWrite(AdoptionPath(path), []byte(marker)); e != nil {
			s.Close()
			return nil, fmt.Errorf("record adoption marker: %w", e)
		}
	}
	return s, nil
}

func DefaultWriter() string {
	host, e := os.Hostname()
	if e != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

func mustJSON(st model.State) []byte {
	b, e := json.Marshal(st)
	if e != nil {
		panic(e)
	}
	return b
}

func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	e := s.lock.Close()
	s.lock = nil
	return e
}

func (s *Store) Read() model.State { s.mu.RLock(); defer s.mu.RUnlock(); return model.Clone(s.state) }

func (s *Store) Generation() int64 { s.mu.RLock(); defer s.mu.RUnlock(); return s.generation }
func (s *Store) Writer() string    { s.mu.RLock(); defer s.mu.RUnlock(); return s.writer }

// Fenced reports a cached takeover. It is cheap enough for the HTTP hot path.
func (s *Store) Fenced() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fenced
}

// VerifyOwnership re-reads the supersede marker and on-disk generation and
// fences this process when another writer has claimed the state file. It is
// called by the service watchdog and on every request, so a takeover is not
// only noticed at the next write.
func (s *Store) VerifyOwnership() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkOwnership()
}

func (s *Store) checkOwnership() error {
	if s.fenced != nil {
		return s.fenced
	}
	// A supersede marker outranks the generation: even a file copied back to
	// its old generation must never be served from a path that was handed off.
	if e := CheckSuperseded(s.path); e != nil {
		s.fenced = e
		return e
	}
	gen, writer, e := ReadHeader(s.path)
	if e != nil {
		s.fenced = model.Unavailable("state ownership cannot be verified: %v", e)
		return s.fenced
	}
	if gen != s.generation || writer != s.writer {
		s.fenced = model.Unavailable("state was claimed by another writer (generation %d, writer %q); this process (generation %d, writer %q) is fenced", gen, writer, s.generation, s.writer)
		return s.fenced
	}
	return nil
}

func (s *Store) Update(fn func(*model.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.checkOwnership(); e != nil {
		return e
	}
	next := model.Clone(s.state)
	if e := fn(&next); e != nil {
		return e
	}
	if e := Validate(next); e != nil {
		return e
	}
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	if e = s.write(s.path, b); e != nil {
		return fmt.Errorf("persist state: %w", e)
	}
	// Post-write verification closes the check-then-write window: if another
	// writer replaced the file after our check, this process learns it lost
	// instead of silently serving facts the other writer never saw.
	if gen, writer, e := ReadHeader(s.path); e != nil || gen != s.generation || writer != s.writer {
		s.fenced = model.Unavailable("state ownership was lost while committing; this process is fenced")
		return s.fenced
	}
	s.state = next
	return nil
}

// MarkStopped records a clean drain. It is best effort: an abrupt stop simply
// leaves the task states as they are, which startup recovery already handles.
func (s *Store) MarkStopped() error {
	return s.Update(func(st *model.State) error {
		st.Emit("process.stopped", st.Writer)
		return nil
	})
}

// AdoptedBy reports the manifest that let this process adopt the state, if any.
func (s *Store) AdoptedBy() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.adoptedBy
}

// ReadHeader decodes only the leading ownership fields of the state file.
func ReadHeader(path string) (int64, string, error) {
	f, e := os.Open(path)
	if e != nil {
		return 0, "", e
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	t, e := dec.Token()
	if e != nil {
		return 0, "", fmt.Errorf("decode state header: %w", e)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return 0, "", fmt.Errorf("state file is not a JSON object")
	}
	var gen int64
	var writer string
	var haveGen, haveWriter bool
	for dec.More() {
		t, e = dec.Token()
		if e != nil {
			return 0, "", fmt.Errorf("decode state header: %w", e)
		}
		key, ok := t.(string)
		if !ok {
			return 0, "", fmt.Errorf("decode state header: unexpected key")
		}
		switch key {
		case "generation":
			if e = dec.Decode(&gen); e != nil {
				return 0, "", fmt.Errorf("decode state generation: %w", e)
			}
			haveGen = true
		case "writer":
			if e = dec.Decode(&writer); e != nil {
				return 0, "", fmt.Errorf("decode state writer: %w", e)
			}
			haveWriter = true
		default:
			var skip json.RawMessage
			if e = dec.Decode(&skip); e != nil {
				return 0, "", fmt.Errorf("decode state header: %w", e)
			}
		}
		if haveGen && haveWriter {
			break
		}
	}
	if !haveGen {
		return 0, "", fmt.Errorf("state has no generation")
	}
	if !haveWriter {
		return 0, "", fmt.Errorf("state has no writer")
	}
	return gen, writer, nil
}

type Tombstone struct {
	Format         string `json:"format"`
	Version        int    `json:"version"`
	StateDigest    string `json:"stateDigest"`
	Generation     int64  `json:"generation"`
	ManifestDigest string `json:"manifestDigest"`
	Destination    string `json:"destination"`
	SupersededAt   string `json:"supersededAt"`
}

const TombstoneFormat = "atlas-superseded"

func CheckSuperseded(statePath string) error {
	data, e := os.ReadFile(TombstonePath(statePath))
	if e != nil {
		if os.IsNotExist(e) {
			return nil
		}
		return fmt.Errorf("state supersede marker cannot be read: %w", e)
	}
	var t Tombstone
	if e = json.Unmarshal(data, &t); e != nil || t.Format != TombstoneFormat || t.Version != 1 {
		return fmt.Errorf("state supersede marker is invalid; refusing to start")
	}
	return model.Unavailable("state was superseded by handoff to %q (manifest %s, generation %d); refusing to start or serve", t.Destination, t.ManifestDigest, t.Generation)
}

func AcquireLock(state string) (*os.File, error) {
	lock, e := os.OpenFile(LockPath(state), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, fmt.Errorf("state is already opened by another process: %w", e)
	}
	return lock, nil
}

func AtomicWrite(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".atlas-write-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return SyncDir(filepath.Dir(path))
}

func SyncDir(dir string) error {
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
