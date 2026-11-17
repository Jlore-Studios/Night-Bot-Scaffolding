package bot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nightbot/internal/config"
	"nightbot/internal/github"
	"nightbot/internal/gitx"
	"nightbot/internal/job"
	"nightbot/internal/sched"
	"nightbot/internal/state"
)

// Token is the bot's GitHub token, which deliver pushes with.
var Token = func() string {
	if t := os.Getenv("NIGHTBOT_GITHUB_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GITHUB_TOKEN")
}

// Deliver reads what the model job left in out and acts on it: comments, labels, the push and
// the pull request. workStatus is the model job's conclusion as the workflow saw it.
func (b *Bot) Deliver(p *job.Plan, out, workStatus string) error {
	key := state.ClaimKey(p.Number, p.QueryID)
	defer b.Kick()
	defer b.release(key, p.Number)

	r := &job.Result{}
	if err := job.Read(filepath.Join(out, "result.json"), r); err != nil {
		r = &job.Result{Action: p.Action, Outcome: job.Failed,
			Error: fmt.Sprintf("the model job left no result (it ended %s)", orUnknown(workStatus))}
	}
	if r.Action != p.Action {
		r.Outcome, r.Error = job.Failed, fmt.Sprintf("the result is for %q, the plan for %q", r.Action, p.Action)
	}
	b.record(p, r)

	switch p.Action {
	case sched.Ask:
		return b.deliverAnswer(p, r)
	case sched.Triage:
		return b.deliverTriage(p, r)
	case sched.Review:
		return b.deliverAudit(p, r)
	}
	return b.deliverChange(p, r, out)
}

func orUnknown(s string) string {
	if s == "" {
		return "without saying how"
	}
	return s
}

// record books the run's minutes and history, and parks a provider that was refused.
func (b *Bot) record(p *job.Plan, r *job.Result) {
	now := b.now()
	_, err := b.Store.Update(fmt.Sprintf("record #%d", p.Number), func(s *state.State) error {
		s.Record(&state.Entry{At: now, Number: p.Number, Action: p.Action, Provider: p.Provider, Model: p.Model,
			Outcome: r.Outcome, Minutes: r.Minutes, RunURL: p.RunURL})
		if r.RateLimited {
			until := r.ResetAt
			if until.IsZero() || until.Before(now) {
				until = now.Add(time.Duration(b.Cfg.ParkMinutes) * time.Minute)
			}
			if s.Parked == nil {
				s.Parked = map[string]time.Time{}
			}
			s.Parked[p.Provider] = until
		}
		s.Prune(now)
		return nil
	})
	if err != nil {
		b.logf("record: %v", err)
	}
}

