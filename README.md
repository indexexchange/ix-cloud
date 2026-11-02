# ix-cloud

Examples and tools for building **Index Cloud** containers: IAB ARTF
`RTBExtensionPoint` gRPC services that Index pulls from a registry and runs inside
its exchange, at the edge, next to the auction. On each auction the exchange calls
the container's `GetMutations` RPC and applies what it returns (shade a bid,
activate/suppress deals, adjust a floor, add segments/CIDs).

This repo is a starting point for partners and internal teams: copy an example,
change what your logic needs, validate it with the tools here, and ship.

## Layout

```
ix-cloud/
├── examples/     # complete, buildable sample containers (one folder each)
│   └── rtd-simple/        # no-op reference: full ARTF contract, zero mutations
├── tools/        # standalone tooling for building/validating containers (one folder each)
│   └── validate-manifest/ # checks a manifest.json against a built image
└── monitoring/   # Grafana dashboards / observability artifacts (one folder each)
    └── rtd-simple/        # performance, baseline, and diagnostic dashboards
```

Each example, tool, and dashboard set is self-contained and has its own `README.md`.

## Start here

- **New to Index Cloud containers?** Read
  [`examples/rtd-simple/`](examples/rtd-simple/) — it implements the full container
  contract (the required `rpc_server_duration` metric, gRPC health, the HTTP
  endpoints, the agent manifest, CalVer tagging, model-presence check) while doing
  nothing to the auction. It is the recommended first deploy: a clean canary that
  proves the onboarding pipeline end to end before you add decision logic.
- **Validating your own container?** Use
  [`tools/validate-manifest/`](tools/validate-manifest/) to confirm your image
  carries its manifest correctly and actually serves the endpoints and metric
  contract it declares.
- **Monitoring a deployed container?** Import the Grafana dashboards in
  [`monitoring/`](monitoring/) — peak/baseline/distribution views built on the
  metrics these containers expose.

## Continuous integration

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) builds and validates every
example on each pull request by running that example's `make verify` (build ->
manifest-vs-image -> `trivy` CVSS-9 scan -> endpoint/no-op contract). It **never
publishes** — pushing a compliant CalVer tag auto-deploys to Index Cloud production,
so that stays a deliberate manual step (`make release`). Each example also ships its
own copy of this workflow so it keeps working after a partner copies it out.

## Adding a new example, tool, or dashboard set

Put it in its own subfolder under `examples/`, `tools/`, or `monitoring/`, with a
`README.md` that explains what it is and how to use it. Keep examples self-contained
so they can be copied out and built on their own. For a new example, add its folder
name to the matrix in [.github/workflows/ci.yml](.github/workflows/ci.yml) so CI
builds and validates it. Never commit internal-only material (partner config,
credentials, private deploy notes) — see each example's `.dockerignore`.

## Licensing

This repository and its examples are licensed **Apache-2.0** (see
[`LICENSE`](LICENSE)). Each example also carries its own `LICENSE`; where present,
that license governs the example (currently all Apache-2.0).

Third-party material included here, and its licences, is listed in [`NOTICE`](NOTICE). Use of the
samples with Index services is also subject to the
[Index Exchange Sample Code Terms](SAMPLE-CODE-TERMS.md).
