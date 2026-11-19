#!/usr/bin/env python3
"""Regenerate the zjsqlite test fixtures.

Run from the repo root (needs python3 with the stdlib sqlite3; the Go tests
never execute this — they only read the committed bytes):

    python3 internal/harness/zjsqlite/testdata/gen.py

Fixtures:
  sessions.db     plain checkpointed database
  wal.db/.db-wal  database whose newest commits live only in the WAL
  big.db          600 rows with long text to force interior + overflow pages
  corrupt.db      sqlite magic followed by garbage
  other.db        valid database with no sessions table
"""
import os
import shutil
import sqlite3
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

SCHEMA = """
CREATE TABLE sessions (
    name TEXT PRIMARY KEY,
    folder TEXT,
    repo TEXT,
    branch TEXT,
    id TEXT,
    last_used_at INTEGER
);
CREATE UNIQUE INDEX sessions_id ON sessions(id);
"""

def make_plain():
    path = os.path.join(HERE, "sessions.db")
    if os.path.exists(path):
        os.remove(path)
    con = sqlite3.connect(path)
    con.executescript(SCHEMA)
    rows = [
        ("model-router", "/home/arhammahajan/dev/model-router", None, "main",
         "52ad00a223c5", 1900000000),
        ("dots", "/home/arhammahajan/dev/dots", None, None,
         "33d125992a7a", 1900000100),
        ("weird name", "/tmp", None, None, "7fab12340001", 1900000200),
    ]
    con.executemany(
        "insert into sessions(name, folder, repo, branch, id, last_used_at)"
        " values (?,?,?,?,?,?)", rows)
    con.commit()
    con.close()
    return path

def make_wal():
    """Rows committed after the last checkpoint live only in the WAL."""
    path = os.path.join(HERE, "wal.db")
    for suffix in ("", "-wal", "-shm"):
        target = path + suffix
        if os.path.exists(target):
            os.remove(target)
    con = sqlite3.connect(path)
    con.executescript(SCHEMA)
    con.execute("PRAGMA wal_autocheckpoint=0")
    con.execute("PRAGMA journal_mode=WAL")
    con.execute(
        "insert into sessions(name, folder, repo, branch, id, last_used_at)"
        " values ('checkpointed','/a',NULL,NULL,'111111111111',1)")
    con.commit()
    con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
    con.execute(
        "insert into sessions(name, folder, repo, branch, id, last_used_at)"
        " values ('wal-only','/b',NULL,NULL,'222222222222',2)")
    con.execute(
        "insert into sessions(name, folder, repo, branch, id, last_used_at)"
        " values ('wal-older','/c',NULL,NULL,'333333333333',3)")
    con.commit()
    # Copy before closing: closing the last connection checkpoints the WAL.
    for suffix in ("", "-wal"):
        shutil.copyfile(path + suffix, path + suffix + ".fixture")
    con.close()
    # Closing may still touch the originals; move the pre-close copies in.
    for suffix in ("", "-wal"):
        os.replace(path + suffix + ".fixture", path + suffix)
    # Drop any -shm the run created; readers reconstruct without it.
    shm = path + "-shm"
    if os.path.exists(shm):
        os.remove(shm)
    return path

def make_big():
    path = os.path.join(HERE, "big.db")
    if os.path.exists(path):
        os.remove(path)
    con = sqlite3.connect(path)
    con.execute("PRAGMA page_size=512")
    con.executescript(SCHEMA)
    long_repo = "/home/arhammahajan/dev/some-extremely-long-workspace-name/" + "x" * 640
    rows = []
    for index in range(600):
        rows.append((f"space-{index:04d}", long_repo, None, None,
                     f"{index:012x}", index))
    con.executemany(
        "insert into sessions(name, folder, repo, branch, id, last_used_at)"
        " values (?,?,?,?,?,?)", rows)
    con.commit()
    con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
    con.close()
    return path

def make_corrupt():
    path = os.path.join(HERE, "corrupt.db")
    with open(path, "wb") as handle:
        handle.write(b"SQLite format 3\x00")
        handle.write(bytes(4096))
    return path

def make_other():
    path = os.path.join(HERE, "other.db")
    if os.path.exists(path):
        os.remove(path)
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE bookmarks(url TEXT PRIMARY KEY, title TEXT)")
    con.execute("INSERT INTO bookmarks VALUES ('https://zellij.dev', 'Zellij')")
    con.commit()
    con.close()
    return path

if __name__ == "__main__":
    for produced in (make_plain(), make_wal(), make_big(), make_corrupt(), make_other()):
        print("wrote", produced)
    # Verify the WAL fixture really holds wal-only content.
    size = os.path.getsize(os.path.join(HERE, "wal.db-wal"))
    if size < 32:
        sys.exit("wal.db-wal looks empty; fixture generation failed")
    print("wal bytes:", size)
