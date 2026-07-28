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

- **No code scan was possible here.** The scanner is a Python `ast` pass over `os.environ` literals, and this repo has no Python sources — anything read via `process.env` is invisible to it. The env var list therefore reflects the Render blueprint alone.
- **The dependency list is affected too.** Edges are inferred from env var *names* (`KAFKA_LLM_SERVICE_URL` → `kafka-llm-service`), so a name the scanner could not see produces no edge. `Calls:` above lists only *observed* edges — check `depends_on_inferred` in `docs/service.yaml` and the platform service map before concluding this service calls nothing.

<!-- END GENERATED -->
