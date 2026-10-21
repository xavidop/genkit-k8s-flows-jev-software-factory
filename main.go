// Command genkit-k8s-flows-jev-software-factory turns a feature request into
// a reviewed Go module. Gemini designs, plans, writes and fixes the code in
// Kubernetes pods; jev, a decision model, decides at every gate whether the
// work moves on, goes back, or stops for a human.
//
//	go run . -request requests/token-bucket.md
//	senro run . -- -request requests/token-bucket.md
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
	"github.com/xavidop/senro"
	"github.com/xavidop/senro/attach"
)

type Secrets struct {
	Gemini   secret.String `source:"env:GEMINI_API_KEY"`
	TypeSafe secret.String `source:"env:TYPESAFE_API_KEY"`
}

func main() {
	ctx := context.Background()

	// Inside a pod, senro re-enters this binary to run a Func step.
	if handled, err := senro.StepChild(ctx); handled {
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	requestPath := flag.String("request", "requests/token-bucket.md", "the feature request to build")
	outDir := flag.String("out", "out", "where the records and the shipped module land")
	maxRework := flag.Int("max-rework", 2, "how many times jev may send the code back before a human is called")
	flag.Parse()

	request, err := filepath.Abs(*requestPath)
	if err != nil {
		log.Fatal(err)
	}
	slug := strings.TrimSuffix(filepath.Base(request), filepath.Ext(request))
	out, err := filepath.Abs(filepath.Join(*outDir, slug))
	if err != nil {
		log.Fatal(err)
	}
	secrets, err := mamori.Load[Secrets](ctx)
	if err != nil {
		log.Fatal(err)
	}

	run := func(p *senro.Pipeline, extra ...senro.Option) error {
		att, err := attach.Listen(ctx, attach.Options{Bind: attach.AutoUnixSocket, Pipeline: p.Name()})
		if err != nil {
			return err
		}
		defer func() { _ = att.Close() }()
		opts := append([]senro.Option{
			senro.WithAttach(att),
			senro.WithSecrets(secrets),
			senro.WithFuncBuild("."),
		}, extra...)
		return senro.Run(ctx, p, opts...)
	}

	// The factory is a state machine over senro runs. Each run ends with a
	// record jev produced, and the record picks the next run.
	state, reason, reworks := "design", "", 0
	for {
		switch state {
		case "design":
			if err := run(designPipeline(request, out)); err != nil {
				exit(err)
			}
			var d DesignResult
			read(out, "design.json", &d)
			fmt.Printf("\ndesign: %s after %d round(s)\n", d.Verdict, len(d.Rounds))
			if d.Verdict == "ready" {
				state = "implement"
			} else {
				state, reason = "human", fmt.Sprintf("the design review ended %q after %d round(s)", d.Verdict, len(d.Rounds))
			}

		case "implement", "rework":
			// A red vet or test is a failed step, so the run is partial; the
			// review still ran (ContinueOnError) and its verdict is what counts.
			if err := run(buildPipeline(out, state == "rework")); err != nil && !partial(err) {
				exit(err)
			}
			var r Review
			read(out, "review.json", &r)
			fmt.Printf("\nreview: %s (quality %s) %v\n", r.Verdict, r.Quality, r.Reasons)
			switch r.Verdict {
			case "ship":
				state = "ship"
			case "rework":
				if reworks >= *maxRework {
					state, reason = "human", fmt.Sprintf("jev asked for rework %d times: %v", reworks+1, r.Reasons)
				} else {
					reworks++
					state = "rework"
				}
			default:
				state, reason = "human", fmt.Sprintf("the code review ended %q: %v", r.Verdict, r.Reasons)
			}

		case "ship", "human":
			if err := run(finishPipeline(out, reason), senro.WithParams(senro.Params{"verdict": state})); err != nil {
				exit(err)
			}
			fmt.Printf("\nfinished: %s, records in %s\n", state, out)
			return
		}
	}
}

func read(dir, name string, v any) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		log.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		log.Fatal(err)
	}
}

// partial reports whether a run ended with some steps failed and the rest
// run, which is what a red build under ContinueOnError produces.
func partial(err error) bool {
	var runErr *senro.RunError
	return errors.As(err, &runErr) && fmt.Sprint(runErr.Status) == "partial"
}

func exit(err error) {
	var runErr *senro.RunError
	if errors.As(err, &runErr) {
		fmt.Fprintf(os.Stderr, "run ended: %s\n", runErr.Status)
		for _, s := range runErr.Steps {
			fmt.Fprintf(os.Stderr, "  %s %s (exit %d)\n", s.ID, s.State, s.ExitCode)
		}
		fmt.Fprintf(os.Stderr, "  evidence: %s/events.jsonl\n", runErr.Dir)
	} else {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}
