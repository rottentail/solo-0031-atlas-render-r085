// Package handoff defines the verifiable record used to move a single-writer
// state path to another host and to fence the old owner.
package handoff

import (
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	Format  = "atlas-handoff"
	Version = 1
)

// Manifest is produced from a stopped source, before the source is fenced.
// It records the ownership generation that was captured and a digest over the
// facts that must survive the move: pinned inputs, accepted promises and
// cleaned results.
type Manifest struct {
	Format      string    `json:"format"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"createdAt"`
	Destination string    `json:"destination"`
	Source      Source    `json:"source"`
	Counts      Counts    `json:"counts"`
	Digest      string    `json:"digest"`
}

type Source struct {
	StatePath   string `json:"statePath"`
	StateDigest string `json:"stateDigest"`
	Generation  int64  `json:"generation"`
	Writer      string `json:"writer"`
	Sequence    int    `json:"sequence"`
	FactsDigest string `json:"factsDigest"`
}

type Counts struct {
	Scenes    int `json:"scenes"`
	Assets    int `json:"assets"`
	Snapshots int `json:"snapshots"`
	Jobs      int `json:"jobs"`
	Artifacts int `json:"artifacts"`
	Receipts  int `json:"receipts"`
}

type factSet struct {
	Scenes    map[string]sceneFact `json:"scenes"`
	Assets    map[string]assetFact `json:"assets"`
	Snapshots map[string]snapFact  `json:"snapshots"`
	Jobs      map[string]jobFact   `json:"jobs"`
	Artifacts map[string]artFact   `json:"artifacts"`
	Receipts  map[string]recFact   `json:"receipts"`
	Sequence  int                  `json:"sequence"`
}

