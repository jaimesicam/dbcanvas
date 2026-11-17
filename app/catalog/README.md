# catalog

Copies of the repository's `versions.yaml` and `versions-arm64.yaml`, embedded into the
dbcanvas binary (`catalogembed.go`) so a downloaded binary has the version catalogue
without a checkout. They are not kept here in git: `make catalog` copies them, and
`make dbcanvas` runs it. A file found on disk (VERSIONS_FILE, /etc/dbcanvas, the working
directory) is always read first.
