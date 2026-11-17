# bundle

What the dbcanvas binary carries besides its code, embedded from here (`bundle.go`) and
unpacked into `~/.dbcanvas/assets/<version>/` when the service starts:

- `images/` — the node images' Dockerfiles, for the Build button of a missing image
- `pagerpanic/` — Pager Panic's question banks
- `licenses/` — DBCanvas's licence, the third-party notices and every bundled component's
  licence, served at /licenses/
- `bighole.tar.gz` — Big Hole, the MongoDB FTDC viewer a node can serve

None of it is kept in git: `make bundle` builds it, and `make dbcanvas` runs that first. A
build without it still works; those features then say what is missing.
