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

.PHONY: all build helper test vet clean install uninstall preflight record

all: build helper

build:
	$(GO) build -o $(BIN) $(CMD)

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

test:
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
	@echo "installed to $(PREFIX)"
	@command -v minutes >/dev/null 2>&1 || echo "note: $(PREFIX) is not on PATH"
	@echo "built from $$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')"

uninstall:
	rm -f $(PREFIX)/minutes $(PREFIX)/minutes-capture $(PREFIX)/minutes-capture.exe

clean:
	rm -f $(BIN) $(HELPER) dist/*.obj
