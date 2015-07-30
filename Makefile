# Set an output prefix, which is the local directory if not specified
PREFIX?=$(shell pwd)

# Populate version variables
# Add to compile time flags
NOTARY_PKG := github.com/docker/notary
VERSION := $(shell cat NOTARY_VERSION)
GITCOMMIT := $(shell git rev-parse --short HEAD)
GITUNTRACKEDCHANGES := $(shell git status --porcelain --untracked-files=no)
ifneq ($(GITUNTRACKEDCHANGES),)
	GITCOMMIT := $(GITCOMMIT)-dirty
endif
CTIMEVAR=-X $(NOTARY_PKG)/version.GitCommit '$(GITCOMMIT)' -X $(NOTARY_PKG)/version.NotaryVersion '$(NOTARY_VERSION)'
GO_LDFLAGS=-ldflags "-w $(CTIMEVAR)"
GO_LDFLAGS_STATIC=-ldflags "-w $(CTIMEVAR) -extldflags -static"
GOOSES = darwin freebsd linux
GOARCHS = amd64

# go cover test variables
COVERDIR=.cover
COVERPROFILE=$(COVERDIR)/cover.out
COVERMODE=count
PKGS = $(shell go list ./... | tr '\n' ' ')

.PHONY: clean all fmt vet lint build test binaries cross cover
.DELETE_ON_ERROR: cover
.DEFAULT: default

all: AUTHORS clean fmt vet fmt lint build test binaries

AUTHORS: .git/HEAD
	git log --format='%aN <%aE>' | sort -fu > $@

# This only needs to be generated by hand when cutting full releases.
version/version.go:
	./version/version.sh > $@

${PREFIX}/bin/notary-server: NOTARY_VERSION $(shell find . -type f -name '*.go')
	@echo "+ $@"
	@godep go build -o $@ ${GO_LDFLAGS} ./cmd/notary-server

${PREFIX}/bin/notary: NOTARY_VERSION $(shell find . -type f -name '*.go')
	@echo "+ $@"
	@godep go build -o $@ ${GO_LDFLAGS} ./cmd/notary

${PREFIX}/bin/notary-signer: NOTARY_VERSION $(shell find . -type f -name '*.go')
	@echo "+ $@"
	@godep go build -o $@ ${GO_LDFLAGS} ./cmd/notary-signer

vet:
	@echo "+ $@"
	@test -z "$$(go tool vet -printf=false . 2>&1 | grep -v Godeps/_workspace/src/ | tee /dev/stderr)"

fmt:
	@echo "+ $@"
	@test -z "$$(gofmt -s -l .| grep -v .pb. | grep -v Godeps/_workspace/src/ | tee /dev/stderr)"

lint:
	@echo "+ $@"
	@test -z "$$(golint ./... | grep -v .pb. | grep -v Godeps/_workspace/src/ | tee /dev/stderr)"

build:
	@echo "+ $@"
	@go build -v ${GO_LDFLAGS} ./...

test:
	@echo "+ $@"
	@go test -test.short ./...

test-full: vet lint
	@echo "+ $@"
	@go test -v ./...

protos:
	@protoc --go_out=plugins=grpc:. proto/*.proto



define gocover
go test -covermode="$(COVERMODE)" -coverprofile="$(COVERDIR)/$(subst /,-,$(1)).cover" "$(1)";
endef

cover:
	@mkdir -p "$(COVERDIR)"
	$(foreach PKG,$(PKGS),$(call gocover,$(PKG)))
	@echo "mode: $(COVERMODE)" > "$(COVERPROFILE)"
	@grep -h -v "^mode:" "$(COVERDIR)"/*.cover >> "$(COVERPROFILE)"
	@go tool cover -func="$(COVERPROFILE)"
	@go tool cover -html="$(COVERPROFILE)"

clean-protos:
	@rm proto/*.pb.go

binaries: ${PREFIX}/bin/notary-server ${PREFIX}/bin/notary ${PREFIX}/bin/notary-signer
	@echo "+ $@"


define template
mkdir -p ${PREFIX}/cross/$(1)/$(2);
GOOS=$(1) GOARCH=$(2) go build -o ${PREFIX}/cross/$(1)/$(2)/notary -a -tags "static_build netgo" -installsuffix netgo ${GO_LDFLAGS_STATIC} ./cmd/notary;
endef

cross:
	$(foreach GOARCH,$(GOARCHS),$(foreach GOOS,$(GOOSES),$(call template,$(GOOS),$(GOARCH))))

clean:
	@echo "+ $@"
	@rm -rf "${PREFIX}/bin/notary-server" "${PREFIX}/bin/notary" "${PREFIX}/bin/notary-signer"
