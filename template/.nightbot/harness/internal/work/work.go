// Package work is the model job: it holds one model login and a read-only GitHub token, writes
// nothing to GitHub, and leaves a result file and a git bundle for deliver to check and push.
//
// A build: the builder works on the item's branch, the repository's checks run (the builder
// fixes red ones), then a fresh session of the same model reviews the change adversarially; its
// findings are fixed and it reviews again, until it approves or the rounds run out.
package work

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"nightbot/internal/config"
	"nightbot/internal/gitx"
	"nightbot/internal/job"
	"nightbot/internal/model"
	"nightbot/internal/prompts"
)

// CILogs reads a failed CI run's logs; the model job's read-only token is enough.
type CILogs interface {
	FailedJobs(runID int64) ([]*JobRef, error)
	JobLog(jobID int64) (string, error)
}

// JobRef is a failed CI job.
type JobRef struct {
	ID   int64
	Name string
}

// Worker does one plan's model work.
type Worker struct {
	Cfg      *config.Config
	Plan     *job.Plan
	Out      string
	Repo     *gitx.Repo
	Runner   *model.Runner
	CI       CILogs
	Deadline time.Time
	Log      io.Writer
	Now      func() time.Time
	// RunCheck runs one check command; a seam for the tests.
	RunCheck func(ctx context.Context, command, dir string, env []string) (string, error)

	result *job.Result
	system string
}

// minCallMinutes is the least time left for a model call to be worth starting.
const minCallMinutes = 15

// Do runs the plan and writes result.json (and change.bundle when there is a change) into Out.
func (w *Worker) Do() *job.Result {
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.Log == nil {
		w.Log = io.Discard
	}
	w.result = &job.Result{Action: w.Plan.Action, Branch: w.Plan.Branch}
	defer func() {
		if p := recover(); p != nil {
			w.result.Outcome, w.result.Error = job.Failed, fmt.Sprint(p)
		}
		w.redact()
		if err := job.Write(filepath.Join(w.Out, "result.json"), w.result); err != nil {
			fmt.Fprintln(w.Log, "result.json:", err)
		}
	}()
	system, err := prompts.Render(w.Cfg, "system", w.vars(nil))
	if err != nil {
		panic(err)
	}
	w.system = system
	w.Repo.MustGit("config", "user.name", w.Cfg.BotLogin)
	w.Repo.MustGit("config", "user.email", noreply(w.Cfg.BotLogin))
	switch w.Plan.Action {
	case "build":
		w.build()
	case "revise":
		w.revise()
	case "review":
		w.audit()
	case "ask":
		w.ask()
	case "triage":
		w.triage()
	default:
		panic("unknown action " + w.Plan.Action)
	}
	return w.result
}

func noreply(login string) string {
	login = strings.TrimSuffix(login, "[bot]")
	return login + "@users.noreply.github.com"
}

// vars are the template variables every prompt has, plus extra.
func (w *Worker) vars(extra map[string]string) map[string]string {
	p := w.Plan
	var checks []string
	for _, c := range w.Cfg.Checks {
		checks = append(checks, fmt.Sprintf("- %s: `%s`", c.Name, c.Run))
	}
	if len(checks) == 0 {
		checks = append(checks, "- (none configured: run what the repository's README says)")
	}
	v := map[string]string{
		"number": fmt.Sprint(p.Number), "title": p.Title, "thread": p.Thread, "branch": p.Branch,
		"base": p.Base, "difficulty": p.Difficulty, "model": p.Model, "provider": p.Provider,
		"checks_list": strings.Join(checks, "\n"), "kind": map[bool]string{true: "pull request", false: "issue"}[p.IsPR],
		"forbidden_paths": strings.Join(w.Cfg.ForbiddenPaths, ", "),
	}
	for k, val := range extra {
		v[k] = val
	}
	return v
}