func (b *Bot) deliverAnswer(p *job.Plan, r *job.Result) error {
	if r.Outcome == job.RateLimited {
		return nil // the question stays queued for another provider
	}
	text := r.Answer
	if r.Outcome != job.Answered {
		text = "I could not answer this: " + firstNonEmpty(r.Error, "the model gave no answer") + "."
	}
	quote := "> " + strings.ReplaceAll(strings.TrimSpace(p.Request), "\n", "\n> ")
	b.say(p.Number, fmt.Sprintf("%s\n\n%s%s\n\n<sub>%s on %s (%s tier)</sub>", quote, b.mention(p.Asker), text,
		p.Model, p.Provider, p.Tier))
	_, err := b.Store.Update("answered "+p.QueryID, func(s *state.State) error {
		kept := s.Queries[:0]
		for _, q := range s.Queries {
			if q.ID != p.QueryID {
				kept = append(kept, q)
			}
		}
		s.Queries = kept
		return nil
	})
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (b *Bot) deliverTriage(p *job.Plan, r *job.Result) error {
	if r.Outcome != job.Triaged || r.Triage == nil {
		if r.Outcome != job.RateLimited {
			b.fail(p.Number, "the triage failed: "+firstNonEmpty(r.Error, "no rating"))
		}
		return nil
	}
	d := r.Triage.Difficulty
	if _, err := b.Store.Update(fmt.Sprintf("triaged #%d", p.Number), func(s *state.State) error {
		item := s.Item(p.Number)
		item.Triaged, item.Difficulty = true, d
		return nil
	}); err != nil {
		return err
	}
	if err := b.GH.AddLabels(p.Number, "difficulty:"+d); err != nil {
		b.logf("label: %v", err)
	}
	b.say(p.Number, fmt.Sprintf("Rated **%s** (so a %s model builds it): %s\n\n<sub>Triage by %s on %s. "+
		"Change the `difficulty:` label to overrule it.</sub>", d, config.Difficulties[d], r.Triage.Summary, p.Model, p.Provider))
	return nil
}

func (b *Bot) deliverChange(p *job.Plan, r *job.Result, out string) error {
	switch r.Outcome {
	case job.Failed:
		b.fail(p.Number, firstNonEmpty(r.Error, "no reason given")+b.runLink(p))
		return nil
	case job.NoChange:
		b.say(p.Number, "I made no change. My report:\n\n"+quoteBlock(r.Report)+b.runLink(p))
		label := "build"
		if p.Action == sched.Revise {
			label = "revise"
		}
		b.GH.RemoveLabel(p.Number, b.Cfg.Label(label))
		return b.clearNotes(p)
	}
	if r.Bundle == "" {
		if r.Outcome == job.RateLimited {
			return nil // nothing done yet; the item stays queued
		}
		b.fail(p.Number, "the model job left no change to deliver")
		return nil
	}
	if err := b.push(p, r, out); err != nil {
		var refused *refusal
		if errors.As(err, &refused) {
			b.fail(p.Number, refused.Error())
			return nil
		}
		return err
	}
	if r.Outcome == job.RateLimited {
		b.logf("pushed the work so far on %s; the item stays queued", p.Branch)
		return nil // the next run carries the branch on
	}

	issue, pr := p.Number, p.PR
	if p.Action == sched.Build {
		existing, err := b.GH.FindPR(p.Branch)
		if err != nil {
			return err
		}
		if existing != nil {
			pr = existing.Number
		} else {
			created, err := b.GH.CreatePR(p.Branch, p.Base, p.Title, b.prBody(p, r), true)
			if err != nil {
				return err
			}
			pr = created.Number
		}
		b.GH.AddLabels(pr, b.Cfg.Label("pr"))
		if p.Rated {
			b.GH.AddLabels(pr, "difficulty:"+p.Difficulty)
		}
		b.GH.RemoveLabel(issue, b.Cfg.Label("build"))
		b.say(issue, fmt.Sprintf("Opened #%d.%s", pr, b.runLink(p)))
	} else {
		issue = 0
		b.GH.RemoveLabel(pr, b.Cfg.Label("revise"))
		b.say(pr, "Revised:\n\n"+quoteBlock(r.Report)+"\n\n"+reviewText(r)+b.runLink(p))
	}
	_, err := b.Store.Update(fmt.Sprintf("delivered #%d", pr), func(s *state.State) error {
		mem := s.Item(pr)
		mem.Branch, mem.LastProvider, mem.Notes, mem.CIRun = p.Branch, p.Provider, nil, 0
		mem.Builders = append(mem.Builders, p.Provider+"/"+p.Model)
		if p.Rated {
			mem.Difficulty = p.Difficulty
		}
		if issue != 0 {
			mem.Issue = issue
			s.Item(issue).PR = pr
			s.Item(issue).Branch = p.Branch
		}
		return nil
	})
	if err != nil {
		return err
	}
	return b.ship(pr, p, r)
}

func (b *Bot) runLink(p *job.Plan) string {
	if p.RunURL == "" {
		return ""
	}
	return fmt.Sprintf("\n\n<sub>[The run](%s): %s on %s.</sub>", p.RunURL, p.Model, p.Provider)
}

func quoteBlock(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "> (no report)"
	}
	if len(text) > 6000 {
		text = text[:6000] + "…"
	}
	return "> " + strings.ReplaceAll(text, "\n", "\n> ")
}

