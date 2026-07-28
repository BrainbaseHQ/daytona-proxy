
PLATFORM_DOCS ?= ./platform-docs
DOCS_RUN ?= python -m brainbase_docs.cli

.PHONY: docs-sync docs-manifest

## Clone/refresh the platform docs into a gitignored ./platform-docs.
## AGENTS.md points agents here for cross-service context.
docs-sync:
	@if [ -d "$(PLATFORM_DOCS)/.git" ]; then \
		git -C "$(PLATFORM_DOCS)" pull --ff-only -q; \
	else \
		git clone --depth 1 git@github.com:BrainbaseHQ/brainbase-platform-docs.git "$(PLATFORM_DOCS)"; \
	fi

## Regenerate docs/service.yaml and the generated block in AGENTS.md.
docs-manifest: docs-sync
	$(DOCS_RUN) manifest --registry $(PLATFORM_DOCS)/registry.yaml
	$(DOCS_RUN) agents-md
