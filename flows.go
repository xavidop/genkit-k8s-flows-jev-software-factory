package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/core/api"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/googlegenai"
	systemonex "github.com/firebase/genkit/go/plugins/systemone/exp"
	"github.com/xavidop/senro"
)

// Two models, two jobs. Gemini designs, plans, writes and fixes code; jev
// decides whether any of it is good enough to move to the next stage.
const (
	chatModel     = "googleai/gemini-3.8-flash"
	decisionModel = "typesafe/jev-latest"

	// maxDesignRounds bounds the design/review loop inside the design flow.
	maxDesignRounds = 3
)

// --- what Gemini produces --------------------------------------------------

type Design struct {
	Title      string   `json:"title"`
	Summary    string   `json:"summary"`
	Approach   string   `json:"approach"`
	Components []string `json:"components"`
	Risks      []string `json:"risks"`
	TestPlan   []string `json:"test_plan"`
}

type Plan struct {
	Module string        `json:"module"`
	Files  []PlannedFile `json:"files"`
	Steps  []string      `json:"steps"`
}

type PlannedFile struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}

type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Code struct {
	Files []File `json:"files"`
	Notes string `json:"notes,omitempty"`
}

// --- what jev decides --------------------------------------------------------
//
// The options, levels and criteria belong to Go types, so every review asks
// the same questions with the same meaning.

type DesignVerdict string

func (DesignVerdict) Criteria() map[DesignVerdict]string {
	return map[DesignVerdict]string{
		"ready":  "Complete, unambiguous, scoped to the request, and testable as described",
		"revise": "The right direction, with gaps a revision can close",
		"reject": "The request is too vague or too large to design from; a human has to clarify it first",
	}
}

type Risk int

func (Risk) Levels() []string { return []string{"Low", "Medium", "High"} }

// Every yes/no question here is one a revision can act on: a "yes" becomes
// a line of feedback for the next round.
type DesignReview struct {
	Verdict        systemonex.Choice[DesignVerdict] `json:"verdict" jsonschema_description:"Is this design ready to implement?"`
	RequestClear   systemonex.Noul                  `json:"request_clear" jsonschema_description:"Does the request say what to build precisely enough that two engineers would build the same thing?"`
	ScopeCreep     systemonex.Noul                  `json:"scope_creep" jsonschema_description:"Does the design add things the request did not ask for?"`
	Underspecified systemonex.Noul                  `json:"underspecified" jsonschema_description:"Is any component described too loosely to implement without guessing: missing fields, signatures, edge cases or error behaviour?"`
	MissingTests   systemonex.Noul                  `json:"missing_tests" jsonschema_description:"Does the test plan leave any requirement of the request untested?"`
	Risk           systemonex.Score[Risk]           `json:"risk" jsonschema_description:"How risky is implementing this design as written?"`
}

type CodeVerdict string

func (CodeVerdict) Criteria() map[CodeVerdict]string {
	return map[CodeVerdict]string{
		"ship":   "Builds, tests pass, does what the request asked, no obvious defects",
		"rework": "Fixable problems: failing or missing tests, a missed requirement, sloppy code",
		"human":  "Something a model should not decide: security, data loss, or the request was misunderstood",
	}
}

type Quality int

func (Quality) Levels() []string { return []string{"Poor", "Acceptable", "Good", "Excellent"} }

type CodeReview struct {
	Verdict         systemonex.Choice[CodeVerdict] `json:"verdict" jsonschema_description:"Should this change ship?"`
	Quality         systemonex.Score[Quality]      `json:"quality" jsonschema_description:"How good is the code?"`
	CoversRequest   systemonex.Noul                `json:"covers_request" jsonschema_description:"Does the implementation do everything the request asked for?"`
	TestsMeaningful systemonex.Noul                `json:"tests_meaningful" jsonschema_description:"Do the tests exercise the behaviour the request cares about, rather than just compile?"`
	BuildGreen      systemonex.Noul                `json:"build_green" jsonschema_description:"According to the logs and exit codes, did go vet and go test both pass?"`
}

// --- the records the pipeline keeps -----------------------------------------

type DesignRound struct {
	Round          int      `json:"round"`
	Verdict        string   `json:"verdict"`
	Ready          float64  `json:"p_ready"`
	RequestClear   float64  `json:"request_clear"`
	ScopeCreep     float64  `json:"scope_creep"`
	Underspecified float64  `json:"underspecified"`
	MissingTests   float64  `json:"missing_tests"`
	Risk           string   `json:"risk"`
	Feedback       []string `json:"feedback,omitempty"`
}

