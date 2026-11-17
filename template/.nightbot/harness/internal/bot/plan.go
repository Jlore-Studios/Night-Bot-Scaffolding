package bot

import (
	"fmt"
	"sort"
	"strings"

	"nightbot/internal/config"
	"nightbot/internal/github"
	"nightbot/internal/job"
	"nightbot/internal/prompts"
	"nightbot/internal/sched"
	"nightbot/internal/state"
)

// PlanOptions narrow what the planner may pick.
type PlanOptions struct {
	Item  int    // one issue or pull request (0: any)
	Mode  string // one action (auto: any)
	Force bool   // outside the providers' hours
}

// threadLimit is how much of an item's text goes to the model.
const threadLimit = 40000

// Plan picks the next item and provider, claims them and returns the plan, or nil and why not.
func (b *Bot) Plan(opts PlanOptions) (*job.Plan, string, error) {
	if b.HaltFile() {
		return nil, "halted: .nightbot/HALT is committed", nil
	}
	var plan *job.Plan
	var why string
	_, err := b.Store.Update("plan", func(st *state.State) error {
		plan, why = nil, ""
		st.Secrets = sortedKeys(b.Secrets)
		st.Prune(b.now())
		if st.Halted {
			why = "halted by " + st.HaltedBy
			return nil
		}
		snap, err := b.snapshotOf(st, opts.Force)
		if err != nil {
			return err
		}
		as := sched.Next(b.Cfg, st, snap.Queue, snap.Avail, opts.Item, opts.Mode)
		if as == nil {
			why = "nothing a free provider can take"
			for _, a := range snap.Assignments {
				if (opts.Item == 0 || a.Item.Number == opts.Item) && a.Waits != "" {
					why = fmt.Sprintf("#%d (%s) waits: %s", a.Item.Number, a.Item.Action, a.Waits)
					break
				}
			}
			if len(snap.Queue) == 0 {
				why = "the queue is empty"
			}
			return nil
		}
		plan = b.planFor(st, as)
		if st.Running == nil {
			st.Running = map[string]*state.Claim{}
		}
		st.Running[as.Item.Key()] = &state.Claim{Number: plan.Number, Action: plan.Action, Provider: plan.Provider,
			Model: plan.Model, RunID: b.RunID, Started: b.now(), QueryID: plan.QueryID}
		return nil
	})
	if err != nil || plan == nil {
		return nil, why, err
	}
	if err := b.fill(plan); err != nil {
		b.release(state.ClaimKey(plan.Number, plan.QueryID), plan.Number)
		return nil, "", err
	}
	if plan.Action != sched.Ask {
		if err := b.GH.AddLabels(plan.Number, b.Cfg.Label("building")); err != nil {
			b.logf("label #%d: %v", plan.Number, err)
		}
	}
	return plan, "", nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// planFor is the plan's skeleton from the assignment and the state; fill reads GitHub for the rest.
func (b *Bot) planFor(st *state.State, as *sched.Assignment) *job.Plan {
	it, prov, seat := as.Item, as.Provider, as.Seat
	p := &job.Plan{Action: it.Action, Number: it.Number, Title: it.Title, IsPR: it.IsPR,
		Difficulty: it.Difficulty, Rated: it.Rated, Provider: prov.ID, CLI: prov.CLI, Model: seat.Model,
		Effort: seat.Effort, Tier: seat.Tier, RunsOn: prov.RunsOn, Login: prov.Login, Base: b.Cfg.DefaultBranch,
		ReviewRounds: b.Cfg.ReviewRounds[it.Difficulty], RunID: b.RunID, RunURL: b.RunURL, Created: b.now()}
	if prov.Login == "secret" {
		p.Secret = prov.Secret
	}
	if p.ReviewRounds == 0 {
		p.ReviewRounds = b.Cfg.ReviewRounds["medium"]
	}
	if mem := st.Peek(it.Number); mem != nil {
		p.Issue = mem.Issue
		p.Branch = mem.Branch
		if it.Action == sched.Revise {
			p.Request = strings.Join(mem.Notes, "\n\n")
			p.CIRun = mem.CIRun
		}
	}
	switch it.Action {
	case sched.Ask:
		p.QueryID, p.CommentID, p.Asker, p.Request = it.Query.ID, it.Query.CommentID, it.Query.Author, it.Query.Text
		p.Branch = fmt.Sprintf("%sask-%s", b.Cfg.BranchPrefix, it.Query.ID)
	case sched.Triage:
		p.Branch = fmt.Sprintf("%striage-%d", b.Cfg.BranchPrefix, it.Number)
	}
	return p
}

// fill reads the item's text, and its branch for builds, from GitHub.
func (b *Bot) fill(p *job.Plan) error {
	issue, err := b.GH.GetIssue(p.Number)
	if err != nil {
		return err
	}
	p.Title = issue.Title
	thread, err := b.thread(issue)
	if err != nil {
		return err
	}
	if p.IsPR && p.Issue != 0 {
		if parent, err := b.GH.GetIssue(p.Issue); err == nil {
			parentThread, err := b.thread(parent)
			if err == nil {
				thread = parentThread + "\n\n" + thread
			}
		}
	}
	p.Thread = prompts.Truncate(thread, threadLimit)
	switch p.Action {
	case sched.Build:
		if p.Branch == "" {
			p.Branch = b.BranchFor(p.Number, p.Title)
		}
		exists, err := b.GH.BranchExists(p.Branch)
		if err != nil {
			return err
		}
		p.Continue = exists
	case sched.Revise, sched.Review:
		pr, err := b.GH.GetPR(p.Number)
		if err != nil {
			return err
		}
		p.Branch, p.PR = pr.Head.Ref, pr.Number
	}
	return nil
}

// thread is an issue's text and its trusted people's comments, each fenced as data.
func (b *Bot) thread(issue *github.Issue) (string, error) {
	kind := "Issue"
	if issue.IsPR() {
		kind = "Pull request"
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("%s #%d: %s", kind, issue.Number, issue.Title),
		prompts.Fence(fmt.Sprintf("%s #%d by @%s", strings.ToLower(kind), issue.Number, issue.User.Login), issue.Body))
	comments, err := b.GH.ListComments(issue.Number)
	if err != nil {
		return "", err
	}
	for _, c := range comments {
		if b.isBot(c.User.Login) || strings.Contains(c.Body, Marker) || !b.Trusted(c.User.Login, c.AuthorAssociation) {
			continue
		}
		parts = append(parts, prompts.Fence(fmt.Sprintf("comment by @%s on %s", c.User.Login,
			c.CreatedAt.Format("2006-01-02")), c.Body))
	}
	return strings.Join(parts, "\n\n"), nil
}

// Outputs are the plan's fields the workflow's later jobs read, which the model job cannot touch.
func Outputs(cfg *config.Config, p *job.Plan) map[string]string {
	if p == nil {
		return map[string]string{"action": "none"}
	}
	cli := cfg.CLIs[p.CLI]
	return map[string]string{"action": p.Action, "number": fmt.Sprint(p.Number), "provider": p.Provider,
		"runs_on": p.RunsOn, "secret": p.Secret, "cli": p.CLI,
		"hosted": fmt.Sprint(config.GitHubHosted(p.RunsOn)), "install": cli.Install}
}