// call runs one model call and books its time; it reports false when the run must stop.
func (w *Worker) call(kind string, write bool, prompt string) (*model.Outcome, bool) {
	left := w.Deadline.Sub(w.Now())
	if left < minCallMinutes*time.Minute {
		w.note(fmt.Sprintf("Stopped before a %s call: %.0f minutes of the job were left.", kind, left.Minutes()))
		return nil, false
	}
	timeout := time.Duration(w.Cfg.CallTimeoutMinutes) * time.Minute
	if left-5*time.Minute < timeout {
		timeout = left - 5*time.Minute
	}
	head := w.Repo.MustGit("rev-parse", "HEAD")
	fmt.Fprintf(w.Log, "nightbot: %s call on %s (%s), up to %s\n", kind, w.Plan.Model, w.Plan.Provider, timeout)
	o := w.Runner.Run(model.Call{Kind: kind, Write: write, Model: w.Plan.Model, Effort: w.Plan.Effort,
		Prompt: prompt, System: w.system, Cwd: w.Repo.Dir, Turns: w.Cfg.MaxTurns[kind], Timeout: timeout})
	w.result.Minutes += o.Duration.Minutes()
	w.result.Calls = append(w.result.Calls, job.Call{Kind: kind, Model: w.Plan.Model,
		Minutes: round(o.Duration.Minutes()), ExitCode: o.ExitCode, Error: o.Err})
	w.backOnBranch(head)
	if !write {
		w.discard(head)
	}
	if o.RateLimited {
		w.result.RateLimited, w.result.ResetAt = true, o.ResetAt
		return o, false
	}
	if o.Err != "" {
		fmt.Fprintf(w.Log, "nightbot: the %s call failed: %s\n%s\n", kind, o.Err, prompts.Tail(o.Output, 4000))
	}
	return o, true
}

func round(x float64) float64 { return float64(int(x*10+0.5)) / 10 }

// backOnBranch puts the checkout back on the plan's branch if the model left it.
func (w *Worker) backOnBranch(head string) {
	current, _ := w.Repo.Git("symbolic-ref", "--quiet", "--short", "HEAD")
	if current != w.Plan.Branch {
		fmt.Fprintf(w.Log, "nightbot: the model left the branch (on %q); back to %s\n", current, w.Plan.Branch)
		w.Repo.MustGit("checkout", "-q", "-B", w.Plan.Branch)
	}
}

// discard throws away whatever a read-only call changed or committed.
func (w *Worker) discard(head string) {
	w.Repo.MustGit("reset", "-q", "--hard", head)
	w.Repo.MustGit("clean", "-q", "-fd")
}

// commit folds everything since head (the model's own commits too) into one commit, after
// putting back any forbidden path. It reports whether there was anything to commit.
func (w *Worker) commit(head, message string) bool {
	w.Repo.MustGit("reset", "-q", "--soft", head)
	dirty, err := w.Repo.Dirty()
	if err != nil {
		panic(err)
	}
	for _, path := range dirty {
		if !config.MatchAny(w.Cfg.ForbiddenPaths, path) {
			continue
		}
		if _, err := w.Repo.Git("cat-file", "-e", head+":"+path); err == nil {
			w.Repo.MustGit("checkout", head, "--", path)
		} else {
			w.Repo.MustGit("rm", "-q", "-f", "--cached", "--ignore-unmatch", "--", path)
			os.Remove(filepath.Join(w.Repo.Dir, path))
		}
		w.result.Reverted = appendNew(w.result.Reverted, path)
	}
	w.Repo.MustGit("add", "-A")
	if _, err := w.Repo.Git("diff", "--cached", "--quiet"); err == nil {
		return false
	}
	w.Repo.MustGit("commit", "-q", "--no-verify", "-m", message)
	return true
}

