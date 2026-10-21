package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/xavidop/senro"
)

// The "repo" workspace is the module the factory is building. Its own
// records live under .factory/ so they travel with the code.

const (
	workspace  = "factory-repo"
	factoryDir = ".factory"
)

func init() {
	senro.RegisterFunc("factory/clean", Clean)
	senro.RegisterFunc("factory/design", DesignStep)
	senro.RegisterFunc("factory/plan", PlanStep)
	senro.RegisterFunc("factory/implement", Implement)
	senro.RegisterFunc("factory/review", ReviewStep)
	senro.RegisterFunc("factory/fix", Fix)
	senro.RegisterFunc("factory/pr", PR)
	senro.RegisterFunc("factory/brief", Brief)
	senro.RegisterFunc("factory/collect", Collect)
	senro.RegisterFunc("factory/publish", Publish)
}

type NoParams struct{}

type CleanParams struct {
	RequestPath string `json:"request_path"`
}

type DirParams struct {
	Dir string `json:"dir"`
}

type BriefParams struct {
	Reason string `json:"reason"`
}

// Clean runs on the coordinator at the start of a request: it empties the
// persistent workspace and drops the request into it.
func Clean(ctx senro.Ctx, p CleanParams) error {
	root := wsPath(ctx, ".")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			return err
		}
	}
	raw, err := os.ReadFile(p.RequestPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(wsPath(ctx, factoryDir), 0o755); err != nil {
		return err
	}
	return writeFile(ctx, "request.md", string(raw))
}

// DesignStep: request in, design and the review rounds out. Gemini and jev.
func DesignStep(ctx senro.Ctx, _ NoParams) error {
	f, err := flows(ctx)
	if err != nil {
		return err
	}
	req, err := readRecord(ctx, "request.md")
	if err != nil {
		return err
	}
	res, err := f.Design.Run(ctx, req)
	if err != nil {
		return err
	}
	for _, r := range res.Rounds {
		fmt.Fprintf(ctx.Stdout(), "round %d: %s decided %q (p_ready %.2f) request_clear=%.2f scope_creep=%.2f underspecified=%.2f missing_tests=%.2f risk=%s\n",
			r.Round, res.Model, r.Verdict, r.Ready, r.RequestClear, r.ScopeCreep, r.Underspecified, r.MissingTests, r.Risk)
		for _, n := range r.Feedback {
			fmt.Fprintf(ctx.Stdout(), "  - %s\n", n)
		}
	}
	if err := writeJSON(ctx, "design.json", res); err != nil {
		return err
	}
	md := "# " + res.Design.Title + "\n\n" + res.Design.Summary + "\n\n## Approach\n\n" + res.Design.Approach +
		"\n\n## Components\n\n" + bullets(res.Design.Components) + "\n## Risks\n\n" + bullets(res.Design.Risks) +
		"\n## Test plan\n\n" + bullets(res.Design.TestPlan)
	return writeFile(ctx, "DESIGN.md", md)
}

// PlanStep: design in, plan out. Gemini.
func PlanStep(ctx senro.Ctx, _ NoParams) error {
	f, err := flows(ctx)
	if err != nil {
		return err
	}
	var res DesignResult
	if err := readJSON(ctx, "design.json", &res); err != nil {
		return err
	}
	if res.Verdict != "ready" {
		fmt.Fprintf(ctx.Stdout(), "design ended %q, nothing to plan\n", res.Verdict)
		return nil
	}
	plan, err := f.Plan.Run(ctx, res.Design)
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout(), "module %s, %d files, %d steps\n", plan.Module, len(plan.Files), len(plan.Steps))
	if err := writeJSON(ctx, "plan.json", plan); err != nil {
		return err
	}
	md := "# Plan for " + plan.Module + "\n\n## Files\n\n"
	for _, pf := range plan.Files {
		md += "- `" + pf.Path + "`: " + pf.Purpose + "\n"
	}
	md += "\n## Steps\n\n"
	for i, s := range plan.Steps {
		md += strconv.Itoa(i+1) + ". " + s + "\n"
	}
	return writeFile(ctx, "PLAN.md", md)
}

