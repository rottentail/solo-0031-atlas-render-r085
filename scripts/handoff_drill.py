#!/usr/bin/env python3
"""End-to-end single-writer handoff drill.

Two independent state directories on one host stand in for two hosts. The drill
performs a real, verifiable cutover and rollback:

1. production facts on the source: a successful job with a receipt, its
   artifact purged, plus a heavy job interrupted with SIGKILL;
2. verify + cutover with atlasctl: capture facts, copy, verify the copy, fence
   the source path with a supersede marker;
3. an accidental restart of the old host on the migrated path is refused;
4. an out-of-band stale copy is detected against the manifest (exit 3);
5. the destination adopts the manifest, replays the interrupted request with
   its original idempotency key, recovers and completes the running job from
   pinned input, and proves pixel equality with a fresh render; the purged
   artifact stays gone while its success fact survives;
6. rollback moves new facts back to the original path and fences the
   destination;
7. a simulated take-over of the live path fences the serving process, so it
   answers 503 rather than stale facts.

Every phase prints one JSON line. Any failed assertion exits non-zero.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zlib

ROOT = Path(__file__).resolve().parents[1]


def tiny_png():
    def chunk(tag, data):
        return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', zlib.crc32(tag + data))
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 2, 2, 8, 6, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b'\0' + bytes([240, 40, 80, 255] * 2) + b'\0' + bytes([40, 80, 240, 128] * 2))) + chunk(b'IEND', b'')


def light_document():
    return {"width": 320, "height": 240, "background": "#F5F1E8", "layers": [
        {"id": "plate", "kind": "rectangle", "width": 220, "height": 150,
         "transform": {"x": 45, "y": 35, "scaleX": 1, "scaleY": 1, "degrees": 5},
         "opacity": 1, "fill": {"kind": "solid", "color": "#145A70"}},
        {"id": "badge", "kind": "image", "width": 60, "height": 60, "assetId": "ribbon",
         "transform": {"x": 20, "y": 30, "scaleX": 1, "scaleY": 1}, "opacity": 1}]}


def heavy_document():
    layers = [{"id": f"sheet-{i}", "kind": "rectangle", "width": 1000, "height": 1000,
               "transform": {"scaleX": 1, "scaleY": 1}, "opacity": .5,
               "fill": {"kind": "solid", "color": f"#10{i}0A{i}"}} for i in range(8)]
    return {"width": 1000, "height": 1000, "background": "#101010", "layers": layers}


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


class Host:
    def __init__(self, atlas, directory, name, manifest=None, writer=None):
        self.atlas = atlas
        self.directory = directory
        self.name = name
        self.manifest = manifest
        self.writer = writer or name
        self.proc = None
        self.log_path = directory / f'{name}.log'
        self.log = open(self.log_path, 'ab')
        self.url = None

    def start(self, expect=True):
        port = free_port()
        self.url = f'http://127.0.0.1:{port}'
        env = dict(os.environ, ATLAS_ADDR=f'127.0.0.1:{port}',
                   ATLAS_STATE=str(self.directory / 'state.json'), ATLAS_WRITER=self.writer)
        if self.manifest:
            env['ATLAS_MANIFEST'] = str(self.manifest)
        self.proc = subprocess.Popen([str(self.atlas)], env=env, stdout=self.log, stderr=self.log)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                if expect:
                    raise RuntimeError(f'{self.name} exited during startup:\n{self.log_path.read_text(errors="replace")}')
                return False
            try:
                self.call('GET', '/healthz', timeout=.5)
                return True
            except (urllib.error.URLError, ConnectionError, TimeoutError):
                time.sleep(.03)
        raise RuntimeError(f'{self.name} startup deadline exceeded')

    def stop(self, abrupt=False):
        if self.proc and self.proc.poll() is None:
            (self.proc.kill if abrupt else self.proc.terminate)()
            self.proc.wait(timeout=30)
        self.proc = None

    def call(self, method, path, body=None, status=200, headers=None, timeout=15):
        hdr = dict(headers or {})
        if isinstance(body, (dict, list)):
            body = json.dumps(body).encode()
            hdr['Content-Type'] = 'application/json'
        request = urllib.request.Request(self.url + path, data=body, method=method, headers=hdr)
        try:
            response = urllib.request.urlopen(request, timeout=timeout)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read()
            if response.status != status:
                raise RuntimeError(f'{self.name} {method} {path}: expected {status}, got {response.status}: {data[:400]!r}')
            if response.headers.get('Content-Type', '').startswith('application/json'):
                return json.loads(data)
            return data

    def wait(self, job_id, wanted=('succeeded',), timeout=300):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            job = self.call('GET', '/v1/jobs/' + job_id)
            if job['state'] in wanted:
                return job
            if job['state'] in ('failed', 'cancelled'):
                raise RuntimeError(f'{self.name} unexpected outcome: {job}')
            time.sleep(.05)
        raise RuntimeError(f'{self.name} job deadline exceeded')


def tool(binary, *args, expect=0):
    result = subprocess.run([str(binary), *map(str, args)], capture_output=True, text=True, timeout=120)
    if result.returncode != expect:
        raise RuntimeError(f'{Path(binary).name} {" ".join(map(str, args))}: expected {expect}, got {result.returncode}\nstdout={result.stdout}\nstderr={result.stderr}')
    return json.loads(result.stdout) if result.stdout.strip() else {}


def note(**kw):
    print(json.dumps(kw, ensure_ascii=False, sort_keys=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--keep', action='store_true', help='keep the drill directory for inspection')
    args = parser.parse_args()
    work = Path(tempfile.mkdtemp(prefix='atlas-handoff-'))
    try:
        run(work)
        if args.keep:
            note(kept=str(work))
    finally:
        if not args.keep:
            shutil.rmtree(work, ignore_errors=True)


def run(work):
    atlas = work / 'atlas'
    atlasctl = work / 'atlasctl'
    subprocess.run(['go', 'build', '-o', str(atlas), './cmd/atlas'], cwd=ROOT, check=True, timeout=120)
    subprocess.run(['go', 'build', '-o', str(atlasctl), './cmd/atlasctl'], cwd=ROOT, check=True, timeout=120)
    source_dir, dest_dir, stale_dir = (work / n for n in ('host-a', 'host-b', 'host-a-copy'))
    for d in (source_dir, dest_dir, stale_dir):
        d.mkdir()
    source_state, dest_state = source_dir / 'state.json', dest_dir / 'state.json'
    outbound, inbound = work / 'handoff-a-to-b.json', work / 'handoff-b-to-a.json'

    # ---- 1. production facts on the source ---------------------------------
    a = Host(atlas, source_dir, 'source', writer='host-a:1001')
    a.start()
    a.call('PUT', '/v1/assets/ribbon', tiny_png(), 201)
    scene = a.call('PUT', '/v1/scenes/emblem', {'name': '迁移印记', 'expectedRevision': 0, 'document': light_document()})
    snap_light = a.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': scene['revision']}, 201)
    done = a.call('POST', '/v1/jobs', {'snapshotId': snap_light['id'], 'scale': 1}, 202, {'Idempotency-Key': 'promise-one'})
    done = a.wait(done['id'])
    artifact = done['artifactId']
    a.call('DELETE', '/v1/artifacts/' + artifact)
    a.call('GET', '/v1/artifacts/' + artifact, status=410)

    heavy = a.call('PUT', '/v1/scenes/emblem', {'name': '迁移印记', 'expectedRevision': scene['revision'], 'document': heavy_document()})
    snap_heavy = a.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': heavy['revision']}, 201)
    hot = a.call('POST', '/v1/jobs', {'snapshotId': snap_heavy['id'], 'scale': 2}, 202, {'Idempotency-Key': 'interrupted'})
    a.wait(hot['id'], wanted=('running',))
    a.stop(abrupt=True)  # real interruption: no drain, no flush, no goodbye
    killed = json.loads(source_state.read_text())
    assert killed['jobs'][hot['id']]['state'] == 'running', killed['jobs'][hot['id']]
    source_bytes = source_state.read_bytes()
    source_gen, source_writer = killed['generation'], killed['writer']
    note(phase='interrupted', job=hot['id'], state='running', generation=source_gen, writer=source_writer)

    # An out-of-band copy the old host might retain: same facts, no fence.
    for name in ('state.json', 'state.json.lock'):
        if (source_dir / name).exists():
            (stale_dir / name).write_bytes((source_dir / name).read_bytes())
    facts = tool(atlasctl, 'inspect', '--state', source_state)['factsDigest']

    # ---- 2. verifiable cutover and fence -----------------------------------
    cut = tool(atlasctl, 'cutover', '--state', source_state, '--to', dest_state, '--manifest', outbound)
    assert cut['source']['factsDigest'] == facts
    assert dest_state.read_bytes() == source_bytes, 'destination copy is not byte-identical'
    assert (source_dir / 'state.json.superseded').exists()
    assert tool(atlasctl, 'verify', '--state', dest_state, '--manifest', outbound)['factsDigest'] == facts
    note(phase='cutover', manifest=cut['digest'], facts=facts, generation=source_gen,
         destination=str(dest_state.resolve()))

    # The old host must not come back on the migrated path.
    old = Host(atlas, source_dir, 'source-restart', writer='host-a:1002')
    assert not old.start(expect=False), 'old host restarted after being superseded'
    assert 'superseded' in old.log_path.read_text(errors='replace')
    note(phase='old-host-restart', refused=True, reason='supersede marker')

    # A stale copy is detected: it advances its own generation and diverges.
    rogue = Host(atlas, stale_dir, 'stale-copy', writer='host-a:stale')
    rogue.start()
    rogue.stop()
    stale = tool(atlasctl, 'inspect', '--state', stale_dir / 'state.json')
    assert stale['generation'] == source_gen + 1 and stale['writer'] == 'host-a:stale'
    mismatch = subprocess.run([str(atlasctl), 'verify', '--state', str(stale_dir / 'state.json'),
                               '--manifest', str(outbound)], capture_output=True, text=True)
    assert mismatch.returncode == 3, (mismatch.returncode, mismatch.stdout, mismatch.stderr)
    note(phase='stale-copy', detected=True, generation=stale['generation'], writer=stale['writer'])

    # ---- 3. destination adopts the manifest --------------------------------
    b = Host(atlas, dest_dir, 'dest', manifest=outbound, writer='host-b:2001')
    b.start()
    status = b.call('GET', '/v1/status')
    assert status['writer'] == 'host-b:2001' and status['generation'] == source_gen + 1, status
    assert status['adoptedBy'] == str(outbound.resolve()), status
    note(phase='adopted', writer=status['writer'], generation=status['generation'], adoptedBy=status['adoptedBy'])

    # Replay of a promise accepted before the move returns the original job,
    # even though its bytes were cleaned before the move.
    replay = b.call('POST', '/v1/jobs', {'snapshotId': snap_light['id'], 'scale': 1}, 202, {'Idempotency-Key': 'promise-one'})
    assert replay['id'] == done['id'] and replay['state'] == 'succeeded' and replay['artifactId'] == artifact, replay
    assert b.call('GET', '/v1/jobs/' + done['id'])['state'] == 'succeeded'
    b.call('GET', '/v1/artifacts/' + artifact, status=410)
    note(phase='replay', key='promise-one', job=replay['id'], artifactGone=True, successFactKept=True)

    # The interrupted job is recovered and completes from its pinned input.
    recovered = b.wait(hot['id'])
    assert recovered['snapshotId'] == snap_heavy['id'] and recovered['attempt'] == 1, recovered
    assert b.call('GET', '/v1/snapshots/' + snap_heavy['id'])['digest'] == snap_heavy['digest']
    fresh = b.wait(b.call('POST', '/v1/jobs', {'snapshotId': snap_heavy['id'], 'scale': 2}, 202,
                          {'Idempotency-Key': 'post-move-check'})['id'])
    diff = b.call('POST', '/v1/compare', {'left': recovered['artifactId'], 'right': fresh['artifactId']})
    assert diff['changedPixels'] == 0 and diff['maxDelta'] == 0, diff
    note(phase='recovered', job=recovered['id'], attempt=recovered['attempt'], pixelsIdentical=True)

    # ---- 4. rollback drill --------------------------------------------------
    b.stop()
    back = tool(atlasctl, 'rollback', '--from', dest_state, '--to', source_state,
                '--manifest', inbound, '--prior', outbound)
    assert back['destination'] == str(source_state.resolve())
    assert (dest_dir / 'state.json.superseded').exists()
    assert not (source_dir / 'state.json.superseded').exists()
    note(phase='rollback', manifest=back['digest'], facts=back['source']['factsDigest'])

    a2 = Host(atlas, source_dir, 'source', manifest=inbound, writer='host-a:1003')
    a2.start()
    after = a2.call('GET', '/v1/status')
    assert after['writer'] == 'host-a:1003' and after['generation'] == back['source']['generation'] + 1, after
    assert after['adoptedBy'] == str(inbound.resolve()), after
    assert a2.call('POST', '/v1/jobs', {'snapshotId': snap_light['id'], 'scale': 1}, 202,
                   {'Idempotency-Key': 'promise-one'})['id'] == done['id']
    assert a2.wait(hot['id'])['artifactId'] == recovered['artifactId']
    assert a2.call('GET', '/v1/jobs/' + fresh['id'])['state'] == 'succeeded'
    png = a2.call('GET', '/v1/artifacts/' + recovered['artifactId'])
    assert png[:8] == b'\x89PNG\r\n\x1a\n' and len(png) > 100
    note(phase='rollback-verified', job=hot['id'], receipt='promise-one',
         facts=back['source']['factsDigest'], postMoveJob=fresh['id'],
         pngDigest=hashlib.sha256(png).hexdigest())

    # The abandoned destination path cannot serve again, even with its lock
    # file removed: the fence is the marker, not the advisory lock.
    (dest_dir / 'state.json.lock').unlink(missing_ok=True)
    old_dest = Host(atlas, dest_dir, 'dest-restart', writer='host-b:2002')
    assert not old_dest.start(expect=False)
    assert 'superseded' in old_dest.log_path.read_text(errors='replace')
    note(phase='abandoned-restart', refused=True, lockRemoved=True)

    # ---- 5. stale owner on the live path is fenced -------------------------
    # Simulate another process taking the same path with a valid state for the
    # next generation (a shared volume whose lock is not honoured).
    intruded = json.loads(source_state.read_text())
    intruded['generation'] += 1
    intruded['writer'] = 'host-x:intruder'
    tmp = source_dir / 'intrusion.tmp'
    tmp.write_text(json.dumps(intruded))
    os.replace(tmp, source_state)
    fenced = False
    for _ in range(100):
        try:
            a2.call('GET', '/healthz', status=503, timeout=.5)
            fenced = True
            break
        except RuntimeError:
            time.sleep(.05)
    assert fenced, 'live process did not fence after take-over'
    a2.call('GET', '/v1/status', status=503)
    a2.call('POST', '/v1/jobs', {'snapshotId': snap_light['id'], 'scale': 1}, 503,
            {'Idempotency-Key': 'after-fence'})
    a2.stop()
    note(phase='generation-fence', servedStaleFacts=False, health=503, writes=503, reads=503)

    note(drill='handoff-cutover-rollback', result='passed')


if __name__ == '__main__':
    main()
