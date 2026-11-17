package github

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory GitHub for the tests: enough behaviour for the harness's flows, and a
// record of every write.
type Fake struct {
	mu         sync.Mutex
	Files      map[string]fakeFile // "branch:path"
	Branches   map[string]bool
	Issues     map[int]*Issue
	Comments   map[int][]*Comment
	PRs        map[int]*PR
	Reviews    map[int][]*Review
	Runs       map[int64]*Run
	Jobs       map[int64][]*Job
	Logs       map[int64]string
	Labels     map[string]bool
	Required   []string
	Login      string
	Reactions  map[int64][]string
	Dispatched []map[string]string
	AutoMerge  map[int]string
	Cancelled  []int64
	Repo       map[string]any
	Protected  map[string][]string
	nextID     int64
	nextNumber int
	// ConflictOnce makes the next PutFile lose its compare-and-set once.
	ConflictOnce bool
}

type fakeFile struct {
	content []byte
	sha     string
}

// NewFake is an empty repository whose numbers start at 1.
func NewFake() *Fake {
	return &Fake{Files: map[string]fakeFile{}, Branches: map[string]bool{"main": true},
		Issues: map[int]*Issue{}, Comments: map[int][]*Comment{}, PRs: map[int]*PR{},
		Reviews: map[int][]*Review{}, Runs: map[int64]*Run{}, Jobs: map[int64][]*Job{},
		Logs: map[int64]string{}, Labels: map[string]bool{}, Reactions: map[int64][]string{},
		AutoMerge: map[int]string{}, Repo: map[string]any{}, Protected: map[string][]string{},
		Login: "nightbot", nextID: 1000, nextNumber: 1}
}

// AddIssue files an issue, or a pull request with pr set, and returns its number.
func (f *Fake) AddIssue(title, body, author string, labels ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addIssue(title, body, author, labels)
}

func (f *Fake) addIssue(title, body, author string, labels []string) int {
	n := f.nextNumber
	f.nextNumber++
	issue := &Issue{Number: n, Title: title, Body: body, State: "open", User: User{Login: author},
		CreatedAt: time.Date(2026, 1, 1, 0, n, 0, 0, time.UTC), HTMLURL: fmt.Sprintf("https://github.com/o/r/issues/%d", n)}
	for _, l := range labels {
		issue.Labels = append(issue.Labels, Label{Name: l})
	}
	issue.UpdatedAt = issue.CreatedAt
	f.Issues[n] = issue
	return n
}

// AddComment adds a comment by author to an issue and returns its id.
func (f *Fake) AddComment(number int, author, association, body string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.Comments[number] = append(f.Comments[number], &Comment{ID: f.nextID, Body: body,
		User: User{Login: author}, AuthorAssociation: association, CreatedAt: time.Now().UTC()})
	return f.nextID
}

// LabelsOf lists an issue's labels, sorted.
func (f *Fake) LabelsOf(number int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	if issue := f.Issues[number]; issue != nil {
		for _, l := range issue.Labels {
			names = append(names, l.Name)
		}
	}
	sort.Strings(names)
	return names
}

// CommentsOn is the bodies of an issue's comments.
func (f *Fake) CommentsOn(number int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var bodies []string
	for _, c := range f.Comments[number] {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

func (f *Fake) GetFile(branch, path string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.Files[branch+":"+path]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s:%s", ErrNotFound, branch, path)
	}
	return append([]byte(nil), file.content...), file.sha, nil
}

func (f *Fake) PutFile(branch, path string, content []byte, sha, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ConflictOnce {
		f.ConflictOnce = false
		f.nextID++
		old := f.Files[branch+":"+path]
		f.Files[branch+":"+path] = fakeFile{content: old.content, sha: fmt.Sprint("sha", f.nextID)}
		return ErrConflict
	}
	if f.Files[branch+":"+path].sha != sha {
		return ErrConflict
	}
	f.nextID++
	f.Files[branch+":"+path] = fakeFile{content: append([]byte(nil), content...), sha: fmt.Sprint("sha", f.nextID)}
	return nil
}

func (f *Fake) BranchExists(branch string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Branches[branch], nil
}

func (f *Fake) CreateOrphanBranch(branch, path string, content []byte, message string) error {
	f.mu.Lock()
	f.Branches[branch] = true
	f.mu.Unlock()
	return f.PutFile(branch, path, content, "", message)
}

func (f *Fake) DeleteBranch(branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Branches, branch)
	return nil
}

func (f *Fake) GetIssue(number int) (*Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue, ok := f.Issues[number]
	if !ok {
		return nil, fmt.Errorf("%w: issue %d", ErrNotFound, number)
	}
	copied := *issue
	copied.Labels = append([]Label(nil), issue.Labels...)
	return &copied, nil
}

func (f *Fake) ListIssues(label, state string) ([]*Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Issue
	numbers := make([]int, 0, len(f.Issues))
	for n := range f.Issues {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	for _, n := range numbers {
		issue := f.Issues[n]
		if (state == "all" || issue.State == state) && (label == "" || issue.HasLabel(label)) {
			copied := *issue
			copied.Labels = append([]Label(nil), issue.Labels...)
			out = append(out, &copied)
		}
	}
	return out, nil
}

func (f *Fake) CreateIssue(title, body string, labels []string) (*Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.addIssue(title, body, f.Login, labels)
	return f.Issues[n], nil
}

func (f *Fake) UpdateIssueBody(number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Issues[number].Body = body
	return nil
}

func (f *Fake) ListComments(number int) ([]*Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*Comment(nil), f.Comments[number]...), nil
}