func (b *Bot) clearNotes(p *job.Plan) error {
	_, err := b.Store.Update(fmt.Sprintf("notes of #%d", p.Number), func(s *state.State) error {
		if mem := s.Peek(p.Number); mem != nil {
			mem.Notes, mem.CIRun = nil, 0
		}
		return nil
	})
	return err
}

// repo is the deliver job's checkout, its git commands authenticated as the bot.
func (b *Bot) repo() *gitx.Repo {
	return &gitx.Repo{Dir: b.Workspace, Env: gitx.AuthEnv(os.Getenv("GITHUB_SERVER_URL"), Token())}
}

// refusal is a change deliver will not push.
type refusal struct{ why string }

func (r *refusal) Error() string { return r.why }

// push checks the bundle the model job left and pushes it to the item's branch: only a change
// that grows from the base, touches no forbidden path, and moves the branch forward.
func (b *Bot) push(p *job.Plan, r *job.Result, out string) error {
	repo := b.repo()
	bundle := filepath.Join(out, filepath.Base(r.Bundle))
	incoming := "refs/nightbot/incoming"
	if _, err := repo.Git("fetch", "-q", "origin", p.Base); err != nil {
		return err
	}
	if _, err := repo.Git("bundle", "verify", "-q", bundle); err != nil {
		return &refusal{"the change's bundle does not verify against this repository: " + err.Error()}
	}
	if _, err := repo.Git("fetch", "-q", "--no-tags", bundle, "+refs/heads/"+p.Branch+":"+incoming); err != nil {
		return &refusal{"the bundle does not hold the branch " + p.Branch}
	}
	head, err := repo.Git("rev-parse", incoming)
	if err != nil {
		return err
	}
	base, err := repo.Git("merge-base", "origin/"+p.Base, incoming)
	if err != nil {
		return &refusal{"the change shares no history with " + p.Base}
	}
	changed, err := repo.Changed(base, incoming)
	if err != nil {
		return err
	}
	var forbidden []string
	for _, path := range changed {
		if config.MatchAny(b.Cfg.ForbiddenPaths, path) {
			forbidden = append(forbidden, path)
		}
	}
	if len(forbidden) > 0 {
		return &refusal{"the change touches forbidden paths, so I pushed nothing: " + strings.Join(forbidden, ", ")}
	}
	lease := "--force-with-lease=refs/heads/" + p.Branch + ":"
	if remote, err := repo.Git("ls-remote", "origin", "refs/heads/"+p.Branch); err == nil && remote != "" {
		sha := strings.Fields(remote)[0]
		if _, err := repo.Git("fetch", "-q", "origin", sha); err != nil {
			return err
		}
		if _, err := repo.Git("merge-base", "--is-ancestor", sha, incoming); err != nil {
			return &refusal{fmt.Sprintf("%s moved while the model worked (it is at %.12s now), so I pushed nothing; it will be worked again", p.Branch, sha)}
		}
		lease += sha
	}
	if os.Getenv("NIGHTBOT_DRY_RUN") != "" {
		b.logf("dry run: push %.12s to %s", head, p.Branch)
		return nil
	}
	if _, err := repo.Git("push", "-q", lease, "origin", incoming+":refs/heads/"+p.Branch); err != nil {
		return err
	}
	b.logf("pushed %.12s to %s", head, p.Branch)
	return nil
}