// Implement: request, design and plan in, source files out. Gemini.
func Implement(ctx senro.Ctx, _ NoParams) error {
	f, err := flows(ctx)
	if err != nil {
		return err
	}
	var in ImplementInput
	if in.Request, err = readRecord(ctx, "request.md"); err != nil {
		return err
	}
	var res DesignResult
	if err := readJSON(ctx, "design.json", &res); err != nil {
		return err
	}
	in.Design = res.Design
	if err := readJSON(ctx, "plan.json", &in.Plan); err != nil {
		return err
	}
	code, err := f.Implement.Run(ctx, in)
	if err != nil {
		return err
	}
	return writeCode(ctx, code)
}

// ReviewStep: everything a reviewer would open in, a verdict out. jev.
func ReviewStep(ctx senro.Ctx, _ NoParams) error {
	f, err := flows(ctx)
	if err != nil {
		return err
	}
	in, err := reviewInput(ctx)
	if err != nil {
		return err
	}
	r, err := f.Review.Run(ctx, in)
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout(), "%s decided %q (confidence %.2f) quality=%s covers_request=%.2f tests_meaningful=%.2f build_green=%.2f\n",
		r.Model, r.Verdict, r.Confidence, r.Quality, r.CoversRequest, r.TestsMeaningful, r.BuildGreen)
	for _, reason := range r.Reasons {
		fmt.Fprintf(ctx.Stdout(), "  - %s\n", reason)
	}
	return writeJSON(ctx, "review.json", r)
}

// Fix: the review and the logs in, corrected files out. Gemini.
func Fix(ctx senro.Ctx, _ NoParams) error {
	f, err := flows(ctx)
	if err != nil {
		return err
	}
	ri, err := reviewInput(ctx)
	if err != nil {
		return err
	}
	in := FixInput{Request: ri.Request, Plan: ri.Plan, Files: ri.Files, VetLog: ri.VetLog, TestLog: ri.TestLog}
	if err := readJSON(ctx, "review.json", &in.Review); err != nil {
		return err
	}
	code, err := f.Fix.Run(ctx, in)
	if err != nil {
		return err
	}
	return writeCode(ctx, code)
}

// PR: the whole story in, a pull request description out. Gemini.
func PR(ctx senro.Ctx, _ NoParams) error {
	f, err := flows(ctx)
	if err != nil {
		return err
	}
	ri, err := reviewInput(ctx)
	if err != nil {
		return err
	}
	in := PRInput{Request: ri.Request, Files: ri.Files, TestLog: ri.TestLog}
	var res DesignResult
	if err := readJSON(ctx, "design.json", &res); err != nil {
		return err
	}
	in.Design = res.Design
	if err := readJSON(ctx, "review.json", &in.Review); err != nil {
		return err
	}
	text, err := f.PR.Run(ctx, in)
	if err != nil {
		return err
	}
	fmt.Fprintln(ctx.Stdout(), text)
	return writeFile(ctx, "PR.md", text+"\n")
}

// Brief: why the factory stopped, for a human. Gemini.
func Brief(ctx senro.Ctx, p BriefParams) error {
	f, err := flows(ctx)
	if err != nil {
		return err
	}
	in := BriefInput{Reason: p.Reason}
	if in.Request, err = readRecord(ctx, "request.md"); err != nil {
		return err
	}
	var d DesignResult
	if err := readJSON(ctx, "design.json", &d); err == nil {
		in.Design = &d
	}
	var r Review
	if err := readJSON(ctx, "review.json", &r); err == nil {
		in.Review = &r
	}
	text, err := f.Brief.Run(ctx, in)
	if err != nil {
		return err
	}
	fmt.Fprintln(ctx.Stdout(), text)
	return writeFile(ctx, "BRIEF.md", text+"\n")
}

