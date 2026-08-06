GO ?= go
LAMBDAS := ingest publish
DIST := dist

.PHONY: all build test e2e fmt vet lint clean fixtures

all: test build

build: $(addprefix $(DIST)/,$(addsuffix .zip,$(LAMBDAS)))

$(DIST)/%.zip: FORCE
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -tags lambda.norpc -trimpath \
		-ldflags '-s -w' -o $(DIST)/bootstrap ./cmd/$*
	cd $(DIST) && zip -q -X $*.zip bootstrap && rm bootstrap

FORCE:

test:
	$(GO) test ./...

# End-to-end suites need Docker (LocalStack, ubuntu, ubi9) and Terraform.
#
# DOCKER_HOST is set explicitly because testcontainers resolves
# /var/run/docker.sock, which on a machine that also has Podman installed may
# be a different daemon than the docker CLI is using. The socket override is
# what LocalStack binds to launch its Lambda containers.
e2e: export DOCKER_HOST ?= unix://$(HOME)/.docker/run/docker.sock
e2e: export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE = /var/run/docker.sock
e2e:
	$(GO) test -tags e2e -timeout 40m ./test/e2e/...

# Regenerates testdata fixtures and golden files inside containers that have
# dpkg-deb, rpmbuild, apt-ftparchive and createrepo_c available.
fixtures:
	./testdata/generate.sh

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(DIST)