// ship decides what happens to an approved or unapproved pull request.
func (b *Bot) ship(pr int, p *job.Plan, r *job.Result) error {
	current, err := b.GH.GetPR(pr)
	if err != nil {
		return err
	}
	if !r.Approved {
		audits := 0
		b.Store.Update(fmt.Sprintf("audit #%d", pr), func(s *state.State) error {
			s.Item(pr).Audits++
			audits = s.Item(pr).Audits
			return nil
		})
		if audits <= b.Cfg.MaxAudits {
			b.GH.AddLabels(pr, b.Cfg.Label("review"))
			b.say(pr, fmt.Sprintf("The review rounds ran out without an approval, so another review run is queued "+
				"(on another provider when one is free; audit %d of %d).\n\n%s", audits, b.Cfg.MaxAudits, reviewText(r)))
			return nil
		}
		b.GH.AddLabels(pr, "human")
		b.say(pr, fmt.Sprintf("%sNo review approved this after %d audits, so it is yours now (`human`).\n\n%s",
			b.operator(), b.Cfg.MaxAudits, reviewText(r)))
		return nil
	}
	if err := b.GH.MarkReady(current); err != nil {
		b.logf("ready #%d: %v", pr, err)
	}
	b.GH.RemoveLabel(pr, b.Cfg.Label("review"))
	var touched []string
	for _, path := range r.Changed {
		if config.MatchAny(b.Cfg.ReviewPaths, path) {
			touched = append(touched, path)
		}
	}
	if len(touched) == 0 && p.Action == sched.Review {
		touched = b.reviewPathsOf(current)
	}
	approval := fmt.Sprintf("Approved by an adversarial review (%s on %s, %s tier).", p.Model, p.Provider, p.Tier)
	switch {
	case len(touched) > 0:
		b.GH.AddLabels(pr, "ready for merge")
		b.say(pr, fmt.Sprintf("%s%s It touches review-only paths (%s), so a person merges it.", b.operator(), approval,
			strings.Join(touched, ", ")))
	case !b.Cfg.AutoMerge:
		b.GH.AddLabels(pr, "ready for merge")
		b.say(pr, approval+" Auto-merge is off (`auto_merge`), so a person merges it.")
	default:
		missing, err := b.unprotected()
		if err != nil || len(missing) > 0 {
			why := fmt.Sprint(err)
			if err == nil {
				why = fmt.Sprintf("%s does not require %s", b.Cfg.DefaultBranch, strings.Join(missing, ", "))
			}
			b.GH.AddLabels(pr, "ready for merge")
			b.say(pr, fmt.Sprintf("%s%s I left auto-merge off: %s.", b.operator(), approval, why))
			return nil
		}
		if err := b.GH.EnableAutoMerge(current, b.Cfg.MergeMethod); err != nil {
			b.GH.AddLabels(pr, "ready for merge")
			b.say(pr, fmt.Sprintf("%s%s Auto-merge would not turn on: %v", b.operator(), approval, err))
			return nil
		}
		b.say(pr, approval+" Auto-merge is on: it merges once the required checks pass.")
	}
	return nil
}

// reviewPathsOf is the review-only paths a pull request touches, for an audit, which has no diff
// of its own: read from the branch against the base in the workspace.
func (b *Bot) reviewPathsOf(pr *github.PR) []string {
	repo := b.repo()
	baseRef, headRef := "refs/remotes/origin/"+pr.Base.Ref, "refs/remotes/origin/"+pr.Head.Ref
	if _, err := repo.Git("fetch", "-q", "origin", "+refs/heads/"+pr.Base.Ref+":"+baseRef,
		"+refs/heads/"+pr.Head.Ref+":"+headRef); err != nil {
		return []string{"(could not read the change: " + err.Error() + ")"}
	}
	base, err := repo.Git("merge-base", baseRef, headRef)
	if err != nil {
		base = baseRef
	}
	changed, _ := repo.Changed(base, headRef)
	var touched []string
	for _, path := range changed {
		if config.MatchAny(b.Cfg.ReviewPaths, path) {
			touched = append(touched, path)
		}
	}
	return touched
}

