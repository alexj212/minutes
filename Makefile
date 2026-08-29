# The build stays one command from the Linux side: the C++ helper is compiled by
# MSVC through WSL interop, so there is no separate Windows-side step.

GO      ?= go
CMD     := ./cmd/minutes
BIN     := dist/minutes
ifeq ($(shell uname -s),Darwin)
HELPER  := dist/minutes-capture
else
HELPER  := dist/minutes-capture.exe
endif
CMDEXE  := /c/Windows/System32/cmd.exe

# Where `make install` puts things. ~/bin is on PATH on both this machine and
# the Mac, which is why it is the default rather than /usr/local/bin.
PREFIX ?= $(HOME)/bin

.PHONY: all build helper test vet clean install uninstall dist publish preflight record check-install

all: build helper

# Stamped the way shabadoo's build does, so a publisher can verify any tool the
# same way. Without these the binary still answers `version`, from what the
# toolchain records about the commit — a development build should be able to say
# which commit it is and whether the tree was dirty.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.built=$(BUILT)

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) $(CMD)

# The native helper for whichever machine this is.
#
# On WSL that means invoking MSVC through interop — build.bat probes for a
# toolchain that is actually complete rather than the newest one installed. On
# macOS it means the CoreAudio helper. There is deliberately no third case:
# a platform without a helper should fail here rather than produce an
# orchestrator that cannot record and only says so at the meeting.
UNAME := $(shell uname -s)

helper:
ifeq ($(UNAME),Darwin)
	native/darwin/build.sh
else
	$(CMDEXE) /c 'native\windows\build.bat'
endif

# Warns when the installed binary is not this working tree.
#
# Run from `test` because that is the moment somebody is about to believe a
# result. The trap is not `go test` — it compiles what is here — it is the
# manual check afterwards against whatever `minutes` happens to be on PATH.
check-install:
	@sh scripts/staleness-check.sh || true

test: check-install
	$(GO) test ./...

vet:
	$(GO) vet ./...

preflight: build helper
	$(BIN) preflight

# A short proof recording. Both tracks must come out non-silent.
record: build helper
	$(BIN) record --duration 15s --segment 5s

# Both binaries go together. The orchestrator finds the helper by looking
# beside itself first, so installing one without the other leaves a `minutes`
# that refuses to record and says the helper is missing.
#
# Windows loads the helper off the WSL filesystem through interop — verified,
# because it is the kind of thing that is easy to assume and awkward to
# discover later.
# Installing from a dirty tree deploys a binary no commit can reproduce. Go
# stamps the working state into the binary, so this is checkable rather than a
# matter of discipline — and it has already happened once here.
install: build helper
	@if [ -n "$$(git status --porcelain 2>/dev/null)" ]; then \
		echo "WARNING: the working tree is dirty."; \
		echo "         The installed binary will be stamped vcs.modified=true and"; \
		echo "         will not be reproducible from any commit. Commit first, or"; \
		echo "         check with: go version -m $(PREFIX)/minutes"; \
	fi
	@mkdir -p $(PREFIX)
	install -m 0755 $(BIN) $(PREFIX)/minutes
	@install -m 0755 $(HELPER) $(PREFIX)/$(notdir $(HELPER))
	@if [ -f dist/minutes-tray.exe ]; then install -m 0755 dist/minutes-tray.exe $(PREFIX)/minutes-tray.exe; fi
	@if [ -f dist/minutes-tray ]; then install -m 0755 dist/minutes-tray $(PREFIX)/minutes-tray; fi
	@echo "installed to $(PREFIX)"
	@command -v minutes >/dev/null 2>&1 || echo "note: $(PREFIX) is not on PATH"
	@echo "built from $$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')"

# A publishable release set for THIS machine, and only this machine.
#
# minutes cannot cross-build a release the way a pure-Go tool can. The
# orchestrator cross-compiles happily; the capture helpers do not — the Windows
# one needs MSVC reachable over interop, the macOS one needs swiftc and a
# signing identity. So a release set is built on the machine it targets, and
# there is no host that can produce all of them.
#
# That is a fact about the tool rather than a limitation to route around: the
# helper exists precisely because the audio APIs are not portable.
dist: build helper
	@rm -rf dist/release && mkdir -p dist/release
	@cp $(BIN) dist/release/
	@cp $(HELPER) dist/release/
	@if [ -f dist/minutes-tray.exe ]; then cp dist/minutes-tray.exe dist/release/; fi
	@if [ -f dist/minutes-tray ]; then cp dist/minutes-tray dist/release/; fi
	@$(BIN) version --json > dist/release/version.json
	@echo "release set for $$($(BIN) version --json | grep '"platform"' | head -1 | cut -d'"' -f4):"
	@ls -1 dist/release | sed 's/^/  /'
	@echo "  (built here; the other platform's set must be built on that machine)"

# Publishing is the tool's call, not the coordinator's, because only the tool
# knows whether it has built a whole set. Half a set is worse than no set: it
# installs, it starts, and it fails at the moment somebody tries to record.
# So this refuses locally and names what is missing, rather than letting the
# coordinator refuse from a distance.
#
# Only ever publishes the platform it was built on. No host can build every
# set — a Windows helper needs MSVC, a darwin helper needs Xcode and a signing
# identity — so each machine publishes its own and the coordinator merges them.
publish: dist
	@sh scripts/publish-guard.sh $(BIN)
	shabadoo publish --tool minutes dist/release

uninstall:
	rm -f $(PREFIX)/minutes $(PREFIX)/minutes-capture $(PREFIX)/minutes-capture.exe
	rm -f $(PREFIX)/minutes-tray $(PREFIX)/minutes-tray.exe

clean:
	rm -f $(BIN) $(HELPER) dist/*.obj
