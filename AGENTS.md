# daytona-proxy

<!-- BEGIN GENERATED -->

## This service

Proxies hosted and port-forwarded services out of brainbase-mas agent sandboxes.

Cross-service context lives in `./platform-docs/`. If that directory is
missing, run `make docs-sync` first.

### Facts

- **Repo:** `BrainbaseHQ/daytona-proxy`
- **Platform:** render
- **Deployed as:** `daytona-proxy`

### Known gaps

- The env var list in `docs/service.yaml` is **incomplete**: it is the union of the Render blueprint and a scan of `os.environ` literals, so vars set only in the Render dashboard, or read through a computed name, are missing.

<!-- END GENERATED -->
