# rtd-simple: no-op onboarding RTD service — local build/run/test helpers.

IMAGE ?= rtd-simple:local
VERSION ?= v0.1.0-local
HTTP_ADDR ?= localhost:8080
GRPC_ADDR ?= localhost:50051
GO_IMAGE ?= golang:1.25.13-bookworm

# Per the IAB ARTF spec, the agent manifest is embedded in the image as a label
# named "agent-manifest" whose value is the manifest JSON. Keep manifest/manifest.json
# as the source of truth and inject it (minified) at build time. MINIFY_MANIFEST
# prints the single-line JSON; used as --label "agent-manifest=$$(...)" in builds.
MINIFY_MANIFEST = python3 -c 'import json;print(json.dumps(json.load(open("manifest/manifest.json"))))'
# agent-manifest-version pins the manifest schema/contract version to the image,
# alongside the agent-manifest label. It must be a supported schema version
# (currently only 1.0.0) and must equal metadata.version inside the manifest. It is
# NOT the CalVer image tag. Bump when the manifest format changes.
MANIFEST_VERSION ?= 1.0.0

# Standard OCI image labels. The license is Apache-2.0 (sample code, no attribution
# required); the source is also embedded in the image at /usr/src/rtd-simple.
OCI_LABELS = \
	--label "org.opencontainers.image.licenses=Apache-2.0" \
	--label "org.opencontainers.image.title=rtd-simple" \
	--label "org.opencontainers.image.description=No-op Index Cloud ARTF sample container. Go source embedded at /usr/src/rtd-simple (Apache-2.0)."

# Release (Docker Hub publish for Index Cloud). REPO is a public repo by default:
# these test/example containers are non-confidential, so Index pulls anonymously
# (no nexus@ read grant needed) and `docker push` auto-creates the public repo.
# TAG defaults to today's global CalVer artifact tag (YYYY.0M.0D.MICRO).
REPO ?= your-namespace/rtd-simple
TAG  ?= $(shell date -u +%Y.%m.%d).0

.PHONY: build up down logs ps health metrics unit harness grpc-health test validate-manifest verify release clean

build: ## Build the amd64 image (runs unit tests inside the build)
	docker build --platform=linux/amd64 --build-arg VERSION=$(VERSION) \
		--label "agent-manifest=$$($(MINIFY_MANIFEST))" \
		--label "agent-manifest-version=$(MANIFEST_VERSION)" \
		$(OCI_LABELS) -t $(IMAGE) .

up: ## Build and start via docker compose
	docker compose up --build -d --force-recreate

down: ## Stop and remove the container
	docker compose down

logs: ## Tail container logs (this is where the per-request no-op lines appear)
	docker compose logs -f

ps: ## Show container status (incl. HEALTHCHECK)
	docker compose ps

health: ## Curl liveness + readiness
	@echo "-- /heartbeat --"   && curl -fsS http://$(HTTP_ADDR)/heartbeat   && echo
	@echo "-- /healthcheck --" && curl -fsS http://$(HTTP_ADDR)/healthcheck && echo

metrics: ## Fetch Prometheus metrics (includes required rpc_server_duration)
	curl -fsS http://$(HTTP_ADDR)/metrics

unit: ## Run Go unit tests
	docker run --rm -v "$(PWD)":/work -w /work $(GO_IMAGE) go test ./...

harness: ## Run the conformance harness against the running container
	docker run --rm -v "$(PWD)":/work -w /work $(GO_IMAGE) \
		go run ./cmd/testrunner -addr host.docker.internal:50051 -metrics http://host.docker.internal:8080/metrics

grpc-health: ## Check the gRPC health service (needs grpcurl)
	grpcurl -plaintext $(GRPC_ADDR) grpc.health.v1.Health/Check

test: unit harness ## Run unit tests + harness

validate-manifest: ## Validate manifest/manifest.json against an image. Usage: make validate-manifest [IMAGE=rtd-simple:local]
	python3 scripts/validate-manifest.py $(IMAGE) --manifest manifest/manifest.json --models models

# verify runs the full pre-publish checks against $(IMAGE) and NEVER publishes:
#   build -> manifest-vs-image -> vulnerability scan -> endpoint/no-op contract.
# This is exactly what CI runs (see .github/workflows/ci.yml); `release` reuses it.
# It is safe to run anywhere (no registry credentials, no push).
verify: build ## Build + run all pre-publish checks against $(IMAGE); never pushes. CI runs this.
	@echo "==> verify 1/3 manifest matches image (labels, declared endpoints, metric contract)"
	python3 scripts/validate-manifest.py $(IMAGE) --manifest manifest/manifest.json --models models
	@echo "==> verify 2/3 vulnerability scan (fails on any CVSS>=9 / CRITICAL, the Index deploy gate)"
	docker run --rm -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy:latest \
		image --scanners vuln --severity CRITICAL --exit-code 1 --quiet $(IMAGE)
	@echo "==> verify 3/3 required endpoints (health + rpc_server_duration + no-op contract)"
	@# mount ./models so the check reproduces the deployed-with-model state (Index
	@# delivers the model via MODEL_PATH; it is not baked into the image).
	@CID=$$(docker run -d --platform=linux/amd64 -p 50051:50051 -p 8080:8080 -v "$(PWD)/models:/models:ro" $(IMAGE)); \
		sleep 3; \
		docker run --rm --network host -v "$(PWD)":/work -w /work $(GO_IMAGE) \
			go run ./cmd/testrunner -addr localhost:50051 -metrics http://localhost:8080/metrics; \
		rc=$$?; docker rm -f $$CID >/dev/null; \
		[ $$rc -eq 0 ] || { echo "endpoint check FAILED"; exit 1; }
	@echo "verify: all checks passed for $(IMAGE) (nothing published)"

release: ## Build, verify, then push for Index Cloud. Usage: make release REPO=you/rtd-simple [TAG=YYYY.0M.0D.N]
	@[ "$(REPO)" != "your-namespace/rtd-simple" ] || { echo "Set REPO, e.g. make release REPO=your-namespace/rtd-simple"; exit 1; }
	@echo "$(TAG)" | grep -Eq '^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]+(-[A-Za-z0-9.-]+)?(_(AMS|EMEA|APAC))?$$' \
		|| { echo "TAG '$(TAG)' is not a valid Index artifact tag: YYYY.0M.0D.MICRO[-annotation][_AMS|_EMEA|_APAC]"; exit 1; }
	@echo "==> naming OK: $(TAG)"
	@# Reuse the exact CI checks, built and run against the release tag (VERSION
	@# stamps the binary's build_info app_version so it matches the CalVer tag).
	$(MAKE) verify IMAGE=$(REPO):$(TAG) VERSION=$(TAG)
	@echo
	@echo "All checks passed. Pushing $(REPO):$(TAG) AUTO-DEPLOYS it to Index Cloud PRODUCTION."
	@printf "Type 'deploy' to push and deploy (anything else aborts): "; \
		read ans; [ "$$ans" = "deploy" ] || { echo "aborted; nothing pushed"; exit 1; }
	docker push $(REPO):$(TAG)
	@echo "Pushed $(REPO):$(TAG). Track the rollout in Grafana (Index Managed > Deployment Tracker)."

clean: ## Remove image and compose resources
	docker compose down --rmi local -v || true
