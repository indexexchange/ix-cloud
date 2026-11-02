# rtd-ab: analysis-only A/B RTD experiment, local build/run/test helpers.

IMAGE ?= rtd-ab:local
VERSION ?= v0.1.0-local
HTTP_ADDR ?= localhost:8080
GRPC_ADDR ?= localhost:50051
GO_IMAGE ?= golang:1.26.10-bookworm

# Share of auctions routed to the test arm, in percent, baked into the binary at
# build time (-X main.bakedTestPct). The container aligns it to the nearest 1/16
# (6.25%) by the last hex digit of the auction id: 10 becomes 2 of 16 = 12.5. The
# rtd.ab.test_pct image label carries the aligned value. Changing the split needs a
# rebuild and a new tag. Validated below: a number from 0 to 100 with at most 2
# decimals.
AB_TEST_PCT ?= 12.5
AB_EFFECTIVE_PCT = $(shell python3 -c "from decimal import Decimal as D;k=int((D('$(AB_TEST_PCT)')*16/100+D('0.5')).to_integral_value(rounding='ROUND_FLOOR'));print(format((D(k)*D('6.25')).normalize(),'f'))")
AB_PCT_RE := ^(100(\.0{1,2})?|[0-9]{1,2}(\.[0-9]{1,2})?)$$
check-pct:
	@echo "$(AB_TEST_PCT)" | grep -Eq '$(AB_PCT_RE)' \
		|| { echo "AB_TEST_PCT '$(AB_TEST_PCT)' invalid: want a number from 0 to 100 with at most 2 decimals"; exit 1; }

# Per the IAB ARTF spec, the agent manifest is embedded in the image as a label
# named "agent-manifest" whose value is the manifest JSON. Keep manifest/manifest.json
# as the source of truth and inject it (minified) at build time. MINIFY_MANIFEST
# prints the single-line JSON; used as --label "agent-manifest=$$(...)" in builds.
MANIFEST_OUT = manifest/manifest.json
MINIFY_MANIFEST = python3 -c 'import json;print(json.dumps(json.load(open("$(MANIFEST_OUT)"))))'
# agent-manifest-version pins the manifest schema/contract version to the image,
# alongside the agent-manifest label. It must be a supported schema version
# (currently only 1.0.0) and must equal metadata.version inside the manifest. It is
# NOT the CalVer image tag. Bump when the manifest format changes.
MANIFEST_VERSION ?= 1.0.0

# Standard OCI image labels. The license is Apache-2.0 (sample code, no attribution
# required); the source is also embedded in the image at /usr/src/rtd-ab.
OCI_LABELS = \
	--label "org.opencontainers.image.licenses=Apache-2.0" \
	--label "org.opencontainers.image.title=rtd-ab" \
	--label "org.opencontainers.image.description=Analysis-only A/B experiment Index Cloud ARTF container; both arms return zero mutations (Apache-2.0)." \
	--label "rtd.ab.test_pct=$(AB_EFFECTIVE_PCT)"

# Release (Docker Hub publish for Index Cloud). REPO is a public repo by default:
# these test/example containers are non-confidential, so Index pulls anonymously
# (no nexus@ read grant needed) and `docker push` auto-creates the public repo.
# TAG defaults to today's global CalVer artifact tag (YYYY.0M.0D.MICRO).
REPO ?= your-namespace/rtd-ab
TAG  ?= $(shell date -u +%Y.%m.%d).0

.PHONY: proto trivy check-pct bench race build up down logs ps health metrics unit harness grpc-health test validate-manifest verify release clean

# Vulnerability gate. The image fails the scan on a finding of ANY severity (CRITICAL
# to UNKNOWN; new advisories often arrive as UNKNOWN) that has a fixed version
# (--ignore-unfixed skips findings with no fix available, which a rebuild cannot
# resolve). Stricter than a CRITICAL-only gate on purpose: Index Cloud's deploy
# scan can reject an image on lower-severity findings. Run on every `make verify`
# and therefore on every `make release`; it pulls the latest trivy database each
# run, so a new advisory can fail a previously clean build.
TRIVY_SEVERITY ?= CRITICAL,HIGH,MEDIUM,LOW,UNKNOWN
trivy: ## Scan $(IMAGE) with trivy; fails on any finding that has a fix
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy:latest \
		image --scanners vuln --severity $(TRIVY_SEVERITY) --ignore-unfixed --exit-code 1 $(IMAGE)

build: check-pct ## Build the amd64 image (runs unit tests inside the build)
	docker build --platform=linux/amd64 --build-arg VERSION=$(VERSION) --build-arg AB_TEST_PCT=$(AB_TEST_PCT) \
		--label "agent-manifest=$$($(MINIFY_MANIFEST))" \
		--label "agent-manifest-version=$(MANIFEST_VERSION)" \
		$(OCI_LABELS) -t $(IMAGE) .

up: ## Build and start via docker compose
	docker compose up --build -d --force-recreate

down: ## Stop and remove the container
	docker compose down

logs: ## Tail container logs (startup lines, errors, one summary line per arm per minute)
	docker compose logs -f

ps: ## Show container status (incl. HEALTHCHECK)
	docker compose ps

health: ## Curl liveness + readiness
	@echo "-- /heartbeat --"   && curl -fsS http://$(HTTP_ADDR)/heartbeat   && echo
	@echo "-- /healthcheck --" && curl -fsS http://$(HTTP_ADDR)/healthcheck && echo

metrics: ## Fetch Prometheus metrics (required rpc_server_duration plus rtd_ab_*)
	curl -fsS http://$(HTTP_ADDR)/metrics

