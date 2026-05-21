// Standalone module so it doesn't pollute the parent kode-gopher module's
// dep graph with the entire GCP SDK. Built once at image-build time
// (sandbox/Dockerfile) to populate $GOCACHE and $GOMODCACHE; nothing
// at runtime ever invokes this binary.
module kode_gopher_prewarm

go 1.26