type DesignResult struct {
	Design  Design        `json:"design"`
	Verdict string        `json:"verdict"`
	Rounds  []DesignRound `json:"rounds"`
	Model   string        `json:"model"`
}

type Review struct {
	Verdict         string   `json:"verdict"`
	Confidence      float64  `json:"confidence"`
	Quality         string   `json:"quality"`
	CoversRequest   float64  `json:"covers_request"`
	TestsMeaningful float64  `json:"tests_meaningful"`
	BuildGreen      float64  `json:"build_green"`
	Reasons         []string `json:"reasons,omitempty"`
	Model           string   `json:"model"`
}

// --- flow inputs -------------------------------------------------------------

type ImplementInput struct {
	Request string `json:"request"`
	Design  Design `json:"design"`
	Plan    Plan   `json:"plan"`
}

type ReviewInput struct {
	Request  string `json:"request"`
	Plan     Plan   `json:"plan"`
	Files    []File `json:"files"`
	VetLog   string `json:"vet_log"`
	VetExit  int    `json:"vet_exit"`
	TestLog  string `json:"test_log"`
	TestExit int    `json:"test_exit"`
}

type FixInput struct {
	Request string `json:"request"`
	Plan    Plan   `json:"plan"`
	Files   []File `json:"files"`
	Review  Review `json:"review"`
	VetLog  string `json:"vet_log"`
	TestLog string `json:"test_log"`
}

type PRInput struct {
	Request string `json:"request"`
	Design  Design `json:"design"`
	Files   []File `json:"files"`
	Review  Review `json:"review"`
	TestLog string `json:"test_log"`
}

type BriefInput struct {
	Request string        `json:"request"`
	Design  *DesignResult `json:"design,omitempty"`
	Review  *Review       `json:"review,omitempty"`
	Reason  string        `json:"reason"`
}

type Flows struct {
	Design    *core.Flow[string, DesignResult, struct{}]
	Plan      *core.Flow[Design, Plan, struct{}]
	Implement *core.Flow[ImplementInput, Code, struct{}]
	Review    *core.Flow[ReviewInput, Review, struct{}]
	Fix       *core.Flow[FixInput, Code, struct{}]
	PR        *core.Flow[PRInput, string, struct{}]
	Brief     *core.Flow[BriefInput, string, struct{}]
}