func (f *Fake) CreateComment(number int, body string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.Comments[number] = append(f.Comments[number], &Comment{ID: f.nextID, Body: body,
		User: User{Login: f.Login, Type: "User"}, CreatedAt: time.Now().UTC()})
	return f.nextID, nil
}

func (f *Fake) UpdateComment(id int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, comments := range f.Comments {
		for _, c := range comments {
			if c.ID == id {
				c.Body = body
				return nil
			}
		}
	}
	return fmt.Errorf("%w: comment %d", ErrNotFound, id)
}

func (f *Fake) AddReaction(commentID int64, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Reactions[commentID] = append(f.Reactions[commentID], content)
	return nil
}

func (f *Fake) AddLabels(number int, labels ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue, ok := f.Issues[number]
	if !ok {
		return fmt.Errorf("%w: issue %d", ErrNotFound, number)
	}
	for _, l := range labels {
		if !issue.HasLabel(l) {
			issue.Labels = append(issue.Labels, Label{Name: l})
		}
		if pr := f.PRs[number]; pr != nil {
			pr.Labels = issue.Labels
		}
	}
	return nil
}

func (f *Fake) RemoveLabel(number int, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue, ok := f.Issues[number]
	if !ok {
		return nil
	}
	kept := issue.Labels[:0]
	for _, l := range issue.Labels {
		if l.Name != label {
			kept = append(kept, l)
		}
	}
	issue.Labels = kept
	if pr := f.PRs[number]; pr != nil {
		pr.Labels = kept
	}
	return nil
}

func (f *Fake) EnsureLabel(name, color, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Labels[name] = true
	return nil
}

func (f *Fake) GetPR(number int) (*PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.PRs[number]
	if !ok {
		return nil, fmt.Errorf("%w: pull request %d", ErrNotFound, number)
	}
	copied := *pr
	return &copied, nil
}

func (f *Fake) FindPR(head string) (*PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pr := range f.PRs {
		if pr.Head.Ref == head && pr.State == "open" {
			copied := *pr
			return &copied, nil
		}
	}
	return nil, nil
}

func (f *Fake) CreatePR(head, base, title, body string, draft bool) (*PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.addIssue(title, body, f.Login, nil)
	f.Issues[n].PullRequest = &struct {
		URL string `json:"url"`
	}{URL: fmt.Sprint("pulls/", n)}
	pr := &PR{Number: n, NodeID: fmt.Sprint("PR_", n), Title: title, Body: body, State: "open",
		Draft: draft, User: User{Login: f.Login}, HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/%d", n)}
	pr.Head.Ref = head
	pr.Base.Ref = base
	f.PRs[n] = pr
	copied := *pr
	return &copied, nil
}

func (f *Fake) UpdatePR(number int, title, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pr := f.PRs[number]; pr != nil {
		pr.Title, pr.Body = title, body
	}
	return nil
}

func (f *Fake) ListReviews(number int) ([]*Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*Review(nil), f.Reviews[number]...), nil
}

func (f *Fake) CreateReview(number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Reviews[number] = append(f.Reviews[number], &Review{Body: body, State: "COMMENTED", User: User{Login: f.Login}})
	return nil
}

func (f *Fake) MarkReady(pr *PR) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.PRs[pr.Number]; p != nil {
		p.Draft = false
	}
	return nil
}

func (f *Fake) EnableAutoMerge(pr *PR, method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.AutoMerge[pr.Number] = method
	return nil
}

func (f *Fake) RequiredChecks(branch string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Required...), nil
}

func (f *Fake) DispatchWorkflow(file, ref string, inputs map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	record := map[string]string{"file": file, "ref": ref}
	for k, v := range inputs {
		record[k] = v
	}
	f.Dispatched = append(f.Dispatched, record)
	return nil
}

func (f *Fake) ListRuns(file, status string) ([]*Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Run
	for _, run := range f.Runs {
		if run.Status == status {
			out = append(out, run)
		}
	}
	return out, nil
}

func (f *Fake) GetRun(id int64) (*Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.Runs[id]
	if !ok {
		return nil, fmt.Errorf("%w: run %d", ErrNotFound, id)
	}
	return run, nil
}

func (f *Fake) CancelRun(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Cancelled = append(f.Cancelled, id)
	return nil
}

func (f *Fake) FailedJobs(runID int64) ([]*Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Jobs[runID], nil
}

func (f *Fake) JobLog(jobID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Logs[jobID], nil
}

func (f *Fake) Whoami() (string, error) { return f.Login, nil }

func (f *Fake) UpdateRepo(settings map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range settings {
		f.Repo[k] = v
	}
	return nil
}

func (f *Fake) ProtectBranch(branch string, checks []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Protected[branch] = checks
	f.Required = checks
	return nil
}

// HasComment says whether any comment on the issue contains text.
func (f *Fake) HasComment(number int, text string) bool {
	for _, body := range f.CommentsOn(number) {
		if strings.Contains(body, text) {
			return true
		}
	}
	return false
}