func appendNew(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func (w *Worker) note(text string) {
	if w.result.Report != "" {
		w.result.Report += "\n\n"
	}
	w.result.Report += "_" + text + "_"
}

// checkout puts the item's branch at HEAD and reports the base the change is measured from.
func (w *Worker) checkout(existing bool) string {
	p := w.Plan
	w.Repo.MustGit("fetch", "-q", "origin", p.Base)
	base := "origin/" + p.Base
	if existing {
		w.Repo.MustGit("fetch", "-q", "origin", "+refs/heads/"+p.Branch+":refs/remotes/origin/"+p.Branch)
		w.Repo.MustGit("checkout", "-q", "-B", p.Branch, "origin/"+p.Branch)
		sha := w.Repo.MustGit("merge-base", base, "HEAD")
		w.result.Base = sha
		return sha
	}
	w.Repo.MustGit("checkout", "-q", "-B", p.Branch, base)
	sha := w.Repo.MustGit("rev-parse", base)
	w.result.Base = sha
	return sha
}

func (w *Worker) build() {
	base := w.checkout(w.Plan.Continue)
	carry := ""
	if w.Plan.Continue {
		carry = "The branch already holds earlier work on this item (an earlier run was cut off). Read `git log " +
			base[:12] + "..HEAD` and carry it on rather than starting over."
	}
	prompt, err := prompts.Render(w.Cfg, "build", w.vars(map[string]string{"continue": carry}))
	if err != nil {
		panic(err)
	}
	head := w.Repo.MustGit("rev-parse", "HEAD")
	o, ok := w.call("build", true, prompt)
	if o != nil {
		w.result.Report = o.Answer
	}
	w.commit(head, fmt.Sprintf("%s (#%d)", w.Plan.Title, w.Plan.Number))
	if !w.hasChange(base) {
		switch {
		case w.result.RateLimited:
			w.finish(base, job.RateLimited)
		case o == nil:
			w.finish(base, job.Failed)
			w.result.Error = "the job's time ran out before the build started"
		case o.Err != "":
			w.finish(base, job.Failed)
			w.result.Error = o.Err
		default:
			w.finish(base, job.NoChange)
		}
		return
	}
	if ok {
		ok = w.checksAndFix(base)
	}
	if ok {
		w.reviewLoop(base)
	}
	w.finish(base, "")
}

func (w *Worker) revise() {
	base := w.checkout(true)
	request := w.Plan.Request
	if logs := w.ciLogs(); logs != "" {
		request += "\n\n## The failing CI jobs (the end of each log)\n\n" + logs
	}
	prompt, err := prompts.Render(w.Cfg, "fix", w.vars(map[string]string{
		"request": prompts.Fence("requests", request), "checks": "(not run yet for this revision)",
		"findings": "(none: this revision answers the request above)"}))
	if err != nil {
		panic(err)
	}
	head := w.Repo.MustGit("rev-parse", "HEAD")
	o, ok := w.call("fix", true, prompt)
	if o != nil {
		w.result.Report = o.Answer
	}
	w.commit(head, fmt.Sprintf("Revise #%d", w.Plan.Number))
	if ok {
		ok = w.checksAndFix(base)
	}
	if ok {
		w.reviewLoop(base)
	}
	w.finish(base, "")
}

// audit reviews a pull request as it stands; it changes nothing.
func (w *Worker) audit() {
	p := w.Plan
	w.Repo.MustGit("fetch", "-q", "origin", p.Base,
		fmt.Sprintf("+refs/pull/%d/head:refs/remotes/origin/pr-%d", p.Number, p.Number))
	w.Repo.MustGit("checkout", "-q", "-B", p.Branch, fmt.Sprintf("origin/pr-%d", p.Number))
	base := w.Repo.MustGit("merge-base", "origin/"+p.Base, "HEAD")
	w.result.Base = base
	w.result.Checks = w.runChecks()
	w.result.ChecksGreen = allGreen(w.result.Checks)
	v, ok := w.review(base, 1, 1, nil)
	if !ok {
		w.result.Outcome = job.RateLimited
		if !w.result.RateLimited {
			w.result.Outcome, w.result.Error = job.Failed, "the job's time ran out before the review"
		}
		return
	}
	w.result.Verdict = v
	w.result.Rounds = 1
	w.result.Approved = v.Approves() && w.result.ChecksGreen
	w.result.Outcome = job.Reviewed
	w.result.Head = w.Repo.MustGit("rev-parse", "HEAD")
}

func (w *Worker) ask() {
	if w.Plan.IsPR {
		w.Repo.MustGit("fetch", "-q", "origin", fmt.Sprintf("+refs/pull/%d/head:refs/remotes/origin/pr-%d", w.Plan.Number, w.Plan.Number))
		w.Repo.MustGit("checkout", "-q", "-B", w.Plan.Branch, fmt.Sprintf("origin/pr-%d", w.Plan.Number))
	} else {
		w.Repo.MustGit("checkout", "-q", "-B", w.Plan.Branch, "origin/"+w.Plan.Base)
	}
	prompt, err := prompts.Render(w.Cfg, "ask", w.vars(map[string]string{
		"question": prompts.Fence("question by @"+w.Plan.Asker, w.Plan.Request)}))
	if err != nil {
		panic(err)
	}
	o, ok := w.call("ask", false, prompt)
	switch {
	case !ok && w.result.RateLimited:
		w.result.Outcome = job.RateLimited
	case o == nil || o.Err != "" || o.Answer == "":
		w.result.Outcome = job.Failed
		if o != nil {
			w.result.Error = o.Err
		}
	default:
		w.result.Outcome, w.result.Answer = job.Answered, o.Answer
	}
}

func (w *Worker) triage() {
	w.Repo.MustGit("checkout", "-q", "-B", w.Plan.Branch, "origin/"+w.Plan.Base)
	prompt, err := prompts.Render(w.Cfg, "triage", w.vars(nil))
	if err != nil {
		panic(err)
	}
	o, ok := w.call("triage", false, prompt)
	t := &job.Triage{}
	switch {
	case !ok && w.result.RateLimited:
		w.result.Outcome = job.RateLimited
	case o == nil || !job.LastJSON(o.Answer, t) || (t.Difficulty != "medium" && t.Difficulty != "hard"):
		// An unreadable rating rates nothing: the item builds as medium, as unrated items do.
		w.result.Outcome, w.result.Triage = job.Triaged, &job.Triage{Difficulty: "medium",
			Summary: "The triage gave no readable rating, so this builds as medium."}
	default:
		w.result.Outcome, w.result.Triage = job.Triaged, t
	}
}

func (w *Worker) hasChange(base string) bool {
	out, _ := w.Repo.Git("rev-list", "--count", base+"..HEAD")
	return out != "" && out != "0"
}

// checksAndFix runs the checks and has the builder fix red ones, up to fix_passes times. It
// reports false when the run must stop.
func (w *Worker) checksAndFix(base string) bool {
	w.result.Checks = w.runChecks()
	w.result.ChecksGreen = allGreen(w.result.Checks)
	for pass := 1; !w.result.ChecksGreen && pass <= w.Cfg.FixPasses; pass++ {
		prompt, err := prompts.Render(w.Cfg, "fix", w.vars(map[string]string{
			"request":  "Make the failing checks below pass without weakening them.",
			"checks":   prompts.Fence("checks", checksText(w.result.Checks)),
			"findings": "(none: only the checks)"}))
		if err != nil {
			panic(err)
		}
		head := w.Repo.MustGit("rev-parse", "HEAD")
		o, ok := w.call("fix", true, prompt)
		w.commit(head, fmt.Sprintf("Fix the checks (#%d)", w.Plan.Number))
		if o != nil && o.Answer != "" {
			w.result.Report += "\n\n**Check fix " + fmt.Sprint(pass) + ":** " + o.Answer
		}
		if !ok {
			return false
		}
		w.result.Checks = w.runChecks()
		w.result.ChecksGreen = allGreen(w.result.Checks)
	}
	return true
}

// reviewLoop reviews the change in a fresh session and fixes what it finds until it approves
// or the rounds run out.
func (w *Worker) reviewLoop(base string) {
	rounds := w.Plan.ReviewRounds
	var previous []job.Finding
	for round := 1; round <= rounds; round++ {
		v, ok := w.review(base, round, rounds, previous)
		if !ok {
			return
		}
		w.result.Verdict, w.result.Rounds = v, round
		if v.Approves() && w.result.ChecksGreen {
			w.result.Approved = true
			return
		}
		if round == rounds {
			return
		}
		findings := v.Blocking()
		if v.Approves() { // approved, but the checks are red
			findings = append(findings, job.Finding{Severity: "blocking", Problem: "The repository's checks fail."})
		}
		previous = findings
		prompt, err := prompts.Render(w.Cfg, "fix", w.vars(map[string]string{
			"request":  "Address every blocking finding of the review below.",
			"findings": prompts.Fence("review", findingsText(findings)),
			"checks":   prompts.Fence("checks", checksText(w.result.Checks))}))
		if err != nil {
			panic(err)
		}
		head := w.Repo.MustGit("rev-parse", "HEAD")
		o, ok := w.call("fix", true, prompt)
		w.commit(head, fmt.Sprintf("Address review round %d (#%d)", round, w.Plan.Number))
		if o != nil && o.Answer != "" {
			w.result.Report += fmt.Sprintf("\n\n**After review %d:** %s", round, o.Answer)
		}
		if !ok || !w.checksAndFix(base) {
			return
		}
	}
}

// review is one adversarial review of base..HEAD in a fresh, read-only session.
func (w *Worker) review(base string, round, rounds int, previous []job.Finding) (*job.Verdict, bool) {
	stat, _ := w.Repo.Git("diff", "--stat", base+"..HEAD")
	diff, _ := w.Repo.Git("diff", base+"..HEAD")
	earlier := "(none: this is the first review)"
	if len(previous) > 0 {
		earlier = prompts.Fence("earlier review", findingsText(previous))
	}
	prompt, err := prompts.Render(w.Cfg, "review", w.vars(map[string]string{
		"round": fmt.Sprint(round), "rounds": fmt.Sprint(rounds), "base_sha": base,
		"report":   prompts.Fence("the builder's report", w.result.Report),
		"checks":   prompts.Fence("checks", checksText(w.result.Checks)),
		"previous": earlier,
		"diff":     "```diff\n" + stat + "\n\n" + prompts.Truncate(diff, w.Cfg.MaxDiffChars) + "\n```"}))
	if err != nil {
		panic(err)
	}
	o, ok := w.call("review", false, prompt)
	if !ok {
		return nil, false
	}
	return job.ParseVerdict(o.Answer), true
}

func (w *Worker) finish(base, outcome string) {
	r := w.result
	r.Head = w.Repo.MustGit("rev-parse", "HEAD")
	changed, _ := w.Repo.Changed(base, "HEAD")
	r.Changed = changed
	switch {
	case outcome != "":
		r.Outcome = outcome
	case r.RateLimited:
		r.Outcome = job.RateLimited
	case r.Approved:
		r.Outcome = job.Approved
	default:
		r.Outcome = job.Unapproved
	}
	if w.hasChange(base) && outcome != job.Failed {
		bundle := filepath.Join(w.Out, "change.bundle")
		if _, err := w.Repo.Git("bundle", "create", bundle, "refs/heads/"+w.Plan.Branch, "^"+base); err != nil {
			r.Outcome, r.Error = job.Failed, err.Error()
			return
		}
		r.Bundle = "change.bundle"
	}
}

// runChecks runs the repository's checks, each with its own timeout, in a clean environment.
func (w *Worker) runChecks() []job.CheckResult {
	run := w.RunCheck
	if run == nil {
		run = runCheck
	}
	env := cleanEnv(os.Environ())
	var results []job.CheckResult
	for _, check := range w.Cfg.Checks {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(check.TimeoutMinutes)*time.Minute)
		start := w.Now()
		out, err := run(ctx, check.Run, w.Repo.Dir, env)
		cancel()
		r := job.CheckResult{Name: check.Name, Passed: err == nil, Minutes: round(w.Now().Sub(start).Minutes())}
		if err != nil {
			r.Output = prompts.Tail(strings.TrimSpace(out)+"\n"+err.Error(), 6000)
		}
		fmt.Fprintf(w.Log, "nightbot: check %s: %v\n", check.Name, map[bool]string{true: "passed", false: "failed"}[r.Passed])
		results = append(results, r)
	}
	return results
}

