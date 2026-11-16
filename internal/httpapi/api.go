package httpapi

import (
	"atlas-render/internal/engine"
	"atlas-render/internal/model"
	"atlas-render/internal/service"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

type API struct {
	Service *service.Service
	Worker  *engine.Worker
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, e error) {
	var de *model.Error
	if errors.As(e, &de) {
		write(w, de.Status, map[string]string{"code": de.Code, "error": de.Message})
		return
	}
	write(w, 500, map[string]string{"code": "internal", "error": "operation could not be committed"})
}
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20))
	d.DisallowUnknownFields()
	if e := d.Decode(dst); e != nil {
		return model.Invalid("invalid JSON: %v", e)
	}
	var extra any
	if e := d.Decode(&extra); !errors.Is(e, io.EOF) {
		return model.Invalid("expected exactly one JSON value")
	}
	return nil
}
func (a API) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := a.Service.Store.Fenced(); e != nil {
			fail(w, e)
			return
		}
		write(w, 200, map[string]string{"status": "ready"})
	})
	m.HandleFunc("PUT /v1/scenes/{id}", a.putScene)
	m.HandleFunc("GET /v1/scenes/{id}", a.getScene)
	m.HandleFunc("POST /v1/scenes/{id}/state", a.sceneState)
	m.HandleFunc("POST /v1/scenes/{id}/snapshots", a.freeze)
	m.HandleFunc("PUT /v1/assets/{id}", a.importAsset)
	m.HandleFunc("POST /v1/assets/{id}/retire", a.retireAsset)
	m.HandleFunc("GET /v1/snapshots/{id}", a.getSnapshot)
	m.HandleFunc("POST /v1/snapshots/{id}/revoke", a.revoke)
	m.HandleFunc("GET /v1/snapshots/{id}/package", a.exportPackage)
	m.HandleFunc("POST /v1/imports/{id}", a.importPackage)
	m.HandleFunc("POST /v1/jobs", a.submit)
	m.HandleFunc("GET /v1/jobs/{id}", a.getJob)
	m.HandleFunc("POST /v1/jobs/{id}/cancel", a.cancel)
	m.HandleFunc("POST /v1/jobs/{id}/retry", a.retry)
	m.HandleFunc("GET /v1/artifacts/{id}", a.download)
	m.HandleFunc("DELETE /v1/artifacts/{id}", a.purge)
	m.HandleFunc("POST /v1/compare", a.compare)
	m.HandleFunc("GET /v1/status", a.status)
	m.HandleFunc("GET /v1/events", a.events)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// A fenced process must not answer with stale facts: a client may
		// otherwise conclude the old host is still authoritative. Ownership is
		// re-read on every request, so a takeover is visible immediately.
		if e := a.Service.Store.VerifyOwnership(); e != nil {
			fail(w, e)
			return
		}
		m.ServeHTTP(w, r)
	})
}
