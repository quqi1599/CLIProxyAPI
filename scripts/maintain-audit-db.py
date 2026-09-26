#!/usr/bin/env python3
"""Offline audit compaction. Default is read-only planning; never deletes backups."""
import argparse
import datetime
from contextlib import closing
import fcntl
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import time
import urllib.request

DATABASE = Path('/opt/cliproxy/content-audit/audit.db')
BACKUPS = Path('/opt/cliproxy/backups')
CONTAINER = 'cli-proxy-api'


def metadata(path):
    with closing(sqlite3.connect(f'file:{path}?mode=ro', uri=True)) as db:
        values = {k: db.execute('PRAGMA ' + k).fetchone()[0]
                  for k in ('page_count', 'page_size', 'freelist_count', 'user_version')}
    return dict(path=str(path), bytes=path.stat().st_size, **values)


def fingerprint(db):
    """Compare every stored value, including encrypted evidence, without outputting it."""
    digest = hashlib.sha256()
    schema = db.execute("SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name").fetchall()
    digest.update(repr(schema).encode())
    counts = {}
    for (name,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"):
        quoted = '"' + name.replace('"', '""') + '"'
        # Sorting row hashes also handles WITHOUT ROWID tables and VACUUM rowid changes.
        hashes = []
        for row in db.execute('SELECT * FROM ' + quoted):
            rh = hashlib.sha256()
            for value in row:
                data = value if isinstance(value, bytes) else repr(value).encode()
                rh.update(type(value).__name__.encode() + b':' + str(len(data)).encode() + b':' + data)
            hashes.append(rh.digest())
        for value in sorted(hashes):
            digest.update(value)
        counts[name] = len(hashes)
    return counts, digest.hexdigest()


def compact_offline(path, backup):
    """Caller must stop every writer. The original is retained until full validation."""
    backup.mkdir(mode=0o700, parents=True, exist_ok=False)
    target = path.with_name(path.name + '.compacted')
    if target.exists():
        raise RuntimeError('compaction target already exists; inspect before retrying')
    stat = path.stat()
    with closing(sqlite3.connect(str(path), timeout=30)) as db:
        checkpoint = db.execute('PRAGMA wal_checkpoint(TRUNCATE)').fetchone()
        if checkpoint[0] != 0:
            raise RuntimeError('checkpoint is busy; another writer may be active')
        before = fingerprint(db)
        db.execute('VACUUM INTO ?', (str(target),))
    with closing(sqlite3.connect(str(target))) as db:
        integrity = db.execute('PRAGMA integrity_check').fetchall()
        if integrity != [('ok',)] or fingerprint(db) != before:
            raise RuntimeError('compacted database failed integrity/content validation')
    with closing(sqlite3.connect(str(target))) as db:
        db.execute('PRAGMA journal_mode=WAL').fetchall()
    os.chmod(target, stat.st_mode & 0o777)
    os.chown(target, stat.st_uid, stat.st_gid)
    with target.open('rb') as f:
        os.fsync(f.fileno())
    # Closed SQLite connections should remove sidecars. Preserve any residuals.
    for suffix in ('-wal', '-shm'):
        sidecar = Path(str(path) + suffix)
        if sidecar.exists():
            if suffix == '-wal' and sidecar.stat().st_size:
                raise RuntimeError('nonempty WAL after checkpoint; refusing replacement')
            shutil.move(str(sidecar), str(backup / sidecar.name))
    original = backup / 'audit.db'
    # A hard link keeps the original available through the atomic replacement.
    os.link(path, original)
    fd = os.open(backup, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    os.replace(target, path)
    for directory in (path.parent, backup):
        fd = os.open(directory, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    result = dict(before_bytes=stat.st_size, after=metadata(path), rollback=str(original),
                  content_sha256=before[1], table_rows=before[0])
    (backup / 'verification.json').write_text(json.dumps(result, indent=2) + '\n')
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true', help='stop CPA during an approved maintenance window')
    parser.add_argument('--expected-image', help='exact docker image reference verified by the operator')
    args = parser.parse_args()
    inspect = json.loads(subprocess.check_output(['docker', 'inspect', CONTAINER]))[0]
    current = metadata(DATABASE)
    candidates = [dict(path=str(p), bytes=p.stat().st_size, modified_utc=datetime.datetime.fromtimestamp(
        p.stat().st_mtime, datetime.timezone.utc).isoformat()) for p in BACKUPS.rglob('*')
        if p.is_file() and p.stat().st_size > 1_000_000_000]
    print(json.dumps(dict(database=current, image=inspect['Config']['Image'],
        reclaimable_bytes=current['freelist_count'] * current['page_size'],
        backup_inventory=candidates, deletion_policy='none; explicit retention decision required'), indent=2), flush=True)
    if not args.apply:
        return
    if args.expected_image != inspect['Config']['Image']:
        raise RuntimeError('expected image does not match running CPA')
    if not inspect['State']['Running']:
        raise RuntimeError('CPA was already stopped; do not change operator state')
    if not any(m['Source'] == str(DATABASE.parent) and m['Destination'] == '/CLIProxyAPI/content-audit'
               for m in inspect['Mounts']):
        raise RuntimeError('audit bind mount does not match expected path')
    if shutil.disk_usage(DATABASE.parent).free < current['bytes'] * 2:
        raise RuntimeError('insufficient compaction/rollback headroom')
    with open('/opt/cliproxy/.audit-maintenance.lock', 'w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        subprocess.run(['docker', 'stop', '--time', '120', CONTAINER], check=True)
        try:
            stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
            result = compact_offline(DATABASE, BACKUPS / ('audit-compact-' + stamp))
            print(json.dumps(result, indent=2), flush=True)
        finally:
            subprocess.run(['docker', 'start', CONTAINER], check=True)
        for _ in range(60):
            try:
                with urllib.request.urlopen('http://127.0.0.1:8317/readyz', timeout=2) as response:
                    if response.status == 200:
                        print('CPA ready; run scoped authenticated business probes before closing maintenance.')
                        return
            except Exception:
                pass
            time.sleep(1)
        raise RuntimeError('CPA did not become ready; retain rollback and inspect logs')


if __name__ == '__main__':
    main()
