package main

import (
	"time"

	"github.com/xavidop/senro"
	"github.com/xavidop/senro/exec"
	"github.com/xavidop/senro/executor/k8s"
	"github.com/xavidop/senro/retry"
)

// One image for every step: the official Go image has sh and tar, which is
// all a func step needs, and a Go toolchain, which the build and test
// command steps need. senro requires the digest.
const stepImage = "docker.io/library/golang:1.27-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd"

func pods() senro.ExecutorTarget {
	return k8s.Pod(stepImage,
		k8s.Namespace("factory"),
		k8s.Resources(
			map[string]string{"cpu": "500m", "memory": "512Mi"},
			map[string]string{"cpu": "2", "memory": "2Gi"},
		))
}

const stepTimeout = 5 * time.Minute

// repo is the module under construction. It is persistent, so every run of
// one request works on the same tree, and bounded, as senro insists. A
// persistent workspace is machine-global and keyed by its name.
func repo() *senro.WorkspaceRef {
	return senro.Workspace(workspace,
		senro.Scope(senro.ScopePersistent),
		senro.MaxAge(24*time.Hour),
		senro.MaxSize(256<<20))
}

// aiStep is the shape every Gemini or jev step shares.
func aiStep(s *senro.StepBuilder, ws *senro.WorkspaceRef, secrets ...string) *senro.StepBuilder {
	s = s.Mount(ws.At("/repo", senro.RW)).Timeout(stepTimeout).Retry(3, retry.OnInfra())
	for _, name := range secrets {
		s = s.SecretEnv("SENRO_"+name, name)
	}
	return s
}

// goStep is a build or test command in the Go image. It records its output
// and exit code under .factory/ for the reviewer, and ContinueOnError lets
// the review run on a red build: jev gets to see why it failed.
func goStep(w *senro.WorkflowBuilder, id, after, cmd string, ws *senro.WorkspaceRef) *senro.StepBuilder {
	script := "mkdir -p .factory; " + cmd + " > .factory/" + id + ".log 2>&1; echo $? > .factory/" + id + ".exit; " +
		"cat .factory/" + id + ".log; exit $(cat .factory/" + id + ".exit)"
	return w.Step(id, exec.Command("sh", "-c", script)).
		Needs(after).
		Mount(ws.At("/repo", senro.RW)).
		WorkDir("/repo").
		Env("HOME", "/tmp").
		Env("GOCACHE", "/tmp/gocache").
		Env("GOPATH", "/tmp/gopath").
		Env("GOFLAGS", "-mod=mod").
		Env("GOPROXY", "off").
		Env("GOTOOLCHAIN", "local").
		Env("CGO_ENABLED", "0").
		Timeout(stepTimeout).
		Retry(3, retry.OnInfra()).
		ContinueOnError()
}

// designPipeline: clean the tree, design (with jev in the loop), plan.
func designPipeline(requestPath, outDir string) *senro.Pipeline {
	p := senro.New("design")
	ws := repo()

	intake := p.Workflow("intake", senro.On(senro.Local()))
	intake.Step("clean", senro.Func("factory/clean", CleanParams{RequestPath: requestPath})).
		Mount(ws.At("/repo", senro.RW))

	design := p.Workflow("design", senro.Needs("intake"), senro.On(pods()))
	aiStep(design.Step("design", senro.Func("factory/design", NoParams{})), ws, "Gemini", "TypeSafe")
	aiStep(design.Step("plan", senro.Func("factory/plan", NoParams{})), ws, "Gemini").Needs("design")

	handoff := p.Workflow("handoff", senro.Needs("design"), senro.On(senro.Local()))
	handoff.Step("collect", senro.Func("factory/collect", DirParams{Dir: outDir})).
		Mount(ws.At("/repo", senro.RO))

	return p
}

// buildPipeline: write (or fix) the code, vet it, test it, let jev review it.
func buildPipeline(outDir string, rework bool) *senro.Pipeline {
	name, fn := "implement", "factory/implement"
	if rework {
		name, fn = "rework", "factory/fix"
	}
	p := senro.New(name)
	ws := repo()

	code := p.Workflow("code", senro.On(pods()))
	aiStep(code.Step(name, senro.Func(fn, NoParams{})), ws, "Gemini")
	goStep(code, "vet", name, "go vet ./...", ws)
	goStep(code, "test", "vet", "go test -count=1 ./...", ws)
	aiStep(code.Step("review", senro.Func("factory/review", NoParams{})), ws, "TypeSafe").Needs("test")

	handoff := p.Workflow("handoff", senro.Needs("code"), senro.On(senro.Local()))
	handoff.Step("collect", senro.Func("factory/collect", DirParams{Dir: outDir})).
		Mount(ws.At("/repo", senro.RO))

	return p
}

// finishPipeline declares both endings; the verdict parameter picks one.
func finishPipeline(outDir, reason string) *senro.Pipeline {
	p := senro.New("finish")
	ws := repo()

	ship := p.Workflow("ship", senro.On(pods()), senro.When(senro.ParamIs("verdict", "ship")))
	aiStep(ship.Step("pr", senro.Func("factory/pr", NoParams{})), ws, "Gemini")

	publish := p.Workflow("publish", senro.Needs("ship"), senro.On(senro.Local()),
		senro.When(senro.ParamIs("verdict", "ship")))
	publish.Step("publish", senro.Func("factory/publish", DirParams{Dir: outDir})).
		Mount(ws.At("/repo", senro.RO))

	escalate := p.Workflow("escalate", senro.On(pods()), senro.When(senro.ParamIs("verdict", "human")))
	aiStep(escalate.Step("brief", senro.Func("factory/brief", BriefParams{Reason: reason})), ws, "Gemini")

	notify := p.Workflow("notify", senro.Needs("escalate"), senro.On(senro.Local()),
		senro.When(senro.ParamIs("verdict", "human")))
	notify.Step("notify", senro.Func("factory/collect", DirParams{Dir: outDir})).
		Mount(ws.At("/repo", senro.RO))

	return p
}
