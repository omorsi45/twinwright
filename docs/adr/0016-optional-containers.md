# ADR 0016: Optional containerized world sidecars

Status: accepted, 2026-09-25

Some simulation scenarios need process isolation: intentionally failing processes, network partitions, service restarts, or realistic HTTP listeners. Putting every world behind Docker by default would slow the common path and couple Twinwright to a daemon that this host does not always have.

The chosen design keeps default worlds in-process. An experimental sidecar config selects `runtime: local` or `runtime: docker`. `local` records an in-process handle. `docker` shells to the docker CLI through an injectable runner; secrets must use `--env-file` with a relative path and never appear on argv. Missing daemon errors are explicit and only affect docker configs. Kubernetes is rejected as a runtime.

This is optional infrastructure. Existing manifests, stores, and scripted fixtures are unchanged. A live docker start was not verified here because the daemon was not running.
