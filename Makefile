# The build stays one command from the Linux side: the C++ helper is compiled by
# MSVC through WSL interop, so there is no separate Windows-side step.

GO      ?= go
CMD     := ./cmd/minutes
BIN     := dist/minutes
HELPER  := dist/minutes-capture.exe
CMDEXE  := /c/Windows/System32/cmd.exe

.PHONY: all build helper test vet clean preflight record

all: build helper

build:
	$(GO) build -o $(BIN) $(CMD)

# Invokes MSVC through interop. build.bat probes for a toolchain that is
# actually complete rather than the newest one installed.
helper:
	$(CMDEXE) /c 'native\windows\build.bat'

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

preflight: build helper
	$(BIN) preflight

# A short proof recording. Both tracks must come out non-silent.
record: build helper
	$(BIN) record --duration 15s --out recordings

clean:
	rm -f $(BIN) $(HELPER) dist/*.obj
