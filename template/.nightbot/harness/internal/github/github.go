// Package github is the little of GitHub's API the harness uses, behind an interface so the
// tests can run against an in-memory fake (fake.go).
package github

import (
	"errors"
	"time"
)

// ErrConflict is a compare-and-set write that lost: the file changed since it was read.
var ErrConflict = errors.New("github: the file changed since it was read")

// ErrNotFound is a 404.
var ErrNotFound = errors.New("github: not found")

// Issue is an issue or a pull request, as the issues API lists both.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	User   User   `json:"user"`
	// AuthorAssociation is the author's relation to the repository (OWNER, MEMBER, …).
	AuthorAssociation string    `json:"author_association"`
	Labels            []Label   `json:"labels"`
	Assignees         []User    `json:"assignees"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	HTMLURL           string    `json:"html_url"`
	// PullRequest is set when the issue is a pull request.
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
}

// HeadRepo is the repository a pull request's branch lives in ("" when it was deleted).
func (p *PR) HeadRepo() string {
	if p.Head.Repo == nil {
		return ""
	}
	return p.Head.Repo.FullName
}

// IsPR says whether the issue is a pull request.
func (i *Issue) IsPR() bool { return i.PullRequest != nil }

// HasLabel says whether the issue carries the label.
func (i *Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// User is a GitHub account.
type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Label is a label on an issue.
type Label struct {
	Name string `json:"name"`
}

// Comment is an issue or pull request comment.
type Comment struct {
	ID                int64     `json:"id"`
	Body              string    `json:"body"`
	User              User      `json:"user"`
	AuthorAssociation string    `json:"author_association"`
	CreatedAt         time.Time `json:"created_at"`
	HTMLURL           string    `json:"html_url"`
}

// PR is a pull request.
type PR struct {
	Number            int    `json:"number"`
	NodeID            string `json:"node_id"`
	Title             string `json:"title"`
	Body              string `json:"body"`
	State             string `json:"state"`
	Draft             bool   `json:"draft"`
	Merged            bool   `json:"merged"`
	HTMLURL           string `json:"html_url"`
	User              User   `json:"user"`
	AuthorAssociation string `json:"author_association"`
	Head              struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Labels []Label `json:"labels"`
}

// Review is a pull request review.
type Review struct {
	ID                int64  `json:"id"`
	Body              string `json:"body"`
	State             string `json:"state"`
	User              User   `json:"user"`
	AuthorAssociation string `json:"author_association"`
}

// Run is a workflow run.
type Run struct {
	ID         int64     `json:"id"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	HeadBranch string    `json:"head_branch"`
	HeadSHA    string    `json:"head_sha"`
	CreatedAt  time.Time `json:"created_at"`
}

// Job is a job of a workflow run.
type Job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// Client is what the harness asks of GitHub. Every method acts on the configured repository.
type Client interface {
	// Files on a branch, for the state file: Put with the sha read (or "" for a new file)
	// returns ErrConflict when the file changed since.
	GetFile(branch, path string) (content []byte, sha string, err error)
	PutFile(branch, path string, content []byte, sha, message string) error
	BranchExists(branch string) (bool, error)
	CreateOrphanBranch(branch, path string, content []byte, message string) error
	DeleteBranch(branch string) error

	GetIssue(number int) (*Issue, error)
	ListIssues(label, state string) ([]*Issue, error)
	CreateIssue(title, body string, labels []string) (*Issue, error)
	UpdateIssueBody(number int, body string) error
	ListComments(number int) ([]*Comment, error)
	CreateComment(number int, body string) (int64, error)
	UpdateComment(id int64, body string) error
	// AddReaction reacts to an issue comment, or with review to a pull request review comment.
	AddReaction(commentID int64, content string, review bool) error
	AddLabels(number int, labels ...string) error
	RemoveLabel(number int, label string) error
	EnsureLabel(name, color, description string) error

	GetPR(number int) (*PR, error)
	FindPR(head string) (*PR, error)
	CreatePR(head, base, title, body string, draft bool) (*PR, error)
	UpdatePR(number int, title, body string) error
	ListReviews(number int) ([]*Review, error)
	CreateReview(number int, body string) error
	MarkReady(pr *PR) error
	// EnableAutoMerge merges the pull request once its checks pass, but only while its head is
	// still headSHA: a push after the approval turns it off.
	EnableAutoMerge(pr *PR, method, headSHA string) error
	// DisableAutoMerge turns auto-merge off and makes the pull request a draft again.
	DisableAutoMerge(pr *PR) error

	RequiredChecks(branch string) ([]string, error)
	DispatchWorkflow(file, ref string, inputs map[string]string) error
	ListRuns(file, status string) ([]*Run, error)
	GetRun(id int64) (*Run, error)
	CancelRun(id int64) error
	FailedJobs(runID int64) ([]*Job, error)
	JobLog(jobID int64) (string, error)

	Whoami() (string, error)
	UpdateRepo(settings map[string]any) error
	ProtectBranch(branch string, checks []string) error
}