// Collect runs on the coordinator: it copies the factory's records out of the
// workspace so the coordinator can read the verdict and pick the next run.
func Collect(ctx senro.Ctx, p DirParams) error {
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(wsPath(ctx, factoryDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := os.ReadFile(wsPath(ctx, filepath.Join(factoryDir, e.Name())))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(p.Dir, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Publish runs on the coordinator: the shipped module lands under out/src,
// next to the records. A real factory would open the pull request here.
func Publish(ctx senro.Ctx, p DirParams) error {
	root := wsPath(ctx, ".")
	dst := filepath.Join(p.Dir, "src")
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == factoryDir {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n++
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout(), "published %d files to %s\n", n, dst)
	return Collect(ctx, p)
}

// --- helpers -----------------------------------------------------------------

func reviewInput(ctx senro.Ctx) (ReviewInput, error) {
	var in ReviewInput
	var err error
	if in.Request, err = readRecord(ctx, "request.md"); err != nil {
		return in, err
	}
	if err := readJSON(ctx, "plan.json", &in.Plan); err != nil {
		return in, err
	}
	if in.Files, err = sourceFiles(ctx); err != nil {
		return in, err
	}
	in.VetLog, in.VetExit = readLog(ctx, "vet")
	in.TestLog, in.TestExit = readLog(ctx, "test")
	return in, nil
}

// sourceFiles is every file in the workspace outside .factory, which is the
// whole module the factory is building.
func sourceFiles(ctx senro.Ctx) ([]File, error) {
	root := wsPath(ctx, ".")
	var files []File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == factoryDir {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, File{Path: rel, Content: string(b)})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

func writeCode(ctx senro.Ctx, code Code) error {
	root := wsPath(ctx, ".")
	for _, f := range code.Files {
		rel := filepath.Clean(f.Path)
		if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) || strings.HasPrefix(rel, factoryDir) {
			return fmt.Errorf("refusing to write outside the module: %q", f.Path)
		}
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(f.Content), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout(), "wrote %s (%d bytes)\n", rel, len(f.Content))
	}
	if code.Notes != "" {
		fmt.Fprintf(ctx.Stdout(), "notes: %s\n", code.Notes)
	}
	return nil
}

// readLog reads what a build or test command left behind: its output and
// its exit code, written by the command step itself.
func readLog(ctx senro.Ctx, name string) (string, int) {
	out, _ := os.ReadFile(wsPath(ctx, filepath.Join(factoryDir, name+".log")))
	code := -1
	if b, err := os.ReadFile(wsPath(ctx, filepath.Join(factoryDir, name+".exit"))); err == nil {
		code, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	return string(out), code
}

func wsPath(ctx senro.Ctx, name string) string {
	dir, ok := ctx.Workspace(workspace)
	if !ok {
		panic(fmt.Sprintf("step %s does not mount workspace %q", ctx.StepID(), workspace))
	}
	return filepath.Join(string(dir), name)
}

func readRecord(ctx senro.Ctx, name string) (string, error) {
	b, err := os.ReadFile(wsPath(ctx, filepath.Join(factoryDir, name)))
	return string(b), err
}

func readJSON(ctx senro.Ctx, name string, v any) error {
	b, err := os.ReadFile(wsPath(ctx, filepath.Join(factoryDir, name)))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(ctx senro.Ctx, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(ctx, name, string(b)+"\n")
}

func writeFile(ctx senro.Ctx, name, content string) error {
	if err := os.MkdirAll(wsPath(ctx, factoryDir), 0o755); err != nil {
		return err
	}
	return os.WriteFile(wsPath(ctx, filepath.Join(factoryDir, name)), []byte(content), 0o644)
}

func bullets(items []string) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString("- " + it + "\n")
	}
	return b.String()
}
