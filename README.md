# A software factory on Kubernetes: Gemini builds, jev decides

A feature request goes in. A designed, planned, implemented, vetted, tested and
reviewed Go module comes out, or a brief for a human explaining why the factory
stopped. Every step is a [Genkit](https://genkit.dev) flow running as a pod through
[senro](https://github.com/xavidop/senro)'s Kubernetes executor. Gemini does the
writing. **jev**, a decision model reached through Genkit's
[System One plugin](https://genkit.dev/docs/go/integrations/system-one/), makes every
decision: is the design ready, should the code ship, go back, or stop.

```
design (run)        implement / rework (run)            finish (run)
  clean   local       implement | fix   Gemini             ship:   pr        Gemini
  design  Gemini+jev  vet               go vet               publish   local
  plan    Gemini      test              go test            human:  brief     Gemini
  collect local       review            jev                  notify    local
                      collect           local
```

The coordinator is a small state machine: each run ends with a record jev
produced (`design.json`, `review.json`), and the record picks the next run.
Inside the design step, Gemini and jev loop up to three times before the
design is called ready.

## What you need

- Go 1.27+, Docker, [kind](https://kind.sigs.k8s.io), `kubectl`
- `GEMINI_API_KEY` and `TYPESAFE_API_KEY` in your environment
- optionally the [senro CLI](https://xavidop.github.io/senro/docs/install/)

## Run it

```sh
scripts/cluster-up.sh            # kind cluster + RBAC + the three files senro reads
source scripts/cluster-env.sh

go run . -request requests/token-bucket.md       # ships a ratelimit package
go run . -request requests/make-it-faster.md     # too vague: jev stops it, a human gets a brief
senro run . -- -request requests/token-bucket.md # same, with the terminal UI
```

Records land in `out/<request>/`: `DESIGN.md`, `PLAN.md`, `design.json`,
`review.json`, `vet.log`, `test.log`, and when it ships, `PR.md` and the module
under `src/`.

```sh
scripts/cluster-down.sh
```

## Files

| File | What it holds |
|---|---|
| `flows.go` | the Genkit flows and jev's question types |
| `steps.go` | the `senro.Func` steps around them |
| `pipeline.go` | the three pipelines, the Go image, the persistent workspace |
| `main.go` | the state machine over runs |
| `k8s/rbac.yaml` | the namespace and the permissions senro needs |
| `requests/` | two sample requests |