type sceneFact struct {
	Revision int    `json:"revision"`
	State    string `json:"state"`
	Document string `json:"document"`
}
type assetFact struct {
	Digest string `json:"digest"`
	State  string `json:"state"`
}
type snapFact struct {
	SceneID string `json:"sceneId"`
	State   string `json:"state"`
	Digest  string `json:"digest"`
}
type jobFact struct {
	SnapshotID string `json:"snapshotId"`
	Scale      int    `json:"scale"`
	Attempt    int    `json:"attempt"`
	State      string `json:"state"`
	ArtifactID string `json:"artifactId"`
	HasError   bool   `json:"hasError"`
	Input      string `json:"input"`
}
type artFact struct {
	State  string `json:"state"`
	Digest string `json:"digest"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}
type recFact struct {
	Fingerprint string `json:"fingerprint"`
	JobID       string `json:"jobId"`
}

// FactsDigest covers exactly the facts a cutover promises to preserve:
// fixed snapshot/payload inputs, task promises (state, attempt, artifact),
// receipts, and cleaned artifacts whose bytes are already gone.
func FactsDigest(st model.State) string {
	f := factSet{
		Scenes:    map[string]sceneFact{},
		Assets:    map[string]assetFact{},
		Snapshots: map[string]snapFact{},
		Jobs:      map[string]jobFact{},
		Artifacts: map[string]artFact{},
		Receipts:  map[string]recFact{},
		Sequence:  st.Sequence,
	}
	for id, v := range st.Scenes {
		f.Scenes[id] = sceneFact{Revision: v.Revision, State: v.State, Document: model.CanonicalDigest(v.Document)}
	}
	for id, v := range st.Assets {
		f.Assets[id] = assetFact{Digest: v.Digest, State: v.State}
	}
	for id, v := range st.Snapshots {
		f.Snapshots[id] = snapFact{SceneID: v.SceneID, State: v.State, Digest: v.Digest}
	}
	for id, v := range st.Jobs {
		// Payload carries the pinned copy of document and asset bytes, so its
		// digest is the fixed input of a retried or recovered attempt.
		f.Jobs[id] = jobFact{SnapshotID: v.SnapshotID, Scale: v.Scale, Attempt: v.Attempt, State: v.State, ArtifactID: v.ArtifactID, HasError: v.Error != "", Input: model.SnapshotDigest(v.Payload)}
	}
	for id, v := range st.Artifacts {
		f.Artifacts[id] = artFact{State: v.State, Digest: v.Digest, Width: v.Width, Height: v.Height}
	}
	for k, v := range st.Receipts {
		f.Receipts[k] = recFact{Fingerprint: v.Fingerprint, JobID: v.JobID}
	}
	return model.CanonicalDigest(f)
}

func CountsOf(st model.State) Counts {
	return Counts{Scenes: len(st.Scenes), Assets: len(st.Assets), Snapshots: len(st.Snapshots), Jobs: len(st.Jobs), Artifacts: len(st.Artifacts), Receipts: len(st.Receipts)}
}

// seal computes the manifest digest over every field except Digest itself.
func (m *Manifest) seal() {
	m.Digest = model.CanonicalDigest(struct {
		Format      string
		Version     int
		CreatedAt   time.Time
		Destination string
		Source      Source
		Counts      Counts
	}{m.Format, m.Version, m.CreatedAt, m.Destination, m.Source, m.Counts})
}

func (m Manifest) Verify() error {
	if m.Format != Format || m.Version != Version {
		return model.Invalid("unsupported handoff manifest")
	}
	probe := m
	probe.Digest = ""
	probe.seal()
	if probe.Digest != m.Digest {
		return model.Invalid("handoff manifest digest mismatch")
	}
	if m.Source.Generation < 1 || m.Source.FactsDigest == "" || m.Source.StateDigest == "" || m.Destination == "" {
		return model.Invalid("handoff manifest is incomplete")
	}
	return nil
}

// VerifyState checks that st is exactly the stopped snapshot recorded in the
// manifest. It rejects a file that was edited, replaced or advanced by another
// writer between capture and adoption.
func (m Manifest) VerifyState(st model.State) error {
	if st.Generation != m.Source.Generation || st.Writer != m.Source.Writer {
		return model.Conflict(fmt.Sprintf("handoff state ownership changed: generation %d writer %q, expected generation %d writer %q", st.Generation, st.Writer, m.Source.Generation, m.Source.Writer))
	}
	if st.Sequence != m.Source.Sequence {
		return model.Conflict(fmt.Sprintf("handoff state sequence changed: %d, expected %d", st.Sequence, m.Source.Sequence))
	}
	if f := FactsDigest(st); f != m.Source.FactsDigest {
		return model.Conflict("handoff facts changed: fixed inputs, promises or purged results differ from the manifest")
	}
	if c := CountsOf(st); c != m.Counts {
		return model.Conflict("handoff entity counts changed")
	}
	return nil
}

// VerifyStateFile checks raw bytes, structural validity and facts without
// claiming the file.
func (m Manifest) VerifyStateFile(path string) error {
	data, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != m.Source.StateDigest {
		return model.Conflict("state file digest does not match handoff manifest")
	}
	var st model.State
	if e = json.Unmarshal(data, &st); e != nil {
		return fmt.Errorf("decode state: %w", e)
	}
	if e = persist.Validate(st); e != nil {
		return e
	}
	return m.VerifyState(st)
}

func decodeState(path string) (model.State, []byte, error) {
	var st model.State
	data, e := os.ReadFile(path)
	if e != nil {
		return st, nil, e
	}
	if e = json.Unmarshal(data, &st); e != nil {
		return st, nil, fmt.Errorf("decode state: %w", e)
	}
	return st, data, persist.Validate(st)
}

func captureLocked(absSource, absDest string) (Manifest, error) {
	var m Manifest
	st, data, e := decodeState(absSource)
	if e != nil {
		return m, e
	}
	sum := sha256.Sum256(data)
	m = Manifest{Format: Format, Version: Version, CreatedAt: time.Now().UTC(), Destination: absDest,
		Source: Source{StatePath: absSource, StateDigest: hex.EncodeToString(sum[:]), Generation: st.Generation, Writer: st.Writer, Sequence: st.Sequence, FactsDigest: FactsDigest(st)},
		Counts: CountsOf(st)}
	m.seal()
	return m, nil
}

// Capture inspects a stopped source. It does not modify the source.
func Capture(sourcePath, destination string) (Manifest, error) {
	var m Manifest
	absSource, e := filepath.Abs(sourcePath)
	if e != nil {
		return m, e
	}
	absDest, e := filepath.Abs(destination)
	if e != nil {
		return m, e
	}
	lock, e := persist.AcquireLock(absSource)
	if e != nil {
		return m, fmt.Errorf("source must be stopped before capture: %w", e)
	}
	defer lock.Close()
	return captureLocked(absSource, absDest)
}

func WriteManifest(path string, m Manifest) error {
	b, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		return e
	}
	return persist.AtomicWrite(path, append(b, '\n'))
}

func ReadManifest(path string) (Manifest, error) {
	var m Manifest
	b, e := os.ReadFile(path)
	if e != nil {
		return m, e
	}
	if e = json.Unmarshal(b, &m); e != nil {
		return m, fmt.Errorf("decode handoff manifest: %w", e)
	}
	return m, m.Verify()
}

// Adopter returns the check passed to persist.OpenWith by a destination
// process. It verifies the on-disk state against the manifest before the new
// process is allowed to claim and increment the generation.
func Adopter(manifestPath string) (func(model.State) error, error) {
	m, e := ReadManifest(manifestPath)
	if e != nil {
		return nil, e
	}
	return func(st model.State) error { return m.VerifyState(st) }, nil
}

func tombstone(statePath string, m Manifest) persist.Tombstone {
	return persist.Tombstone{Format: persist.TombstoneFormat, Version: 1, StateDigest: m.Source.StateDigest,
		Generation: m.Source.Generation, ManifestDigest: m.Digest, Destination: m.Destination,
		SupersededAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

func writeTombstone(statePath string, m Manifest) error {
	b, e := json.MarshalIndent(tombstone(statePath, m), "", "  ")
	if e != nil {
		return e
	}
	return persist.AtomicWrite(persist.TombstonePath(statePath), append(b, '\n'))
}

// fenceLocked stamps the supersede marker on a stopped, verified source.
func fenceLocked(absSource string, m Manifest) error {
	if e := m.VerifyStateFile(absSource); e != nil {
		return fmt.Errorf("refusing to fence a source that no longer matches the manifest: %w", e)
	}
	return writeTombstone(absSource, m)
}

// Cutover performs the complete, verifiable move in one locked sequence:
// capture the stopped source, publish the manifest, copy the state to the
// destination, verify the copy, then fence the source. The source lock is held
// throughout, so an accidental old-host start cannot interleave. If the process
// dies before the fence, re-running with replace=true is safe because the
// source remains authoritative until its tombstone exists.
func Cutover(sourcePath, destinationPath, manifestPath string, replace bool) (Manifest, error) {
	var m Manifest
	absSource, e := filepath.Abs(sourcePath)
	if e != nil {
		return m, e
	}
	absDest, e := filepath.Abs(destinationPath)
	if e != nil {
		return m, e
	}
	if absSource == absDest {
		return m, model.Invalid("source and destination must differ")
	}
	lock, e := persist.AcquireLock(absSource)
	if e != nil {
		return m, fmt.Errorf("source must be stopped before cutover: %w", e)
	}
	defer lock.Close()
	if e = persist.CheckSuperseded(absSource); e != nil {
		return m, e
	}
	if _, e = os.Stat(absDest); e == nil && !replace {
		return m, model.Conflict("destination state already exists; pass --replace to overwrite a superseded copy")
	}
	if m, e = captureLocked(absSource, absDest); e != nil {
		return m, e
	}
	if e = WriteManifest(manifestPath, m); e != nil {
		return m, e
	}
	data, e := os.ReadFile(absSource)
	if e != nil {
		return m, e
	}
	if e = persist.AtomicWrite(absDest, data); e != nil {
		return m, e
	}
	if e = m.VerifyStateFile(absDest); e != nil {
		return m, fmt.Errorf("destination copy did not verify: %w", e)
	}
	// The destination copy is now verified. A stale supersede marker from an
	// earlier life of this path is released only at this point.
	if e = os.Remove(persist.TombstonePath(absDest)); e != nil && !os.IsNotExist(e) {
		return m, e
	}
	if e = fenceLocked(absSource, m); e != nil {
		return m, e
	}
	if e = persist.SyncDir(filepath.Dir(absSource)); e != nil {
		return m, e
	}
	return m, persist.SyncDir(filepath.Dir(absDest))
}

// Rollback moves ownership back from the current path to the original path.
// It records a new manifest for the return trip, checks that the original path
// still matches the prior manifest and its tombstone, copies and verifies the
// live state, then fences the path being abandoned. The return manifest is
// written next to the original path so its next process can adopt it.
func Rollback(currentPath, originalPath, returnManifestPath string, prior Manifest) (Manifest, error) {
	var m Manifest
	absCurrent, e := filepath.Abs(currentPath)
	if e != nil {
		return m, e
	}
	absOriginal, e := filepath.Abs(originalPath)
	if e != nil {
		return m, e
	}
	if absCurrent == absOriginal {
		return m, model.Invalid("rollback needs two distinct paths")
	}
	// Lock in a stable order to avoid deadlock if both are stopped.
	first, second := absCurrent, absOriginal
	if second < first {
		first, second = second, first
	}
	lockA, e := persist.AcquireLock(first)
	if e != nil {
		return m, fmt.Errorf("path %s must be stopped before rollback: %w", first, e)
	}
	defer lockA.Close()
	lockB, e := persist.AcquireLock(second)
	if e != nil {
		return m, fmt.Errorf("path %s must be stopped before rollback: %w", second, e)
	}
	defer lockB.Close()

	// The original path must still be the byte-exact fenced copy we handed
	// over. A destination claim only advances the destination copy, so this
	// strict check rejects a hand-edited or replaced original.
	if _, e = os.Stat(persist.TombstonePath(absOriginal)); e != nil {
		return m, fmt.Errorf("original path is not fenced; refusing to roll back over a live owner: %w", e)
	}
	if e = prior.VerifyStateFile(absOriginal); e != nil {
		return m, fmt.Errorf("original path no longer matches the outbound manifest: %w", e)
	}
	if m, e = captureLocked(absCurrent, absOriginal); e != nil {
		return m, e
	}
	if e = WriteManifest(returnManifestPath, m); e != nil {
		return m, e
	}
	data, e := os.ReadFile(absCurrent)
	if e != nil {
		return m, e
	}
	if e = persist.AtomicWrite(absOriginal, data); e != nil {
		return m, e
	}
	if e = m.VerifyStateFile(absOriginal); e != nil {
		return m, fmt.Errorf("restored copy did not verify: %w", e)
	}
	if e = os.Remove(persist.TombstonePath(absOriginal)); e != nil && !os.IsNotExist(e) {
		return m, e
	}
	if e = os.Remove(persist.AdoptionPath(absOriginal)); e != nil && !os.IsNotExist(e) {
		return m, e
	}
	if e = writeTombstone(absCurrent, m); e != nil {
		return m, e
	}
	if e = persist.SyncDir(filepath.Dir(absOriginal)); e != nil {
		return m, e
	}
	return m, persist.SyncDir(filepath.Dir(absCurrent))
}