func defineFlows(g *genkit.Genkit) *Flows {
	f := &Flows{}

	// Design is a loop: Gemini designs, jev reviews, Gemini revises with the
	// review as feedback, until jev says ready or reject, or the rounds run out.
	f.Design = genkit.DefineFlow(g, "designFeature", func(ctx context.Context, request string) (DesignResult, error) {
		var res DesignResult
		feedback := ""
		for round := 1; round <= maxDesignRounds; round++ {
			prompt := "Request:\n" + request
			if round > 1 {
				prompt += "\n\nYour previous design:\n" + asJSON(res.Design) + "\n\nReviewer feedback:\n" + feedback
			}
			d, _, err := genkit.GenerateData[Design](ctx, g,
				ai.WithModelName(chatModel),
				ai.WithSystem("You are a senior Go engineer writing a short design for exactly what the request asks. "+
					"Stay inside the request: no extra features. Components are the Go types and functions you will create. "+
					"The test plan names concrete, deterministic test cases. Be brief."),
				ai.WithPrompt("%s", prompt))
			if err != nil {
				return res, err
			}
			if d == nil {
				return res, fmt.Errorf("design: the model returned no design")
			}
			res.Design = *d

			out, resp, err := genkit.GenerateData[DesignReview](ctx, g,
				ai.WithModelName(decisionModel),
				ai.WithSystem("The state has a feature request and a design written for it."),
				ai.WithPromptParts(ai.NewDataPart(map[string]any{"request": request, "design": res.Design})))
			if err != nil {
				return res, err
			}
			if out == nil {
				return res, fmt.Errorf("design review: the model returned no answers")
			}
			// jev's verdict, then our rules on top. The yes/no answers are
			// the feedback: each one over the line is a sentence Gemini can act on.
			verdict := string(out.Verdict.Choice)
			var notes []string
			if out.RequestClear.Probability < 0.4 {
				verdict = "reject" // nobody should design from a request two engineers would read differently
			}
			if out.ScopeCreep.Probability >= 0.5 {
				notes = append(notes, "remove everything the request did not ask for")
			}
			if out.Underspecified.Probability >= 0.5 {
				notes = append(notes, "specify every component fully: fields, signatures, edge cases and error behaviour")
			}
			if out.MissingTests.Probability >= 0.5 {
				notes = append(notes, "add a test case for every requirement in the request")
			}
			if verdict == "ready" && len(notes) > 0 {
				verdict = "revise"
			}
			res.Model = systemonex.ResponseInfo(resp).Model
			res.Rounds = append(res.Rounds, DesignRound{
				Round: round, Verdict: verdict, Ready: out.Verdict.Probabilities["ready"],
				RequestClear: out.RequestClear.Probability, ScopeCreep: out.ScopeCreep.Probability,
				Underspecified: out.Underspecified.Probability, MissingTests: out.MissingTests.Probability,
				Risk: out.Risk.Label(), Feedback: notes,
			})
			res.Verdict = verdict
			if verdict != "revise" {
				break
			}
			if len(notes) == 0 {
				notes = append(notes, "tighten the approach: exact signatures, edge cases, error behaviour, and one test per requirement")
			}
			feedback = fmt.Sprintf("The reviewer rated the design %q (probability of ready %.2f). Do the following:\n- %s",
				verdict, out.Verdict.Probabilities["ready"], strings.Join(notes, "\n- "))
		}
		return res, nil
	})

	f.Plan = genkit.DefineFlow(g, "planImplementation", func(ctx context.Context, d Design) (Plan, error) {
		p, _, err := genkit.GenerateData[Plan](ctx, g,
			ai.WithModelName(chatModel),
			ai.WithSystem("Turn the design into an implementation plan for one Go module. "+
				"List every file to create with a path relative to the module root, including go.mod and the _test.go files, "+
				"then the ordered steps. Standard library only."),
			ai.WithPrompt("Design:\n%s", asJSON(d)))
		if err != nil {
			return Plan{}, err
		}
		if p == nil {
			return Plan{}, fmt.Errorf("plan: the model returned no plan")
		}
		return *p, nil
	})

	codeRules := "Return every file in full with its path relative to the module root. go.mod declares the module path " +
		"and 'go 1.26' and no requires. Standard library only. Tests must be deterministic: inject time, never sleep. " +
		"Every file must compile with go vet. Keep comments short."

	f.Implement = genkit.DefineFlow(g, "implementPlan", func(ctx context.Context, in ImplementInput) (Code, error) {
		c, _, err := genkit.GenerateData[Code](ctx, g,
			ai.WithModelName(chatModel),
			ai.WithSystem("You write complete, compiling Go code for the plan. "+codeRules),
			ai.WithPrompt("Request:\n%s\n\nDesign:\n%s\n\nPlan:\n%s", in.Request, asJSON(in.Design), asJSON(in.Plan)))
		if err != nil {
			return Code{}, err
		}
		if c == nil {
			return Code{}, fmt.Errorf("implement: the model returned no code")
		}
		return *c, nil
	})

	// Review is the gate between "the code exists" and "the code ships". The
	// state is everything a human reviewer would open: the request, the plan,
	// the files, and what go vet and go test printed.
	f.Review = genkit.DefineFlow(g, "reviewChange", func(ctx context.Context, in ReviewInput) (Review, error) {
		out, resp, err := genkit.GenerateData[CodeReview](ctx, g,
			ai.WithModelName(decisionModel),
			ai.WithSystem("The state has the feature request, the implementation plan, every source file, "+
				"and the output and exit codes of go vet and go test. Exit code 0 means success."),
			ai.WithPromptParts(ai.NewDataPart(in)))
		if err != nil {
			return Review{}, err
		}
		if out == nil {
			return Review{}, fmt.Errorf("review: the model returned no answers")
		}
		r := Review{
			Verdict:         string(out.Verdict.Choice),
			Confidence:      out.Verdict.Confidence,
			Quality:         out.Quality.Label(),
			CoversRequest:   out.CoversRequest.Probability,
			TestsMeaningful: out.TestsMeaningful.Probability,
			BuildGreen:      out.BuildGreen.Probability,
			Model:           systemonex.ResponseInfo(resp).Model,
		}
		// The thresholds are ours. A red build never ships, whatever the
		// model's overall verdict.
		if in.VetExit != 0 {
			r.Reasons = append(r.Reasons, "go vet failed")
		}
		if in.TestExit != 0 {
			r.Reasons = append(r.Reasons, "go test failed")
		}
		if r.CoversRequest < 0.6 {
			r.Reasons = append(r.Reasons, "the implementation does not cover the request")
		}
		if r.TestsMeaningful < 0.5 {
			r.Reasons = append(r.Reasons, "the tests do not exercise the requested behaviour")
		}
		if out.Quality.Level() == 0 {
			r.Reasons = append(r.Reasons, "code quality is poor")
		}
		if r.Verdict == "ship" && len(r.Reasons) > 0 {
			r.Verdict = "rework"
		}
		return r, nil
	})

	f.Fix = genkit.DefineFlow(g, "fixChange", func(ctx context.Context, in FixInput) (Code, error) {
		c, _, err := genkit.GenerateData[Code](ctx, g,
			ai.WithModelName(chatModel),
			ai.WithSystem("You fix Go code after a failed review. Address every reason, make the tests pass, "+
				"and change nothing else. "+codeRules),
			ai.WithPrompt("Request:\n%s\n\nPlan:\n%s\n\nReview:\n%s\n\ngo vet output:\n%s\n\ngo test output:\n%s\n\nCurrent files:\n%s",
				in.Request, asJSON(in.Plan), asJSON(in.Review), in.VetLog, in.TestLog, asJSON(in.Files)))
		if err != nil {
			return Code{}, err
		}
		if c == nil {
			return Code{}, fmt.Errorf("fix: the model returned no code")
		}
		return *c, nil
	})

	f.PR = genkit.DefineFlow(g, "writePullRequest", func(ctx context.Context, in PRInput) (string, error) {
		return genkit.GenerateText(ctx, g,
			ai.WithModelName(chatModel),
			ai.WithSystem("Write a pull request description in Markdown: a one-line title as a level-1 heading, "+
				"then Summary, Changes (one bullet per file), Tests (quote the go test output), and Risk. Under 250 words."),
			ai.WithPrompt("Request:\n%s\n\nDesign:\n%s\n\nReview:\n%s\n\ngo test output:\n%s\n\nFiles:\n%s",
				in.Request, asJSON(in.Design), asJSON(in.Review), in.TestLog, asJSON(in.Files)))
	})

	f.Brief = genkit.DefineFlow(g, "escalationBrief", func(ctx context.Context, in BriefInput) (string, error) {
		return genkit.GenerateText(ctx, g,
			ai.WithModelName(chatModel),
			ai.WithSystem("Write a one-screen handoff brief for an engineering lead about work an automated factory stopped. "+
				"Markdown with three sections: What was asked, Why it stopped, What a human should decide. Under 150 words."),
			ai.WithPrompt("%s", asJSON(in)))
	})

	return f
}