# pkg/pb (protobuf Go bindings) is generated, not committed. Targets that run Go
# code on the host or in a golang container depend on this stamp, which re-runs
# generation whenever a .proto file or the generator script changes.
PB_STAMP := pkg/pb/.generated
PB_SRC := $(shell find proto -name '*.proto') scripts/gen-proto.sh

proto: ## Generate pkg/pb from the IAB .proto files in proto/ (Docker; pinned, checksum-verified protoc)
	docker run --rm -v "$(PWD)":/work -w /work \
		-e HOST_UID=$$(id -u) -e HOST_GID=$$(id -g) $(GO_IMAGE) sh scripts/gen-proto.sh
	@touch $(PB_STAMP)

$(PB_STAMP): $(PB_SRC)
	$(MAKE) proto

race: $(PB_STAMP) ## Run unit tests with the race detector (host Go, needs cgo)
	go test -race -count=1 ./...

bench: $(PB_STAMP) ## Router, strategy and recorder benchmarks with allocations
	go test -run xxx -bench . -benchmem ./internal/router ./internal/ab

unit: $(PB_STAMP) ## Run Go unit tests
	docker run --rm -v "$(PWD)":/work -w /work $(GO_IMAGE) go test ./...

harness: $(PB_STAMP) ## Run the harness (baked split + overrides 0, 10, 33.33, 100) against fresh containers of $(IMAGE)
	scripts/harness.sh $(IMAGE) $(GO_IMAGE) $(AB_TEST_PCT)

grpc-health: ## Check the gRPC health service (needs grpcurl)
	grpcurl -plaintext $(GRPC_ADDR) grpc.health.v1.Health/Check

test: unit harness ## Run unit tests + harness (the harness starts its own containers)

validate-manifest: ## Validate manifest/manifest.json against an image. Usage: make validate-manifest [IMAGE=rtd-ab:local]
	python3 scripts/validate-manifest.py $(IMAGE) --manifest $(MANIFEST_OUT) --models models

# verify runs the full pre-publish checks against $(IMAGE) and NEVER publishes:
#   build -> manifest-vs-image -> vulnerability scan -> endpoint/no-op contract.
# This is exactly what CI runs (see .github/workflows/ci.yml); `release` reuses it.
# It is safe to run anywhere (no registry credentials, no push).
verify: $(PB_STAMP) build ## Build + run all pre-publish checks against $(IMAGE); never pushes. CI runs this.
	@echo "==> verify 1/3 manifest matches image (labels, declared endpoints, metric contract)"
	python3 scripts/validate-manifest.py $(IMAGE) --manifest $(MANIFEST_OUT) --models models
	@echo "==> verify 2/3 vulnerability scan (fails on any $(TRIVY_SEVERITY) finding that has a fix)"
	$(MAKE) --no-print-directory trivy IMAGE=$(IMAGE)
	@echo "==> verify 3/3 endpoint contract, zero-mutation no-op, split share per lifecycle, metrics, log volume"
	@# mounts ./models so the check reproduces the deployed-with-model state (Index
	@# delivers the model via MODEL_PATH; it is not baked into the image).
	scripts/harness.sh $(IMAGE) $(GO_IMAGE) $(AB_TEST_PCT)
	@echo "verify: all checks passed for $(IMAGE) (nothing published)"

release: ## Build, verify, then push for Index Cloud. Usage: make release REPO=you/rtd-ab [TAG=YYYY.0M.0D.N]
	@[ "$(REPO)" != "your-namespace/rtd-ab" ] || { echo "Set REPO, e.g. make release REPO=your-namespace/rtd-ab"; exit 1; }
	@echo "$(TAG)" | grep -Eq '^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]+(-[A-Za-z0-9.-]+)?(_(AMS|EMEA|APAC))?$$' \
		|| { echo "TAG '$(TAG)' is not a valid Index artifact tag: YYYY.0M.0D.MICRO[-annotation][_AMS|_EMEA|_APAC]"; exit 1; }
	@# The vulnerability gate may not be weakened for a release.
	@[ "$(TRIVY_SEVERITY)" = "CRITICAL,HIGH,MEDIUM,LOW,UNKNOWN" ] \
		|| { echo "TRIVY_SEVERITY must be CRITICAL,HIGH,MEDIUM,LOW,UNKNOWN for a release (got '$(TRIVY_SEVERITY)')"; exit 1; }
	@echo "==> naming OK: $(TAG)"
	@if docker buildx imagetools inspect $(REPO):$(TAG) >/dev/null 2>&1; then \
		echo "TAG $(TAG) already exists in $(REPO); refusing to overwrite it"; exit 1; fi
	@# Reuse the exact CI checks, built and run against the release tag (VERSION
	@# stamps the binary's build_info app_version so it matches the CalVer tag).
	$(MAKE) verify IMAGE=$(REPO):$(TAG) VERSION=$(TAG) AB_TEST_PCT=$(AB_TEST_PCT)
	@echo
	@echo "All checks passed. Pushing $(REPO):$(TAG) AUTO-DEPLOYS it to Index Cloud PRODUCTION."
	@printf "Type 'deploy' to push and deploy (anything else aborts): "; \
		read ans; [ "$$ans" = "deploy" ] || { echo "aborted; nothing pushed"; exit 1; }
	docker push $(REPO):$(TAG)
	@echo "Pushed $(REPO):$(TAG). Track the rollout in Grafana (Index Managed > Deployment Tracker)."

clean: ## Remove image and compose resources
	docker compose down --rmi local -v || true