var sensitive = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|PRIVATE_KEY|API_KEY|CREDENTIAL|^ACTIONS_|^NIGHTBOT_)`)

func cleanEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); !sensitive.MatchString(name) {
			out = append(out, kv)
		}
	}
	return out
}

func runCheck(ctx context.Context, command, dir string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out")
	}
	return out.String(), err
}

func allGreen(checks []job.CheckResult) bool {
	for _, c := range checks {
		if !c.Passed {
			return false
		}
	}
	return true
}

func checksText(checks []job.CheckResult) string {
	if len(checks) == 0 {
		return "No checks are configured."
	}
	var b strings.Builder
	for _, c := range checks {
		state := "passed"
		if !c.Passed {
			state = "FAILED"
		}
		fmt.Fprintf(&b, "### %s: %s\n", c.Name, state)
		if c.Output != "" {
			fmt.Fprintf(&b, "```\n%s\n```\n", c.Output)
		}
	}
	return b.String()
}

func findingsText(findings []job.Finding) string {
	var b strings.Builder
	for i, f := range findings {
		where := f.File
		if f.Line > 0 {
			where += fmt.Sprintf(":%d", f.Line)
		}
		fmt.Fprintf(&b, "%d. [%s] %s %s", i+1, f.Severity, where, f.Problem)
		if f.Fix != "" {
			fmt.Fprintf(&b, " Fix: %s", f.Fix)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (w *Worker) ciLogs() string {
	if w.Plan.CIRun == 0 || w.CI == nil {
		return ""
	}
	jobs, err := w.CI.FailedJobs(w.Plan.CIRun)
	if err != nil {
		return "(the CI logs could not be read: " + err.Error() + ")"
	}
	var b strings.Builder
	for i, j := range jobs {
		if i == 3 {
			fmt.Fprintf(&b, "(%d more failed jobs)\n", len(jobs)-3)
			break
		}
		log, err := w.CI.JobLog(j.ID)
		if err != nil {
			log = "(unreadable: " + err.Error() + ")"
		}
		fmt.Fprintf(&b, "### %s\n```\n%s\n```\n", j.Name, prompts.Tail(log, 6000))
	}
	return b.String()
}

// redact keeps the model's login out of everything result.json records.
func (w *Worker) redact() {
	secret := w.Runner.Secret
	if len(secret) < 8 {
		return
	}
	scrub := func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") }
	r := w.result
	r.Report, r.Answer, r.Error = scrub(r.Report), scrub(r.Answer), scrub(r.Error)
	for i := range r.Checks {
		r.Checks[i].Output = scrub(r.Checks[i].Output)
	}
	for i := range r.Calls {
		r.Calls[i].Error = scrub(r.Calls[i].Error)
	}
	if r.Verdict != nil {
		r.Verdict.Summary = scrub(r.Verdict.Summary)
		for i := range r.Verdict.Findings {
			r.Verdict.Findings[i].Problem = scrub(r.Verdict.Findings[i].Problem)
			r.Verdict.Findings[i].Fix = scrub(r.Verdict.Findings[i].Fix)
		}
	}
}
