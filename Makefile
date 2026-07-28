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
# brainbase-docs is not a dependency of this repo, so it has to come from
# somewhere else. uvx runs it straight out of the docs checkout: no venv, no
# install step, and identical behaviour whether this repo is node, pip or uv.
#
# The alternatives all fail somewhere. `uv run brainbase-docs` resyncs the
# repo's venv from its lockfile and drops anything not in it. `pip install -e`
# hits PEP 668 on a Homebrew python3. And a bare `python` does not exist on
# macOS at all.
DOCS_RUN ?= uvx --from $(PLATFORM_DOCS) brainbase-docs

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
	  echo "  Could not run brainbase-docs via:"; \
	  echo "      $(DOCS_RUN)"; \
	  echo ""; \
	  echo "  This needs uv. Install it:"; \
	  echo "      curl -LsSf https://astral.sh/uv/install.sh | sh"; \
	  echo ""; \
	  echo "  Or point DOCS_RUN at your own install:"; \
	  echo "      make docs-manifest DOCS_RUN=\"python3 -m brainbase_docs.cli\""; \
	  echo ""; \
	  exit 1; }
	$(DOCS_RUN) manifest --registry $(PLATFORM_DOCS)/registry.yaml
	$(DOCS_RUN) agents-md
