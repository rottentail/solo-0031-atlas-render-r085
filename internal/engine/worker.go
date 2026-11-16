package engine

import (
	"atlas-render/internal/codec"
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/raster"
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"
)

type Worker struct {
	store  *persist.Store
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func New(st *persist.Store) *Worker {
	return &Worker{store: st, active: map[string]context.CancelFunc{}}
}

// Watch polls ownership so a process that loses the state to another writer
// stops its rendering loop even when no HTTP requests arrive. It returns the
// fencing error so the caller can cancel the worker context.
func (w *Worker) Watch(ctx context.Context, interval time.Duration) error {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if e := w.store.VerifyOwnership(); e != nil {
				return e
			}
		}
	}
}
func (w *Worker) Recover() error {
	return w.store.Update(func(s *model.State) error {
		for id, j := range s.Jobs {
			if j.State == "running" {
				j.State = "queued"
				j.UpdatedAt = time.Now().UTC()
				s.Jobs[id] = j
				s.Emit("job.recovered", id)
			}
		}
		return nil
	})
}
func (w *Worker) Cancel(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cancel := w.active[id]; cancel != nil {
		cancel()
	}
}
func (w *Worker) Run(ctx context.Context) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if e := w.step(ctx); e != nil {
				log.Printf("render worker: %v", e)
			}
		}
	}
}
func (w *Worker) claim() (model.Job, bool, error) {
	var job model.Job
	found := false
	state := w.store.Read()
	ids := []string{}
	for id, j := range state.Jobs {
		if j.State == "queued" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return job, false, nil
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := state.Jobs[ids[i]], state.Jobs[ids[j]]
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	e := w.store.Update(func(s *model.State) error {
		j := s.Jobs[ids[0]]
		if j.State != "queued" {
			return nil
		}
		j.State = "running"
		j.UpdatedAt = time.Now().UTC()
		s.Jobs[j.ID] = j
		s.Emit("job.running", j.ID)
		job = j
		found = true
		return nil
	})
	return job, found, e
}
func (w *Worker) step(parent context.Context) error {
	j, ok, e := w.claim()
	if e != nil || !ok {
		return e
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	w.mu.Lock()
	w.active[j.ID] = cancel
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.active, j.ID); w.mu.Unlock() }()
	// Cancellation can commit between claim and tracking; consult the committed state.
	current := w.store.Read().Jobs[j.ID]
	if current.State != "running" {
		cancel()
	}
	plan, e := raster.Compile(j.Payload, j.Scale)
	var data []byte
	if e == nil {
		var imgErr error
		img, imgErr := plan.Render(ctx)
		e = imgErr
		if e == nil {
			data, e = codec.Encode(img)
		}
	}
	// A stopped or superseded process must not commit a result; the next owner
	// recovers the running job from its pinned input instead.
	if parent.Err() != nil {
		return nil
	}
	if e = w.store.Fenced(); e != nil {
		return e
	}
	return w.store.Update(func(s *model.State) error {
		v := s.Jobs[j.ID]
		if v.State != "running" || v.Attempt != j.Attempt {
			return nil
		}
		v.UpdatedAt = time.Now().UTC()
		if e != nil {
			if errors.Is(e, context.Canceled) {
				v.State = "cancelled"
			} else {
				v.State = "failed"
				v.Error = e.Error()
			}
			s.Jobs[v.ID] = v
			s.Emit("job."+v.State, v.ID)
			return nil
		}
		a := model.Artifact{ID: model.ID("png", s.Sequence+1), JobID: v.ID, State: "live", Digest: model.Digest(data), Width: plan.Width, Height: plan.Height, PNG: data}
		s.Artifacts[a.ID] = a
		v.State = "succeeded"
		v.ArtifactID = a.ID
		s.Jobs[v.ID] = v
		s.Emit("job.succeeded", v.ID)
		return nil
	})
}
