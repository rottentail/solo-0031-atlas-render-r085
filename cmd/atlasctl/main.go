// Command atlasctl prepares and verifies a single-writer state handoff.
//
// It only touches state files while the owning service is stopped; the
// service's advisory lock is acquired for every mutating step. No command
// rewrites a live state or removes a supersede marker from an unverified path.
package main

import (
	"atlas-render/internal/handoff"
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

const usage = `atlasctl - verifiable single-writer state handoff

usage:
  atlasctl capture --state PATH --to DEST --manifest FILE
  atlasctl cutover --state PATH --to DEST --manifest FILE [--replace]
  atlasctl rollback --from PATH --to PATH --manifest FILE [--prior FILE]
  atlasctl verify --state PATH --manifest FILE
  atlasctl inspect --state PATH

capture   Verify a stopped state and record its facts in a manifest.
cutover   Capture, copy, verify the copy, then fence the source path.
rollback  Move ownership back to the original path and fence the current one.
verify    Check that PATH still matches the manifest; exits 3 on mismatch.
inspect   Print ownership generation, writer and a facts digest.

The old host is fenced by STATE.superseded. A fenced process refuses to start
and refuses to serve, even if its lock file or state file was copied back.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var e error
	switch os.Args[1] {
	case "capture":
		e = capture(os.Args[2:])
	case "cutover":
		e = cutover(os.Args[2:])
	case "rollback":
		e = rollback(os.Args[2:])
	case "verify":
		e = verify(os.Args[2:])
	case "inspect":
		e = inspect(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if e != nil {
		fmt.Fprintf(os.Stderr, "atlasctl: %v\n", e)
		os.Exit(1)
	}
}

func emit(v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e == nil {
		fmt.Println(string(b))
	}
	return e
}

func capture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	state := fs.String("state", "", "stopped source state path")
	to := fs.String("to", "", "destination state path recorded in the manifest")
	manifest := fs.String("manifest", "", "manifest output path")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *state == "" || *to == "" || *manifest == "" {
		return fmt.Errorf("capture requires --state, --to and --manifest")
	}
	m, e := handoff.Capture(*state, *to)
	if e != nil {
		return e
	}
	if e = handoff.WriteManifest(*manifest, m); e != nil {
		return e
	}
	return emit(m)
}

func cutover(args []string) error {
	fs := flag.NewFlagSet("cutover", flag.ContinueOnError)
	state := fs.String("state", "", "stopped source state path")
	to := fs.String("to", "", "destination state path")
	manifest := fs.String("manifest", "", "manifest output path")
	replace := fs.Bool("replace", false, "overwrite an existing destination copy")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *state == "" || *to == "" || *manifest == "" {
		return fmt.Errorf("cutover requires --state, --to and --manifest")
	}
	m, e := handoff.Cutover(*state, *to, *manifest, *replace)
	if e != nil {
		return e
	}
	return emit(m)
}

func rollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	from := fs.String("from", "", "current state path (must be stopped)")
	to := fs.String("to", "", "original state path to restore (must be stopped)")
	manifest := fs.String("manifest", "", "return manifest output path")
	prior := fs.String("prior", "", "outbound manifest that fenced --to")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *from == "" || *to == "" || *manifest == "" || *prior == "" {
		return fmt.Errorf("rollback requires --from, --to, --manifest and --prior")
	}
	p, e := handoff.ReadManifest(*prior)
	if e != nil {
		return e
	}
	m, e := handoff.Rollback(*from, *to, *manifest, p)
	if e != nil {
		return e
	}
	return emit(m)
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	state := fs.String("state", "", "state path to check")
	manifest := fs.String("manifest", "", "manifest to check against")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *state == "" || *manifest == "" {
		return fmt.Errorf("verify requires --state and --manifest")
	}
	m, e := handoff.ReadManifest(*manifest)
	if e != nil {
		return e
	}
	if e = m.VerifyStateFile(*state); e != nil {
		if _, w := os.Stderr.WriteString("mismatch\n"); w != nil {
			return w
		}
		os.Exit(3)
	}
	return emit(map[string]any{"state": *state, "manifest": m.Digest, "factsDigest": m.Source.FactsDigest, "generation": m.Source.Generation})
}

func inspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	state := fs.String("state", "", "state path to inspect")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *state == "" {
		return fmt.Errorf("inspect requires --state")
	}
	gen, writer, e := persist.ReadHeader(*state)
	if e != nil {
		return e
	}
	lock, e := persist.AcquireLock(*state)
	running := e != nil
	if !running {
		_ = lock.Close()
	}
	out := map[string]any{"state": *state, "generation": gen, "writer": writer, "running": running}
	if st, e := readWhole(*state); e == nil {
		out["factsDigest"] = handoff.FactsDigest(st)
		out["counts"] = handoff.CountsOf(st)
		out["sequence"] = st.Sequence
	}
	if e = persist.CheckSuperseded(*state); e != nil {
		out["superseded"] = e.Error()
	}
	return emit(out)
}

func readWhole(path string) (model.State, error) {
	var st model.State
	b, e := os.ReadFile(path)
	if e != nil {
		return st, e
	}
	if e = json.Unmarshal(b, &st); e != nil {
		return st, e
	}
	return st, persist.Validate(st)
}