// --- engine ------------------------------------------------------------------
//
// A func step in a pod runs in a fresh process, so Genkit is initialised
// lazily, inside the step, from the secret files senro delivered to that pod.

var (
	engineOnce sync.Once
	engine     *Flows
	engineErr  error
)

func flows(ctx senro.Ctx) (*Flows, error) {
	engineOnce.Do(func() {
		var plugins []api.Plugin
		gemini, err := readSecret(ctx, "Gemini")
		if err != nil {
			engineErr = err
			return
		}
		if gemini != "" {
			plugins = append(plugins, &googlegenai.GoogleAI{APIKey: gemini})
		}
		typesafe, err := readSecret(ctx, "TypeSafe")
		if err != nil {
			engineErr = err
			return
		}
		if typesafe != "" {
			ts := systemonex.TypeSafe()
			ts.APIKey = typesafe
			plugins = append(plugins, ts)
		}
		g := genkit.Init(ctx, genkit.WithPlugins(plugins...))
		engine = defineFlows(g)
	})
	return engine, engineErr
}

// readSecret returns the value of a secret this step declared, or "" when
// it did not. ctx.Secret is a file path, never the value.
func readSecret(ctx senro.Ctx, name string) (string, error) {
	path := ctx.Secret(name)
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading secret %s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func asJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
