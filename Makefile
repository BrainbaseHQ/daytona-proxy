PLATFORM_DOCS ?= ./platform-docs
# ONE shared checkout, symlinked into each repo. Sixteen repos each cloning
# their own copy would be sixteen things to keep current, pulled at different
# times, with no way to tell which is stale.
#
# If you already keep a checkout next to this repo, that one is used and is
# never pulled -- it is yours, and it may be on a branch or have local edits.
PLATFORM_DOCS_HOME ?= $(HOME)/.cache/brainbase/platform-docs
DOCS_REPO_SSH := git@github.com:BrainbaseHQ/brainbase-platform-docs.git
DOCS_REPO_HTTPS := https://github.com/BrainbaseHQ/brainbase-platform-docs.git
DOCS_RUN ?= python -m brainbase_docs.cli

.PHONY: docs-sync docs-manifest

## Point ./platform-docs at the shared docs checkout, cloning it if needed.
## Agents are sent here by AGENTS.md for cross-service context.
## SSH first, then HTTPS: `gh auth login` issues an HTTPS token by default, so
## an SSH-only clone fails with "Permission denied (publickey)" for anyone
## without a key -- on their very first `make docs-manifest`.
docs-sync:
	@set -e; \
	target="$(PLATFORM_DOCS_HOME)"; ours=1; \
	if [ -d "../brainbase-platform-docs/.git" ]; then \
	  target="$$(cd ../brainbase-platform-docs && pwd)"; ours=0; \
	fi; \
	if [ "$$ours" = "1" ]; then \
	  if [ -d "$$target/.git" ]; then \
	    git -C "$$target" pull --ff-only -q || echo "  (could not fast-forward $$target; using it as-is)"; \
	  else \
	    mkdir -p "$$(dirname "$$target")"; \
	    git clone --depth 1 "$(DOCS_REPO_SSH)" "$$target" 2>/dev/null \
	      || git clone --depth 1 "$(DOCS_REPO_HTTPS)" "$$target"; \
	  fi; \
	else \
	  echo "  using your checkout at $$target (not pulling it)"; \
	fi; \
	if [ -L "$(PLATFORM_DOCS)" ]; then rm -f "$(PLATFORM_DOCS)"; \
	elif [ -e "$(PLATFORM_DOCS)" ]; then rm -rf "$(PLATFORM_DOCS)"; fi; \
	ln -s "$$target" "$(PLATFORM_DOCS)"; \
	echo "  $(PLATFORM_DOCS) -> $$target"

## Regenerate docs/service.yaml and the generated block in AGENTS.md.
## Idempotent: re-running on unchanged code produces no diff.
docs-manifest: docs-sync
	@$(DOCS_RUN) --help >/dev/null 2>&1 || { \
	  echo ""; \
	  echo "  brainbase-docs is not installed in this environment."; \
	  echo "  Install it from the checkout docs-sync just made:"; \
	  echo ""; \
	  echo "      pip install -e $(PLATFORM_DOCS)"; \
	  echo ""; \
	  exit 1; }
	$(DOCS_RUN) manifest --registry $(PLATFORM_DOCS)/registry.yaml
	$(DOCS_RUN) agents-md