// unprotected is the required_checks the default branch does not require, or an error when its
// protection cannot be read. Auto-merge merges at once on a branch that requires nothing.
func (b *Bot) unprotected() ([]string, error) {
	required, err := b.GH.RequiredChecks(b.Cfg.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("its branch protection could not be read: %w", err)
	}
	if len(required) == 0 {
		return nil, fmt.Errorf("%s requires no checks, so auto-merge would merge before CI", b.Cfg.DefaultBranch)
	}
	have := map[string]bool{}
	for _, name := range required {
		have[name] = true
	}
	var missing []string
	for _, name := range b.Cfg.RequiredChecks {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

func (b *Bot) deliverAudit(p *job.Plan, r *job.Result) error {
	switch r.Outcome {
	case job.RateLimited:
		return nil
	case job.Failed:
		b.fail(p.Number, firstNonEmpty(r.Error, "the review failed")+b.runLink(p))
		return nil
	}
	if err := b.GH.CreateReview(p.Number, Marker+"\n"+reviewText(r)+b.runLink(p)); err != nil {
		b.logf("review #%d: %v", p.Number, err)
	}
	b.GH.RemoveLabel(p.Number, b.Cfg.Label("review"))
	pr, err := b.GH.GetPR(p.Number)
	if err != nil {
		return err
	}
	if !hasLabel(pr.Labels, b.Cfg.Label("pr")) {
		return nil // a person's pull request: the review is all the bot does
	}
	if r.Approved {
		return b.ship(p.Number, p, r)
	}
	_, err = b.Store.Update(fmt.Sprintf("audit of #%d", p.Number), func(s *state.State) error {
		mem := s.Item(p.Number)
		mem.Notes = append(mem.Notes, "An audit by "+p.Model+" on "+p.Provider+" found:\n\n"+reviewText(r))
		return nil
	})
	if err != nil {
		return err
	}
	return b.GH.AddLabels(p.Number, b.Cfg.Label("revise"))
}

func hasLabel(labels []github.Label, name string) bool {
	for _, l := range labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// reviewText is a review's verdict and findings, and the checks, as Markdown.
func reviewText(r *job.Result) string {
	var b strings.Builder
	if v := r.Verdict; v != nil {
		verdict := "changes requested"
		if v.Approves() {
			verdict = "approved"
		}
		fmt.Fprintf(&b, "**Review (round %d): %s.** %s\n", r.Rounds, verdict, v.Summary)
		if list := findingsList(v); list != "" {
			b.WriteString("\n" + list)
		}
	}
	if len(r.Checks) > 0 {
		b.WriteString("\n| Check | Result |\n|---|---|\n")
		for _, c := range r.Checks {
			mark := "✅ passed"
			if !c.Passed {
				mark = "❌ failed"
			}
			fmt.Fprintf(&b, "| %s | %s |\n", c.Name, mark)
		}
	}
	if len(r.Reverted) > 0 {
		fmt.Fprintf(&b, "\nPut back (forbidden paths): %s\n", strings.Join(r.Reverted, ", "))
	}
	return b.String()
}

func findingsList(v *job.Verdict) string {
	if v == nil {
		return ""
	}
	var b strings.Builder
	for _, f := range v.Findings {
		where := ""
		if f.File != "" {
			where = " `" + f.File
			if f.Line > 0 {
				where += fmt.Sprintf(":%d", f.Line)
			}
			where += "`"
		}
		fmt.Fprintf(&b, "- **%s**%s: %s", f.Severity, where, f.Problem)
		if f.Fix != "" {
			b.WriteString(" _Fix:_ " + f.Fix)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (b *Bot) prBody(p *job.Plan, r *job.Result) string {
	return fmt.Sprintf("%s\nCloses #%d.\n\n## What the builder says it did\n\n%s\n\n## Review\n\n%s\n\n"+
		"<sub>Built by %s on %s (%s tier) from [this run](%s). Comment `%s revise <what to change>` or "+
		"request changes to have it revised.</sub>", Marker, p.Number, quoteBlock(r.Report), reviewText(r),
		p.Model, p.Provider, p.Tier, p.RunURL, b.Cfg.CommandPrefix)
}
