# relaybin

Built copies of `cmd/dbcanvas-relay`, gzipped, one per Linux architecture
(`dbcanvas-relay-linux-amd64.gz`, `dbcanvas-relay-linux-arm64.gz`). They are embedded
into DBCanvas (`noderelay.go`) and are not kept in git: `make relaybin` builds them, and
the image and release builds run it first. A build without them still works; it reaches
nodes through bash only.
